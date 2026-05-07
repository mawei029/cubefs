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
	"github.com/cubefs/cubefs/sdk/data/stream"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/errors"
)

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
		Ec:              nil,
		Ebsc:            nil,
		EnableBcache:    false,
		WConcurrency:    0,
		ReadConcurrency: 0,
		FileCache:       false,
		FileSize:        0,
	}
	ec := &stream.ExtentClient{}
	err := gohook.HookMethod(ec, "Write", MockWriteTrue, nil)
	if err != nil {
		panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
	}
	mockConfig.Ec = ec

	reader := NewReader(mockConfig)
	assert.NotEmpty(t, reader, nil)
}

func TestFileSize(t *testing.T) {
	testCase := []struct {
		valid      bool
		objEks     []proto.ObjExtentKey
		expectSize uint64
		expectOk   bool
	}{
		{false, nil, 0, false},
		{true, nil, 0, true},
		{true, []proto.ObjExtentKey{{Size: uint64(100), FileOffset: uint64(100)}}, 200, true},
	}

	for _, tc := range testCase {
		reader := Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		reader.valid = tc.valid
		reader.objExtentKeys = tc.objEks
		gotSize, gotOk := reader.fileSize()
		assert.Equal(t, tc.expectSize, gotSize)
		assert.Equal(t, tc.expectOk, gotOk)
	}

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
		reader := Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
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
		{MockGetObjExtentsTrue, 501, 100, io.EOF},
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
		reader := Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		reader.mw = mw
		_, got := reader.prepareEbsSlice(tc.offset, tc.size)
		assert.Equal(t, tc.expectError, got)
	}
}

