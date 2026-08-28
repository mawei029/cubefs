// Copyright 2026 The CubeFS Authors.
package blobstore

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brahma-adshonor/gohook"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/proto"
)

func prefetchTestReader(t *testing.T, ino uint64, blockSize int) *Reader {
	t.Helper()
	s := mustTestECStreamerWithEbsc(ino, nil, blockSize)
	r := s.fReader
	r.aheadReadEnable = true
	r.minReadAheadSize = 0
	r.preReadLimiter = &blobPreReadLimiter{maxBytes: 1 << 20}
	return r
}

var prefetchReadFill uint32

func mockPrefetchReadFill(ebsc *BlobStoreClient, ctx context.Context, volName string,
	buf []byte, offset uint64, size uint64, oek proto.ObjExtentKey,
) (int, error) {
	n := int(size)
	if n > len(buf) {
		n = len(buf)
	}
	fill := byte(atomic.LoadUint32(&prefetchReadFill))
	if fill == 0 {
		fill = 0xAA
	}
	for i := 0; i < n; i++ {
		buf[i] = fill
	}
	return n, nil
}

// Covers flags, copy/hit helpers, holdsExtent/coversFill, promote/clear, seq advance.
func TestReaderPrefetch_windowBasics(t *testing.T) {
	s := mustTestECStreamerWithEbsc(801, nil, 16)
	r := s.fReader
	require.False(t, r.prefetchEnabled())
	require.Equal(t, 0, r.prefetchBufCap())

	r.aheadReadEnable = true
	require.True(t, r.prefetchEnabled())
	require.Equal(t, 32, r.prefetchBufCap()) // dual * blockSize(16)
	r.seqHeatBytes = prefetchHeatBytes - 1
	require.False(t, prefetchReady(r))
	r.seqHeatBytes = prefetchHeatBytes
	require.True(t, prefetchReady(r))

	win := []byte{0, 1, 2, 3, 4, 5, 6, 7}
	dst := make([]byte, 4)
	require.False(t, copyFromWindow(dst, 0, 4, nil, 0, 8))
	require.False(t, copyFromWindow(dst, 0, 4, win, 0, 0))
	require.False(t, copyFromWindow(dst, 2, 4, win, 4, 4))
	require.False(t, copyFromWindow(dst, 6, 4, win, 0, 8))
	require.True(t, copyFromWindow(dst, 2, 4, win, 0, 8))
	require.Equal(t, []byte{2, 3, 4, 5}, dst)

	r.wins = aheadPair{active: &aheadWin{buf: win, off: 8, valid: 8}, standby: &aheadWin{}}
	require.True(t, r.tryPrefetchHit(dst, 10, 4))
	require.Equal(t, []byte{2, 3, 4, 5}, dst)
	require.False(t, r.tryStandbyHit(dst, 10, 4))

	w := aheadWin{off: 8, valid: 8}
	require.True(t, w.alreadyHas(8, 8))
	require.False(t, w.alreadyHas(7, 2))
	require.False(t, w.holdsExtent(0, 0))
	require.True(t, w.holdsExtent(8, 8))
	require.False(t, w.coversFill(8, 4))
	atomic.StoreInt32(&w.inflight, 1)
	w.fillOff, w.fillLen = 8, 16
	require.True(t, w.coversFill(8, 4))
	require.False(t, w.coversFill(21, 4))
	require.True(t, w.holdsExtent(8, 16))
	require.False(t, w.holdsExtent(8, 17))
	require.True(t, w.coversValidOrFill(10, 4))

	r.wins = aheadPair{
		active:  &aheadWin{buf: []byte{1}, off: 0, valid: 8},
		standby: &aheadWin{buf: []byte{2}, off: 8, valid: 8},
	}
	atomic.StoreInt32(&r.wins.active.inflight, 1)
	atomic.StoreInt32(&r.wins.standby.inflight, 7)
	origActive, origStandby := r.wins.active, r.wins.standby
	r.promoteStandby()
	require.Equal(t, origStandby, r.wins.active)
	require.Equal(t, origActive, r.wins.standby)
	require.Equal(t, int32(7), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, int32(1), atomic.LoadInt32(&r.wins.standby.inflight))
	r.wins.clearMeta()
	require.Equal(t, 0, r.wins.active.off)
	require.Equal(t, 0, r.wins.active.valid)
	require.Equal(t, 0, r.wins.standby.off)
	require.Equal(t, 0, r.wins.standby.valid)
	// inflight must survive clearMeta so an in-flight filler keeps exclusive ownership.
	require.Equal(t, int32(7), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, int32(1), atomic.LoadInt32(&r.wins.standby.inflight))
	atomic.StoreInt32(&r.wins.active.inflight, 0)
	atomic.StoreInt32(&r.wins.standby.inflight, 0)

	require.Equal(t, 0, seqAdvanceBytes(0, 8, 8))
	require.Equal(t, 8, seqAdvanceBytes(8, 8, 8))
	require.Equal(t, 4, seqAdvanceBytes(4, 8, 8))
	seqR := &Reader{}
	require.False(t, seqR.isSequentialRead(0, 4))
	seqR.hasLastRead = true
	seqR.lastReadOff, seqR.lastReadEnd = 0, 128
	require.True(t, seqR.isSequentialRead(64, 64))
	require.True(t, seqR.isSequentialRead(128, 64))
	require.True(t, seqR.isSequentialRead(128+sequentialGapMax, 64))
	require.False(t, seqR.isSequentialRead(128+sequentialGapMax+1, 64))
}

