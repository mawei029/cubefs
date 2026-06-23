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
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/stretchr/testify/require"

	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/blobstore/common/crc32block"
	blobberr "github.com/cubefs/cubefs/blobstore/common/errors"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/util/bytespool"
	cproto "github.com/cubefs/cubefs/proto"
)

var dataCache []byte

var testMainPatches *gomonkey.Patches

func mockSafeEbsRetrySleep(ctx context.Context, retryInterval time.Duration) (time.Duration, error) {
	retryInterval = retryInterval*12/10 + retryInterval/2
	if retryInterval > EbsMaxSleepInterval {
		retryInterval = EbsMaxSleepInterval
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
		return retryInterval, nil
	}
}

func TestMain(m *testing.M) {
	// EbsMaxRetryTimes=200 with exponential backoff; skip real sleep in retry-branch tests.
	testMainPatches = gomonkey.NewPatches()
	testMainPatches.ApplyFunc(safeEbsRetrySleep, mockSafeEbsRetrySleep)
	code := m.Run()
	testMainPatches.Reset()
	os.Exit(code)
}

type fakeAccessAPI struct {
	putFn    func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error)
	getFn    func(context.Context, *access.GetArgs) (io.ReadCloser, error)
	deleteFn func(context.Context, *access.DeleteArgs) ([]proto.Location, error)
}

func (f *fakeAccessAPI) Put(ctx context.Context, args *access.PutArgs) (proto.Location, access.HashSumMap, error) {
	return f.putFn(ctx, args)
}

func (f *fakeAccessAPI) Get(ctx context.Context, args *access.GetArgs) (io.ReadCloser, error) {
	return f.getFn(ctx, args)
}

func (f *fakeAccessAPI) Delete(ctx context.Context, args *access.DeleteArgs) ([]proto.Location, error) {
	return f.deleteFn(ctx, args)
}

func testBlobStoreClient(client access.API) *BlobStoreClient {
	return &BlobStoreClient{
		client:        client,
		maxTimeoutSec: EbsMaxTimeout,
	}
}

func TestNewEbsClientMaxTimeout(t *testing.T) {
	patches := gomonkey.ApplyFunc(access.New, func(access.Config) (access.API, error) {
		return &fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) { return nil, nil },
			putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
				return proto.Location{}, nil, nil
			},
			deleteFn: func(context.Context, *access.DeleteArgs) ([]proto.Location, error) { return nil, nil },
		}, nil
	})
	defer patches.Reset()

	t.Run("default when zero", func(t *testing.T) {
		cli, err := NewEbsClient(access.Config{}, 0)
		require.NoError(t, err)
		require.Equal(t, EbsMaxTimeout, cli.maxTimeoutSec)
	})

	t.Run("default when too large", func(t *testing.T) {
		cli, err := NewEbsClient(access.Config{}, 600)
		require.Equal(t, EbsMaxTimeout, cli.maxTimeoutSec)
		require.NoError(t, err)
	})

	t.Run("custom timeout", func(t *testing.T) {
		cli, err := NewEbsClient(access.Config{}, 120)
		require.NoError(t, err)
		require.Equal(t, 120*time.Second, cli.maxTimeoutSec)
	})
}

type MockEbsService struct {
	service *httptest.Server
}

func NewMockEbsService() *MockEbsService {
	dataCache = make([]byte, 1<<25)
	mockServer := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.URL.Path == "/put" {
				putSize := req.URL.Query().Get("size")

				dataSize, _ := strconv.Atoi(putSize)
				size := req.Header.Get("Content-Length")
				l, _ := strconv.Atoi(size)

				w.Header().Set("X-Ack-Crc-Encoded", "1")
				w.WriteHeader(http.StatusOK)

				body := crc32block.NewBodyDecoder(req.Body)
				defer body.Close()
				dataCache = dataCache[:cap(dataCache)]
				dataCache = dataCache[:crc32block.DecodeSizeWithDefualtBlock(int64(l))]
				io.ReadFull(body, dataCache)

				hashesStr := req.URL.Query().Get("hashes")
				algsInt, _ := strconv.Atoi(hashesStr)
				algs := access.HashAlgorithm(algsInt)

				hashSumMap := algs.ToHashSumMap()
				for alg := range hashSumMap {
					hasher := alg.ToHasher()
					hasher.Write(dataCache)
					hashSumMap[alg] = hasher.Sum(nil)
				}

				loc := proto.Location{Size_: uint64(dataSize)}
				fillCrc(&loc)
				resp := access.PutResp{
					Location:   loc,
					HashSumMap: hashSumMap,
				}
				b, _ := json.Marshal(resp)
				w.Write(b)

			} else if req.URL.Path == "/get" {
				var args access.GetArgs
				requestBody(req, &args)
				if !verifyCrc(&args.Location) {
					w.WriteHeader(http.StatusForbidden)
					return
				}

				data := make([]byte, args.ReadSize)
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(http.StatusOK)
				w.Write(data)

			} else if req.URL.Path == "/delete" {
				args := access.DeleteArgs{}
				requestBody(req, &args)
				if !args.IsValid() {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				for _, loc := range args.Locations {
					if !verifyCrc(&loc) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
				}

				b, _ := json.Marshal(access.DeleteResp{})
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Length", strconv.Itoa(len(b)))
				w.WriteHeader(http.StatusOK)
				w.Write(b)

			} else {
				w.WriteHeader(http.StatusOK)
			}
		}))

	return &MockEbsService{
		service: mockServer,
	}
}

func requestBody(req *http.Request, val interface{}) {
	l := req.Header.Get("Content-Length")
	size, _ := strconv.Atoi(l)
	data := make([]byte, size)
	io.ReadFull(req.Body, data)
	json.Unmarshal(data, val)
}

