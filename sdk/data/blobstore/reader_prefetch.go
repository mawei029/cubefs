// Copyright 2026 The CubeFS Authors.
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
	"sync/atomic"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
)

// Call path: File.Read -> ECExtentClient.Read -> ECStreamer.Read -> Reader.Read
//
//	-> observeRead (heat / cool) -> readEbsRange | readWithPrefetch (this file).
//
// Arm after >=1MiB sequential. Dual OEK windows; no boundary Wait.
// coversFill (inflight covering this read) is not a miss. True miss may kick async.
const (
	windowCnt          = 2       // always dual windows
	prefetchHeatBytes  = 1 << 20 // arm async windows after 1MiB sequential
	sequentialGapMax   = 256 << 10
	coolMissStreakCnt  = 64 // after proven hit: tolerate ~one window of FUSE misses
	coolMissColdCnt    = 8  // never hit: cool quickly on false heat / random
	asyncKickMissLimit = 4  // only schedule async on the first few true misses
)

// observeRead decides whether to arm prefetch (access pattern), not whether a window hit.
// missStreak / kick live in readWithPrefetch: they need hit vs coversFill after the window check.
// Warm (sequential or covered by a prefetch window) → accumulate heat; else cool immediately
// (may be large seek, short random, or mid-range jump — false heat must not keep kicking async fills).
func (reader *Reader) observeRead(offset, size int) (heatReady bool) {
	if size <= 0 {
		return false
	}

	reader.mu.Lock()
	defer reader.mu.Unlock()

	if !reader.hasLastRead {
		reader.hasLastRead = true
		reader.lastReadOff = offset
		reader.lastReadEnd = offset + size
		return false
	}

	if reader.isSequentialRead(offset, size) || reader.coversAnyWindow(offset, size) {
		reader.seqHeatBytes += uint64(seqAdvanceBytes(offset, size, reader.lastReadEnd))
	} else {
		// Pattern no longer looks sequential; disarm before more async over-fetch.
		log.LogDebugf("TRACE prefetch ino(%v) event(cool) reason(non_seq) off(%v) size(%v) lastEnd(%v)",
			reader.ecStreamer.Inode(), offset, size, reader.lastReadEnd)
		reader.coolPrefetchLocked()
	}
	reader.lastReadOff = offset
	reader.lastReadEnd = offset + size
	return reader.seqHeatBytes >= prefetchHeatBytes
}

func (reader *Reader) coolPrefetch() {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.coolPrefetchLocked()
}

func (reader *Reader) coolPrefetchLocked() {
	reader.invalidateReadBufLocked()
	reader.seqHeatBytes = 0
	reader.missStreak = 0
	reader.prefetchHit = false
}

