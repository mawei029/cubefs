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
	blobberr "github.com/cubefs/cubefs/blobstore/common/errors"
	ebsproto "github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/common/rpc"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/exporter"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
	"github.com/google/uuid"
)

const (
	// MaxRetryTimes is the max retry count after the first failure for each EBS call (total requests = 1 + MaxRetryTimes); retry interval starts from RetrySleepInterval and doubles each attempt.
	MaxRetryTimes      = 4
	RetrySleepInterval = 100 * time.Millisecond
	SendTimeLimit      = 20 * 1000 // ms
)

// BlobStoreClient wraps blobstore access API for Reader/Writer EBS I/O.
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
	// Retry in loop when access.Get fails.
	log.LogDebugf("TRACE Ebs Read,oek(%v) loc(%v)", oek, loc)
	var body io.ReadCloser
	defer func() {
		if body != nil {
			body.Close()
		}
	}()
	for attempt, backoff := 0, RetrySleepInterval; attempt <= MaxRetryTimes; attempt, backoff = attempt+1, backoff*2 {
		body, err = ebs.client.Get(ctx, &access.GetArgs{Location: loc, Offset: offset, ReadSize: size})
		if err == nil {
			break
		}
		code := rpc.DetectStatusCode(err)
		if code == blobberr.CodeBidNotFound || code == blobberr.CodeShardMarkDeleted {
			// Old location was deleted or bid does not exist: retrying on same location is meaningless; let upper layer RefreshExtents fetch a new key.
			break
		}
		log.LogWarnf("TRACE Ebs Read,oek(%v), err(%v), requestId(%v), retry(%v)/%v", oek, err, requestId, attempt, MaxRetryTimes)
		if attempt == MaxRetryTimes {
			break
		}
		time.Sleep(backoff)
	}
	if err != nil {
		log.LogErrorf("[ecBlob] EBS Get fail vol(%v) locOff(%v) readSz(%v) status(%v) oekFileOff(%v) err(%v) reqId(%v)",
			volName, offset, size, rpc.DetectStatusCode(err), oek.FileOffset, err, requestId)
		return 0, err
	}

	readN, err = io.ReadFull(body, buf)
	if err != nil {
		log.LogErrorf("[ecBlob] EBS ReadFull fail vol(%v) want(%v) oekFileOff(%v) err(%v) reqId(%v)",
			volName, size, oek.FileOffset, err, requestId)
		return 0, err
	}
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Ebs Read Exit requestId(%v) requestReadSize(%v) readN(%v) bufLen(%v) oek(%v) cost(%v)ns, (%v)ms",
		requestId, size, readN, len(buf), oek, elapsed.Nanoseconds(), elapsed.Milliseconds())
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
	for attempt, backoff := 0, RetrySleepInterval; attempt <= MaxRetryTimes; attempt, backoff = attempt+1, backoff*2 {
		location, _, err = ebs.client.Put(ctx, &access.PutArgs{
			Size: int64(size),
			Body: bytes.NewReader(data),
		})
		if err == nil {
			break
		}
		log.LogWarnf("TRACE Ebs write, err(%v), requestId(%v), retry(%v)/%v", err, requestId, attempt, MaxRetryTimes)
		if attempt == MaxRetryTimes {
			break
		}
		if time.Since(start) > time.Duration(SendTimeLimit)*time.Millisecond {
			log.LogWarnf("TRACE Ebs write timeout requestId(%v) time(%v)", requestId, time.Since(start))
			err = errors.New(fmt.Sprintf("Ebs write timeout requestId(%v) time(%v)", requestId, time.Since(start)))
			break
		}
		time.Sleep(backoff)
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
	log.LogDebugf("TRACE Ebs Read, oek(%v) loc(%v)", oek, loc)
	defer func() {
		if body != nil {
			body.Close()
		}
	}()
	for attempt, backoff := 0, RetrySleepInterval; attempt <= MaxRetryTimes; attempt, backoff = attempt+1, backoff*2 {
		body, err = ebs.client.Get(ctx, &access.GetArgs{Location: loc, Offset: offset, ReadSize: size})
		if err == nil {
			break
		}
		log.LogWarnf("TRACE Ebs Read, oek(%v), err(%v), requestId(%v), retry(%v)/%v", oek, err, requestId, attempt, MaxRetryTimes)
		if attempt == MaxRetryTimes {
			break
		}
		time.Sleep(backoff)
	}
	if err != nil {
		log.LogErrorf("TRACE Ebs Read, oek(%v), err(%v), requestId(%v)", oek, err, requestId)
		return
	}
	elapsed := time.Since(start)
	log.LogDebugf("TRACE Ebs Read Exit, oek(%v) size(%v), consume(%v)ns", oek, size, elapsed.Nanoseconds())
	return
}

