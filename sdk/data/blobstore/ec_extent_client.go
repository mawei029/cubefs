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
//   - InodeGeneration：当前 InodeGet 返回的 info.Generation；与流上 inoVersion 做 max 合并，用于 Reader 与元数据视图对齐（非「权威代际」，写入后仍以 refreshExtents/InodeGet 抬高为准）。
type ECStreamOpenArgs struct {
	Ino             uint64
	OpenFlags       uint32 // 与 FUSE 打开模式一致；用于 noteOpenAccessMode 维护 rdonly（对齐副本 if !s.rdonly||s.dirty）。
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

// ECExtentClient：EC/Blob inode 级流注册表（与副本 ExtentClient 的 map+Open/Close/Evict 用法对齐）。
type ECExtentClient struct {
	mu           sync.RWMutex // 读多写少：查询/IO 路径 RLock，Open/Close/Evict 全表修改 Lock
	streamers    map[uint64]*ECStreamer
	LimitManager *manager.LimitManager
}

// NewObjExtentClient 创建客户端；cfg 可为 nil。
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

// OpenStreamWithArgs：EC/Blob 打开流（携带 EBS、池、块大小等）；数据面须调用本方法（ExtentClientAPI.OpenStream 四参在 EC 上返回 ENOTSUP）。
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
	} else {
		// CloseStream(ref==0) 会释放 RW 但保留 map 项；再次 Open 须重建 Reader/Writer。
		// s.mu.Lock()
		// s.applyOpenLocked(args)
		// s.mu.Unlock()
	}

	atomic.AddInt32(&s.refCnt, 1)
	log.LogDebugf("ECExtentClient OpenStreamWithArgs: ino(%v) ref(%v)", args.Ino, atomic.LoadInt32(&s.refCnt))
	return nil
}

// SetStreamer 将 ino 映射到指定 ECStreamer（不修改 refCnt，不执行 mergeOpenSnapshot）。
// 仅供测试或特殊装配；正常挂载须使用 OpenStreamWithArgs，否则与 CloseStream 引用计数及打开快照语义不一致。
func (c *ECExtentClient) SetStreamer(ino uint64, s *ECStreamer) {
	if c == nil || s == nil {
		log.LogErrorf("ECExtentClient SetStreamer: c is nil or s is nil, ino(%v)", ino)
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.streamers[ino] = s
}

// CloseStream 对齐副本 ExtentClient.CloseStream：不在本路径从 streamers 删表项（删表仅在 EvictStream 等 evict 流程）。
// - rdonly：仅 refCnt--（与副本仅 refcnt--、不 IssueReleaseRequest 一致），资源留待 EvictStream 在 ref==0 时回收。
// - 非 rdonly：对齐 IssueReleaseRequest → release()，refCnt--，ref==0 时 closeReaderWriterLocked 回收端点，仍保留 map 项直至 EvictStream。
func (c *ECExtentClient) CloseStream(ino uint64) error {
	c.mu.Lock()
	s, ok := c.streamers[ino]
	if !ok {
		c.mu.Unlock()
		return nil
	}
	if log.EnableDebug() {
		log.LogDebugf("CloseStream: stream(%s)", s.String())
	}
	c.mu.Unlock()

	n := atomic.AddInt32(&s.refCnt, -1)
	if n > 0 {
		if log.EnableDebug() {
			log.LogDebugf("ECExtentClient CloseStream: ref not zero, ino(%v) ref(%v)", ino, n)
		}
		return nil
	}
	if n < 0 {
		log.LogWarnf("ECExtentClient CloseStream: negative ref detected, ino(%v) ref(%v), reset to 0", ino, n)
		atomic.StoreInt32(&s.refCnt, 0)
		return nil
	}

	// 减减，见到为0做flush，调用streamer的flush
	if err := s.Flush(context.Background()); err != nil {
		atomic.AddInt32(&s.refCnt, 1)
		log.LogErrorf("ECExtentClient CloseStream: flush streamer failed, ino(%v) err(%v)", ino, err)
		return err
	}

	// 这里先不要了
	// if err := s.closeReaderWriterLocked(ino, context.Background()); err != nil {
	// 	atomic.AddInt32(&s.refCnt, 1)
	// 	log.LogErrorf("ECExtentClient CloseStream: close reader and writer failed, ino(%v) err(%v)", ino, err)
	// 	return err
	// }
	return nil
}

// EvictStream 对齐副本 ExtentClient.EvictStream：仅在本路径在 refCnt==0 时回收端点并 delete。
// 与副本 rdonly 分支 extent_client.go 638-643 一致：refCnt>0 时 LogWarnf("evict: streamer...") 并 return nil（不删表项），
// 不因可写/只读分支返回 EAGAIN——Forget 等调用方与副本 ec.EvictStream 一样按「本次未驱逐」处理即可。
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
		log.LogErrorf("ECExtentClient EvictStream: flush writer failed, ino(%v) err(%v)", ino, err)
		return err
	}

	if cur := c.streamers[ino]; cur == s {
		delete(c.streamers, ino)
	}
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