// Heat/cool, release cache, oek snapshot.
func TestReaderPrefetch_observeHeatCoolAndCache(t *testing.T) {
	r := prefetchTestReader(t, 802, 32)
	require.True(t, r.ensurePrefetchBuf())
	r.wins.active.off, r.wins.active.valid = 0, 32
	gen0 := atomic.LoadUint64(&r.prefetchGen)

	r.observeRead(0, 128<<10)
	require.False(t, prefetchReady(r))
	require.Equal(t, uint64(0), r.seqHeatBytes)

	const step = 128 << 10
	off := step
	for r.seqHeatBytes < prefetchHeatBytes {
		r.observeRead(off, step)
		off += step
	}
	require.True(t, prefetchReady(r))
	r.observeRead(off+16, step)
	require.True(t, prefetchReady(r))
	r.observeRead(0, step)
	require.False(t, prefetchReady(r))
	require.Equal(t, uint64(0), r.seqHeatBytes)
	require.Greater(t, atomic.LoadUint64(&r.prefetchGen), gen0)
	require.Equal(t, 0, r.wins.active.valid)

	r2 := prefetchTestReader(t, 803, 16)
	require.True(t, r2.ensurePrefetchBuf())
	r2.wins.active.off, r2.wins.active.valid = 1, 8
	reserved := r2.prefetchReserved
	require.Greater(t, reserved, int64(0))
	gen1 := atomic.LoadUint64(&r2.prefetchGen)
	r2.releasePrefetchCache()
	require.Greater(t, atomic.LoadUint64(&r2.prefetchGen), gen1)
	require.Equal(t, int64(0), r2.prefetchReserved)
	require.Nil(t, r2.wins.active.buf)

	empty := snapshotReadOnlyOeks(NewReadOnlyOeks(nil))
	require.NotNil(t, empty)
	require.Equal(t, 0, empty.Len())
	src := NewReadOnlyOeks([]proto.ObjExtentKey{{FileOffset: 0, Size: 8}, {FileOffset: 8, Size: 8}})
	snap := snapshotReadOnlyOeks(src)
	require.Equal(t, 2, snap.Len())
	require.Equal(t, src.At(1), snap.At(1))
}