// readWithPrefetch serves an armed read: hit active/standby, or sync-fetch on miss/coversFill.
// Holds prefetchInfo.mu only around window/heat updates; releases it across readEbsRange.
//
// observeRead answers "should we arm?"; missStreak answers "is the window useful once armed?".
// The latter needs hit/coversFill, so count only here—after the window check and a successful
// sync read (serve first, then account). coversFill is not a miss; EBS error does not bump streak.
func (reader *Reader) readWithPrefetch(ctx context.Context, buf []byte, offset, size int, fileSize uint64, fuseReqSize int, beg time.Time) (int, error) {
	reader.mu.Lock()
	if reader.tryPrefetchHit(buf, offset, size) {
		reader.missStreak = 0
		reader.prefetchHit = true
		reader.kickStandbyNext(ctx, fileSize)
		activeOff, activeValid := reader.wins.active.off, reader.wins.active.valid
		reader.mu.Unlock()
		if log.EnableDebug() {
			log.LogDebugf("TRACE prefetch ino(%v) event(hit) cost(%v)us off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(0) activeOff(%v) activeValid(%v)",
				reader.ecStreamer.Inode(), time.Since(beg).Microseconds(), offset, fuseReqSize, size, activeOff, activeValid)
		}
		return size, nil
	}

	if reader.tryStandbyHit(buf, offset, size) {
		n, err := reader.finishStandbyHitLocked(ctx, size, fileSize, fuseReqSize, beg)
		reader.mu.Unlock()
		return n, err
	}

	// Inflight fill already covers this read: sync fallback, do not count as miss.
	covering := reader.wins.active.coversFill(offset, size) || reader.wins.standby.coversFill(offset, size)
	reader.mu.Unlock()

	n, err := reader.readEbsRange(ctx, offset, uint32(size), fileSize, buf[:size])
	if err != nil {
		return 0, err
	}

	missStreak := atomic.LoadUint32(&reader.missStreak)
	if !covering {
		reader.mu.Lock()
		// True miss feedback: cool if useless streak is long; else limited async kick.
		reader.missStreak++
		coolLimit := uint32(coolMissColdCnt)
		if reader.prefetchHit {
			coolLimit = uint32(coolMissStreakCnt)
		}
		if reader.missStreak >= coolLimit {
			log.LogDebugf("TRACE prefetch ino(%v) event(cool) reason(miss_streak) off(%v) missStreak(%v) useful(%v)",
				reader.ecStreamer.Inode(), offset, reader.missStreak, reader.prefetchHit)
			reader.coolPrefetchLocked()
		} else if reader.missStreak <= asyncKickMissLimit {
			reader.ensureOekAsyncWindows(ctx, offset, fileSize)
		}
		missStreak = reader.missStreak
		reader.mu.Unlock()
	}

	if log.EnableDebug() {
		log.LogDebugf("TRACE prefetch ino(%v) event(miss) cost(%v)us off(%v) fuseReq(%v) fuseRet(%v) ebsFetchBytes(%v) missStreak(%v) covering(%v)",
			reader.ecStreamer.Inode(), time.Since(beg).Microseconds(), offset, fuseReqSize, n, n, missStreak, covering)
	}
	return n, nil
}

func (reader *Reader) finishStandbyHitLocked(ctx context.Context, size int, fileSize uint64, fuseReqSize int, beg time.Time) (int, error) {
	reader.missStreak = 0
	reader.prefetchHit = true
	reader.promoteStandby()
	reader.kickStandbyNext(ctx, fileSize)
	if log.EnableDebug() {
		log.LogDebugf("TRACE prefetch ino(%v) event(hit-standby) cost(%v)us fuseReq(%v) fuseRet(%v) ebsFetchBytes(0) activeOff(%v) activeValid(%v)",
			reader.ecStreamer.Inode(), time.Since(beg).Microseconds(), fuseReqSize, size, reader.wins.active.off, reader.wins.active.valid)
	}
	return size, nil
}

// prefetchForwardFloor is the earliest FileOffset allowed for new async fills.
// When active already holds/fills a window, refuse earlier oeks (kernel readahead rewind).
// Returns -1 when there is no forward floor yet.
func (reader *Reader) prefetchForwardFloor() int {
	w := reader.wins.active
	if w.valid > 0 {
		return w.off
	}
	if atomic.LoadInt32(&w.inflight) != 0 && w.fillLen > 0 {
		return w.fillOff
	}
	return -1
}

// ensureOekAsyncWindows schedules async fill for the oek containing offset (or next) and the following oek.
// Skips active if standby already holds/fills that oek (dedup). Never schedules oeks behind the active window.
func (reader *Reader) ensureOekAsyncWindows(ctx context.Context, offset int, fileSize uint64) {
	oeks := reader.ecStreamer.OeksLocked()
	if oeks.Len() == 0 {
		return
	}
	idx := oeks.FindContainOrAfter(uint64(offset))
	if idx < 0 {
		return
	}
	floor := reader.prefetchForwardFloor()
	cur := oeks.At(idx)
	if floor < 0 || int(cur.FileOffset) >= floor {
		// Do not start active async for an extent standby already has or is filling.
		if !reader.wins.standby.holdsExtent(int(cur.FileOffset), int(cur.Size)) {
			reader.scheduleAsyncExtent(ctx, cur, fileSize, false)
		}
	}
	// Dual window: if a next oek exists (idx+1 in range), fill it into standby when not behind floor.
	if idx+1 < oeks.Len() {
		next := oeks.At(idx + 1)
		if floor < 0 || int(next.FileOffset) >= floor {
			reader.scheduleAsyncExtent(ctx, next, fileSize, true)
		}
	}
}

