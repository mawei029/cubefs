package blobstore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/log"
)

// ecSlowEnsureReadViewLogThreshold 与 blobstore.Writer 侧 slow 日志阈值一致思路：仅异常长耗时打 Info。
const ecSlowEnsureReadViewLogThreshold = 10 * time.Second

var _ stream.ECStreamerAPI = (*ECStreamer)(nil)

// ECStreamer：inode 级共享的 Reader/Writer 占位聚合（与副本 Streamer 概念对齐，实现后续补全）。
type ECStreamer struct {
	volName   string
	ino       uint64
	blockSize int
	mw        *meta.MetaWrapper
	ebsc      *BlobStoreClient
	// Open/NewFile 引用 +1，Release 最后一个句柄 -1 至 0 时 Close reader/writer 并 map.Delete
	refCnt int32
	// 句柄无关的逻辑文件尾与代际，语义对齐副本 Streamer.extents（ExtentCache.size / gen）：
	// - 写完成/flush：raiseFileSize（noteWriteFinished / noteWriterFlushCommitted，只抬高，覆盖写不缩尾）
	// - 截断/Setattr：updateMetaInfo(..., &commitSize) → commitFileSize（可压低）
	// - GetObjExtents 刷新且无脏缓冲：setFileSize(logicalReadBound) 与 meta 对齐（可压低）
	// - InodeGet 锚点对齐：syncLogicalSizeFromInode（读路径 syncInodeView）
	fileSize uint64
	// 对应元数据的变化，看本地副本元数据，开了新extent，truncate，新写Blob也要变化
	inoVersion uint64
	oeks       []proto.ObjExtentKey
	// dirty：对外唯一同步标志。dirty=1 表示读/Flush 前须同步（未落盘缓冲和/或 oeks 旧）；dirty=0 表示已与 meta 对齐且 Writer 无未落盘字节。
	// 不变式：未落盘字节存在 ⇒ dirty=1；cleanDirty 仅于 completeFlushMeta/updateMetaInfo（无脏缓冲）之后。外部只判断 isDirty()。
	dirty uint32

	// mu：单锁串行化本 inode 的 fReader/fWriter、Open/Close 与 lazyInit/closeReaderWriterLocked、以及 Read/Write/Flush 与 ensureReadViewCurrent。
	// 与副本 Streamer 由单 goroutine server 串行处理请求等价；网络 IO 在持锁下完成，换吞吐换正确性（LTP gf04/gf05）。
	mu      sync.RWMutex
	fReader *Reader // TODO：next version，reader和writer合并成一个
	fWriter *Writer

	// once / objExtentsOnceErr：对齐副本 Streamer.once + ExtentClient Read/Write 首次 s.once.Do(GetExtents)；
	// 随 ECStreamer 实例只执行一次；最后一关 Close 从 map 删除后由 NewECStreamer 得到新的 zero Once，与副本「删表项再 Open 新建 Streamer」一致。
	once sync.Once
}

// NewECStreamer 构造 ECStreamer；生产路径通常 r、w 均为 nil，由 OpenStreamWithArgs 在持 s.mu 下 lazyInitReaderWriter 创建 Reader/Writer。
func NewECStreamer(args ECStreamOpenArgs, r *Reader, w *Writer) (*ECStreamer, error) {
	s := &ECStreamer{
		volName:    args.VolName,
		mw:         args.Mw,
		ebsc:       args.Ebsc,
		ino:        args.Ino,
		fileSize:   args.FileSize,
		blockSize:  args.BlockSize,
		inoVersion: args.InodeGeneration,
		fReader:    r,
		fWriter:    w,
	}

	cfg := args.toClientConfig(s)
	if s.fReader == nil {
		s.fReader = NewReader(cfg)
	}
	if s.fWriter == nil {
		s.fWriter = NewWriter(cfg)
	}
	return s, nil
}

// ----- ECStreamer 对外方法（首字母大写）-----
func (s *ECStreamer) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fReader = nil
	s.fWriter = nil
}

func (s *ECStreamer) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.fWriter.Flush(s.ino, ctx); err != nil {
		return err
	}

	return s.updateMetaInfo(false, nil)
}

// FlushAndFreeCache 先走与读路径一致的 Flush（ensureReadViewCurrent），再释放 Writer 池化缓冲；供 Open 重入 oec 流前收敛旧状态。
func (s *ECStreamer) FlushAndFreeCache(ctx context.Context) error {
	// todo : 简单，writer调用Flush。 writer.Flush(s.ino, ctx)， 更新reader里的预读。加上获取新 meta， oeks
	// 如果不是脏数据，diry为false不要操作
	if err := s.Flush(ctx); err != nil {
		return err
	}
	if w := s.fWriter; w != nil {
		w.FreeCache()
	}
	return nil
}

