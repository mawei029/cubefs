package blobstore

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/log"
)

// ecSlowEnsureReadViewLogThreshold 与 blobstore.Writer 侧 slow 日志阈值一致思路：仅异常长耗时打 Info。
const ecSlowEnsureReadViewLogThreshold = 10 * time.Second

var _ stream.ECStreamerAPI = (*ECStreamer)(nil)

// ECStreamer：inode 级共享的 Reader/Writer 占位聚合（与副本 Streamer 概念对齐，实现后续补全）。
type ECStreamer struct {
	ino    uint64
	refCnt int32 // Open/NewFile 引用 +1，Release 最后一个句柄 -1 至 0 时 Close reader/writer 并 map.Delete
	// 句柄无关的「合并逻辑长度」与 inode 版本视图：Open 时 mergeOpenSnapshot；写路径抬高 fileSize；RefreshExtents/InodeGet 对齐 inoVersion/fileSize。
	fileSize   uint64
	inoVersion uint64
	// dirty：Reader 可见视图可能落后于 Writer/元数据（缓冲内有未落盘字节或 SetFileSize 等后置 1）；ensureReadViewCurrent 在 Flush+RefreshExtents 后，
	// 且确认 Writer 无脏缓冲时置 0；Writer 在仅元数据已提交且无脏缓冲时也可通过 noteWriterFlushCommitted 直接清 dirty。
	dirty uint32
	// rdonly：与副本 Streamer.rdonly 对齐（原子 1=仅 RO 类打开、读前可跳过视图 flush；任一会话 O_RDWR/O_WRONLY 后置 0）。
	// 读路径：if !s.rdonly || s.dirty → ensureReadViewCurrent（见 readAfterFlush）。
	rdonly uint32

	// extentMetaEpoch：Writer 每次将 ObjExtents 提交到 meta 后原子递增；Reader 用 extentEpochSeen 对比，在无 dirty 时仍能发现 oeks 过期并 Refresh。
	extentMetaEpoch uint32

	// mu：单锁串行化本 inode 的 fReader/fWriter、Open/Close 与 lazyInit/teardown、以及 Read/Write/Flush 与 ensureReadViewCurrent。
	// 与副本 Streamer 由单 goroutine server 串行处理请求等价；网络 IO 在持锁下完成，换吞吐换正确性（LTP gf04/gf05）。
	mu      sync.Mutex
	fReader *Reader
	fWriter *Writer

	// onceObjExtents / objExtentsOnceErr：对齐副本 Streamer.once + ExtentClient Read/Write 首次 s.once.Do(GetExtents)；
	// 随 ECStreamer 实例只执行一次；最后一关 Close 从 map 删除后由 NewECStreamer 得到新的 zero Once，与副本「删表项再 Open 新建 Streamer」一致。
	onceObjExtents    sync.Once
	objExtentsOnceErr error
}

// NewECStreamer 构造 ECStreamer；生产路径通常 r、w 均为 nil，由 OpenStreamWithArgs 在持 s.mu 下 lazyInitReaderWriter 创建 Reader/Writer。
func NewECStreamer(ino uint64, r *Reader, w *Writer) *ECStreamer {
	s := &ECStreamer{ino: ino}
	if r != nil || w != nil {
		s.mu.Lock()
		s.fReader = r
		s.fWriter = w
		s.mu.Unlock()
	}
	if r != nil {
		r.ecStreamer = s
	}
	if w != nil {
		w.ecStreamer = s
	}
	atomic.StoreUint32(&s.rdonly, 1)
	return s
}

// ----- ECStreamer 对外方法（首字母大写）-----

func (s *ECStreamer) Cleanup() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fReader = nil
	s.fWriter = nil
}

func (s *ECStreamer) Evict() error {
	return nil
}

func (s *ECStreamer) Flush(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureReadViewCurrentLocked(ctx)
}

// FlushAndFreeCache 先走与读路径一致的 Flush（ensureReadViewCurrent），再释放 Writer 池化缓冲；供 Open 重入 oec 流前收敛旧状态。
func (s *ECStreamer) FlushAndFreeCache(ctx context.Context) error {
	if err := s.Flush(ctx); err != nil {
		return err
	}
	if w := s.Writer(); w != nil {
		w.FreeCache()
	}
	return nil
}

func (s *ECStreamer) Inode() uint64 {
	return s.ino
}

