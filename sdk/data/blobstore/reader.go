// Copyright 2022 The CubeFS Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package blobstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/client/blockcache/bcache"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
	"github.com/prometheus/client_golang/prometheus"
)

var readerMetric = prometheus.NewSummaryVec(
	prometheus.SummaryOpts{
		Namespace:  "cubefs",
		Subsystem:  "client",
		Name:       "reader_cost_time",
		Help:       "time cost in cubefs sdk",
		Objectives: map[float64]float64{0.5: 0.05, 0.75: 0.025, 0.9: 0.01, 0.95: 0.005, 0.99: 0.001, 0.999: 0.0001, 0.9999: 0.00001},
	}, []string{"api"})

func init() {
	prometheus.MustRegister(readerMetric)
}

type rwSlice struct {
	index        int
	fileOffset   uint64
	size         uint32
	rOffset      uint64
	rSize        uint32
	read         int
	Data         []byte
	objExtentKey proto.ObjExtentKey
}

func (s rwSlice) String() string {
	return fmt.Sprintf("rwSlice{fileOffset(%v),size(%v),rOffset(%v),rSize(%v),read(%v),objExtentKey(%v)}", s.fileOffset, s.size, s.rOffset, s.rSize, s.read, s.objExtentKey)
}

func (reader *Reader) String() string {
	return fmt.Sprintf("Reader{address(%v),volName(%v),volType(%v),ino(%v),fileSize(%v),enableBcache(%v),fileCache(%v)},readConcurrency(%v)",
		&reader, reader.volName, reader.volType, reader.ino, reader.fileLength, reader.enableBcache, reader.fileCache, reader.readConcurrency)
}

type Reader struct {
	volName         string
	volType         int
	ino             uint64
	err             chan error
	bc              *bcache.BcacheClient
	mw              *meta.MetaWrapper
	ec              *stream.ExtentClient
	ebs             *BlobStoreClient
	readConcurrency int
	wg              sync.WaitGroup
	sync.Mutex
	close         bool
	extentKeys    []proto.ExtentKey
	objExtentKeys []proto.ObjExtentKey
	enableBcache  bool
	fileCache     bool
	fileLength    uint64
	valid         bool
	inflightCache sync.Map
	limitManager  *manager.LimitManager

	// extentsGeneration / metaReportedSize：来自 MetaWrapper.GetObjExtents（与 proto.GetObjExtentsResponse 的 Generation、Size 一致），
	// 在 extent 列表变化时递增，可作「数据视图版本」指纹；成功 refresh 后有效。
	extentsGeneration uint64
	metaReportedSize  uint64
	// inodeViewGen / inodeViewSize：与 file 层 InodeGet 对齐的 inode 代数与长度；用于 EnsureAlignedForRead 判断 Reader 缓存是否落后于 meta。
	inodeViewGen  uint64
	inodeViewSize uint64

	// blockSize + readBuf：EC/BlobStore 读路径在「开启 aheadRead 且文件大于 minReadAheadSize」时，
	// 将多次 FUSE 小读（如 max_read=128KiB）合并为至多 blockSize（EbsBlockSize）一次的 EBS 拉取，结果缓存在 readBuf。
	// 与副本流 AheadReadWindow 的开关语义类似，实现是 Reader 内缓冲而非 stream 模块。
	blockSize        int
	aheadReadEnable  bool
	minReadAheadSize uint64
	readBuf          []byte
	bufBaseOff       int   // file offset of readBuf[bufOff]
	bufValidLen      int   // valid bytes in readBuf[bufOff:bufOff+bufValidLen]
	prefetchReserved int64 // bytes reserved from global blob prefetch budget for this reader
	prefetchLimiter  *blobReadPrefetchLimiter
}

type ClientConfig struct {
	VolName         string
	VolType         int
	BlockSize       int
	Ino             uint64
	Bc              *bcache.BcacheClient
	Mw              *meta.MetaWrapper
	Ec              *stream.ExtentClient
	Ebsc            *BlobStoreClient
	EnableBcache    bool
	WConcurrency    int
	ReadConcurrency int
	FileCache       bool
	FileSize        uint64
	PoolId          uint8

	AheadReadEnable  bool
	MinReadAheadSize int   // bytes; file must be larger than this to use prefetch (same semantics as stream)
	PrefetchTotalMem int64 // Blob prefetch global memory budget in bytes; shares the same knob source as stream AheadReadTotalMem.
}

