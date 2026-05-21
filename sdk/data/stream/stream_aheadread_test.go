package stream

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/wrapper"
	"github.com/cubefs/cubefs/util"
)

func newTestStreamerWithAheadRead(t *testing.T, partitionID uint64) (*Streamer, *AheadReadCache) {
	t.Helper()

	w := &wrapper.Wrapper{}
	w.InitInnerReq(true)
	// Preload a DataPartition to avoid fetching from master
	dp := &wrapper.DataPartition{
		DataPartitionResponse: proto.DataPartitionResponse{
			PartitionID: partitionID,
		},
	}
	dp.ClientWrapper = w
	wrapper.InsertPartitionForTest(w, dp)

	client := &ExtentClient{}
	client.dataWrapper = w
	client.streamRetryTimeout = time.Second

	// Enable AheadRead cache
	arc := NewAheadReadCache(true, 16*util.MB, 100000, 2, util.CacheReadBlockSize)

	s := &Streamer{}
	s.client = client
	s.inode = 12345
	s.aheadReadEnable = true
	s.isOpen = true
	// Initialize extents to avoid nil pointer in getNextExtent
	s.extents = NewExtentCache(s.inode)
	// Construct AheadReadWindow (no background goroutine needed)
	s.aheadReadWindow = &AheadReadWindow{
		cache:    arc,
		streamer: s,
		taskC:    make(chan *AheadReadTask, arc.winCnt),
	}
	s.aheadReadBlockSize = util.CacheReadBlockSize
	return s, arc
}

func putCacheBlock(arc *AheadReadCache, inode, partitionID, extentID uint64, cacheOffset int, availSize int, fill byte) string {
	key := createAheadBlockKey(inode, partitionID, extentID, 0, cacheOffset)
	bv := &AheadReadBlock{}
	bv.inode = inode
	bv.partitionId = partitionID
	bv.extentId = extentID
	bv.offset = uint64(cacheOffset)
	bv.size = uint64(util.CacheReadBlockSize)
	bv.data = make([]byte, util.CacheReadBlockSize)
	for i := 0; i < availSize; i++ {
		bv.data[i] = fill
	}
	bv.time = time.Now().Unix()
	bv.key = key
	atomic.StoreUint64(&bv.readBytes, uint64(availSize))
	atomic.StoreUint32(&bv.state, AheadReadBlockStateInit)
	arc.blockCache.Store(key, bv)
	return key
}

func TestAheadRead_FullHit_SingleBlock(t *testing.T) {
	s, arc := newTestStreamerWithAheadRead(t, 1)
	defer arc.Stop()

	// Prepare a cache block starting at 0, 2MB available, fill 'A'
	avail := 2 * util.MB
	putCacheBlock(arc, s.inode, 1, 100, 0, avail, 'A')

	reqSize := 1 * util.MB
	offset := 512 * util.KB
	reqData := make([]byte, reqSize)
	ek := &proto.ExtentKey{PartitionId: 1, ExtentId: 100, FileOffset: 0, ExtentOffset: 0, Size: 8 * util.MB}
	req := &ExtentRequest{FileOffset: offset, Size: reqSize, Data: reqData, ExtentKey: ek}

	read, err := s.aheadRead(req, 0)
	if err != nil {
		t.Fatalf("aheadRead error: %v", err)
	}
	if read != reqSize {
		t.Fatalf("read size mismatch, want %d, got %d", reqSize, read)
	}
	for i := 0; i < reqSize; i++ {
		if reqData[i] != 'A' {
			t.Fatalf("unexpected data at %d, want 'A', got %v", i, reqData[i])
		}
	}
}