// String 供调试日志使用，与副本 Streamer.String 用途对齐；仅读原子字段与 ino，调用方无需持 s.mu。
func (s *ECStreamer) String() string {
	if s == nil {
		return "ECStreamer{nil}"
	}
	return fmt.Sprintf("ECStreamer{ino(%v), ref(%v), rdonly(%v), dirty(%v), fileSize(%v), inoVer(%v), addr(%p)}",
		s.ino, atomic.LoadInt32(&s.refCnt), atomic.LoadUint32(&s.rdonly) != 0, atomic.LoadUint32(&s.dirty) != 0,
		atomic.LoadUint64(&s.fileSize), atomic.LoadUint64(&s.inoVersion), s)
}

func (s *ECStreamer) NewReader(cfg ClientConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fReader == nil {
		s.fReader = NewReader(cfg)
	}
}

func (s *ECStreamer) NewWriter(cfg ClientConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fWriter == nil {
		s.fWriter = NewWriter(cfg)
	}
}

func (s *ECStreamer) Open(openForWrite bool) error {
	_ = openForWrite
	return nil
}

func (s *ECStreamer) Read(ctx context.Context, dst []byte, offset int, size int, poolId uint8, isMigration bool) (int, error) {
	return s.readAfterFlush(ctx, dst, offset, size, poolId, isMigration, false, 0, 0)
}

func (s *ECStreamer) RefCnt() int32 {
	return atomic.LoadInt32(&s.refCnt)
}

func (s *ECStreamer) Release() error {
	return nil
}

func (s *ECStreamer) Reader() *Reader {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fReader
}

// Truncate 占位：Blob/EC 截断请走 ECExtentClient.Truncate（需 MetaWrapper）。
func (s *ECStreamer) Truncate(ctx context.Context, size int, fullPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.fWriter
	if w == nil || w.mw == nil {
		return syscall.EBADF
	}
	return s.truncateV2Locked(ctx, w.mw, s.ino, uint64(size), fullPath, w)
}

// Write 实现 stream.ECStreamerAPI；带 pool/waitForFlush 的写见 WriteWithOpts。
func (s *ECStreamer) Write(ctx context.Context, offset int, data []byte, flags int, checkFunc func() error,
	storageClass uint32, isMigration bool,
) (int, error) {
	return s.WriteWithOpts(ctx, offset, data, flags, checkFunc, 0, storageClass, isMigration, false)
}

// WriteWithOpts EC/Blob 写路径扩展：poolId、waitForFlush 由上层 Extent 风格 API 传入。
func (s *ECStreamer) WriteWithOpts(ctx context.Context, offset int, data []byte, flags int, checkFunc func() error,
	poolId uint8, storageClass uint32, isMigration, waitForFlush bool,
) (int, error) {
	_, _, _, _ = checkFunc, poolId, storageClass, isMigration
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onceObjExtents.Do(func() {
		s.loadObjExtentsFromMetaLocked()
	})
	if s.objExtentsOnceErr != nil {
		return 0, fmt.Errorf("get extents err(%w)", s.objExtentsOnceErr)
	}
	w := s.fWriter
	if w == nil {
		log.LogErrorf("ECStreamer.WriteWithOpts: writer is nil, ino(%v) offset(%v) len(%v) flags(%v)", s.ino, offset, len(data), flags)
		return 0, syscall.EBADF
	}
	n, err := w.Write(ctx, offset, data, flags)
	if err != nil {
		log.LogErrorf("ECStreamer.WriteWithOpts: writer.Write failed, ino(%v) offset(%v) len(%v) flags(%v) err(%v)",
			s.ino, offset, len(data), flags, err)
		return n, err
	}
	if waitForFlush {
		if err := s.ensureReadViewCurrentLocked(ctx); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *ECStreamer) Writer() *Writer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fWriter
}

// ----- ECStreamer 内部方法（首字母小写）-----

// ensureReadViewCurrentLocked：调用方已持 s.mu；若有 Writer 则 Flush、RefreshExtents、对齐长度并清 dirty。
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
		if atomic.LoadUint32(&s.dirty) == 0 {
			return nil
		}
		writer := s.fWriter
		reader := s.fReader
		if reader == nil {
			return syscall.EBADF
		}
		if writer != nil {
			if err := writer.Flush(s.ino, ctx); err != nil {
				return err
			}
		}
		extGen, err := reader.RefreshExtents()
		if err != nil {
			return err
		}
		s.mergeInodeGen(extGen)
		if lb, ok := reader.LogicalReadBound(); ok {
			for {
				old := atomic.LoadUint64(&s.fileSize)
				next := old
				if lb > next {
					next = lb
				}
				if atomic.CompareAndSwapUint64(&s.fileSize, old, next) {
					break
				}
			}
		}
		if writer != nil && writer.HasDirtyBuffer() {
			if err := ctx.Err(); err != nil {
				return err
			}
			time.Sleep(retrySleep)
			continue
		}
		atomic.StoreUint32(&s.dirty, 0)
		return nil
	}
	log.LogInfof("ECStreamer ensureReadViewCurrent exceeded max loops: ino(%v) loops(%v) dur(%v)",
		s.ino, maxSyncLoops, time.Since(t0))
	return fmt.Errorf("ECStreamer.ensureReadViewCurrentLocked: exceeded %d sync loops ino(%v) (possible dirty/flush livelock)",
		maxSyncLoops, s.ino)
}