type blobReadPrefetchLimiter struct {
	maxBytes  int64
	usedBytes int64
}

func (l *blobReadPrefetchLimiter) tryAcquire(n int64) bool {
	if l == nil || n <= 0 {
		return true
	}
	for {
		used := atomic.LoadInt64(&l.usedBytes)
		if used+n > l.maxBytes {
			return false
		}
		if atomic.CompareAndSwapInt64(&l.usedBytes, used, used+n) {
			return true
		}
	}
}

func (l *blobReadPrefetchLimiter) release(n int64) {
	if l == nil || n <= 0 {
		return
	}
	atomic.AddInt64(&l.usedBytes, -n)
}

var (
	blobReadPrefetchLimiterMu sync.Mutex
	blobReadPrefetchLimiterGV *blobReadPrefetchLimiter
)

func getBlobReadPrefetchLimiter(totalMem int64) *blobReadPrefetchLimiter {
	if totalMem <= 0 {
		return nil
	}
	blobReadPrefetchLimiterMu.Lock()
	defer blobReadPrefetchLimiterMu.Unlock()
	if blobReadPrefetchLimiterGV == nil {
		blobReadPrefetchLimiterGV = &blobReadPrefetchLimiter{maxBytes: totalMem}
		log.LogInfof("blob prefetch limiter enabled, totalMem(%v)", totalMem)
		return blobReadPrefetchLimiterGV
	}
	if blobReadPrefetchLimiterGV.maxBytes != totalMem {
		log.LogWarnf("blob prefetch limiter already initialized with totalMem(%v), ignore new totalMem(%v)",
			blobReadPrefetchLimiterGV.maxBytes, totalMem)
	}
	return blobReadPrefetchLimiterGV
}

func NewReader(config ClientConfig) (reader *Reader) {
	reader = new(Reader)

	reader.volName = config.VolName
	reader.volType = config.VolType
	reader.ino = config.Ino
	reader.bc = config.Bc
	reader.ec = config.Ec
	reader.ebs = config.Ebsc
	reader.mw = config.Mw
	reader.enableBcache = config.EnableBcache
	reader.readConcurrency = config.ReadConcurrency
	reader.fileCache = config.FileCache

	reader.limitManager = config.Ec.LimitManager
	reader.blockSize = config.BlockSize
	reader.aheadReadEnable = config.AheadReadEnable
	mra := config.MinReadAheadSize
	if mra < 0 {
		mra = 0
	}
	reader.minReadAheadSize = uint64(mra)
	reader.prefetchLimiter = getBlobReadPrefetchLimiter(config.PrefetchTotalMem)
	// readBuf is allocated lazily on first prefetch Read to avoid holding EbsBlockSize per open file
	// when the file turns out tiny or ahead-read is off after fileSize check.
	return
}

// ensurePrefetchBuf 保证 readBuf 至少为 blockSize，并向全局 prefetch 预算记账；预算不足时返回 false，Read 将退化为按次 readEbsRange。
func (reader *Reader) ensurePrefetchBuf() bool {
	if reader.blockSize <= 0 {
		return false
	}
	if reader.readBuf != nil && len(reader.readBuf) >= reader.blockSize {
		return true
	}
	need := int64(reader.blockSize) - reader.prefetchReserved
	if need > 0 && !reader.prefetchLimiter.tryAcquire(need) {
		log.LogDebugf("TRACE reader prefetch budget exhausted. ino(%v) need(%v)", reader.ino, need)
		return false
	}
	if need > 0 {
		reader.prefetchReserved += need
	}
	reader.readBuf = make([]byte, reader.blockSize)
	return true
}

