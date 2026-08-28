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

// rwSlice is one logical read span：ObjExtent ranges use EBS; uncovered spans use hole=true with zero-filled Data and no EBS.
type rwSlice struct {
	index        int
	fileOffset   uint64
	size         uint32
	rOffset      uint64
	rSize        uint32
	read         int
	Data         []byte // subslice of readEbsRange dst; hole spans zeroed in readSliceRange
	objExtentKey proto.ObjExtentKey
	hole         bool
}

func (s rwSlice) String() string {
	return fmt.Sprintf("rwSlice{fileOffset(%v),size(%v),rOffset(%v),rSize(%v),read(%v),hole(%v),objExtentKey(%v)}", s.fileOffset, s.size, s.rOffset, s.rSize, s.read, s.hole, s.objExtentKey)
}

func (reader *Reader) String() string {
	return fmt.Sprintf("Reader{address(%v),volName(%v),ino(%v),enableBcache(%v)},readConcurrency(%v)",
		&reader, reader.ecStreamer.Volume(), reader.ecStreamer.Inode(), reader.enableBcache, reader.readConcurrency)
}

// aheadWin is one prefetch window: active (consumed) or standby (filled in background).
// Heap-allocated so promote() swaps pointers; an in-flight filler always writes this object.
type aheadWin struct {
	buf      []byte // allocated lazily on first prefetch Read to avoid holding prefetchCap per open file
	off      int    // file offset of buf[0]
	valid    int    // ready bytes
	inflight int32  // async fill in progress
	fillOff  int    // starting offset of the async fill
	fillLen  int    // length of the async fill
}

// aheadPair: active for sequential consume; standby for the next OEK window (always dual).
type aheadPair struct {
	active  *aheadWin
	standby *aheadWin
}

// Reader is bound to one inode ECStreamer: split reads by oeks, sparse zero-fill, optional prefetch and async L1 refill.
type Reader struct {
	bc              *bcache.BcacheClient
	readConcurrency int
	enableBcache    bool
	inflightCache   sync.Map // TODO: keep for now; dedupe asyncCache L1 refill per extent key
	limitManager    *manager.LimitManager

	ecStreamer *ECStreamer // required; logical tail/gen/oeks from stream (see NewReader)

	// prefetch conf and info
	prefetchConf
	prefetchInfo
}

// Prefetch (reader_prefetch.go): off unless AheadReadEnable.
// Heat after prefetchHeatBytes sequential; cool when not sequential and outside windows.
// Always dual windows async-fill ObjExtentKey spans from sorted oeks.
// Conf is immutable after NewReader; reserved uses atomics; limiter is CAS-based.
type prefetchConf struct {
	aheadReadEnable  bool
	minReadAheadSize uint64
	aheadWindowCnt   int
	prefetchReserved int64 // atomic; bytes reserved from global blobPreReadLimiter
	preReadLimiter   *blobPreReadLimiter
}

// Mutable prefetch state. Guarded by mu (embedded as Reader.mu).
type prefetchInfo struct {
	mu sync.Mutex

	hasLastRead  bool   // observeRead cursor initialized
	lastReadOff  int    // last request offset
	lastReadEnd  int    // last request end (exclusive)
	seqHeatBytes uint64 // sequential heat accumulator
	missStreak   uint32 // consecutive true prefetch misses since last hit/cool
	prefetchHit  bool   // true after at least one prefetch hit since last cool

	wins        aheadPair // active + standby; each holds one oek extent
	prefetchGen uint64    // incremented when prefetch buffer is invalidated/released
}

// ClientConfig builds Reader/Writer; production fills via ECStreamOpenArgs.toClientConfig.
type ClientConfig struct {
	VolName         string
	VolType         int
	BlockSize       int
	Ino             uint64
	Bc              *bcache.BcacheClient
	Mw              *meta.MetaWrapper
	LimitManager    *manager.LimitManager // may share replica ec / oec limiter
	ECStreamer      *ECStreamer           // required; nil panics in NewReader/NewWriter
	Ebsc            *BlobStoreClient
	EnableBcache    bool
	WConcurrency    int
	ReadConcurrency int
	FileCache       bool
	FileSize        uint64
	PoolId          uint8

	AheadReadEnable  bool
	AheadWindowCnt   int
	MinReadAheadSize int   // prefetch only if logical file size exceeds this (mount/stream policy)
	PrefetchTotalMem int64 // global prefetch memory budget (same knob as AheadReadTotalMem)
}

type blobPreReadLimiter struct {
	maxBytes  int64
	usedBytes int64
}