// ensurePrefetchBuf allocates dual windows and takes blobPreReadLimiter quota.
// Caller must NOT hold prefetchInfo.mu.
func (reader *Reader) ensurePrefetchBuf() bool {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	return reader.ensurePrefetchBufLocked()
}

// ensurePrefetchBufLocked requires prefetchInfo.mu.
func (reader *Reader) ensurePrefetchBufLocked() bool {
	w := reader.ecStreamer.BlockSize()
	if w <= 0 {
		return false
	}
	activeOK := reader.wins.active.buf != nil && cap(reader.wins.active.buf) >= w
	standbyOK := reader.wins.standby.buf != nil && cap(reader.wins.standby.buf) >= w
	if activeOK && standbyOK {
		return true
	}

	needTotal := int64(w * windowCnt)
	reserved := atomic.LoadInt64(&reader.prefetchReserved)
	need := needTotal - reserved
	if need > 0 && !reader.preReadLimiter.tryAcquire(need) {
		log.LogDebugf("TRACE reader prefetch budget exhausted. ino(%v) need(%v)", reader.ecStreamer.Inode(), need)
		return false
	}

	if need > 0 {
		atomic.AddInt64(&reader.prefetchReserved, need)
	}
	poolMax := reader.prefetchBufCap()
	if !activeOK {
		if reader.wins.active.buf != nil {
			readerPutBuf(reader.wins.active.buf, poolMax)
		}
		reader.wins.active.buf = readerGetBuf(w, poolMax)
		if reader.wins.active.buf == nil || cap(reader.wins.active.buf) < w {
			reader.preReadLimiter.release(need)
			atomic.AddInt64(&reader.prefetchReserved, -need)
			return false
		}
	}

	if !standbyOK {
		if reader.wins.standby.buf != nil {
			readerPutBuf(reader.wins.standby.buf, poolMax)
		}
		reader.wins.standby.buf = readerGetBuf(w, poolMax)
		if reader.wins.standby.buf == nil || cap(reader.wins.standby.buf) < w {
			reader.preReadLimiter.release(need)
			atomic.AddInt64(&reader.prefetchReserved, -need)
			return false
		}
	}

	return true
}

// ensureWindowCap grows a single window buffer when oek.Size exceeds the default BlockSize allocation.
func (reader *Reader) ensureWindowCap(w *aheadWin, need int) bool {
	if need <= 0 {
		return false
	}
	if w.buf != nil && cap(w.buf) >= need {
		return true
	}
	if !reader.ensurePrefetchBufLocked() {
		return false
	}
	if w.buf != nil && cap(w.buf) >= need {
		return true
	}
	extra := int64(need)
	if w.buf != nil {
		extra = int64(need - cap(w.buf))
	}
	if extra > 0 && !reader.preReadLimiter.tryAcquire(extra) {
		return false
	}
	poolMax := reader.prefetchBufCap()
	if need > poolMax {
		poolMax = need
	}
	old := w.buf
	nb := readerGetBuf(need, poolMax)
	if nb == nil || cap(nb) < need {
		if extra > 0 {
			reader.preReadLimiter.release(extra)
		}
		return false
	}
	if old != nil {
		readerPutBuf(old, reader.prefetchBufCap())
	}
	w.buf = nb
	if extra > 0 {
		atomic.AddInt64(&reader.prefetchReserved, extra)
	}
	return true
}

// kickStandbyNext schedules the oek after active into standby (no-op if already has/inflight).
func (reader *Reader) kickStandbyNext(ctx context.Context, fileSize uint64) {
	if reader.wins.active.valid <= 0 {
		return
	}
	oeks := reader.ecStreamer.OeksLocked()
	idx := oeks.FindContainOrAfter(uint64(reader.wins.active.off))
	if idx < 0 || idx+1 >= oeks.Len() {
		return
	}
	reader.scheduleAsyncExtent(ctx, oeks.At(idx+1), fileSize, true)
}

