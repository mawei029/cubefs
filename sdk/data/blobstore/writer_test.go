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
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
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

	streamer, _ := NewECStreamer(ECStreamOpenArgs{
		VolName:   "testVolume",
		Ino:       1000,
		BlockSize: 1 << 23,
		Mw:        &meta.MetaWrapper{},
		Ebsc:      blobStoreClient,
	}, nil, nil)

	config := ClientConfig{
		VolName:         "testVolume",
		VolType:         1,
		BlockSize:       1 << 23,
		Ino:             1000,
		LimitManager:    newTestLimitManager(),
		EnableBcache:    false,
		WConcurrency:    10,
		ReadConcurrency: 10,
		FileCache:       false,
		FileSize:        0,
		ECStreamer:      streamer,
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

func TestNewWriter_panicsWithoutECStreamer(t *testing.T) {
	defer func() {
		require.NotNil(t, recover())
	}()
	_ = NewWriter(ClientConfig{VolName: "v", Ino: 1})
}

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

func TestWriter_TruncateV2FromExtentsNilEbsc(t *testing.T) {
	s := mustTestECStreamer(1, nil, nil)
	s.mw = &meta.MetaWrapper{}
	s.ebsc = nil
	_, _, err := s.fWriter.TruncateV2FromExtents(context.Background(), 10, 100, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ebsc nil")
}

func TestWriter_TruncateV2FromExtents_no_shrink_when_target_ge_current(t *testing.T) {
	s := mustTestECStreamerWithEbsc(265, &BlobStoreClient{}, 8<<20)
	eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 100}}
	newEk, del, err := s.fWriter.TruncateV2FromExtents(context.Background(), 100, 100, eks)
	require.NoError(t, err)
	require.True(t, newEk.IsEmpty())
	require.True(t, del.IsEmpty())

	newEk, del, err = s.fWriter.TruncateV2FromExtents(context.Background(), 200, 100, eks)
	require.NoError(t, err)
	require.True(t, newEk.IsEmpty())
	require.True(t, del.IsEmpty())
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
		args := ECStreamOpenArgs{Ino: 99, BlockSize: tc.blockSize, Mw: &meta.MetaWrapper{}}
		st, _ := NewECStreamer(args, nil, nil)
		sliceGot := st.fWriter.prepareWriteSlice(0, tc.data)
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
		s := mustTestECStreamer(1001, nil, nil)
		SeedLogicalViewForTest(s, tc.fileSize, 0)
		gotSize := uint64(s.fWriter.CacheFileSize())
		assert.Equal(t, tc.expectSize, gotSize)
	}
}

