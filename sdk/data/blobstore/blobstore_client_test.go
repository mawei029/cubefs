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
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/blobstore/common/crc32block"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/util/bytespool"
	cproto "github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/require"
)

var dataCache []byte

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
