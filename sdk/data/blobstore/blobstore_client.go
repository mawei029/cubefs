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
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/blobstore/common/codemode"
	ebsproto "github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
	"github.com/google/uuid"
)

const (
	MaxRetryTimes      = 200
	RetrySleepInterval = 100 * time.Millisecond
	SendTimeLimit      = 20 * 1000 // ms
)

type BlobStoreClient struct {
	client access.API
}

func NewEbsClient(cfg access.Config) (*BlobStoreClient, error) {
	cli, err := access.New(cfg)
	return &BlobStoreClient{
		client: cli,
	}, err
}

func (ebs *BlobStoreClient) Read(ctx context.Context, volName string, buf []byte, offset uint64, size uint64, oek proto.ObjExtentKey) (readN int, err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("ebs-read", err, bgTime, 1)
	}()

	requestId := uuid.New().String()
	log.LogDebugf("TRACE Ebs Read Enter requestId(%v), oek(%v)", requestId, oek)
	ctx = access.WithRequestID(ctx, requestId)
	start := time.Now()

	metric := exporter.NewTPCnt(createOPMetric(buf, "ebsread"))
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: volName})
	}()
	blobs := oek.Blobs
	sliceInfos := make([]ebsproto.Slice, 0)
	for _, b := range blobs {
		sliceInfo := ebsproto.Slice{
			MinSliceID: ebsproto.BlobID(b.MinBid),
			Vid:        ebsproto.Vid(b.Vid),
			Count:      uint32(b.Count),
		}
		sliceInfos = append(sliceInfos, sliceInfo)
	}
	loc := ebsproto.Location{
		ClusterID: ebsproto.ClusterID(oek.Cid),
		Size_:     oek.Size,
		Crc:       oek.Crc,
		CodeMode:  codemode.CodeMode(oek.CodeMode),
		SliceSize: oek.BlobSize,
		Slices:    sliceInfos,
	}
	// func get has retry
	log.LogDebugf("TRACE Ebs Read,oek(%v) loc(%v)", oek, loc)
	var body io.ReadCloser
	defer func() {
		if body != nil {
			body.Close()
		}
	}()
	for i := 0; i < MaxRetryTimes; i++ {
		body, err = ebs.client.Get(ctx, &access.GetArgs{Location: loc, Offset: offset, ReadSize: size})
		if err == nil {
			break
		}
		log.LogWarnf("TRACE Ebs Read,oek(%v), err(%v), requestId(%v),retryTimes(%v)", oek, err, requestId, i)
		time.Sleep(RetrySleepInterval)
	}
	if err != nil {
		log.LogErrorf("TRACE Ebs Read,oek(%v), err(%v), requestId(%v)", oek, err, requestId)
		return 0, err
	}

	readN, err = io.ReadFull(body, buf)
	if err != nil {
		log.LogErrorf("TRACE Ebs Read,oek(%v), err(%v), requestId(%v)", oek, err, requestId)
		return 0, err
	}
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Ebs Read Exit,oek(%v) readN(%v),bufLen(%v),consume(%v)ns", oek, readN, len(buf), elapsed.Nanoseconds())
	return readN, nil
}

func (ebs *BlobStoreClient) Write(ctx context.Context, volName string, data []byte, size uint32) (location ebsproto.Location, err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("ebs-write", err, bgTime, 1)
	}()

	requestId := uuid.New().String()
	log.LogDebugf("TRACE Ebs Write Enter,requestId(%v)  len(%v)", requestId, size)
	start := time.Now()
	ctx = access.WithRequestID(ctx, requestId)
	metric := exporter.NewTPCnt(createOPMetric(data, "ebswrite"))
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: volName})
	}()
	for i := 0; i < MaxRetryTimes; i++ {
		location, _, err = ebs.client.Put(ctx, &access.PutArgs{
			Size: int64(size),
			Body: bytes.NewReader(data),
		})
		if err == nil {
			break
		}
		log.LogWarnf("TRACE Ebs write, err(%v), requestId(%v),retryTimes(%v)", err, requestId, i)
		if time.Since(start) > time.Duration(SendTimeLimit)*time.Millisecond {
			log.LogWarnf("TRACE Ebs write timeout requestId(%v) time(%v)", requestId, time.Since(start))
			err = errors.New(fmt.Sprintf("Ebs write timeout requestId(%v) time(%v)", requestId, time.Since(start)))
			break
		}
		time.Sleep(time.Duration(i+1) * RetrySleepInterval)
	}
	if err != nil {
		log.LogErrorf("TRACE Ebs write,err(%v),requestId(%v)", err.Error(), requestId)
		return location, err
	}
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Ebs Write Exit,requestId(%v)  len(%v) consume(%v)ns", requestId, len(data), elapsed.Nanoseconds())
	return location, nil
}

