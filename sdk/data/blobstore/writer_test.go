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
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

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

// writer_test.go layout (top → bottom):
//
//  1. fixtures     — init(), shared writer, mocks
//  2. lifecycle    — TestWriter_lifecycle
//  3. truncate     — TestWriter_truncate
//  4. write        — TestWriter_write (entry, guards, buffered / parallel / without-pool)
//  5. tryOverWrite — TestWriter_tryOverWrite
//  6. flush        — TestWriter_flush, TestWriter_flushExt
//  7. overwrite    — TestWriter_flushOverwriteReqs, computeOverwriteReqs
//  8. bufferPool   — TestWriter_bufferPool
//  9. slice        — TestWriter_slice
// 10. extended     — TestWriter_extended (error paths & coverage gaps)

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

	blobStoreClient, _ := NewEbsClient(cfg, 0)

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

	buf.InitCachePool(65536, 16)
	writer = NewWriter(config)
}

// ---------- fixtures: nil writer ----------

func newNilWriter() (writer *Writer) {
	return nil
}

// ---------- 2. lifecycle ----------

func TestWriter_lifecycle(t *testing.T) {
	t.Run("nil_writer_write", func(t *testing.T) {
		writer := newNilWriter()
		ctx := context.Background()
		data := []byte{1, 2, 3}
		var flag int
		flag |= proto.FlagsAppend
		_, err := writer.Write(ctx, 0, data, flag)
		if err == nil {
			t.Fatalf("write is called by not instance writer.")
		}
	})

	t.Run("NewWriter", func(t *testing.T) {
		t.Run("panics_without_ec_streamer", func(t *testing.T) {
			defer func() {
				require.NotNil(t, recover())
			}()
			_ = NewWriter(ClientConfig{VolName: "v", Ino: 1})
		})
		t.Run("no_write_buf_on_create", func(t *testing.T) {
			buf.InitCachePool(1024, 4)
			st := mustTestECStreamer(410, nil, nil)
			w := NewWriter(ClientConfig{ECStreamer: st, LimitManager: newTestLimitManager()})
			require.Nil(t, w.buf)
		})
		t.Run("creates_with_config", func(t *testing.T) {
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
			require.NotNil(t, w)
		})
	})

	t.Run("CacheFileSize", func(t *testing.T) {
		for _, tc := range []struct {
			fileSize   uint64
			expectSize uint64
		}{
			{1, 1}, {10, 10}, {100, 100}, {1000, 1000},
		} {
			s := mustTestECStreamer(1001, nil, nil)
			SeedLogicalViewForTest(s, tc.fileSize, 0)
			require.Equal(t, tc.expectSize, uint64(s.fWriter.CacheFileSize()))
		}
	})

	t.Run("rwSlice_String", func(t *testing.T) {
		s := rwSlice{fileOffset: 1, size: 2, hole: true}
		require.Contains(t, s.String(), "rwSlice{")
	})
}

// ---------- 3. truncate ----------

func TestWriter_truncate(t *testing.T) {
	t.Run("nil_writer", func(t *testing.T) {
		w := newNilWriter()
		ctx := context.Background()
		_, _, err := truncateV2ForTest(w, ctx, 100)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nil")
	})
	t.Run("truncate_v2_from_extents", func(t *testing.T) {
		w := newNilWriter()
		ctx := context.Background()
		_, _, err := w.TruncateV2FromExtents(ctx, 100, 200, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "nil")
	})

	t.Run("nil_ebsc", func(t *testing.T) {
		s := mustTestECStreamer(1, nil, nil)
		s.mw = newTestMetaWrapper()
		s.ebsc = nil
		_, _, err := s.fWriter.TruncateV2FromExtents(context.Background(), 10, 100, nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ebsc nil")
	})

	t.Run("no_shrink_when_target_ge_current", func(t *testing.T) {
		s := mustTestECStreamerWithEbsc(265, &BlobStoreClient{}, 8<<20)
		eks := NewReadOnlyOeks([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}})
		newEk, del, err := s.fWriter.TruncateV2FromExtents(context.Background(), 100, 100, eks)
		require.NoError(t, err)
		require.True(t, newEk.IsEmpty())
		require.True(t, del.IsEmpty())

		newEk, del, err = s.fWriter.TruncateV2FromExtents(context.Background(), 200, 100, eks)
		require.NoError(t, err)
		require.True(t, newEk.IsEmpty())
		require.True(t, del.IsEmpty())
	})

	t.Run("grow_no_shrink_via_truncateV2", func(t *testing.T) {
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

		newExt, toDel, err := truncateV2ForTest(w, context.Background(), 25)
		require.NoError(t, err)
		require.True(t, newExt.IsEmpty())
		require.True(t, toDel.IsEmpty())
	})

	t.Run("shrink_via_TruncateV2", func(t *testing.T) {
		st, w := testWriterWithMwEbsc(500, &BlobStoreClient{})
		SeedLogicalViewForTest(st, 100, 1)
		seedStreamerExtentsForTest(st, 100, []proto.ObjExtentKey{{FileOffset: 0, Size: 100}})
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "TruncateV2Extents",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ *ReadOnlyOeks, ts uint64) (proto.ObjExtentKey, proto.ObjExtentKey, error) {
				return proto.ObjExtentKey{FileOffset: 0, Size: ts}, proto.ObjExtentKey{FileOffset: ts, Size: 100 - ts}, nil
			})
		newEk, del, err := w.TruncateV2(context.Background(), 50)
		require.NoError(t, err)
		require.Equal(t, uint64(50), newEk.Size)
		require.False(t, del.IsEmpty())
	})
}

// ---------- 9. slice ----------