func (l *blobPreReadLimiter) tryAcquire(n int64) bool {
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

func (l *blobPreReadLimiter) release(n int64) {
	if l == nil || n <= 0 {
		return
	}
	atomic.AddInt64(&l.usedBytes, -n)
}

var (
	blobPreReadLimiterMu sync.Mutex
	blobPreReadLimiterGV *blobPreReadLimiter
)

func getBlobPreReadLimiter(totalMem int64) *blobPreReadLimiter {
	if totalMem <= 0 {
		return nil
	}
	blobPreReadLimiterMu.Lock()
	defer blobPreReadLimiterMu.Unlock()
	if blobPreReadLimiterGV == nil {
		blobPreReadLimiterGV = &blobPreReadLimiter{maxBytes: totalMem}
		log.LogInfof("blob pre-read limiter enabled, totalMem(%v)", totalMem)
		return blobPreReadLimiterGV
	}
	if blobPreReadLimiterGV.maxBytes != totalMem {
		log.LogWarnf("blob pre-read limiter already initialized with totalMem(%v), ignore new totalMem(%v)",
			blobPreReadLimiterGV.maxBytes, totalMem)
	}
	return blobPreReadLimiterGV
}

var readerBytePool sync.Pool

// readerGetBuf allocates a buffer of length n. maxPooledCap is Reader.prefetchBufCap();
// only buffers with 0 < cap <= maxPooledCap are eligible for readerBytePool.
func readerGetBuf(n, maxPooledCap int) []byte {
	if n <= 0 {
		return nil
	}
	if maxPooledCap > 0 && n <= maxPooledCap {
		if v := readerBytePool.Get(); v != nil {
			bp := v.(*[]byte)
			b := *bp
			if cap(b) >= n && cap(b) <= maxPooledCap {
				return b[:n]
			}
			readerBytePool.Put(bp)
		}
	}
	return make([]byte, n)
}

// readerPutBuf returns b to readerBytePool when 0 < cap(b) <= maxPooledCap; nil/empty/oversized are no-ops.
func readerPutBuf(b []byte, maxPooledCap int) {
	if b == nil || maxPooledCap <= 0 {
		return
	}
	c := cap(b)
	if c <= 0 || c > maxPooledCap {
		return
	}
	buf := b[:c]
	readerBytePool.Put(&buf)
}

// readerZeroBuf zeros the buffer, only for hole read
func readerZeroBuf(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// releaseAllPrefetchBuffers detaches window buffers. If a window is still async-filling,
// do not Put into readerBytePool (filler still writes that array); only nil the slot and
// let the filler reclaim via readerPutBuf when it finishes.
func releaseAllPrefetchBuffers(reader *Reader) {
	capN := reader.prefetchBufCap()
	releaseWindowBuffer(reader.wins.active, capN)
	releaseWindowBuffer(reader.wins.standby, capN)
}

func releaseWindowBuffer(w *aheadWin, maxPooledCap int) {
	if w == nil || w.buf == nil {
		return
	}
	if atomic.LoadInt32(&w.inflight) != 0 {
		w.buf = nil
		return
	}
	readerPutBuf(w.buf, maxPooledCap)
	w.buf = nil
}

func NewReader(config ClientConfig) (reader *Reader) {
	if config.ECStreamer == nil {
		panic("blobstore.NewReader: ClientConfig.ECStreamer is required")
	}
	reader = new(Reader)

	reader.bc = config.Bc
	reader.enableBcache = config.EnableBcache
	reader.readConcurrency = config.ReadConcurrency

	reader.limitManager = config.LimitManager
	reader.aheadReadEnable = config.AheadReadEnable
	reader.aheadWindowCnt = config.AheadWindowCnt
	mra := config.MinReadAheadSize
	if mra < 0 {
		mra = 0
	}
	reader.minReadAheadSize = uint64(mra)
	reader.preReadLimiter = getBlobPreReadLimiter(config.PrefetchTotalMem)
	reader.ecStreamer = config.ECStreamer
	reader.wins.ensure() // both windows exist for the Reader lifetime; buf is allocated lazily

	log.LogDebugf("blobstore NewReader: ino(%v) aheadReadEnable(%v) dualWindow(true) blockSize(%v) minReadAheadSize(%v) aheadReadTotalMemGB(%v)",
		reader.ecStreamer.Inode(), reader.aheadReadEnable, reader.ecStreamer.BlockSize(), reader.minReadAheadSize, config.PrefetchTotalMem)

	// readBuf is allocated lazily on first prefetch Read to avoid holding prefetchCap per open file
	// when the file turns out tiny or ahead-read is off after fileSize check.
	return
}

// Read is the blobstore entry for an EC FUSE read.
// Caller ECStreamer.readLocked holds s.mu (write lock if dirty, else RLock).
func (reader *Reader) Read(ctx context.Context, buf []byte, offset int, size int) (n int, err error) {
	beg := time.Now()
	defer func() {
		d := time.Since(beg)
		readerMetric.WithLabelValues("BlobstorRead").Observe(float64(d.Microseconds()))
	}()

	if size != len(buf) {
		size = len(buf)
	}
	if size == 0 {
		return 0, nil
	}
	fuseReqSize := size

	// Logical read bound (includes unflushed writer tail); dirty sync is done in ECStreamer.Read before this call.
	fileSize := reader.ecStreamer.fileSizeViewLocked()

	// Same as replica Streamer.read: offset >= logical tail returns n=0, err=nil (POSIX EOF); caller buf unchanged.
	if uint64(offset) >= fileSize {
		return 0, nil
	}

	if uint64(offset)+uint64(size) > fileSize {
		readerZeroBuf(buf[fileSize-uint64(offset) : uint64(size)])
		size = int(fileSize - uint64(offset))
	}

	// No prefetch: EBS/holes materialize directly into caller buf (no merge buffer).
	normalReadFunc := func() (int, error) {
		n, err := reader.readEbsRange(ctx, offset, uint32(size), fileSize, buf[:size])
		if err != nil {
			return 0, err
		}
		if log.EnableDebug() {
			log.LogDebugf("TRACE reader Read done ino(%v) cost(%v)us off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) path(no-prefetch)",
				reader.ecStreamer.Inode(), time.Since(beg).Microseconds(), offset, fuseReqSize, n, n)
		}
		return n, nil
	}

	heatReady := reader.observeRead(offset, size)
	usePrefetch := reader.prefetchEnabled() && fileSize > reader.minReadAheadSize && heatReady
	if !usePrefetch {
		return normalReadFunc()
	}
	if !reader.ensurePrefetchBuf() {
		return normalReadFunc()
	}
	if size >= reader.ecStreamer.BlockSize() {
		reader.coolPrefetch()
		return normalReadFunc()
	}

	return reader.readWithPrefetch(ctx, buf, offset, size, fileSize, fuseReqSize, beg)
}

// rwSlicesAllHoles reports whether every slice is a sparse hole (no EBS read).
func rwSlicesAllHoles(rSlices []*rwSlice) bool {
	for _, rs := range rSlices {
		if !rs.hole {
			return false
		}
	}
	return true
}

// readEbsRange splits [offset,offset+size) into rwSlices and reads EBS sequentially into dst (holes zeroed in place).
func (reader *Reader) readEbsRange(ctx context.Context, offset int, size uint32, fileSize uint64, dst []byte) (int, error) {
	return reader.readEbsRangeWithOeks(ctx, offset, size, fileSize, reader.ecStreamer.OeksLocked(), dst)
}

// readEbsRangeWithOeks issues EBS IO with the given oeks snapshot (async prefetch must not read a mutating view).
func (reader *Reader) readEbsRangeWithOeks(ctx context.Context, offset int, size uint32, fileSize uint64, oeks *ReadOnlyOeks, dst []byte) (int, error) {
	beg := time.Now()
	if int(size) > len(dst) {
		log.LogErrorf("readEbsRange: dst too short ino(%v) need(%v) have(%v)", reader.ecStreamer.Inode(), size, len(dst))
		return 0, syscall.EIO
	}
	rSlices, readSize, err := reader.prepareEbsSliceFromOeks(offset, size, fileSize, oeks, dst)
	if log.EnableDebug() {
		log.LogDebugf("TRACE reader readEbsRange. ino(%v) cost(%v)us rSlices-length(%v) readSize(%v)", reader.ecStreamer.Inode(), time.Since(beg).Microseconds(), len(rSlices), readSize)
	}
	if err != nil {
		return 0, err
	}
	if readSize == 0 {
		return 0, nil
	}
	sliceSize := len(rSlices)
	if sliceSize == 0 || rwSlicesAllHoles(rSlices) {
		// don't need to ebs read, all is holes, just zero the buffer
		readerZeroBuf(dst[:readSize])
		return int(readSize), nil
	}

	errCh := make(chan error, sliceSize)
	if sliceSize == 1 {
		if err := reader.readSliceRange(ctx, rSlices[0], errCh); err != nil {
			return 0, err
		}
		if log.EnableDebug() {
			log.LogDebugf("TRACE reader readEbsRange done. ino(%v) cost(%v)us rSlice-length(%v)", reader.ecStreamer.Inode(), time.Since(beg).Microseconds(), sliceSize)
		}
		return int(readSize), nil
	}

	// TODO: next version, don't use task pool to read the slices
	var wg sync.WaitGroup
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
			return 0, err
		}
	}

	if log.EnableDebug() {
		log.LogDebugf("TRACE reader readEbsRange done. ino(%v) cost(%v)us rSlices-length(%v)", reader.ecStreamer.Inode(), time.Since(beg).Microseconds(), sliceSize)
	}
	return int(readSize), nil
}