func (ebs *BlobStoreClient) Delete(oeks []proto.ObjExtentKey) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("ebs-delete", err, bgTime, 1)
	}()

	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*3)
	defer cancel()

	locs := make([]ebsproto.Location, 0)

	for _, oek := range oeks {
		sliceInfos := make([]ebsproto.Slice, 0)
		for _, b := range oek.Blobs {
			sliceInfo := ebsproto.Slice{
				MinSliceID: ebsproto.BlobID(b.MinBid),
				Vid:        ebsproto.Vid(b.Vid),
				Count:      uint32(b.Count),
			}
			sliceInfos = append(sliceInfos, sliceInfo)
		}

		loc := ebsproto.Location{
			ClusterID: ebsproto.ClusterID(oek.Cid),
			Size_:     oek.Size,
			Crc:       oek.Crc,
			CodeMode:  codemode.CodeMode(oek.CodeMode),
			SliceSize: oek.BlobSize,
			Slices:    sliceInfos,
		}
		locs = append(locs, loc)
	}

	requestId := uuid.New().String()
	log.LogDebugf("start Ebs delete Enter,requestId(%v)  len(%v)", requestId, len(oeks))
	start := time.Now()
	ctx = access.WithRequestID(ctx, requestId)
	metric := exporter.NewTPCnt("ebsdel")
	defer func() {
		metric.SetWithLabels(err, map[string]string{})
	}()

	elapsed := time.Since(start)
	_, err = ebs.client.Delete(ctx, &access.DeleteArgs{Locations: locs})
	if err != nil {
		log.LogErrorf("[EbsDelete] Ebs delete error, id(%v), consume(%v)ns, err(%v)", requestId, elapsed.Nanoseconds(), err.Error())
		return err
	}

	log.LogDebugf("Ebs delete Exit,requestId(%v)  len(%v) consume(%v)ns", requestId, len(oeks), elapsed.Nanoseconds())

	return err
}

func createOPMetric(buf []byte, tag string) string {
	if len(buf) >= 0 && len(buf) < 4*util.KB {
		return tag + "0K_4K"
	} else if len(buf) >= 4*util.KB && len(buf) < 128*util.KB {
		return tag + "4K_128K"
	} else if len(buf) >= 128*util.KB && len(buf) < 1*util.MB {
		return tag + "128K_1M"
	} else if len(buf) >= 1*util.MB && len(buf) < 4*util.MB {
		return tag + "1M_4M"
	}
	return tag + "4M_8M"
}

func createOPMetricBySize(size uint64, tag string) string {
	if size < 4*util.KB {
		return tag + "0K_4K"
	} else if size >= 4*util.KB && size < 128*util.KB {
		return tag + "4K_128K"
	} else if size >= 128*util.KB && size < 1*util.MB {
		return tag + "128K_1M"
	} else if size >= 1*util.MB && size < 4*util.MB {
		return tag + "1M_4M"
	} else if size >= 4*util.MB && size < 16*util.MB {
		return tag + "4M_16M"
	} else if size >= 16*util.MB && size < 64*util.MB {
		return tag + "16M_64M"
	} else if size >= 64*util.MB && size < 256*util.MB {
		return tag + "64M_256M"
	} else if size >= 256*util.MB && size < 1024*util.MB {
		return tag + "256M_1G"
	}
	return tag + "1G_"
}

