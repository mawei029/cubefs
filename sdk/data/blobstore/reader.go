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
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cubefs/cubefs/client/blockcache/bcache"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
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
	// hole marks [start,end) ranges that are not covered by any ObjExtent; Data is pre-filled with zeros and readSliceRange skips EBS.
	hole bool
}

func (s rwSlice) String() string {
	return fmt.Sprintf("rwSlice{fileOffset(%v),size(%v),rOffset(%v),rSize(%v),read(%v),hole(%v),objExtentKey(%v)}", s.fileOffset, s.size, s.rOffset, s.rSize, s.read, s.hole, s.objExtentKey)
}

func (reader *Reader) String() string {
	return fmt.Sprintf("Reader{address(%v),volName(%v),ino(%v),enableBcache(%v)},readConcurrency(%v)",
		&reader, reader.volName, reader.ino, reader.enableBcache, reader.readConcurrency)
}

type Reader struct {
	volName         string
	ino             uint64
	bc              *bcache.BcacheClient
	mw              *meta.MetaWrapper
	ebs             *BlobStoreClient
	readConcurrency int
	sync.Mutex
	close bool
	// ebsReadInflight：readEbsRange 并行读 EBS 时会临时 Unlock；Close 须等该窗口结束再置 close，避免与并发 Read/AIO 交错。
	// 不设「超时后仍关」：否则可能在读未完成时关 Reader，引发 EIO / 数据错乱（LTP growfiles）。
	ebsReadInflight int32
	objExtentKeys   []proto.ObjExtentKey
	enableBcache    bool
	valid           bool
	inflightCache   sync.Map
	limitManager    *manager.LimitManager

	// metaReportedSize comes from MetaWrapper.GetObjExtents Size.
	metaReportedSize uint64

	// blockSize + readBuf: when aheadRead is enabled and file size is larger than minReadAheadSize in EC/BlobStore reads,
	// merge multiple small FUSE reads (for example max_read=128KiB) into at most one EBS fetch of up to prefetchBufCap() (2×blockSize, e.g. 2×8MiB), cached in readBuf.
	// Semantics are similar to replica-stream AheadReadWindow, but implementation is Reader-side buffering instead of stream module logic.
	blockSize        int
	aheadReadEnable  bool
	minReadAheadSize uint64
	readBuf          []byte
	bufBaseOff       int   // file offset of readBuf[bufOff]
	bufValidLen      int   // valid bytes in readBuf[bufOff:bufOff+bufValidLen]
	prefetchReserved int64 // bytes reserved from global blob prefetch budget for this reader
	prefetchLimiter  *blobReadPrefetchLimiter

	// oec / 冷卷 Blob：与所属 ECStreamer 绑定；须非 nil（见 NewReader）。dirty / inoVersion / fileSize 与流协同。
	ecStreamer *ECStreamer
	// extentEpochSeen：上次 Refresh/加载 ObjExtents 时看到的 ECStreamer.extentMetaEpoch；Writer 提交 meta 后递增 epoch，读侧据此使缓存失效。
	extentEpochSeen uint32
}

type ClientConfig struct {
	VolName   string
	VolType   int
	BlockSize int
	Ino       uint64
	Bc        *bcache.BcacheClient
	Mw        *meta.MetaWrapper
	// LimitManager 与副本 ExtentClient / ECExtentClient 侧共享（例如 ec.LimitManager 或 oec.LimitManager）。
	LimitManager *manager.LimitManager
	// ECStreamer 必填：须与 OpenStreamWithArgs / inode 冷路径构造的 ClientConfig 一致传入非 nil，否则 NewReader/NewWriter 会 panic。
	ECStreamer      *ECStreamer
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
	if config.ECStreamer == nil {
		panic("blobstore.NewReader: ClientConfig.ECStreamer is required")
	}
	reader = new(Reader)

	reader.volName = config.VolName
	reader.ino = config.Ino
	reader.bc = config.Bc
	reader.ebs = config.Ebsc
	reader.mw = config.Mw
	reader.enableBcache = config.EnableBcache
	reader.readConcurrency = config.ReadConcurrency

	reader.limitManager = config.LimitManager
	reader.blockSize = config.BlockSize
	reader.aheadReadEnable = config.AheadReadEnable
	mra := config.MinReadAheadSize
	if mra < 0 {
		mra = 0
	}
	reader.minReadAheadSize = uint64(mra)
	reader.prefetchLimiter = getBlobReadPrefetchLimiter(config.PrefetchTotalMem)
	reader.ecStreamer = config.ECStreamer
	// readBuf is allocated lazily on first prefetch Read to avoid holding 2×EbsBlockSize per open file
	// when the file turns out tiny or ahead-read is off after fileSize check.
	return
}

