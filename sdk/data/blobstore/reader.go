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

// rwSlice is one logical read span：ObjExtent ranges use EBS; uncovered spans use hole=true with zero-filled Data and no EBS.
type rwSlice struct {
	index        int
	fileOffset   uint64
	size         uint32
	rOffset      uint64
	rSize        uint32
	read         int
	Data         []byte
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

// Reader is bound to one inode ECStreamer: split reads by oeks, sparse zero-fill, optional prefetch and async L1 refill.
type Reader struct {
	bc              *bcache.BcacheClient
	readConcurrency int
	enableBcache    bool
	inflightCache   sync.Map // TODO: keep for now; dedupe asyncCache L1 refill per extent key
	limitManager    *manager.LimitManager

	// Prefetch: when aheadRead is on and fileSize > minReadAheadSize, merge small reads into one readEbsRange (<= prefetchBufCap = 2*blockSize) cached in readBuf.
	// Semantics match replica AheadReadWindow; buffering lives in Reader, not stream.
	aheadReadEnable  bool
	minReadAheadSize uint64
	readBuf          []byte
	bufBaseOff       int   // file offset of valid data at readBuf[0] (prefetch block start)
	bufValidLen      int   // valid bytes in readBuf[0:bufValidLen] (prefetch block end)
	prefetchReserved int64 // bytes reserved from global blobPreReadLimiter
	preReadLimiter   *blobPreReadLimiter

	ecStreamer *ECStreamer // required; logical tail/gen/oeks from stream (see NewReader)
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
	mra := config.MinReadAheadSize
	if mra < 0 {
		mra = 0
	}
	reader.minReadAheadSize = uint64(mra)
	reader.preReadLimiter = getBlobPreReadLimiter(config.PrefetchTotalMem)
	reader.ecStreamer = config.ECStreamer
	// readBuf is allocated lazily on first prefetch Read to avoid holding 2×EbsBlockSize per open file
	// when the file turns out tiny or ahead-read is off after fileSize check.
	return
}

// prefetchBufCap is readBuf capacity and the max single prefetch fetch size (two EBS logical blocks).
func (reader *Reader) prefetchBufCap() int {
	if reader.ecStreamer.BlockSize() <= 0 {
		return 0
	}
	return reader.ecStreamer.BlockSize() * 2
}

// ensurePrefetchBuf reserves readBuf and global budget; on failure Read falls back to per-call readEbsRange.
func (reader *Reader) ensurePrefetchBuf() bool {
	capW := reader.prefetchBufCap()
	if capW <= 0 {
		return false
	}
	if reader.readBuf != nil && len(reader.readBuf) >= capW {
		return true
	}
	need := int64(capW) - reader.prefetchReserved
	if need > 0 && !reader.preReadLimiter.tryAcquire(need) {
		log.LogDebugf("TRACE reader prefetch budget exhausted. ino(%v) need(%v)", reader.ecStreamer.Inode(), need)
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
				reader.ecStreamer.Inode(), offset, d, n, err)
		}
	}()

	if reader == nil {
		return 0, fmt.Errorf("reader is not opened yet")
	}

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
		// TODD: next version, optionally zero caller buf at entry (not required for correctness)
		return 0, nil
	}
	// TODO: next version, optionally zero-fill caller buf for holes at entry
	if uint64(offset)+uint64(size) > fileSize {
		size = int(fileSize - uint64(offset))
	}

	// No prefetch: holes are zero-filled in readEbsRange/prepareEbsSlice, then copied into buf.
	normalReadFunc := func() (int, error) {
		data, err := reader.readEbsRange(ctx, offset, uint32(size), fileSize)
		if err != nil {
			return 0, err
		}
		n := copy(buf, data)
		log.LogDebugf("TRACE reader Read done ino(%v) off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) path(no-prefetch)",
			reader.ecStreamer.Inode(), offset, fuseReqSize, n, len(data))
		return n, nil
	}

	// Prefetch gate: requires blockSize configured, mount-level aheadRead enabled, and file length > minReadAheadSize (same as stream-side policy to avoid over-buffering small files).
	usePrefetch := reader.ecStreamer.BlockSize() > 0 && reader.aheadReadEnable && fileSize > reader.minReadAheadSize
	// case 1: prefetch disabled or file too small -> each call does one readEbsRange;
	if !usePrefetch {
		return normalReadFunc()
	}
	// case 2: prefetch budget reservation failed and global prefetch pool is exhausted: still read correctly but without readBuf, fallback as above;
	if !reader.ensurePrefetchBuf() {
		return normalReadFunc()
	}
	// case 3: single request >= blockSize: cannot fit prefetch window, read EBS range directly and invalidate buffer to avoid half-block logic.
	if size >= reader.ecStreamer.BlockSize() {
		reader.invalidateReadBuf()
		return normalReadFunc()
	}

	// case 4: Prefetch: on miss fetch [offset,offset+fetch) with fetch<=prefetchBufCap; later reads in window copy from readBuf.
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
		data, err := reader.readEbsRange(ctx, offset, uint32(fetch), fileSize)
		if err != nil {
			return 0, err
		}
		if len(data) == 0 {
			log.LogErrorf("reader Read prefetch buffer is empty. ino(%v) offset(%v) fetchLen(%v)", reader.ecStreamer.Inode(), offset, fetch)
			reader.invalidateReadBuf()
			return 0, syscall.EIO
		}
		ebsFetchSize = len(data)
		if len(data) > len(reader.readBuf) {
			log.LogInfof("extend reader Read prefetch buffer. ino(%v) offset(%v) fetchLen(%v) readBufLen(%v)", reader.ecStreamer.Inode(), offset, len(data), len(reader.readBuf))
			reader.readBuf = make([]byte, len(data))
		}
		copy(reader.readBuf, data)
		reader.bufBaseOff = offset
		reader.bufValidLen = len(data)
		log.LogDebugf("TRACE reader Read prefetch. ino(%v) offset(%v) fetchLen(%v)", reader.ecStreamer.Inode(), offset, len(data))
	}

	if reader.bufValidLen <= 0 {
		log.LogErrorf("reader Read prefetch buffer is invalid. ino(%v) offset(%v) bufValidLen(%v)", reader.ecStreamer.Inode(), offset, reader.bufValidLen)
		return 0, syscall.EIO
	}

	if offset < reader.bufBaseOff || offset >= reader.bufBaseOff+reader.bufValidLen {
		log.LogErrorf("reader Read prefetch buffer is out of range. ino(%v) offset(%v) bufBaseOff(%v) bufValidLen(%v)", reader.ecStreamer.Inode(), offset, reader.bufBaseOff, reader.bufValidLen)
		return 0, syscall.EIO
	}
	if size > reader.bufValidLen-(offset-reader.bufBaseOff) {
		log.LogWarnf("reader Read prefetch buffer is too small. ino(%v) offset(%v) bufBaseOff(%v) bufValidLen(%v)", reader.ecStreamer.Inode(), offset, reader.bufBaseOff, reader.bufValidLen)
		reader.invalidateReadBuf()
		return normalReadFunc()
	}

	// case 4.1: copy data from readBuf to destination buffer.
	winStart := offset - reader.bufBaseOff
	copy(buf, reader.readBuf[winStart:winStart+size])

	log.LogDebugf("TRACE reader Read done, temp ignore, ino(%v) off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) prefetchBufRemain(%v) path(prefetch)",
		reader.ecStreamer.Inode(), offset, fuseReqSize, size, ebsFetchSize, reader.bufValidLen)
	return size, nil
}