func calcCrc(loc *proto.Location) (uint32, error) {
	crcWriter := crc32.New(crc32.IEEETable)

	buf := bytespool.Alloc(1024)
	defer bytespool.Free(buf)

	n := loc.Encode2(buf)
	if n < 4 {
		return 0, fmt.Errorf("no enough bytes(%d) fill into buf", n)
	}

	if _, err := crcWriter.Write(buf[4:n]); err != nil {
		return 0, fmt.Errorf("fill crc %s", err.Error())
	}

	return crcWriter.Sum32(), nil
}

func fillCrc(loc *proto.Location) error {
	crc, err := calcCrc(loc)
	if err != nil {
		return err
	}
	loc.Crc = crc
	return nil
}

func verifyCrc(loc *proto.Location) bool {
	crc, err := calcCrc(loc)
	if err != nil {
		return false
	}
	return loc.Crc == crc
}

func TestEbsClient_Write_Read(t *testing.T) {
	cfg := access.Config{}
	mockServer := NewMockEbsService()
	cfg.PriorityAddrs = []string{mockServer.service.URL}
	cfg.ConnMode = access.QuickConnMode
	cfg.MaxSizePutOnce = 1 << 20
	defer mockServer.service.Close()

	blobStoreClient, err := NewEbsClient(cfg, 0)
	if err != nil {
		panic(err)
	}
	testCases := []struct {
		size int
	}{
		{1},
		{1023},
		{1 << 10},
		{1 << 20},
	}
	for _, tc := range testCases {
		data := make([]byte, tc.size)
		ctx := context.Background()
		location, err := blobStoreClient.Write(ctx, "testVol", data, uint32(tc.size))
		require.Exactly(t, nil, err)

		// read prepare
		blobs := make([]cproto.Blob, 0)
		for _, info := range location.Slices {
			blob := cproto.Blob{
				MinBid: uint64(info.MinSliceID),
				Count:  uint64(info.Count),
				Vid:    uint64(info.Vid),
			}
			blobs = append(blobs, blob)
		}
		oek := cproto.ObjExtentKey{
			Cid:      uint64(location.ClusterID),
			CodeMode: uint8(location.CodeMode),
			Size:     location.Size_,
			BlobSize: location.SliceSize,
			Blobs:    blobs,
			BlobsLen: uint32(len(blobs)),
			Crc:      location.Crc,
		}
		buf := make([]byte, oek.Size)
		read, err := blobStoreClient.Read(ctx, "", buf, 0, oek.Size, oek)
		require.NoError(t, err)
		require.Exactly(t, tc.size, read)
	}
}

func TestComputeOverwriteReqs_NoOverlap(t *testing.T) {
	// buffer [100, 200)，extents 均在 200 之后，则仅产生一段新数据
	objExtents := []cproto.ObjExtentKey{
		{FileOffset: 250, Size: 50},
	}
	reqs := computeOverwriteReqs(100, 200, objExtents)
	require.Len(t, reqs, 1)
	require.Equal(t, uint64(100), reqs[0].NewExtent.FileOffset)
	require.Equal(t, uint64(100), reqs[0].NewExtent.Size)
	require.True(t, reqs[0].DiscardExtent.IsEmpty())
}

func TestComputeOverwriteReqs_PartialOverlap(t *testing.T) {
	// buffer [100, 200), extent [50, 150) -> 重叠 [100, 150)
	objExtents := []cproto.ObjExtentKey{
		{FileOffset: 50, Size: 100},
	}
	reqs := computeOverwriteReqs(100, 200, objExtents)
	require.Len(t, reqs, 2)
	require.Equal(t, uint64(100), reqs[0].NewExtent.FileOffset)
	require.Equal(t, uint64(50), reqs[0].NewExtent.Size)
	require.Equal(t, uint64(50), reqs[0].DiscardExtent.FileOffset)
	require.Equal(t, uint64(150), reqs[1].NewExtent.FileOffset) // gap [150, 200)
	require.Equal(t, uint64(50), reqs[1].NewExtent.Size)
	require.True(t, reqs[1].DiscardExtent.IsEmpty())
}

// TestComputeTruncateReqs_PartialSpan 覆盖截断时部分保留、部分重叠、部分丢弃的基本场景。
func TestComputeTruncateReqs_PartialSpan(t *testing.T) {
	objExtents := []cproto.ObjExtentKey{
		{FileOffset: 0, Size: 100},
		{FileOffset: 100, Size: 100},
		{FileOffset: 200, Size: 50},
	}
	req := ComputeTruncateReqs(150, objExtents)
	require.False(t, req.KeepExtent.IsEmpty())
	require.Equal(t, uint64(100), req.KeepExtent.FileOffset)
	require.Equal(t, uint64(50), req.KeepExtent.Size)
	require.False(t, req.DiscardFrom.IsEmpty())
	require.Equal(t, uint64(100), req.DiscardFrom.FileOffset)
	require.Equal(t, uint64(100), req.DiscardFrom.Size)
}

func TestComputeTruncateReqs_EmptyInput(t *testing.T) {
	req := ComputeTruncateReqs(100, nil)
	require.True(t, req.KeepExtent.IsEmpty())
	require.True(t, req.DiscardFrom.IsEmpty())
}

func TestComputeTruncateReqs_tail_only_discard(t *testing.T) {
	eks := []cproto.ObjExtentKey{
		{FileOffset: 0, Size: 50},
		{FileOffset: 50, Size: 50},
	}
	req := ComputeTruncateReqs(50, eks)
	require.True(t, req.KeepExtent.IsEmpty())
	require.False(t, req.DiscardFrom.IsEmpty())
	require.Equal(t, uint64(50), req.DiscardFrom.FileOffset)
	require.Equal(t, uint64(50), req.DiscardFrom.Size)
}