func (reader *Reader) Read(ctx context.Context, buf []byte, offset int, size int) (int, error) {
	beg := time.Now()
	defer func() {
		readerMetric.WithLabelValues("BlobstorRead").Observe(float64(time.Since(beg).Microseconds()))
	}()

	if reader == nil {
		return 0, fmt.Errorf("reader is not opened yet")
	}
	if reader.close {
		return 0, os.ErrInvalid
	}

	reader.Lock()
	defer reader.Unlock()

	if size != len(buf) {
		size = len(buf)
	}
	if size == 0 {
		return 0, nil
	}
	fuseReqSize := size

	if err := reader.ensureExtentsLoaded(); err != nil {
		return 0, err
	}
	fileSize, valid := reader.fileSize()
	reader.fileLength = fileSize
	if !valid {
		log.LogErrorf("Reader: invoke fileSize fail. ino(%v)  offset(%v) size(%v)", reader.ino, offset, size)
		return 0, syscall.EIO
	}
	if uint64(offset) >= fileSize {
		return 0, io.EOF
	}
	if uint64(offset)+uint64(size) > fileSize {
		size = int(fileSize - uint64(offset))
	}

	normalReadFunc := func() (int, error) {
		data, err := reader.readEbsRange(ctx, offset, uint32(size))
		if err != nil {
			return 0, err
		}
		n := copy(buf, data)
		log.LogDebugf("TRACE reader Read done ino(%v) off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) path(no-prefetch)",
			reader.ino, offset, fuseReqSize, n, len(data))
		return n, nil
	}

	// 预读门控：必须配置 blockSize、挂载打开 aheadRead，且文件长度大于 minReadAheadSize（与 stream 侧一致，避免小文件多占缓冲）。
	usePrefetch := reader.blockSize > 0 && reader.aheadReadEnable && fileSize > reader.minReadAheadSize
	// case 1: 未开预读或文件过小 → 每调一次 readEbsRange;
	if !usePrefetch {
		return normalReadFunc()
	}

	// case 2: 预读预算失败, 全局 prefetch 内存池用尽：本趟仍保证读对，但不建 readBuf → 同上兜底；
	if !reader.ensurePrefetchBuf() {
		return normalReadFunc()
	}

	// case3: 单次请求不小于 blockSize：无法放入预读窗口，直接走 EBS 范围读并丢弃缓冲，避免半块逻辑
	if size >= reader.blockSize {
		reader.invalidateReadBuf()
		return normalReadFunc()
	}

	// case4: 聚合小读 预读窗口：一次 readEbsRange 最多拉 [offset, offset+fetch)，fetch≤blockSize；同一窗口内多次 FUSE Read 只拷 readBuf 不重复访问 EBS。
	ebsFetchSize := 0
	if reader.bufValidLen == 0 || offset < reader.bufBaseOff || offset >= reader.bufBaseOff+reader.bufValidLen {
		// 缓冲空、或请求落在当前缓冲窗口外 → 作废旧窗口，按新 offset 重新拉一块
		reader.invalidateReadBuf()
		fetch := reader.blockSize
		rem := int(fileSize - uint64(offset))
		if fetch > rem {
			fetch = rem
		}
		if fetch <= 0 {
			return 0, io.EOF
		}
		data, err := reader.readEbsRange(ctx, offset, uint32(fetch))
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			log.LogErrorf("reader Read prefetch buffer is empty. ino(%v) offset(%v) fetchLen(%v)", reader.ino, offset, fetch)
			reader.invalidateReadBuf()
			return 0, io.EOF
		}
		ebsFetchSize = len(data)
		if len(data) > len(reader.readBuf) {
			log.LogInfof("extend reader Read prefetch buffer. ino(%v) offset(%v) fetchLen(%v) readBufLen(%v)", reader.ino, offset, len(data), len(reader.readBuf))
			reader.readBuf = make([]byte, len(data))
		}
		copy(reader.readBuf, data)
		reader.bufBaseOff = offset
		reader.bufValidLen = len(data)
		log.LogDebugf("TRACE reader Read prefetch. ino(%v) offset(%v) fetchLen(%v)", reader.ino, offset, len(data))
	}

	if reader.bufValidLen <= 0 {
		log.LogErrorf("reader Read prefetch buffer is invalid. ino(%v) offset(%v) bufValidLen(%v)", reader.ino, offset, reader.bufValidLen)
		return 0, io.EOF
	}

	if offset < reader.bufBaseOff || offset >= reader.bufBaseOff+reader.bufValidLen {
		log.LogErrorf("reader Read prefetch buffer is out of range. ino(%v) offset(%v) bufBaseOff(%v) bufValidLen(%v)", reader.ino, offset, reader.bufBaseOff, reader.bufValidLen)
		return 0, syscall.EIO
	}
	if size > reader.bufValidLen-(offset-reader.bufBaseOff) {
		log.LogWarnf("reader Read prefetch buffer is too small. ino(%v) offset(%v) bufBaseOff(%v) bufValidLen(%v)", reader.ino, offset, reader.bufBaseOff, reader.bufValidLen)
		reader.invalidateReadBuf()
		return normalReadFunc()
	}

	// case4.1 从 readBuf 拷出数据到目的地buf
	winStart := offset - reader.bufBaseOff
	copy(buf, reader.readBuf[winStart:winStart+size])

	log.LogDebugf("TRACE reader Read done, temp ignore, ino(%v) off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) prefetchBufRemain(%v) path(prefetch)",
		reader.ino, offset, fuseReqSize, size, ebsFetchSize, reader.bufValidLen)
	return size, nil
}