func (s *ECStreamer) Volume() string {
	return s.volName
}

func (s *ECStreamer) Inode() uint64 {
	return s.ino
}

func (s *ECStreamer) BlockSize() int {
	return s.blockSize
}

func (s *ECStreamer) Ebsc() *BlobStoreClient {
	return s.ebsc
}

func (s *ECStreamer) Mw() *meta.MetaWrapper {
	return s.mw
}

func (s *ECStreamer) RefCnt() int32 {
	return atomic.LoadInt32(&s.refCnt)
}

// OeksLocked 在持 RLock 下拷贝 oeks，供 Reader/Writer 在无 ECStreamer.mu 时使用。
func (s *ECStreamer) OeksLocked() []proto.ObjExtentKey {
	return append([]proto.ObjExtentKey(nil), s.oeks...)
}

// String 供调试日志使用，与副本 Streamer.String 用途对齐；仅读原子字段与 ino，调用方无需持 s.mu。
func (s *ECStreamer) String() string {
	if s == nil {
		log.LogErrorf("ECStreamer String: s is nil")
		return "ECStreamer{nil}"
	}
	return fmt.Sprintf("ECStreamer{ino(%v), ref(%v), dirty(%v), fileSize(%v), inoVer(%v), addr(%p)}",
		s.ino, atomic.LoadInt32(&s.refCnt), s.isDirty(),
		atomic.LoadUint64(&s.fileSize), atomic.LoadUint64(&s.inoVersion), s)
}

func (s *ECStreamer) NewReader(cfg ClientConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fReader == nil {
		cfg.ECStreamer = s
		s.fReader = NewReader(cfg)
	}
}

func (s *ECStreamer) NewWriter(cfg ClientConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fWriter == nil {
		cfg.ECStreamer = s
		s.fWriter = NewWriter(cfg)
	}
}

func (s *ECStreamer) Reader() *Reader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fReader
}

func (s *ECStreamer) Writer() *Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fWriter
}

// Truncate 占位：Blob/EC 截断请走 ECExtentClient.Truncate（需 MetaWrapper）。
func (s *ECStreamer) Truncate(ctx context.Context, size uint64, fullPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.fWriter == nil {
		log.LogErrorf("ECStreamer Truncate: writer is nil, ino(%v) size(%v) fullPath(%v)", s.ino, size, fullPath)
		return syscall.EBADF
	}

	return s.truncateV2Locked(ctx, s.ino, size, fullPath)
}

func (s *ECStreamer) Read(ctx context.Context, dst []byte, offset int, size int, poolId uint8, isMigration bool) (int, error) {
	_, _, _ = poolId, isMigration, size
	if size == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var errGetExtents error
	s.once.Do(func() {
		errGetExtents = s.updateMetaInfo(false, nil)
	})
	if errGetExtents != nil {
		return 0, fmt.Errorf("get extents err(%w)", errGetExtents)
	}

	// dirty=1 时须 Flush（落盘缓冲和/或刷新 oeks，LTP gf05/gf19）；持 mu 串行，与 Write 不并发。
	if s.isDirty() {
		// if err := s.ensureReadViewCurrentLocked(ctx); err != nil {
		// 	return 0, err
		// }
		if err := s.fWriter.Flush(s.ino, ctx); err != nil {
			return 0, err
		}
		if err := s.updateMetaInfo(false, nil); err != nil {
			return 0, err
		}
	}

	if s.fReader == nil {
		log.LogErrorf("ECStreamer.readAfterFlush: reader is nil, ino(%v) offset(%v) size(%v)", s.ino, offset, size)
		return 0, syscall.EBADF
	}
	return s.fReader.Read(ctx, dst, offset, size)
}