func TestParallelWrite(t *testing.T) {
	ctx := context.Background()
	data := []byte("Hello world")
	offset := 0

	writer.ecStreamer.cleanDirty()
	writer.blockPosition = 0
	mw := writer.ecStreamer.mw
	if mw == nil {
		mw = &meta.MetaWrapper{}
		writer.ecStreamer.mw = mw
	}
	err := gohook.HookMethod(mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	if err != nil {
		panic(fmt.Sprintf("Hook advance instance method failed:%s", err.Error()))
	}

	_, _ = writer.doParallelWrite(ctx, data, offset)
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
		ECStreamer:      mustTestECStreamer(1000, nil, nil),
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
	writer.ecStreamer.mw = mw
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
		writer.ecStreamer.ebsc = ebsc
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
func MockAppendObjExtentKeysWithCheckTrue(mw *meta.MetaWrapper, inode uint64, newEk, discardEk proto.ObjExtentKey) error {
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
		ECStreamer:      mustTestECStreamer(1000, nil, nil),
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
	testWriter.ecStreamer.mw = mw

	// Mock BlobStoreClient for writeSlice
	ebsc := &BlobStoreClient{}
	err = gohook.HookMethod(ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err, "Hook BlobStoreClient Write failed")
	testWriter.ecStreamer.ebsc = ebsc

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

	st, testWriter := testWriterWithMwEbsc(1000, &BlobStoreClient{})
	_ = st
	testWriter.buf = make([]byte, 1024)
	testWriter.fileOffset = 100
	testWriter.blockPosition = 100
	seedDirtyForTest(testWriter.ecStreamer)

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
	testWriter.ecStreamer.mw = mw

	// Mock BlobStoreClient
	ebsc := &BlobStoreClient{}
	err = gohook.HookMethod(ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err, "Hook BlobStoreClient Write failed")
	testWriter.ecStreamer.ebsc = ebsc

	// Test flushExt
	err = testWriter.flushExt(testWriter.ecStreamer.ino, ctx, false)
	require.NoError(t, err, "flushExt failed")

	// Verify buffer is reset
	require.Equal(t, 0, testWriter.blockPosition, "blockPosition should be reset to 0 after flush.")
	require.False(t, testWriter.ecStreamer.isDirty(), "dirty should be false after flush")
}

func TestWriterWrite_NewGuardBranches(t *testing.T) {
	t.Run("too large returns EOPNOTSUPP", func(t *testing.T) {
		w := mustTestECStreamer(900, nil, nil).fWriter
		n, err := w.Write(context.Background(), 0, make([]byte, MaxBufferSize+1), 0)
		require.Equal(t, 0, n)
		require.ErrorIs(t, err, syscall.EOPNOTSUPP)
	})

	t.Run("append offset mismatch returns EOPNOTSUPP", func(t *testing.T) {
		s := mustTestECStreamer(1, nil, nil)
		SeedLogicalViewForTest(s, 10, 0)
		w := s.fWriter
		n, err := w.Write(context.Background(), 0, []byte("x"), proto.FlagsAppend)
		require.Equal(t, 0, n)
		require.ErrorIs(t, err, syscall.EOPNOTSUPP)
	})
}

func TestWriterSetFileSizeAndTruncateV2GrowNoShrink(t *testing.T) {
	s := mustTestECStreamer(1, nil, nil)
	SeedLogicalViewForTest(s, 123, 0)
	w := s.fWriter
	require.Equal(t, 123, w.CacheFileSize())

	mw := &meta.MetaWrapper{}
	err := gohook.HookMethod(mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
		return 1, 20, nil, []proto.ObjExtentKey{{FileOffset: 0, Size: 20}}, nil
	}, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(mw, "GetObjExtents")

	s.mw = mw
	s.ebsc = &BlobStoreClient{}
	seedStreamerExtentsForTest(s, 20, []proto.ObjExtentKey{{FileOffset: 0, Size: 20}})

	newExt, toDel, err := w.TruncateV2(context.Background(), 25)
	require.NoError(t, err)
	require.True(t, newExt.IsEmpty())
	require.True(t, toDel.IsEmpty())
}

func TestWriterCoverageAdditionalBranches(t *testing.T) {
	t.Run("flush empty buffer returns nil", func(t *testing.T) {
		w := mustTestECStreamer(901, nil, nil).fWriter
		require.NoError(t, w.Flush(901, context.Background()))
	})

	t.Run("flushWithoutPool inconsistent state", func(t *testing.T) {
		s, w := testWriterWithMwEbsc(1, &BlobStoreClient{})
		w.fileOffset = 1
		w.buf = []byte{1, 2, 3}
		seedDirtyForTest(s)
		err := w.flushWithoutPool(s.ino, context.Background(), false)
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
}

func TestWriterCoverageMoreLowFunctions(t *testing.T) {
	t.Run("writeWithoutPool guards and success", func(t *testing.T) {
		var nilWriter *Writer
		_, err := nilWriter.WriteWithoutPool(context.Background(), 0, []byte("a"))
		require.Error(t, err)

		_, w := testWriterWithMwEbsc(1, &BlobStoreClient{})
		w.buf = make([]byte, 0, 8)
		w.limitManager = manager.NewLimitManager(nil)
		_, err = w.WriteWithoutPool(context.Background(), 1, []byte("a"))
		require.ErrorIs(t, err, syscall.EOPNOTSUPP)

		// success path
		err = gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

		n, err := w.WriteWithoutPool(context.Background(), 0, []byte("abc"))
		require.NoError(t, err)
		require.Equal(t, 3, n)
	})

	t.Run("writeFromReader and flushWithoutPool and freecache", func(t *testing.T) {
		t.Skip("需完整 EBS mock 链，暂由 writer_dirty / ec_streamer 增量单测覆盖 flush 路径")
		buf.InitCachePool(8)
		s := mustTestECStreamerWithEbsc(2, &BlobStoreClient{}, 8)
		s.mw = &meta.MetaWrapper{}
		w := s.fWriter
		w.buf = make([]byte, 0, 8)

		err := gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

		h := md5.New()
		size, err := w.WriteFromReader(context.Background(), strings.NewReader("abcdefghi"), h)
		require.NoError(t, err)
		require.Greater(t, size, uint64(0))

		// FlushWithoutPool wrapper branch
		seedDirtyForTest(s)
		w.fileOffset = len(w.buf)
		require.NoError(t, w.FlushWithoutPool(w.ecStreamer.ino, context.Background()))

		// FreeCache/allocateCache branches
		w.allocateCache()
		require.NotNil(t, w.buf)
		w.FreeCache()
		w.FreeCache()
	})

	t.Run("flush function direct path", func(t *testing.T) {
		s := mustTestECStreamerWithEbsc(3, &BlobStoreClient{}, 4)
		s.mw = &meta.MetaWrapper{}
		w := s.fWriter
		w.blockPosition = 4
		w.fileOffset = 4
		w.buf = make([]byte, 4)
		seedDirtyForTest(s)
		copy(w.buf, []byte("data"))
		err := gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

		require.NoError(t, w.flush(w.ecStreamer.ino, context.Background(), true))
		require.Equal(t, 0, w.blockPosition)
	})

	t.Run("flushWithoutPool then tryOverWrite no panic", func(t *testing.T) {
		t.Skip("tryOverWrite 依赖完整 extent 视图，暂由增量单测覆盖")
		blockSize := 8
		s := mustTestECStreamerWithEbsc(5, &BlobStoreClient{}, blockSize)
		s.mw = &meta.MetaWrapper{}
		w := s.fWriter
		w.blockPosition = 59287 % blockSize
		w.fileOffset = 100
		w.buf = make([]byte, blockSize)
		seedDirtyForTest(s)
		err := gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

		require.NoError(t, w.flushWithoutPool(w.ecStreamer.ino, context.Background(), false))
		require.Equal(t, 0, len(w.buf))
		require.Equal(t, 0, w.blockPosition)

		err = gohook.HookMethod(w.ecStreamer.mw, "GetObjExtents", func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 100, nil, nil, nil
		}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.mw, "GetObjExtents")
		err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck", func(_ *meta.MetaWrapper, _ uint64, _, _ proto.ObjExtentKey) error {
			return nil
		}, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck")

		n, err := w.tryOverWrite(context.Background(), 0, []byte("ab"), 0)
		require.NoError(t, err)
		require.Equal(t, 2, n)
		require.GreaterOrEqual(t, len(w.buf), 2)
	})
}

func TestWriter_Flush_not_dirty_noop(t *testing.T) {
	s := mustTestECStreamer(301, nil, nil)
	w := s.fWriter
	require.NoError(t, w.Flush(301, context.Background()))
}

func TestWriter_Flush_dirty_empty_buffer_cleans(t *testing.T) {
	s := mustTestECStreamer(302, nil, nil)
	w := s.fWriter
	seedDirtyForTest(s)
	require.NoError(t, w.Flush(302, context.Background()))
	require.False(t, s.isDirty())
}

func TestWriter_notifyCompleteFlushMeta_cleans_dirty(t *testing.T) {
	w := &Writer{fileOffset: 64, blockPosition: 8}
	s := mustTestECStreamer(303, nil, w)
	seedDirtyForTest(s)
	s.mw = &meta.MetaWrapper{}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 64, nil, nil, nil
		})

	require.NoError(t, w.notifyCompleteFlushMeta())
	require.False(t, s.isDirty())
	require.Equal(t, 0, w.bufferDirtyLen())
}

