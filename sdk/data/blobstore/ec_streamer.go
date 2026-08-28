package blobstore

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/log"
)

const (
	streamerNormal uint32 = 0
	streamerError  uint32 = 1

	flushIdle uint32 = 0
	flushRun  uint32 = 1
)

// ReadOnlyOeks is an immutable view of sorted obj extents shared by Reader/Writer.
type ReadOnlyOeks struct {
	items []proto.ObjExtentKey
}

// NewReadOnlyOeks wraps a oek slice as a shared read-only view (no copy).
func NewReadOnlyOeks(oeks []proto.ObjExtentKey) *ReadOnlyOeks {
	return &ReadOnlyOeks{items: oeks}
}

func (r *ReadOnlyOeks) Len() int {
	if r == nil {
		return 0
	}
	return len(r.items)
}

func (r *ReadOnlyOeks) At(idx int) proto.ObjExtentKey {
	return r.items[idx]
}

// FindContainOrAfter returns the index of the sorted oek whose [FileOffset, FileOffset+Size)
// contains fileOff. If fileOff is in a hole (or before the first oek), returns the first oek after
// that hole (FileOffset > fileOff). Returns -1 if the list is empty or fileOff is past the last extent.
func (r *ReadOnlyOeks) FindContainOrAfter(fileOff uint64) int {
	n := r.Len()
	if n == 0 {
		return -1
	}
	lo, hi := 0, n
	for lo < hi {
		mid := (lo + hi) / 2
		oek := r.At(mid)
		start, end := oek.FileOffset, oek.FileOffset+oek.Size
		if start <= fileOff && fileOff < end {
			return mid
		}
		if start <= fileOff {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	// lo is the first index with FileOffset > fileOff: fileOff is in a hole
	if lo < n {
		return lo
	}
	return -1
}

// ECStreamer shares Reader/Writer and logical view (fileSize, inoVersion, oeks, dirty) per inode.
// refCnt via OpenStreamWithArgs/CloseStream; map delete and nil RW pointers in EvictStream.
type ECStreamer struct {
	volName   string
	ino       uint64
	blockSize int
	mw        *meta.MetaWrapper
	ebsc      *BlobStoreClient
	refCnt    int32
	// Logical file tail and inode generation, aligned with replica Streamer.extents (size/gen):
	// Writes raise tail only; truncate may lower via updateMetaInfo; clean refresh aligns with meta/oek tail.
	fileSize   uint64
	inoVersion uint64
	oeks       *ReadOnlyOeks
	dirty      uint32 // dirty: 1=must sync before read/flush (buffer and/or stale oeks); 0=clean. External code uses isDirty() only.
	flushing   uint32 // 1 while flushLocked holds s.mu. TryFlush skips; Flush waits on s.mu (Go 1.18: not atomic.Bool).
	status     uint32 // streamerError: poison after io.EOF; later Write/Flush skip EBS.

	// mu serializes RW, flush, updateMetaInfo, Read/Write; EBS IO under lock (correctness over throughput, LTP).
	mu      sync.RWMutex
	fReader *Reader // TODO: next version, merge reader and writer into one
	fWriter *Writer

	once   sync.Once       // once: first Read/Write pulls meta; CloseStream zero ref resets once so next Open refreshes.
	client *ECExtentClient // client is the map owner; nil in tests that construct a streamer without ECExtentClient.
}

// NewECStreamer constructs stream; production passes nil r/w, OpenStreamWithArgs creates RW under s.mu.
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

// ----- ECStreamer exported methods -----

// CanFlush is the idle scheduleFlush gate: false when poisoned or flushLocked is already running.
func (s *ECStreamer) CanFlush() bool {
	if atomic.LoadUint32(&s.flushing) != flushIdle {
		return false
	}
	return s.rejectIfInError() == nil
}

// Flush is FUSE Fsync, O_SYNC write, and CloseStream.
// If another flushLocked holds s.mu, this waits then flushes again; it does not return nil for an inflight flush.
func (s *ECStreamer) Flush(ctx context.Context) error {
	if err := s.rejectIfInError(); err != nil {
		return err
	}
	return s.handleIoError(s.flushLocked(ctx))
}

// flushLocked sets flushing while holding s.mu around writer.Flush and updateMetaInfo. Caller must not hold s.mu.
func (s *ECStreamer) flushLocked(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	atomic.StoreUint32(&s.flushing, flushRun)
	defer atomic.StoreUint32(&s.flushing, flushIdle)

	if err := s.fWriter.Flush(s.ino, ctx); err != nil {
		return err
	}

	return s.updateMetaInfo(nil)
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

// OeksLocked returns the shared read-only oeks view (no slice copy).
// Caller must already hold s.mu (Read/Write/Flush paths); do not mutate the returned object.
func (s *ECStreamer) OeksLocked() *ReadOnlyOeks {
	return s.oeks
}

func (s *ECStreamer) HasObjExtents() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.oeks != nil && len(s.oeks.items) > 0
}

// String for debug logs; reads atomic fields and ino without s.mu.
func (s *ECStreamer) String() string {
	if s == nil {
		log.LogErrorf("ECStreamer String: s is nil")
		return "ECStreamer{nil}"
	}
	return fmt.Sprintf("ECStreamer{ino(%v), ref(%v), dirty(%v), err(%v), fileSize(%v), inoVer(%v), addr(%p)}",
		s.ino, atomic.LoadInt32(&s.refCnt), s.isDirty(), s.inError(),
		atomic.LoadUint64(&s.fileSize), atomic.LoadUint64(&s.inoVersion), s)
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

// Truncate under lock: grow/shrink via truncateV2Locked (Flush → GetObjExtents → TruncateV2 → updateMetaInfo).
func (s *ECStreamer) Truncate(ctx context.Context, size uint64, fullPath string) error {
	if err := s.rejectIfInError(); err != nil {
		return err
	}
	return s.handleIoError(s.truncateLocked(ctx, size, fullPath))
}

func (s *ECStreamer) truncateLocked(ctx context.Context, size uint64, fullPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.fWriter == nil {
		log.LogErrorf("ECStreamer Truncate: writer is nil, ino(%v) size(%v) fullPath(%v)", s.ino, size, fullPath)
		return syscall.EBADF
	}

	return s.truncateV2Locked(ctx, s.ino, size, fullPath)
}

func (s *ECStreamer) Read(ctx context.Context, dst []byte, offset int, size int) (n int, err error) {
	if size == 0 {
		return 0, nil
	}
	if err := s.rejectIfInError(); err != nil {
		return 0, err
	}
	n, err = s.readLocked(ctx, dst, offset, size)
	return n, s.handleIoError(err)
}

func (s *ECStreamer) readLocked(ctx context.Context, dst []byte, offset int, size int) (int, error) {
	// TODO: next version, lock tuning for reads; short term document; mid term split view sync vs data IO or short lock only when dirty.
	s.mu.Lock()
	defer s.mu.Unlock()

	var errGetExtents error
	s.once.Do(func() {
		errGetExtents = s.updateMetaInfo(nil)
	})
	if errGetExtents != nil {
		return 0, fmt.Errorf("get extents err(%w)", errGetExtents)
	}

	// When dirty, Flush writer then updateMetaInfo so Reader sees persisted oeks (LTP gf05/gf19).
	if s.isDirty() {
		if err := s.fWriter.Flush(s.ino, ctx); err != nil {
			return 0, err
		}
		if err := s.updateMetaInfo(nil); err != nil {
			return 0, err
		}
	}

	if s.fReader == nil {
		log.LogErrorf("ECStreamer.readAfterFlush: reader is nil, ino(%v) offset(%v) size(%v)", s.ino, offset, size)
		return 0, syscall.EBADF
	}
	return s.fReader.Read(ctx, dst, offset, size)
}

// Write performs buffered or direct blob I/O on this inode. O_SYNC / waitForFlush is handled in client/fs after oec.Write returns.
func (s *ECStreamer) Write(ctx context.Context, offset int, data []byte, flags int) (n int, err error) {
	if len(data) == 0 {
		return 0, nil
	}
	if err := s.rejectIfInError(); err != nil {
		return 0, err
	}
	n, err = s.writeLocked(ctx, offset, data, flags)
	return n, s.handleIoError(err)
}

func (s *ECStreamer) writeLocked(ctx context.Context, offset int, data []byte, flags int) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var errGetExtents error
	s.once.Do(func() {
		errGetExtents = s.updateMetaInfo(nil)
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

// FileSizeView returns (logical tail, inoVersion) for oec.FileSize and Reader bounds.
func (s *ECStreamer) FileSizeView() (size int, gen uint64) {
	if s == nil {
		log.LogWarnf("ECStreamer FileSizeView: s is nil")
		return 0, 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.fileSizeViewLocked()), atomic.LoadUint64(&s.inoVersion)
}

func (s *ECStreamer) CloseReaderWriter() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeReaderWriterLocked(s.ino, context.Background())
}

func (s *ECStreamer) RefreshExtentsCache() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.updateMetaInfo(nil)
}

// For fs_volume.go
func (s *ECStreamer) WriteFromReader(ctx context.Context, reader io.Reader, h hash.Hash) (n uint64, err error) {
	if err := s.rejectIfInError(); err != nil {
		return 0, err
	}
	n, err = s.writeFromReaderLocked(ctx, reader, h)
	return n, s.handleIoError(err)
}

func (s *ECStreamer) writeFromReaderLocked(ctx context.Context, reader io.Reader, h hash.Hash) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errGetExtents error
	s.once.Do(func() {
		errGetExtents = s.updateMetaInfo(nil)
	})
	if errGetExtents != nil {
		return 0, fmt.Errorf("get extents err(%w)", errGetExtents)
	}
	return s.fWriter.WriteFromReader(ctx, reader, h)
}

func (s *ECStreamer) WriteWithoutPool(ctx context.Context, writeOffset int, data []byte) (n int, err error) {
	if err := s.rejectIfInError(); err != nil {
		return 0, err
	}
	n, err = s.writeWithoutPoolLocked(ctx, writeOffset, data)
	return n, s.handleIoError(err)
}

func (s *ECStreamer) writeWithoutPoolLocked(ctx context.Context, writeOffset int, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errGetExtents error
	s.once.Do(func() {
		errGetExtents = s.updateMetaInfo(nil)
	})
	if errGetExtents != nil {
		return 0, fmt.Errorf("get extents err(%w)", errGetExtents)
	}
	return s.fWriter.WriteWithoutPool(ctx, writeOffset, data)
}

func (s *ECStreamer) FlushWithoutPool(ino uint64, ctx context.Context) error {
	if err := s.rejectIfInError(); err != nil {
		return err
	}
	s.mu.Lock()
	err := s.fWriter.FlushWithoutPool(ino, ctx)
	s.mu.Unlock()
	return s.handleIoError(err)
}

// ----- ECStreamer internal methods -----

func (s *ECStreamer) markDirty() {
	atomic.StoreUint32(&s.dirty, 1)
}

func (s *ECStreamer) cleanDirty() {
	atomic.StoreUint32(&s.dirty, 0)
}

func (s *ECStreamer) isDirty() bool {
	return atomic.LoadUint32(&s.dirty) != 0
}

func (s *ECStreamer) setError() {
	atomic.StoreUint32(&s.status, streamerError)
}

func (s *ECStreamer) inError() bool {
	return atomic.LoadUint32(&s.status) >= streamerError
}

func (s *ECStreamer) rejectIfInError() error {
	if s.inError() {
		// Same substring as replica Streamer.IssueWriteRequest so file.go isWriteEio treats it as NOTSUP.
		return fmt.Errorf("IssueWriteRequest: stream writer in error status, ino(%v)", s.ino)
	}
	return nil
}

// mergeInodeGen/raiseFileSize updated under mu on write paths; lock-free readers Load only.
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

// raiseFileSize raises logical tail to max(current, lb); skip lb==0; do not use setFileSize on overwrite paths.
func (s *ECStreamer) raiseFileSize(lb uint64) {
	if s == nil {
		log.LogErrorf("ECStreamer raiseFileSize: streamer is nil, lb(%v)", lb)
		return
	}
	if lb == 0 {
		// skip: may be a zero-length file: use empty file, truncate to zero...
		return
	}
	cur := atomic.LoadUint64(&s.fileSize)
	if lb > cur {
		atomic.StoreUint64(&s.fileSize, lb)
	}
}

// updateMetaInfo refreshes oeks, inoVersion, fileSize from meta GetObjExtents.
// Caller must hold s.mu. With unflushed writer buffer: raise fileSize and stay dirty; else cleanDirty.
func (s *ECStreamer) updateMetaInfo(commitSize *uint64) error {
	if s == nil || s.mw == nil {
		log.LogErrorf("updateMetaInfo: s is nil or mw is nil, ino(%v)", s.ino)
		return nil
	}

	// TODO: if force refresh configured, skip dirty short-circuit before GetObjExtents
	// if !force && !s.isDirty() {
	// 	return nil // skip when not dirty
	// }

	// 1. fetch latest gen, size, oeks
	gen, size, _, objExtents, err := s.mw.GetObjExtents(s.ino)
	if err != nil {
		log.LogErrorf("ino(%v) GetObjExtents err(%v)", s.ino, err)
		return err
	}

	// must has mutex here, because oeks is used by reader and writer
	s.oeks = &ReadOnlyOeks{items: objExtents}
	sort.Slice(s.oeks.items, func(i, j int) bool {
		return s.oeks.items[i].FileOffset < s.oeks.items[j].FileOffset
	})

	// TODO: only use atomit.StoreUint64 for inoVersion and fileSize
	// atomic.StoreUint64(&s.inoVersion, gen)
	// atomic.StoreUint64(&s.fileSize, size)
	// if w := s.fWriter; w.bufferDirtyLen() > 0 {
	// 	// dirty with buffer: keep dirty; fileSize = max(meta tail, writer tail)
	// 	s.raiseFileSize(logicalReadBound(size, objExtents))
	// 	s.raiseFileSize(uint64(w.fileOffset))
	// 	s.markDirty()
	// } else {
	// 	s.cleanDirty()
	// }

	s.mergeInodeGen(gen)
	if commitSize != nil {
		// Truncate/Setattr: clamp fileSize and trim writer tail (commitSize from truncateV2Locked).
		s.commitFileSize(*commitSize)
		atomic.StoreUint64(&s.inoVersion, gen)
	} else {
		lb := logicalReadBound(size, objExtents)
		if !s.isDirty() {
			s.setFileSize(lb)
		} else if w := s.fWriter; w.bufferDirtyLen() > 0 {
			// Dirty with buffered data: keep dirty; fileSize = max(meta tail, writer tail).
			s.raiseFileSize(lb)
			s.raiseFileSize(uint64(w.fileOffset))
			s.markDirty()
		} else {
			// Dirty without buffer (stale oeks only) or after flush: align with meta and clear dirty.
			s.setFileSize(lb)
			s.cleanDirty()
		}
	}

	s.invalidateReaderPrefetchBuf()
	return nil
}

// resetExtentsOnceLocked resets once for next updateMetaInfo; after CloseStream zero ref and dropIOCaches, under mu.
func (s *ECStreamer) resetExtentsOnceLocked() {
	s.once = sync.Once{}
}

// dropIOCachesLocked frees writer pool buffer and reader prefetch; keeps RW objects; CloseStream zero-ref path.
func (s *ECStreamer) dropIOCachesLocked() {
	if r := s.fReader; r != nil {
		r.releasePrefetchCache()
	}
	if w := s.fWriter; w != nil {
		w.FreeCache()
	}
}

// closeReaderWriterLocked under mu: Flush writer + dropIOCaches; nil RW only on EvictStream delete.
func (s *ECStreamer) closeReaderWriterLocked(ino uint64, ctx context.Context) error {
	if s.inError() {
		s.dropIOCachesLocked()
		return nil
	}
	if w := s.fWriter; w != nil {
		if err := w.Flush(ino, ctx); err != nil {
			return err
		}
	}
	s.dropIOCachesLocked()
	return nil
}

// invalidateReaderPrefetchBuf drops reader prefetch after truncate/write (LTP ftest sparse+truncate).
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

// fileSizeViewLocked returns max(atomic fileSize, writer.fileOffset). Caller must hold s.mu.
func (s *ECStreamer) fileSizeViewLocked() uint64 {
	if s == nil {
		log.LogWarnf("ECStreamer fileSizeViewLocked: s is nil")
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

// commitFileSize sets logical tail (may shrink), resets writer buffer, cleanDirty; truncate fallback.
func (s *ECStreamer) commitFileSize(size uint64) {
	s.setFileSize(size)
	if w := s.fWriter; w != nil {
		tail := int(size)
		if w.fileOffset > tail {
			w.fileOffset = tail
		}
		w.resetBuffer()
	}
	s.cleanDirty()
}

// truncateV2Locked caller holds s.mu: Flush → GetObjExtents → grow (meta only) or shrink (Writer.TruncateV2FromExtents + meta).
func (s *ECStreamer) truncateV2Locked(ctx context.Context, ino uint64, targetSize uint64, fullPath string) error {
	if s.fWriter == nil {
		log.LogErrorf("ECStreamer truncateV2: writer nil, ino(%v)", ino)
		return syscall.EBADF
	}
	if err := s.fWriter.Flush(ino, ctx); err != nil {
		return err
	}

	empty := proto.ObjExtentKey{}
	_, currentSize, _, objExtents, err := s.mw.GetObjExtents(ino)
	if err != nil {
		// create new file
		if err == syscall.ENOENT || strings.Contains(err.Error(), syscall.ENOENT.Error()) {
			log.LogDebugf("ECStreamer truncateV2: ino(%v) not found, new empty size(%v)", ino, targetSize)
			s.markDirty()
			if err := s.mw.TruncateV2(ino, targetSize, fullPath, empty, empty); err != nil {
				return err
			}
			return s.updateMetaInfo(&targetSize)
		}
		return err
	}

	if targetSize == currentSize {
		if s.isDirty() {
			return s.updateMetaInfo(&targetSize)
		}
		return nil
	}

	if targetSize > currentSize {
		if err := s.mw.TruncateV2(ino, targetSize, fullPath, empty, empty); err != nil {
			return err
		}
		return s.updateMetaInfo(&targetSize)
	}

	// shrink file, and no oeks
	if len(objExtents) == 0 {
		if err := s.mw.TruncateV2(ino, targetSize, fullPath, empty, empty); err != nil {
			return err
		}
		return s.updateMetaInfo(&targetSize)
	}

	// shrink file, and has oeks
	sort.Slice(objExtents, func(i, j int) bool {
		return objExtents[i].FileOffset < objExtents[j].FileOffset
	})
	newObjExtent, toDeleteFrom, err := s.fWriter.TruncateV2FromExtents(ctx, targetSize, currentSize, NewReadOnlyOeks(objExtents))
	if err != nil {
		return err
	}

	// delete and new extent is empty, don't need to change meta extent, just update file size
	if toDeleteFrom.IsEmpty() {
		lastEk := objExtents[len(objExtents)-1]
		if !newObjExtent.IsEmpty() || targetSize < lastEk.FileOffset+lastEk.Size {
			log.LogErrorf("ECStreamer truncateV2: shrink file, newObjExtent wrong or targetSize wrong, ino(%v) targetSize(%v) newObjExtent(%v) toDeleteFrom(%v)", ino, targetSize, newObjExtent, toDeleteFrom)
			return errors.New("shrink file, and has oeks, but newObjExtent is not empty or targetSize is wrong")
		}
		if err := s.mw.TruncateV2(ino, targetSize, fullPath, empty, empty); err != nil {
			return err
		}
		return s.updateMetaInfo(&targetSize)
	}

	// target < currentSize, and has oeks, and delete extent is not empty.
	if err := s.mw.TruncateV2(ino, targetSize, fullPath, newObjExtent, toDeleteFrom); err != nil {
		return err
	}
	return s.updateMetaInfo(&targetSize)
}

// inodeDeleted reports whether metanode no longer has this inode. Must not run under s.mu: InodeGet_ll is RPC.
func (s *ECStreamer) inodeDeleted() bool {
	if s == nil || s.mw == nil {
		return false
	}

	info, err := s.mw.InodeGet_ll(s.ino, false)
	if err == nil && info != nil {
		return false
	}
	if err == syscall.ENOENT || errors.Is(err, syscall.ENOENT) ||
		(err != nil && strings.Contains(err.Error(), syscall.ENOENT.Error())) {
		s.setError()
		return true
	}
	return false
}

// dropStreamer clears IO caches and nils RW under s.mu. Called when the inode is gone.
func (s *ECStreamer) dropStreamer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropIOCachesLocked()
	s.fReader = nil
	s.fWriter = nil
}

// handleIoError runs after s.mu is released. nil/EBADF: return as-is. io.EOF: poison, keep map. already inError: return, skip meta RPC.
// inode ENOENT: dropStreamer then removeDeletedStreamer. other errors with inode still present: return err, no poison.
func (s *ECStreamer) handleIoError(err error) error {
	// don't handle nil or EBADF
	if err == nil || errors.Is(err, syscall.EBADF) {
		return err
	}
	// EOF is a local error, set error and return, prevent later IO
	if errors.Is(err, io.EOF) {
		s.setError()
		log.LogWarnf("ECStreamer: IO failed, inode still present, poison streamer ino(%v) ref(%v) err(%v)",
			s.ino, atomic.LoadInt32(&s.refCnt), err)
		return err
	}
	// if already in error, return. reduce call metanode RPC
	if s.inError() {
		return err
	}

	// inode deleted, drop map entry. prevent later IO
	if s.inodeDeleted() {
		log.LogWarnf("ECStreamer: inode deleted, drop streamer ino(%v) ref(%v) err(%v)",
			s.ino, atomic.LoadInt32(&s.refCnt), err)
		s.dropStreamer()
		s.client.removeDeletedStreamer(s)
		return err
	}

	return err
}