func (ebs *BlobStoreClient) Put(ctx context.Context, volName string, f io.Reader, size uint64) (oek []proto.ObjExtentKey, md5 [][]byte, err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("ebs-write", err, bgTime, 1)
	}()

	requestId := uuid.New().String()
	log.LogDebugf("TRACE Ebs Put Enter, requestId(%v)  len(%v)", requestId, size)
	start := time.Now()
	ctx = access.WithRequestID(ctx, requestId)
	metric := exporter.NewTPCnt(createOPMetricBySize(size, "ebswrite"))
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: volName})
	}()

	var from uint64
	var part uint64 = util.ExtentSize
	rest := size
	for rest > 0 {
		var putSize uint64
		if rest > part {
			putSize = part
		} else {
			putSize = rest
		}
		rest -= putSize
		var location ebsproto.Location
		var hash access.HashSumMap
		location, hash, err = ebs.client.Put(ctx, &access.PutArgs{
			Size:   int64(putSize),
			Hashes: access.HashAlgMD5,
			Body:   f,
		})
		if err != nil {
			log.LogErrorf("TRACE Ebs Put, err(%v),requestId(%v)", err.Error(), requestId)
			return
		}

		var _md5 []byte
		_md5, err = hex.DecodeString(hash.GetSumVal(access.HashAlgMD5).(string))
		if err != nil {
			log.LogErrorf("decode md5 %v, err %v", hash.GetSumVal(access.HashAlgMD5).(string), err)
			return
		}
		oek = append(oek, locationToObjExtentKey(location, from))
		md5 = append(md5, _md5)
		from += putSize
		log.LogDebugf("TRACE Ebs Put, requestId(%v) loc(%v) putSize(%v)", requestId, location, putSize)
	}

	elapsed := time.Since(start)
	log.LogDebugf("TRACE Ebs Put Exit, requestId(%v) oek(%v) md5(%v) size(%v) consume(%v)ns", requestId, oek, md5, size, elapsed.Nanoseconds())
	return
}

func locationToObjExtentKey(location ebsproto.Location, from uint64) (oek proto.ObjExtentKey) {
	blobs := make([]proto.Blob, 0)
	for _, info := range location.Slices {
		blob := proto.Blob{
			MinBid: uint64(info.MinSliceID),
			Count:  uint64(info.Count),
			Vid:    uint64(info.Vid),
		}
		blobs = append(blobs, blob)
	}
	oek = proto.ObjExtentKey{
		Cid:        uint64(location.ClusterID),
		CodeMode:   uint8(location.CodeMode),
		Size:       location.Size_,
		BlobSize:   location.SliceSize,
		Blobs:      blobs,
		BlobsLen:   uint32(len(blobs)),
		FileOffset: from,
		Crc:        location.Crc,
	}
	return
}

func (ebs *BlobStoreClient) Get(ctx context.Context, volName string, offset uint64, size uint64, oek proto.ObjExtentKey) (body io.ReadCloser, err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("ebs-read", err, bgTime, 1)
	}()

	requestId := uuid.New().String()
	log.LogDebugf("TRACE Ebs Read Enter requestId(%v), oek(%v)", requestId, oek)
	ctx = access.WithRequestID(ctx, requestId)
	start := time.Now()

	metric := exporter.NewTPCnt(createOPMetricBySize(size, "ebsread"))
	defer func() {
		metric.SetWithLabels(err, map[string]string{exporter.Vol: volName})
	}()
	blobs := oek.Blobs
	sliceInfos := make([]ebsproto.Slice, 0)
	for _, b := range blobs {
		sliceInfo := ebsproto.Slice{
			MinSliceID: ebsproto.BlobID(b.MinBid),
			Vid:        ebsproto.Vid(b.Vid),
			Count:      uint32(b.Count),
		}
		sliceInfos = append(sliceInfos, sliceInfo)
	}
	loc := ebsproto.Location{
		ClusterID: ebsproto.ClusterID(oek.Cid),
		Size_:     oek.Size,
		Crc:       oek.Crc,
		CodeMode:  codemode.CodeMode(oek.CodeMode),
		SliceSize: oek.BlobSize,
		Slices:    sliceInfos,
	}
	// func get has retry
	log.LogDebugf("TRACE Ebs Read, oek(%v) loc(%v)", oek, loc)
	defer func() {
		if body != nil {
			body.Close()
		}
	}()
	for i := 0; i < MaxRetryTimes; i++ {
		body, err = ebs.client.Get(ctx, &access.GetArgs{Location: loc, Offset: offset, ReadSize: size})
		if err == nil {
			break
		}
		log.LogWarnf("TRACE Ebs Read, oek(%v), err(%v), requestId(%v),retryTimes(%v)", oek, err, requestId, i)
		time.Sleep(RetrySleepInterval)
	}
	if err != nil {
		log.LogErrorf("TRACE Ebs Read, oek(%v), err(%v), requestId(%v)", oek, err, requestId)
		return
	}
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Ebs Read Exit, oek(%v) size(%v), consume(%v)ns", oek, size, elapsed.Nanoseconds())
	return
}

