package blobstore

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cubefs/cubefs/client/blockcache/bcache"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/log"
)

var _ stream.ExtentClientAPI = (*ECExtentClient)(nil)

// ObjExtentConfig 仅保留构造所需项；LimitManager 可与副本 ExtentClient 共享。
type ObjExtentConfig struct {
	LimitManager *manager.LimitManager
}

// ECStreamOpenArgs 由 client/fs 在打开 oec 流时组装（与副本 ExtentClient.OpenStream 相比，Blob/EC 需注入 EBS、池、块大小等）。
//
// FileSize / InodeGeneration 语义（供 OpenStreamWithArgs → mergeOpenSnapshot）：
//   - FileSize：逻辑文件长度快照。Open/Setattr 等路径通常取 fileSizeVersion2（inode.Size、oec 流上缓存、Writer.CacheFileSize() 等合并上界，避免读越界与 Attr 偏小）；Create 可取 info.Size。
//   - InodeGeneration：当前 InodeGet 返回的 info.Generation；与流上 inoVersion 做 max 合并，用于 Reader 与元数据视图对齐（非「权威代际」，写入后仍以 RefreshExtents/InodeGet 抬高为准）。
type ECStreamOpenArgs struct {
	Ino             uint64
	OpenFlags       uint32 // 保留：与 FUSE 打开模式一致；Reader/Writer 现由 OpenStreamWithArgs 内一并创建，本字段不参与端点选择。
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

	AheadReadEnable  bool
	MinReadAheadSize int
	PrefetchTotalMem int64
}