// prefetchBufCap is readBuf capacity and the max single prefetch fetch size (two EBS logical blocks).
func (reader *Reader) prefetchBufCap() int {
	if reader.blockSize <= 0 {
		return 0
	}
	return reader.blockSize * 2
}

// ensurePrefetchBuf ensures readBuf capacity is at least prefetchBufCap (2×blockSize) and reserves global prefetch budget; on budget shortage it returns false and Read falls back to per-call readEbsRange.
func (reader *Reader) ensurePrefetchBuf() bool {
	capW := reader.prefetchBufCap()
	if capW <= 0 {
		return false
	}
	if reader.readBuf != nil && len(reader.readBuf) >= capW {
		return true
	}
	need := int64(capW) - reader.prefetchReserved
	if need > 0 && !reader.prefetchLimiter.tryAcquire(need) {
		log.LogDebugf("TRACE reader prefetch budget exhausted. ino(%v) need(%v)", reader.ino, need)
		return false
	}
	if need > 0 {
		reader.prefetchReserved += need
	}
	reader.readBuf = make([]byte, capW)
	return true
}

func (reader *Reader) Read(ctx context.Context, buf []byte, offset int, size int) (n int, err error) {
	beg := time.Now()
	defer func() {
		d := time.Since(beg)
		readerMetric.WithLabelValues("BlobstorRead").Observe(float64(d.Microseconds()))
		if d >= slowOpInfoThreshold {
			log.LogInfof("blobstore slow Reader.Read ino(%v) off(%v) dur(%v) retN(%v) err(%v)",
				reader.ino, offset, d, n, err)
		}
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
	if !valid {
		log.LogErrorf("Reader: invoke fileSize fail. ino(%v)  offset(%v) size(%v)", reader.ino, offset, size)
		return 0, syscall.EIO
	}
	// 与副本 Streamer.read 一致：起点已在文件逻辑尾之后时返回 0 字节、err=nil（POSIX：EOF 以 n==0 表示，不要求 io.EOF）。
	if uint64(offset) >= fileSize {
		return 0, nil
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

	// Prefetch gate: requires blockSize configured, mount-level aheadRead enabled, and file length > minReadAheadSize (same as stream-side policy to avoid over-buffering small files).
	usePrefetch := reader.blockSize > 0 && reader.aheadReadEnable && fileSize > reader.minReadAheadSize
	// case 1: prefetch disabled or file too small -> each call does one readEbsRange;
	if !usePrefetch {
		return normalReadFunc()
	}

	// case 2: prefetch budget reservation failed and global prefetch pool is exhausted: still read correctly but without readBuf, fallback as above;
	if !reader.ensurePrefetchBuf() {
		return normalReadFunc()
	}

	// case 3: single request >= blockSize: cannot fit prefetch window, read EBS range directly and invalidate buffer to avoid half-block logic.
	if size >= reader.blockSize {
		reader.invalidateReadBuf()
		return normalReadFunc()
	}

	// case 4: aggregate small reads with prefetch window: one readEbsRange fetches at most [offset, offset+fetch), fetch<=prefetchBufCap; repeated FUSE reads in same window copy from readBuf without extra EBS calls.
	ebsFetchSize := 0
	if reader.bufValidLen == 0 || offset < reader.bufBaseOff || offset >= reader.bufBaseOff+reader.bufValidLen {
		// rem: bytes from offset to logical file end (meta / ObjExtent tail / stream fileSize). fetch is capped by prefetchBufCap and rem,
		// so a 1MiB tail yields a 1MiB window; a 20MiB tail yields at most prefetchBufCap (e.g. 16MiB when blockSize is 8MiB).
		// readEbsRange/prepareEbsSlice materialize [offset, offset+fetch): ObjExtent ranges go through EBS Read; holes are zero-filled.
		// On success the merged buffer length equals fetch (the requested logical span); if offset is already at EOF, fetch<=0 above.
		reader.invalidateReadBuf()
		fetch := reader.prefetchBufCap()
		rem := int(fileSize - uint64(offset))
		if fetch > rem {
			fetch = rem
		}
		if fetch <= 0 {
			return 0, nil
		}
		data, err := reader.readEbsRange(ctx, offset, uint32(fetch))
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			log.LogErrorf("reader Read prefetch buffer is empty. ino(%v) offset(%v) fetchLen(%v)", reader.ino, offset, fetch)
			reader.invalidateReadBuf()
			return 0, syscall.EIO
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
		return 0, syscall.EIO
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

	// case 4.1: copy data from readBuf to destination buffer.
	winStart := offset - reader.bufBaseOff
	copy(buf, reader.readBuf[winStart:winStart+size])

	log.LogDebugf("TRACE reader Read done, temp ignore, ino(%v) off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) prefetchBufRemain(%v) path(prefetch)",
		reader.ino, offset, fuseReqSize, size, ebsFetchSize, reader.bufValidLen)
	return size, nil
}

// readEbsRange splits ranges by ObjExtent and runs readSliceRange in parallel; each slice eventually calls BlobStoreClient.Read (access.Get + ReadFull).
// Caller must hold reader.Mutex for prepareEbsSlice; the mutex is released for the duration of EBS/network I/O so RefreshExtents / EnsureAlignedForRead
// can update extent metadata (avoids FUSE hangs under concurrent read + write + getattr).
func (reader *Reader) readEbsRange(ctx context.Context, offset int, size uint32) ([]byte, error) {
	rSlices, err := reader.prepareEbsSlice(offset, size)
	log.LogDebugf("TRACE reader readEbsRange. ino(%v)  rSlices-length(%v) ", reader.ino, len(rSlices))
	if err != nil {
		return nil, err
	}
	sliceSize := len(rSlices)
	if sliceSize == 0 {
		return make([]byte, 0), nil
	}

	atomic.AddInt32(&reader.ebsReadInflight, 1)
	reader.Unlock()
	defer func() {
		reader.Lock()
		atomic.AddInt32(&reader.ebsReadInflight, -1)
	}()

	var wg sync.WaitGroup
	errCh := make(chan error, sliceSize)
	wg.Add(sliceSize)
	pool := New(reader.readConcurrency, sliceSize)
	defer pool.Close()
	for _, rs := range rSlices {
		rs := rs
		pool.Execute(rs, func(param *rwSlice) {
			defer wg.Done()
			reader.readSliceRange(ctx, param, errCh)
		})
	}
	wg.Wait()
	for i := 0; i < sliceSize; i++ {
		if err, ok := <-errCh; !ok || err != nil {
			return nil, err
		}
	}
	out := make([]byte, 0, size)
	for i := 0; i < sliceSize; i++ {
		out = append(out, rSlices[i].Data...)
	}
	return out, nil
}

// invalidateReadBuf clears prefetch window (without freeing underlying slice); next Read will run readEbsRange again.
func (reader *Reader) invalidateReadBuf() {
	reader.bufValidLen = 0
	reader.bufBaseOff = 0
}

func (reader *Reader) Close(ctx context.Context) {
	const waitStep = 2 * time.Millisecond
	reader.Lock()
	// 循环内会 Unlock：不得再套 defer Unlock，避免二次解锁。
	for atomic.LoadInt32(&reader.ebsReadInflight) > 0 {
		reader.Unlock()
		time.Sleep(waitStep)
		reader.Lock()
	}
	reader.close = true
	if reader.prefetchReserved > 0 {
		reader.prefetchLimiter.release(reader.prefetchReserved)
		reader.prefetchReserved = 0
	}
	reader.Unlock()
}

// prepareEbsSlice splits [offset, offset+size) into rwSlices: hole ranges are marked hole=true and zero-filled; overlapping ObjExtent ranges use
// rOffset/rSize to describe read region inside that extent object (must satisfy rOffset+rSize<=oek.Size, otherwise access.Get returns ErrIllegalArguments).
func (reader *Reader) prepareEbsSlice(offset int, size uint32) ([]*rwSlice, error) {
	if offset < 0 {
		return nil, syscall.EIO
	}
	if err := reader.ensureExtentsLoaded(); err != nil {
		return nil, err
	}

	fileSize, valid := reader.fileSize()
	log.LogDebugf("TRACE blobStore prepareEbsSlice Enter. ino(%v)  fileSize(%v) ", reader.ino, fileSize)
	if !valid {
		log.LogErrorf("Reader: invoke fileSize fail. ino(%v)  offset(%v) size(%v)", reader.ino, offset, size)
		return nil, syscall.EIO
	}
	log.LogDebugf("TRACE blobStore prepareEbsSlice. ino(%v)  offset(%v) size(%v)", reader.ino, offset, size)
	// 与副本 Streamer.read 对「空洞请求且 FileOffset > filesize」一致：无切片、无错误，readEbsRange 得到空结果。
	if uint64(offset) >= fileSize {
		return nil, nil
	}

	if uint64(offset)+uint64(size) > fileSize {
		size = uint32(fileSize - uint64(offset))
	}
	start := uint64(offset)
	end := start + uint64(size)

	keys := append([]proto.ObjExtentKey(nil), reader.objExtentKeys...)
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].FileOffset < keys[j].FileOffset
	})

	chunks := make([]*rwSlice, 0)
	cur := start
	for i := range keys {
		oek := keys[i]
		ekEnd := oek.FileOffset + uint64(oek.Size)
		if ekEnd <= cur {
			continue
		}
		if oek.FileOffset >= end {
			break
		}
		if cur < oek.FileOffset {
			holeLen := oek.FileOffset - cur
			chunks = append(chunks, &rwSlice{
				hole:       true,
				fileOffset: cur,
				rSize:      uint32(holeLen),
				Data:       make([]byte, holeLen),
			})
			cur = oek.FileOffset
		}
		ov := end
		if ekEnd < ov {
			ov = ekEnd
		}
		if cur >= ov {
			continue
		}
		rOff := cur - oek.FileOffset
		rSz := ov - cur
		chunks = append(chunks, &rwSlice{
			index:        i,
			fileOffset:   oek.FileOffset,
			size:         uint32(oek.Size),
			rOffset:      rOff,
			rSize:        uint32(rSz),
			objExtentKey: oek,
			Data:         make([]byte, rSz),
		})
		cur = ov
		if cur >= end {
			break
		}
	}
	if cur < end {
		holeLen := end - cur
		chunks = append(chunks, &rwSlice{
			hole:       true,
			fileOffset: cur,
			rSize:      uint32(holeLen),
			Data:       make([]byte, holeLen),
		})
	}

	log.LogDebugf("TRACE blobStore prepareEbsSlice Exit. ino(%v)  offset(%v) size(%v) rwSlices_len(%v)", reader.ino, offset, size, len(chunks))
	return chunks, nil
}

