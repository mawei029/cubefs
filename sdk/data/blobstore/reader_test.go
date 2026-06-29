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
		ECStreamer:      mustTestECStreamer(2, nil, nil),
	}

	reader := NewReader(mockConfig)
	assert.NotEmpty(t, reader, nil)
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

func Test_logicalReadBound(t *testing.T) {
	testCase := []struct {
		metaSize   uint64
		objEks     []proto.ObjExtentKey
		expectSize uint64
	}{
		{0, nil, 0},
		{0, []proto.ObjExtentKey{{Size: 100, FileOffset: 100}}, 200},
		{100, []proto.ObjExtentKey{{FileOffset: 20, Size: 20}}, 100},
		{100, nil, 100},
	}
	for _, tc := range testCase {
		assert.Equal(t, tc.expectSize, logicalReadBound(tc.metaSize, tc.objEks))
	}
}

func TestECStreamer_fileSizeViewLocked_with_writer_tail(t *testing.T) {
	s := mustTestECStreamer(1, nil, nil)
	SeedLogicalViewForTest(s, 100, 1)
	w := s.fWriter
	w.fileOffset = 500
	s.mu.Lock()
	require.Equal(t, uint64(500), s.fileSizeViewLocked())
	s.mu.Unlock()
}

func TestECStreamer_FileSizeView_includes_writer_tail(t *testing.T) {
	s := mustTestECStreamer(2, nil, nil)
	SeedLogicalViewForTest(s, 100, 1)
	s.fWriter.fileOffset = 500
	sz, gen := s.FileSizeView()
	require.Equal(t, 500, sz)
	require.Equal(t, uint64(1), gen)
}

func TestPrepareEbsSlice_sparseHeadMiddleTailHoles(t *testing.T) {
	s := mustTestECStreamer(0, nil, nil)
	seedStreamerExtentsForTest(s, 100, []proto.ObjExtentKey{{FileOffset: 20, Size: 20}})
	r := &Reader{limitManager: manager.NewLimitManager(nil), ecStreamer: s}
	slices, err := r.prepareEbsSlice(0, 100, 100)
	require.NoError(t, err)
	require.Len(t, slices, 3)
	require.True(t, slices[0].hole)
	require.False(t, slices[1].hole)
	require.True(t, slices[2].hole)
}

func TestReader_Read_prefetch_and_past_eof(t *testing.T) {
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
}

func TestNeedCacheL1(t *testing.T) {
	testCase := []struct {
		enableCache bool
		expectCache bool
	}{
		{false, false},
		{true, true},
	}
	for _, tc := range testCase {
		reader := Reader{}
		reader.limitManager = manager.NewLimitManager(nil)
		reader.enableBcache = tc.enableCache
		assert.Equal(t, tc.expectCache, reader.needCacheL1())
	}
}

func TestReader_releasePrefetchCache(t *testing.T) {
	l := &blobPreReadLimiter{maxBytes: 128}
	require.True(t, l.tryAcquire(32))
	s := mustTestECStreamerWithEbsc(88, nil, 16)
	r := &Reader{
		ecStreamer:       s,
		preReadLimiter:   l,
		readBuf:          make([]byte, 32),
		prefetchReserved: 32,
	}
	r.releasePrefetchCache()
	require.Nil(t, r.readBuf)
	require.Equal(t, int64(0), r.prefetchReserved)
	require.Equal(t, int64(0), atomic.LoadInt64(&l.usedBytes))
	r.releasePrefetchCache() // idempotent
}

func TestGetBlobPreReadLimiter_branches(t *testing.T) {
	require.Nil(t, getBlobPreReadLimiter(0))
	require.Nil(t, getBlobPreReadLimiter(-1))

	first := getBlobPreReadLimiter(1024)
	require.NotNil(t, first)
	require.Equal(t, int64(1024), first.maxBytes)

	second := getBlobPreReadLimiter(2048)
	require.Same(t, first, second)
}

func TestBlobPreReadLimiterAndEnsurePrefetchBuf(t *testing.T) {
	l := &blobPreReadLimiter{maxBytes: 8}
	assert.True(t, l.tryAcquire(4))
	assert.False(t, l.tryAcquire(5))
	l.release(2)
	assert.True(t, l.tryAcquire(4))

	s := mustTestECStreamerWithEbsc(1, nil, 0)
	r := &Reader{ecStreamer: s, preReadLimiter: &blobPreReadLimiter{maxBytes: 4}}
	assert.False(t, r.ensurePrefetchBuf())

	s2 := mustTestECStreamerWithEbsc(2, nil, 16)
	r2 := &Reader{ecStreamer: s2, preReadLimiter: &blobPreReadLimiter{maxBytes: 64}}
	assert.True(t, r2.ensurePrefetchBuf())
}

func TestReader_readSliceRange_with_ecStreamer_ebsc(t *testing.T) {
	ebsc := newSafeBlobStoreClientForTest()
	s := mustTestECStreamerWithEbsc(12407, ebsc, 0)
	r := &Reader{
		limitManager: manager.NewLimitManager(nil),
		ecStreamer:   s,
		enableBcache: false,
	}
	errCh := make(chan error, 1)
	rs := &rwSlice{rSize: 11, Data: make([]byte, 11)}
	err := gohook.HookMethod(ebsc, "Read", MockEbscReadTrue, nil)
	require.NoError(t, err)
	defer gohook.UnHookMethod(ebsc, "Read")
	require.NoError(t, r.readSliceRange(context.Background(), rs, errCh))
	require.NoError(t, <-errCh)
}

func TestReader_String_and_nil_Read(t *testing.T) {
	s := mustTestECStreamer(90, nil, nil)
	r := s.Reader()
	require.Contains(t, r.String(), "Reader{")
	var nilR *Reader
	_, err := nilR.Read(context.Background(), []byte{1}, 0, 1)
	require.Error(t, err)
}