func TestWriter_notifyAfterWrite_marks_dirty(t *testing.T) {
	w := &Writer{fileOffset: 32}
	s := mustTestECStreamer(304, nil, w)
	w.notifyAfterWrite()
	require.True(t, s.isDirty())
	require.Equal(t, uint64(32), atomic.LoadUint64(&s.fileSize))
}

func TestWriter_bufferDirtyLen_pool_and_without_pool(t *testing.T) {
	s := mustTestECStreamer(306, nil, nil)
	w := s.fWriter
	w.blockPosition = 5
	require.Equal(t, 5, w.bufferDirtyLen())
	w.blockPosition = 0
	w.buf = []byte{1, 2, 3}
	require.Equal(t, 3, w.bufferDirtyLen())
}

func TestWriter_flushExt_empty_dirty_updates_meta_only(t *testing.T) {
	w := &Writer{}
	s := mustTestECStreamer(305, nil, w)
	s.mw = &meta.MetaWrapper{}
	seedDirtyForTest(s)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(s.mw), "GetObjExtents",
		func(_ *meta.MetaWrapper, _ uint64) (uint64, uint64, []proto.ExtentKey, []proto.ObjExtentKey, error) {
			return 1, 0, nil, nil, nil
		})

	require.NoError(t, w.flushExt(305, context.Background(), false))
	require.False(t, s.isDirty())
}