// RefCnt 与副本 ExtentClient.RefCnt 对齐。
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

// FileSize 返回 ECStreamer 当前时刻逻辑文件尾与 inoVersion；语义对齐副本 ExtentClient.FileSize（Extents.Size）。
// 读路径与未携带 inode 视图的调用方使用；fstat/getattr 请用 FstatSizeView。
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

// Read 与副本 ExtentClient.Read 签名一致；读前在 ECStreamer 内对齐视图，逻辑尾以 FileSizeView 为准。
func (c *ECExtentClient) Read(ino uint64, data []byte, offset int, size int, poolId uint8, isMigration bool) (int, error) {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()

	if s == nil || !ok {
		log.LogErrorf("ECExtentClient ReadWithInodeView: stream not opened, ino(%v) offset(%v) size(%v)", ino, offset, size)
		return 0, syscall.EBADF
	}

	return s.Read(context.Background(), data, offset, size, poolId, isMigration)
}

// Write 与副本 ExtentClient.Write 参数顺序及含义对齐；waitForFlush 为真时 ECStreamer.WriteWithOpts 在成功后 ensureReadViewCurrent（Flush+Refresh）。
func (c *ECExtentClient) Write(ino uint64, offset int, data []byte, flags int, checkFunc func() error,
	poolId uint8, storageClass uint32, isMigration bool,
) (int, error) {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil || !ok {
		log.LogErrorf("ECExtentClient Write: stream not opened, ino(%v) offset(%v) len(%v) flags(%v)", ino, offset, len(data), flags)
		return 0, syscall.EBADF
	}
	return s.Write(context.Background(), offset, data, flags, checkFunc, storageClass, isMigration)
}

// Flush 与副本 ExtentClient.Flush 对齐（未打开流时返回 EBADF）。
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

// Truncate 与副本 ExtentClient.Truncate 对齐：要求 oec 上该 ino 已 Open 且存在 Writer（与 doECTruncateV2 先 ensureBlobStoreWriter 一致）。
// 调用链：Truncate -> ECStreamer.truncateV2Locked -> Writer.Flush / mw.GetObjExtents / Writer.TruncateV2 -> mw.TruncateV2。
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

// NeedsReadViewSync 为真表示读前须同步（ECStreamer.dirty==1）；无未同步写时恒为 false。
func (c *ECExtentClient) NeedsReadViewSync(ino uint64) bool {
	c.mu.RLock()
	s, ok := c.streamers[ino]
	c.mu.RUnlock()
	if s == nil && ok {
		log.LogErrorf("ECExtentClient NeedsReadViewSync: streamer not found, ino(%v)", ino)
		return false
	}
	return s.isDirty()
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

// Close 在进程/挂载退出等场景下清理本客户端注册的全部 ECStreamer，对齐副本 ExtentClient.Close 中
// 「在 streamerLock 下拷贝 inode 列表，再逐个 EvictStream」的核心行为。
// 不包含副本侧的 stopCh、WaitGroup、dataWrapper.Stop、RemoteCache.Stop；上层仍须按既有顺序关闭 MetaWrapper 等。
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