// hasReaderForViewSync 在持 s.mu 下可调用；否则内部短暂加锁。
func (s *ECStreamer) hasReaderForViewSync() bool {
	s.mu.Lock()
	ok := s.fReader != nil
	s.mu.Unlock()
	return ok
}

// lazyInitReaderWriter 由 OpenStreamWithArgs 在已持 s.mu 下调用。
func (s *ECStreamer) lazyInitReaderWriter(cfg ClientConfig) {
	if s.fReader == nil {
		s.fReader = NewReader(cfg)
	}
	if s.fWriter == nil {
		s.fWriter = NewWriter(cfg)
	}
}

// loadObjExtentsFromMetaLocked 在持 s.mu 下由 onceObjExtents 回调执行。
func (s *ECStreamer) loadObjExtentsFromMetaLocked() {
	r := s.fReader
	if r == nil {
		s.objExtentsOnceErr = syscall.EBADF
		return
	}
	gen, err := r.RefreshExtents()
	if err != nil {
		s.objExtentsOnceErr = err
		return
	}
	s.mergeInodeGen(gen)
}

// teardownEndpointsLocked 在持 s.mu 下回收 Reader/Writer。
// 与副本 Streamer 一致：最后一关 Close 后若从 ECExtentClient.streamers 删除本对象，下次 Open 走 NewECStreamer，
// onceObjExtents 随新实例为零值，无需像「复用同一 heap 对象且仅换 Reader」那样手动清空 sync.Once。
func (s *ECStreamer) teardownEndpointsLocked(ino uint64, ctx context.Context) error {
	w := s.fWriter
	r := s.fReader
	if w != nil {
		if err := w.Flush(ino, ctx); err != nil {
			return err
		}
		w.FreeCache()
		w.Close(ctx)
	}
	s.fReader = nil
	s.fWriter = nil
	if r != nil {
		r.Close(ctx)
	}
	return nil
}

func (s *ECStreamer) ensureReadViewCurrentExternal(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureReadViewCurrentLocked(ctx)
}

func (s *ECStreamer) markDirty() {
	atomic.StoreUint32(&s.dirty, 1)
}

// mergeInodeGen 将 meta / ObjExtents 返回的 generation 与流上 inoVersion 取 max（与副本 extents.gen 视图类似）。
func (s *ECStreamer) mergeInodeGen(inodeGen uint64) {
	for {
		oldG := atomic.LoadUint64(&s.inoVersion)
		nextG := oldG
		if inodeGen > nextG {
			nextG = inodeGen
		}
		if atomic.CompareAndSwapUint64(&s.inoVersion, oldG, nextG) {
			return
		}
	}
}

// mergeOpenSnapshot 每次 Open 时用 inode 视图抬高 fileSize / inoVersion（与本地 Writer 缓存取 max）。
func (s *ECStreamer) mergeOpenSnapshot(metaFileSize, inodeGen uint64) {
	s.mergeInodeGen(inodeGen)
	for {
		old := atomic.LoadUint64(&s.fileSize)
		next := old
		if metaFileSize > next {
			next = metaFileSize
		}
		if atomic.CompareAndSwapUint64(&s.fileSize, old, next) {
			return
		}
	}
}

// noteOpenAccessMode 按 FUSE 打开模式维护 rdonly，与副本 Streamer 上 rdonly 语义一致。
func (s *ECStreamer) noteOpenAccessMode(openFlags uint32) {
	acc := int(openFlags) & syscall.O_ACCMODE
	if acc == syscall.O_RDWR || acc == syscall.O_WRONLY {
		atomic.StoreUint32(&s.rdonly, 0)
		return
	}
	if atomic.LoadUint32(&s.rdonly) != 0 {
		atomic.StoreUint32(&s.rdonly, 1)
	}
}