func TestComputeTruncateReqs_unsorted_input(t *testing.T) {
	eks := []cproto.ObjExtentKey{
		{FileOffset: 100, Size: 50},
		{FileOffset: 0, Size: 50},
	}
	req := ComputeTruncateReqs(75, eks)
	require.True(t, req.KeepExtent.IsEmpty())
	require.Equal(t, uint64(100), req.DiscardFrom.FileOffset)
	require.Equal(t, uint64(50), req.DiscardFrom.Size)
}

func TestComputeTruncateReqs_all_keep_no_discard(t *testing.T) {
	eks := []cproto.ObjExtentKey{{FileOffset: 0, Size: 100}}
	req := ComputeTruncateReqs(100, eks)
	require.True(t, req.KeepExtent.IsEmpty())
	require.True(t, req.DiscardFrom.IsEmpty())
}

func TestCreateOPMetric_and_createOPMetricBySize(t *testing.T) {
	require.Equal(t, "tag0K_4K", createOPMetric(make([]byte, 1), "tag"))
	require.Equal(t, "tag4K_128K", createOPMetric(make([]byte, 8*1024), "tag"))
	require.Equal(t, "tag128K_1M", createOPMetric(make([]byte, 200*1024), "tag"))
	require.Equal(t, "tag1M_4M", createOPMetric(make([]byte, 2*1024*1024), "tag"))
	require.Equal(t, "tag4M_8M", createOPMetric(make([]byte, 5*1024*1024), "tag"))

	require.Equal(t, "sz0K_4K", createOPMetricBySize(1024, "sz"))
	require.Equal(t, "sz4K_128K", createOPMetricBySize(64*1024, "sz"))
	require.Equal(t, "sz128K_1M", createOPMetricBySize(512*1024, "sz"))
	require.Equal(t, "sz1M_4M", createOPMetricBySize(2*1024*1024, "sz"))
	require.Equal(t, "sz4M_16M", createOPMetricBySize(8*1024*1024, "sz"))
	require.Equal(t, "sz16M_64M", createOPMetricBySize(32*1024*1024, "sz"))
	require.Equal(t, "sz64M_256M", createOPMetricBySize(128*1024*1024, "sz"))
	require.Equal(t, "sz256M_1G", createOPMetricBySize(512*1024*1024, "sz"))
	require.Equal(t, "sz1G_", createOPMetricBySize(2*1024*1024*1024, "sz"))
}

func TestLocationToObjExtentKey(t *testing.T) {
	loc := proto.Location{
		ClusterID: 7,
		Size_:     99,
		Crc:       12345,
		CodeMode:  2,
		SliceSize: 4096,
		Slices: []proto.Slice{
			{MinSliceID: 10, Vid: 20, Count: 3},
		},
	}
	oek := locationToObjExtentKey(loc, 1000)
	require.Equal(t, uint64(7), oek.Cid)
	require.Equal(t, uint64(99), oek.Size)
	require.Equal(t, uint64(1000), oek.FileOffset)
	require.Equal(t, uint32(12345), oek.Crc)
	require.Equal(t, uint8(2), oek.CodeMode)
	require.Equal(t, uint32(4096), oek.BlobSize)
	require.Len(t, oek.Blobs, 1)
	require.Equal(t, uint64(10), oek.Blobs[0].MinBid)
}

func TestApplyTruncateReqs_discard_only_no_ebs(t *testing.T) {
	req := truncateReq{
		DiscardFrom: cproto.ObjExtentKey{FileOffset: 100, Size: 50},
	}
	ebs := &BlobStoreClient{}
	out, del, err := ebs.ApplyTruncateReqs(context.Background(), "vol", req)
	require.NoError(t, err)
	require.True(t, out.IsEmpty())
	require.Equal(t, req.DiscardFrom, del)
}

func TestApplyTruncateReqs_keepSize_gt_discard(t *testing.T) {
	req := truncateReq{
		KeepExtent:  cproto.ObjExtentKey{FileOffset: 0, Size: 100},
		DiscardFrom: cproto.ObjExtentKey{FileOffset: 0, Size: 50},
	}
	ebs := &BlobStoreClient{}
	out, del, err := ebs.ApplyTruncateReqs(context.Background(), "vol", req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "keepSize")
	require.True(t, out.IsEmpty())
	require.True(t, del.IsEmpty())
}

func TestTruncateV2Extents_EmptyInput(t *testing.T) {
	ebs := &BlobStoreClient{}
	ctx := context.Background()
	out, toDel, err := ebs.TruncateV2Extents(ctx, "vol", nil, 100)
	require.NoError(t, err)
	require.True(t, out.IsEmpty())
	require.True(t, toDel.IsEmpty())
}

func TestTruncateV2Extents_OnlyKeepNoEBS(t *testing.T) {
	// 仅保留、无覆盖、无删除时，ApplyTruncateReqs 只返回 keep，不调 EBS
	objExtents := []cproto.ObjExtentKey{
		{FileOffset: 0, Size: 50},
		{FileOffset: 50, Size: 50},
	}
	req := ComputeTruncateReqs(100, objExtents)
	require.True(t, req.KeepExtent.IsEmpty())
	require.True(t, req.DiscardFrom.IsEmpty())

	ebs := &BlobStoreClient{}
	ctx := context.Background()
	out, toDel, err := ebs.ApplyTruncateReqs(ctx, "vol", req)
	require.NoError(t, err)
	require.True(t, out.IsEmpty())
	require.True(t, toDel.IsEmpty())
}

func TestApplyTruncateReqs_ReadError(t *testing.T) {
	ebs := &BlobStoreClient{}
	req := truncateReq{
		KeepExtent:  cproto.ObjExtentKey{FileOffset: 0, Size: 10},
		DiscardFrom: cproto.ObjExtentKey{FileOffset: 0, Size: 20},
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(ebs), "Read",
		func(_ *BlobStoreClient, _ context.Context, _ string, _ []byte, _ uint64, _ uint64, _ cproto.ObjExtentKey) (int, error) {
			return 0, io.ErrUnexpectedEOF
		})
	out, toDel, err := ebs.ApplyTruncateReqs(context.Background(), "vol", req)
	require.Error(t, err)
	require.True(t, out.IsEmpty())
	require.True(t, toDel.IsEmpty())
}

