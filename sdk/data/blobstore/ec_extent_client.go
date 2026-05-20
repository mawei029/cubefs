package blobstore

import (
	"context"
	"hash"
	"io"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cubefs/cubefs/client/blockcache/bcache"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/log"
)

// ObjExtentConfig holds construction fields; LimitManager may be shared with replica ExtentClient.
type ObjExtentConfig struct {
	LimitManager *manager.LimitManager
}

// ECStreamOpenArgs is built by client/fs.openOECStream; Blob/EC do not use replica OpenStream(inode,...) API.
//
// FileSize/InodeGeneration set on first NewECStreamer; reuse only bumps refCnt, args do not overwrite stream view.
// CloseStream(refCnt==0) resets extents once; next IO refreshes meta; re-Open often Flush first.
type ECStreamOpenArgs struct {
	Ino             uint64
	OpenFlags       uint32 // FUSE open mode; reserved, OpenStreamWithArgs does not branch on it yet
	PoolId          uint8
	FileSize        uint64
	InodeGeneration uint64

	VolName         string
	VolType         int
	BlockSize       int
	Ebsc            *BlobStoreClient
	Bc              *bcache.BcacheClient
	Mw              *meta.MetaWrapper
	EnableBcache    bool
	WConcurrency    int
	ReadConcurrency int
	LimitManager    *manager.LimitManager

	AheadReadEnable  bool
	MinReadAheadSize int
	PrefetchTotalMem int64
}

func (a ECStreamOpenArgs) toClientConfig(s *ECStreamer) ClientConfig {
	return ClientConfig{
		VolName:          a.VolName,
		VolType:          a.VolType,
		BlockSize:        a.BlockSize,
		Ino:              a.Ino,
		Bc:               a.Bc,
		Mw:               a.Mw,
		LimitManager:     a.LimitManager,
		ECStreamer:       s,
		Ebsc:             a.Ebsc,
		EnableBcache:     a.EnableBcache,
		WConcurrency:     a.WConcurrency,
		ReadConcurrency:  a.ReadConcurrency,
		FileCache:        false,
		FileSize:         a.FileSize,
		PoolId:           a.PoolId,
		AheadReadEnable:  a.AheadReadEnable,
		MinReadAheadSize: a.MinReadAheadSize,
		PrefetchTotalMem: a.PrefetchTotalMem,
	}
}

// ECExtentClient is per-inode EC/Blob stream registry, lifecycle aligned with replica ExtentClient:
//
//	OpenStreamWithArgs — refCnt++ (NewECStreamer if needed)
//	CloseStream        — refCnt--; at zero: Flush + dropIOCaches + resetExtentsOnce, map entry and RW kept
//	EvictStream        — CloseReaderWriter and delete map entry only when refCnt==0
//	Close              — EvictStream all inodes (unmount/exit)
type ECExtentClient struct {
	mu           sync.RWMutex // IO lookup RLock; Open/Close/Evict map changes use Lock
	streamers    map[uint64]*ECStreamer
	LimitManager *manager.LimitManager
}

// NewObjExtentClient creates client; cfg may be nil.
func NewObjExtentClient(cfg ObjExtentConfig) *ECExtentClient {
	lm := (*manager.LimitManager)(nil)
	if cfg.LimitManager != nil {
		lm = cfg.LimitManager
	}
	if lm == nil {
		lm = manager.NewLimitManager(nil)
	}
	return &ECExtentClient{
		streamers:    make(map[uint64]*ECStreamer),
		LimitManager: lm,
	}
}

// OpenStreamWithArgs opens or reuses ECStreamer for ino and increments refCnt.
// New stream: build RW and initial fileSize/inoVersion; existing stream only bumps refCnt (view refreshed after CloseStream zeros ref).
func (c *ECExtentClient) OpenStreamWithArgs(args ECStreamOpenArgs) (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	s, ok := c.streamers[args.Ino]
	if !ok {
		s, err = NewECStreamer(args, nil, nil)
		if err != nil {
			return err
		}
		c.streamers[args.Ino] = s
		log.LogDebugf("ECExtentClient OpenStreamWithArgs: new ECStreamer ino(%v)", args.Ino)
	}

	atomic.AddInt32(&s.refCnt, 1)
	log.LogDebugf("ECExtentClient OpenStreamWithArgs: ino(%v) ref(%v)", args.Ino, atomic.LoadInt32(&s.refCnt))
	return nil
}