// noteWriteFinished 在 Writer 仍可能持有未落盘缓冲或仅靠逻辑尾更新时调用：抬高 logical 尾并标记读视图待同步。
func (s *ECStreamer) noteWriteFinished(logicalEnd uint64) {
	for {
		old := atomic.LoadUint64(&s.fileSize)
		next := old
		if logicalEnd > next {
			next = logicalEnd
		}
		if atomic.CompareAndSwapUint64(&s.fileSize, old, next) {
			break
		}
	}
	s.markDirty()
}

// noteWriterFlushCommitted 在 Writer flush/直写路径已将 ObjExtents 提交 meta、且当前无脏缓冲时调用：清 dirty 并抬高 extent 世代，使读侧可按 epoch 失效缓存而不必每次 ensureReadViewCurrent。
func (s *ECStreamer) noteWriterFlushCommitted(logicalEnd uint64) {
	for {
		old := atomic.LoadUint64(&s.fileSize)
		next := old
		if logicalEnd > next {
			next = logicalEnd
		}
		if atomic.CompareAndSwapUint64(&s.fileSize, old, next) {
			break
		}
	}
	atomic.StoreUint32(&s.dirty, 0)
	atomic.AddUint32(&s.extentMetaEpoch, 1)
}

// readAfterFlush 对齐副本 ExtentClient.Read：持 s.mu 下 once 拉 ObjExtents、ensureReadViewCurrentLocked、EnsureAligned（跳过重复 dirty flush）与 reader.Read。
func (s *ECStreamer) readAfterFlush(ctx context.Context, dst []byte, offset int, size int, poolId uint8, isMigration bool, alignInode bool, inodeGen, inodeSize uint64) (int, error) {
	_, _, _ = poolId, isMigration, size
	if size == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	s.onceObjExtents.Do(func() {
		s.loadObjExtentsFromMetaLocked()
	})
	if s.objExtentsOnceErr != nil {
		return 0, fmt.Errorf("get extents err(%w)", s.objExtentsOnceErr)
	}
	rdonly := atomic.LoadUint32(&s.rdonly) != 0
	dirty := atomic.LoadUint32(&s.dirty) != 0
	if !rdonly || dirty {
		if err := s.ensureReadViewCurrentLocked(ctx); err != nil {
			return 0, err
		}
	}
	reader := s.fReader
	if reader == nil {
		log.LogErrorf("ECStreamer.readAfterFlush: reader is nil, ino(%v) offset(%v) size(%v)", s.ino, offset, size)
		return 0, syscall.EBADF
	}
	if alignInode {
		if err := reader.ensureAlignedForRead(inodeGen, inodeSize, true); err != nil {
			return 0, err
		}
	}
	return reader.Read(ctx, dst, offset, size)
}

// syncEffectiveSize SetFileSize/Truncate 等与 Writer 对齐的逻辑长度。
func (s *ECStreamer) syncEffectiveSize(size uint64) {
	atomic.StoreUint64(&s.fileSize, size)
	s.markDirty()
}

// truncateV2Locked 调用方已持 s.mu；w 为当前 fWriter。
func (s *ECStreamer) truncateV2Locked(ctx context.Context, mw *meta.MetaWrapper, ino uint64, targetSize uint64, fullPath string, w *Writer) error {
	if mw == nil {
		return syscall.EBADF
	}
	if w == nil {
		log.LogErrorf("ECStreamer truncateV2: writer nil, ino(%v)", ino)
		return syscall.EBADF
	}
	if err := w.Flush(ino, ctx); err != nil {
		return err
	}

	_, currentSize, _, objExtents, err := mw.GetObjExtents(ino)
	if err != nil {
		if err == syscall.ENOENT || strings.Contains(err.Error(), syscall.ENOENT.Error()) {
			log.LogDebugf("ECStreamer truncateV2: ino(%v) not found, new empty size(%v)", ino, targetSize)
			if err := mw.TruncateV2(ino, targetSize, fullPath, nil, nil); err != nil {
				return err
			}
			w.SetFileSize(targetSize)
			return nil
		}
		return err
	}

	if targetSize == currentSize {
		return nil
	}

	if targetSize > currentSize {
		if err := mw.TruncateV2(ino, targetSize, fullPath, objExtents, nil); err != nil {
			return err
		}
		w.SetFileSize(targetSize)
		return nil
	}

	newObjExtents, toDeletes, err := w.TruncateV2FromExtents(ctx, targetSize, currentSize, objExtents)
	if err != nil {
		return err
	}
	if err := mw.TruncateV2(ino, targetSize, fullPath, newObjExtents, toDeletes); err != nil {
		return err
	}
	w.SetFileSize(targetSize)
	return nil
}