func (a ECStreamOpenArgs) toClientConfig(c *ECExtentClient, s *ECStreamer) ClientConfig {
	return ClientConfig{
		VolName:          a.VolName,
		VolType:          a.VolType,
		BlockSize:        a.BlockSize,
		Ino:              a.Ino,
		Bc:               a.Bc,
		Mw:               a.Mw,
		LimitManager:     c.LimitManager,
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

// ECExtentClient：EC/Blob inode 级流注册表（与副本 ExtentClient 的 map+Open/Close/Evict 用法对齐）。
type ECExtentClient struct {
	mu           sync.RWMutex // 读多写少：查询/IO 路径 RLock，Open/Close/Evict 全表修改 Lock
	streamers    map[uint64]*ECStreamer
	LimitManager *manager.LimitManager

	// BeforeEBSShrinkHook 在 Blob 缩容调用 Writer.TruncateV2 之前执行（与原先 file.doECTruncateV2 中 ec.OpenStream+Flush+defer Close 一致）。
	// 返回的 cleanup 在 Truncate 返回前必须调用；为 nil 时不做副本同步（测试或特殊场景）。
	BeforeEBSShrinkHook func(ino uint64, fullPath string) (cleanup func(), err error)
}

// NewObjExtentClient 创建客户端；cfg 可为 nil。
func NewObjExtentClient(cfg *ObjExtentConfig) *ECExtentClient {
	lm := (*manager.LimitManager)(nil)
	if cfg != nil {
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

// OpenStreamWithArgs：EC/Blob 打开流（携带 EBS、池、块大小等）；数据面须调用本方法（ExtentClientAPI.OpenStream 四参在 EC 上返回 ENOTSUP）。
func (c *ECExtentClient) OpenStreamWithArgs(args ECStreamOpenArgs) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	s, ok := c.streamers[args.Ino]
	if !ok {
		s = NewECStreamer(args.Ino, nil, nil)
		c.streamers[args.Ino] = s
		log.LogDebugf("ECExtentClient OpenStreamWithArgs: new ECStreamer ino(%v)", args.Ino)
	}

	atomic.AddInt32(&s.refCnt, 1)
	s.mergeOpenSnapshot(args.FileSize, args.InodeGeneration)
	cfg := args.toClientConfig(c, s)
	s.ensureOpenEndpoints(cfg)
	log.LogDebugf("ECExtentClient OpenStreamWithArgs: ino(%v) ref(%v)", args.Ino, atomic.LoadInt32(&s.refCnt))
	return nil
}

// SetStreamer 将 ino 映射到指定 ECStreamer（不修改 refCnt，不执行 mergeOpenSnapshot）。
// 仅供测试或特殊装配；正常挂载须使用 OpenStreamWithArgs，否则与 CloseStream 引用计数及打开快照语义不一致。
func (c *ECExtentClient) SetStreamer(ino uint64, s *ECStreamer) {
	if c == nil || s == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streamers[ino] = s
}

// OpenStream 实现 stream.ExtentClientAPI；Blob/EC 数据面须使用 OpenStreamWithArgs（本方法恒返回 ENOTSUP）。
func (c *ECExtentClient) OpenStream(ino uint64, openForWrite bool, isCache bool, fullPath string) error {
	_ = openForWrite
	_ = isCache
	_ = fullPath
	if log.EnableDebug() {
		log.LogDebugf("ECExtentClient.OpenStream: ino(%v) returns ENOTSUP — use OpenStreamWithArgs for Blob/EC", ino)
	}
	return syscall.ENOTSUP
}

// CloseStream：refCnt--，为 0 时回收 Reader/Writer 并删表项。
func (c *ECExtentClient) CloseStream(ino uint64) error {
	c.mu.Lock()
	s := c.streamers[ino]
	if s == nil {
		c.mu.Unlock()
		return nil
	}
	n := atomic.AddInt32(&s.refCnt, -1)
	if n > 0 {
		c.mu.Unlock()
		log.LogDebugf("ECExtentClient CloseStream: ino(%v) ref(%v)", ino, n)
		return nil
	}
	if n < 0 {
		log.LogWarnf("ECExtentClient CloseStream: negative ref detected, ino(%v) ref(%v), reset to 0", ino, n)
		atomic.StoreInt32(&s.refCnt, 0)
		c.mu.Unlock()
		return nil
	}
	c.mu.Unlock()

	w := s.Writer()
	if w != nil {
		if err := w.Flush(ino, context.Background()); err != nil {
			atomic.AddInt32(&s.refCnt, 1)
			log.LogErrorf("ECExtentClient CloseStream: flush writer failed, ino(%v) err(%v)", ino, err)
			return err
		}
		w.FreeCache()
	}
	if r := s.Reader(); r != nil {
		r.Close(context.Background())
	}

	c.mu.Lock()
	if cur := c.streamers[ino]; cur == s && atomic.LoadInt32(&s.refCnt) == 0 {
		s.Cleanup()
		delete(c.streamers, ino)
	}
	c.mu.Unlock()
	log.LogDebugf("ECExtentClient CloseStream: ino(%v) evicted", ino)
	return nil
}

// EvictStream 与副本 ExtentClient.EvictStream 语义对齐：无活跃引用时回收 Reader/Writer 并删表项。
func (c *ECExtentClient) EvictStream(ino uint64) error {
	c.mu.Lock()
	s := c.streamers[ino]
	if s == nil {
		c.mu.Unlock()
		return nil
	}
	if atomic.LoadInt32(&s.refCnt) > 0 {
		c.mu.Unlock()
		log.LogWarnf("ECExtentClient EvictStream: streamer still referenced, ino(%v) ref(%v)", ino, atomic.LoadInt32(&s.refCnt))
		return syscall.EAGAIN
	}
	c.mu.Unlock()

	if w := s.Writer(); w != nil {
		if err := w.Flush(ino, context.Background()); err != nil {
			log.LogErrorf("ECExtentClient EvictStream: flush writer failed, ino(%v) err(%v)", ino, err)
			return err
		}
		w.FreeCache()
	}
	if r := s.Reader(); r != nil {
		r.Close(context.Background())
	}

	c.mu.Lock()
	if cur := c.streamers[ino]; cur == s && atomic.LoadInt32(&s.refCnt) == 0 {
		s.Cleanup()
		delete(c.streamers, ino)
	}
	c.mu.Unlock()
	return nil
}

// GetStreamer 与副本 ExtentClient.GetStreamer 对齐（可能返回 nil）。
func (c *ECExtentClient) GetStreamer(ino uint64) *ECStreamer {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.streamers[ino]
}

func (c *ECExtentClient) Reader(ino uint64) *Reader {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		return nil
	}
	return s.Reader()
}

func (c *ECExtentClient) Writer(ino uint64) *Writer {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		return nil
	}
	return s.Writer()
}

// RefCnt 与副本 ExtentClient.RefCnt 对齐。
func (c *ECExtentClient) RefCnt(ino uint64) int32 {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		return 0
	}
	return atomic.LoadInt32(&s.refCnt)
}