// readEbsRange splits [offset,offset+size) into rwSlices, reads EBS in parallel, returns merged buffer (holes zeroed in prepareEbsSlice).
func (reader *Reader) readEbsRange(ctx context.Context, offset int, size uint32, fileSize uint64) ([]byte, error) {
	rSlices, err := reader.prepareEbsSlice(offset, size, fileSize)
	log.LogDebugf("TRACE reader readEbsRange. ino(%v)  rSlices-length(%v) ", reader.ecStreamer.Inode(), len(rSlices))
	if err != nil {
		return nil, err
	}
	sliceSize := len(rSlices)
	if sliceSize == 0 {
		return make([]byte, 0), nil
	}

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

// invalidateReadBuf clears prefetch window metadata; next Read refetches via readEbsRange.
func (reader *Reader) invalidateReadBuf() {
	reader.bufValidLen = 0
	reader.bufBaseOff = 0
}

// releasePrefetchCache frees readBuf and releases global budget; call under ECStreamer.mu, not concurrent with Read.
func (reader *Reader) releasePrefetchCache() {
	if reader == nil {
		return
	}
	if reader.prefetchReserved > 0 && reader.preReadLimiter != nil {
		reader.preReadLimiter.release(reader.prefetchReserved)
		reader.prefetchReserved = 0
	}
	reader.readBuf = nil
	reader.bufValidLen = 0
	reader.bufBaseOff = 0
}

// prepareEbsSlice splits [offset,offset+size) by sorted oeks into hole and data slices.
// Sparse layout: |--hole--|==oek1==|--hole--|==oek2==|--hole--|; holes use zero Data, readSliceRange skips EBS.
// Data slice rOffset/rSize is within oek; require rOffset+rSize <= oek.Size.
// No oeks: whole range is one hole; offset>=fileSize returns nil,nil (same as replica hole read).
func (reader *Reader) prepareEbsSlice(offset int, size uint32, fileSize uint64) ([]*rwSlice, error) {
	if offset < 0 {
		return nil, syscall.EIO
	}

	log.LogDebugf("TRACE blobStore prepareEbsSlice Enter. ino(%v) fileSize(%v) offset(%v) size(%v)",
		reader.ecStreamer.Inode(), fileSize, offset, size)

	if uint64(offset) >= fileSize {
		return nil, nil
	}

	if uint64(offset)+uint64(size) > fileSize {
		size = uint32(fileSize - uint64(offset))
	}
	start := uint64(offset)
	end := start + uint64(size)

	keys := append([]proto.ObjExtentKey(nil), reader.ecStreamer.OeksLocked()...)
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

	log.LogDebugf("TRACE blobStore prepareEbsSlice Exit. ino(%v)  offset(%v) size(%v) rwSlices_len(%v)", reader.ecStreamer.Inode(), offset, size, len(chunks))
	return chunks, nil
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
		log.LogDebugf("TRACE blobStore readSliceRange hole skip EBS. ino(%v) len(%v)", reader.ecStreamer.Inode(), rs.rSize)
		errCh <- nil
		return
	}

	volume := reader.ecStreamer.Volume()
	cacheKey := util.GenerateKey(volume, reader.ecStreamer.Inode(), rs.fileOffset)
	log.LogDebugf("TRACE blobStore readSliceRange Enter. ino(%v) rs.fileOffset(%v),rs.rOffset(%v),rs.rSize(%v) cacheKey(%v) ",
		reader.ecStreamer.Inode(), rs.fileOffset, rs.rOffset, rs.rSize, cacheKey)

	buf := make([]byte, rs.rSize)
	var readN int

	bgTime := stat.BeginStat()
	stat.EndStat("CacheGet", nil, bgTime, 1)
	metric := exporter.NewTPCnt("CacheGet")
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: volume})
	}()

	// read local cache
	if reader.enableBcache {
		readN, err = reader.bc.Get(volume, cacheKey, buf, rs.rOffset, rs.rSize)
		if err == nil {
			if readN == int(rs.rSize) {
				// L1 cache hit.
				metric := exporter.NewTPCnt("L1CacheGetHit")
				stat.EndStat("CacheHit-L1", nil, bgTime, 1)
				defer func() {
					metric.SetWithLabels(err, map[string]string{exporter.Vol: volume})
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
	_, err = reader.ecStreamer.Ebsc().Read(ctx, volume, buf, rs.rOffset, uint64(rs.rSize), rs.objExtentKey)
	if d := time.Since(t0); d >= slowOpInfoThreshold {
		log.LogInfof("blobstore slow ebs.Read ino(%v) fileOff(%v) rOff(%v) rSize(%v) dur(%v) err(%v)",
			reader.ecStreamer.Inode(), rs.fileOffset, rs.rOffset, rs.rSize, d, err)
	}
	if err != nil {
		errCh <- err
		return
	}
	read := copy(rs.Data, buf)
	errCh <- nil

	// With L1 enabled, async full-extent read into cache (inflightCache dedupes by cacheKey).
	if !reader.needCacheL1() || reader.bc == nil {
		log.LogDebugf("TRACE blobStore readSliceRange exit without cache. read counter=%v", read)
		return nil
	}

	asyncCtx := context.Background()
	go reader.asyncCache(asyncCtx, cacheKey, rs.objExtentKey)

	log.LogDebugf("TRACE blobStore readSliceRange exit with cache. read counter=%v", read)
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
	buf := make([]byte, objExtentKey.Size)
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