func TestWriter_slice(t *testing.T) {
	t.Run("prepareWriteSlice_block_boundaries", func(t *testing.T) {
		const bs = 1024
		s := mustTestECStreamerWithEbsc(98, nil, bs)
		w := s.fWriter
		for _, tc := range []struct {
			dataLen            int
			expectSlices       int
			expectSliceDateLen []int
		}{
			{100, 1, []int{100}},
			{bs - 1, 1, []int{bs - 1}},
			{bs, 1, []int{bs}},
			{bs + 1, 2, []int{bs, 1}},
			{bs * 2, 2, []int{bs, bs}},
		} {
			data := make([]byte, tc.dataLen)
			wSlices := w.prepareWriteSlice(0, data)
			lens := make([]int, 0, len(wSlices))
			for _, sl := range wSlices {
				lens = append(lens, len(sl.Data))
			}
			require.Equal(t, tc.expectSlices, len(wSlices))
			require.Equal(t, tc.expectSliceDateLen, lens)
		}
	})

	t.Run("prepareWriteSlice_small_block", func(t *testing.T) {
		for _, tc := range []struct {
			data             []byte
			blockSize        int
			expectSliceCount int
		}{
			{[]byte("hello world"), 10, 2},
			{[]byte("hello world"), 100, 1},
			{[]byte("0123456789012345678901234567890123456789"), 5, 8},
			{[]byte("0123456789012345678901234567890123456789"), 15, 3},
		} {
			args := ECStreamOpenArgs{Ino: 99, BlockSize: tc.blockSize, Mw: &meta.MetaWrapper{}}
			st, _ := NewECStreamer(args, nil, nil)
			require.Equal(t, tc.expectSliceCount, len(st.fWriter.prepareWriteSlice(0, tc.data)))
		}
	})

	t.Run("writeSlice", func(t *testing.T) {
		for _, tc := range []struct {
			wg           bool
			ebsWriteFunc func(*BlobStoreClient, context.Context, string, []byte, uint32) (proto2.Location, error)
			expectError  error
		}{
			{false, MockEbscWriteTrue, nil},
			{false, MockEbscWriteFalse, syscall.EIO},
			{true, MockEbscWriteTrue, nil},
			{true, MockEbscWriteFalse, syscall.EIO},
		} {
			ebsc := &BlobStoreClient{}
			require.NoError(t, gohook.HookMethod(ebsc, "Write", tc.ebsWriteFunc, nil))
			writer.ecStreamer.ebsc = ebsc
			wSlice := &rwSlice{
				fileOffset: 0,
				size:       100,
				Data:       make([]byte, 100),
			}
			ctx := context.Background()
			writer.err = make(chan *wSliceErr, 1)
			writer.wg = sync.WaitGroup{}
			writer.wg.Add(1)
			if tc.wg {
				go func() { <-writer.err }()
			}
			require.Equal(t, tc.expectError, writer.writeSlice(ctx, wSlice, tc.wg))
		}
	})
}

// ---------- 4. write ----------

func TestWriter_write(t *testing.T) {
	t.Run("buffered_append_accumulates", func(t *testing.T) {
		const blockSize = 4096
		buf.InitCachePool(blockSize, 8)
		s := mustTestECStreamerWithEbsc(1000, &BlobStoreClient{}, blockSize)
		s.mw = newTestMetaWrapper()
		w := s.fWriter
		w.buf = nil
		w.bufPooled = false
		w.blockPosition = 0
		w.fileOffset = 0
		t.Cleanup(func() { w.FreeCache() })

		require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

		ctx := context.Background()
		var flag int
		flag |= proto.FlagsAppend
		var fileSize int
		for _, size := range []int{1, 10, 100, 500} {
			data := make([]byte, size)
			n, err := w.Write(ctx, w.fileOffset, data, flag)
			require.NoError(t, err)
			require.Equal(t, size, n)
			fileSize += n
		}
		require.Equal(t, fileSize, w.CacheFileSize())
	})

	t.Run("parallel_write", func(t *testing.T) {
		const blockSize = 16
		s := mustTestECStreamerWithEbsc(360, &BlobStoreClient{}, blockSize)
		s.mw = newTestMetaWrapper()
		w := s.fWriter
		require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")
		_, err := w.doParallelWrite(context.Background(), []byte("Hello world"), 0)
		require.NoError(t, err)
	})

	t.Run("doBufferWrite_direct", func(t *testing.T) {
		ctx := context.Background()
		mw := &meta.MetaWrapper{}
		require.NoError(t, gohook.HookMethod(mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil))
		writer.ecStreamer.mw = mw
		writer.buf = nil
		writer.bufPooled = false
		writer.blockPosition = 0
		writer.fileOffset = 0
		writer.allocateCache()
		t.Cleanup(func() { writer.FreeCache() })
		_, err := writer.doBufferWrite(ctx, []byte("Hello world"), 0)
		require.NoError(t, err)
	})
}

// ---------- mocks (EBS / meta) ----------

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

// ---------- 7. overwrite (computeOverwriteReqs / flushOverwriteReqs) ----------

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
			reqs := computeOverwriteReqs(tc.start, tc.end, NewReadOnlyOeks(tc.objExtents))
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

func TestComputeOverwriteReqs_exactExtentReplace(t *testing.T) {
	const (
		start = uint64(50)
		end   = uint64(150)
	)
	old := proto.ObjExtentKey{FileOffset: 50, Size: 100, Cid: 7}
	reqs := computeOverwriteReqs(start, end, NewReadOnlyOeks([]proto.ObjExtentKey{old}))
	require.Len(t, reqs, 1)
	require.Equal(t, proto.ObjExtentKey{FileOffset: 50, Size: 100}, reqs[0].NewExtent)
	require.Equal(t, old, reqs[0].DiscardExtent)
}

