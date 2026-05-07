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
	"reflect"
	"strconv"
	"strings"
	"testing"

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

	blobStoreClient, err := NewEbsClient(cfg)
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
	require.Len(t, req.KeepExtents, 1)
	require.Equal(t, uint64(0), req.KeepExtents[0].FileOffset)
	require.Equal(t, uint64(100), req.KeepExtents[0].Size)
	require.Len(t, req.OverwriteReqs, 1)
	require.Equal(t, uint64(100), req.OverwriteReqs[0].NewExtent.FileOffset)
	require.Equal(t, uint64(50), req.OverwriteReqs[0].NewExtent.Size)
	require.Equal(t, uint64(100), req.OverwriteReqs[0].DiscardExtent.FileOffset)
	require.Equal(t, uint64(100), req.OverwriteReqs[0].DiscardExtent.Size)
	require.Len(t, req.DiscardOnly, 1)
	require.Equal(t, uint64(200), req.DiscardOnly[0].FileOffset)
}

func TestComputeTruncateReqs_EmptyInput(t *testing.T) {
	req := ComputeTruncateReqs(100, nil)
	require.Empty(t, req.KeepExtents)
	require.Empty(t, req.OverwriteReqs)
	require.Empty(t, req.DiscardOnly)
}

func TestTruncateV2Extents_EmptyInput(t *testing.T) {
	ebs := &BlobStoreClient{}
	ctx := context.Background()
	out, _, err := ebs.TruncateV2Extents(ctx, "vol", nil, 100)
	require.NoError(t, err)
	require.Nil(t, out)
}

func TestTruncateV2Extents_OnlyKeepNoEBS(t *testing.T) {
	// 仅保留、无覆盖、无删除时，ApplyTruncateReqs 只返回 keep，不调 EBS
	objExtents := []cproto.ObjExtentKey{
		{FileOffset: 0, Size: 50},
		{FileOffset: 50, Size: 50},
	}
	req := ComputeTruncateReqs(100, objExtents)
	require.Len(t, req.KeepExtents, 2)
	require.Empty(t, req.OverwriteReqs)
	require.Empty(t, req.DiscardOnly)

	ebs := &BlobStoreClient{}
	ctx := context.Background()
	out, toDel, err := ebs.ApplyTruncateReqs(ctx, "vol", req)
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Len(t, toDel, 0)
	require.Equal(t, uint64(0), out[0].FileOffset)
	require.Equal(t, uint64(50), out[0].Size)
	require.Equal(t, uint64(50), out[1].FileOffset)
	require.Equal(t, uint64(50), out[1].Size)
}

func TestApplyTruncateReqs_ReadError(t *testing.T) {
	ebs := &BlobStoreClient{}
	req := truncateReq{
		OverwriteReqs: []overwriteReq{
			{
				NewExtent:     cproto.ObjExtentKey{FileOffset: 0, Size: 10},
				DiscardExtent: cproto.ObjExtentKey{FileOffset: 0, Size: 20},
			},
		},
	}

	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(ebs), "Read",
		func(_ *BlobStoreClient, _ context.Context, _ string, _ []byte, _ uint64, _ uint64, _ cproto.ObjExtentKey) (int, error) {
			return 0, io.ErrUnexpectedEOF
		})
	out, toDel, err := ebs.ApplyTruncateReqs(context.Background(), "vol", req)
	require.Error(t, err)
	require.Nil(t, out)
	require.Len(t, toDel, 0)
}

func TestApplyTruncateReqs_PutNoKeys(t *testing.T) {
	ebs := &BlobStoreClient{}
	req := truncateReq{
		OverwriteReqs: []overwriteReq{
			{
				NewExtent:     cproto.ObjExtentKey{FileOffset: 0, Size: 10},
				DiscardExtent: cproto.ObjExtentKey{FileOffset: 0, Size: 20},
			},
		},
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
	require.Nil(t, out)
	require.Len(t, toDel, 0)
}

func TestApplyTruncateReqs_DeleteError(t *testing.T) {
	ebs := &BlobStoreClient{}
	req := truncateReq{
		DiscardOnly: []cproto.ObjExtentKey{{FileOffset: 0, Size: 20}},
	}

	out, toDel, err := ebs.ApplyTruncateReqs(context.Background(), "vol", req)
	require.NoError(t, err)
	require.NotNil(t, out)
	require.Len(t, out, 0)
	require.Len(t, toDel, 1)
}

func TestBlobStoreClientReadRetryBranches(t *testing.T) {
	oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
	buf := make([]byte, 4)

	t.Run("bid not found no retry", func(t *testing.T) {
		attempt := 0
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return nil, blobberr.ErrNoSuchBid
			},
		}}
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, 1, attempt)
	})

	t.Run("readfull error", func(t *testing.T) {
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				return io.NopCloser(strings.NewReader("x")), nil
			},
		}}
		_, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.Error(t, err)
	})

	t.Run("retry then success", func(t *testing.T) {
		attempt := 0
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				if attempt == 1 {
					return nil, io.ErrUnexpectedEOF
				}
				return io.NopCloser(strings.NewReader("abcd")), nil
			},
		}}
		n, err := ebs.Read(context.Background(), "v", buf, 0, 4, oek)
		require.NoError(t, err)
		require.Equal(t, 4, n)
		require.GreaterOrEqual(t, attempt, 2)
	})
}

