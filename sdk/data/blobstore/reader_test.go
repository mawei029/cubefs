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
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
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

// reader_test.go layout (top → bottom):
//
//  1. fixtures     — newSafeBlobStoreClientForTest, EBS/bcache/meta mocks
//  2. lifecycle    — TestReader_lifecycle
//  3. slice        — TestReader_slice (logicalReadBound, prepareEbsSlice)
//  4. streamer     — TestReader_streamer (fileSizeView / writer tail)
//  5. read         — TestReader_read (prefetch, past EOF, clamp beyond EOF, short read)
//  6. bcache       — TestReader_bcache
//  7. prefetch     — TestReader_prefetch (limiter, ensurePrefetchBuf)
//  8. incremental  — TestReader_incremental_last2commits (recent commit coverage)

// ---------- 1. fixtures: test client & mocks ----------

func newSafeBlobStoreClientForTest() *BlobStoreClient {
	return &BlobStoreClient{
		maxTimeoutSec: EbsMaxTimeout,
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
	n := int(size)
	if n > len(buf) {
		n = len(buf)
	}
	// Repeat a short pattern so mocks succeed for prefetch fetch sizes (>>12 bytes).
	pattern := []byte("Hello world.")
	for i := 0; i < n; i++ {
		buf[i] = pattern[i%len(pattern)]
	}
	return n, nil
}

func MockEbscReadFalse(ebsc *BlobStoreClient, ctx context.Context, volName string,
	buf []byte, offset uint64, size uint64,
	oek proto.ObjExtentKey,
) (readN int, err error) {
	return 0, syscall.EIO
}

// MockEbscReadShort always returns at most 8 bytes with err=nil to exercise readSliceRange short-read check.
func MockEbscReadShort(ebsc *BlobStoreClient, ctx context.Context, volName string,
	buf []byte, offset uint64, size uint64,
	oek proto.ObjExtentKey,
) (readN int, err error) {
	n := 8
	if n > len(buf) {
		n = len(buf)
	}
	if uint64(n) > size {
		n = int(size)
	}
	for i := 0; i < n; i++ {
		buf[i] = 's'
	}
	return n, nil
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

// ---------- 2. lifecycle ----------

func TestReader_lifecycle(t *testing.T) {
	t.Run("NewReader", func(t *testing.T) {
		reader := NewReader(ClientConfig{
			VolName:      "cfs",
			VolType:      0,
			BlockSize:    0,
			Ino:          2,
			LimitManager: newTestLimitManager(),
			EnableBcache: false,
			ECStreamer:   mustTestECStreamer(2, nil, nil),
		})
		assert.NotEmpty(t, reader, nil)
	})

	t.Run("String_and_nil_Read", func(t *testing.T) {
		s := mustTestECStreamer(90, nil, nil)
		r := s.Reader()
		require.Contains(t, r.String(), "Reader{")
		var nilR *Reader
		_, err := nilR.Read(context.Background(), []byte{1}, 0, 1)
		require.Error(t, err)
	})
}

// ---------- 3. slice ----------

func TestReader_slice(t *testing.T) {
	t.Run("logicalReadBound", func(t *testing.T) {
		for _, tc := range []struct {
			metaSize   uint64
			objEks     []proto.ObjExtentKey
			expectSize uint64
		}{
			{0, nil, 0},
			{0, []proto.ObjExtentKey{{Size: 100, FileOffset: 100}}, 200},
			{100, []proto.ObjExtentKey{{FileOffset: 20, Size: 20}}, 100},
			{100, nil, 100},
		} {
			assert.Equal(t, tc.expectSize, logicalReadBound(tc.metaSize, tc.objEks))
		}
	})

	t.Run("prepareEbsSlice_sparse_holes", func(t *testing.T) {
		s := mustTestECStreamer(0, nil, nil)
		seedStreamerExtentsForTest(s, 100, []proto.ObjExtentKey{{FileOffset: 20, Size: 20}})
		r := &Reader{limitManager: manager.NewLimitManager(nil), ecStreamer: s}
		slices, _, err := r.prepareEbsSlice(0, 100, 100, make([]byte, 100))
		require.NoError(t, err)
		require.Len(t, slices, 3)
		require.True(t, slices[0].hole)
		require.False(t, slices[1].hole)
		require.True(t, slices[2].hole)
	})
}

// ---------- 4. streamer ----------

func TestReader_streamer(t *testing.T) {
	t.Run("fileSizeViewLocked_with_writer_tail", func(t *testing.T) {
		s := mustTestECStreamer(1, nil, nil)
		SeedLogicalViewForTest(s, 100, 1)
		s.fWriter.fileOffset = 500
		s.mu.Lock()
		require.Equal(t, uint64(500), s.fileSizeViewLocked())
		s.mu.Unlock()
	})
	t.Run("FileSizeView_includes_writer_tail", func(t *testing.T) {
		s := mustTestECStreamer(2, nil, nil)
		SeedLogicalViewForTest(s, 100, 1)
		s.fWriter.fileOffset = 500
		sz, gen := s.FileSizeView()
		require.Equal(t, 500, sz)
		require.Equal(t, uint64(1), gen)
	})
}

// ---------- 5. read (prefetch, past EOF, clamp beyond EOF, short read) ----------

func TestReader_read(t *testing.T) {
	t.Run("prefetch_and_past_eof", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(701, ebsc, 32)
		seedStreamerExtentsForTest(s, 0, []proto.ObjExtentKey{{FileOffset: 0, Size: 50}})
		r := s.fReader
		r.readConcurrency = 1
		r.aheadReadEnable = true
		r.minReadAheadSize = 0
		r.preReadLimiter = &blobPreReadLimiter{maxBytes: 512}

		err := gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil)
		require.NoError(t, err)
		defer gohook.UnHookMethod(ebsc, "Read")

		n, err := r.Read(context.Background(), make([]byte, 3), 0, 3)
		require.NoError(t, err)
		require.Equal(t, 3, n)

		n, err = r.Read(context.Background(), make([]byte, 2), 60, 2)
		require.NoError(t, err)
		require.Equal(t, 0, n)
	})

	// Request 20B on a 10B file: clamp size before EBS; return n=10, err=nil (not io.EOF).
	t.Run("clamp_beyond_eof", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(702, ebsc, 32)
		seedStreamerExtentsForTest(s, 10, []proto.ObjExtentKey{{FileOffset: 0, Size: 10}})
		r := s.fReader
		r.readConcurrency = 1
		r.aheadReadEnable = false
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")

		// read from offset 0
		buf := make([]byte, 20)
		for i := range buf {
			buf[i] = byte(i%10) + '0'
		}
		n, err := r.Read(context.Background(), buf, 0, 20)
		require.NoError(t, err)
		require.Equal(t, 10, n)
		require.Equal(t, "Hello worl", string(buf[0:10]))
		require.Equal(t, "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00", string(buf[10:20]))
		require.Equal(t, byte(0), buf[19])

		// read from offset 2
		buf = make([]byte, 20)
		for i := range buf {
			buf[i] = byte(i%10) + 'a'
		}
		// fmt.Println("buf:", buf)
		n, err = r.Read(context.Background(), buf, 2, 20)
		require.NoError(t, err)
		require.Equal(t, 10-2, n)
		require.Equal(t, "Hello wo", string(buf[0:8]))
		require.Equal(t, "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00", string(buf[8:20]))
		require.Equal(t, byte(0), buf[19])
		// fmt.Println("buf:", buf)
	})

	// After clamp, Ebsc returns fewer bytes than rSize → short-read error (not silent truncate).
	t.Run("ebsc_short_read_errors", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(703, ebsc, 32)
		seedStreamerExtentsForTest(s, 10, []proto.ObjExtentKey{{FileOffset: 0, Size: 10}})
		r := s.fReader
		r.readConcurrency = 1
		r.aheadReadEnable = false
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadShort, nil))
		defer gohook.UnHookMethod(ebsc, "Read")

		_, err := r.Read(context.Background(), make([]byte, 10), 0, 10)
		require.Error(t, err)
		require.Contains(t, err.Error(), "short read")
		require.Contains(t, err.Error(), "want(10)")
		require.Contains(t, err.Error(), "got(8)")
	})
}