// scheduleAsyncExtent fills one ObjExtentKey into active or standby.
// Caller must hold prefetchInfo.mu. EBS runs unlocked; publish takes prefetchInfo.mu again.
func (reader *Reader) scheduleAsyncExtent(ctx context.Context, oek proto.ObjExtentKey, fileSize uint64, toStandby bool) {
	start := int(oek.FileOffset)
	fetch := int(oek.Size)
	if start < 0 || fetch <= 0 || uint64(start) >= fileSize {
		return
	}
	if uint64(start)+uint64(fetch) > fileSize {
		fetch = int(fileSize - uint64(start))
	}
	if fetch <= 0 {
		return
	}
	if !reader.ensurePrefetchBufLocked() {
		return
	}
	w := reader.wins.active
	if toStandby {
		w = reader.wins.standby
	}
	if atomic.LoadInt32(&w.inflight) != 0 {
		return
	}
	// Dedup across the pair: never pull the same extent into both windows.
	if toStandby {
		if reader.wins.standby.holdsExtent(start, fetch) || reader.wins.active.holdsExtent(start, fetch) {
			return
		}
	} else if reader.wins.active.holdsExtent(start, fetch) || reader.wins.standby.holdsExtent(start, fetch) {
		return
	}
	if !reader.ensureWindowCap(w, fetch) {
		return
	}
	if !atomic.CompareAndSwapInt32(&w.inflight, 0, 1) {
		return
	}
	w.fillOff = start
	w.fillLen = fetch
	w.off = 0
	w.valid = 0

	gen := atomic.LoadUint64(&reader.prefetchGen)
	oeksSnap := snapshotReadOnlyOeks(reader.ecStreamer.OeksLocked())
	s := reader.ecStreamer
	ino := s.Inode()
	dst := w.buf
	poolMax := reader.prefetchBufCap()

	go func() {
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		bgTime := stat.BeginStat()
		var err error
		n := 0
		defer func() {
			stat.EndStat("blobstore-async-prefetch", err, bgTime, 1)
			cancel()
		}()

		if dst != nil && cap(dst) >= fetch {
			n, err = reader.readEbsRangeWithOeks(pctx, start, uint32(fetch), fileSize, oeksSnap, dst[:fetch])
			if err != nil {
				log.LogWarnf("asyncPrefetch: ebs fail ino(%v) start(%v) fetch(%v) err(%v)", ino, start, fetch, err)
			}
		}

		reader.mu.Lock()
		// Window still owns dst only if release/replace did not detach it (same backing array).
		windowOwnsDst := prefetchWindowOwnsBuf(w.buf, dst)
		if err == nil && n > 0 && atomic.LoadUint64(&reader.prefetchGen) == gen && windowOwnsDst && cap(w.buf) >= n {
			w.off = start
			w.valid = n
			if log.EnableDebug() {
				log.LogDebugf("TRACE prefetch ino(%v) event(async-ready) start(%v) len(%v) standby(%v) cost(%v)us",
					ino, start, n, toStandby, time.Since(*bgTime).Microseconds())
			}
		} else if !windowOwnsDst {
			// Close/release orphaned this buffer; reclaim after exclusive write finished.
			readerPutBuf(dst, poolMax)
		}
		w.fillOff = 0
		w.fillLen = 0
		atomic.StoreInt32(&w.inflight, 0)
		reader.mu.Unlock()
	}()
}

func (reader *Reader) prefetchEnabled() bool {
	return reader.aheadReadEnable && reader.ecStreamer.BlockSize() > 0
}

func (reader *Reader) prefetchBufCap() int {
	if !reader.aheadReadEnable {
		return 0
	}
	bs := reader.ecStreamer.BlockSize()
	if bs <= 0 {
		return 0
	}
	return bs * windowCnt // always dual windows
}

func (reader *Reader) promoteStandby() {
	reader.wins.active, reader.wins.standby = reader.wins.standby, reader.wins.active
	reader.wins.standby.off = 0
	reader.wins.standby.valid = 0
}

func (reader *Reader) invalidateReadBuf() {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.invalidateReadBufLocked()
}

func (reader *Reader) invalidateReadBufLocked() {
	atomic.AddUint64(&reader.prefetchGen, 1)
	reader.wins.clearMeta()
}

// releasePrefetchCache bumps gen, returns limiter quota immediately, and detaches buffers.
// Inflight fillers keep exclusive ownership of orphaned arrays until they Put them.
func (reader *Reader) releasePrefetchCache() {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	atomic.AddUint64(&reader.prefetchGen, 1)
	if reserved := atomic.SwapInt64(&reader.prefetchReserved, 0); reserved > 0 && reader.preReadLimiter != nil {
		reader.preReadLimiter.release(reserved)
	}
	releaseAllPrefetchBuffers(reader)
	reader.wins.clearMeta()
}