// Dedup via holdsExtent, early kick after 1MiB, kickStandbyNext after promote, miss without wait on active inflight.
func TestReaderPrefetch_dedupKickAndMiss(t *testing.T) {
	const (
		ext0 = 0
		ext1 = 2 << 20
		ext2 = 4 << 20
		esz  = 2 << 20
		fsz  = 6 << 20
	)
	ebsc := newSafeBlobStoreClientForTest()
	s := mustTestECStreamerWithEbsc(804, ebsc, esz)
	seedStreamerExtentsForTest(s, fsz, []proto.ObjExtentKey{
		{FileOffset: ext0, Size: esz},
		{FileOffset: ext1, Size: esz},
		{FileOffset: ext2, Size: esz},
	})
	r := s.fReader
	r.readConcurrency = 1
	r.aheadReadEnable = true
	r.minReadAheadSize = 0
	r.preReadLimiter = &blobPreReadLimiter{maxBytes: 16 << 20}
	require.True(t, r.ensurePrefetchBuf())

	atomic.StoreUint32(&prefetchReadFill, 0xBB)
	t.Cleanup(func() { atomic.StoreUint32(&prefetchReadFill, 0) })
	require.NoError(t, gohook.HookMethod(ebsc, "Read", mockPrefetchReadFill, nil))
	t.Cleanup(func() { _ = gohook.UnHookMethod(ebsc, "Read") })

	r.wins.active.off, r.wins.active.valid = ext0, esz
	r.kickStandbyNext(context.Background(), fsz)
	require.True(t, anyPrefetchInflight(r) || r.wins.standby.valid > 0 || r.wins.standby.holdsExtent(ext1, esz))
	waitAsyncPrefetchForTest(r)
	require.True(t, r.wins.standby.alreadyHas(ext1, esz) || r.wins.standby.valid == esz)
	require.Equal(t, ext1, r.wins.standby.off)

	// Second kick is idempotent (standby already has next oek).
	r.kickStandbyNext(context.Background(), fsz)
	require.False(t, anyPrefetchInflight(r))
	require.Equal(t, ext1, r.wins.standby.off)

	// Dedup: same extent already on standby must not schedule into active.
	require.Equal(t, ext0, r.wins.active.off)
	require.Equal(t, esz, r.wins.active.valid)
	r.scheduleAsyncExtent(context.Background(), proto.ObjExtentKey{FileOffset: ext1, Size: esz}, fsz, false)
	require.Equal(t, int32(0), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, ext0, r.wins.active.off)
	require.Equal(t, esz, r.wins.active.valid)

	// Inflight fill also counts as holdsExtent for cross-window dedup.
	atomic.StoreInt32(&r.wins.standby.inflight, 1)
	r.wins.standby.fillOff, r.wins.standby.fillLen = ext2, esz
	r.wins.standby.off, r.wins.standby.valid = 0, 0
	require.True(t, r.wins.standby.holdsExtent(ext2, esz))
	r.scheduleAsyncExtent(context.Background(), proto.ObjExtentKey{FileOffset: ext2, Size: esz}, fsz, false)
	require.Equal(t, int32(0), atomic.LoadInt32(&r.wins.active.inflight))
	atomic.StoreInt32(&r.wins.standby.inflight, 0)
	r.wins.standby.fillOff, r.wins.standby.fillLen = 0, 0

	r.wins.standby.off, r.wins.standby.valid = ext1, esz
	r.kickStandbyNext(context.Background(), fsz) // alreadyHas next → no-op
	require.False(t, anyPrefetchInflight(r))

	// Active inflight covering read: sync path, coversFill is not a miss.
	heatReaderPrefetch(r, 0)
	atomic.StoreInt32(&r.wins.active.inflight, 1)
	r.wins.active.fillOff, r.wins.active.fillLen = 0, 16
	r.wins.active.off, r.wins.active.valid = 0, 0
	r.wins.standby.off, r.wins.standby.valid = 0, 0
	r.missStreak = 0
	atomic.StoreUint32(&prefetchReadFill, 0xAA)
	buf := make([]byte, 4)
	n, err := readUnderStreamerMu(s, context.Background(), buf, 0, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, []byte{0xAA, 0xAA, 0xAA, 0xAA}, buf)
	require.Equal(t, uint32(0), atomic.LoadUint32(&r.missStreak))
	atomic.StoreInt32(&r.wins.active.inflight, 0)
	waitAsyncPrefetchForTest(r)
}