func TestBlobStoreClientWriteAndGetRetryBranches(t *testing.T) {
	t.Run("write retry then max fail", func(t *testing.T) {
		attempt := 0
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
			putFn: func(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
				attempt++
				return proto.Location{}, nil, io.ErrClosedPipe
			},
		}}
		_, err := ebs.Write(context.Background(), "v", []byte("abcd"), 4)
		require.Error(t, err)
		require.Equal(t, MaxRetryTimes+1, attempt)
	})

	t.Run("get retry then success", func(t *testing.T) {
		attempt := 0
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				if attempt <= 2 {
					return nil, io.ErrUnexpectedEOF
				}
				return io.NopCloser(strings.NewReader("abcd")), nil
			},
		}}
		body, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.NoError(t, err)
		require.NotNil(t, body)
		_ = body.Close()
		require.GreaterOrEqual(t, attempt, 3)
	})

	t.Run("get retry hits max", func(t *testing.T) {
		attempt := 0
		oek := cproto.ObjExtentKey{Cid: 1, CodeMode: 1, Size: 4, BlobSize: 4, Blobs: []cproto.Blob{{MinBid: 1, Count: 1, Vid: 1}}, BlobsLen: 1}
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
			getFn: func(context.Context, *access.GetArgs) (io.ReadCloser, error) {
				attempt++
				return nil, io.ErrUnexpectedEOF
			},
		}}
		_, err := ebs.Get(context.Background(), "v", 0, 4, oek)
		require.Error(t, err)
		require.Equal(t, MaxRetryTimes+1, attempt)
	})
}

func TestApplyTruncateReqs_MoreBranches(t *testing.T) {
	t.Run("discard zero size skipped", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		req := truncateReq{
			OverwriteReqs: []overwriteReq{{
				NewExtent:     cproto.ObjExtentKey{FileOffset: 10, Size: 0},
				DiscardExtent: cproto.ObjExtentKey{FileOffset: 10, Size: 0},
			}},
		}
		out, del, err := ebs.ApplyTruncateReqs(context.Background(), "v", req)
		require.NoError(t, err)
		require.Empty(t, out)
		require.Empty(t, del)
	})

	t.Run("read short and put error", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		req := truncateReq{
			OverwriteReqs: []overwriteReq{{
				NewExtent:     cproto.ObjExtentKey{FileOffset: 100, Size: 2},
				DiscardExtent: cproto.ObjExtentKey{FileOffset: 100, Size: 4},
			}},
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(ebs), "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ []byte, _ uint64, _ uint64, _ cproto.ObjExtentKey) (int, error) {
				return 2, nil
			})
		patches.ApplyMethod(reflect.TypeOf(ebs), "Put",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ io.Reader, _ uint64) ([]cproto.ObjExtentKey, [][]byte, error) {
				return nil, nil, io.ErrClosedPipe
			})
		out, del, err := ebs.ApplyTruncateReqs(context.Background(), "v", req)
		require.Error(t, err)
		require.Nil(t, out)
		require.Empty(t, del)
	})

	t.Run("new key fileoffset rewritten", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		req := truncateReq{
			OverwriteReqs: []overwriteReq{{
				NewExtent:     cproto.ObjExtentKey{FileOffset: 200, Size: 2},
				DiscardExtent: cproto.ObjExtentKey{FileOffset: 200, Size: 4},
			}},
		}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(ebs), "Read",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ []byte, _ uint64, size uint64, _ cproto.ObjExtentKey) (int, error) {
				return int(size), nil
			})
		patches.ApplyMethod(reflect.TypeOf(ebs), "Put",
			func(_ *BlobStoreClient, _ context.Context, _ string, _ io.Reader, _ uint64) ([]cproto.ObjExtentKey, [][]byte, error) {
				return []cproto.ObjExtentKey{{FileOffset: 0, Size: 2}}, nil, nil
			})
		out, del, err := ebs.ApplyTruncateReqs(context.Background(), "v", req)
		require.NoError(t, err)
		require.Len(t, out, 1)
		require.Equal(t, uint64(200), out[0].FileOffset)
		require.Len(t, del, 1)
	})
}

func TestComputeTruncateReqsAndTruncateV2Extents(t *testing.T) {
	exts := []cproto.ObjExtentKey{
		{FileOffset: 10, Size: 10},
		{FileOffset: 0, Size: 10},
		{FileOffset: 20, Size: 10},
	}
	req := ComputeTruncateReqs(15, exts)
	require.Len(t, req.KeepExtents, 1)
	require.Len(t, req.OverwriteReqs, 1)
	require.Len(t, req.DiscardOnly, 1)

	t.Run("truncatev2 calls apply", func(t *testing.T) {
		ebs := &BlobStoreClient{}
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(ebs), "ApplyTruncateReqs",
			func(_ *BlobStoreClient, _ context.Context, _ string, in truncateReq) ([]cproto.ObjExtentKey, []cproto.ObjExtentKey, error) {
				require.NotEmpty(t, in.OverwriteReqs)
				return []cproto.ObjExtentKey{{FileOffset: 0, Size: 1}}, nil, nil
			})
		out, del, err := ebs.TruncateV2Extents(context.Background(), "v", exts, 15)
		require.NoError(t, err)
		require.Len(t, out, 1)
		require.Empty(t, del)
	})
}

func TestBlobStoreClientPutDeleteAndLocationBranches(t *testing.T) {
	t.Run("put one chunk success", func(t *testing.T) {
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
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
		}}

		oeks, md5s, err := ebs.Put(context.Background(), "v", strings.NewReader("abc"), 3)
		require.NoError(t, err)
		require.Len(t, oeks, 1)
		require.Len(t, md5s, 1)
		require.Equal(t, uint64(3), oeks[0].Size)
	})

	t.Run("delete success", func(t *testing.T) {
		called := false
		ebs := &BlobStoreClient{client: &fakeAccessAPI{
			deleteFn: func(_ context.Context, args *access.DeleteArgs) ([]proto.Location, error) {
				called = true
				require.Len(t, args.Locations, 1)
				return nil, nil
			},
		}}
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