func (reader *Reader) isSequentialRead(offset, size int) bool {
	if size <= 0 || !reader.hasLastRead {
		return false
	}
	if offset >= reader.lastReadOff && offset <= reader.lastReadEnd {
		return true
	}
	if offset > reader.lastReadEnd && offset-reader.lastReadEnd <= sequentialGapMax {
		return true
	}
	return false
}

func (reader *Reader) coversAnyWindow(offset, size int) bool {
	if size <= 0 {
		return false
	}
	if reader.wins.active.coversValidOrFill(offset, size) {
		return true
	}
	return reader.wins.standby.coversValidOrFill(offset, size)
}

func (reader *Reader) tryPrefetchHit(dst []byte, offset, size int) bool {
	return reader.wins.active.copyHit(dst, offset, size)
}

func (reader *Reader) tryStandbyHit(dst []byte, offset, size int) bool {
	return reader.wins.standby.copyHit(dst, offset, size)
}

func (p *aheadPair) ensure() {
	p.active = &aheadWin{}
	p.standby = &aheadWin{}
}

// clearMeta drops published/pending fill ranges but must NOT clear inflight.
// An in-flight filler still owns the window buffer until it finishes and clears inflight itself;
// zeroing inflight here would let a new schedule CAS succeed and double-write the same buf.
func (p *aheadPair) clearMeta() {
	p.active.off = 0
	p.active.valid = 0
	p.active.fillOff = 0
	p.active.fillLen = 0
	p.standby.off = 0
	p.standby.valid = 0
	p.standby.fillOff = 0
	p.standby.fillLen = 0
}

func (w *aheadWin) alreadyHas(start, fetch int) bool {
	return w.valid > 0 && start >= w.off && start+fetch <= w.off+w.valid
}

func (w *aheadWin) coversFill(offset, size int) bool {
	if atomic.LoadInt32(&w.inflight) == 0 || w.fillLen <= 0 {
		return false
	}
	return offset >= w.fillOff && offset+size <= w.fillOff+w.fillLen
}

// holdsExtent reports whether this window already has or is async-filling [start,start+fetch).
func (w *aheadWin) holdsExtent(start, fetch int) bool {
	if fetch <= 0 {
		return false
	}
	return w.alreadyHas(start, fetch) || w.coversFill(start, fetch)
}

func (w *aheadWin) coversValidOrFill(offset, size int) bool {
	if w.valid > 0 && offset >= w.off && offset+size <= w.off+w.valid {
		return true
	}
	return w.coversFill(offset, size)
}

func (w *aheadWin) copyHit(dst []byte, offset, size int) bool {
	return copyFromWindow(dst, offset, size, w.buf, w.off, w.valid)
}

// copyFromWindow requires [offset,offset+size) to be fully inside [base, base+valid); no partial copy.
func copyFromWindow(dst []byte, offset, size int, win []byte, base, valid int) bool {
	if valid <= 0 || win == nil || size <= 0 {
		return false
	}
	if offset < base {
		return false
	}
	rel := offset - base
	if rel+size > valid {
		return false
	}
	copy(dst[:size], win[rel:rel+size])
	return true
}

func seqAdvanceBytes(offset, size, lastEnd int) int {
	end := offset + size
	if end <= lastEnd {
		return 0
	}
	if offset >= lastEnd {
		return size
	}
	return end - lastEnd
}

func snapshotReadOnlyOeks(src *ReadOnlyOeks) *ReadOnlyOeks {
	if src.Len() == 0 {
		return NewReadOnlyOeks(nil)
	}
	items := make([]proto.ObjExtentKey, src.Len())
	for i := 0; i < src.Len(); i++ {
		items[i] = src.At(i)
	}
	return NewReadOnlyOeks(items)
}

// prefetchWindowOwnsBuf reports whether windowBuf still references the same array as dst.
func prefetchWindowOwnsBuf(windowBuf, dst []byte) bool {
	if cap(windowBuf) == 0 || cap(dst) == 0 || cap(windowBuf) < cap(dst) {
		return false
	}
	return &windowBuf[0] == &dst[0]
}