// readEbsRange 按 ObjExtent 切分并行 readSliceRange，每个切片最终调用 BlobStoreClient.Read（access.Get + ReadFull）。
// 调用方须已持 reader.Mutex。
func (reader *Reader) readEbsRange(ctx context.Context, offset int, size uint32) ([]byte, error) {
	rSlices, err := reader.prepareEbsSlice(offset, size)
	log.LogDebugf("TRACE reader readEbsRange. ino(%v)  rSlices-length(%v) ", reader.ino, len(rSlices))
	if err != nil {
		return nil, err
	}
	sliceSize := len(rSlices)
	if sliceSize > 0 {
		reader.wg.Add(sliceSize)
		pool := New(reader.readConcurrency, sliceSize)
		defer pool.Close()
		reader.err = make(chan error, sliceSize)
		for _, rs := range rSlices {
			pool.Execute(rs, func(param *rwSlice) {
				reader.readSliceRange(ctx, param)
			})
		}

		reader.wg.Wait()
		for i := 0; i < sliceSize; i++ {
			if err, ok := <-reader.err; !ok || err != nil {
				return nil, err
			}
		}
		close(reader.err)
	}
	out := make([]byte, 0, size)
	for i := 0; i < sliceSize; i++ {
		out = append(out, rSlices[i].Data...)
	}
	return out, nil
}

// invalidateReadBuf 清空预读窗口（不释放底层切片），下次 Read 会重新 readEbsRange。
func (reader *Reader) invalidateReadBuf() {
	reader.bufValidLen = 0
	reader.bufBaseOff = 0
}

func (reader *Reader) Close(ctx context.Context) {
	reader.Lock()
	reader.close = true
	if reader.prefetchReserved > 0 {
		reader.prefetchLimiter.release(reader.prefetchReserved)
		reader.prefetchReserved = 0
	}
	reader.Unlock()
}

// prepareEbsSlice 将 [offset, offset+size) 切成若干 rwSlice，每个对应一段 ObjExtentKey 内的连续区间，供并行 readSliceRange。
// ObjExtents 须已由 ensureExtentsLoaded / RefreshExtents 加载；此处不再重复调用 GetObjExtents（已去掉历史上的二次 refresh）。
func (reader *Reader) prepareEbsSlice(offset int, size uint32) ([]*rwSlice, error) {
	if offset < 0 {
		return nil, syscall.EIO
	}
	if err := reader.ensureExtentsLoaded(); err != nil {
		return nil, err
	}
	chunks := make([]*rwSlice, 0)
	endflag := false
	selected := false

	fileSize, valid := reader.fileSize()
	reader.fileLength = fileSize
	log.LogDebugf("TRACE blobStore prepareEbsSlice Enter. ino(%v)  fileSize(%v) ", reader.ino, fileSize)
	if !valid {
		log.LogErrorf("Reader: invoke fileSize fail. ino(%v)  offset(%v) size(%v)", reader.ino, offset, size)
		return nil, syscall.EIO
	}
	log.LogDebugf("TRACE blobStore prepareEbsSlice. ino(%v)  offset(%v) size(%v)", reader.ino, offset, size)
	if uint64(offset) >= fileSize {
		return nil, io.EOF
	}

	start := uint64(offset)
	if uint64(offset)+uint64(size) > fileSize {
		size = uint32(fileSize - uint64(offset))
	}
	end := uint64(offset + int(size))
	for index, oek := range reader.objExtentKeys {
		rs := &rwSlice{}
		selected = false
		if oek.FileOffset <= start && start < oek.FileOffset+(oek.Size) {
			rs.index = index
			rs.fileOffset = oek.FileOffset
			rs.size = uint32(oek.Size)
			rs.rOffset = start - oek.FileOffset
			rs.rSize = uint32(oek.FileOffset + oek.Size - start)
			selected = true
		}
		if end <= oek.FileOffset+oek.Size {
			rs.rSize = uint32(end - start)
			selected = true
			endflag = true
		}
		if selected {
			rs.objExtentKey = oek
			rs.Data = make([]byte, rs.rSize)
			start = oek.FileOffset + oek.Size
			chunks = append(chunks, rs)
			log.LogDebugf("TRACE blobStore prepareEbsSlice. ino(%v)  offset(%v) size(%v) rwSlice(%v)", reader.ino, offset, size, rs)
		}
		if endflag {
			break
		}
	}
	log.LogDebugf("TRACE blobStore prepareEbsSlice Exit. ino(%v)  offset(%v) size(%v) rwSlices(%v)", reader.ino, offset, size, chunks)
	return chunks, nil
}

