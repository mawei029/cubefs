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
	"sort"
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
	// hole marks [start,end) ranges that are not covered by any ObjExtent; Data is pre-filled with zeros and readSliceRange skips EBS.
	hole bool
}

func (s rwSlice) String() string {
	return fmt.Sprintf("rwSlice{fileOffset(%v),size(%v),rOffset(%v),rSize(%v),read(%v),hole(%v),objExtentKey(%v)}", s.fileOffset, s.size, s.rOffset, s.rSize, s.read, s.hole, s.objExtentKey)
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

	// extentsGeneration / metaReportedSize come from MetaWrapper.GetObjExtents (same meaning as Generation/Size in proto.GetObjExtentsResponse),
	// they increase when extent list changes and can be used as a data-view version fingerprint; valid after successful refresh.
	extentsGeneration uint64
	metaReportedSize  uint64
	// inodeViewGen / inodeViewSize are inode generation/size aligned with file-layer InodeGet; used by EnsureAlignedForRead to detect stale Reader cache.
	inodeViewGen  uint64
	inodeViewSize uint64

	// blockSize + readBuf: when aheadRead is enabled and file size is larger than minReadAheadSize in EC/BlobStore reads,
	// merge multiple small FUSE reads (for example max_read=128KiB) into at most one EBS fetch of blockSize (EbsBlockSize), and cache result in readBuf.
	// Semantics are similar to replica-stream AheadReadWindow, but implementation is Reader-side buffering instead of stream module logic.
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

// ensurePrefetchBuf ensures readBuf capacity is at least blockSize and reserves global prefetch budget; on budget shortage it returns false and Read falls back to per-call readEbsRange.
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

	// case 4: aggregate small reads with prefetch window: one readEbsRange fetches at most [offset, offset+fetch), fetch<=blockSize; repeated FUSE reads in same window copy from readBuf without extra EBS calls.
	ebsFetchSize := 0
	if reader.bufValidLen == 0 || offset < reader.bufBaseOff || offset >= reader.bufBaseOff+reader.bufValidLen {
		// Buffer is empty or request falls outside current window -> invalidate old window and fetch a new block from the new offset.
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

	// case 4.1: copy data from readBuf to destination buffer.
	winStart := offset - reader.bufBaseOff
	copy(buf, reader.readBuf[winStart:winStart+size])

	log.LogDebugf("TRACE reader Read done, temp ignore, ino(%v) off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) prefetchBufRemain(%v) path(prefetch)",
		reader.ino, offset, fuseReqSize, size, ebsFetchSize, reader.bufValidLen)
	return size, nil
}

// readEbsRange splits ranges by ObjExtent and runs readSliceRange in parallel; each slice eventually calls BlobStoreClient.Read (access.Get + ReadFull).
// Caller must already hold reader.Mutex.
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

// invalidateReadBuf clears prefetch window (without freeing underlying slice); next Read will run readEbsRange again.
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
func (reader *Reader) readSliceRange(ctx context.Context, rs *rwSlice) (err error) {
	defer reader.wg.Done()
	if rs.hole {
		log.LogDebugf("TRACE blobStore readSliceRange hole skip EBS. ino(%v) len(%v)", reader.ino, rs.rSize)
		reader.err <- nil
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
		return nil
	}
	if err := reader.refreshEbsExtents(); err != nil {
		return syscall.EIO
	}
	return nil
}

// EnsureAlignedForRead compares file-layer InodeGet Generation/Size against Reader cache; refresh extents on mismatch,
// used when inode/ObjExtents have advanced after write/truncate while Reader still holds stale extent list.
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

// SyncInodeView updates inode-view anchor after alignment with meta (for example via RefreshExtents), preventing next Read from false stale detection.
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

// RefreshExtents re-fetches ObjExtents from meta and updates cache; used after truncate so later Reads use updated extents and size.
// Do not call GetObjExtents while holding lock to avoid blocking other Reads.
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