// SetStreamer maps ino to ECStreamer (no refCnt change, no open snapshot merge).
// Test injection only; production must use OpenStreamWithArgs for consistent refCnt lifecycle.
func (c *ECExtentClient) SetStreamer(ino uint64, s *ECStreamer) {
	if c == nil || s == nil {
		log.LogErrorf("ECExtentClient SetStreamer: c is nil or s is nil, ino(%v)", ino)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streamers[ino] = s
}

// CloseStream decrements refCnt; map entry removed only in EvictStream.
// refCnt>0: return; refCnt==0: Flush, dropIOCaches, resetExtentsOnce; RW objects kept for re-Open.
// On Flush failure refCnt is rolled back; negative refCnt logs Warn and still attempts Flush.
func (c *ECExtentClient) CloseStream(ino uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	s, ok := c.streamers[ino]
	if !ok {
		return nil
	}
	if log.EnableDebug() {
		log.LogDebugf("CloseStream: stream(%s)", s.String())
	}

	n := atomic.AddInt32(&s.refCnt, -1)
	if n > 0 {
		if log.EnableDebug() {
			log.LogDebugf("ECExtentClient CloseStream: ref not zero, ino(%v) ref(%v)", ino, n)
		}
		return nil
	}
	if n < 0 {
		log.LogWarnf("ECExtentClient CloseStream: negative ref detected, ino(%v) ref(%v), force flush", ino, n)
		atomic.StoreInt32(&s.refCnt, 0)
		// Same as n==0: best-effort Flush and dropIOCaches to avoid stale dirty data
	}

	if err := s.Flush(context.Background()); err != nil {
		atomic.AddInt32(&s.refCnt, 1)
		log.LogErrorf("ECExtentClient CloseStream: flush streamer failed, ino(%v) err(%v)", ino, err)
		return err
	}

	s.mu.Lock()
	s.dropIOCachesLocked()
	s.resetExtentsOnceLocked()
	s.mu.Unlock()
	return nil
}

// EvictStream closes RW and deletes map entry when refCnt==0; refCnt>0 warns and returns nil (entry kept for Forget retry).
func (c *ECExtentClient) EvictStream(ino uint64) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	s, ok := c.streamers[ino]
	if !ok {
		return nil
	}
	log.LogDebugf("EvictStream: stream(%v)", s.String())

	if atomic.LoadInt32(&s.refCnt) > 0 {
		log.LogWarnf("evict: streamer(%v) refcnt(%v)", s.String(), atomic.LoadInt32(&s.refCnt))
		return nil
	}

	if err := s.CloseReaderWriter(); err != nil {
		log.LogErrorf("ECExtentClient EvictStream: CloseReaderWriter failed, ino(%v) err(%v)", ino, err)
		return err
	}

	if cur := c.streamers[ino]; cur == s {
		delete(c.streamers, ino)
		s.fReader = nil
		s.fWriter = nil
	}
	return nil
}

// GetStreamer matches replica ExtentClient.GetStreamer (may be nil).
func (c *ECExtentClient) GetStreamer(ino uint64) *ECStreamer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.streamers[ino]
}

func (c *ECExtentClient) HasReader(ino uint64) bool {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	return ok && s != nil && s.fReader != nil
}

func (c *ECExtentClient) HasWriter(ino uint64) bool {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	return ok && s != nil && s.fWriter != nil
}

func (c *ECExtentClient) Reader(ino uint64) *Reader {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		if ok {
			log.LogErrorf("ECExtentClient GetStreamer: streamer not found, ino(%v)", ino)
		}
		return nil
	}
	return s.Reader()
}

func (c *ECExtentClient) Writer(ino uint64) *Writer {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		if ok {
			log.LogErrorf("ECExtentClient Writer: streamer not found, ino(%v)", ino)
		}
		return nil
	}
	return s.Writer()
}

// RefCnt matches replica ExtentClient.RefCnt.
func (c *ECExtentClient) RefCnt(ino uint64) int32 {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		if ok {
			log.LogErrorf("ECExtentClient RefCnt: streamer not found, ino(%v)", ino)
		}
		return 0
	}
	return atomic.LoadInt32(&s.refCnt)
}