// Write 实现 stream.ECStreamerAPI；带 pool/waitForFlush 的写见 WriteWithOpts。
func (s *ECStreamer) Write(ctx context.Context, offset int, data []byte, flags int, checkFunc func() error,
	storageClass uint32, isMigration bool,
) (int, error) {
	_, _, _ = checkFunc, storageClass, isMigration
	if len(data) == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var errGetExtents error
	s.once.Do(func() {
		errGetExtents = s.updateMetaInfo(false, nil)
	})
	if errGetExtents != nil {
		return 0, fmt.Errorf("get extents err(%w)", errGetExtents)
	}

	if s.fWriter == nil {
		log.LogErrorf("ECStreamer.WriteWithOpts: writer is nil, ino(%v) offset(%v) len(%v) flags(%v)", s.ino, offset, len(data), flags)
		return 0, syscall.EBADF
	}

	n, err := s.fWriter.Write(ctx, offset, data, flags)
	if err != nil {
		log.LogErrorf("ECStreamer.WriteWithOpts: writer.Write failed, ino(%v) offset(%v) len(%v) flags(%v) err(%v)",
			s.ino, offset, len(data), flags, err)
		return n, err
	}
	return n, nil
}

// FileSizeView 返回 (effectiveSize, inoVersion)，供 oec.FileSize / 读上界使用（读路径不含 inode 合并）。
func (s *ECStreamer) FileSizeView() (size int, gen uint64) {
	if s == nil {
		log.LogWarnf("ECStreamer FileSizeView: s is nil")
		return 0, 0
	}
	return int(s.fileSizeView()), atomic.LoadUint64(&s.inoVersion)
}

func (s *ECStreamer) CloseReaderWriter() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeReaderWriterLocked(s.ino, context.Background())
}

func (s *ECStreamer) RefreshExtentsCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateMetaInfo(false, nil)
}

// ----- ECStreamer 内部方法（首字母小写）-----

func (s *ECStreamer) markDirty() {
	atomic.StoreUint32(&s.dirty, 1)
}

func (s *ECStreamer) cleanDirty() {
	atomic.StoreUint32(&s.dirty, 0)
}

func (s *ECStreamer) isDirty() bool {
	return atomic.LoadUint32(&s.dirty) != 0
}

// mergeInodeGen / mergeMaxFileSize / raiseFileSize 在 ECStreamer.mu 串行化写路径下更新；
// 无锁读者仅 Load，写侧一次 Load + 条件 Store，不做 CAS 重试。
func (s *ECStreamer) mergeInodeGen(inodeGen uint64) {
	if s == nil || inodeGen == 0 {
		log.LogErrorf("ECStreamer mergeInodeGen: s is nil or inodeGen is 0, ino(%v)", s.ino)
		return
	}
	cur := atomic.LoadUint64(&s.inoVersion)
	if inodeGen > cur {
		atomic.StoreUint64(&s.inoVersion, inodeGen)
	}
}

func (s *ECStreamer) raiseInodeVersion() {
	atomic.AddUint64(&s.inoVersion, 1)
}

func (s *ECStreamer) setFileSize(size uint64) {
	atomic.StoreUint64(&s.fileSize, size)
}

// raiseFileSize 将逻辑尾抬至 lb（取 max）；lb==0 时跳过。将逻辑尾抬至 logicalEnd（取 max）；覆盖写/中间 flush 不得用 setFileSize 以免误缩尾。
func (s *ECStreamer) raiseFileSize(lb uint64) {
	if s == nil || lb == 0 {
		log.LogErrorf("ECStreamer raiseFileSize: s is nil or lb is 0, ino(%v) lb(%v)", s.ino, lb)
		return
	}
	cur := atomic.LoadUint64(&s.fileSize)
	if lb > cur {
		atomic.StoreUint64(&s.fileSize, lb)
	}
}

// updateMetaInfo 从 meta 刷新 oeks、inoVersion 与 fileSize。
// commitSize 非 nil：截断/Setattr，commitFileSize 可压低；否则与 meta 对齐，有脏缓冲时保留 max(meta尾, writer.fileOffset)。
func (s *ECStreamer) updateMetaInfo(needLock bool, commitSize *uint64) error {
	if s == nil || s.mw == nil {
		log.LogErrorf("updateMetaInfo: s is nil or mw is nil, ino(%v)", s.ino)
		return nil
	}

	// TODO: 配了强制更新往下走；否则先判断dirty，再获取meta，避免重复获取meta
	// if !force && !s.isDirty() {
	// 	return nil // 没有脏数据，直接返回
	// }

	// 1. 先获取最新的 gen，size，oeks
	gen, size, _, objExtents, err := s.mw.GetObjExtents(s.ino)
	if err != nil {
		log.LogErrorf("ino(%v) GetObjExtents err(%v)", s.ino, err)
		return err
	}

	if needLock {
		s.mu.Lock()
		s.oeks = objExtents
		s.mu.Unlock()
	} else {
		s.oeks = objExtents
	}

	s.mergeInodeGen(gen)
	if commitSize != nil {
		// 截断/Setattr：压低 fileSize 并裁剪 Writer 尾（commitSize 由 truncateV2Locked 传入）
		s.commitFileSize(*commitSize)
	} else {
		lb := logicalReadBound(size, objExtents)
		if !s.isDirty() {
			s.setFileSize(lb)
		} else if w := s.fWriter; w.bufferDirtyLen() > 0 {
			// dirty 且仍有物理缓冲：保留 dirty，fileSize 取 max(meta 尾, writer 尾)
			s.raiseFileSize(lb)
			s.raiseFileSize(uint64(w.fileOffset))
			s.markDirty()
		} else {
			// dirty 无物理缓冲（仅 oeks 旧）或 flush 后：与 meta 对齐并清 dirty
			s.setFileSize(lb)
			s.cleanDirty()
		}
	}

	s.invalidateReaderPrefetchBuf()
	return nil
}