// readSliceRange handles one rwSlice in worker pool: try block-cache hit first, otherwise throttle and call ebs.Read for the single ObjExtent slice.
// errCh must be buffered with capacity >= 1; sends exactly one result per invocation.
func (reader *Reader) readSliceRange(ctx context.Context, rs *rwSlice, errCh chan error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			errCh <- fmt.Errorf("blobstore readSliceRange panic: %v", r)
			panic(r)
		}
	}()
	if rs.hole {
		log.LogDebugf("TRACE blobStore readSliceRange hole skip EBS. ino(%v) len(%v)", reader.ino, rs.rSize)
		errCh <- nil
		return
	}
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
			if readN == int(rs.rSize) {

				// L1 cache hit.
				metric := exporter.NewTPCnt("L1CacheGetHit")
				stat.EndStat("CacheHit-L1", nil, bgTime, 1)
				defer func() {
					metric.SetWithLabels(err, map[string]string{exporter.Vol: reader.volName})
				}()

				copy(rs.Data, buf)
				errCh <- nil
				return
			}
		}
	}

	readLimitOn := false
	if !readLimitOn && reader.limitManager != nil {
		reader.limitManager.ReadAlloc(ctx, int(rs.rSize))
	}

	t0 := time.Now()
	_, err = reader.ebs.Read(ctx, reader.volName, buf, rs.rOffset, uint64(rs.rSize), rs.objExtentKey)
	if d := time.Since(t0); d >= slowOpInfoThreshold {
		log.LogInfof("blobstore slow ebs.Read ino(%v) fileOff(%v) rOff(%v) rSize(%v) dur(%v) err(%v)",
			reader.ino, rs.fileOffset, rs.rOffset, rs.rSize, d, err)
	}
	if err != nil {
		errCh <- err
		return
	}
	read := copy(rs.Data, buf)
	errCh <- nil

	// When block cache is enabled and client exists: asynchronously read full ObjExtent and Put into L1; otherwise return directly (this path no longer triggers asyncCache by mistake).
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

	// Only one async refill is allowed per cacheKey to avoid duplicated concurrent EBS reads.
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