func TestApplyTruncateReqs_PutNoKeys(t *testing.T) {
	ebs := &BlobStoreClient{}
	req := truncateReq{
		KeepExtent:  cproto.ObjExtentKey{FileOffset: 0, Size: 10},
		DiscardFrom: cproto.ObjExtentKey{FileOffset: 0, Size: 20},
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(ebs), "Read",
		func(_ *BlobStoreClient, _ context.Context, _ string, _ []byte, _ uint64, size uint64, _ cproto.ObjExtentKey) (int, error) {
			return int(size), nil
		})
	patches.ApplyMethod(reflect.TypeOf(ebs), "Put",
		func(_ *BlobStoreClient, _ context.Context, _ string, _ io.Reader, _ uint64) ([]cproto.ObjExtentKey, [][]byte, error) {
			return nil, nil, nil
		})
	out, toDel, err := ebs.ApplyTruncateReqs(context.Background(), "vol", req)
	require.ErrorIs(t, err, errPutNoKeys)
	require.True(t, out.IsEmpty())
	require.True(t, toDel.IsEmpty())
}

func TestApplyTruncateReqs_DeleteError(t *testing.T) {
	ebs := &BlobStoreClient{}
	req := truncateReq{
		DiscardFrom: cproto.ObjExtentKey{FileOffset: 0, Size: 20},
	}

	out, toDel, err := ebs.ApplyTruncateReqs(context.Background(), "vol", req)
	require.NoError(t, err)
	require.True(t, out.IsEmpty())
	require.False(t, toDel.IsEmpty())
	require.Equal(t, uint64(0), toDel.FileOffset)
}

// type alwaysFailReader struct{}

// func (alwaysFailReader) Read([]byte) (int, error) {
// 	return 0, io.ErrUnexpectedEOF
// }

type trackCloseReader struct {
	io.Reader
	closed bool
}

func (r *trackCloseReader) Close() error {
	r.closed = true
	return nil
}

// func putSuccessFn(args *access.PutArgs) (proto.Location, access.HashSumMap, error) {
// 	sum := md5.Sum([]byte("abc"))
// 	return proto.Location{
// 			ClusterID: 1,
// 			Size_:     uint64(args.Size),
// 			CodeMode:  1,
// 			SliceSize: uint32(args.Size),
// 			Slices:    []proto.Slice{{MinSliceID: 1, Vid: 1, Count: 1}},
// 		},
// 		access.HashSumMap{access.HashAlgMD5: sum[:]},
// 		nil
// }

func TestBlobStoreClientReadRetryBranches(t *testing.T) {
	oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
	buf := make([]byte, 4)

	t.Run("bid not found no retry", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return nil, blobberr.ErrNoSuchBid
			},
		})
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, 1, attempt)
	})

	t.Run("readfull error", func(t *testing.T) {
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("x")), nil
			},
		})
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
	})

	t.Run("retry then success", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				if attempt == 1 {
					return nil, io.ErrUnexpectedEOF
				}
				return io.NopCloser(strings.NewReader("abcd")), nil
			},
		})
		n, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.NoError(t, err)
		require.Equal(t, 4, n)
		require.GreaterOrEqual(t, attempt, 2)
	})

	t.Run("readfull retry then success", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				if attempt == 1 {
					return io.NopCloser(strings.NewReader("x")), nil
				}
				return io.NopCloser(strings.NewReader("abcd")), nil
			},
		})
		n, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.NoError(t, err)
		require.Equal(t, 4, n)
		require.Equal(t, 2, attempt)
	})

	t.Run("shard mark deleted no retry", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return nil, blobberr.ErrShardMarkDeleted
			},
		})
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, 1, attempt)
	})

	t.Run("read timeout", func(t *testing.T) {
		patches := gomonkey.ApplyFunc(time.Since, func(time.Time) time.Duration {
			return EbsMaxTimeout + time.Second
		})
		defer patches.Reset()

		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				return nil, io.ErrClosedPipe
			},
		})
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Ebs Read timeout")
	})

	t.Run("read timeout uses custom maxTimeoutSec", func(t *testing.T) {
		custom := EbsMaxSleepInterval // 30 * time.Second
		patches := gomonkey.ApplyFunc(time.Since, func(time.Time) time.Duration {
			return custom + time.Second
		})
		defer patches.Reset()

		ebs := &BlobStoreClient{
			client: &fakeAccessAPI{
				getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
					return nil, io.ErrClosedPipe
				},
			},
			maxTimeoutSec: custom,
		}
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Ebs Read timeout")
	})

	t.Run("read ctx canceled during retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				if attempt == 1 {
					cancel()
				}
				return nil, io.ErrClosedPipe
			},
		})
		_, err := ebs.Read(ctx, "v", buf, 0, 4, oek)
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("read max fail on readfull", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return io.NopCloser(strings.NewReader("x")), nil
			},
		})
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, EbsMaxRetryTimes, attempt)
	})
}