// logicalReadBound returns max(meta size, oek logical tails) for writer/updateMetaInfo.
func logicalReadBound(metaReportedSize uint64, objKeys []proto.ObjExtentKey) uint64 {
	logical := metaReportedSize
	for i := range objKeys {
		end := objKeys[i].FileOffset + uint64(objKeys[i].Size)
		if end > logical {
			logical = end
		}
	}
	return logical
}

// prepareEbsSlice splits [offset,offset+size) by sorted oeks into hole and data slices backed by dst subranges.
func (reader *Reader) prepareEbsSlice(offset int, size uint32, fileSize uint64, dst []byte) ([]*rwSlice, uint32, error) {
	return reader.prepareEbsSliceFromOeks(offset, size, fileSize, reader.ecStreamer.OeksLocked(), dst)
}

// prepareEbsSliceFromOeks is prepareEbsSlice with an explicit oeks snapshot (async prefetch).
// Sparse layout: |--hole--|==oek1==|--hole--|==oek2==|--hole--|; holes zeroed in readSliceRange.
// Data slice rOffset/rSize is within oek; require rOffset+rSize <= oek.Size.
// No oeks: whole range is one hole; offset>=fileSize returns readSize=0.
func (reader *Reader) prepareEbsSliceFromOeks(offset int, size uint32, fileSize uint64, keys *ReadOnlyOeks, dst []byte) ([]*rwSlice, uint32, error) {
	if offset < 0 {
		return nil, 0, syscall.EIO
	}

	log.LogDebugf("TRACE blobStore prepareEbsSlice Enter. ino(%v) fileSize(%v) offset(%v) size(%v)",
		reader.ecStreamer.Inode(), fileSize, offset, size)

	if uint64(offset) >= fileSize {
		return nil, 0, nil
	}

	if uint64(offset)+uint64(size) > fileSize {
		size = uint32(fileSize - uint64(offset))
	}
	if int(size) > len(dst) {
		log.LogErrorf("prepareEbsSlice: dst too short ino(%v) need(%v) have(%v)", reader.ecStreamer.Inode(), size, len(dst))
		return nil, 0, syscall.EIO
	}
	start := uint64(offset)
	end := start + uint64(size)

	if keys == nil {
		keys = NewReadOnlyOeks(nil)
	}

	chunks := make([]*rwSlice, 0)
	cur := start
	for i := 0; i < keys.Len(); i++ {
		oek := keys.At(i)
		ekEnd := oek.FileOffset + uint64(oek.Size)
		if ekEnd <= cur {
			continue
		}
		if oek.FileOffset >= end {
			break
		}
		if cur < oek.FileOffset {
			holeLen := oek.FileOffset - cur
			relOff := int(cur - start)
			chunks = append(chunks, &rwSlice{
				hole:       true,
				fileOffset: cur,
				rSize:      uint32(holeLen),
				Data:       dst[relOff : relOff+int(holeLen)],
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
		relOff := int(cur - start)
		chunks = append(chunks, &rwSlice{
			index:        i,
			fileOffset:   oek.FileOffset,
			size:         uint32(oek.Size),
			rOffset:      rOff,
			rSize:        uint32(rSz),
			objExtentKey: oek,
			Data:         dst[relOff : relOff+int(rSz)],
		})
		cur = ov
		if cur >= end {
			break
		}
	}
	if cur < end {
		holeLen := end - cur
		relOff := int(cur - start)
		chunks = append(chunks, &rwSlice{
			hole:       true,
			fileOffset: cur,
			rSize:      uint32(holeLen),
			Data:       dst[relOff : relOff+int(holeLen)],
		})
	}

	log.LogDebugf("TRACE blobStore prepareEbsSlice Exit. ino(%v)  offset(%v) size(%v) rwSlices_len(%v)", reader.ecStreamer.Inode(), offset, size, len(chunks))
	return chunks, size, nil
}

// readSliceRange handles one rwSlice: holes succeed immediately; data tries L1 then Ebsc.Read into rs.Data. errCh cap >= 1.
func (reader *Reader) readSliceRange(ctx context.Context, rs *rwSlice, errCh chan error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			errCh <- fmt.Errorf("blobstore readSliceRange panic: %v", r)
			panic(r)
		}
	}()
	if rs.hole {
		readerZeroBuf(rs.Data) // is hole, reset the buffer to zero
		log.LogDebugf("TRACE blobStore readSliceRange hole skip EBS. ino(%v) len(%v)", reader.ecStreamer.Inode(), rs.rSize)
		errCh <- nil
		return
	}

	volume := reader.ecStreamer.Volume()
	cacheKey := util.GenerateKey(volume, reader.ecStreamer.Inode(), rs.fileOffset)
	log.LogDebugf("TRACE blobStore readSliceRange Enter. ino(%v) rs.fileOffset(%v),rs.rOffset(%v),rs.rSize(%v) cacheKey(%v) ",
		reader.ecStreamer.Inode(), rs.fileOffset, rs.rOffset, rs.rSize, cacheKey)

	var readN int

	bgTime := stat.BeginStat()
	metric := exporter.NewTPCnt("ReadSlice")
	defer func() {
		stat.EndStat("ReadSlice", err, bgTime, 1)
		metric.SetWithLabels(err, map[string]string{exporter.Vol: volume})
		if log.EnableDebug() {
			log.LogDebugf("TRACE reader readSliceRange done. ino(%v) cost(%v)us err(%v)", reader.ecStreamer.Inode(), time.Since(*bgTime).Microseconds(), err)
		}
	}()

	// read local cache
	if reader.enableBcache {
		readN, err = reader.bc.Get(volume, cacheKey, rs.Data, rs.rOffset, rs.rSize)
		if err == nil {
			if readN == int(rs.rSize) {
				// L1 cache hit.
				metric := exporter.NewTPCnt("L1CacheGetHit")
				stat.EndStat("CacheHit-L1", err, bgTime, 1)
				defer func() {
					metric.SetWithLabels(err, map[string]string{exporter.Vol: volume})
				}()

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
	readN, err = reader.ecStreamer.Ebsc().Read(ctx, volume, rs.Data, rs.rOffset, uint64(rs.rSize), rs.objExtentKey)
	if log.EnableDebug() {
		log.LogDebugf("blobstore slow ebs.Read ino(%v) fileOff(%v) rOff(%v) rSize(%v) cost(%v)us err(%v)",
			reader.ecStreamer.Inode(), rs.fileOffset, rs.rOffset, rs.rSize, time.Since(t0).Microseconds(), err)
	}
	if err != nil {
		errCh <- err
		return
	}
	if readN != int(rs.rSize) {
		err = fmt.Errorf("blobstore readSliceRange short read want(%v) got(%v)", rs.rSize, readN)
		errCh <- err
		return
	}
	errCh <- nil

	// With L1 enabled, async full-extent read into cache (inflightCache dedupes by cacheKey).
	if !reader.needCacheL1() || reader.bc == nil {
		log.LogDebugf("TRACE blobStore readSliceRange exit without cache. read counter=%v", readN)
		return nil
	}

	asyncCtx := context.Background()
	go reader.asyncCache(asyncCtx, cacheKey, rs.objExtentKey)

	log.LogDebugf("TRACE blobStore readSliceRange exit with cache. read counter=%v", readN)
	return nil
}

// asyncCache reads full ObjExtent in background and Puts to L1; one inflight task per cacheKey.
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

	volName := reader.ecStreamer.Volume()
	buf := readerGetBuf(int(objExtentKey.Size), reader.prefetchBufCap())
	defer readerPutBuf(buf, reader.prefetchBufCap())
	read, err := reader.ecStreamer.Ebsc().Read(ctx, volName, buf, 0, uint64(len(buf)), objExtentKey)
	if err != nil || read != len(buf) {
		log.LogErrorf("ERROR blobStore asyncCache fail, size no match. cacheKey=%v, objExtentKey.size=%v, read=%v",
			cacheKey, len(buf), read)
		return
	}

	if reader.needCacheL1() {
		reader.bc.Put(volName, cacheKey, buf)
	}

	log.LogDebugf("TRACE blobStore asyncCache(L1) Exit. cacheKey=%v", cacheKey)
}

func (reader *Reader) needCacheL1() bool {
	return reader.enableBcache
}