// ensureExtentsLoaded fetches ObjExtents from meta when they were not loaded successfully yet (retry when valid=false).
// Keep historical behavior: return syscall.EIO to upper layers on fetch failure for easier FUSE-path handling.
func (reader *Reader) ensureExtentsLoaded() error {
	if reader.valid {
		if atomic.LoadUint32(&reader.ecStreamer.extentMetaEpoch) != reader.extentEpochSeen {
			reader.valid = false
		}
		if reader.valid {
			return nil
		}
	}
	if err := reader.refreshEbsExtents(); err != nil {
		return syscall.EIO
	}
	return nil
}

// EnsureAlignedForRead 比对 InodeGet 的 Generation/Size 与流上 inoVersion/fileSize 及 Reader cache；不一致时 RefreshExtents。
// skipDirtyFlush 为真表示调用方已在 ECStreamer.readAfterFlush 中执行过 ensureReadViewCurrentLocked，此处不再二次刷 dirty（且调用方已持 s.mu）。
func (reader *Reader) EnsureAlignedForRead(inodeGen, inodeSize uint64) error {
	return reader.ensureAlignedForRead(inodeGen, inodeSize, false)
}

func (reader *Reader) ensureAlignedForRead(inodeGen, inodeSize uint64, skipDirtyFlush bool) error {
	reader.Lock()
	es := reader.ecStreamer
	stale := !reader.valid
	if !stale {
		streamSz := atomic.LoadUint64(&es.fileSize)
		// inodeSize 来自 File.Read 合并后的读上界（InodeGet + fileSizeVersion2）；若流上仍保留截断前的更大 logical tail，
		// 仅靠「streamSz < inodeSize」无法失效缓存（LTP ftest01/03/05/07：path truncate / ftruncate 与随机读交错）。
		stale = atomic.LoadUint32(&es.dirty) != 0 ||
			atomic.LoadUint64(&es.inoVersion) < inodeGen ||
			streamSz < inodeSize ||
			streamSz > inodeSize
	}
	reader.Unlock()
	if !stale {
		return nil
	}
	if !skipDirtyFlush && atomic.LoadUint32(&es.dirty) != 0 && es.hasReaderForViewSync() {
		if err := es.ensureReadViewCurrentExternal(context.Background()); err != nil {
			return err
		}
	}
	if _, err := reader.RefreshExtents(); err != nil {
		return err
	}
	reader.SyncInodeView(inodeGen, inodeSize)
	return nil
}