func TestAheadRead_PartialHit_SingleBlock(t *testing.T) {
	s, arc := newTestStreamerWithAheadRead(t, 2)
	defer arc.Stop()

	// Cache block [0, 800KB) available, fill 'A'
	avail := 800 * util.KB
	putCacheBlock(arc, s.inode, 2, 200, 0, avail, 'A')

	reqSize := 1 * util.MB
	offset := 512 * util.KB
	reqData := make([]byte, reqSize)
	ek := &proto.ExtentKey{PartitionId: 2, ExtentId: 200, FileOffset: 0, ExtentOffset: 0, Size: 8 * util.MB}
	req := &ExtentRequest{FileOffset: offset, Size: reqSize, Data: reqData, ExtentKey: ek}

	read, err := s.aheadRead(req, 0)
	if err != nil {
		t.Fatalf("aheadRead error: %v", err)
	}
	// Only 800KB-512KB=288KB should hit from cache
	want := 288 * util.KB
	if read != want {
		t.Fatalf("read size mismatch, want %d, got %d", want, read)
	}
	for i := 0; i < want; i++ {
		if reqData[i] != 'A' {
			t.Fatalf("unexpected data at %d, want 'A', got %v", i, reqData[i])
		}
	}
}

func TestAheadReadBlockPool_SizeGuard(t *testing.T) {
	orig := atomic.LoadInt64(&aheadReadBlockSize)
	defer atomic.StoreInt64(&aheadReadBlockSize, orig)

	// Drain any pooled blocks so the pool New func (allocating with the
	// currently published size) is exercised deterministically.
	atomic.StoreInt64(&aheadReadBlockSize, 2*int64(util.MB))
	blk := getAheadReadBlock()
	if int64(cap(blk.data)) != 2*int64(util.MB) {
		t.Fatalf("pooled block cap mismatch, want %d, got %d", 2*util.MB, cap(blk.data))
	}
	putAheadReadBlock(blk)

	// Publish a different size; getAheadReadBlock must reallocate the buffer
	// because the pooled block no longer matches the configured size.
	atomic.StoreInt64(&aheadReadBlockSize, 8*int64(util.MB))
	blk2 := getAheadReadBlock()
	if int64(cap(blk2.data)) != 8*int64(util.MB) {
		t.Fatalf("guard reallocation failed, want cap %d, got %d", 8*util.MB, cap(blk2.data))
	}
	putAheadReadBlock(blk2)

	// Same size again: guard branch should not reallocate (cap stays the same).
	blk3 := getAheadReadBlock()
	if int64(cap(blk3.data)) != 8*int64(util.MB) {
		t.Fatalf("unexpected cap on matching size, want %d, got %d", 8*util.MB, cap(blk3.data))
	}
	putAheadReadBlock(blk3)
}

func TestNewAheadReadCache_BlockSizeFallback(t *testing.T) {
	orig := atomic.LoadInt64(&aheadReadBlockSize)
	defer atomic.StoreInt64(&aheadReadBlockSize, orig)

	// Disabled cache returns nil regardless of other params.
	if arc := NewAheadReadCache(false, 16*util.MB, 100, 2, util.CacheReadBlockSize); arc != nil {
		t.Fatalf("expected nil cache when disabled, got %v", arc)
	}

	// Non-positive block size must fall back to the default (2MB).
	arc := NewAheadReadCache(true, 16*util.MB, 100, 2, 0)
	if arc == nil {
		t.Fatal("expected non-nil cache")
	}
	defer arc.Stop()
	if arc.blockSize != util.DefaultAheadReadBlockSize {
		t.Fatalf("expected fallback block size %d, got %d", util.DefaultAheadReadBlockSize, arc.blockSize)
	}
	if got := atomic.LoadInt64(&aheadReadBlockSize); got != util.DefaultAheadReadBlockSize {
		t.Fatalf("expected published block size %d, got %d", util.DefaultAheadReadBlockSize, got)
	}
	wantBlocks := int64(16*util.MB) / int64(util.DefaultAheadReadBlockSize)
	if arc.totalBlockCnt != wantBlocks {
		t.Fatalf("expected totalBlockCnt %d, got %d", wantBlocks, arc.totalBlockCnt)
	}

	// Explicit custom block size is honoured.
	custom := int64(2 * util.MB)
	arc2 := NewAheadReadCache(true, 16*util.MB, 100, 2, custom)
	if arc2 == nil {
		t.Fatal("expected non-nil cache for custom size")
	}
	defer arc2.Stop()
	if arc2.blockSize != custom {
		t.Fatalf("expected custom block size %d, got %d", custom, arc2.blockSize)
	}
}