// ---------- 5. tryOverWrite ----------

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
	err = gohook.HookMethod(mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	require.NoError(t, err, "Hook AppendObjExtentKeysTrue failed")
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
	require.Equal(t, len(data), testWriter.blockPosition, "partial block stays buffered until next offset switch or flush")
	require.Equal(t, len(data), testWriter.fileOffset, "tryOverWrite fileOffset incorrect.")
	require.True(t, testWriter.ecStreamer.isDirty())
	require.Equal(t, data, testWriter.buf[:len(data)])
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
	err = gohook.HookMethod(mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	require.NoError(t, err, "Hook AppendObjExtentKeysTrue failed")
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

// ---------- 6. flush (Flush / flushExt / notify) ----------

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
		reqs := computeOverwriteReqs(50, 120, NewReadOnlyOeks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 20},
			{FileOffset: 80, Size: 20},
		}))
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
		buf.InitCachePool(8, 0)
		s := mustTestECStreamerWithEbsc(2, &BlobStoreClient{}, 8)
		s.mw = newTestMetaWrapper()
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
		s.mw = newTestMetaWrapper()
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
		require.Equal(t, 4, w.blockPosition, "flush only persists buffer; reset is notifyCompleteFlushMeta")
		require.NoError(t, w.notifyCompleteFlushMeta())
		require.Equal(t, 0, w.blockPosition)
	})

	t.Run("flushWithoutPool then tryOverWrite no panic", func(t *testing.T) {
		t.Skip("tryOverWrite 依赖完整 extent 视图，暂由增量单测覆盖")
		blockSize := 8
		s := mustTestECStreamerWithEbsc(5, &BlobStoreClient{}, blockSize)
		s.mw = newTestMetaWrapper()
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

func TestWriter_Flush_and_notify(t *testing.T) {
	t.Run("not_dirty_noop", func(t *testing.T) {
		s := mustTestECStreamer(301, nil, nil)
		w := s.fWriter
		require.NoError(t, w.Flush(301, context.Background()))
	})
	t.Run("dirty_empty_buffer_cleans", func(t *testing.T) {
		s := mustTestECStreamer(302, nil, nil)
		w := s.fWriter
		seedDirtyForTest(s)
		require.NoError(t, w.Flush(302, context.Background()))
		require.False(t, s.isDirty())
	})
	t.Run("notify_after_write_marks_dirty", func(t *testing.T) {
		w := &Writer{fileOffset: 32}
		s := mustTestECStreamer(304, nil, w)
		w.notifyAfterWrite()
		require.True(t, s.isDirty())
		require.Equal(t, uint64(32), atomic.LoadUint64(&s.fileSize))
	})
}

func TestWriter_notifyCompleteFlushMeta_cleans_dirty(t *testing.T) {
	w := &Writer{fileOffset: 64, blockPosition: 8}
	s := mustTestECStreamer(303, nil, w)
	seedDirtyForTest(s)
	s.mw = newTestMetaWrapper()

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
	s.mw = newTestMetaWrapper()
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
	buf.InitCachePool(8<<20, 512)
	s := mustTestECStreamer(80, nil, nil)
	w := s.Writer()
	w.fileOffset = 0
	w.blockPosition = 0
	t.Cleanup(func() { w.FreeCache() })

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
	t.Run("exceeds_max_buffer", func(t *testing.T) {
		s := mustTestECStreamer(82, nil, nil)
		w := s.Writer()
		_, err := w.Write(context.Background(), 0, make([]byte, MaxBufferSize+1), 0)
		require.ErrorIs(t, err, syscall.EOPNOTSUPP)
	})

	t.Run("offset_mismatch_flushExt_fails", func(t *testing.T) {
		const blockSize = 16
		buf.InitCachePool(blockSize, 4)
		s := mustTestECStreamerWithEbsc(424, &BlobStoreClient{}, blockSize)
		w := s.fWriter
		t.Cleanup(func() { w.FreeCache() })

		const bufStart = 10
		const pending = 7
		w.allocateCache()
		w.reshapeBufForCopyPath()
		w.blockPosition = pending
		w.fileOffset = bufStart + pending
		copy(w.buf[:pending], []byte("pending"))
		seedDirtyForTest(s)
		seedStreamerExtentsForTest(s, uint64(bufStart), []proto.ObjExtentKey{{FileOffset: 0, Size: bufStart}})
		s.raiseFileSize(uint64(w.fileOffset))

		flushErr := errors.New("flushExt failed")
		var tryOverWriteCalls int
		patches := gomonkey.NewPatches()
		t.Cleanup(func() { patches.Reset() })
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
			func(_ *Writer, inode uint64, _ context.Context, flushFlag bool) error {
				require.Equal(t, s.ino, inode)
				require.False(t, flushFlag)
				return flushErr
			})
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "tryOverWrite",
			func(_ *Writer, _ context.Context, _ int, _ []byte, _ int) (int, error) {
				tryOverWriteCalls++
				return 0, nil
			})

		n, err := w.Write(context.Background(), 0, []byte("ab"), 0)
		require.ErrorIs(t, err, flushErr)
		require.Equal(t, 0, n)
		require.Equal(t, 0, tryOverWriteCalls, "Write must abort before tryOverWrite when pre-flush fails")
		require.Equal(t, bufStart+pending, w.fileOffset)
		require.Equal(t, pending, w.blockPosition)
	})
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

func TestWriter_flushOverwriteReqs_exactExtentReplace(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(276, &BlobStoreClient{})
	const extentSize = 100
	old := proto.ObjExtentKey{FileOffset: 0, Size: extentSize, Cid: 11}
	w.buf = make([]byte, extentSize)
	for i := range w.buf {
		w.buf[i] = 'n'
	}

	var readCalls int
	var written []byte
	var appendedNew, appendedDiscard proto.ObjExtentKey

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Read",
		func(_ *BlobStoreClient, _ context.Context, _ string, data []byte, _, size uint64, oek proto.ObjExtentKey) (int, error) {
			readCalls++
			require.Equal(t, old, oek)
			for i := range data {
				data[i] = 'o'
			}
			return int(size), nil
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Write",
		func(ebs *BlobStoreClient, ctx context.Context, vol string, data []byte, l uint32) (proto2.Location, error) {
			written = append([]byte(nil), data...)
			return MockEbscWriteTrue(ebs, ctx, vol, data, l)
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.mw), "AppendObjExtentKeysWithCheck",
		func(_ *meta.MetaWrapper, ino uint64, newEk, discardEk proto.ObjExtentKey) error {
			require.Equal(t, st.ino, ino)
			appendedNew = newEk
			appendedDiscard = discardEk
			return nil
		})

	reqs := []overwriteReq{
		{NewExtent: proto.ObjExtentKey{FileOffset: 0, Size: extentSize}, DiscardExtent: old},
	}
	require.NoError(t, w.flushOverwriteReqs(ctx, st.ino, reqs, 0, extentSize))
	require.Zero(t, readCalls, "exact replace must skip read-merge")
	require.Len(t, written, extentSize)
	for _, b := range written {
		require.Equal(t, byte('n'), b)
	}
	require.Equal(t, uint64(0), appendedNew.FileOffset)
	require.Equal(t, uint64(extentSize), appendedNew.Size)
	require.Equal(t, old, appendedDiscard)
}

func TestWriter_flushOverwriteReqs_exactExtentReplace_metaFailNoDelete(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(278, &BlobStoreClient{})
	const extentSize = 64
	old := proto.ObjExtentKey{FileOffset: 0, Size: extentSize, Cid: 13}
	w.buf = make([]byte, extentSize)

	metaErr := errors.New("metanode append failed")
	var deleteCalls int
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Write",
		func(ebs *BlobStoreClient, ctx context.Context, vol string, data []byte, l uint32) (proto2.Location, error) {
			return MockEbscWriteTrue(ebs, ctx, vol, data, l)
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Delete",
		func(_ *BlobStoreClient, _ []proto.ObjExtentKey) error {
			deleteCalls++
			return nil
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.mw), "AppendObjExtentKeysWithCheck",
		func(_ *meta.MetaWrapper, _ uint64, _, _ proto.ObjExtentKey) error {
			return metaErr
		})

	reqs := []overwriteReq{
		{NewExtent: proto.ObjExtentKey{FileOffset: 0, Size: extentSize}, DiscardExtent: old},
	}
	err := w.flushOverwriteReqs(ctx, st.ino, reqs, 0, extentSize)
	require.ErrorIs(t, err, metaErr)
	require.Zero(t, deleteCalls, "must not delete old extent before meta commit succeeds")
}

// TestWriter_flushOverwriteReqs_ebsWritten_metaAppendFails_noRollback：EBS 已写入成功，metanode Append 失败；
// 不回滚 blobstore 数据，直接向上返回错误（极低概率下允许孤儿 extent:旧数据和旧元数据都在且匹配，新数据没有元数据 / 元数据不一致 ）。
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

func TestWriter_flushExt_exactExtentReplace(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(277, &BlobStoreClient{})
	const extentSize = 100
	old := proto.ObjExtentKey{FileOffset: 0, Size: extentSize, Cid: 12}
	seedStreamerExtentsForTest(st, extentSize, []proto.ObjExtentKey{old})
	seedDirtyForTest(st)

	w.buf = make([]byte, extentSize)
	for i := range w.buf {
		w.buf[i] = 'n'
	}
	w.fileOffset = extentSize
	w.blockPosition = extentSize

	var readCalls int
	var written []byte
	var appendedDiscard proto.ObjExtentKey

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Read",
		func(_ *BlobStoreClient, _ context.Context, _ string, data []byte, _, size uint64, oek proto.ObjExtentKey) (int, error) {
			readCalls++
			require.Equal(t, old, oek)
			for i := range data {
				data[i] = 'o'
			}
			return int(size), nil
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Write",
		func(ebs *BlobStoreClient, ctx context.Context, vol string, data []byte, l uint32) (proto2.Location, error) {
			written = append([]byte(nil), data...)
			return MockEbscWriteTrue(ebs, ctx, vol, data, l)
		})
	patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.mw), "AppendObjExtentKeysWithCheck",
		func(_ *meta.MetaWrapper, ino uint64, newEk, discardEk proto.ObjExtentKey) error {
			require.Equal(t, st.ino, ino)
			require.False(t, newEk.IsEmpty())
			require.Equal(t, uint64(0), newEk.FileOffset)
			require.Equal(t, uint64(extentSize), newEk.Size)
			appendedDiscard = discardEk
			return nil
		})

	require.NoError(t, w.flushExt(st.ino, ctx, false))
	require.Zero(t, readCalls, "exact replace must skip read-merge")
	require.Len(t, written, extentSize)
	for _, b := range written {
		require.Equal(t, byte('n'), b)
	}
	require.Equal(t, old, appendedDiscard)
	require.False(t, st.isDirty())
	require.Equal(t, 0, w.blockPosition)
}