// coversFill → sync without miss; miss_streak cool; late filler keeps own window after promote.
func TestReaderPrefetch_coversFillMissCoolAndPromote(t *testing.T) {
	const (
		esz = 64
		fsz = 128
	)
	ebsc := newSafeBlobStoreClientForTest()
	s := mustTestECStreamerWithEbsc(808, ebsc, esz)
	seedStreamerExtentsForTest(s, fsz, []proto.ObjExtentKey{
		{FileOffset: 0, Size: esz},
		{FileOffset: esz, Size: esz},
	})
	r := s.fReader
	r.readConcurrency = 1
	r.aheadReadEnable = true
	r.minReadAheadSize = 0
	r.preReadLimiter = &blobPreReadLimiter{maxBytes: 512}
	require.True(t, r.ensurePrefetchBuf())
	heatReaderPrefetch(r, 0)
	r.prefetchHit = true
	r.missStreak = 3 // non-zero to prove coversFill does not bump

	atomic.StoreUint32(&prefetchReadFill, 0xCC)
	t.Cleanup(func() { atomic.StoreUint32(&prefetchReadFill, 0) })
	require.NoError(t, gohook.HookMethod(ebsc, "Read", mockPrefetchReadFill, nil))
	t.Cleanup(func() { _ = gohook.UnHookMethod(ebsc, "Read") })

	// Standby inflight covers the next oek: sync EBS, no Wait, missStreak unchanged, no promote.
	standby := r.wins.standby
	atomic.StoreInt32(&standby.inflight, 1)
	standby.fillOff, standby.fillLen = esz, esz
	standby.off, standby.valid = 0, 0

	buf := make([]byte, 4)
	n, err := readUnderStreamerMu(s, context.Background(), buf, esz, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, []byte{0xCC, 0xCC, 0xCC, 0xCC}, buf)
	require.Equal(t, uint32(3), atomic.LoadUint32(&r.missStreak))
	require.Equal(t, 0, r.wins.active.off) // not promoted
	atomic.StoreInt32(&standby.inflight, 0)
	standby.fillOff, standby.fillLen = 0, 0
	waitAsyncPrefetchForTest(r)

	// miss_streak cool: one more miss after streak already at coolMissStreakCnt-1.
	r.wins.active.off, r.wins.active.valid = 0, 0
	r.wins.standby.off, r.wins.standby.valid = 0, 0
	r.missStreak = coolMissStreakCnt - 1
	r.seqHeatBytes = prefetchHeatBytes
	r.prefetchHit = true
	atomic.StoreUint32(&prefetchReadFill, 0xDD)
	n, err = readUnderStreamerMu(s, context.Background(), buf, 0, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.False(t, prefetchReady(r))
	require.Equal(t, uint32(0), atomic.LoadUint32(&r.missStreak))
	require.Equal(t, uint64(0), r.seqHeatBytes)
	waitAsyncPrefetchForTest(r)

	// Late filler writes its own *aheadWin after promote swaps pointers.
	r2 := prefetchTestReader(t, 809, 16)
	require.True(t, r2.ensurePrefetchBuf())
	active := r2.wins.active
	standby2 := r2.wins.standby
	for i := 0; i < 16; i++ {
		standby2.buf[i] = 0x22
	}
	standby2.off, standby2.valid = 16, 16
	atomic.StoreInt32(&active.inflight, 1)
	active.fillOff, active.fillLen = 0, 16
	r2.promoteStandby()
	require.Equal(t, standby2, r2.wins.active)
	require.Equal(t, active, r2.wins.standby)
	for i := 0; i < 16; i++ {
		active.buf[i] = 0xAA
	}
	active.off, active.valid = 0, 16
	atomic.StoreInt32(&active.inflight, 0)
	require.Equal(t, byte(0x22), r2.wins.active.buf[0])
	require.Equal(t, 16, r2.wins.active.off)
	require.Equal(t, byte(0xAA), r2.wins.standby.buf[0])
	require.Equal(t, 0, r2.wins.standby.off)
}

// Adaptive: unused false-heat cools on non-seq; cold miss cools fast; async kicks stop after asyncKickMissLimit.
func TestReaderPrefetch_adaptiveRandomDisarm(t *testing.T) {
	r := prefetchTestReader(t, 810, 32)
	r.preReadLimiter = &blobPreReadLimiter{maxBytes: 1 << 20}
	require.True(t, r.ensurePrefetchBuf())

	// Build heat with tight sequential steps, then a medium gap ( >128KiB, <BlockSize) without useful hit → cool.
	const step = 128 << 10
	r.observeRead(0, step)
	off := step
	for r.seqHeatBytes < prefetchHeatBytes {
		r.observeRead(off, step)
		off += step
	}
	require.True(t, prefetchReady(r))
	require.False(t, r.prefetchHit)
	// Gap > sequentialGapMax but still below default coolSeekThreshold when streamer BlockSize is tiny:
	// force non-seq by jumping far past lastEnd without covering windows.
	r.ecStreamer.blockSize = 8 << 20
	r.observeRead(off+sequentialGapMax+1, step)
	require.False(t, prefetchReady(r))
	require.Equal(t, uint64(0), r.seqHeatBytes)

	ebsc := newSafeBlobStoreClientForTest()
	s := mustTestECStreamerWithEbsc(811, ebsc, 16)
	seedStreamerExtentsForTest(s, 256, []proto.ObjExtentKey{
		{FileOffset: 0, Size: 16}, {FileOffset: 16, Size: 16}, {FileOffset: 32, Size: 16},
	})
	r2 := s.fReader
	r2.readConcurrency = 1
	r2.aheadReadEnable = true
	r2.minReadAheadSize = 0
	r2.preReadLimiter = &blobPreReadLimiter{maxBytes: 512}
	require.True(t, r2.ensurePrefetchBuf())
	heatReaderPrefetch(r2, 0)
	require.False(t, r2.prefetchHit)

	atomic.StoreUint32(&prefetchReadFill, 0xEE)
	t.Cleanup(func() { atomic.StoreUint32(&prefetchReadFill, 0) })
	require.NoError(t, gohook.HookMethod(ebsc, "Read", mockPrefetchReadFill, nil))
	t.Cleanup(func() { _ = gohook.UnHookMethod(ebsc, "Read") })

	buf := make([]byte, 4)
	for i := 0; i < coolMissColdCnt; i++ {
		n, err := readUnderStreamerMu(s, context.Background(), buf, 48, 4) // hole-ish / miss
		require.NoError(t, err)
		require.Equal(t, 4, n)
	}
	require.False(t, prefetchReady(r2))
	require.Equal(t, uint32(0), atomic.LoadUint32(&r2.missStreak))
	require.False(t, r2.prefetchHit)
	waitAsyncPrefetchForTest(r2)

	// After a hit, miss beyond asyncKickMissLimit must not schedule more async.
	heatReaderPrefetch(r2, 0)
	r2.prefetchHit = true
	r2.wins.active.off, r2.wins.active.valid = 0, 0
	r2.wins.standby.off, r2.wins.standby.valid = 0, 0
	r2.missStreak = asyncKickMissLimit
	n, err := readUnderStreamerMu(s, context.Background(), buf, 0, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, uint32(asyncKickMissLimit+1), atomic.LoadUint32(&r2.missStreak))
	require.False(t, anyPrefetchInflight(r2))
}

// Concurrency (invalidate vs inflight), no backward OEK kick after promote, canceled schedule ctx
// must not cancel Background async fill / subsequent Read.
func TestReaderPrefetch_invalidateRewindCanceledCtx(t *testing.T) {
	const (
		esz = 64
		fsz = 192
	)
	ebsc := newSafeBlobStoreClientForTest()
	s := mustTestECStreamerWithEbsc(820, ebsc, esz)
	seedStreamerExtentsForTest(s, fsz, []proto.ObjExtentKey{
		{FileOffset: 0, Size: esz},
		{FileOffset: esz, Size: esz},
		{FileOffset: esz * 2, Size: esz},
	})
	r := s.fReader
	r.readConcurrency = 1
	r.aheadReadEnable = true
	r.minReadAheadSize = 0
	r.preReadLimiter = &blobPreReadLimiter{maxBytes: 512}
	require.True(t, r.ensurePrefetchBuf())

	atomic.StoreUint32(&prefetchReadFill, 0xAB)
	t.Cleanup(func() { atomic.StoreUint32(&prefetchReadFill, 0) })
	require.NoError(t, gohook.HookMethod(ebsc, "Read", mockPrefetchReadFill, nil))
	t.Cleanup(func() { _ = gohook.UnHookMethod(ebsc, "Read") })

	// 1) invalidate must not clear inflight → second schedule cannot CAS into the same window.
	atomic.StoreInt32(&r.wins.active.inflight, 1)
	r.wins.active.fillOff, r.wins.active.fillLen = 0, esz
	r.wins.active.off, r.wins.active.valid = 0, esz
	gen0 := atomic.LoadUint64(&r.prefetchGen)
	r.invalidateReadBuf()
	require.Equal(t, gen0+1, atomic.LoadUint64(&r.prefetchGen))
	require.Equal(t, int32(1), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, 0, r.wins.active.valid)
	r.scheduleAsyncExtent(context.Background(), proto.ObjExtentKey{FileOffset: 0, Size: esz}, fsz, false)
	require.Equal(t, int32(1), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, 0, r.wins.active.valid)
	atomic.StoreInt32(&r.wins.active.inflight, 0)

	// 2) After promote to [esz,2*esz), rewind miss must not re-schedule oek[0].
	for i := 0; i < esz; i++ {
		r.wins.active.buf[i] = 0x11
		r.wins.standby.buf[i] = 0x22
	}
	r.wins.active.off, r.wins.active.valid = esz, esz
	r.wins.standby.off, r.wins.standby.valid = 0, 0
	r.ensureOekAsyncWindows(context.Background(), esz-8, fsz)
	require.Equal(t, int32(0), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, esz, r.wins.active.off)
	require.Equal(t, esz, r.wins.active.valid)
	require.Equal(t, byte(0x11), r.wins.active.buf[0])
	waitAsyncPrefetchBrief(t, r)

	// 3) Canceled parent ctx must not abort async fill (schedule uses Background); Read still hits.
	r.wins.active.off, r.wins.active.valid = 0, 0
	r.wins.standby.off, r.wins.standby.valid = 0, 0
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	r.scheduleAsyncExtent(canceled, proto.ObjExtentKey{FileOffset: 0, Size: esz}, fsz, false)
	waitAsyncPrefetchBrief(t, r)
	require.Equal(t, esz, r.wins.active.valid)
	require.Equal(t, 0, r.wins.active.off)
	heatReaderPrefetch(r, 0)
	buf := make([]byte, 4)
	n, err := readUnderStreamerMu(s, context.Background(), buf, 0, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, []byte{0xAB, 0xAB, 0xAB, 0xAB}, buf)
	require.Equal(t, uint32(0), atomic.LoadUint32(&r.missStreak))
	require.True(t, r.prefetchHit)
}

func waitAsyncPrefetchBrief(t *testing.T, reader *Reader) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for anyPrefetchInflight(reader) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.False(t, anyPrefetchInflight(reader), "async prefetch still inflight after brief wait")
}