// TruncateV2Extents 根据目标大小截断 ObjExtentKey 列表，复用 overwrite 的 ComputeTruncateReqs + ApplyTruncateReqs：
// 完全在 targetSize 之前的保留，完全在之后的仅删 EBS，部分重叠的走读→截断→写新→删旧。
// 返回截断后的新 ObjExtentKey 列表（用于 meta TruncateV2）。
func (ebs *BlobStoreClient) TruncateV2Extents(ctx context.Context, volName string, objExtentKeys []proto.ObjExtentKey, targetSize uint64) (newObjExtents []proto.ObjExtentKey, err error) {
	log.LogDebugf("TruncateV2Extents: volName(%v) objExtentKeys(%v) targetSize(%v)", volName, objExtentKeys, targetSize)
	if len(objExtentKeys) == 0 {
		return nil, nil
	}

	req := ComputeTruncateReqs(targetSize, objExtentKeys)
	return ebs.ApplyTruncateReqs(ctx, volName, req)
}

// ComputeTruncateReqs 根据目标大小 targetSize 与现有 objExtents 计算截断请求。
// 复用 overwriteReq 结构：部分重叠的 extent 对应一个 OverwriteReq（读旧→截断→写新→删旧）。
func ComputeTruncateReqs(targetSize uint64, objExtents []proto.ObjExtentKey) truncateReq {
	eks := make([]proto.ObjExtentKey, len(objExtents))
	copy(eks, objExtents)
	sort.Slice(eks, func(i, j int) bool { return eks[i].FileOffset < eks[j].FileOffset })

	var keep []proto.ObjExtentKey
	var overwriteReqs []overwriteReq
	var discardOnly []proto.ObjExtentKey
	for _, oek := range eks {
		end := oek.FileOffset + oek.Size
		if end <= targetSize {
			keep = append(keep, oek)
			continue
		}
		if oek.FileOffset >= targetSize {
			discardOnly = append(discardOnly, oek)
			continue
		}
		keepSize := targetSize - oek.FileOffset
		overwriteReqs = append(overwriteReqs, overwriteReq{
			NewExtent:     proto.ObjExtentKey{FileOffset: oek.FileOffset, Size: keepSize},
			DiscardExtent: oek,
		})
	}
	return truncateReq{KeepExtents: keep, OverwriteReqs: overwriteReqs, DiscardOnly: discardOnly}
}

// ApplyTruncateReqs 执行 TruncateReq：对每个 overwriteReq 读旧 extent、截断、写新 blob、收集新 key；
// 最后删除所有需废弃的 extent（OverwriteReq 中的 DiscardExtent + DiscardOnly）。
// 返回保留的 extents + 新写入的 extents，供 meta TruncateV2 使用。
func (ebs *BlobStoreClient) ApplyTruncateReqs(ctx context.Context, volName string, req truncateReq) (newObjExtents []proto.ObjExtentKey, err error) {
	newObjExtents = make([]proto.ObjExtentKey, 0, len(req.KeepExtents)+len(req.OverwriteReqs))
	newObjExtents = append(newObjExtents, req.KeepExtents...)
	var toDelete []proto.ObjExtentKey
	toDelete = append(toDelete, req.DiscardOnly...)

	for _, r := range req.OverwriteReqs {
		discard := r.DiscardExtent
		buf := make([]byte, discard.Size)
		readN, err := ebs.Read(ctx, volName, buf, 0, discard.Size, discard)
		if err != nil {
			log.LogErrorf("ApplyTruncateReqs: read extent (%v) err(%v)", discard, err)
			return nil, err
		}
		if uint64(readN) != discard.Size {
			log.LogWarnf("ApplyTruncateReqs: read short extent(%v) readN(%v)", discard, readN)
		}
		truncated := buf[:r.NewExtent.Size]
		newOeks, _, err := ebs.Put(ctx, volName, bytes.NewReader(truncated), uint64(len(truncated)))
		if err != nil {
			log.LogErrorf("ApplyTruncateReqs: put truncated err(%v)", err)
			return nil, err
		}
		if len(newOeks) == 0 {
			log.LogErrorf("ApplyTruncateReqs: put returned no keys")
			return nil, errPutNoKeys //nolint:wrapcheck
		}
		newKey := newOeks[0]
		newKey.FileOffset = r.NewExtent.FileOffset
		newObjExtents = append(newObjExtents, newKey)
		toDelete = append(toDelete, discard)
	}

	if len(toDelete) > 0 {
		if err = ebs.Delete(toDelete); err != nil {
			log.LogErrorf("ApplyTruncateReqs: delete old extents err(%v)", err)
			return nil, err
		}
	}
	return newObjExtents, nil
}