// readSliceRange 在任务池里处理单个 rwSlice：先尝试块缓存命中，否则限流后对单段 ObjExtent 调用 ebs.Read。
func (reader *Reader) readSliceRange(ctx context.Context, rs *rwSlice) (err error) {
	defer reader.wg.Done()
	log.LogDebugf("TRACE blobStore readSliceRange Enter. ino(%v)  rs.fileOffset(%v),rs.rOffset(%v),rs.rSize(%v) ", reader.ino, rs.fileOffset, rs.rOffset, rs.rSize)
	cacheKey := util.GenerateKey(reader.volName, reader.ino, rs.fileOffset)
	log.LogDebugf("TRACE blobStore readSliceRange. ino(%v)  cacheKey(%v) ", reader.ino, cacheKey)
	buf := make([]byte, rs.rSize)
	var readN int

	bgTime := stat.BeginStat()
	stat.EndStat("CacheGet", nil, bgTime, 1)
	metric := exporter.NewTPCnt("CacheGet")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: reader.volName})
	}()

	// read local cache
	if reader.enableBcache {
		readN, err = reader.bc.Get(reader.volName, cacheKey, buf, rs.rOffset, rs.rSize)
		if err == nil {
			reader.ec.BcacheHealth = true
			if readN == int(rs.rSize) {

				// L1 cache hit.
				metric := exporter.NewTPCnt("L1CacheGetHit")
				stat.EndStat("CacheHit-L1", nil, bgTime, 1)
				defer func() {
					metric.SetWithLabels(err, map[string]string{exporter.Vol: reader.volName})
				}()

				copy(rs.Data, buf)
				reader.err <- nil
				return
			}
		}
	}

	readLimitOn := false
	if !readLimitOn {
		reader.limitManager.ReadAlloc(ctx, int(rs.rSize))
	}

	_, err = reader.ebs.Read(ctx, reader.volName, buf, rs.rOffset, uint64(rs.rSize), rs.objExtentKey)
	if err != nil {
		reader.err <- err
		return
	}
	read := copy(rs.Data, buf)
	reader.err <- nil

	// 开启块缓存且客户端存在时：异步按整 ObjExtent 读入并 Put L1；否则直接返回（本路径不再误触发 asyncCache）。
	if !reader.needCacheL1() || reader.bc == nil {
		log.LogDebugf("TRACE blobStore readSliceRange exit without cache. read counter=%v", read)
		return nil
	}

	asyncCtx := context.Background()
	go reader.asyncCache(asyncCtx, cacheKey, rs.objExtentKey)

	log.LogDebugf("TRACE blobStore readSliceRange exit with cache. read counter=%v", read)
	return nil
}

func (reader *Reader) asyncCache(ctx context.Context, cacheKey string, objExtentKey proto.ObjExtentKey) {
	var err error
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("read-async-cache", err, bgTime, 1)
	}()

	log.LogDebugf("TRACE blobStore asyncCache Enter. cacheKey=%v", cacheKey)

	// 同一 cacheKey 仅允许一处异步回填，避免并发重复读 EBS。
	if _, ok := reader.inflightCache.Load(cacheKey); ok {
		return
	}

	reader.inflightCache.Store(cacheKey, true)
	defer reader.inflightCache.Delete(cacheKey)

	buf := make([]byte, objExtentKey.Size)
	read, err := reader.ebs.Read(ctx, reader.volName, buf, 0, uint64(len(buf)), objExtentKey)
	if err != nil || read != len(buf) {
		log.LogErrorf("ERROR blobStore asyncCache fail, size no match. cacheKey=%v, objExtentKey.size=%v, read=%v",
			cacheKey, len(buf), read)
		return
	}

	if reader.needCacheL1() {
		reader.bc.Put(reader.volName, cacheKey, buf)
	}

	log.LogDebugf("TRACE blobStore asyncCache(L1) Exit. cacheKey=%v", cacheKey)
}