// ensureReadViewCurrentLocked：调用方已持 s.mu；Flush 脏缓冲后刷新 oeks/fileSize，直至无脏缓冲。
func (s *ECStreamer) ensureReadViewCurrentLocked(ctx context.Context) error {
	const maxSyncLoops = 128
	const retrySleep = 4 * time.Millisecond
	t0 := time.Now()
	defer func() {
		if d := time.Since(t0); d >= ecSlowEnsureReadViewLogThreshold {
			log.LogInfof("ECStreamer slow ensureReadViewCurrent: ino(%v) dur(%v)", s.ino, d)
		}
	}()
	for loops := 0; loops < maxSyncLoops; loops++ {
		w := s.fWriter
		if w == nil || !s.isDirty() {
			return nil
		}
		if s.fReader == nil {
			log.LogErrorf("ECStreamer ensureReadViewCurrent: reader is nil, ino(%v)", s.ino)
			return syscall.EBADF
		}
		if w := s.fWriter; w != nil {
			if err := w.Flush(s.ino, ctx); err != nil {
				return err
			}
		}
		if err := s.updateMetaInfo(false, nil); err != nil {
			return err
		}
		if s.isDirty() {
			log.LogWarnf("ECStreamer ensureReadViewCurrent: ino(%v) still dirty after flush", s.ino)
			if err := ctx.Err(); err != nil {
				return err
			}
			time.Sleep(retrySleep)
			continue
		}
		return nil
	}
	log.LogInfof("ECStreamer ensureReadViewCurrent exceeded max loops: ino(%v) loops(%v) dur(%v)",
		s.ino, maxSyncLoops, time.Since(t0))
	return fmt.Errorf("ECStreamer.ensureReadViewCurrentLocked: exceeded %d sync loops ino(%v) (possible dirty/flush livelock)",
		maxSyncLoops, s.ino)
}

// closeReaderWriterLocked 在已持 s.mu 下 Flush 并关闭 Blob Reader/Writer，清空 fReader/fWriter。
// 与副本 Streamer 一致：最后一关 Close 后若从 ECExtentClient.streamers 删除本对象，下次 Open 走 NewECStreamer，
// once 随新实例为零值，无需像「复用同一 heap 对象且仅换 Reader」那样手动清空 sync.Once。
func (s *ECStreamer) closeReaderWriterLocked(ino uint64, ctx context.Context) error {
	if w := s.fWriter; w != nil {
		if err := w.Flush(ino, ctx); err != nil {
			return err
		}
		w.FreeCache()
	}
	// s.fReader = nil
	// s.fWriter = nil
	return nil
}

// invalidateReaderPrefetchBuf 丢弃 Reader 侧预读窗口，避免截断/洞区扩展后仍命中旧条带字节（LTP ftest 稀疏+截断+再扩）。
// 可在已持 s.mu 时调用（与 WriteWithOpts / truncateV2Locked 同序）；内部仅对 Reader 自旋锁。
func (s *ECStreamer) invalidateReaderPrefetchBuf() {
	if s == nil {
		log.LogErrorf("ECStreamer invalidateReaderPrefetchBuf: s is nil")
		return
	}
	r := s.fReader
	if r == nil {
		log.LogErrorf("ECStreamer invalidateReaderPrefetchBuf: reader is nil, ino(%v)", s.ino)
		return
	}

	r.invalidateReadBuf()
}