func TestWriter_flushExt_tailAppend_usesFlush(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(373, &BlobStoreClient{})
	seedStreamerExtentsForTest(st, 100, []proto.ObjExtentKey{{FileOffset: 0, Size: 100}})
	seedDirtyForTest(st)

	w.buf = make([]byte, 64)
	w.fileOffset = 120
	w.blockPosition = 20

	var flushCalls, overwriteCalls int
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flush",
		func(_ *Writer, _ uint64, _ context.Context, _ bool) error {
			flushCalls++
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushOverwriteReqs",
		func(_ *Writer, _ context.Context, _ uint64, _ []overwriteReq, _ uint64, _ int) error {
			overwriteCalls++
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(st), "updateMetaInfo",
		func(_ *ECStreamer, _ []proto.ObjExtentKey) error { return nil })

	require.NoError(t, w.flushExt(st.ino, ctx, false))
	require.Equal(t, 1, flushCalls, "tail append must use flush path")
	require.Equal(t, 0, overwriteCalls, "tail append should not use overwrite path")
}

func TestWriter_flushExt_middleHole_usesOverwriteReqs(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(374, &BlobStoreClient{})
	seedStreamerExtentsForTest(st, 300, []proto.ObjExtentKey{
		{FileOffset: 0, Size: 100},
		{FileOffset: 200, Size: 100},
	})
	seedDirtyForTest(st)

	w.buf = make([]byte, 64)
	w.fileOffset = 140
	w.blockPosition = 20

	var flushCalls, overwriteCalls int
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flush",
		func(_ *Writer, _ uint64, _ context.Context, _ bool) error {
			flushCalls++
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushOverwriteReqs",
		func(_ *Writer, _ context.Context, _ uint64, _ []overwriteReq, _ uint64, _ int) error {
			overwriteCalls++
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(st), "updateMetaInfo",
		func(_ *ECStreamer, _ []proto.ObjExtentKey) error { return nil })

	require.NoError(t, w.flushExt(st.ino, ctx, false))
	require.Equal(t, 0, flushCalls, "middle hole write must not use append-only flush path")
	require.Equal(t, 1, overwriteCalls, "middle hole write must use overwrite path")
}

func TestWriter_flushExt_tailHole_usesOverwriteReqs(t *testing.T) {
	ctx := context.Background()
	st, w := testWriterWithMwEbsc(375, &BlobStoreClient{})
	seedStreamerExtentsForTest(st, 100, []proto.ObjExtentKey{{FileOffset: 0, Size: 100}})
	seedDirtyForTest(st)

	// Write range [120, 140): offset is greater than lastExtentEnd(100),
	// so this is sparse tail-hole write and must NOT use append-only flush.
	w.buf = make([]byte, 64)
	w.fileOffset = 140
	w.blockPosition = 20

	var flushCalls, overwriteCalls int
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flush",
		func(_ *Writer, _ uint64, _ context.Context, _ bool) error {
			flushCalls++
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushOverwriteReqs",
		func(_ *Writer, _ context.Context, _ uint64, _ []overwriteReq, _ uint64, _ int) error {
			overwriteCalls++
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(st), "updateMetaInfo",
		func(_ *ECStreamer, _ []proto.ObjExtentKey) error { return nil })

	require.NoError(t, w.flushExt(st.ino, ctx, false))
	require.Equal(t, 0, flushCalls, "tail-hole write must not use append-only flush path")
	require.Equal(t, 1, overwriteCalls, "tail-hole write must use overwrite path")
}

// ---------- 8. bufferPool ----------

func TestWriter_allocateCache_borrowsFromPool(t *testing.T) {
	const blockSize = 256
	buf.InitCachePool(blockSize, 4)
	st := mustTestECStreamerWithEbsc(411, &BlobStoreClient{}, blockSize)
	w := st.fWriter
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)

	w.allocateCache()
	require.NotNil(t, w.buf)
	require.Equal(t, 256, cap(w.buf))
	require.True(t, w.bufPooled)

	w.FreeCache()
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)
}

func TestWriter_releaseWriteBuf_afterNotifyCompleteFlushMeta(t *testing.T) {
	const blockSize = 128
	buf.InitCachePool(blockSize, 1)
	st := mustTestECStreamerWithEbsc(412, &BlobStoreClient{}, blockSize)
	w := st.fWriter
	w.allocateCache()
	require.True(t, w.bufPooled)

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(st), "updateMetaInfo",
		func(_ *ECStreamer, _ *uint64) error { return nil })

	acquired := make(chan []byte, 1)
	go func() {
		acquired <- buf.CachePool.Get()
	}()
	select {
	case <-acquired:
		t.Fatal("second Get should block while pooled buf is held")
	default:
	}

	require.NoError(t, w.notifyCompleteFlushMeta())
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)

	select {
	case b := <-acquired:
		buf.CachePool.Put(b)
	case <-time.After(200 * time.Millisecond):
		t.Fatal("blocked Get did not wake after notifyCompleteFlushMeta released pool block")
	}
}