// SyncInodeView updates inode-view anchor after alignment with meta (for example via RefreshExtents), preventing next Read from false stale detection.
func (reader *Reader) SyncInodeView(inodeGen, inodeSize uint64) {
	es := reader.ecStreamer
	for {
		oldG := atomic.LoadUint64(&es.inoVersion)
		nextG := oldG
		if inodeGen > nextG {
			nextG = inodeGen
		}
		if atomic.CompareAndSwapUint64(&es.inoVersion, oldG, nextG) {
			break
		}
	}
	for {
		oldS := atomic.LoadUint64(&es.fileSize)
		nextS := oldS
		if inodeSize > nextS {
			nextS = inodeSize
		}
		// 与「只抬高不降低」相反：元数据或路径 truncate 后 inode 锚点变小，必须把流上 logical 顶降下来，
		// 否则 fileSize() 仍取 max(logical, streamSz) 会把已截断区间当仍有数据（洞区读到旧字节 / fstat 偏大）。
		if inodeSize < nextS {
			nextS = inodeSize
		}
		if atomic.CompareAndSwapUint64(&es.fileSize, oldS, nextS) {
			break
		}
	}
}

func (reader *Reader) refreshEbsExtents() error {
	t0 := time.Now()
	gen, sz, eks, oeks, err := reader.mw.GetObjExtents(reader.ino)
	if d := time.Since(t0); d >= slowOpInfoThreshold {
		log.LogInfof("blobstore slow GetObjExtents(refreshEbsExtents) ino(%v) dur(%v) err(%v)", reader.ino, d, err)
	}
	if err != nil {
		reader.valid = false
		log.LogErrorf("TRACE blobStore refreshEbsExtents error. ino(%v)  err(%v) ", reader.ino, err)
		return err
	}
	reader.valid = true
	reader.metaReportedSize = sz
	reader.objExtentKeys = oeks
	_ = eks
	reader.extentEpochSeen = atomic.LoadUint32(&reader.ecStreamer.extentMetaEpoch)
	lb := logicalReadBound(reader.metaReportedSize, reader.objExtentKeys)
	reader.ecStreamer.mergeMaxFileSize(lb)
	log.LogDebugf("TRACE blobStore refreshEbsExtents ok. ino(%v) gen(%v) metaSz(%v) objExtentKeys(%v) ",
		reader.ino, gen, sz, reader.objExtentKeys)
	return nil
}