// FileSize returns current logical tail and inoVersion; aligned with replica ExtentClient.FileSize.
// For callers without inode merge rules; fstat/getattr use client/fs fileSizeVersion2.
func (c *ECExtentClient) FileSize(ino uint64) (size int, gen uint64, valid bool) {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil || !ok {
		return 0, 0, false
	}
	size, gen = s.FileSizeView()
	return size, gen, true
}

// Read matches replica ExtentClient.Read signature; view synced inside ECStreamer; tail from FileSizeView.
func (c *ECExtentClient) Read(ino uint64, data []byte, offset int, size int) (int, error) {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()

	if s == nil || !ok {
		log.LogErrorf("ECExtentClient Read: stream not opened, ino(%v) offset(%v) size(%v)", ino, offset, size)
		return 0, syscall.EBADF
	}

	return s.Read(context.Background(), data, offset, size)
}

// Write matches replica ExtentClient.Write; O_SYNC flush is done by client/fs after Write.
func (c *ECExtentClient) Write(ino uint64, offset int, data []byte, flags int) (int, error) {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil || !ok {
		log.LogErrorf("ECExtentClient Write: stream not opened, ino(%v) offset(%v) len(%v) flags(%v)", ino, offset, len(data), flags)
		return 0, syscall.EBADF
	}
	return s.Write(context.Background(), offset, data, flags)
}

func (c *ECExtentClient) WriteFromReader(ctx context.Context, ino uint64, reader io.Reader, h hash.Hash) (uint64, error) {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil || !ok {
		log.LogErrorf("ECExtentClient WriteFromReader: stream not opened, ino(%v)", ino)
		return 0, syscall.EBADF
	}
	return s.WriteFromReader(ctx, reader, h)
}

// Flush matches replica ExtentClient.Flush (EBADF if stream not open).
func (c *ECExtentClient) Flush(ino uint64) error {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil || !ok {
		log.LogErrorf("ECExtentClient Flush: stream not opened, ino(%v)", ino)
		return syscall.EBADF
	}
	return s.Flush(context.Background())
}

// Truncate requires open stream with Writer: ECStreamer.truncateV2Locked → Writer → mw.TruncateV2.
func (c *ECExtentClient) Truncate(parentIno uint64, ino uint64, targetSize uint64, fullPath string) error {
	_ = parentIno
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()

	if s == nil || !ok {
		log.LogErrorf("ECExtentClient Truncate: stream not opened, ino(%v)", ino)
		return syscall.EBADF
	}

	return s.Truncate(context.Background(), targetSize, fullPath)
}

func (c *ECExtentClient) RefreshExtentsCache(ino uint64) error {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()

	if s == nil {
		if ok {
			log.LogErrorf("ECExtentClient RefreshExtentsCache: stream not opened, ino(%v)", ino)
			return syscall.EBADF
		}
		log.LogInfof("ECExtentClient RefreshExtentsCache: stream not opened, ino(%v)", ino)
		return nil
	}

	return s.RefreshExtentsCache()
}

// NeedsReadViewSync is true when read must sync (dirty==1); false when clean.
func (c *ECExtentClient) NeedsReadViewSync(ino uint64) bool {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		if ok {
			log.LogErrorf("ECExtentClient NeedsReadViewSync: streamer not found, ino(%v)", ino)
		}
		return false
	}
	return s.isDirty()
}

// OpenStream is not supported on Blob/EC; use OpenStreamWithArgs (always returns ENOTSUP).
func (c *ECExtentClient) OpenStream(ino uint64, openForWrite bool, isCache bool, fullPath string) error {
	_ = openForWrite
	_ = isCache
	_ = fullPath
	if log.EnableDebug() {
		log.LogDebugf("ECExtentClient.OpenStream: ino(%v) returns ENOTSUP — use OpenStreamWithArgs for Blob/EC", ino)
	}
	return syscall.ENOTSUP
}

// Close copies inode list then EvictStream each; aligned with replica stream teardown (no replica-only stopCh/RemoteCache).
func (c *ECExtentClient) Close() error {
	var inodes []uint64
	c.mu.Lock()
	inodes = make([]uint64, 0, len(c.streamers))
	for inode := range c.streamers {
		inodes = append(inodes, inode)
	}
	c.mu.Unlock()
	for _, inode := range inodes {
		_ = c.EvictStream(inode)
	}
	return nil
}