func TestNewAheadReadCache_TotalBlockCntClamp(t *testing.T) {
	orig := atomic.LoadInt64(&aheadReadBlockSize)
	defer atomic.StoreInt64(&aheadReadBlockSize, orig)

	// totalMem smaller than a single block would yield totalBlockCnt == 0;
	// the cache must clamp it to 1 so prefetch stays usable.
	arc := NewAheadReadCache(true, 1*util.MB, 100, 2, 4*int64(util.MB))
	if arc == nil {
		t.Fatal("expected non-nil cache")
	}
	defer arc.Stop()
	if arc.totalBlockCnt != 1 {
		t.Fatalf("expected totalBlockCnt clamped to 1, got %d", arc.totalBlockCnt)
	}
	if got := atomic.LoadInt64(&arc.availableBlockCnt); got != 1 {
		t.Fatalf("expected availableBlockCnt 1, got %d", got)
	}
	if cap(arc.availableBlockC) != 1 {
		t.Fatalf("expected availableBlockC capacity 1, got %d", cap(arc.availableBlockC))
	}
}

func TestGetCurrentExtent_SkipSmallExtents(t *testing.T) {
	s, arc := newTestStreamerWithAheadRead(t, 9)
	defer arc.Stop()
	s.aheadReadBlockSize = util.CacheReadBlockSize

	// Small extent (below block size) must be skipped even if it covers the offset.
	small := &proto.ExtentKey{PartitionId: 9, ExtentId: 10, FileOffset: 0, Size: uint32(util.CacheReadBlockSize) - 1}
	// Large extent covering the same offset range; this is the expected match.
	large := &proto.ExtentKey{PartitionId: 9, ExtentId: 11, FileOffset: uint64(util.CacheReadBlockSize), Size: uint32(8 * util.MB)}
	s.extents.root.ReplaceOrInsert(small)
	s.extents.root.ReplaceOrInsert(large)

	// Offset inside the large extent: small extent is skipped, large extent returned.
	off := util.CacheReadBlockSize + 1024
	ek := s.getCurrentExtent(off)
	if ek == nil {
		t.Fatalf("expected to find large extent at offset %d", off)
	}
	if ek.ExtentId != 11 {
		t.Fatalf("expected extent 11, got %d", ek.ExtentId)
	}

	// Offset only covered by the small extent: nothing should match.
	if ek := s.getCurrentExtent(100); ek != nil {
		t.Fatalf("expected no extent when only small extent covers offset, got %v", ek)
	}
}

func TestAheadRead_CrossBlocks_FullHit(t *testing.T) {
	s, arc := newTestStreamerWithAheadRead(t, 3)
	defer arc.Stop()

	// Prepare two consecutive cache blocks:
	// Block0: [0, 4MB) fully available, fill 'A'
	putCacheBlock(arc, s.inode, 3, 300, 0, util.CacheReadBlockSize, 'A')
	// Block1: [4MB, 4MB+512KB) available, fill 'B'
	putCacheBlock(arc, s.inode, 3, 300, util.CacheReadBlockSize, 512*util.KB, 'B')

	reqSize := 1 * util.MB
	offset := util.CacheReadBlockSize - 512*util.KB // 3.5MB
	reqData := make([]byte, reqSize)
	ek := &proto.ExtentKey{PartitionId: 3, ExtentId: 300, FileOffset: 0, ExtentOffset: 0, Size: 8 * util.MB}
	req := &ExtentRequest{FileOffset: offset, Size: reqSize, Data: reqData, ExtentKey: ek}

	read, err := s.aheadRead(req, 0)
	if err != nil {
		t.Fatalf("aheadRead error: %v", err)
	}
	if read != reqSize {
		t.Fatalf("read size mismatch, want %d, got %d", reqSize, read)
	}
	// First 512KB from block0 ('A'), next 512KB from block1 ('B')
	for i := 0; i < 512*util.KB; i++ {
		if reqData[i] != 'A' {
			t.Fatalf("unexpected data A at %d, got %v", i, reqData[i])
		}
	}
	for i := 512 * util.KB; i < reqSize; i++ {
		if reqData[i] != 'B' {
			t.Fatalf("unexpected data B at %d, got %v", i, reqData[i])
		}
	}
}