// TruncateV2Extents truncates ObjExtentKey list by target size, reusing overwrite flow ComputeTruncateReqs + ApplyTruncateReqs:
// keep extents fully before targetSize, delete-only extents fully after it, and for partial overlap do read -> trim -> write new -> delete old.
// Return the truncated ObjExtentKey list for meta TruncateV2.
func (ebs *BlobStoreClient) TruncateV2Extents(ctx context.Context, volName string, objExtentKeys []proto.ObjExtentKey, targetSize uint64,
) (newObjExtents []proto.ObjExtentKey, toDelete []proto.ObjExtentKey, err error) {
	log.LogDebugf("TruncateV2Extents: volName(%v) objExtentKeys(%v) targetSize(%v)", volName, objExtentKeys, targetSize)
	if len(objExtentKeys) == 0 {
		return nil, nil, nil
	}

	req := ComputeTruncateReqs(targetSize, objExtentKeys)
	return ebs.ApplyTruncateReqs(ctx, volName, req)
}

// ComputeTruncateReqs computes truncate operations from targetSize and existing objExtents.
// Reuse overwriteReq structure: each partially overlapped extent maps to one OverwriteReq (read old -> trim -> write new -> delete old).
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

// ApplyTruncateReqs executes TruncateReq: for each overwriteReq, read old extent, trim, write new blob, and collect new keys;
// then delete all discarded extents (DiscardExtent in OverwriteReq plus DiscardOnly).
// Return kept extents plus newly written extents for meta TruncateV2.
func (ebs *BlobStoreClient) ApplyTruncateReqs(ctx context.Context, volName string, req truncateReq,
) (newObjExtents []proto.ObjExtentKey, toDelete []proto.ObjExtentKey, err error) {
	newObjExtents = make([]proto.ObjExtentKey, 0, len(req.KeepExtents)+len(req.OverwriteReqs))
	newObjExtents = append(newObjExtents, req.KeepExtents...)
	toDelete = make([]proto.ObjExtentKey, 0, len(req.DiscardOnly)+len(req.OverwriteReqs))
	toDelete = append(toDelete, req.DiscardOnly...)

	// TODO: next version, at most one OverwriteReqs entry
	for _, r := range req.OverwriteReqs {
		discard := r.DiscardExtent
		if discard.Size == 0 {
			continue
		}
		toDelete = append(toDelete, discard)

		buf := make([]byte, discard.Size)
		readN, err := ebs.Read(ctx, volName, buf, 0, discard.Size, discard)
		if err != nil {
			log.LogErrorf("ApplyTruncateReqs: read extent (%v) err(%v)", discard, err)
			return nil, nil, err
		}
		if uint64(readN) != discard.Size {
			log.LogWarnf("ApplyTruncateReqs: read short extent(%v) readN(%v)", discard, readN)
		}
		truncated := buf[:r.NewExtent.Size]
		newOeks, _, err := ebs.Put(ctx, volName, bytes.NewReader(truncated), uint64(len(truncated)))
		if err != nil {
			log.LogErrorf("ApplyTruncateReqs: put truncated err(%v)", err)
			return nil, nil, err
		}
		if len(newOeks) == 0 {
			log.LogErrorf("ApplyTruncateReqs: put returned no keys")
			return nil, nil, errPutNoKeys //nolint:wrapcheck
		}
		newKey := newOeks[0]
		newKey.FileOffset = r.NewExtent.FileOffset
		newObjExtents = append(newObjExtents, newKey)
	}

	return newObjExtents, toDelete, nil
}