// ---------- 6. bcache ----------

func TestReader_bcache(t *testing.T) {
	t.Run("needCacheL1", func(t *testing.T) {
		for _, tc := range []struct {
			enableCache bool
			expectCache bool
		}{
			{false, false},
			{true, true},
		} {
			reader := Reader{}
			reader.limitManager = manager.NewLimitManager(nil)
			reader.enableBcache = tc.enableCache
			assert.Equal(t, tc.expectCache, reader.needCacheL1())
		}
	})
}

// ---------- 7. prefetch ----------

func TestReader_prefetch(t *testing.T) {
	t.Run("limiter_singleton_and_acquire", func(t *testing.T) {
		require.Nil(t, getBlobPreReadLimiter(0))
		require.Nil(t, getBlobPreReadLimiter(-1))

		first := getBlobPreReadLimiter(1024)
		require.NotNil(t, first)
		require.Equal(t, int64(1024), first.maxBytes)

		second := getBlobPreReadLimiter(2048)
		require.Same(t, first, second)

		l := &blobPreReadLimiter{maxBytes: 8}
		assert.True(t, l.tryAcquire(4))
		assert.False(t, l.tryAcquire(5))
		l.release(2)
		assert.True(t, l.tryAcquire(4))
	})

	t.Run("ensurePrefetchBuf", func(t *testing.T) {
		s := mustTestECStreamerWithEbsc(1, nil, 0)
		r := &Reader{ecStreamer: s, preReadLimiter: &blobPreReadLimiter{maxBytes: 4}}
		assert.False(t, r.ensurePrefetchBuf())

		s2 := mustTestECStreamerWithEbsc(2, nil, 16)
		r2 := &Reader{ecStreamer: s2, aheadWindowCnt: 1, prefetchCap: 16, preReadLimiter: &blobPreReadLimiter{maxBytes: 64}}
		assert.True(t, r2.ensurePrefetchBuf())
	})
}

