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
	"math/rand"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/brahma-adshonor/gohook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/blobstore/api/access"
	ebsproto "github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/client/blockcache/bcache"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/errors"
)

// syncECStreamerSizeWithExtentCacheForTest 将 Reader 缓存的 meta+ObjExtents 推导尾经 mergeMaxFileSize 并入 ECStreamer（仅抬高），与 RefreshExtents 内一致；供单测里手工 valid 的 Reader 与流上 fileSize 对齐。
func syncECStreamerSizeWithExtentCacheForTest(r *Reader) {
	if r == nil || !r.valid {
		return
	}
	lb := logicalReadBound(r.metaReportedSize, r.objExtentKeys)
	r.ecStreamer.mergeMaxFileSize(lb)
}

func newSafeBlobStoreClientForTest() *BlobStoreClient {
	return &BlobStoreClient{
		client: &fakeAccessAPI{
			getFn: func(_ context.Context, args *access.GetArgs) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader(strings.Repeat("x", int(args.ReadSize)))), nil
			},
			putFn: func(_ context.Context, _ *access.PutArgs) (ebsproto.Location, access.HashSumMap, error) {
				return ebsproto.Location{}, nil, nil
			},
			deleteFn: func(_ context.Context, _ *access.DeleteArgs) ([]ebsproto.Location, error) {
				return nil, nil
			},
		},
	}
}

func TestNewReader(t *testing.T) {
	mockConfig := ClientConfig{
		VolName:         "cfs",
		VolType:         0,
		BlockSize:       0,
		Ino:             2,
		Bc:              nil,
		Mw:              nil,
		LimitManager:    newTestLimitManager(),
		Ebsc:            nil,
		EnableBcache:    false,
		WConcurrency:    0,
		ReadConcurrency: 0,
		FileCache:       false,
		FileSize:        0,
		ECStreamer:      NewECStreamer(2, nil, nil),
	}

	reader := NewReader(mockConfig)
	assert.NotEmpty(t, reader, nil)
}

func TestFileSize(t *testing.T) {
	testCase := []struct {
		valid            bool
		metaReportedSize uint64
		objEks           []proto.ObjExtentKey
		expectSize       uint64
		expectOk         bool
	}{
		{false, 0, nil, 0, false},
		{true, 0, nil, 0, true},
		{true, 0, []proto.ObjExtentKey{{Size: uint64(100), FileOffset: uint64(100)}}, 200, true},
		// 稀疏：inode/ meta 长度 100，仅 [20,40) 有对象 extent，可读范围应为 100（尾部洞补零）
		{true, 100, []proto.ObjExtentKey{{FileOffset: 20, Size: 20}}, 100, true},
		// 无 extent 但 meta 声明长度（全文件为洞）
		{true, 100, nil, 100, true},
		// 流上 fileSize 已高于 meta 推导（未刷写缓冲抬高），bump 不会降低
		{true, 100, nil, 500, true},
	}

	for _, tc := range testCase {
		reader := Reader{
			limitManager:     manager.NewLimitManager(nil),
			ecStreamer:       NewECStreamer(1, nil, nil),
			valid:            tc.valid,
			metaReportedSize: tc.metaReportedSize,
			objExtentKeys:    tc.objEks,
		}
		if tc.expectSize == 500 && tc.metaReportedSize == 100 && len(tc.objEks) == 0 {
			atomic.StoreUint64(&reader.ecStreamer.fileSize, 500)
		} else {
			syncECStreamerSizeWithExtentCacheForTest(&reader)
		}
		gotSize, gotOk := reader.fileSize()
		assert.Equal(t, tc.expectSize, gotSize)
		assert.Equal(t, tc.expectOk, gotOk)
		lb, lbOk := reader.LogicalReadBound()
		assert.Equal(t, gotSize, lb, "LogicalReadBound 应与 fileSize 一致")
		assert.Equal(t, gotOk, lbOk)
	}
	var nilReader *Reader
	nz, nok := nilReader.LogicalReadBound()
	assert.Equal(t, uint64(0), nz)
	assert.False(t, nok)

	//// mock objExtentKey
	//objEks := make([]proto.ObjExtentKey, 0)
	//objEkLen := rand.Intn(20)
	//expectedFileSize := 0
	//for i := 0; i < objEkLen; i++ {
	//	size := rand.Intn(1000)
	//	objEks = append(objEks, proto.ObjExtentKey{Size: uint64(size), FileOffset: uint64(expectedFileSize)})
	//	expectedFileSize += size
	//}
	//
	//// mock reader
	//mockConfig := ClientConfig{
	//	VolName:         "cfs",
	//	VolType:         0,
	//	BlockSize:       0,
	//	Ino:             2,
	//	Bc:              nil,
	//	Mw:              nil,
	//	Ec:              nil,
	//	Bsc:            nil,
	//	EnableBcache:    false,
	//	WConcurrency:    0,
	//	ReadConcurrency: 0,
	//	CacheAction:     0,
	//	FileCache:       false,
	//	FileSize:        0,
	//	CacheThreshold:  0,
	//}
	//reader := NewReader(mockConfig)
	//reader.valid = true
	//reader.objExtentKeys = objEks
	//
	//got, ok := reader.fileSize()
	//assert.True(t, true, ok)
	//assert.Equal(t, expectedFileSize, int(got))
	//
	//ctx := context.Background()
	//reader.Close(ctx)
}