func TestAheadRead_DoTask_ReadFailed(t *testing.T) {
	s, arc := newTestStreamerWithAheadRead(t, 4)
	defer arc.Stop()

	// Create a task with an invalid host to simulate read failure
	dp := &wrapper.DataPartition{
		DataPartitionResponse: proto.DataPartitionResponse{
			PartitionID: 4,
			Hosts:       []string{"127.0.0.1:1"}, // invalid host
		},
	}
	ek := &proto.ExtentKey{PartitionId: 4, ExtentId: 400, FileOffset: 0, ExtentOffset: 0, Size: 8 * util.MB}
	p := NewReadPacket(ek, 0, util.CacheReadBlockSize, s.inode, 0, false)
	req := &ExtentRequest{
		FileOffset: 0,
		Size:       util.CacheReadBlockSize,
		ExtentKey:  ek,
	}

	task := &AheadReadTask{
		p:         p,
		dp:        dp,
		time:      time.Now(),
		req:       req,
		cacheSize: util.CacheReadBlockSize,
		cacheType: "test",
		logTime:   &time.Time{},
		reqID:     "req-1",
		poolId:    0,
		retry:     MaxCacheBlockRetry + 1, // set to max to avoid pushing back to taskC
	}

	key := createAheadBlockKey(s.inode, 4, 400, 0, 0)

	// Ensure block is not in cache before
	if _, ok := arc.blockCache.Load(key); ok {
		t.Fatalf("block should not be in cache")
	}

	// Call doTask directly
	s.aheadReadWindow.doTask(task)

	// Block should be deleted from cache
	if _, ok := arc.blockCache.Load(key); ok {
		t.Fatalf("block should be deleted from cache after read failure")
	}
}

func TestAheadRead_BackgroundTaskTickerStop(t *testing.T) {
	// This test verifies that backgroundAheadReadTask stops its ticker
	// when the streamer is closed, covering the defer ticker.Stop() line.
	arc := NewAheadReadCache(true, 16*util.MB, 100000, 2)

	s := &Streamer{}
	s.inode = 99999
	s.isOpen = false // stream is closed so backgroundAheadReadTask will exit on ticker

	arw := &AheadReadWindow{
		taskC:    make(chan *AheadReadTask, arc.winCnt),
		cache:    arc,
		streamer: s,
	}

	// Start backgroundAheadReadTask — it will see isOpen==false on the next
	// ticker tick and return, which triggers defer ticker.Stop().
	done := make(chan struct{})
	go func() {
		arw.backgroundAheadReadTask()
		close(done)
	}()

	// Wait for the goroutine to exit (it should exit within ~1s ticker interval)
	select {
	case <-done:
		// backgroundAheadReadTask exited, defer ticker.Stop() was executed
	case <-time.After(5 * time.Second):
		t.Fatal("backgroundAheadReadTask did not exit within 5s, defer ticker.Stop() may not have been called")
	}

	arc.Stop()
}

func TestAheadRead_EvictCacheBlock(t *testing.T) {
	s, arc := newTestStreamerWithAheadRead(t, 5)
	defer arc.Stop()

	// Prepare a cache block in Init state
	key := putCacheBlock(arc, s.inode, 5, 500, 0, util.CacheReadBlockSize, 'A')

	// Verify it's in cache
	if _, ok := arc.blockCache.Load(key); !ok {
		t.Fatalf("block should be in cache")
	}

	req := &ExtentRequest{
		FileOffset: 0,
		Size:       util.CacheReadBlockSize,
		ExtentKey:  &proto.ExtentKey{PartitionId: 5, ExtentId: 500, FileOffset: 0, ExtentOffset: 0, Size: 8 * util.MB},
	}

	s.aheadReadWindow.evictCacheBlock(req)

	// Verify it's deleted from cache
	if _, ok := arc.blockCache.Load(key); ok {
		t.Fatalf("block should be deleted from cache after evictCacheBlock")
	}
}
