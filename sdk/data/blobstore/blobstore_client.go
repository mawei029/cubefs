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
	"math/rand"
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
	EbsMaxRetryTimes    = 200
	EbsRetryInterval    = 100 * time.Millisecond
	EbsMaxSleepInterval = 30 * time.Second
	EbsMaxTimeout       = 10 * time.Minute
)

func safeEbsRetrySleep(ctx context.Context, retryInterval time.Duration) (time.Duration, error) {
	// 1.2X + random ; 1.2x: 100ms * 12/10 = 120ms
	retryInterval = retryInterval*12/10 + time.Duration(rand.Int63n(int64(retryInterval)))
	if retryInterval > EbsMaxSleepInterval {
		retryInterval = EbsMaxSleepInterval
	}

	timer := time.NewTimer(retryInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		log.LogWarnf("TRACE Ebs RetrySleep ctx done, retryInterval(%v), err(%v)", retryInterval, ctx.Err())
		return 0, ctx.Err()
	case <-timer.C:
		return retryInterval, nil
	}
}

// BlobStoreClient wraps blobstore access API for Reader/Writer EBS I/O.
type BlobStoreClient struct {
	client        access.API
	maxTimeoutSec time.Duration // from config streamRetryTimeout
}

func NewEbsClient(cfg access.Config, maxTimeoutSec int) (*BlobStoreClient, error) {
	cli, err := access.New(cfg)
	if maxTimeoutSec <= 0 || maxTimeoutSec >= 600 {
		maxTimeoutSec = int(EbsMaxTimeout.Seconds())
	}

	return &BlobStoreClient{
		client:        cli,
		maxTimeoutSec: time.Duration(maxTimeoutSec) * time.Second,
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

	log.LogDebugf("TRACE Ebs Read,oek(%v) loc(%v)", oek, loc)
	var body io.ReadCloser
	doCloseBodyFn := func() {
		if body != nil {
			body.Close()
			body = nil
		}
	}
	defer doCloseBodyFn()

	var sleepErr error
	retryInterval := EbsRetryInterval
	for attempt := 0; attempt < EbsMaxRetryTimes; attempt++ {
		body, err = ebs.client.Get(ctx, &access.GetArgs{Location: loc, Offset: offset, ReadSize: size})
		if err == nil {
			readN, err = io.ReadFull(body, buf)
			doCloseBodyFn()
			if err == nil {
				break
			}
		}
		doCloseBodyFn()

		code := rpc.DetectStatusCode(err)
		if code == blobberr.CodeBidNotFound || code == blobberr.CodeShardMarkDeleted {
			// Old location was deleted or bid does not exist: retrying on same location is meaningless; let upper layer RefreshExtents fetch a new key.
			log.LogWarnf("TRACE Ebs Read non-retryable err, oek(%v), err(%v), requestId(%v)", oek, err, requestId)
			return 0, err
		}

		if time.Since(start) > ebs.maxTimeoutSec {
			log.LogWarnf("TRACE Ebs Read timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds())
			err = errors.New(fmt.Sprintf("Ebs Read timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds()))
			break
		}

		log.LogWarnf("TRACE Ebs Read, oek(%v), err(%v), requestId(%v), retry(%v)/%v cost(%v)ms",
			oek, err, requestId, attempt, EbsMaxRetryTimes, time.Since(start).Milliseconds())

		retryInterval, sleepErr = safeEbsRetrySleep(ctx, retryInterval)
		if sleepErr != nil {
			err = sleepErr
			break
		}
	}

	elapsed := time.Since(start)
	if err != nil {
		log.LogErrorf("[ecBlob] EBS Read fail vol(%v) locOff(%v) want(%v) status(%v) oekFileOff(%v) err(%v) reqId(%v) consume(%v)ns",
			volName, offset, size, rpc.DetectStatusCode(err), oek.FileOffset, err, requestId, elapsed.Nanoseconds())
		return 0, err
	}

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

	var sleepErr error
	retryInterval := EbsRetryInterval
	for attempt := 0; attempt < EbsMaxRetryTimes; attempt++ {
		location, _, err = ebs.client.Put(ctx, &access.PutArgs{
			Size: int64(size),
			Body: bytes.NewReader(data),
		})
		if err == nil {
			break
		}

		if time.Since(start) > ebs.maxTimeoutSec {
			log.LogWarnf("TRACE Ebs write timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds())
			err = errors.New(fmt.Sprintf("Ebs write timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds()))
			break
		}

		log.LogWarnf("TRACE Ebs write, err(%v), requestId(%v), retry(%v)/%v cost(%v)ms",
			err, requestId, attempt, EbsMaxRetryTimes, retryInterval.Milliseconds())

		retryInterval, sleepErr = safeEbsRetrySleep(ctx, retryInterval)
		if sleepErr != nil {
			err = sleepErr
			break
		}
	}
	elapsed := time.Since(start)

	if err != nil {
		log.LogErrorf("TRACE Ebs write,err(%v),requestId(%v), cost(%v)ms", err.Error(), requestId, elapsed.Milliseconds())
		return location, err
	}

	log.LogDebugf("TRACE Ebs Write Exit,requestId(%v)  len(%v) cost(%v)ms", requestId, len(data), elapsed.Milliseconds())
	return location, nil
}

func (ebs *BlobStoreClient) Delete(oeks []proto.ObjExtentKey) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("ebs-delete", err, bgTime, 1)
	}()

	ctx, cancel := context.WithTimeout(context.TODO(), time.Second*3) // Delete: only send to kafka, 3 seconds is enough
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

	var sleepErr error
	retryInterval := EbsRetryInterval
	for attempt := 0; attempt < EbsMaxRetryTimes; attempt++ {
		_, err = ebs.client.Delete(ctx, &access.DeleteArgs{Locations: locs})
		if err == nil {
			break
		}

		if time.Since(start) > ebs.maxTimeoutSec {
			log.LogWarnf("TRACE Ebs Delete timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds())
			err = errors.New(fmt.Sprintf("Ebs Delete timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds()))
			break
		}

		log.LogWarnf("TRACE Ebs Delete, locs(%v), err(%v), requestId(%v), retry(%v)/%v cost(%v)ms",
			locs, err, requestId, attempt, EbsMaxRetryTimes, time.Since(start).Milliseconds())

		retryInterval, sleepErr = safeEbsRetrySleep(ctx, retryInterval)
		if sleepErr != nil {
			err = sleepErr
			break
		}
	}
	elapsed := time.Since(start)

	if err != nil {
		// call ebs-access delete, just send delete msg to kafka, so we don't need to check the error code(CodeBidNotFound/CodeShardMarkDeleted)
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
		chunk := make([]byte, putSize)
		if _, err = io.ReadFull(io.LimitReader(f, int64(putSize)), chunk); err != nil {
			log.LogErrorf("TRACE Ebs Put, read chunk err(%v), requestId(%v)", err, requestId)
			return
		}

		var sleepErr error
		retryInterval := EbsRetryInterval
		for attempt := 0; attempt < EbsMaxRetryTimes; attempt++ {
			location, hash, err = ebs.client.Put(ctx, &access.PutArgs{
				Size:   int64(putSize),
				Hashes: access.HashAlgMD5,
				Body:   bytes.NewReader(chunk),
			})
			if err == nil {
				break
			}

			if time.Since(start) > ebs.maxTimeoutSec {
				log.LogWarnf("TRACE Ebs Put timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds())
				err = errors.New(fmt.Sprintf("Ebs Put timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds()))
				break
			}

			log.LogWarnf("TRACE Ebs Put, err(%v), requestId(%v), retry(%v)/%v cost(%v)ms",
				err, requestId, attempt, EbsMaxRetryTimes, retryInterval.Milliseconds())

			retryInterval, sleepErr = safeEbsRetrySleep(ctx, retryInterval)
			if sleepErr != nil {
				err = sleepErr
				break
			}
		}
		if err != nil {
			log.LogErrorf("TRACE Ebs Put, err(%v), requestId(%v)", err.Error(), requestId)
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
		if err != nil && body != nil {
			body.Close()
		}
	}()

	var sleepErr error
	retryInterval := EbsRetryInterval
	for attempt := 0; attempt < EbsMaxRetryTimes; attempt++ {
		body, err = ebs.client.Get(ctx, &access.GetArgs{Location: loc, Offset: offset, ReadSize: size})
		if err == nil {
			break
		}
		if body != nil {
			body.Close()
			body = nil
		}

		code := rpc.DetectStatusCode(err)
		if code == blobberr.CodeBidNotFound || code == blobberr.CodeShardMarkDeleted {
			// Old location was deleted or bid does not exist: retrying on same location is meaningless; let upper layer RefreshExtents fetch a new key.
			log.LogWarnf("TRACE Ebs Get non-retryable err, oek(%v), err(%v), requestId(%v)", oek, err, requestId)
			return nil, err
		}

		if time.Since(start) > ebs.maxTimeoutSec {
			log.LogWarnf("TRACE Ebs Get timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds())
			err = errors.New(fmt.Sprintf("Ebs Get timeout requestId(%v) cost(%v)ms", requestId, time.Since(start).Milliseconds()))
			break
		}

		log.LogWarnf("TRACE Ebs Get, oek(%v), err(%v), requestId(%v), retry(%v)/%v cost(%v)ms",
			oek, err, requestId, attempt, EbsMaxRetryTimes, time.Since(start).Milliseconds())

		retryInterval, sleepErr = safeEbsRetrySleep(ctx, retryInterval)
		if sleepErr != nil {
			err = sleepErr
			break
		}
	}

	elapsed := time.Since(start)
	if err != nil {
		log.LogErrorf("TRACE Ebs Get, oek(%v), err(%v), requestId(%v) consume(%v)ns", oek, err, requestId, elapsed.Nanoseconds())
		return
	}

	log.LogDebugf("TRACE Ebs Read Exit, oek(%v) size(%v), consume(%v)ns", oek, size, elapsed.Nanoseconds())
	return
}

// TruncateV2Extents truncates ObjExtentKey list by target size, reusing overwrite flow ComputeTruncateReqs + ApplyTruncateReqs:
// keep extents fully before targetSize, delete-only extents fully after it, and for partial overlap do read -> trim -> write new -> delete old.
// Returns deltas for meta TruncateV2: at most one NewObjExtent and one ToDelete anchor (first tail extent).
func (ebs *BlobStoreClient) TruncateV2Extents(ctx context.Context, volName string, objExtentKeys []proto.ObjExtentKey, targetSize uint64,
) (newObjExtent proto.ObjExtentKey, toDeleteFrom proto.ObjExtentKey, err error) {
	log.LogDebugf("TruncateV2Extents: volName(%v) objExtentKeys(%v) targetSize(%v)", volName, objExtentKeys, targetSize)
	if len(objExtentKeys) == 0 {
		return proto.ObjExtentKey{}, proto.ObjExtentKey{}, nil
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

	var partialKeep, discardFrom proto.ObjExtentKey
	for _, oek := range eks {
		end := oek.FileOffset + oek.Size
		// all keep extents
		if end <= targetSize {
			continue
		}
		// all delete extents
		if oek.FileOffset >= targetSize {
			if discardFrom.IsEmpty() {
				discardFrom = oek
			}
			break
		}
		// partial keep extent, keep some part and new write it, discard all old extent
		keepSize := targetSize - oek.FileOffset
		partialKeep = proto.ObjExtentKey{FileOffset: oek.FileOffset, Size: keepSize}
		discardFrom = oek
		break
	}
	return truncateReq{KeepExtent: partialKeep, DiscardFrom: discardFrom}
}

// ApplyTruncateReqs executes TruncateReq: optional single overwriteReq (read old -> trim -> write new),
// then returns meta deltas (NewObjExtent, ToDelete anchor). EBS discard of tail extents is handled by the caller hook.
func (ebs *BlobStoreClient) ApplyTruncateReqs(ctx context.Context, volName string, req truncateReq,
) (newObjExtent proto.ObjExtentKey, toDeleteFrom proto.ObjExtentKey, err error) {
	toDeleteFrom = req.DiscardFrom
	keepSome := req.KeepExtent
	if toDeleteFrom.Size == 0 || keepSome.Size == 0 {
		return keepSome, toDeleteFrom, nil
	}
	keepSize := keepSome.Size
	if keepSize > toDeleteFrom.Size {
		err = fmt.Errorf("ApplyTruncateReqs: keepSize(%v) > discard.Size(%v)", keepSize, toDeleteFrom.Size)
		log.LogErrorf("%v", err)
		return proto.ObjExtentKey{}, proto.ObjExtentKey{}, err
	}

	buf := make([]byte, keepSize)
	readN, err := ebs.Read(ctx, volName, buf, 0, keepSize, toDeleteFrom)
	if err != nil {
		log.LogErrorf("ApplyTruncateReqs: read extent (%v) keepSize(%v) err(%v)", toDeleteFrom, keepSize, err)
		return proto.ObjExtentKey{}, proto.ObjExtentKey{}, err
	}
	if uint64(readN) != keepSize {
		log.LogWarnf("ApplyTruncateReqs: read short extent(%v) want(%v) readN(%v)", toDeleteFrom, keepSize, readN)
		return proto.ObjExtentKey{}, proto.ObjExtentKey{},
			fmt.Errorf("ApplyTruncateReqs: read short want(%v) got(%v)", keepSize, readN)
	}
	newOeks, _, err := ebs.Put(ctx, volName, bytes.NewReader(buf), uint64(len(buf)))
	if err != nil {
		log.LogErrorf("ApplyTruncateReqs: put truncated err(%v)", err)
		return proto.ObjExtentKey{}, proto.ObjExtentKey{}, err
	}
	if len(newOeks) == 0 {
		log.LogErrorf("ApplyTruncateReqs: put returned no keys")
		return proto.ObjExtentKey{}, proto.ObjExtentKey{}, errPutNoKeys //nolint:wrapcheck
	}
	newObjExtent = newOeks[0]
	newObjExtent.FileOffset = keepSome.FileOffset
	return newObjExtent, toDeleteFrom, nil
}