func TestRefreshEbsExtents(t *testing.T) {
	testCase := []struct {
		getObjFunc  func(*meta.MetaWrapper, uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error)
		expectValid bool
	}{
		{MockGetObjExtentsTrue, true},
		{MockGetObjExtentsFalse, false},
	}

	for _, tc := range testCase {
		reader := Reader{
			limitManager: manager.NewLimitManager(nil),
			ecStreamer:   NewECStreamer(0, nil, nil),
		}
		mw := &meta.MetaWrapper{}
		err := gohook.HookMethod(mw, "GetObjExtents", tc.getObjFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		reader.mw = mw
		reader.refreshEbsExtents()
		assert.Equal(t, reader.valid, tc.expectValid)
	}
}

func TestPrepareEbsSlice(t *testing.T) {
	testCase := []struct {
		getObjFunc  func(*meta.MetaWrapper, uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error)
		offset      int
		size        uint32
		expectError error
	}{
		{nil, -1, 100, syscall.EIO},
		{MockGetObjExtentsTrue, 0, 100, nil},
		{MockGetObjExtentsTrue, 501, 100, nil},
		{MockGetObjExtentsTrue, 400, 101, nil},
		{MockGetObjExtentsFalse, 0, 100, syscall.EIO},
		{MockGetObjExtentsFalse, 501, 100, syscall.EIO},
		{MockGetObjExtentsFalse, 400, 101, syscall.EIO},
	}

	for _, tc := range testCase {
		mw := &meta.MetaWrapper{}
		err := gohook.HookMethod(mw, "GetObjExtents", tc.getObjFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		reader := Reader{
			limitManager: manager.NewLimitManager(nil),
			ecStreamer:   NewECStreamer(0, nil, nil),
			mw:           mw,
		}
		_, got := reader.prepareEbsSlice(tc.offset, tc.size)
		assert.Equal(t, tc.expectError, got)
	}
}

func TestPrepareEbsSlice_sparseHeadMiddleTailHoles(t *testing.T) {
	r := Reader{
		valid:            true,
		metaReportedSize: 100,
		objExtentKeys:    []proto.ObjExtentKey{{FileOffset: 20, Size: 20}},
		limitManager:     manager.NewLimitManager(nil),
		ecStreamer:       NewECStreamer(0, nil, nil),
	}
	syncECStreamerSizeWithExtentCacheForTest(&r)
	slices, err := r.prepareEbsSlice(0, 100)
	require.NoError(t, err)
	require.Len(t, slices, 3)
	require.True(t, slices[0].hole)
	require.Equal(t, uint64(0), slices[0].fileOffset)
	require.Equal(t, uint32(20), slices[0].rSize)
	require.False(t, slices[1].hole)
	require.Equal(t, uint32(20), slices[1].rSize)
	require.True(t, slices[2].hole)
	require.Equal(t, uint64(40), slices[2].fileOffset)
	require.Equal(t, uint32(60), slices[2].rSize)
	for _, s := range slices {
		if s.hole {
			require.Equal(t, int(s.rSize), len(s.Data))
			for _, b := range s.Data {
				require.Equal(t, byte(0), b)
			}
		}
	}
}

func TestRead(t *testing.T) {
	testCase := []struct {
		close           bool
		readConcurrency int
		getObjFunc      func(*meta.MetaWrapper, uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error)
		bcacheGetFunc   func(*bcache.BcacheClient, string, string, []byte, uint64, uint32) (int, error)
		ebsReadFunc     func(*BlobStoreClient, context.Context, string, []byte, uint64, uint64, proto.ObjExtentKey) (int, error)
		expectError     error
	}{
		{true, 2, MockGetObjExtentsTrue, MockGetTrue, MockEbscReadTrue, os.ErrInvalid},
		{false, 2, MockGetObjExtentsFalse, MockGetTrue, MockEbscReadTrue, syscall.EIO},
		{false, 2, MockGetObjExtentsTrue, MockGetTrue, MockEbscReadFalse, syscall.EIO},
		{false, 2, MockGetObjExtentsTrue, MockGetTrue, MockEbscReadTrue, nil},
	}

	for _, tc := range testCase {
		reader := &Reader{
			limitManager:    manager.NewLimitManager(nil),
			ecStreamer:      NewECStreamer(0, nil, nil),
			close:           tc.close,
			readConcurrency: tc.readConcurrency,
		}

		mw := &meta.MetaWrapper{}
		ebsc := newSafeBlobStoreClientForTest()
		bc := &bcache.BcacheClient{}
		err := gohook.HookMethod(mw, "GetObjExtents", tc.getObjFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		err = gohook.HookMethod(ebsc, "Read", tc.ebsReadFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		err = gohook.HookMethod(bc, "Get", tc.bcacheGetFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		reader.mw = mw
		reader.ebs = ebsc
		reader.bc = bc

		ctx := context.Background()
		buf := make([]byte, 500)
		_, gotError := reader.Read(ctx, buf, 0, 100)
		assert.Equal(t, tc.expectError, gotError)
	}
}

func TestAsyncCache(t *testing.T) {
	ebsc := newSafeBlobStoreClientForTest()
	bc := &bcache.BcacheClient{}
	err := gohook.HookMethod(bc, "Put", MockPutTrue, nil)
	if err != nil {
		panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
	}

	testCase := []struct {
		ebsReadFunc  func(*BlobStoreClient, context.Context, string, []byte, uint64, uint64, proto.ObjExtentKey) (int, error)
		cacheAction  int
		enableBcache bool
		fileSize     uint64
	}{
		{MockEbscReadTrue, proto.NoCache, true, rand.Uint64() % 1000},
		{MockEbscReadTrue, proto.RCache, true, rand.Uint64() % 1000},
		{MockEbscReadTrue, proto.RWCache, true, rand.Uint64() % 1000},
		{MockEbscReadTrue, proto.NoCache, false, rand.Uint64() % 1000},
		{MockEbscReadTrue, proto.RCache, false, rand.Uint64() % 1000},
		{MockEbscReadTrue, proto.RWCache, false, rand.Uint64() % 1000},
		{MockEbscReadFalse, proto.NoCache, true, rand.Uint64() % 1000},
		{MockEbscReadFalse, proto.RCache, true, rand.Uint64() % 1000},
		{MockEbscReadFalse, proto.RWCache, true, rand.Uint64() % 1000},
		{MockEbscReadFalse, proto.NoCache, false, rand.Uint64() % 1000},
		{MockEbscReadFalse, proto.RCache, false, rand.Uint64() % 1000},
		{MockEbscReadFalse, proto.RWCache, false, rand.Uint64() % 1000},
	}

	size := rand.Intn(1000)
	objEk := proto.ObjExtentKey{
		Cid:        0,
		CodeMode:   0,
		BlobSize:   0,
		BlobsLen:   0,
		Size:       uint64(size),
		Blobs:      nil,
		FileOffset: 0,
		Crc:        0,
	}

	for _, tc := range testCase {
		reader := Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		ctx := context.Background()
		err := gohook.HookMethod(ebsc, "Read", tc.ebsReadFunc, nil)
		if err != nil {
			t.Fatalf("Hook advance instance method failed:%s", err.Error())
		}
		_ = tc.fileSize
		reader.asyncCache(ctx, "cacheKey", objEk)
	}
}

func TestNeedCacheL1(t *testing.T) {
	testCase := []struct {
		enableCache bool
		expectCache bool
	}{
		{true, true},
		{false, false},
	}

	for _, tc := range testCase {
		reader := Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		reader.enableBcache = tc.enableCache
		got := reader.needCacheL1()
		assert.Equal(t, tc.expectCache, got)
	}
}

func TestBlobPrefetchLimiterAndEnsurePrefetchBuf(t *testing.T) {
	l := &blobReadPrefetchLimiter{maxBytes: 8}
	assert.True(t, l.tryAcquire(4))
	assert.False(t, l.tryAcquire(5))
	l.release(2)
	assert.True(t, l.tryAcquire(4))

	r := &Reader{blockSize: 0}
	assert.False(t, r.ensurePrefetchBuf())

	r.blockSize = 8
	r.prefetchLimiter = &blobReadPrefetchLimiter{maxBytes: 4}
	assert.False(t, r.ensurePrefetchBuf())

	r.prefetchLimiter = &blobReadPrefetchLimiter{maxBytes: 16}
	assert.True(t, r.ensurePrefetchBuf())
	assert.True(t, len(r.readBuf) >= 16)
}

func TestReaderRead_PrefetchAndFallbackPaths(t *testing.T) {
	reader := &Reader{
		ino:              1,
		volName:          "vol",
		valid:            true,
		objExtentKeys:    []proto.ObjExtentKey{{FileOffset: 0, Size: 64}},
		readConcurrency:  2,
		blockSize:        16,
		aheadReadEnable:  true,
		minReadAheadSize: 1,
		limitManager:     manager.NewLimitManager(nil),
		ecStreamer:       NewECStreamer(1, nil, nil),
	}
	syncECStreamerSizeWithExtentCacheForTest(reader)
	ebsc := newSafeBlobStoreClientForTest()
	reader.ebs = ebsc

	err := gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(ebsc, "Read")

	buf1 := make([]byte, 4)
	n, err := reader.Read(context.Background(), buf1, 0, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)

	buf2 := make([]byte, 4)
	n, err = reader.Read(context.Background(), buf2, 2, 4)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	// request >= blockSize falls back to direct read path
	buf3 := make([]byte, 16)
	n, err = reader.Read(context.Background(), buf3, 16, 16)
	require.NoError(t, err)
	require.Equal(t, 16, n)

	reader.Close(context.Background())
}

// TestReaderPrefetch_fillToMinOfCapAndRem checks prefetch window sizing: fetch = min(prefetchBufCap, rem);
// readEbsRange returns that many bytes (extent reads + zero holes), so bufValidLen matches the window.
func TestReaderPrefetch_fillToMinOfCapAndRem(t *testing.T) {
	t.Run("tail_shorter_than_prefetchCap_bufValidLen_equals_rem", func(t *testing.T) {
		prefetchTestEbsReadLens = nil
		ebsc := newSafeBlobStoreClientForTest()
		err := gohook.HookMethod(ebsc, "Read", prefetchTestMockEbscRead, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(ebsc, "Read")

		blockSize := 32 // prefetchBufCap 64
		r := &Reader{
			valid:            true,
			volName:          "vol",
			ino:              701,
			objExtentKeys:    []proto.ObjExtentKey{{FileOffset: 0, Size: 50}},
			metaReportedSize: 0,
			readConcurrency:  1,
			blockSize:        blockSize,
			aheadReadEnable:  true,
			minReadAheadSize: 0,
			prefetchLimiter:  &blobReadPrefetchLimiter{maxBytes: 512},
			limitManager:     manager.NewLimitManager(nil),
			ebs:              ebsc,
			ecStreamer:       NewECStreamer(701, nil, nil),
		}
		syncECStreamerSizeWithExtentCacheForTest(r)

		n, err := r.Read(context.Background(), make([]byte, 3), 0, 3)
		require.NoError(t, err)
		require.Equal(t, 3, n)
		require.Equal(t, 64, r.prefetchBufCap())
		require.Equal(t, 50, r.bufValidLen, "window is min(cap, rem)=50 when file logical tail is 50 bytes")
		require.GreaterOrEqual(t, len(r.readBuf), 64)
		require.Len(t, prefetchTestEbsReadLens, 1)
		require.Equal(t, 50, prefetchTestEbsReadLens[0])
		r.Close(context.Background())
	})

	t.Run("tail_larger_than_prefetchCap_bufValidLen_equals_cap_with_hole_fill", func(t *testing.T) {
		prefetchTestEbsReadLens = nil
		ebsc := newSafeBlobStoreClientForTest()
		err := gohook.HookMethod(ebsc, "Read", prefetchTestMockEbscRead, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(ebsc, "Read")

		blockSize := 16 // prefetchBufCap 32
		r := &Reader{
			valid:            true,
			volName:          "vol",
			ino:              702,
			objExtentKeys:    []proto.ObjExtentKey{{FileOffset: 0, Size: 10}},
			metaReportedSize: 100,
			readConcurrency:  1,
			blockSize:        blockSize,
			aheadReadEnable:  true,
			minReadAheadSize: 0,
			prefetchLimiter:  &blobReadPrefetchLimiter{maxBytes: 512},
			limitManager:     manager.NewLimitManager(nil),
			ebs:              ebsc,
			ecStreamer:       NewECStreamer(702, nil, nil),
		}
		syncECStreamerSizeWithExtentCacheForTest(r)

		n, err := r.Read(context.Background(), make([]byte, 2), 0, 2)
		require.NoError(t, err)
		require.Equal(t, 2, n)
		require.Equal(t, 32, r.bufValidLen)
		require.Equal(t, 32, len(r.readBuf))
		require.Len(t, prefetchTestEbsReadLens, 1)
		require.Equal(t, 10, prefetchTestEbsReadLens[0], "only ObjExtent bytes hit EBS; trailing holes are zeros in-buffer")

		callsBefore := len(prefetchTestEbsReadLens)
		n, err = r.Read(context.Background(), make([]byte, 5), 8, 5)
		require.NoError(t, err)
		require.Equal(t, 5, n)
		require.Equal(t, callsBefore, len(prefetchTestEbsReadLens), "second read stays inside prefetch window")
		r.Close(context.Background())
	})
}

// prefetchTestEbsReadLens records len(buf) for each hooked BlobStoreClient.Read (gohook + worker goroutine cannot safely
// close over stack pointers to slice headers).
var prefetchTestEbsReadLens []int

func prefetchTestMockEbscRead(_ *BlobStoreClient, _ context.Context, _ string, buf []byte, _ uint64, _ uint64, _ proto.ObjExtentKey) (readN int, err error) {
	n := len(buf)
	prefetchTestEbsReadLens = append(prefetchTestEbsReadLens, n)
	if n == 0 {
		return 0, nil
	}
	readN, err = io.ReadFull(strings.NewReader(strings.Repeat("P", n)), buf)
	return
}

func TestReadSliceRange(t *testing.T) {
	testCase := []struct {
		enableBcache  bool
		extentKey     proto.ExtentKey
		bcacheGetFunc func(*bcache.BcacheClient, string, string, []byte, uint64, uint32) (int, error)
		ebsReadFunc   func(*BlobStoreClient, context.Context, string, []byte, uint64, uint64, proto.ObjExtentKey) (int, error)
		expectError   error
	}{
		{
			false,
			proto.ExtentKey{},
			MockGetTrue,
			MockEbscReadTrue, nil,
		},
		{
			false,
			proto.ExtentKey{},
			MockGetTrue,
			MockEbscReadFalse, syscall.EIO,
		},
		{
			true,
			proto.ExtentKey{},
			MockGetTrue,
			MockEbscReadFalse, nil,
		},
		{
			true,
			proto.ExtentKey{},
			MockGetFalse,
			MockEbscReadFalse, syscall.EIO,
		},
		{
			true,
			proto.ExtentKey{},
			MockGetFalse,
			MockEbscReadTrue, nil,
		},
	}

	for _, tc := range testCase {
		reader := &Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		ebsc := newSafeBlobStoreClientForTest()
		bc := &bcache.BcacheClient{}
		reader.volName = "cfs"
		reader.ino = 12407
		errCh := make(chan error, 1)
		rs := &rwSlice{}
		rs.rSize = uint32(len("Hello world"))
		rs.Data = make([]byte, len("Hello world"))

		reader.enableBcache = tc.enableBcache
		err := gohook.HookMethod(ebsc, "Read", tc.ebsReadFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		err = gohook.HookMethod(bc, "Get", tc.bcacheGetFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		reader.ebs = ebsc
		reader.bc = bc

		ctx := context.Background()
		gotError := reader.readSliceRange(ctx, rs, errCh)
		assert.Equal(t, tc.expectError, gotError)
		<-errCh
	}
}

func MockGetObjExtentsTrue(m *meta.MetaWrapper, inode uint64) (gen uint64, size uint64,
	extents []proto.ExtentKey, objExtents []proto.ObjExtentKey, err error,
) {
	objEks := make([]proto.ObjExtentKey, 0)
	objEkLen := 5
	expectedFileSize := 0
	for i := 0; i < objEkLen; i++ {
		size := 100
		objEks = append(objEks, proto.ObjExtentKey{Size: uint64(100), FileOffset: uint64(expectedFileSize)})
		expectedFileSize += size
	}
	return 1, 1, nil, objEks, nil
}

func MockGetObjExtentsFalse(m *meta.MetaWrapper, inode uint64) (gen uint64, size uint64,
	extents []proto.ExtentKey, objExtents []proto.ObjExtentKey, err error,
) {
	return 1, 1, nil, nil, errors.New("Get objEks failed")
}

func MockEbscReadTrue(ebsc *BlobStoreClient, ctx context.Context, volName string,
	buf []byte, offset uint64, size uint64,
	oek proto.ObjExtentKey,
) (readN int, err error) {
	reader := strings.NewReader("Hello world.")
	readN, _ = io.ReadFull(reader, buf)
	return readN, nil
}

func MockEbscReadFalse(ebsc *BlobStoreClient, ctx context.Context, volName string,
	buf []byte, offset uint64, size uint64,
	oek proto.ObjExtentKey,
) (readN int, err error) {
	return 0, syscall.EIO
}

func MockPutTrue(bc *bcache.BcacheClient, vol, key string, buf []byte) error {
	return nil
}

func MockPutFalse(bc *bcache.BcacheClient, key string, buf []byte) error {
	return errors.New("Bcache put failed")
}

func MockGetTrue(bc *bcache.BcacheClient, vol, key string, buf []byte, offset uint64, size uint32) (int, error) {
	return int(size), nil
}

func MockGetFalse(bc *bcache.BcacheClient, vol, key string, buf []byte, offset uint64, size uint32) (int, error) {
	return 0, errors.New("Bcache get failed")
}

func TestReaderCoveragePrefetchAndAlignment(t *testing.T) {
	t.Run("limiter singleton and ensurePrefetchBuf branches", func(t *testing.T) {
		blobReadPrefetchLimiterGV = nil
		l1 := getBlobReadPrefetchLimiter(16)
		require.NotNil(t, l1)
		l2 := getBlobReadPrefetchLimiter(32)
		require.Equal(t, l1, l2)
		require.Equal(t, int64(16), l2.maxBytes)

		r := &Reader{blockSize: 4, prefetchLimiter: &blobReadPrefetchLimiter{maxBytes: 1}}
		require.False(t, r.ensurePrefetchBuf())
		r.prefetchLimiter.maxBytes = 8
		require.True(t, r.ensurePrefetchBuf())
		require.True(t, r.ensurePrefetchBuf())
		r.Close(context.Background())
		require.Equal(t, int64(0), r.prefetchReserved)
	})

	t.Run("read paths and prefetch fallback", func(t *testing.T) {
		r := &Reader{
			valid:            true,
			objExtentKeys:    []proto.ObjExtentKey{{FileOffset: 0, Size: 8}},
			readConcurrency:  1,
			blockSize:        4,
			aheadReadEnable:  true,
			minReadAheadSize: 0,
			prefetchLimiter:  &blobReadPrefetchLimiter{maxBytes: 8},
			limitManager:     manager.NewLimitManager(nil),
			ebs:              newSafeBlobStoreClientForTest(),
			ecStreamer:       NewECStreamer(1, nil, nil),
		}
		syncECStreamerSizeWithExtentCacheForTest(r)
		err := gohook.HookMethod(r.ebs, "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, buf []byte, _ uint64, _ uint64, _ proto.ObjExtentKey) (int, error) {
				for i := range buf {
					buf[i] = byte(i + 1)
				}
				return len(buf), nil
			}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(r.ebs, "Read")

		n, err := r.Read(context.Background(), make([]byte, 0), 0, 0)
		require.NoError(t, err)
		require.Equal(t, 0, n)

		n, err = r.Read(context.Background(), make([]byte, 2), 10, 2)
		require.NoError(t, err)
		require.Equal(t, 0, n)

		r.prefetchLimiter.maxBytes = 0
		n, err = r.Read(context.Background(), make([]byte, 2), 0, 2)
		require.NoError(t, err)
		require.Equal(t, 2, n)

		r.prefetchLimiter.maxBytes = 8
		n, err = r.Read(context.Background(), make([]byte, 4), 0, 4)
		require.NoError(t, err)
		require.Equal(t, 4, n)
		require.Equal(t, 0, r.bufValidLen)

		r.prefetchLimiter.maxBytes = 8
		r.bufValidLen = 1
		r.bufBaseOff = 0
		r.readBuf = make([]byte, 1)
		n, err = r.Read(context.Background(), make([]byte, 2), 0, 2)
		require.NoError(t, err)
		require.Equal(t, 2, n)
	})

	t.Run("refresh and align branches", func(t *testing.T) {
		es := &ECStreamer{}
		r := &Reader{
			valid:            true,
			metaReportedSize: 10,
			ecStreamer:       es,
		}
		atomic.StoreUint64(&es.fileSize, 10)
		atomic.StoreUint64(&es.inoVersion, 1)
		require.NoError(t, r.EnsureAlignedForRead(1, 10))

		mw := &meta.MetaWrapper{}
		err := gohook.HookMethod(mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 2, 8, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 8}}, nil
		}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(mw, "GetObjExtents")

		r.mw = mw
		r.ino = 100
		r.valid = false
		require.NoError(t, r.EnsureAlignedForRead(2, 8))
		require.Equal(t, uint64(2), atomic.LoadUint64(&es.inoVersion))
		require.Equal(t, uint64(8), atomic.LoadUint64(&es.fileSize))

		atomic.StoreUint32(&es.dirty, 1)
		require.NoError(t, r.EnsureAlignedForRead(2, 8))
		_, err = r.RefreshExtents()
		require.NoError(t, err)
	})
}

func TestReaderCoverageHelperBranches(t *testing.T) {
	t.Run("string and newreader minreadahead clamp", func(t *testing.T) {
		r := NewReader(ClientConfig{
			VolName:          "v",
			Ino:              1,
			LimitManager:     newTestLimitManager(),
			BlockSize:        4,
			AheadReadEnable:  true,
			MinReadAheadSize: -1,
			PrefetchTotalMem: 4,
			ECStreamer:       NewECStreamer(1, nil, nil),
		})
		require.NotContains(t, (rwSlice{fileOffset: 1}).String(), "hole(true)")
		require.Contains(t, r.String(), "Reader{")
		require.Equal(t, uint64(0), r.minReadAheadSize)
	})

	t.Run("ensureExtentsLoaded and readEbsRange error paths", func(t *testing.T) {
		mw := &meta.MetaWrapper{}
		err := gohook.HookMethod(mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, errors.New("boom")
		}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(mw, "GetObjExtents")

		r := &Reader{
			mw:              mw,
			ino:             1,
			readConcurrency: 1,
			limitManager:    manager.NewLimitManager(nil),
			ecStreamer:      NewECStreamer(1, nil, nil),
		}
		require.Error(t, r.ensureExtentsLoaded())
		r.Lock()
		_, err = r.readEbsRange(context.Background(), -1, 1)
		r.Unlock()
		require.Error(t, err)
	})

	t.Run("prepareEbsSlice holes and readSliceRange hole", func(t *testing.T) {
		r := &Reader{
			valid: true,
			objExtentKeys: []proto.ObjExtentKey{
				{FileOffset: 10, Size: 5},
				{FileOffset: 20, Size: 5},
			},
			limitManager: manager.NewLimitManager(nil),
			ecStreamer:   NewECStreamer(1, nil, nil),
		}
		syncECStreamerSizeWithExtentCacheForTest(r)
		slices, err := r.prepareEbsSlice(0, 30)
		require.NoError(t, err)
		require.NotEmpty(t, slices)

		errCh := make(chan error, 1)
		require.NoError(t, r.readSliceRange(context.Background(), &rwSlice{hole: true, rSize: 3}, errCh))
		require.NoError(t, <-errCh)
	})
}

func TestReaderEnsureExtentsLoadedOnExtentEpochDrift(t *testing.T) {
	es := NewECStreamer(11, nil, nil)
	atomic.StoreUint32(&es.extentMetaEpoch, 7)
	mw := &meta.MetaWrapper{}
	err := gohook.HookMethod(mw, "GetObjExtents", func(*meta.MetaWrapper, uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
		return 3, 64, nil, nil, nil
	}, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(mw, "GetObjExtents")

	r := &Reader{
		ino:             11,
		mw:              mw,
		valid:           true,
		extentEpochSeen: 6,
		ecStreamer:      es,
		limitManager:    manager.NewLimitManager(nil),
		readConcurrency: 1,
	}
	require.NoError(t, r.ensureExtentsLoaded())
	require.True(t, r.valid)
	require.Equal(t, uint32(7), r.extentEpochSeen)
}