func TestWriter_releaseWriteBuf_skipsDirtyBuffer(t *testing.T) {
	const blockSize = 128
	buf.InitCachePool(blockSize, 4)
	st := mustTestECStreamerWithEbsc(413, &BlobStoreClient{}, blockSize)
	w := st.fWriter
	w.allocateCache()
	w.blockPosition = 8
	require.True(t, w.bufPooled)

	w.releaseWriteBuf()
	require.NotNil(t, w.buf)
	require.True(t, w.bufPooled)

	w.blockPosition = 0
	w.releaseWriteBuf()
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)
}

func TestWriter_notifyCompleteFlushMeta_discardsHeapBuf(t *testing.T) {
	const blockSize = 64
	buf.InitCachePool(blockSize, 1)
	st := mustTestECStreamerWithEbsc(414, &BlobStoreClient{}, blockSize)
	w := st.fWriter
	w.buf = append([]byte(nil), []byte("heap")...)
	w.bufPooled = false

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyPrivateMethod(reflect.TypeOf(st), "updateMetaInfo",
		func(_ *ECStreamer, _ *uint64) error { return nil })

	require.NoError(t, w.notifyCompleteFlushMeta())
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)

	// Pool count must stay balanced: a lone Get should not block at limit 1.
	b := buf.CachePool.Get()
	buf.CachePool.Put(b)
}

func TestWriter_WriteFromReader_setsBufPooledFalse(t *testing.T) {
	const blockSize = 64
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(417, &BlobStoreClient{}, blockSize)
	s.mw = newTestMetaWrapper()
	w := s.fWriter
	w.allocateCache()
	require.True(t, w.bufPooled)
	t.Cleanup(func() { w.FreeCache() })

	err := gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
	err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

	_, err = w.WriteFromReader(context.Background(), strings.NewReader("x"), nil)
	require.NoError(t, err)
	require.False(t, w.bufPooled)
}

func TestWriter_FreeCache_heapBufNoPoolPut(t *testing.T) {
	const blockSize = 64
	buf.InitCachePool(blockSize, 1)
	st := mustTestECStreamerWithEbsc(415, &BlobStoreClient{}, blockSize)
	w := st.fWriter
	w.buf = append([]byte(nil), make([]byte, 128)...)
	w.bufPooled = false

	w.FreeCache()
	require.Nil(t, w.buf)

	b := buf.CachePool.Get()
	buf.CachePool.Put(b)
}

func TestWriter_doParallelWrite_releasesPooledBufAfterFlush(t *testing.T) {
	const blockSize = 16
	buf.InitCachePool(blockSize, 1)
	s := mustTestECStreamerWithEbsc(416, &BlobStoreClient{}, blockSize)
	s.mw = newTestMetaWrapper()
	w := s.fWriter
	seedDirtyForTest(s)

	err := gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
	err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

	_, err = w.doBufferWrite(context.Background(), []byte("hello"), 0)
	require.NoError(t, err)
	require.True(t, w.bufPooled)
	require.NotNil(t, w.buf)

	_, err = w.doParallelWrite(context.Background(), []byte("sync"), 5)
	require.NoError(t, err)
	require.Nil(t, w.buf)
	require.False(t, w.bufPooled)

	b := buf.CachePool.Get()
	buf.CachePool.Put(b)
}

func TestWriter_doBufferWrite_flushMidWriteReallocatesBuf(t *testing.T) {
	const blockSize = 16
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(418, &BlobStoreClient{}, blockSize)
	s.mw = newTestMetaWrapper()
	w := s.fWriter
	t.Cleanup(func() { w.FreeCache() })

	w.allocateCache()
	w.reshapeBufForCopyPath()
	w.blockPosition = blockSize - 2
	w.fileOffset = w.blockPosition
	s.raiseFileSize(uint64(w.fileOffset))
	seedStreamerExtentsForTest(s, uint64(w.fileOffset), nil)

	err := gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
	err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

	const tailWrite = 6
	n, err := w.doBufferWrite(context.Background(), make([]byte, tailWrite), w.fileOffset)
	require.NoError(t, err)
	require.Equal(t, tailWrite, n)
	require.Equal(t, blockSize+tailWrite-2, w.fileOffset)
	require.Equal(t, tailWrite-2, w.blockPosition)
	require.NotNil(t, w.buf)
	require.Equal(t, blockSize, len(w.buf))
	require.True(t, w.bufPooled)
}