func (reader *Reader) needCacheL1() bool {
	return reader.enableBcache
}

// ensureExtentsLoaded 在尚未成功拉取 ObjExtents 时向 meta 拉取一次（valid=false 时重试）。
// 与历史行为一致：拉取失败时对上层返回 syscall.EIO，便于 FUSE 路径处理。
func (reader *Reader) ensureExtentsLoaded() error {
	if reader.valid {
		return nil
	}
	if err := reader.refreshEbsExtents(); err != nil {
		return syscall.EIO
	}
	return nil
}

// EnsureAlignedForRead 用 file 层 InodeGet 得到的 Generation/Size 与 Reader 内缓存对比；不一致则 RefreshExtents，
// 用于写入或截断后 inode 与 ObjExtents 已前进但 Reader 仍持有旧 extent 列表的情况。
func (reader *Reader) EnsureAlignedForRead(inodeGen, inodeSize uint64) error {
	var stale bool
	reader.Lock()
	stale = !reader.valid || reader.metaReportedSize != inodeSize || reader.inodeViewGen != inodeGen
	reader.Unlock()
	if !stale {
		return nil
	}
	if err := reader.RefreshExtents(); err != nil {
		return err
	}
	reader.Lock()
	reader.inodeViewGen = inodeGen
	reader.inodeViewSize = inodeSize
	reader.Unlock()
	return nil
}

// SyncInodeView 在已通过 RefreshExtents 等路径与 meta 对齐后，更新 inode 视图锚点，避免下一次 Read 误判为落后。
func (reader *Reader) SyncInodeView(inodeGen, inodeSize uint64) {
	reader.Lock()
	defer reader.Unlock()
	reader.inodeViewGen = inodeGen
	reader.inodeViewSize = inodeSize
}

func (reader *Reader) refreshEbsExtents() error {
	gen, sz, eks, oeks, err := reader.mw.GetObjExtents(reader.ino)
	if err != nil {
		reader.valid = false
		log.LogErrorf("TRACE blobStore refreshEbsExtents error. ino(%v)  err(%v) ", reader.ino, err)
		return err
	}
	reader.valid = true
	reader.extentsGeneration = gen
	reader.metaReportedSize = sz
	reader.extentKeys = eks
	reader.objExtentKeys = oeks
	log.LogDebugf("TRACE blobStore refreshEbsExtents ok. ino(%v) gen(%v) metaSz(%v) extentKeys(%v)  objExtentKeys(%v) ",
		reader.ino, gen, sz, reader.extentKeys, reader.objExtentKeys)
	return nil
}

func (reader *Reader) fileSize() (uint64, bool) {
	objKeys := reader.objExtentKeys
	if !reader.valid {
		return 0, false
	}
	if len(objKeys) > 0 {
		lastIndex := len(objKeys) - 1
		return objKeys[lastIndex].FileOffset + objKeys[lastIndex].Size, true
	}
	return 0, true
}

// RefreshExtents 从 meta 重新拉取 ObjExtents 并更新缓存，供 Truncate 后调用，使后续 Read 使用新 extent 与 size。
// 不在持锁状态下调用 GetObjExtents，避免阻塞其他 Read。
func (reader *Reader) RefreshExtents() error {
	gen, sz, eks, oeks, err := reader.mw.GetObjExtents(reader.ino)
	if err != nil {
		reader.Lock()
		reader.valid = false
		reader.Unlock()
		log.LogErrorf("RefreshExtents: ino(%v) err(%v)", reader.ino, err)
		return err
	}
	reader.Lock()
	reader.valid = true
	reader.extentsGeneration = gen
	reader.metaReportedSize = sz
	reader.extentKeys = eks
	reader.objExtentKeys = oeks
	if len(oeks) > 0 {
		last := oeks[len(oeks)-1]
		reader.fileLength = last.FileOffset + last.Size
	}
	reader.invalidateReadBuf()
	reader.Unlock()
	log.LogDebugf("RefreshExtents: ino(%v) gen(%v) metaSz(%v) objExtentKeysLen(%v) fileLength(%v)",
		reader.ino, gen, sz, len(oeks), reader.fileLength)
	return nil
}