// fileSizeView 返回 max(已提交尾, fWriter 未下刷尾)。直接读 s.fWriter，不抢 s.mu。
// readAfterFlush 等已持 s.mu 路径及 Reader.fileSize 须用本方法，禁止经 EffectiveLogicalSize 调 Writer() 重入死锁。
func (s *ECStreamer) fileSizeView() uint64 {
	if s == nil {
		log.LogWarnf("ECStreamer fileSizeView: s is nil")
		return 0
	}
	sz := atomic.LoadUint64(&s.fileSize)
	if w := s.fWriter; w != nil {
		if tail := uint64(w.fileOffset); tail > sz {
			return tail
		}
	}
	return sz
}

// noteWriteFinished 缓冲写成功：raiseFileSize(logicalEnd) + dirty；logicalEnd 通常为 writer.fileOffset。
func (s *ECStreamer) noteWriteFinished(logicalEnd uint64) {
	s.raiseFileSize(logicalEnd)
	s.markDirty()
	s.invalidateReaderPrefetchBuf()
}

// noteWriterFlushCommitted flush 已提交 meta：raiseFileSize(logicalEnd) + 清 dirty；覆盖写时 logicalEnd 不得小于原尾。
func (s *ECStreamer) noteWriterFlushCommitted(logicalEnd uint64) {
	s.raiseFileSize(logicalEnd)
	s.cleanDirty()
	s.invalidateReaderPrefetchBuf()
}

// commitFileSize 将逻辑文件尾设为 size（可压低 atomic fileSize），丢弃 Writer 残余缓冲并清 dirty。
// 不访问 meta，由 updateMetaInfo 在 GetObjExtents 刷新 oeks 之后调用；截断/Setattr 前须已 Flush。
func (s *ECStreamer) commitFileSize(size uint64) {
	s.setFileSize(size)
	if w := s.fWriter; w != nil {
		tail := int(size)
		if w.fileOffset > tail {
			w.fileOffset = tail
		}
		w.resetBuffer()
		if len(w.buf) > 0 {
			w.resetBufferWithoutPool()
		}
	}
	s.cleanDirty()
}

// commitLogicalSize 仅用于单测或已持锁且无需 GetObjExtents 的本地收敛；生产截断请用 updateMetaInfo(..., &size)。
func (s *ECStreamer) commitLogicalSize(size uint64) {
	s.commitFileSize(size)
	s.invalidateReaderPrefetchBuf()
}

// truncateV2Locked 调用方已持 s.mu；w 为当前 fWriter。
func (s *ECStreamer) truncateV2Locked(ctx context.Context, ino uint64, targetSize uint64, fullPath string) error {
	if s.fWriter == nil {
		log.LogErrorf("ECStreamer truncateV2: writer nil, ino(%v)", ino)
		return syscall.EBADF
	}
	// 调用streamer的Flush， 只是下刷数据和写道meta // flush完后需要更新size和oeks
	if err := s.fWriter.Flush(ino, ctx); err != nil {
		return err
	}

	_, currentSize, _, objExtents, err := s.mw.GetObjExtents(ino)
	if err != nil {
		// create new file
		if err == syscall.ENOENT || strings.Contains(err.Error(), syscall.ENOENT.Error()) {
			log.LogDebugf("ECStreamer truncateV2: ino(%v) not found, new empty size(%v)", ino, targetSize)
			s.markDirty()
			if err := s.mw.TruncateV2(ino, targetSize, fullPath, nil, nil); err != nil {
				return err
			}
			return s.updateMetaInfo(false, &targetSize)
		}
		return err
	}

	if targetSize == currentSize {
		if s.isDirty() {
			return s.updateMetaInfo(false, &targetSize)
		}
		return nil
	}

	// s.markDirty()

	// TODO next version, 在服务端meta判断这个逻辑是否还符合，size扩大
	if targetSize > currentSize {
		if err := s.mw.TruncateV2(ino, targetSize, fullPath, objExtents, nil); err != nil {
			return err
		}
		return s.updateMetaInfo(false, &targetSize)
	}

	// 1. 扩大，meta； 2.缩小，writer 覆盖写。最后一个extent，做读改写 3. 不变 4. 新建文件
	newObjExtents, toDeletes, err := s.fWriter.TruncateV2FromExtents(ctx, targetSize, currentSize, objExtents)
	if err != nil {
		return err
	}
	if err := s.mw.TruncateV2(ino, targetSize, fullPath, newObjExtents, toDeletes); err != nil {
		return err
	}
	return s.updateMetaInfo(false, &targetSize)
}