// FileSize 返回流上维护的长度与 inoVersion；语义对齐副本 ExtentClient.FileSize。
func (c *ECExtentClient) FileSize(ino uint64) (size int, gen uint64, valid bool) {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		return 0, 0, false
	}
	return int(atomic.LoadUint64(&s.fileSize)), atomic.LoadUint64(&s.inoVersion), true
}

// Read 与副本 ExtentClient.Read 签名一致（内部使用 context.Background；取消语义由上层 FUSE 超时等保证）。
//
//go:noinline
func (c *ECExtentClient) Read(ino uint64, data []byte, offset int, size int, poolId uint8, isMigration bool) (int, error) {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		log.LogErrorf("ECExtentClient Read: stream not opened, ino(%v) offset(%v) size(%v)", ino, offset, size)
		return 0, syscall.EBADF
	}
	return s.Read(context.Background(), data, offset, size, poolId, isMigration)
}

// Write 与副本 ExtentClient.Write 参数顺序及含义对齐；waitForFlush 为真时 ECStreamer.WriteWithOpts 在成功后 ensureReadViewCurrent（Flush+Refresh）。
//
//go:noinline
func (c *ECExtentClient) Write(ino uint64, offset int, data []byte, flags int, checkFunc func() error,
	poolId uint8, storageClass uint32, isMigration, waitForFlush bool,
) (int, error) {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		log.LogErrorf("ECExtentClient Write: stream not opened, ino(%v) offset(%v) len(%v) flags(%v)", ino, offset, len(data), flags)
		return 0, syscall.EBADF
	}
	return s.WriteWithOpts(context.Background(), offset, data, flags, checkFunc, poolId, storageClass, isMigration, waitForFlush)
}

// NeedsReadViewSync 为真表示 ECStreamer.dirty 置位，读路径应先 Flush 再 EnsureAlignedForRead，避免在未刷写缓冲时仅 Refresh 导致不一致。
// O_RDWR 打开的文件在无未同步写时恒为 false，避免 LTP 等场景下每次读都走 Flush。
func (c *ECExtentClient) NeedsReadViewSync(ino uint64) bool {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		return false
	}
	return atomic.LoadUint32(&s.dirty) != 0
}

// Flush 与副本 ExtentClient.Flush 对齐（未打开流时返回 EBADF）。
func (c *ECExtentClient) Flush(ino uint64) error {
	c.mu.RLock()
	s := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil {
		log.LogErrorf("ECExtentClient Flush: stream not opened, ino(%v)", ino)
		return syscall.EBADF
	}
	return s.Flush(context.Background())
}

// Truncate 与副本 ExtentClient.Truncate 对齐：要求 oec 上该 ino 已 Open 且存在 Writer（与 doECTruncateV2 先 ensureBlobStoreWriter 一致）。
// 调用链：Truncate -> ECStreamer.truncateV2 -> Writer.Flush / mw.GetObjExtents / Writer.TruncateV2 -> mw.TruncateV2。
func (c *ECExtentClient) Truncate(mw *meta.MetaWrapper, parentIno uint64, ino uint64, size int, fullPath string) error {
	_ = parentIno
	if mw == nil {
		return syscall.EINVAL
	}
	s := c.GetStreamer(ino)
	if s == nil {
		log.LogErrorf("ECExtentClient Truncate: stream not opened, ino(%v)", ino)
		return syscall.EBADF
	}
	targetSize := uint64(size)
	return s.truncateV2(context.Background(), mw, ino, targetSize, fullPath, c.BeforeEBSShrinkHook)
}