func TestBlobStoreClientWriteAndGetRetryBranches(t *testing.T) {
	t.Run("write retry then max fail", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
				attempt++
				return proto.Location{}, nil, io.ErrClosedPipe
			},
		})
		_, err := ebs.Write(context.Background(), "v", []byte("abcd"), 4)
		require.Error(t, err)
		require.Equal(t, EbsMaxRetryTimes, attempt)
	})

	t.Run("write timeout", func(t *testing.T) {
		patches := gomonkey.ApplyFunc(time.Since, func(time.Time) time.Duration {
			return EbsMaxTimeout + time.Second
		})
		defer patches.Reset()

		ebs := testBlobStoreClient(&fakeAccessAPI{
			putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
				return proto.Location{}, nil, io.ErrClosedPipe
			},
		})
		_, err := ebs.Write(context.Background(), "v", []byte("abcd"), 4)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Ebs write timeout")
	})

	t.Run("write ctx canceled during retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
				attempt++
				if attempt == 1 {
					cancel()
				}
				return proto.Location{}, nil, io.ErrClosedPipe
			},
		})
		_, err := ebs.Write(ctx, "v", []byte("abcd"), 4)
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("get retry then success", func(t *testing.T) {
		attempt := 0
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				if attempt <= 2 {
					return nil, io.ErrUnexpectedEOF
				}
				return io.NopCloser(strings.NewReader("abcd")), nil
			},
		})
		body, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.NoError(t, err)
		require.NotNil(t, body)
		_ = body.Close()
		require.GreaterOrEqual(t, attempt, 3)
	})

	t.Run("get retry hits max", func(t *testing.T) {
		attempt := 0
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return nil, io.ErrUnexpectedEOF
			},
		})
		_, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, EbsMaxRetryTimes, attempt)
	})

	t.Run("get bid not found no retry", func(t *testing.T) {
		attempt := 0
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return nil, blobberr.ErrNoSuchBid
			},
		})
		_, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, 1, attempt)
	})

	t.Run("get shard mark deleted no retry", func(t *testing.T) {
		attempt := 0
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return nil, blobberr.ErrShardMarkDeleted
			},
		})
		_, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, 1, attempt)
	})

	t.Run("get timeout", func(t *testing.T) {
		patches := gomonkey.ApplyFunc(time.Since, func(time.Time) time.Duration {
			return EbsMaxTimeout + time.Second
		})
		defer patches.Reset()

		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				return nil, io.ErrClosedPipe
			},
		})
		_, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.Error(t, err)
		require.Contains(t, err.Error(), "Ebs Get timeout")
	})

	t.Run("get ctx canceled during retry", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		attempt := 0
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				if attempt == 1 {
					cancel()
				}
				return nil, io.ErrClosedPipe
			},
		})
		_, err := ebs.Get(ctx, "v", 0, 4, oek)
		require.Error(t, err)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("get closes body when get returns body and error", func(t *testing.T) {
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		tr := &trackCloseReader{Reader: strings.NewReader("x")}
		ebs := testBlobStoreClient(&fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				return tr, io.ErrClosedPipe
			},
		})
		_, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.Error(t, err)
		require.True(t, tr.closed)
	})
}

func TestApplyTruncateReqs_MoreBranches(t *testing.T) {
	t.Run("discard zero size skipped", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		req := truncateReq{}
		out, del, err := ebs.ApplyTruncateReqs(context.Background(), "v", req)
		require.NoError(t, err)
		require.True(t, out.IsEmpty())
		require.True(t, del.IsEmpty())
	})

	t.Run("read short returns error", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		req := truncateReq{
			KeepExtent:  cproto.ObjExtentKey{FileOffset: 100, Size: 2},
			DiscardFrom: cproto.ObjExtentKey{FileOffset: 100, Size: 4},
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		putCalled := false
		patches.ApplyMethod(reflect.TypeOf(ebs), "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ []byte, _ uint64, _ uint64, _ cproto.ObjExtentKey) (int, error) {
				return 1, nil
			})
		patches.ApplyMethod(reflect.TypeOf(ebs), "Put",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ io.Reader, _ uint64) ([]cproto.ObjExtentKey, [][]byte, error) {
				putCalled = true
				return nil, nil, io.ErrClosedPipe
			})
		out, del, err := ebs.ApplyTruncateReqs(context.Background(), "v", req)
		require.Error(t, err)
		require.Contains(t, err.Error(), "read short want(2) got(1)")
		require.False(t, putCalled)
		require.True(t, out.IsEmpty())
		require.True(t, del.IsEmpty())
	})

	t.Run("new key fileoffset rewritten", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		req := truncateReq{
			KeepExtent:  cproto.ObjExtentKey{FileOffset: 200, Size: 2},
			DiscardFrom: cproto.ObjExtentKey{FileOffset: 200, Size: 4},
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(ebs), "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, buf []byte, offset, size uint64, _ cproto.ObjExtentKey) (int, error) {
				require.Equal(t, uint64(0), offset)
				require.Equal(t, uint64(2), size)
				require.Len(t, buf, 2)
				return int(size), nil
			})
		patches.ApplyMethod(reflect.TypeOf(ebs), "Put",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ io.Reader, _ uint64) ([]cproto.ObjExtentKey, [][]byte, error) {
				return []cproto.ObjExtentKey{{FileOffset: 0, Size: 2}}, nil, nil
			})
		out, del, err := ebs.ApplyTruncateReqs(context.Background(), "v", req)
		require.NoError(t, err)
		require.Equal(t, uint64(200), out.FileOffset)
		require.Equal(t, uint64(200), del.FileOffset)
		require.Equal(t, uint64(4), del.Size)
	})
}

func TestComputeTruncateReqsAndTruncateV2Extents(t *testing.T) {
	exts := []cproto.ObjExtentKey{
		{FileOffset: 10, Size: 10},
		{FileOffset: 0, Size: 10},
		{FileOffset: 20, Size: 10},
	}
	req := ComputeTruncateReqs(15, exts)
	require.False(t, req.KeepExtent.IsEmpty())
	require.Equal(t, uint64(10), req.KeepExtent.FileOffset)
	require.Equal(t, uint64(5), req.KeepExtent.Size)
	require.False(t, req.DiscardFrom.IsEmpty())
	require.Equal(t, uint64(10), req.DiscardFrom.FileOffset)

	t.Run("truncatev2 calls apply", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(ebs), "ApplyTruncateReqs",
			func(_ *BlobStoreClient, _ context.Context, _ string, in truncateReq) (cproto.ObjExtentKey, cproto.ObjExtentKey, error) {
				require.False(t, in.DiscardFrom.IsEmpty())
				require.Equal(t, uint64(5), in.KeepExtent.Size)
				return cproto.ObjExtentKey{FileOffset: 10, Size: 5}, in.DiscardFrom, nil
			})
		out, del, err := ebs.TruncateV2Extents(context.Background(), "v", exts, 15)
		require.NoError(t, err)
		require.False(t, out.IsEmpty())
		require.False(t, del.IsEmpty())
		require.Equal(t, uint64(10), del.FileOffset)
	})
}