func TestWriter_tryOverWrite_pwrite_at_offset(t *testing.T) {
	s := mustTestECStreamer(80, nil, nil)
	w := s.Writer()
	w.fileOffset = 0
	w.blockPosition = 0

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
		func(_ *Writer, _ uint64, _ context.Context, _ bool) error { return nil })
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "notifyCompleteFlushMeta",
		func(_ *Writer) error { return nil })

	n, err := w.Write(context.Background(), 16, []byte("hello"), 0)
	require.NoError(t, err)
	require.Equal(t, 5, n)
}

func TestWriter_tryOverWrite_invalid_freeSize(t *testing.T) {
	s := mustTestECStreamer(83, nil, nil)
	w := s.Writer()
	w.fileOffset = 0
	w.blockPosition = w.ecStreamer.BlockSize()

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
		func(_ *Writer, _ uint64, _ context.Context, _ bool) error { return nil })

	_, err := w.Write(context.Background(), 8, []byte("x"), 0)
	require.ErrorIs(t, err, syscall.EINVAL)
}

func TestWriter_Write_exceeds_max_buffer(t *testing.T) {
	s := mustTestECStreamer(82, nil, nil)
	w := s.Writer()
	_, err := w.Write(context.Background(), 0, make([]byte, MaxBufferSize+1), 0)
	require.ErrorIs(t, err, syscall.EOPNOTSUPP)
}

func TestWriter_Write_sync_at_tail(t *testing.T) {
	s := mustTestECStreamerWithEbsc(84, &BlobStoreClient{}, 8<<20)
	w := s.Writer()
	w.fileOffset = 0
	w.blockPosition = 0

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "doParallelWrite",
		func(_ *Writer, _ context.Context, data []byte, _ int) (int, error) { return len(data), nil })
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "notifyCompleteFlushMeta",
		func(_ *Writer) error { return nil })

	n, err := w.Write(context.Background(), 0, []byte("z"), proto.FlagsSyncWrite)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestWriter_Write_append_buffered(t *testing.T) {
	s := mustTestECStreamer(81, nil, nil)
	w := s.Writer()
	w.fileOffset = 0
	w.blockPosition = 0

	n, err := w.Write(context.Background(), 0, []byte("abc"), 0)
	require.NoError(t, err)
	require.Equal(t, 3, n)
	require.True(t, s.isDirty())
}

func TestWriter_flushOverwriteReqs_new_and_merge(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(270, &BlobStoreClient{})
	_ = st
	const bufOff = uint64(50)
	w.buf = make([]byte, 128)
	for i := range w.buf {
		w.buf[i] = byte(i)
	}

	require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
	defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
	require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck", MockAppendObjExtentKeysWithCheckTrue, nil))
	defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck")

	t.Run("hole append only", func(t *testing.T) {
		reqs := []overwriteReq{
			{NewExtent: proto.ObjExtentKey{FileOffset: 60, Size: 10}, DiscardExtent: proto.ObjExtentKey{}},
		}
		require.NoError(t, w.flushOverwriteReqs(ctx, st.ino, reqs, bufOff, 20))
	})

	t.Run("partial merge with discard", func(t *testing.T) {
		old := proto.ObjExtentKey{FileOffset: 50, Size: 30}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, data []byte, _, size uint64, _ proto.ObjExtentKey) (int, error) {
				for i := range data {
					data[i] = byte(i)
				}
				return int(size), nil
			})
		reqs := []overwriteReq{
			{NewExtent: proto.ObjExtentKey{FileOffset: 55, Size: 5}, DiscardExtent: old},
		}
		require.NoError(t, w.flushOverwriteReqs(ctx, st.ino, reqs, bufOff, 10))
	})

	t.Run("skip zero-size req", func(t *testing.T) {
		reqs := []overwriteReq{{NewExtent: proto.ObjExtentKey{FileOffset: 70, Size: 0}}}
		require.NoError(t, w.flushOverwriteReqs(ctx, st.ino, reqs, bufOff, 0))
	})

	t.Run("read old extent fails", func(t *testing.T) {
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ []byte, _, _ uint64, _ proto.ObjExtentKey) (int, error) {
				return 0, syscall.EIO
			})
		reqs := []overwriteReq{
			{NewExtent: proto.ObjExtentKey{FileOffset: 55, Size: 5}, DiscardExtent: proto.ObjExtentKey{FileOffset: 50, Size: 20}},
		}
		err := w.flushOverwriteReqs(ctx, st.ino, reqs, bufOff, 5)
		require.Error(t, err)
		require.Contains(t, err.Error(), "read discard extent")
	})
}