var prefetchBlockGate chan struct{}

func mockPrefetchReadFillGated(ebsc *BlobStoreClient, ctx context.Context, volName string,
	buf []byte, offset uint64, size uint64, oek proto.ObjExtentKey,
) (int, error) {
	if ch := prefetchBlockGate; ch != nil {
		<-ch
	}
	return mockPrefetchReadFill(ebsc, ctx, volName, buf, offset, size, oek)
}

// Close/release while async fill is in flight must not Put the window buffer into the pool
// (filler still writes it); limiter quota is returned immediately; filler must not publish.
func TestReaderPrefetch_releaseDuringInflight(t *testing.T) {
	const (
		esz = 64
		fsz = 64
	)
	ebsc := newSafeBlobStoreClientForTest()
	s := mustTestECStreamerWithEbsc(821, ebsc, esz)
	seedStreamerExtentsForTest(s, fsz, []proto.ObjExtentKey{
		{FileOffset: 0, Size: esz},
	})
	r := s.fReader
	r.readConcurrency = 1
	r.aheadReadEnable = true
	r.minReadAheadSize = 0
	limiter := &blobPreReadLimiter{maxBytes: 512}
	r.preReadLimiter = limiter
	require.True(t, r.ensurePrefetchBuf())
	require.Greater(t, r.prefetchReserved, int64(0))
	require.Equal(t, r.prefetchReserved, atomic.LoadInt64(&limiter.usedBytes))

	gate := make(chan struct{})
	prefetchBlockGate = gate
	t.Cleanup(func() { prefetchBlockGate = nil })

	atomic.StoreUint32(&prefetchReadFill, 0xCD)
	t.Cleanup(func() { atomic.StoreUint32(&prefetchReadFill, 0) })
	require.NoError(t, gohook.HookMethod(ebsc, "Read", mockPrefetchReadFillGated, nil))
	t.Cleanup(func() { _ = gohook.UnHookMethod(ebsc, "Read") })

	s.mu.Lock()
	r.scheduleAsyncExtent(context.Background(), proto.ObjExtentKey{FileOffset: 0, Size: esz}, fsz, false)
	s.mu.Unlock()

	deadline := time.Now().Add(200 * time.Millisecond)
	for atomic.LoadInt32(&r.wins.active.inflight) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	require.Equal(t, int32(1), atomic.LoadInt32(&r.wins.active.inflight))
	require.NotNil(t, r.wins.active.buf)

	// Concurrent pool user: safe only if release did not Put the still-written array.
	poolMax := r.prefetchBufCap()
	other := readerGetBuf(esz, poolMax)
	require.NotNil(t, other)

	s.mu.Lock()
	s.dropIOCachesLocked()
	s.mu.Unlock()

	require.Nil(t, r.wins.active.buf)
	require.Equal(t, int32(1), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, int64(0), r.prefetchReserved)
	require.Equal(t, int64(0), atomic.LoadInt64(&limiter.usedBytes))

	// If release wrongly Put the in-flight array, Get may return it; concurrent write races filler.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			for j := range other {
				other[j] = byte(i)
			}
		}
	}()
	close(gate)
	waitAsyncPrefetchBrief(t, r)
	<-done
	readerPutBuf(other, poolMax)

	require.Equal(t, int32(0), atomic.LoadInt32(&r.wins.active.inflight))
	require.Equal(t, 0, r.wins.active.valid)
	require.Nil(t, r.wins.active.buf)

	// Reopen / re-arm path: budget can be acquired again (no leak from prior close).
	require.True(t, r.ensurePrefetchBuf())
	require.Greater(t, r.prefetchReserved, int64(0))
	require.Equal(t, r.prefetchReserved, atomic.LoadInt64(&limiter.usedBytes))
	r.releasePrefetchCache()
	require.Equal(t, int64(0), r.prefetchReserved)
	require.Equal(t, int64(0), atomic.LoadInt64(&limiter.usedBytes))
}