// assertObjExtentsWithinInodeSize 校验 ObjExtentKey 有序、互不重叠，且任意字节范围不超过 inode 逻辑长度。
func assertObjExtentsWithinInodeSize(t *testing.T, oeks []cproto.ObjExtentKey, inodeSize uint64) {
	t.Helper()
	cp := append([]cproto.ObjExtentKey(nil), oeks...)
	sort.Slice(cp, func(i, j int) bool {
		if cp[i].FileOffset != cp[j].FileOffset {
			return cp[i].FileOffset < cp[j].FileOffset
		}
		return cp[i].Size < cp[j].Size
	})
	var maxEnd uint64
	for i := range cp {
		o := cp[i]
		if o.Size == 0 {
			continue
		}
		require.LessOrEqual(t, o.FileOffset+o.Size, inodeSize,
			"extent end exceeds inode size: oek=%v inodeSize=%d", o, inodeSize)
		require.Less(t, o.FileOffset, inodeSize, "extent start beyond tail inodeSize=%d oek=%v", inodeSize, o)
		if i > 0 {
			prev := cp[i-1]
			if prev.Size == 0 {
				continue
			}
			prevEnd := prev.FileOffset + prev.Size
			require.LessOrEqual(t, prevEnd, o.FileOffset, "overlapping extents prev=%v cur=%v", prev, o)
		}
		if e := o.FileOffset + o.Size; e > maxEnd {
			maxEnd = e
		}
	}
	require.LessOrEqual(t, maxEnd, inodeSize)
}

func discardDedupKey(o cproto.ObjExtentKey) string {
	return fmt.Sprintf("%d:%d:%d", o.FileOffset, o.Size, o.Cid)
}

// mergeTruncateV2Deltas mirrors metanode checkTruncateV2Conflict apply: prefix before ToDelete anchor + optional New.
func mergeTruncateV2Deltas(eksInInode []cproto.ObjExtentKey, newObj, toDelete cproto.ObjExtentKey) []cproto.ObjExtentKey {
	if toDelete.IsEmpty() {
		return append([]cproto.ObjExtentKey(nil), eksInInode...)
	}
	for j, ek := range eksInInode {
		if ek.FileOffset == toDelete.FileOffset {
			final := append([]cproto.ObjExtentKey(nil), eksInInode[:j]...)
			if !newObj.IsEmpty() {
				final = append(final, newObj)
			}
			return final
		}
	}
	return append([]cproto.ObjExtentKey(nil), eksInInode...)
}

// truncateV2DroppedKeys returns extents enqueued for GC (suffix from ToDelete anchor).
func truncateV2DroppedKeys(before []cproto.ObjExtentKey, _, toDelete cproto.ObjExtentKey) []cproto.ObjExtentKey {
	if toDelete.IsEmpty() {
		return nil
	}
	for j, ek := range before {
		if ek.FileOffset == toDelete.FileOffset {
			return append([]cproto.ObjExtentKey(nil), before[j:]...)
		}
	}
	return nil
}