func TestRead(t *testing.T) {
	testCase := []struct {
		close            bool
		readConcurrency  int
		getObjFunc       func(*meta.MetaWrapper, uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error)
		bcacheGetFunc    func(*bcache.BcacheClient, string, string, []byte, uint64, uint32) (int, error)
		checkDpExistFunc func(*stream.ExtentClient, uint64) error
		// readExtentFunc   func(*stream.ExtentClient, uint64, *proto.ExtentKey, []byte, int, int, uint32) (int, error, bool)
		ebsReadFunc func(*BlobStoreClient, context.Context, string, []byte, uint64, uint64, proto.ObjExtentKey) (int, error)
		expectError error
	}{
		{true, 2, MockGetObjExtentsTrue, MockGetTrue, MockCheckDataPartitionExistTrue, MockEbscReadTrue, os.ErrInvalid},
		{false, 2, MockGetObjExtentsFalse, MockGetTrue, MockCheckDataPartitionExistTrue, MockEbscReadTrue, syscall.EIO},
		{false, 2, MockGetObjExtentsTrue, MockGetTrue, MockCheckDataPartitionExistTrue, MockEbscReadFalse, syscall.EIO},
		{false, 2, MockGetObjExtentsTrue, MockGetTrue, MockCheckDataPartitionExistTrue, MockEbscReadTrue, nil},
	}

	for _, tc := range testCase {
		reader := &Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		reader.close = tc.close
		reader.readConcurrency = tc.readConcurrency

		mw := &meta.MetaWrapper{}
		ebsc := newSafeBlobStoreClientForTest()
		bc := &bcache.BcacheClient{}
		ec := &stream.ExtentClient{}
		err := gohook.HookMethod(mw, "GetObjExtents", tc.getObjFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		err = gohook.HookMethod(ec, "CheckDataPartitionExsit", tc.checkDpExistFunc, nil)
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
	ec := &stream.ExtentClient{}
	bc := &bcache.BcacheClient{}
	err := gohook.HookMethod(ec, "Write", MockWriteTrue, nil)
	if err != nil {
		panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
	}
	err = gohook.HookMethod(bc, "Put", MockPutTrue, nil)
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
		reader.fileLength = tc.fileSize
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
	assert.True(t, len(r.readBuf) >= 8)
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
	}
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

func TestReadSliceRange(t *testing.T) {
	testCase := []struct {
		enableBcache     bool
		extentKey        proto.ExtentKey
		bcacheGetFunc    func(*bcache.BcacheClient, string, string, []byte, uint64, uint32) (int, error)
		checkDpExistFunc func(*stream.ExtentClient, uint64) error
		// readExtentFunc   func(*stream.ExtentClient, uint64, *proto.ExtentKey, []byte, int, int, uint32) (int, error, bool)
		ebsReadFunc func(*BlobStoreClient, context.Context, string, []byte, uint64, uint64, proto.ObjExtentKey) (int, error)
		expectError error
	}{
		{
			false,
			proto.ExtentKey{},
			MockGetTrue, MockCheckDataPartitionExistTrue,
			MockEbscReadTrue, nil,
		},
		{
			false,
			proto.ExtentKey{},
			MockGetTrue, MockCheckDataPartitionExistTrue,
			MockEbscReadFalse, syscall.EIO,
		},
		{
			true,
			proto.ExtentKey{},
			MockGetTrue, MockCheckDataPartitionExistTrue,
			MockEbscReadFalse, nil,
		},
		{
			true,
			proto.ExtentKey{},
			MockGetFalse, MockCheckDataPartitionExistTrue,
			MockEbscReadFalse, syscall.EIO,
		},
		{
			true,
			proto.ExtentKey{},
			MockGetFalse, MockCheckDataPartitionExistTrue,
			MockEbscReadTrue, nil,
		},
	}

	for _, tc := range testCase {
		reader := &Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		ebsc := newSafeBlobStoreClientForTest()
		bc := &bcache.BcacheClient{}
		ec := &stream.ExtentClient{}
		reader.volName = "cfs"
		reader.ino = 12407
		reader.fileLength = 10
		reader.ec = ec
		reader.err = make(chan error)
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
		err = gohook.HookMethod(ec, "CheckDataPartitionExsit", tc.checkDpExistFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		reader.ebs = ebsc
		reader.bc = bc

		ctx := context.Background()
		reader.wg.Add(1)
		go func() {
			<-reader.err
		}()
		gotError := reader.readSliceRange(ctx, rs)
		assert.Equal(t, tc.expectError, gotError)
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

func MockReadExtentTrue(client *stream.ExtentClient, inode uint64, ek *proto.ExtentKey,
	data []byte, offset int, size int, poolId uint8, storageClass uint32,
) (read int, err error, b bool) {
	return len("Hello world"), nil, true
}

func MockReadExtentFalse(client *stream.ExtentClient, inode uint64, ek *proto.ExtentKey,
	data []byte, offset int, size int, poolId uint8, storageClass uint32,
) (read int, err error) {
	return 0, errors.New("Read extent failed")
}

func MockCheckDataPartitionExistTrue(client *stream.ExtentClient, partitionID uint64) error {
	return nil
}

func MockCheckDataPartitionExistFalse(client *stream.ExtentClient, partitionID uint64) error {
	return errors.New("CheckDataPartitionExist failed")
}

func MockWriteTrue(client *stream.ExtentClient, inode uint64, offset int, data []byte,
	flags int, checkFunc func() error, poolId uint8, storageClass uint32, isMigration, waitForFlush bool,
) (write int, err error) {
	return len(data), nil
}

func MockWriteFalse(client *stream.ExtentClient, inode uint64, offset int, data []byte,
	flags int,
) (write int, err error) {
	return 0, errors.New("Write failed")
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
		}
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

		_, err = r.Read(context.Background(), make([]byte, 2), 10, 2)
		require.ErrorIs(t, err, io.EOF)

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
		r := &Reader{
			valid:            true,
			metaReportedSize: 10,
			inodeViewGen:     1,
			inodeViewSize:    10,
		}
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
		require.Equal(t, uint64(2), r.inodeViewGen)
		require.Equal(t, uint64(8), r.inodeViewSize)
		require.NoError(t, r.RefreshExtents())
	})
}

func TestReaderCoverageHelperBranches(t *testing.T) {
	t.Run("string and newreader minreadahead clamp", func(t *testing.T) {
		ec := &stream.ExtentClient{}
		ec.LimitManager = manager.NewLimitManager(nil)
		r := NewReader(ClientConfig{
			VolName:          "v",
			Ino:              1,
			Ec:               ec,
			BlockSize:        4,
			AheadReadEnable:  true,
			MinReadAheadSize: -1,
			PrefetchTotalMem: 4,
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

		r := &Reader{mw: mw, ino: 1, readConcurrency: 1, limitManager: manager.NewLimitManager(nil)}
		require.Error(t, r.ensureExtentsLoaded())
		_, err = r.readEbsRange(context.Background(), -1, 1)
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
		}
		slices, err := r.prepareEbsSlice(0, 30)
		require.NoError(t, err)
		require.NotEmpty(t, slices)

		r.err = make(chan error, 1)
		r.wg.Add(1)
		require.NoError(t, r.readSliceRange(context.Background(), &rwSlice{hole: true, rSize: 3}))
		r.wg.Wait()
		require.NoError(t, <-r.err)
	})
}
