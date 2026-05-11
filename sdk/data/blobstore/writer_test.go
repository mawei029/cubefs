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
	"crypto/md5"
	"fmt"
	"io"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/brahma-adshonor/gohook"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/blobstore/api/access"
	proto2 "github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util/buf"
)

var writer *Writer

func newTestLimitManager() *manager.LimitManager {
	return manager.NewLimitManager(nil)
}

func init() {
	// start ebs mock service
	mockServer := NewMockEbsService()
	cfg := access.Config{
		ConnMode: access.QuickConnMode,
		Consul: access.ConsulConfig{
			Address: mockServer.service.URL[7:],
		},
		PriorityAddrs:  []string{mockServer.service.URL},
		MaxSizePutOnce: 1 << 20,
	}

	blobStoreClient, _ := NewEbsClient(cfg)

	config := ClientConfig{
		VolName:         "testVolume",
		VolType:         1,
		BlockSize:       1 << 23,
		Ino:             1000,
		Bc:              nil,
		Mw:              nil,
		LimitManager:    newTestLimitManager(),
		Ebsc:            blobStoreClient,
		EnableBcache:    false,
		WConcurrency:    10,
		ReadConcurrency: 10,
		FileCache:       false,
		FileSize:        0,
		ECStreamer:      NewECStreamer(1000, nil, nil),
	}

	buf.InitCachePool(8388608)
	writer = NewWriter(config)
}

func newNilWriter() (writer *Writer) {
	return nil
}

func TestNotInstanceWriter_Write(t *testing.T) {
	writer := newNilWriter()
	ctx := context.Background()
	data := []byte{1, 2, 3}
	var flag int
	flag |= proto.FlagsAppend
	_, err := writer.Write(ctx, 0, data, flag)
	// expect err is not nil
	if err == nil {
		t.Fatalf("write is called by not instance writer.")
	}
}

