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
	// extentMetaEpoch：Writer 每次将 ObjExtents 提交到 meta 后原子递增；Reader 用 extentEpochSeen 对比，在无 dirty 时仍能发现 oeks 过期并 Refresh。
	extentMetaEpoch uint32

	rmLock  sync.RWMutex
	fReader *Reader
	fWriter *Writer
}

// NewECStreamer 构造 ECStreamer；生产路径通常 r、w 均为 nil，由 OpenStreamWithArgs 内 lazyInitReaderWriter 创建 Reader/Writer。
func NewECStreamer(ino uint64, r *Reader, w *Writer) *ECStreamer {
	s := &ECStreamer{ino: ino}
	if r != nil || w != nil {
		s.rmLock.Lock()
		s.fReader = r
		s.fWriter = w
		s.rmLock.Unlock()
	}
	if r != nil {
		r.ecStreamer = s
	}
	if w != nil {
		w.ecStreamer = s
	}
	return s
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

// syncEffectiveSize SetFileSize/Truncate 等与 Writer 对齐的逻辑长度。
func (s *ECStreamer) syncEffectiveSize(size uint64) {
	atomic.StoreUint64(&s.fileSize, size)
	s.markDirty()
}

func (s *ECStreamer) Inode() uint64 {
	return s.ino
}

func (s *ECStreamer) Open(openForWrite bool) error {
	_ = openForWrite
	return nil
}

// hasReaderForViewSync 为真时 ensureReadViewCurrent 可执行（至少需 Reader 端点；否则返回 EBADF）。
func (s *ECStreamer) hasReaderForViewSync() bool {
	s.rmLock.RLock()
	defer s.rmLock.RUnlock()
	return s.fReader != nil
}

// ensureReadViewCurrent：若有 Writer 则 Flush；RefreshExtents；对齐 fileSize/inoVersion；在无 Writer 脏缓冲后清 dirty。
// 不持 rmLock 做网络 IO；若 Flush 与 Refresh 之间又写入，则 HasDirtyBuffer 为真继续循环。
func (s *ECStreamer) ensureReadViewCurrent(ctx context.Context) error {
	const maxSyncLoops = 128
	const retrySleep = 4 * time.Millisecond
	for loops := 0; loops < maxSyncLoops; loops++ {
		if atomic.LoadUint32(&s.dirty) == 0 {
			return nil
		}
		s.rmLock.RLock()
		writer := s.fWriter
		reader := s.fReader
		s.rmLock.RUnlock()
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
			// 仍未收敛且本轮回不 return：退让一小段时间，避免与其它写路径紧耦合时忙等
			if err := ctx.Err(); err != nil {
				return err
			}
			time.Sleep(retrySleep)
			continue
		}
		atomic.StoreUint32(&s.dirty, 0)
		return nil
	}
	return fmt.Errorf("ECStreamer.ensureReadViewCurrent: exceeded %d sync loops ino(%v) (possible dirty/flush livelock)",
		maxSyncLoops, s.ino)
}

// lazyInitReaderWriter 在持 rmLock 下补齐 Reader/Writer（与 O_RDONLY/O_RDWR 无关，避免 O_RDWR 后再写才懒建 Writer 等边界）。
func (s *ECStreamer) lazyInitReaderWriter(cfg ClientConfig) {
	s.rmLock.Lock()
	defer s.rmLock.Unlock()
	if s.fReader == nil {
		s.fReader = NewReader(cfg)
	}
	if s.fWriter == nil {
		s.fWriter = NewWriter(cfg)
	}
}

func (s *ECStreamer) Read(ctx context.Context, dst []byte, offset int, size int, poolId uint8, isMigration bool) (int, error) {
	_, _, _ = poolId, isMigration, size
	if err := s.ensureReadViewCurrent(ctx); err != nil {
		return 0, err
	}
	s.rmLock.RLock()
	reader := s.fReader
	s.rmLock.RUnlock()
	if reader == nil {
		log.LogErrorf("ECStreamer.Read: reader is nil, ino(%v) offset(%v) size(%v)", s.ino, offset, size)
		return 0, syscall.EBADF
	}
	return reader.Read(ctx, dst, offset, size)
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
	s.rmLock.RLock()
	w := s.fWriter
	s.rmLock.RUnlock()
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
		if err := s.ensureReadViewCurrent(ctx); err != nil {
			return n, err
		}
	}
	return n, nil
}

func (s *ECStreamer) Flush(ctx context.Context) error {
	return s.ensureReadViewCurrent(ctx)
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

// Truncate 占位：Blob/EC 截断请走 ECExtentClient.Truncate（需 MetaWrapper）。
func (s *ECStreamer) Truncate(ctx context.Context, size int, fullPath string) error {
	w := s.Writer()
	if w == nil || w.mw == nil {
		return syscall.EBADF
	}
	return s.truncateV2(ctx, w.mw, s.ino, uint64(size), fullPath)
}

// truncateV2 实现 EC/Blob TruncateV2：Flush -> GetObjExtents -> 仅 meta / EBS 缩容分支；缩容前可选副本 ec 同步（BeforeEBSShrinkHook）。
func (s *ECStreamer) truncateV2(ctx context.Context, mw *meta.MetaWrapper, ino uint64, targetSize uint64, fullPath string) error {
	if mw == nil {
		return syscall.EBADF
	}
	s.rmLock.RLock()
	w := s.fWriter
	s.rmLock.RUnlock()
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

func (s *ECStreamer) Release() error {
	return nil
}

// Evict 满足 stream.ECStreamerAPI：单 inode 回收由 ECExtentClient.EvictStream 完成。
func (s *ECStreamer) Evict() error {
	return nil
}

func (s *ECStreamer) RefCnt() int32 {
	return atomic.LoadInt32(&s.refCnt)
}

func (s *ECStreamer) Reader() *Reader {
	s.rmLock.RLock()
	defer s.rmLock.RUnlock()
	return s.fReader
}

func (s *ECStreamer) Writer() *Writer {
	s.rmLock.RLock()
	defer s.rmLock.RUnlock()
	return s.fWriter
}

func (s *ECStreamer) NewReader(cfg ClientConfig) {
	s.rmLock.Lock()
	defer s.rmLock.Unlock()
	if s.fReader == nil {
		s.fReader = NewReader(cfg)
	}
}

func (s *ECStreamer) NewWriter(cfg ClientConfig) {
	s.rmLock.Lock()
	defer s.rmLock.Unlock()
	if s.fWriter == nil {
		s.fWriter = NewWriter(cfg)
	}
}

func (s *ECStreamer) Cleanup() {
	s.rmLock.Lock()
	defer s.rmLock.Unlock()
	s.fReader = nil
	s.fWriter = nil
}