// ---------- 8. incremental ----------

func TestReader_incremental_last2commits(t *testing.T) {
	t.Run("readerBytePool_edges", func(t *testing.T) {
		const maxCap = 16 << 20
		require.Nil(t, readerGetBuf(0, maxCap))

		readerPutBuf(nil, maxCap)
		oversized := make([]byte, maxCap+1)
		readerPutBuf(oversized, maxCap)

		b1 := readerGetBuf(64, maxCap)
		require.Equal(t, 64, len(b1))
		require.LessOrEqual(t, cap(b1), maxCap)
		readerPutBuf(b1, maxCap)
		b2 := readerGetBuf(64, maxCap)
		require.Equal(t, 64, len(b2))
		require.Equal(t, cap(b1), cap(b2))
		readerPutBuf(b2, maxCap)

		readerPutBuf(make([]byte, 8, 16), maxCap)
		b3 := readerGetBuf(32, maxCap)
		require.Equal(t, 32, len(b3))
		require.LessOrEqual(t, cap(b3), maxCap)

		big := readerGetBuf(maxCap+1, maxCap)
		require.Equal(t, maxCap+1, len(big))
		require.Greater(t, cap(big), maxCap)
		readerPutBuf(big, maxCap)
	})

	t.Run("readerReleasePrefetchReadBuf_pooled", func(t *testing.T) {
		s := mustTestECStreamerWithEbsc(501, nil, 16)
		r := &Reader{ecStreamer: s, readBuf: make([]byte, 16), prefetchCap: 16, bufValidLen: 4}
		readerReleasePrefetchReadBuf(r)
		require.Nil(t, r.readBuf)
	})

	t.Run("NewReader_aheadWindowCnt_and_prefetch_cap", func(t *testing.T) {
		s := mustTestECStreamerWithEbsc(502, nil, 32)
		r := NewReader(ClientConfig{ECStreamer: s, AheadWindowCnt: -2, MinReadAheadSize: -1})
		require.Equal(t, 1, r.aheadWindowCnt)
		require.Equal(t, 0, int(r.minReadAheadSize))
		require.Equal(t, 32, r.prefetchCap)
		require.Equal(t, 32, r.prefetchBufCap())

		r2 := NewReader(ClientConfig{ECStreamer: s, AheadWindowCnt: 4})
		require.Equal(t, 4, r2.aheadWindowCnt)
		require.Equal(t, 128, r2.prefetchCap)
		require.Equal(t, r2.prefetchCap, r2.prefetchBufCap())
	})

	t.Run("prepareEbsSlice_and_readEbsRange", func(t *testing.T) {
		s := mustTestECStreamer(503, nil, nil)
		r := &Reader{ecStreamer: s, readConcurrency: 1}

		_, _, err := r.prepareEbsSlice(-1, 8, 8, make([]byte, 8))
		require.ErrorIs(t, err, syscall.EIO)

		_, readSize, err := r.prepareEbsSlice(16, 8, 8, make([]byte, 8))
		require.NoError(t, err)
		require.Equal(t, uint32(0), readSize)

		_, _, err = r.prepareEbsSlice(0, 16, 16, make([]byte, 8))
		require.ErrorIs(t, err, syscall.EIO)

		dst := make([]byte, 32)
		seedStreamerExtentsForTest(s, 32, nil)
		n, err := r.readEbsRange(context.Background(), 0, 32, 32, dst)
		require.NoError(t, err)
		require.Equal(t, 32, n)
		for i := range dst {
			require.Equal(t, byte(0), dst[i])
		}

		_, err = r.readEbsRange(context.Background(), 0, 16, 16, make([]byte, 4))
		require.ErrorIs(t, err, syscall.EIO)

		n, err = r.readEbsRange(context.Background(), 16, 8, 8, make([]byte, 8))
		require.NoError(t, err)
		require.Equal(t, 0, n)

		_, err = r.readEbsRange(context.Background(), -1, 8, 8, make([]byte, 8))
		require.ErrorIs(t, err, syscall.EIO)
	})

	t.Run("readEbsRange_zero_slices_zeroes_dst", func(t *testing.T) {
		s := mustTestECStreamer(5031, nil, nil)
		r := &Reader{ecStreamer: s, readConcurrency: 1}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(r), "prepareEbsSlice",
			func(_ *Reader, _ int, _ uint32, _ uint64, dst []byte) ([]*rwSlice, uint32, error) {
				return nil, 8, nil
			})
		dst := make([]byte, 8)
		n, err := r.readEbsRange(context.Background(), 0, 8, 16, dst)
		require.NoError(t, err)
		require.Equal(t, 8, n)
		for i := range dst {
			require.Equal(t, byte(0), dst[i])
		}
	})

	t.Run("readEbsRange_single_slice_sync", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(5032, ebsc, 16)
		seedStreamerExtentsForTest(s, 16, []proto.ObjExtentKey{{FileOffset: 0, Size: 16}})
		r := &Reader{ecStreamer: s, readConcurrency: 1}
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")

		dst := make([]byte, 8)
		n, err := r.readEbsRange(context.Background(), 0, 8, 16, dst)
		require.NoError(t, err)
		require.Equal(t, 8, n)
	})

	t.Run("readEbsRange_single_slice_error", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(5033, ebsc, 16)
		seedStreamerExtentsForTest(s, 16, []proto.ObjExtentKey{{FileOffset: 0, Size: 16}})
		r := &Reader{ecStreamer: s, readConcurrency: 1}
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadFalse, nil))
		defer gohook.UnHookMethod(ebsc, "Read")

		_, err := r.readEbsRange(context.Background(), 0, 8, 16, make([]byte, 8))
		require.ErrorIs(t, err, syscall.EIO)
	})

	t.Run("readSliceRange_hole_and_bcache_miss", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(504, ebsc, 0)
		r := &Reader{
			limitManager: manager.NewLimitManager(nil),
			ecStreamer:   s,
			enableBcache: true,
			bc:           &bcache.BcacheClient{},
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(r.bc), "Get", MockGetFalse)

		hole := make([]byte, 4)
		errCh := make(chan error, 1)
		require.NoError(t, r.readSliceRange(context.Background(), &rwSlice{hole: true, rSize: 4, Data: hole}, errCh))
		require.NoError(t, <-errCh)
		for i := range hole {
			require.Equal(t, byte(0), hole[i])
		}

		r.enableBcache = false
		data := make([]byte, 11)
		errCh = make(chan error, 1)
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")
		require.NoError(t, r.readSliceRange(context.Background(), &rwSlice{rSize: 11, Data: data, objExtentKey: proto.ObjExtentKey{Size: 11}}, errCh))
		require.NoError(t, <-errCh)
	})

	t.Run("readSliceRange_bcache_hit", func(t *testing.T) {
		s := mustTestECStreamerWithEbsc(5041, nil, 0)
		r := &Reader{
			ecStreamer:   s,
			enableBcache: true,
			bc:           &bcache.BcacheClient{},
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(r.bc), "Get", MockGetTrue)
		data := make([]byte, 8)
		errCh := make(chan error, 1)
		require.NoError(t, r.readSliceRange(context.Background(), &rwSlice{rSize: 8, Data: data, objExtentKey: proto.ObjExtentKey{Size: 8}}, errCh))
		require.NoError(t, <-errCh)
	})

	t.Run("readSliceRange_bcache_miss_spawns_async_cache", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(5042, ebsc, 0)
		r := &Reader{
			ecStreamer:   s,
			enableBcache: true,
			bc:           &bcache.BcacheClient{},
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(r.bc), "Get", MockGetFalse)
		var asyncDone sync.WaitGroup
		asyncDone.Add(1)
		patches.ApplyPrivateMethod(reflect.TypeOf(r), "asyncCache",
			func(_ *Reader, _ context.Context, _ string, _ proto.ObjExtentKey) {
				defer asyncDone.Done()
			})
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")
		data := make([]byte, 11)
		errCh := make(chan error, 1)
		require.NoError(t, r.readSliceRange(context.Background(), &rwSlice{rSize: 11, Data: data, objExtentKey: proto.ObjExtentKey{Size: 11}}, errCh))
		require.NoError(t, <-errCh)
		asyncDone.Wait()
	})

	t.Run("readSliceRange_ebsc_no_bcache", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(12407, ebsc, 0)
		r := &Reader{
			limitManager: manager.NewLimitManager(nil),
			ecStreamer:   s,
			enableBcache: false,
		}
		errCh := make(chan error, 1)
		rs := &rwSlice{rSize: 11, Data: make([]byte, 11)}
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")
		require.NoError(t, r.readSliceRange(context.Background(), rs, errCh))
		require.NoError(t, <-errCh)
	})

	t.Run("readEbsRange_parallel_ok", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(505, ebsc, 16)
		seedStreamerExtentsForTest(s, 16, []proto.ObjExtentKey{
			{FileOffset: 0, Size: 8},
			{FileOffset: 8, Size: 8},
		})
		r := &Reader{ecStreamer: s, readConcurrency: 2}
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")

		dst := make([]byte, 16)
		n, err := r.readEbsRange(context.Background(), 0, 16, 16, dst)
		require.NoError(t, err)
		require.Equal(t, 16, n)
	})

	t.Run("readEbsRange_parallel_error", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(5051, ebsc, 16)
		seedStreamerExtentsForTest(s, 16, []proto.ObjExtentKey{
			{FileOffset: 0, Size: 8},
			{FileOffset: 8, Size: 8},
		})
		r := &Reader{ecStreamer: s, readConcurrency: 2}
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadFalse, nil))
		defer gohook.UnHookMethod(ebsc, "Read")
		_, err := r.readEbsRange(context.Background(), 0, 16, 16, make([]byte, 16))
		require.ErrorIs(t, err, syscall.EIO)
	})

	t.Run("Read_prefetch_hit_and_fallback", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(506, ebsc, 16)
		seedStreamerExtentsForTest(s, 64, []proto.ObjExtentKey{{FileOffset: 0, Size: 64}})
		r := s.fReader
		r.readConcurrency = 1
		r.aheadReadEnable = true
		r.aheadWindowCnt = 2
		r.prefetchCap = 32
		r.minReadAheadSize = 0
		r.preReadLimiter = &blobPreReadLimiter{maxBytes: 512}
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")

		buf := make([]byte, 3)
		n, err := r.Read(context.Background(), buf, 0, 3)
		require.NoError(t, err)
		require.Equal(t, 3, n)

		big := make([]byte, 20)
		n, err = r.Read(context.Background(), big, 0, 20)
		require.NoError(t, err)
		require.Equal(t, 20, n)

		r.bufBaseOff = 0
		r.bufValidLen = 8
		small := make([]byte, 12)
		n, err = r.Read(context.Background(), small, 0, 12)
		require.NoError(t, err)
		require.Equal(t, 12, n)
	})

	t.Run("Read_prefetch_extend_buf", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(507, ebsc, 16)
		seedStreamerExtentsForTest(s, 100, []proto.ObjExtentKey{{FileOffset: 0, Size: 100}})
		r := s.fReader
		r.readConcurrency = 1
		r.aheadReadEnable = true
		r.aheadWindowCnt = 4
		r.prefetchCap = 64
		r.minReadAheadSize = 0
		r.preReadLimiter = &blobPreReadLimiter{maxBytes: 512}
		r.readBuf = make([]byte, 8)
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyPrivateMethod(reflect.TypeOf(r), "ensurePrefetchBuf", func(_ *Reader) bool { return true })

		n, err := r.Read(context.Background(), make([]byte, 4), 0, 4)
		require.NoError(t, err)
		require.Equal(t, 4, n)
		require.GreaterOrEqual(t, cap(r.readBuf), 64)
	})

	t.Run("asyncCache_uses_pooled_buf", func(t *testing.T) {
		ebsc := newSafeBlobStoreClientForTest()
		s := mustTestECStreamerWithEbsc(508, ebsc, 0)
		r := &Reader{
			ecStreamer:   s,
			enableBcache: true,
			bc:           &bcache.BcacheClient{},
			prefetchCap:  16 << 20,
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		require.NoError(t, gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil))
		defer gohook.UnHookMethod(ebsc, "Read")
		patches.ApplyMethod(reflect.TypeOf(r.bc), "Put", MockPutTrue)

		r.asyncCache(context.Background(), "cache-key", proto.ObjExtentKey{Size: 16})
	})

	t.Run("releasePrefetchCache_pooled", func(t *testing.T) {
		l := &blobPreReadLimiter{maxBytes: 128}
		require.True(t, l.tryAcquire(32))
		s := mustTestECStreamerWithEbsc(509, nil, 16)
		r := &Reader{
			ecStreamer:       s,
			preReadLimiter:   l,
			readBuf:          make([]byte, 32),
			prefetchCap:      32,
			prefetchReserved: 32,
		}
		r.releasePrefetchCache()
		require.Nil(t, r.readBuf)
		require.Equal(t, int64(0), r.prefetchReserved)
		require.Equal(t, int64(0), atomic.LoadInt64(&l.usedBytes))
	})
}