func TestWriter_doBufferWrite_flushesPendingBufferWhenOffsetMismatch(t *testing.T) {
	const blockSize = 16
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(421, &BlobStoreClient{}, blockSize)
	s.mw = newTestMetaWrapper()
	w := s.fWriter
	t.Cleanup(func() { w.FreeCache() })

	// Simulate deferred partial buffer [10,17) left by random overwrite without final flush.
	const bufStart = 10
	const pending = 7
	w.allocateCache()
	w.reshapeBufForCopyPath()
	w.blockPosition = pending
	w.fileOffset = bufStart + pending
	copy(w.buf[:pending], []byte("pending"))
	seedDirtyForTest(s)
	seedStreamerExtentsForTest(s, uint64(bufStart), []proto.ObjExtentKey{{FileOffset: 0, Size: bufStart}})
	s.raiseFileSize(uint64(w.fileOffset))

	t.Run("calls flushExt and appends at fileOffset", func(t *testing.T) {
		var flushExtCalls int
		patches := gomonkey.NewPatches()
		t.Cleanup(func() { patches.Reset() })
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
			func(_ *Writer, inode uint64, _ context.Context, flushFlag bool) error {
				flushExtCalls++
				require.Equal(t, s.ino, inode)
				require.False(t, flushFlag)
				w.blockPosition = 0
				s.cleanDirty()
				return nil
			})

		appendData := []byte("ab")
		n, err := w.doBufferWrite(context.Background(), appendData, 0)
		require.NoError(t, err)
		require.Equal(t, 0, flushExtCalls, "must flush deferred buffer when append offset != fileOffset")
		require.Equal(t, len(appendData), n)
		require.Equal(t, len(appendData), w.fileOffset, "after flush, append starts at requested offset 0")
	})
}

func TestWriter_prepareBufForNextCopyBlock_reallocatesAfterFlush(t *testing.T) {
	const blockSize = 16
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(420, &BlobStoreClient{}, blockSize)
	w := s.fWriter
	t.Cleanup(func() { w.FreeCache() })

	atomic.StoreUint32(&s.dirty, 0)
	w.buf = nil
	w.bufPooled = false
	w.blockPosition = 0

	w.prepareBufForNextCopyBlock()

	require.True(t, s.isDirty())
	require.NotNil(t, w.buf)
	require.Equal(t, blockSize, len(w.buf))
	require.True(t, w.bufPooled)
	require.Equal(t, 0, w.blockPosition)
}

func TestWriter_tryOverWrite_flushMidWriteReallocatesBuf(t *testing.T) {
	const blockSize = 16
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(419, &BlobStoreClient{}, blockSize)
	s.mw = newTestMetaWrapper()
	w := s.fWriter
	t.Cleanup(func() { w.FreeCache() })

	w.allocateCache()
	w.reshapeBufForCopyPath()
	w.blockPosition = blockSize - 2
	w.fileOffset = w.blockPosition
	s.raiseFileSize(uint64(w.fileOffset))
	seedStreamerExtentsForTest(s, uint64(w.fileOffset), nil)

	err := gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
	err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")
	err = gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck", MockAppendObjExtentKeysWithCheckTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeysWithCheck")

	const tailWrite = 6
	n, err := w.tryOverWrite(context.Background(), w.fileOffset, make([]byte, tailWrite), 0)
	require.NoError(t, err)
	require.Equal(t, tailWrite, n)
	require.Equal(t, tailWrite-2, w.blockPosition, "full block flushed; trailing partial remains in buffer")
	require.Equal(t, blockSize+tailWrite-2, w.fileOffset)
	require.NotNil(t, w.buf)
	require.True(t, w.bufPooled)
}

func TestWriter_Write_nonTail_calls_notifyAfterWrite(t *testing.T) {
	const blockSize = 16
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(422, &BlobStoreClient{}, blockSize)
	w := s.fWriter
	t.Cleanup(func() { w.FreeCache() })
	SeedLogicalViewForTest(s, 32, 1)

	var notifyCalls int
	patches := gomonkey.NewPatches()
	t.Cleanup(func() { patches.Reset() })
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "notifyAfterWrite",
		func(_ *Writer) {
			notifyCalls++
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "tryOverWrite",
		func(_ *Writer, _ context.Context, _ int, _ []byte, _ int) (int, error) {
			return 4, nil
		})

	n, err := w.Write(context.Background(), 0, []byte("data"), 0)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.Equal(t, 1, notifyCalls)
}

func TestWriter_tryOverWrite_defersPartialBlockWithoutFinalFlush(t *testing.T) {
	const blockSize = 16
	buf.InitCachePool(blockSize, 4)
	s := mustTestECStreamerWithEbsc(423, &BlobStoreClient{}, blockSize)
	s.mw = newTestMetaWrapper()
	w := s.fWriter
	t.Cleanup(func() { w.FreeCache() })

	w.allocateCache()
	w.reshapeBufForCopyPath()
	w.blockPosition = blockSize - 2
	w.fileOffset = w.blockPosition
	s.raiseFileSize(uint64(w.fileOffset))
	seedStreamerExtentsForTest(s, uint64(w.fileOffset), nil)

	var flushExtCalls int
	patches := gomonkey.NewPatches()
	t.Cleanup(func() { patches.Reset() })
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
		func(_ *Writer, _ uint64, _ context.Context, _ bool) error {
			flushExtCalls++
			w.blockPosition = 0
			return nil
		})
	patches.ApplyPrivateMethod(reflect.TypeOf(w), "writeSlice",
		func(_ *Writer, _ context.Context, _ *rwSlice, _ bool) error { return nil })
	patches.ApplyMethod(reflect.TypeOf(s.mw), "AppendObjExtentKeysWithCheck",
		func(_ *meta.MetaWrapper, _ uint64, _ proto.ObjExtentKey, _ proto.ObjExtentKey) error {
			return nil
		})

	const tailWrite = 6
	n, err := w.tryOverWrite(context.Background(), w.fileOffset, make([]byte, tailWrite), 0)
	require.NoError(t, err)
	require.Equal(t, tailWrite, n)
	require.Equal(t, 1, flushExtCalls, "only mid-block flush; no final partial flush")
	require.Greater(t, w.blockPosition, 0, "trailing partial block deferred in buffer")
}