// logicalReadBound returns max(inode logical size from meta, furthest byte covered by ObjExtents).
// Sparse files: meta may be 100 while extents only [20,40)—holes [0,20) and [40,100) must read as zeros in prepareEbsSlice.
func logicalReadBound(metaReportedSize uint64, objKeys []proto.ObjExtentKey) uint64 {
	logical := metaReportedSize
	for i := range objKeys {
		end := objKeys[i].FileOffset + objKeys[i].Size
		if end > logical {
			logical = end
		}
	}
	return logical
}

func (reader *Reader) fileSize() (uint64, bool) {
	if !reader.valid {
		return 0, false
	}
	// 逻辑文件尾以 ECStreamer.fileSize 为准（Open/写/截断/SyncInodeView/RefreshExtents 已维护）。
	return atomic.LoadUint64(&reader.ecStreamer.fileSize), true
}

func (reader *Reader) logicalReadBoundLocked() (uint64, bool) {
	if !reader.valid {
		return 0, false
	}
	return atomic.LoadUint64(&reader.ecStreamer.fileSize), true
}

// LogicalReadBound 返回与 Read/prepareEbsSlice 一致的逻辑文件尾（即 ECStreamer.fileSize）。
func (reader *Reader) LogicalReadBound() (uint64, bool) {
	if reader == nil {
		return 0, false
	}
	reader.Lock()
	defer reader.Unlock()
	return reader.logicalReadBoundLocked()
}

// RefreshExtents re-fetches ObjExtents from meta and updates cache; used after truncate so later Reads use updated extents and size.
// Do not call GetObjExtents while holding lock to avoid blocking other Reads.
// 返回值 gen 来自 GetObjExtents，供 ECStreamer 与 inode 代际对齐。
func (reader *Reader) RefreshExtents() (gen uint64, err error) {
	t0 := time.Now()
	gen, sz, eks, oeks, err := reader.mw.GetObjExtents(reader.ino)
	if d := time.Since(t0); d >= slowOpInfoThreshold {
		log.LogInfof("blobstore slow GetObjExtents(RefreshExtents) ino(%v) dur(%v) err(%v)", reader.ino, d, err)
	}
	if err != nil {
		reader.Lock()
		reader.valid = false
		reader.Unlock()
		log.LogErrorf("RefreshExtents: ino(%v) err(%v)", reader.ino, err)
		return 0, err
	}
	reader.Lock()
	reader.valid = true
	reader.metaReportedSize = sz
	reader.objExtentKeys = oeks
	_ = eks
	reader.extentEpochSeen = atomic.LoadUint32(&reader.ecStreamer.extentMetaEpoch)
	lb := logicalReadBound(reader.metaReportedSize, reader.objExtentKeys)
	reader.ecStreamer.mergeMaxFileSize(lb)
	reader.invalidateReadBuf()
	reader.Unlock()
	log.LogDebugf("RefreshExtents: ino(%v) gen(%v) metaSz(%v) objExtentKeysLen(%v)",
		reader.ino, gen, sz, len(oeks))
	return gen, nil
}