// TestTruncateV2Extents_MultiRoundConsistency 覆盖多轮 shrink/expand 交替下 ComputeTruncateReqs + ApplyTruncateReqs 链路与 discard 累积。
// 复现关注点：每轮返回的 newObjExtents 与目标逻辑长度一致、无越界/重叠；跨轮 toDelete 无重复键。
func TestTruncateV2Extents_MultiRoundConsistency(t *testing.T) {
	const MiB = uint64(1 << 20)
	ctx := context.Background()
	vol := "ut-vol-multi-trunc"

	newEbsWithEchoReadPut := func(t *testing.T) (*BlobStoreClient, func()) {
		t.Helper()
		ebs := &BlobStoreClient{}
		p := gomonkey.NewPatches()
		var putCnt int
		p.ApplyMethod(reflect.TypeOf(ebs), "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, buf []byte, _ uint64, size uint64, _ cproto.ObjExtentKey) (int, error) {
				return int(size), nil
			})
		p.ApplyMethod(reflect.TypeOf(ebs), "Put",
			func(_ *BlobStoreClient, _ context.Context, _ string, r io.Reader, size uint64) ([]cproto.ObjExtentKey, [][]byte, error) {
				_, err := io.Copy(io.Discard, r)
				if err != nil {
					return nil, nil, err
				}
				putCnt++
				// FileOffset 由 ApplyTruncateReqs 在返回后覆写；此处占位 0。
				return []cproto.ObjExtentKey{{FileOffset: 0, Size: size, Cid: uint64(9000 + putCnt)}}, nil, nil
			})
		return ebs, func() { p.Reset() }
	}

	t.Run("16KiB_two_8KiB_stripes_then_4K_12K_6K", func(t *testing.T) {
		const KiB = uint64(1024)
		ebs, cleanup := newEbsWithEchoReadPut(t)
		defer cleanup()

		exts := []cproto.ObjExtentKey{
			{FileOffset: 0, Size: 8 * KiB, Cid: 1},
			{FileOffset: 8 * KiB, Size: 8 * KiB, Cid: 2},
		}
		var inodeSize uint64
		var allDel []cproto.ObjExtentKey
		seenDel := make(map[string]struct{})

		round := func(name string, target uint64) {
			t.Helper()
			newObj, delFrom, err := ebs.TruncateV2Extents(ctx, vol, exts, target)
			require.NoError(t, err, name)
			dropped := truncateV2DroppedKeys(exts, newObj, delFrom)
			for _, d := range dropped {
				k := discardDedupKey(d)
				_, dup := seenDel[k]
				require.False(t, dup, "%s duplicate discard key %v", name, d)
				seenDel[k] = struct{}{}
			}
			allDel = append(allDel, dropped...)
			exts = mergeTruncateV2Deltas(exts, newObj, delFrom)
			inodeSize = target
			assertObjExtentsWithinInodeSize(t, exts, inodeSize)
		}

		round("to_4KiB", 4*KiB)
		require.NotEmpty(t, exts)
		round("to_12KiB", 12*KiB)
		round("to_6KiB", 6*KiB)

		require.Equal(t, uint64(6*KiB), inodeSize)
		assertObjExtentsWithinInodeSize(t, exts, inodeSize)
		require.NotEmpty(t, allDel)
	})

	t.Run("16MiB_two_8MiB_stripes_then_4M_12M_6M", func(t *testing.T) {
		ebs, cleanup := newEbsWithEchoReadPut(t)
		defer cleanup()

		exts := []cproto.ObjExtentKey{
			{FileOffset: 0, Size: 8 * MiB, Cid: 1},
			{FileOffset: 8 * MiB, Size: 8 * MiB, Cid: 2},
		}
		var inodeSize uint64
		var allDel []cproto.ObjExtentKey
		seenDel := make(map[string]struct{})

		round := func(name string, target uint64) {
			t.Helper()
			newObj, delFrom, err := ebs.TruncateV2Extents(ctx, vol, exts, target)
			require.NoError(t, err, name)
			dropped := truncateV2DroppedKeys(exts, newObj, delFrom)
			for _, d := range dropped {
				k := discardDedupKey(d)
				_, dup := seenDel[k]
				require.False(t, dup, "%s duplicate discard key %v", name, d)
				seenDel[k] = struct{}{}
			}
			allDel = append(allDel, dropped...)
			exts = mergeTruncateV2Deltas(exts, newObj, delFrom)
			inodeSize = target
			assertObjExtentsWithinInodeSize(t, exts, inodeSize)
		}

		round("to_4MiB", 4*MiB)
		require.NotEmpty(t, exts)
		round("to_12MiB", 12*MiB)
		round("to_6MiB", 6*MiB)

		require.Equal(t, uint64(6*MiB), inodeSize)
		assertObjExtentsWithinInodeSize(t, exts, inodeSize)
		require.NotEmpty(t, allDel, "expected some discard keys across rounds")
	})

	t.Run("16MiB_two_stripes_shrink_chain_12M_6M_2M_strict_tail", func(t *testing.T) {
		ebs, cleanup := newEbsWithEchoReadPut(t)
		defer cleanup()

		exts := []cproto.ObjExtentKey{
			{FileOffset: 0, Size: 8 * MiB, Cid: 11},
			{FileOffset: 8 * MiB, Size: 8 * MiB, Cid: 12},
		}
		var inodeSize uint64
		seenDel := make(map[string]struct{})

		round := func(target uint64) {
			newObj, delFrom, err := ebs.TruncateV2Extents(ctx, vol, exts, target)
			require.NoError(t, err)
			for _, d := range truncateV2DroppedKeys(exts, newObj, delFrom) {
				k := discardDedupKey(d)
				_, dup := seenDel[k]
				require.False(t, dup, "duplicate discard %v", d)
				seenDel[k] = struct{}{}
			}
			exts = mergeTruncateV2Deltas(exts, newObj, delFrom)
			inodeSize = target
			assertObjExtentsWithinInodeSize(t, exts, inodeSize)
		}

		round(12 * MiB)
		round(6 * MiB)
		round(2 * MiB)

		require.Equal(t, uint64(2*MiB), inodeSize)
		maxEnd := uint64(0)
		for _, o := range exts {
			if e := o.FileOffset + o.Size; e > maxEnd {
				maxEnd = e
			}
		}
		require.Equal(t, inodeSize, maxEnd, "dense tail: logical extent coverage should match inode size, exts=%v", exts)
	})
}