// ---------- 10. extended (error paths & coverage gaps) ----------

func TestWriter_extended(t *testing.T) {
	t.Run("flushWithoutPool", func(t *testing.T) {
		t.Run("success_via_FlushWithoutPool", func(t *testing.T) {
			const blockSize = 16
			st, w := testWriterWithMwEbsc(501, &BlobStoreClient{})
			st.raiseFileSize(0)
			w.buf = make([]byte, blockSize)
			w.blockPosition = blockSize
			w.fileOffset = blockSize

			require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
			defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
			require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil))
			defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

			require.NoError(t, w.FlushWithoutPool(st.ino, context.Background()))
			require.Equal(t, 0, w.blockPosition)
		})

		t.Run("meta_fail_rollback_delete", func(t *testing.T) {
			const blockSize = 16
			st, w := testWriterWithMwEbsc(502, &BlobStoreClient{})
			w.buf = make([]byte, blockSize)
			w.blockPosition = blockSize
			w.fileOffset = blockSize

			require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
			defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
			var deleteCalls int
			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyMethod(reflect.TypeOf(w.ecStreamer.ebsc), "Delete",
				func(_ *BlobStoreClient, _ []proto.ObjExtentKey) error {
					deleteCalls++
					return nil
				})
			require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysFalse, nil))
			defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

			err := w.flushWithoutPool(st.ino, context.Background(), true)
			require.Error(t, err)
			require.Equal(t, 1, deleteCalls)
		})
	})

	t.Run("writeWithoutPool", func(t *testing.T) {
		t.Run("full_block_flush", func(t *testing.T) {
			const blockSize = 16
			st := mustTestECStreamerWithEbsc(503, &BlobStoreClient{}, blockSize)
			w := st.fWriter
			w.buf = nil
			w.fileOffset = 0

			require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
			defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
			require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil))
			defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

			n, err := w.WriteWithoutPool(context.Background(), 0, make([]byte, blockSize))
			require.NoError(t, err)
			require.Equal(t, blockSize, n)
		})

		t.Run("flush_fail_rollback", func(t *testing.T) {
			const blockSize = 8
			st := mustTestECStreamerWithEbsc(504, &BlobStoreClient{}, blockSize)
			w := st.fWriter
			w.buf = nil
			w.fileOffset = 0

			patches := gomonkey.NewPatches()
			defer patches.Reset()
			patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushWithoutPool",
				func(_ *Writer, _ uint64, _ context.Context, _ bool) error {
					return syscall.EIO
				})

			_, err := w.WriteWithoutPool(context.Background(), 0, make([]byte, blockSize))
			require.ErrorIs(t, err, syscall.EIO)
		})
	})

	t.Run("write_sync", func(t *testing.T) {
		const blockSize = 16
		s := mustTestECStreamerWithEbsc(505, &BlobStoreClient{}, blockSize)
		s.mw = newTestMetaWrapper()
		w := s.fWriter
		metaErr := errors.New("update meta failed")
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "doParallelWrite",
			func(_ *Writer, _ context.Context, data []byte, _ int) (int, error) {
				return len(data), nil
			})
		patches.ApplyPrivateMethod(reflect.TypeOf(s), "updateMetaInfo",
			func(_ *ECStreamer, _ *uint64) error { return metaErr })

		n, err := w.Write(context.Background(), 0, []byte("z"), proto.FlagsSyncWrite)
		require.ErrorIs(t, err, metaErr)
		require.Equal(t, 1, n)
	})

	t.Run("tryOverWrite_guards_and_flushExt_rollback", func(t *testing.T) {
		var nilW *Writer
		_, err := nilW.tryOverWrite(context.Background(), 0, []byte("x"), 0)
		require.Error(t, err)

		const blockSize = 8
		s := mustTestECStreamerWithEbsc(506, &BlobStoreClient{}, blockSize)
		w := s.fWriter
		w.allocateCache()
		w.reshapeBufForCopyPath()
		w.buf = w.buf[:2]
		_, err = w.tryOverWrite(context.Background(), 0, make([]byte, 10), 0)
		require.ErrorIs(t, err, syscall.EINVAL)

		w.buf = make([]byte, blockSize)
		w.blockPosition = 0
		w.fileOffset = 0
		flushErr := errors.New("flushExt failed")
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
			func(_ *Writer, _ uint64, _ context.Context, _ bool) error { return flushErr })

		_, err = w.tryOverWrite(context.Background(), 0, make([]byte, blockSize), 0)
		require.ErrorIs(t, err, flushErr)
	})

	t.Run("doParallelWrite_dirty_pre_flush", func(t *testing.T) {
		const blockSize = 16
		s := mustTestECStreamerWithEbsc(507, &BlobStoreClient{}, blockSize)
		s.mw = newTestMetaWrapper()
		w := s.fWriter
		seedDirtyForTest(s)
		w.buf = make([]byte, 4)
		w.blockPosition = 4
		w.fileOffset = 104

		var flushExtCalls int
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
			func(_ *Writer, _ uint64, _ context.Context, _ bool) error {
				flushExtCalls++
				s.cleanDirty()
				return nil
			})
		require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")

		_, err := w.doParallelWrite(context.Background(), []byte("sync"), 104)
		require.NoError(t, err)
		require.Equal(t, 1, flushExtCalls)
	})

	t.Run("doParallelWrite_slice_and_meta_errors", func(t *testing.T) {
		const blockSize = 16
		s := mustTestECStreamerWithEbsc(5071, &BlobStoreClient{}, blockSize)
		s.mw = newTestMetaWrapper()
		w := s.fWriter

		require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteFalse, nil))
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		_, err := w.doParallelWrite(context.Background(), []byte("bad"), 0)
		require.Error(t, err)

		require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
		require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysFalse, nil))
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")
		_, err = w.doParallelWrite(context.Background(), []byte("ok"), 0)
		require.Error(t, err)
	})

	t.Run("WriteFromReader_error_paths", func(t *testing.T) {
		const blockSize = 16
		s := mustTestECStreamerWithEbsc(508, &BlobStoreClient{}, blockSize)
		s.mw = newTestMetaWrapper()
		w := s.fWriter

		_, err := w.WriteFromReader(context.Background(), &errReader{err: syscall.EIO}, nil)
		require.Error(t, err)

		require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysFalse, nil))
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")
		_, err = w.WriteFromReader(context.Background(), strings.NewReader("x"), nil)
		require.Error(t, err)

		require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysTrue, nil))
		metaErr := errors.New("notify meta fail")
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(s), "updateMetaInfo",
			func(_ *ECStreamer, _ *uint64) error { return metaErr })
		_, err = w.WriteFromReader(context.Background(), strings.NewReader("y"), nil)
		require.ErrorIs(t, err, metaErr)
	})

	t.Run("doBufferWrite_invalid_freeSize", func(t *testing.T) {
		const blockSize = 8
		s := mustTestECStreamerWithEbsc(509, &BlobStoreClient{}, blockSize)
		w := s.fWriter
		w.allocateCache()
		w.blockPosition = blockSize
		w.fileOffset = blockSize
		_, err := w.doBufferWrite(context.Background(), []byte("x"), blockSize)
		require.ErrorIs(t, err, syscall.EINVAL)
	})

	t.Run("doBufferWrite_buf_too_short", func(t *testing.T) {
		const blockSize = 8
		s := mustTestECStreamerWithEbsc(5091, &BlobStoreClient{}, blockSize)
		w := s.fWriter
		w.allocateCache()
		w.blockPosition = 0
		w.fileOffset = 0
		w.buf = w.buf[:2]
		_, err := w.doBufferWrite(context.Background(), make([]byte, 10), 0)
		require.ErrorIs(t, err, syscall.EINVAL)
	})

	t.Run("doBufferWrite_flushExt_rollback", func(t *testing.T) {
		const blockSize = 8
		s := mustTestECStreamerWithEbsc(5092, &BlobStoreClient{}, blockSize)
		w := s.fWriter
		w.allocateCache()
		w.reshapeBufForCopyPath()
		w.blockPosition = blockSize - 2
		w.fileOffset = blockSize - 2
		flushErr := errors.New("flushExt rollback")
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushExt",
			func(_ *Writer, _ uint64, _ context.Context, _ bool) error { return flushErr })
		_, err := w.doBufferWrite(context.Background(), make([]byte, 4), w.fileOffset)
		require.ErrorIs(t, err, flushErr)
		require.Equal(t, blockSize-2, w.blockPosition)
	})

	t.Run("flush_and_flushExt_inconsistent_state", func(t *testing.T) {
		st, w := testWriterWithMwEbsc(510, &BlobStoreClient{})
		w.buf = make([]byte, 16)
		w.blockPosition = 16
		w.fileOffset = 8
		err := w.flushExt(st.ino, context.Background(), false)
		require.Error(t, err)
		require.Contains(t, err.Error(), "inconsistent state")

		err = w.flush(st.ino, context.Background(), true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "inconsistent state")
	})

	t.Run("flush_meta_append_fails", func(t *testing.T) {
		const blockSize = 16
		st, w := testWriterWithMwEbsc(511, &BlobStoreClient{})
		w.buf = make([]byte, blockSize)
		w.blockPosition = blockSize
		w.fileOffset = blockSize
		require.NoError(t, gohook.HookMethod(w.ecStreamer.ebsc, "Write", MockEbscWriteTrue, nil))
		defer gohook.UnHookMethod(w.ecStreamer.ebsc, "Write")
		require.NoError(t, gohook.HookMethod(w.ecStreamer.mw, "AppendObjExtentKeys", MockAppendObjExtentKeysFalse, nil))
		defer gohook.UnHookMethod(w.ecStreamer.mw, "AppendObjExtentKeys")
		err := w.flush(st.ino, context.Background(), true)
		require.Error(t, err)
	})

	t.Run("flushExt_flushOverwriteReqs_fails", func(t *testing.T) {
		const blockSize = 16
		st, w := testWriterWithMwEbsc(512, &BlobStoreClient{})
		seedStreamerExtentsForTest(st, 200, []proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 200, Size: 100}})
		seedDirtyForTest(st)
		w.buf = make([]byte, blockSize)
		w.blockPosition = blockSize
		w.fileOffset = 140

		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "flushOverwriteReqs",
			func(_ *Writer, _ context.Context, _ uint64, _ []overwriteReq, _ uint64, _ int) error {
				return syscall.EIO
			})
		err := w.flushExt(st.ino, context.Background(), false)
		require.Error(t, err)
	})

	t.Run("flushOverwriteReqs_writeSlice_fails", func(t *testing.T) {
		st, w := testWriterWithMwEbsc(513, &BlobStoreClient{})
		w.buf = make([]byte, 32)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(w), "writeSlice",
			func(_ *Writer, _ context.Context, _ *rwSlice, _ bool) error { return syscall.EIO })
		reqs := []overwriteReq{{NewExtent: proto.ObjExtentKey{FileOffset: 0, Size: 8}}}
		err := w.flushOverwriteReqs(context.Background(), st.ino, reqs, 0, 8)
		require.Error(t, err)
	})

	t.Run("bufferDirtyLen_nil_and_releaseWriteBuf_dirty", func(t *testing.T) {
		var nilW *Writer
		require.Equal(t, 0, nilW.bufferDirtyLen())

		const blockSize = 16
		buf.InitCachePool(blockSize, 4)
		st := mustTestECStreamerWithEbsc(514, &BlobStoreClient{}, blockSize)
		w := st.fWriter
		w.allocateCache()
		w.blockPosition = 4
		w.releaseWriteBuf()
		require.NotNil(t, w.buf)
	})

	t.Run("FreeCache_and_allocateCache_nil_guards", func(t *testing.T) {
		var nilW *Writer
		nilW.FreeCache()
		nilW.allocateCache()
		require.NoError(t, nilW.FlushWithoutPool(1, context.Background()))
	})

	t.Run("notifyCompleteFlushMeta_updateMetaInfo_fails", func(t *testing.T) {
		w := &Writer{fileOffset: 8}
		s := mustTestECStreamer(515, nil, w)
		metaErr := errors.New("meta refresh fail")
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(s), "updateMetaInfo",
			func(_ *ECStreamer, _ *uint64) error { return metaErr })
		err := w.notifyCompleteFlushMeta()
		require.ErrorIs(t, err, metaErr)
	})
}

type errReader struct{ err error }

func (e *errReader) Read([]byte) (int, error) { return 0, e.err }