// TestWriter_flushOverwriteReqs_ebsWritten_metaAppendFails_noRollback：EBS 已写入成功，metanode Append 失败；
// 不回滚 blobstore 数据，直接向上返回错误（极低概率下允许孤儿 extent / 元数据不一致）。
func TestWriter_flushOverwriteReqs_ebsWritten_metaAppendFails_noRollback(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(273, &BlobStoreClient{})
	const bufOff uint64 = 0
	w.buf = []byte("overwrite-payload")

	metaErr := errors.New("metanode append obj extent keys failed")
	var appendCalls int
	var appendedNew proto.ObjExtentKey
	var ebsWriteCalls, deleteCalls int
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Write",
		func(ebs *BlobStoreClient, ctx context.Context, vol string, data []byte, l uint32) (proto2.Location, error) {
			ebsWriteCalls++
			return MockEbscWriteTrue(ebs, ctx, vol, data, l)
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Delete",
		func(_ *BlobStoreClient, _ []proto.ObjExtentKey) error {
			deleteCalls++
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.mw), "AppendObjExtentKeysWithCheck",
		func(_ *meta.MetaWrapper, ino uint64, newEk, discardEk proto.ObjExtentKey) error {
			appendCalls++
			require.Equal(t, st.ino, ino)
			require.True(t, discardEk.IsEmpty())
			require.False(t, newEk.IsEmpty())
			appendedNew = newEk
			return metaErr
		})

	reqs := []overwriteReq{
		{NewExtent: proto.ObjExtentKey{FileOffset: 0, Size: uint64(len(w.buf))}, DiscardExtent: proto.ObjExtentKey{}},
	}
	err := w.flushOverwriteReqs(ctx, st.ino, reqs, bufOff, len(w.buf))
	require.ErrorIs(t, err, metaErr)
	require.Equal(t, 1, ebsWriteCalls, "data must be written to EBS before meta update")
	require.Equal(t, 1, appendCalls)
	require.Equal(t, uint64(0), appendedNew.FileOffset)
	require.NotZero(t, appendedNew.Cid, "writeSlice should fill objExtentKey from EBS location")
	require.Zero(t, deleteCalls, "must not rollback EBS when only metanode append fails")
}

func TestWriter_reshapeBufForCopyPath_branches(t *testing.T) {
	var nilW *Writer
	nilW.reshapeBufForCopyPath()

	s := mustTestECStreamer(271, nil, nil)
	w := s.Writer()
	w.reshapeBufForCopyPath()

	w.blockPosition = w.ecStreamer.BlockSize() + 1
	w.buf = make([]byte, 0, w.ecStreamer.BlockSize())
	w.reshapeBufForCopyPath()
	require.Equal(t, 0, w.blockPosition)
	require.Equal(t, w.ecStreamer.BlockSize(), len(w.buf))
}

func TestRwSlice_String(t *testing.T) {
	s := rwSlice{fileOffset: 1, size: 2, hole: true}
	require.Contains(t, s.String(), "rwSlice{")
}

func TestWriter_flushExt_partial_overlap_path(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(272, &BlobStoreClient{})
	seedStreamerExtentsForTest(st, 200, []proto.ObjExtentKey{{FileOffset: 0, Size: 200}})
	seedDirtyForTest(st)
	w.buf = make([]byte, 64)
	w.fileOffset = 120
	w.blockPosition = 20

	require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
	defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
	require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck", MockAppendObjExtentKeysWithCheckTrue, nil))
	defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck")

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Read",
		func(_ *BlobStoreClient, _ context.Context, _ string, data []byte, _, size uint64, _ proto.ObjExtentKey) (int, error) {
			return int(size), nil
		})

	require.NoError(t, w.flushExt(st.ino, ctx, false))
	require.False(t, st.isDirty())
}