func TestBlobStoreClientPutDeleteAndLocationBranches(t *testing.T) {
	t.Run("put one chunk success", func(t *testing.T) {
		ebs := testBlobStoreClient(&fakeAccessAPI{
			putFn: func(_ context.Context, args *access.PutArgs) (proto.Location, access.HashSumMap, error) {
				sum := md5.Sum([]byte("x"))
				return proto.Location{
						ClusterID: 1,
						Size_:     uint64(args.Size),
						CodeMode:  1,
						SliceSize: uint32(args.Size),
						Slices:    []proto.Slice{{MinSliceID: 1, Vid: 1, Count: 1}},
					},
					access.HashSumMap{access.HashAlgMD5: sum[:]},
					nil
			},
		})

		oeks, md5s, err := ebs.Put(context.Background(), "v", strings.NewReader("abc"), 3)
		require.NoError(t, err)
		require.Len(t, oeks, 1)
		require.Len(t, md5s, 1)
		require.Equal(t, uint64(3), oeks[0].Size)
	})

	// t.Run("put retry then success", func(t *testing.T) {
	// 	attempt := 0
	// 	ebs := testBlobStoreClient(&fakeAccessAPI{
	// 		putFn: func(_ context.Context, args *access.PutArgs) (proto.Location, access.HashSumMap, error) {
	// 			attempt++
	// 			if attempt == 1 {
	// 				return proto.Location{}, nil, io.ErrClosedPipe
	// 			}
	// 			return putSuccessFn(args)
	// 		},
	// 	})
	// 	oeks, _, err := ebs.Put(context.Background(), "v", strings.NewReader("abc"), 3)
	// 	require.NoError(t, err)
	// 	require.Len(t, oeks, 1)
	// 	require.Equal(t, 2, attempt)
	// })

	// t.Run("put read fail", func(t *testing.T) {
	// 	ebs := testBlobStoreClient(&fakeAccessAPI{
	// 		putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
	// 			t.Fatal("put should not be called when read fails")
	// 			return proto.Location{}, nil, nil
	// 		},
	// 	})
	// 	_, _, err := ebs.Put(context.Background(), "v", alwaysFailReader{}, 3)
	// 	require.Error(t, err)
	// })

	// t.Run("put put max fail", func(t *testing.T) {
	// 	attempt := 0
	// 	ebs := testBlobStoreClient(&fakeAccessAPI{
	// 		putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
	// 			attempt++
	// 			return proto.Location{}, nil, io.ErrClosedPipe
	// 		},
	// 	})
	// 	_, _, err := ebs.Put(context.Background(), "v", strings.NewReader("abc"), 3)
	// 	require.Error(t, err)
	// 	require.Equal(t, EbsMaxRetryTimes, attempt)
	// })

	// t.Run("put put timeout", func(t *testing.T) {
	// 	patches := gomonkey.ApplyFunc(time.Since, func(time.Time) time.Duration {
	// 		return EbsMaxTimeout + time.Second
	// 	})
	// 	defer patches.Reset()

	// 	ebs := testBlobStoreClient(&fakeAccessAPI{
	// 		putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
	// 			return proto.Location{}, nil, io.ErrClosedPipe
	// 		},
	// 	})
	// 	_, _, err := ebs.Put(context.Background(), "v", strings.NewReader("abc"), 3)
	// 	require.Error(t, err)
	// 	require.Contains(t, err.Error(), "Ebs Put timeout")
	// })
	//
	// t.Run("put ctx canceled during retry", func(t *testing.T) {
	// 	ctx, cancel := context.WithCancel(context.Background())
	// 	attempt := 0
	// 	ebs := testBlobStoreClient(&fakeAccessAPI{
	// 		putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
	// 			attempt++
	// 			if attempt == 1 {
	// 				cancel()
	// 			}
	// 			return proto.Location{}, nil, io.ErrClosedPipe
	// 		},
	// 	})
	// 	_, _, err := ebs.Put(ctx, "v", strings.NewReader("abc"), 3)
	// 	require.Error(t, err)
	// 	require.ErrorIs(t, err, context.Canceled)
	// })

	t.Run("delete retry then success", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			deleteFn: func(_ context.Context, args *access.DeleteArgs) ([]proto.Location, error) {
				attempt++
				if attempt == 1 {
					return nil, io.ErrClosedPipe
				}
				require.Len(t, args.Locations, 1)
				return nil, nil
			},
		})
		err := ebs.Delete([]cproto.ObjExtentKey{{Cid: 1, Size: 1, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}}})
		require.NoError(t, err)
		require.Equal(t, 2, attempt)
	})

	t.Run("delete retry max fail", func(t *testing.T) {
		attempt := 0
		ebs := testBlobStoreClient(&fakeAccessAPI{
			deleteFn: func(context.Context, *access.DeleteArgs) ([]proto.Location, error) {
				attempt++
				return nil, io.ErrClosedPipe
			},
		})
		err := ebs.Delete([]cproto.ObjExtentKey{{Cid: 1, Size: 1, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}}})
		require.Error(t, err)
		require.Equal(t, EbsMaxRetryTimes, attempt)
	})

	t.Run("delete timeout", func(t *testing.T) {
		patches := gomonkey.ApplyFunc(time.Since, func(time.Time) time.Duration {
			return EbsMaxTimeout + time.Second
		})
		defer patches.Reset()

		ebs := testBlobStoreClient(&fakeAccessAPI{
			deleteFn: func(context.Context, *access.DeleteArgs) ([]proto.Location, error) {
				return nil, io.ErrClosedPipe
			},
		})
		err := ebs.Delete([]cproto.ObjExtentKey{{Cid: 1, Size: 1, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}}})
		require.Error(t, err)
		require.Contains(t, err.Error(), "Ebs Delete timeout")
	})

	t.Run("delete success", func(t *testing.T) {
		called := false
		ebs := testBlobStoreClient(&fakeAccessAPI{
			deleteFn: func(_ context.Context, args *access.DeleteArgs) ([]proto.Location, error) {
				called = true
				require.Len(t, args.Locations, 1)
				return nil, nil
			},
		})
		err := ebs.Delete([]cproto.ObjExtentKey{{Cid: 1, Size: 1, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}}})
		require.NoError(t, err)
		require.True(t, called)
	})

	t.Run("locationToObjExtentKey", func(t *testing.T) {
		oek := locationToObjExtentKey(proto.Location{
			ClusterID: 1,
			Size_:     10,
			CodeMode:  1,
			SliceSize: 4,
			Slices:    []proto.Slice{{MinSliceID: 10, Vid: 2, Count: 3}},
		}, 5)
		require.Equal(t, uint64(5), oek.FileOffset)
		require.Len(t, oek.Blobs, 1)
		require.Equal(t, uint64(10), oek.Blobs[0].MinBid)
	})
}

func TestZSafeEbsRetrySleepReal(t *testing.T) {
	testMainPatches.Reset()
	defer testMainPatches.ApplyFunc(safeEbsRetrySleep, mockSafeEbsRetrySleep)

	t.Run("ctx already canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := safeEbsRetrySleep(ctx, EbsRetryInterval)
		require.ErrorIs(t, err, context.Canceled)
	})

	t.Run("returns increased interval", func(t *testing.T) {
		next, err := safeEbsRetrySleep(context.Background(), EbsRetryInterval)
		require.NoError(t, err)
		require.Greater(t, next, EbsRetryInterval)
	})

	t.Run("ctx canceled while waiting", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(10 * time.Millisecond)
			cancel()
		}()
		_, err := safeEbsRetrySleep(ctx, time.Second)
		require.ErrorIs(t, err, context.Canceled)
	})
}