// TestWriter_TruncateV2_NilReturnsError 校验 nil Writer 调用 TruncateV2 返回错误（EC truncate 基本分支）。
func TestWriter_TruncateV2_NilReturnsError(t *testing.T) {
	w := newNilWriter()
	ctx := context.Background()
	_, _, err := w.TruncateV2(ctx, 100)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

// TestWriter_TruncateV2FromExtents_NilReturnsError 校验 nil Writer 调用 TruncateV2FromExtents 返回错误。
func TestWriter_TruncateV2FromExtents_NilReturnsError(t *testing.T) {
	w := newNilWriter()
	ctx := context.Background()
	_, _, err := w.TruncateV2FromExtents(ctx, 100, 200, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nil")
}

func TestWriter_doBufferWrite_(t *testing.T) {
	// write data to buffer,not write to ebs when len(buffer)<BlockSize
	ctx := context.Background()
	testCases := []struct {
		offset int
		data   []byte
		n      int
	}{
		{0, make([]byte, 1), 1},
		{1, make([]byte, 10), 10},
		{11, make([]byte, 100), 100},
		{111, make([]byte, 1000), 1000},
		{1111, make([]byte, 10000), 10000},
		{11111, make([]byte, 100000), 100000},
		{111111, make([]byte, 1000000), 1000000},
		{1111111, make([]byte, 5000000), 5000000},
	}
	var flag int
	flag |= proto.FlagsAppend
	var fileSize int
	for _, tc := range testCases {
		n, err := writer.Write(ctx, tc.offset, tc.data, flag)
		if n != tc.n || err != nil {
			t.Fatalf("write fail. write n(%v),expect n(%v) err(%v)", n, tc.n, err)
		}
		fileSize += tc.n
	}
	if writer.CacheFileSize() != fileSize {
		t.Fatalf("write fail. fileSize is correct. fileSize:(%v),expect:(%v)", writer.CacheFileSize(), fileSize)
	}
}

func TestWriter_prepareWriteSlice(t *testing.T) {
	testCases := []struct {
		offset             int
		dataLen            int
		expectSlices       int
		expectSliceDateLen []int
	}{
		{0, 100, 1, []int{100}},
		{0, 1<<23 - 1, 1, []int{1<<23 - 1}},
		{0, 1 << 23, 1, []int{1 << 23}},
		{0, 1<<23 + 1, 2, []int{1 << 23, 1}},
		{0, 1 << 24, 2, []int{1 << 23, 1 << 23}},
	}
	for _, tc := range testCases {
		data := make([]byte, tc.dataLen)
		wSlices := writer.prepareWriteSlice(tc.offset, data)
		actualSlices := len(wSlices)
		actualSliceDateLen := make([]int, 0)
		for _, wSlice := range wSlices {
			actualSliceDateLen = append(actualSliceDateLen, len(wSlice.Data))
		}
		if actualSlices != tc.expectSlices || !reflect.DeepEqual(actualSliceDateLen, tc.expectSliceDateLen) {
			t.Fatalf("prepareWriteSlice fail. actualSlices(%v) expectSlices(%v) "+
				"actualSliceDateLen(%v) expectSliceDateLen(%v)",
				actualSlices, tc.expectSlices, actualSliceDateLen, tc.expectSliceDateLen)
		}
	}
}

func TestPrepareWriteSlice(t *testing.T) {
	testCase := []struct {
		data             []byte
		blockSize        int
		expectSliceCount int
	}{
		{[]byte("hello world"), 10, 2},
		{[]byte("hello world"), 100, 1},
		{[]byte("0123456789012345678901234567890123456789"), 5, 8},
		{[]byte("0123456789012345678901234567890123456789"), 15, 3},
	}
	for _, tc := range testCase {
		writer.blockSize = tc.blockSize
		sliceGot := writer.prepareWriteSlice(0, tc.data)
		assert.Equal(t, tc.expectSliceCount, len(sliceGot))
	}
}

func TestCacheFileSize(t *testing.T) {
	testCase := []struct {
		fileSize   uint64
		expectSize uint64
	}{
		{1, 1},
		{10, 10},
		{100, 100},
		{1000, 1000},
	}
	for _, tc := range testCase {
		writer.fileSize = tc.fileSize
		gotSize := uint64(writer.CacheFileSize())
		assert.Equal(t, tc.expectSize, gotSize)
	}
}

func TestParallelWrite(t *testing.T) {
	ctx := context.Background()
	data := []byte("Hello world")
	offset := 0

	// 全局 writer 可能被同包其它用例留下 dirty；doParallelWrite 会先 flushExt，需干净状态或完整 mock Mw。
	writer.Lock()
	writer.dirty = false
	writer.blockPosition = 0
	writer.Unlock()

	mw := &meta.MetaWrapper{}
	err := gohook.HookMethod(mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	if err != nil {
		panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
	}
	writer.mw = mw

	writer.doParallelWrite(ctx, data, offset)
}

func TestNewWriter(t *testing.T) {
	config := ClientConfig{
		VolName:         "cfs",
		VolType:         1,
		BlockSize:       1 << 23,
		Ino:             1000,
		Bc:              nil,
		Mw:              nil,
		LimitManager:    newTestLimitManager(),
		Ebsc:            nil,
		EnableBcache:    false,
		WConcurrency:    10,
		ReadConcurrency: 10,
		FileCache:       false,
		FileSize:        0,
		ECStreamer:      NewECStreamer(1000, nil, nil),
	}
	w := NewWriter(config)
	_ = w.String()
}

func TestBufferWrite(t *testing.T) {
	ctx := context.Background()
	data := []byte("Hello world")
	offset := 0

	mw := &meta.MetaWrapper{}
	err := gohook.HookMethod(mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	if err != nil {
		panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
	}
	writer.mw = mw
	writer.blockSize = 8388608
	writer.buf = buf.CachePool.Get()

	writer.doBufferWrite(ctx, data, offset)
}

func TestWriteSlice(t *testing.T) {
	testCase := []struct {
		wg           bool
		ebsWriteFunc func(*BlobStoreClient, context.Context, string, []byte, uint32) (proto2.Location, error)
		expectError  error
	}{
		{false, MockEbscWriteTrue, nil},
		{false, MockEbscWriteFalse, syscall.EIO},
		{true, MockEbscWriteTrue, nil},
		{true, MockEbscWriteFalse, syscall.EIO},
	}

	for _, tc := range testCase {
		ebsc := &BlobStoreClient{}
		err := gohook.HookMethod(ebsc, "Write", tc.ebsWriteFunc, nil)
		if err != nil {
			panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
		}
		writer.ebsc = ebsc
		wSlice := &rwSlice{
			index:        0,
			fileOffset:   0,
			size:         100,
			rOffset:      0,
			rSize:        100,
			read:         0,
			Data:         make([]byte, 100),
			objExtentKey: proto.ObjExtentKey{},
		}

		ctx := context.Background()
		writer.err = make(chan *wSliceErr)
		writer.wg.Add(1)
		if tc.wg {
			go func() {
				<-writer.err
			}()
		}
		gotError := writer.writeSlice(ctx, wSlice, tc.wg)
		assert.Equal(t, tc.expectError, gotError)
	}
}

func MockEbscWriteTrue(ebs *BlobStoreClient, ctx context.Context, volName string, data []byte, l uint32) (location proto2.Location, err error) {
	loc := proto2.Location{
		ClusterID: 1,
		CodeMode:  0,
		Size_:     100,
		SliceSize: 100,
		Crc:       1024,
		Slices: []proto2.Slice{
			{
				MinSliceID: 1,
				Vid:        1,
				Count:      1,
			},
		},
	}
	return loc, nil
}

// ============== Mock functions =============
func MockEbscWriteFalse(ebs *BlobStoreClient, ctx context.Context, volName string, data []byte, l uint32) (location proto2.Location, err error) {
	return proto2.Location{}, syscall.EIO
}

func MockAppendObjExtentKeysTrue(mw *meta.MetaWrapper, inode uint64, eks []proto.ObjExtentKey) error {
	return nil
}

func MockAppendObjExtentKeysFalse(mw *meta.MetaWrapper, inode uint64, eks []proto.ObjExtentKey) error {
	return syscall.EIO
}

// MockGetObjExtentsEmpty returns empty extents (no existing data)
func MockGetObjExtentsEmpty(mw *meta.MetaWrapper, inode uint64) (gen uint64, sz uint64, exts []proto.ExtentKey, objExts []proto.ObjExtentKey, err error) {
	return 1, 0, nil, []proto.ObjExtentKey{}, nil
}

// MockAppendObjExtentKeysWithCheckTrue mocks successful AppendObjExtentKeysWithCheck
func MockAppendObjExtentKeysWithCheckTrue(mw *meta.MetaWrapper, inode uint64, newEk []proto.ObjExtentKey, discardEk []proto.ObjExtentKey) error {
	return nil
}

// TestComputeOverwriteReqs tests the computeOverwriteReqs function with basic scenarios
func TestComputeOverwriteReqs(t *testing.T) {
	// Create a simple writer for testing
	// testWriter := &Writer{}

	testCases := []struct {
		name       string
		start      uint64
		end        uint64
		objExtents []proto.ObjExtentKey
		result     []overwriteReq
	}{
		{
			name:       "empty extents - new data only",
			start:      0,
			end:        100,
			objExtents: []proto.ObjExtentKey{},
			result: []overwriteReq{
				{NewExtent: proto.ObjExtentKey{FileOffset: 0, Size: 100}, DiscardExtent: proto.ObjExtentKey{}},
			},
		},
		{
			name:  "partial overlap - buffer overlaps one extent",
			start: 100,
			end:   200,
			objExtents: []proto.ObjExtentKey{
				{FileOffset: 50, Size: 100}, // [50, 150)
			},
			result: []overwriteReq{
				{NewExtent: proto.ObjExtentKey{FileOffset: 100, Size: 50}, DiscardExtent: proto.ObjExtentKey{FileOffset: 50, Size: 100}},
				{NewExtent: proto.ObjExtentKey{FileOffset: 150, Size: 50}, DiscardExtent: proto.ObjExtentKey{}},
			},
		},
		{
			name:  "complete overlap - buffer fully within extent",
			start: 100,
			end:   150,
			objExtents: []proto.ObjExtentKey{
				{FileOffset: 50, Size: 200}, // [50, 250)
			},
			result: []overwriteReq{
				{NewExtent: proto.ObjExtentKey{FileOffset: 100, Size: 50}, DiscardExtent: proto.ObjExtentKey{FileOffset: 50, Size: 200}},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reqs := computeOverwriteReqs(tc.start, tc.end, tc.objExtents)
			require.NotEqual(t, 0, len(reqs), "computeOverwriteReqs fail. got 0 reqs, expect at least 1")
			require.Equal(t, len(tc.result), len(reqs))

			// Verify that all requests cover the buffer range
			totalSize := uint64(0)
			for _, req := range reqs {
				totalSize += req.NewExtent.Size
			}
			require.Equal(t, tc.end-tc.start, totalSize, "computeOverwriteReqs fail. size not match")
			require.Equal(t, tc.result, reqs)
		})
	}
}

// TestTryOverWrite_Basic tests tryOverWrite with basic scenario (small data, no flush needed)
func TestTryOverWrite_Basic(t *testing.T) {
	ctx := context.Background()

	// Create a new writer for testing
	config := ClientConfig{
		VolName:         "testVolume",
		VolType:         1,
		BlockSize:       1024, // Small block size for testing
		Ino:             1000,
		Bc:              nil,
		Mw:              nil,
		LimitManager:    newTestLimitManager(),
		Ebsc:            nil,
		EnableBcache:    false,
		WConcurrency:    10,
		ReadConcurrency: 10,
		FileCache:       false,
		FileSize:        0,
		ECStreamer:      NewECStreamer(1000, nil, nil),
	}

	testWriter := NewWriter(config)
	testWriter.buf = make([]byte, 1024)
	testWriter.fileOffset = 0
	testWriter.blockPosition = 0

	// Mock MetaWrapper for flushExt
	mw := &meta.MetaWrapper{}
	err := gohook.HookMethod(mw, "GetObjExtents", MockGetObjExtentsEmpty, nil)
	require.NoError(t, err, "Hook GetObjExtents failed")

	err = gohook.HookMethod(mw, "AppendObjExtentKeysWithCheck", MockAppendObjExtentKeysWithCheckTrue, nil)
	require.NoError(t, err, "Hook AppendObjExtentKeysWithCheckTrue failed")
	testWriter.mw = mw

	// Mock BlobStoreClient for writeSlice
	ebsc := &BlobStoreClient{}
	err = gohook.HookMethod(ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err, "Hook BlobStoreClient Write failed")
	testWriter.ebsc = ebsc

	// Test: write small data (less than blockSize)
	data := make([]byte, 100)
	for i := range data {
		data[i] = byte(i % 256)
	}

	var flag int
	flag |= proto.FlagsAppend

	size, err := testWriter.tryOverWrite(ctx, 0, data, flag)
	require.NoError(t, err, "tryOverWrite failed")
	require.Equal(t, len(data), size, "tryOverWrite returned wrong size.")
	require.Equal(t, 0, testWriter.blockPosition, "tryOverWrite should flush trailing partial block and reset blockPosition.")
	require.Equal(t, len(data), testWriter.fileOffset, "tryOverWrite fileOffset incorrect.")
}

// TestFlushExt_Basic tests flushExt with basic scenario (no existing extents)
func TestFlushExt_Basic(t *testing.T) {
	ctx := context.Background()

	// Create a new writer for testing
	config := ClientConfig{
		VolName:         "testVolume",
		VolType:         1,
		BlockSize:       1024,
		Ino:             1000,
		Bc:              nil,
		Mw:              nil,
		LimitManager:    newTestLimitManager(),
		Ebsc:            nil,
		EnableBcache:    false,
		WConcurrency:    10,
		ReadConcurrency: 10,
		FileCache:       false,
		FileSize:        0,
		ECStreamer:      NewECStreamer(1000, nil, nil),
	}

	testWriter := NewWriter(config)
	testWriter.buf = make([]byte, 1024)
	testWriter.fileOffset = 100
	testWriter.blockPosition = 100
	testWriter.dirty = true

	// Fill buffer with test data
	for i := 0; i < 100; i++ {
		testWriter.buf[i] = byte(i % 256)
	}

	// Mock MetaWrapper - return empty extents (no overlap)
	mw := &meta.MetaWrapper{}
	err := gohook.HookMethod(mw, "GetObjExtents", MockGetObjExtentsEmpty, nil)
	require.NoError(t, err, "Hook GetObjExtents failed")

	err = gohook.HookMethod(mw, "AppendObjExtentKeysWithCheck", MockAppendObjExtentKeysWithCheckTrue, nil)
	require.NoError(t, err, "Hook AppendObjExtentKeysWithCheckTrue failed")
	testWriter.mw = mw

	// Mock BlobStoreClient
	ebsc := &BlobStoreClient{}
	err = gohook.HookMethod(ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err, "Hook BlobStoreClient Write failed")
	testWriter.ebsc = ebsc

	// Test flushExt
	err = testWriter.flushExt(testWriter.ino, ctx, false)
	require.NoError(t, err, "flushExt failed")

	// Verify buffer is reset
	require.Equal(t, 0, testWriter.blockPosition, "blockPosition should be reset to 0 after flush.")
	require.False(t, testWriter.dirty, "dirty should be false after flush")
}

func TestWriterWrite_NewGuardBranches(t *testing.T) {
	t.Run("too large returns EOPNOTSUPP", func(t *testing.T) {
		w := &Writer{}
		n, err := w.Write(context.Background(), 0, make([]byte, MaxBufferSize+1), 0)
		require.Equal(t, 0, n)
		require.ErrorIs(t, err, syscall.EOPNOTSUPP)
	})

	t.Run("append offset mismatch returns EOPNOTSUPP", func(t *testing.T) {
		w := &Writer{fileSize: 10}
		n, err := w.Write(context.Background(), 0, []byte("x"), proto.FlagsAppend)
		require.Equal(t, 0, n)
		require.ErrorIs(t, err, syscall.EOPNOTSUPP)
	})
}

func TestWriterSetFileSizeAndTruncateV2GrowNoShrink(t *testing.T) {
	w := &Writer{ecStreamer: NewECStreamer(1, nil, nil)}
	w.SetFileSize(123)
	require.Equal(t, 123, w.CacheFileSize())

	mw := &meta.MetaWrapper{}
	err := gohook.HookMethod(mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
		return 1, 20, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 20}}, nil
	}, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(mw, "GetObjExtents")

	w.mw = mw
	w.ebsc = &BlobStoreClient{}
	w.ino = 1
	w.volName = "vol"

	newExts, toDel, err := w.TruncateV2(context.Background(), 25)
	require.NoError(t, err)
	require.Len(t, newExts, 1)
	require.Len(t, toDel, 0)
}

func TestWriterCoverageAdditionalBranches(t *testing.T) {
	t.Run("flush overwrite selector", func(t *testing.T) {
		w := &Writer{overwrite: true}
		require.NoError(t, w.Flush(1, context.Background()))
	})

	t.Run("flushWithoutPool inconsistent state", func(t *testing.T) {
		w := &Writer{
			ino:        1,
			fileOffset: 1,
			buf:        []byte{1, 2, 3},
			dirty:      true,
		}
		err := w.flushWithoutPool(1, context.Background(), false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "inconsistent state")
	})

	t.Run("computeOverwriteReqs covers continue and hole", func(t *testing.T) {
		reqs := computeOverwriteReqs(50, 120, []proto.ObjExtentKey{
			{FileOffset: 0, Size: 20},
			{FileOffset: 80, Size: 20},
		})
		require.NotEmpty(t, reqs)
		var hasHole bool
		for _, r := range reqs {
			if r.DiscardExtent.Size == 0 {
				hasHole = true
			}
		}
		require.True(t, hasHole)
	})

	t.Run("truncateV2 getObjExtents error", func(t *testing.T) {
		mw := &meta.MetaWrapper{}
		err := gohook.HookMethod(mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 0, 0, nil, nil, io.ErrUnexpectedEOF
		}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(mw, "GetObjExtents")

		w := &Writer{mw: mw, ebsc: &BlobStoreClient{}, ino: 1, volName: "v"}
		_, _, err = w.TruncateV2(context.Background(), 1)
		require.Error(t, err)
	})
}

func TestWriterCoverageMoreLowFunctions(t *testing.T) {
	t.Run("writeWithoutPool guards and success", func(t *testing.T) {
		var nilWriter *Writer
		_, err := nilWriter.WriteWithoutPool(context.Background(), 0, []byte("a"))
		require.Error(t, err)

		w := &Writer{
			blockSize:    8,
			buf:          make([]byte, 0, 8),
			ino:          1,
			volName:      "v",
			limitManager: manager.NewLimitManager(nil),
			ebsc:         &BlobStoreClient{},
			mw:           &meta.MetaWrapper{},
			ecStreamer:   NewECStreamer(1, nil, nil),
		}
		_, err = w.WriteWithoutPool(context.Background(), 1, []byte("a"))
		require.ErrorIs(t, err, syscall.EOPNOTSUPP)

		// success path
		err = gohook.HookMethod(w.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ebsc, "Write")
		err = gohook.HookMethod(w.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.mw, "AppendObjExtentKeys")

		n, err := w.WriteWithoutPool(context.Background(), 0, []byte("abc"))
		require.NoError(t, err)
		require.Equal(t, 3, n)
	})

	t.Run("writeFromReader and flushWithoutPool and freecache", func(t *testing.T) {
		buf.InitCachePool(8)
		w := NewWriter(ClientConfig{
			VolName:      "v",
			BlockSize:    8,
			Ino:          2,
			LimitManager: newTestLimitManager(),
			Ebsc:         &BlobStoreClient{},
			Mw:           &meta.MetaWrapper{},
			WConcurrency: 1,
			ECStreamer:   NewECStreamer(2, nil, nil),
		})
		w.buf = make([]byte, 0, 8)

		err := gohook.HookMethod(w.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ebsc, "Write")
		err = gohook.HookMethod(w.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.mw, "AppendObjExtentKeys")

		h := md5.New()
		size, err := w.WriteFromReader(context.Background(), strings.NewReader("abcdefghi"), h)
		require.NoError(t, err)
		require.Equal(t, uint64(9), size)

		// FlushWithoutPool wrapper branch
		w.dirty = true
		w.fileOffset = len(w.buf)
		require.NoError(t, w.FlushWithoutPool(w.ino, context.Background()))

		// FreeCache/allocateCache branches
		w.allocateCache()
		require.NotNil(t, w.buf)
		w.FreeCache()
		w.FreeCache()
	})

	t.Run("flush function direct path", func(t *testing.T) {
		w := &Writer{
			ino:           3,
			volName:       "v",
			blockSize:     4,
			blockPosition: 4,
			fileOffset:    4,
			dirty:         true,
			buf:           make([]byte, 4),
			limitManager:  manager.NewLimitManager(nil),
			ebsc:          &BlobStoreClient{},
			mw:            &meta.MetaWrapper{},
			ecStreamer:    NewECStreamer(3, nil, nil),
		}
		copy(w.buf, []byte("data"))
		err := gohook.HookMethod(w.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ebsc, "Write")
		err = gohook.HookMethod(w.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.mw, "AppendObjExtentKeys")

		require.NoError(t, w.flush(w.ino, context.Background(), true))
		require.Equal(t, 0, w.blockPosition)
	})

	t.Run("flushWithoutPool then tryOverWrite no panic", func(t *testing.T) {
		blockSize := 8
		w := &Writer{
			ino:           5,
			volName:       "v",
			blockSize:     blockSize,
			blockPosition: 59287 % blockSize, // stale index after hypothetical copy-path use
			fileOffset:    100,
			dirty:         true,
			buf:           make([]byte, blockSize),
			overwrite:     true,
			limitManager:  newTestLimitManager(),
			ebsc:          &BlobStoreClient{},
			mw:            &meta.MetaWrapper{},
			ecStreamer:    NewECStreamer(5, nil, nil),
		}
		err := gohook.HookMethod(w.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ebsc, "Write")
		err = gohook.HookMethod(w.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.mw, "AppendObjExtentKeys")

		require.NoError(t, w.flushWithoutPool(w.ino, context.Background(), false))
		require.Equal(t, 0, len(w.buf))
		require.Equal(t, 0, w.blockPosition)

		err = gohook.HookMethod(w.mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 100, nil, nil, nil
		}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.mw, "GetObjExtents")
		err = gohook.HookMethod(w.mw, "AppendObjExtentKeysWithCheck", func(_ *meta.MetaWrapper, _ uint64, _, _ []proto.ObjExtentKey) error {
			return nil
		}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.mw, "AppendObjExtentKeysWithCheck")

		n, err := w.tryOverWrite(context.Background(), 0, []byte("ab"), 0)
		require.NoError(t, err)
		require.Equal(t, 2, n)
		require.Equal(t, blockSize, len(w.buf))
	})
}
