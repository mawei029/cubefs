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
	"fmt"
	"hash"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/cubefs/cubefs/client/blockcache/bcache"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/buf"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
)

const (
	MaxBufferSize = 512 * util.MB
)

var errPutNoKeys = errors.New("ebs put returned no extent keys")

// overwriteReq represents an overwrite request that contains both the new extent and the old extent
// overwriteReq 与 overwrite.overwriteReq 一致，Writer 与 TruncateV2 共用
// overwriteReq 表示一次覆盖请求：新写入范围 NewExtent，以及需要废弃的旧范围 DiscardExtent（可为空）。
// 与 writer 的 flushExt/computeOverwriteReqs 共用同一结构，TruncateV2 复用时仅产生至多一个部分重叠的 req。
type overwriteReq struct {
	NewExtent     proto.ObjExtentKey // 要写入的新范围（部分重叠时为截断后的范围）
	DiscardExtent proto.ObjExtentKey // 需要废弃的旧 extent（可为空）
}

// truncateReq 为 TruncateV2 的请求结果：保留的 extents、至多一个部分重叠的 OverwriteReq、仅需删除的 extents。
type truncateReq struct {
	KeepExtents   []proto.ObjExtentKey
	OverwriteReqs []overwriteReq
	DiscardOnly   []proto.ObjExtentKey
}

type wSliceErr struct {
	err        error
	fileOffset uint64
	size       uint32
}

type Writer struct {
	volType      int
	volName      string
	blockSize    int
	ino          uint64
	err          chan *wSliceErr
	bc           *bcache.BcacheClient
	mw           *meta.MetaWrapper
	ebsc         *BlobStoreClient
	wConcurrency int
	wg           sync.WaitGroup
	once         sync.Once
	sync.RWMutex
	enableBcache  bool
	buf           []byte
	fileOffset    int
	fileCache     bool
	fileSize      uint64
	dirty         bool
	blockPosition int
	limitManager  *manager.LimitManager
	overwrite     bool // true: overwrite mode(flushExt); false: append mode(flush).
}

func NewWriter(config ClientConfig) (writer *Writer) {
	writer = new(Writer)

	writer.volName = config.VolName
	writer.volType = config.VolType
	writer.blockSize = config.BlockSize
	writer.ino = config.Ino
	writer.err = nil
	writer.bc = config.Bc
	writer.mw = config.Mw
	writer.ebsc = config.Ebsc
	writer.wConcurrency = config.WConcurrency
	writer.wg = sync.WaitGroup{}
	writer.once = sync.Once{}
	writer.RWMutex = sync.RWMutex{}
	writer.enableBcache = config.EnableBcache
	writer.fileCache = config.FileCache
	writer.fileSize = config.FileSize
	writer.dirty = false
	writer.allocateCache()
	writer.limitManager = config.Ec.LimitManager

	return
}

func (writer *Writer) String() string {
	return fmt.Sprintf("Writer{address(%v),volName(%v),volType(%v),ino(%v),blockSize(%v),fileSize(%v),enableBcache(%v),fileCache(%v)},wConcurrency(%v)",
		&writer, writer.volName, writer.volType, writer.ino, writer.blockSize, writer.fileSize, writer.enableBcache, writer.fileCache, writer.wConcurrency)
}

func (writer *Writer) WriteWithoutPool(ctx context.Context, offset int, data []byte) (size int, err error) {
	// atomic.StoreInt32(&writer.idle, 0)
	if writer == nil {
		return 0, fmt.Errorf("writer is not opened yet")
	}
	log.LogDebugf("TRACE blobStore WriteWithoutPool Enter: ino(%v) offset(%v) len(%v) fileSize(%v)",
		writer.ino, offset, len(data), writer.CacheFileSize())

	if len(data) > MaxBufferSize || offset != writer.CacheFileSize() {
		log.LogErrorf("TRACE blobStore WriteWithoutPool error,may be len(%v)>512MB,offset(%v)!=fileSize(%v)",
			len(data), offset, writer.CacheFileSize())
		err = syscall.EOPNOTSUPP
		return
	}
	// write buffer
	log.LogDebugf("TRACE blobStore WriteWithoutPool: ino(%v) offset(%v) len(%v)",
		writer.ino, offset, len(data))

	size, err = writer.doBufferWriteWithoutPool(ctx, data, offset)

	return
}

func (writer *Writer) Write(ctx context.Context, offset int, data []byte, flags int) (size int, err error) {
	if writer == nil {
		return 0, fmt.Errorf("writer is not opened yet")
	}
	log.LogDebugf("TRACE blobStore Write Enter: ino(%v) offset(%v) len(%v) flags&proto.FlagsAppend(%v) fileSize(%v) overwrite(%t)",
		writer.ino, offset, len(data), flags&proto.FlagsAppend, writer.CacheFileSize(), writer.overwrite)

	// Case 1: Validate write request: data too large, not append mode, or non-contiguous write (unless overwrite mode)
	//invalid := len(data) > MaxBufferSize || flags&proto.FlagsAppend == 0 || (offset > writer.CacheFileSize() && !writer.overwrite)
	invalid := len(data) > MaxBufferSize || offset > writer.CacheFileSize()
	if invalid {
		log.LogErrorf("TRACE blobStore Write error,may be len(%v)>512MB,flags(%v)!=flagAppend,offset(%v)!=fileSize(%v), overwrite(%t)",
			len(data), flags, offset, writer.CacheFileSize(), writer.overwrite)
		return 0, syscall.EOPNOTSUPP
	}

	if flags&proto.FlagsAppend != 0 && offset < writer.CacheFileSize() {
		log.LogWarnf("offset need reset. blobStore Write: ino(%v) offset(%v) len(%v) flags&proto.FlagsAppend(%v) fileSize(%v) overwrite(%t)",
			writer.ino, offset, len(data), flags&proto.FlagsAppend, writer.CacheFileSize(), writer.overwrite)
		// offset = writer.CacheFileSize()
	}

	// Case 2: Handle overwrite: either already in overwrite mode or writing before current file offset
	// Overwrite requires special handling to merge with existing extents and discard old data
	if writer.overwrite || offset < writer.CacheFileSize() {
		return writer.tryOverWrite(ctx, offset, data, flags)
	}

	// Case 3: Sequential append write: data is appended to the end of file
	// Case 3.1: with buffer: Use buffered write for better performance (data stays in buffer until flush)
	if flags&proto.FlagsSyncWrite == 0 {
		size, err = writer.doBufferWrite(ctx, data, offset)
		return
	}

	// Case 3.2: Synchronous write: write directly to EBS without buffering
	// This ensures data is immediately persisted but has lower throughput
	size, err = writer.doParallelWrite(ctx, data, offset)
	return
}

// tryOverWrite handles overwrite by buffering data and flushing with extent merge logic
func (writer *Writer) tryOverWrite(ctx context.Context, offset int, data []byte, flags int) (size int, err error) {
	if writer == nil {
		return 0, fmt.Errorf("writer is not opened yet")
	}

	writer.Lock()
	defer writer.Unlock()

	if offset != writer.fileOffset {
		// Flush existing buffer data before starting new write at different offset
		if err = writer.flushExt(writer.ino, ctx, false); err != nil {
			log.LogErrorf("TRACE blobStore tryOverWrite error,flush ext fail,ino(%v) offset(%v) len(%v) flags(%v) err(%v)",
				writer.ino, offset, len(data), flags, err)
			return 0, err
		}
		writer.fileOffset = offset
	}

	remainSize, position := len(data), 0
	log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)

	// The loop will write data to buffer in blocks, flushing when buffer is full
	// Process data in chunks until all data is written to buffer
	for remainSize > 0 {
		freeSize := writer.blockSize - writer.blockPosition
		if remainSize < freeSize {
			freeSize = remainSize
		}

		// Copy data and update position: advance in both input data and buffer
		copy(writer.buf[writer.blockPosition:], data[position:position+freeSize])
		position += freeSize             // Move forward in input data
		writer.blockPosition += freeSize // Move forward in buffer
		remainSize -= freeSize           // Decrease remaining data to process
		writer.fileOffset += freeSize    // Update file offset (logical position in file)
		writer.dirty = true              // Mark buffer as modified (needs flush)

		log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.fileSize(%v) writer.fileOffset(%v) writer.blockPosition(%v) position(%v) freeSize(%v)",
			writer.ino, writer.fileSize, writer.fileOffset, writer.blockPosition, position, freeSize)

		// Check buffer is full: when position are equal, buffer is completely filled. we flush buffer and continue
		if writer.blockPosition == writer.blockSize {
			log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)
			// Flush buffer with overwrite logic: this will handle extent overlap and discard old data
			err = writer.flushExt(writer.ino, ctx, false)
			if err != nil {
				// Rollback the state to maintain consistency, remove the failed buffer and revert position pointers
				writer.buf = writer.buf[:writer.blockPosition-freeSize]
				writer.fileOffset -= freeSize
				writer.blockPosition -= freeSize
				return position, err
			}
			// After successful flush, blockPosition is reset to 0 (in flushExt -> resetBuffer). buffer is empty and ready for next chunk
		}
	}

	// Update file size if write extends beyond current file size
	if offset+len(data) > int(writer.fileSize) {
		writer.fileSize = uint64(offset + len(data))
	}

	log.LogDebugf("TRACE blobStore tryOverWrite Exit: ino(%v) writer.fileSize(%v) writer.fileOffset(%v)", writer.ino, writer.fileSize, writer.fileOffset)
	return len(data), nil
}

func (writer *Writer) doParallelWrite(ctx context.Context, data []byte, offset int) (size int, err error) {
	log.LogDebugf("TRACE blobStore doDirectWrite: ino(%v) offset(%v) len(%v)", writer.ino, offset, len(data))
	writer.Lock()
	defer writer.Unlock()
	wSlices := writer.prepareWriteSlice(offset, data)
	log.LogDebugf("TRACE blobStore prepareWriteSlice: wSlices(%v)", wSlices)
	sliceSize := len(wSlices)

	writer.wg.Add(sliceSize)
	writer.err = make(chan *wSliceErr, sliceSize)
	pool := New(writer.wConcurrency, sliceSize)
	defer pool.Close()
	for _, wSlice := range wSlices {
		pool.Execute(wSlice, func(param *rwSlice) {
			writer.writeSlice(ctx, param, true)
		})
	}
	writer.wg.Wait()
	for i := 0; i < sliceSize; i++ {
		if wErr := <-writer.err; wErr != nil {
			log.LogErrorf("slice write error,ino(%v) fileoffset(%v) sliceSize(%v) err(%v)",
				writer.ino, wErr.fileOffset, wErr.size, wErr.err)
			return 0, wErr.err
		}
	}
	close(writer.err)
	// update meta
	oeks := make([]proto.ObjExtentKey, 0)
	for _, wSlice := range wSlices {
		size += int(wSlice.size)
		oeks = append(oeks, wSlice.objExtentKey)
	}
	log.LogDebugf("TRACE blobStore appendObjExtentKeys: oeks(%v)", oeks)
	if err = writer.mw.AppendObjExtentKeys(writer.ino, oeks); err != nil {
		log.LogErrorf("slice write error,meta append ebsc extent keys fail,ino(%v) fileOffset(%v) len(%v) err(%v)", writer.ino, offset, len(data), err)
		return
	}
	atomic.AddUint64(&writer.fileSize, uint64(size))

	return
}

func (writer *Writer) WriteFromReader(ctx context.Context, reader io.Reader, h hash.Hash) (size uint64, err error) {
	var (
		tmp         = buf.ClodVolWriteBufPool.Get().([]byte)
		exec        = NewExecutor(writer.wConcurrency)
		leftToWrite int
	)
	defer buf.ClodVolWriteBufPool.Put(tmp) // nolint: staticcheck

	writer.fileOffset = 0
	writer.err = make(chan *wSliceErr)

	var oeksLock sync.RWMutex
	oeks := make([]proto.ObjExtentKey, 0)

	writeBuff := func() {
		bufSize := len(writer.buf)
		log.LogDebugf("writeBuff: bufSize(%v), leftToWrite(%v), err(%v)", bufSize, leftToWrite, err)
		if bufSize == writer.blockSize || (leftToWrite == 0 && err == io.EOF) {
			wSlice := &rwSlice{
				fileOffset: uint64(writer.fileOffset - bufSize),
				size:       uint32(bufSize),
			}
			wSlice.Data = make([]byte, bufSize)
			copy(wSlice.Data, writer.buf)
			writer.buf = writer.buf[:0]
			if (err == nil || err == io.EOF) && h != nil {
				h.Write(wSlice.Data)
				log.LogDebugf("writeBuff: bufSize(%v), md5", bufSize)
			}
			writer.wg.Add(1)

			write := func() {
				defer writer.wg.Done()
				err := writer.writeSlice(ctx, wSlice, false)
				if err != nil {
					writer.Lock()
					if len(writer.err) > 0 {
						writer.Unlock()
						return
					}
					wErr := &wSliceErr{
						err:        err,
						fileOffset: wSlice.fileOffset,
						size:       wSlice.size,
					}
					writer.err <- wErr
					writer.Unlock()
					return
				}

				oeksLock.Lock()
				oeks = append(oeks, wSlice.objExtentKey)
				oeksLock.Unlock()
			}

			exec.Run(write)
		}
	}

LOOP:
	for {
		position := 0
		leftToWrite, err = reader.Read(tmp)
		if err != nil && err != io.EOF {
			return
		}

		for leftToWrite > 0 {
			log.LogDebugf("WriteFromReader: leftToWrite(%v), err(%v)", leftToWrite, err)
			writer.RLock()
			errNum := len(writer.err)
			writer.RUnlock()
			if errNum > 0 {
				break LOOP
			}

			freeSize := writer.blockSize - len(writer.buf)
			writeSize := util.Min(leftToWrite, freeSize)
			writer.buf = append(writer.buf, tmp[position:position+writeSize]...)
			position += writeSize
			leftToWrite -= writeSize
			writer.fileOffset += writeSize
			writer.dirty = true

			writeBuff()

		}
		if err == io.EOF {
			log.LogDebugf("WriteFromReader: EOF")
			if len(writer.buf) > 0 {
				writeBuff()
			}
			err = nil
			writer.wg.Wait()
			var wErr *wSliceErr
			select {
			case wErr := <-writer.err:
				err = wErr.err
			default:
			}
			if err != nil {
				log.LogErrorf("slice write error,ino(%v) fileoffset(%v)  sliceSize(%v) err(%v)", writer.ino, wErr.fileOffset, wErr.size, err)
			}
			break
		}
	}

	log.LogDebugf("WriteFromReader before sort: %v", oeks)
	sort.Slice(oeks, func(i, j int) bool {
		return oeks[i].FileOffset < oeks[j].FileOffset
	})
	log.LogDebugf("WriteFromReader after sort: %v", oeks)
	if err = writer.mw.AppendObjExtentKeys(writer.ino, oeks); err != nil {
		log.LogErrorf("WriteFromReader error,meta append ebsc extent keys fail,ino(%v), err(%v)", writer.ino, err)
		return
	}

	size = uint64(writer.fileOffset)
	atomic.AddUint64(&writer.fileSize, size)
	return
}

func (writer *Writer) doBufferWriteWithoutPool(ctx context.Context, data []byte, offset int) (size int, err error) {
	log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool Enter: ino(%v) offset(%v) len(%v)", writer.ino, offset, len(data))

	writer.fileOffset = offset
	dataSize := len(data)
	position := 0
	log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)
	writer.Lock()
	defer writer.Unlock()
	for dataSize > 0 {
		freeSize := writer.blockSize - len(writer.buf)
		if dataSize < freeSize {
			freeSize = dataSize
		}
		log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool: ino(%v) writer.fileSize(%v) writer.fileOffset(%v) position(%v) freeSize(%v)", writer.ino, writer.fileSize, writer.fileOffset, position, freeSize)
		writer.buf = append(writer.buf, data[position:position+freeSize]...)
		log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool:ino(%v) writer.buf.len(%v)", writer.ino, len(writer.buf))
		position += freeSize
		dataSize -= freeSize
		writer.fileOffset += freeSize
		writer.dirty = true

		if len(writer.buf) == writer.blockSize {
			log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)
			writer.Unlock()
			err = writer.flushWithoutPool(writer.ino, ctx, false)
			writer.Lock()
			if err != nil {
				// Revert only this iteration's append (already-flushed prefixes must not be rolled back).
				if freeSize > len(writer.buf) {
					log.LogErrorf("doBufferWriteWithoutPool: rollback len err ino(%v) freeSize(%v) bufLen(%v)", writer.ino, freeSize, len(writer.buf))
					return 0, err
				}
				writer.buf = writer.buf[:len(writer.buf)-freeSize]
				writer.fileOffset -= freeSize
				return 0, err
			}
		}
	}

	size = len(data)
	atomic.AddUint64(&writer.fileSize, uint64(size))

	log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool Exit: ino(%v) writer.fileSize(%v) writer.fileOffset(%v)", writer.ino, writer.fileSize, writer.fileOffset)
	return size, nil
}

func (writer *Writer) doBufferWrite(ctx context.Context, data []byte, offset int) (size int, err error) {
	log.LogDebugf("TRACE blobStore doBufferWrite Enter: ino(%v) offset(%v) len(%v)", writer.ino, offset, len(data))

	writer.fileOffset = offset
	dataSize := len(data)
	position := 0
	log.LogDebugf("TRACE blobStore doBufferWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)
	writer.Lock()
	defer writer.Unlock()
	for dataSize > 0 {
		freeSize := writer.blockSize - writer.blockPosition
		if dataSize < freeSize {
			freeSize = dataSize
		}
		log.LogDebugf("TRACE blobStore doBufferWrite: ino(%v) writer.fileSize(%v) writer.fileOffset(%v) writer.blockPosition(%v) position(%v) freeSize(%v)", writer.ino, writer.fileSize, writer.fileOffset, writer.blockPosition, position, freeSize)
		copy(writer.buf[writer.blockPosition:], data[position:position+freeSize])
		log.LogDebugf("TRACE blobStore doBufferWrite:ino(%v) writer.buf.len(%v)", writer.ino, len(writer.buf))
		position += freeSize
		writer.blockPosition += freeSize
		dataSize -= freeSize
		writer.fileOffset += freeSize
		writer.dirty = true

		if writer.blockPosition == writer.blockSize {
			log.LogDebugf("TRACE blobStore doBufferWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)
			writer.Unlock()
			err = writer.flush(writer.ino, ctx, false)
			writer.Lock()
			if err != nil {
				writer.buf = writer.buf[:writer.blockPosition-freeSize]
				writer.fileOffset -= freeSize
				writer.blockPosition -= freeSize
				return
			}
		}
	}

	size = len(data)
	atomic.AddUint64(&writer.fileSize, uint64(size))

	log.LogDebugf("TRACE blobStore doBufferWrite Exit: ino(%v) writer.fileSize(%v) writer.fileOffset(%v)", writer.ino, writer.fileSize, writer.fileOffset)
	return size, nil
}

func (writer *Writer) FlushWithoutPool(ino uint64, ctx context.Context) (err error) {
	if writer == nil {
		return
	}
	return writer.flushWithoutPool(ino, ctx, true)
}

func (writer *Writer) Flush(ino uint64, ctx context.Context) (err error) {
	if writer == nil {
		return
	}

	// If in overwrite mode, use flushExt which handles extent overlap and discard logic
	// Otherwise, use normal flush which simply appends new extent
	if writer.overwrite {
		writer.Lock()
		defer writer.Unlock()
		return writer.flushExt(ino, ctx, true)
	}

	// Normal append flush: no overlap handling needed
	return writer.flush(ino, ctx, true)
}

func (writer *Writer) prepareWriteSlice(offset int, data []byte) []*rwSlice {
	size := len(data)
	wSlices := make([]*rwSlice, 0)
	wSliceCount := size / writer.blockSize
	remainSize := size % writer.blockSize
	for index := 0; index < wSliceCount; index++ {
		offset := offset + index*writer.blockSize
		wSlice := &rwSlice{
			index:      index,
			fileOffset: uint64(offset),
			size:       uint32(writer.blockSize),
			Data:       data[index*writer.blockSize : (index+1)*writer.blockSize],
		}
		wSlices = append(wSlices, wSlice)
	}
	offset = offset + wSliceCount*writer.blockSize
	if remainSize > 0 {
		wSlice := &rwSlice{
			index:      wSliceCount,
			fileOffset: uint64(offset),
			size:       uint32(remainSize),
			Data:       data[wSliceCount*writer.blockSize:],
		}
		wSlices = append(wSlices, wSlice)
	}

	return wSlices
}

func (writer *Writer) writeSlice(ctx context.Context, wSlice *rwSlice, wg bool) (err error) {
	if wg {
		defer writer.wg.Done()
	}
	writer.limitManager.WriteAlloc(ctx, int(wSlice.size))
	log.LogDebugf("TRACE blobStore,writeSlice to ebs. ino(%v) fileOffset(%v) len(%v)", writer.ino, wSlice.fileOffset, wSlice.size)
	location, err := writer.ebsc.Write(ctx, writer.volName, wSlice.Data, wSlice.size)
	if err != nil {
		if wg {
			writer.err <- &wSliceErr{err: err, fileOffset: wSlice.fileOffset, size: wSlice.size}
		}
		return err
	}
	log.LogDebugf("TRACE blobStore,location(%v)", location)
	blobs := make([]proto.Blob, 0)
	for _, info := range location.Slices {
		blob := proto.Blob{
			MinBid: uint64(info.MinSliceID),
			Count:  uint64(info.Count),
			Vid:    uint64(info.Vid),
		}
		blobs = append(blobs, blob)
	}
	wSlice.objExtentKey = proto.ObjExtentKey{
		Cid:        uint64(location.ClusterID),
		CodeMode:   uint8(location.CodeMode),
		Size:       location.Size_,
		BlobSize:   location.SliceSize,
		Blobs:      blobs,
		BlobsLen:   uint32(len(blobs)),
		FileOffset: wSlice.fileOffset,
		Crc:        location.Crc,
	}
	log.LogDebugf("TRACE blobStore,objExtentKey(%v)", wSlice.objExtentKey)

	if wg {
		writer.err <- nil
	}
	return
}

func (writer *Writer) resetBufferWithoutPool() {
	writer.buf = writer.buf[:0]
}

func (writer *Writer) resetBuffer() {
	// writer.buf = writer.buf[:0]
	writer.blockPosition = 0
}

func (writer *Writer) flushWithoutPool(inode uint64, ctx context.Context, flushFlag bool) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("blobstore-flush", err, bgTime, 1)
	}()

	log.LogDebugf("TRACE blobStore flushWithoutPool: ino(%v) buf-len(%v) flushFlag(%v)", inode, len(writer.buf), flushFlag)
	writer.Lock()
	defer func() {
		writer.dirty = false
		writer.Unlock()
	}()

	if len(writer.buf) == 0 || !writer.dirty {
		return
	}
	bufferSize := len(writer.buf)
	if writer.fileOffset < bufferSize {
		err = fmt.Errorf("flushWithoutPool: inconsistent state ino(%v) fileOffset(%v) < len(buf)(%v)", inode, writer.fileOffset, bufferSize)
		log.LogErrorf(err.Error())
		return err
	}
	wSlice := &rwSlice{
		fileOffset: uint64(writer.fileOffset - bufferSize),
		size:       uint32(bufferSize),
		Data:       writer.buf,
	}
	err = writer.writeSlice(ctx, wSlice, false)
	if err != nil {
		if flushFlag {
			atomic.AddUint64(&writer.fileSize, -uint64(bufferSize))
		}
		return
	}

	oeks := make([]proto.ObjExtentKey, 0)
	// update meta
	oeks = append(oeks, wSlice.objExtentKey)
	if err = writer.mw.AppendObjExtentKeys(writer.ino, oeks); err != nil {
		log.LogErrorf("slice write error,meta append ebsc extent keys fail,ino(%v) fileOffset(%v) len(%v) err(%v)", inode, wSlice.fileOffset, wSlice.size, err)
		// Rollback: delete orphaned data on EBS to avoid blobstore leak
		if delErr := writer.ebsc.Delete(oeks); delErr != nil {
			log.LogWarnf("flushWithoutPool: rollback delete ebs extent fail,ino(%v) fileOffset(%v) err(%v)", inode, wSlice.fileOffset, delErr)
		}
		return
	}
	writer.resetBufferWithoutPool()
	return
}

// computeOverwriteReqs computes overwrite request list based on the current buffer range [bufferStart, bufferEnd)
// and existing objExtents. Each returned overwriteReq represents a range to write (NewExtent) and its corresponding
// old range to discard (DiscardExtent, which may be empty). objExtents must be sorted by FileOffset in ascending order.
//
// Algorithm:
//  1. Iterate through existing extents in sorted order
//  2. For each extent, determine if it overlaps with buffer range [start, end)
//  3. Generate requests for:
//     - Non-overlapping regions (new data, no discard)
//     - Overlapping regions (new data + old extent to discard)
//  4. Handle partial overlaps where buffer only covers part of an extent
//
// Example:
//
//	Buffer: [100, 200), Existing extents: [50, 150), [200, 250)
//	Writer.fileOffset==200, Writer.fileSize==300 ; eks: {offset:50, size:100}, {offset:200, size:50}
//	Result:
//	  - req1: NewExtent=[100, 150), DiscardExtent=[50, 150) (partial overlap)
//	  - req2: NewExtent=[150, 200), DiscardExtent=empty (new data, no overlap)
func computeOverwriteReqs(start, end uint64, objExtents []proto.ObjExtentKey) (reqs []overwriteReq) {
	reqs = make([]overwriteReq, 0)
	for _, ek := range objExtents {
		if end <= ek.FileOffset {
			reqs = append(reqs, overwriteReq{
				NewExtent:     proto.ObjExtentKey{FileOffset: start, Size: end - start},
				DiscardExtent: proto.ObjExtentKey{},
			})
			start = end
			break
		}
		// Extent ends before current start: buffer lies in a hole after this ek; skip to avoid
		// uint64 underflow in reqSize when end > ek.FileOffset+ek.Size.
		if start >= ek.FileOffset+ek.Size {
			continue
		}
		if start < ek.FileOffset {
			reqs = append(reqs, overwriteReq{
				NewExtent:     proto.ObjExtentKey{FileOffset: start, Size: ek.FileOffset - start},
				DiscardExtent: proto.ObjExtentKey{},
			})
			start = ek.FileOffset
		}
		reqSize := end - start
		if end > ek.FileOffset+ek.Size {
			reqSize = ek.FileOffset + ek.Size - start
		}
		reqs = append(reqs, overwriteReq{
			NewExtent:     proto.ObjExtentKey{FileOffset: start, Size: reqSize},
			DiscardExtent: ek,
		})
		start += reqSize
		if end <= ek.FileOffset+ek.Size {
			break
		}
	}
	if start < end {
		reqs = append(reqs, overwriteReq{
			NewExtent:     proto.ObjExtentKey{FileOffset: start, Size: end - start},
			DiscardExtent: proto.ObjExtentKey{},
		})
	}
	return reqs
}

// flushOverwriteReqs applies the request list: write all slices to EBS, then update metadata in one batch.
// 1. For each req: build wSlice (read/merge old extent if partial overwrite), writeSlice to EBS, collect newExtent + discardExtent.
// 2. One AppendObjExtentKeysWithCheck(ino, newExtents, discardExtents); on failure rollback (delete all new extents).
// Call flow: writer.mw.AppendObjExtentKeysWithCheckBatch -> metanode BatchObjExtentAppendWithCheck -> fsmAppendObjExtentsWithCheck (multi-pair) -> objExtDelCh <- toDelete
func (writer *Writer) flushOverwriteReqs(ctx context.Context, inode uint64, reqs []overwriteReq, bufOff uint64, bufferSize int, flushFlag bool) (err error) {
	newExtents := make([]proto.ObjExtentKey, 0, len(reqs))
	discardExtents := make([]proto.ObjExtentKey, 0, len(reqs))

	rollbackFn := func() {
		for _, written := range newExtents {
			if delErr := writer.ebsc.Delete([]proto.ObjExtentKey{written}); delErr != nil {
				log.LogWarnf("flushExt: rollback delete ebs extent fail,ino(%v) fileOffset(%v) err(%v)", writer.ino, written.FileOffset, delErr)
			}
		}
	}

	for _, req := range reqs {
		ek := req.NewExtent
		off := ek.FileOffset - bufOff // Calculate offset within buffer: convert file offset to buffer offset

		wSlice := &rwSlice{
			fileOffset: ek.FileOffset,
			size:       uint32(ek.Size),
			Data:       writer.buf[off : off+ek.Size],
		}

		// If old extent exists, need to merge new data with FULL old data. Can't change extent size, only modify its content
		if !req.DiscardExtent.IsEmpty() {
			// Allocate buffer for FULL old extent (may be larger than new data)
			discardExtent := req.DiscardExtent
			data := make([]byte, discardExtent.Size)

			readN, readErr := writer.ebsc.Read(ctx, writer.volName, data, 0, discardExtent.Size, discardExtent)
			if readErr != nil || readN != int(discardExtent.Size) {
				msg := fmt.Sprintf("flushExt: read discard extent from ebs fail,ino(%v) fileOffset(%v) len(%v) readN(%v) err(%v)",
					inode, discardExtent.FileOffset, discardExtent.Size, readN, readErr)
				log.LogError(msg)
				return errors.New(msg)
			}
			log.LogDebugf("flushExt: read discard extent from ebs success,ino(%v) fileOffset(%v) len(%v) readN(%v)",
				inode, discardExtent.FileOffset, discardExtent.Size, readN)

			// Merge new data into old extent: copy new data over old data at corresponding position
			// This preserves the extent size while updating its content. relativeOffset: new data within old extent
			// Copy new data to the correct position in the old extent (not at the beginning)
			relativeOffset := ek.FileOffset - discardExtent.FileOffset
			ret := copy(data[relativeOffset:], wSlice.Data)

			if ret != int(ek.Size) {
				msg := fmt.Sprintf("flushExt: copy discard extent data fail,ino(%v) fileOffset(%v) len(%v) readN(%v) ret(%v)",
					inode, discardExtent.FileOffset, discardExtent.Size, readN, ret)
				log.LogError(msg)
				return errors.New(msg)
			}
			// Update slice to use merged data (full extent size, not just new data size)
			wSlice.size = uint32(discardExtent.Size)
			wSlice.Data = data
			wSlice.fileOffset = discardExtent.FileOffset
		}

		// Write the slice to EBS (either new data or merged data)
		log.LogDebugf("flushExt: write slice ino(%v) fileOffset(%v) len(%v) discardExtent(%v)", inode, wSlice.fileOffset, wSlice.size, req.DiscardExtent)
		if err = writer.writeSlice(ctx, wSlice, false); err != nil {
			if flushFlag {
				atomic.AddUint64(&writer.fileSize, -uint64(bufferSize))
			}
			// Rollback: delete already-written extents (use newExtents: real EBS location keys, not req.NewExtent)
			rollbackFn()
			return
		}

		newExtents = append(newExtents, wSlice.objExtentKey)
		discardExtents = append(discardExtents, req.DiscardExtent)
	}

	// Atomic metadata update: add batch new extent and discard old extent
	if err = writer.mw.AppendObjExtentKeysWithCheck(writer.ino, newExtents, discardExtents); err != nil {
		log.LogErrorf("flushExt: append obj extent keys with check batch fail,ino(%v) count(%v) err(%v)", inode, len(newExtents), err)
		// TODO internal retry? Rollback: delete all written extents in this batch to avoid orphaned data on EBS
		// rollbackFn()
		// return
	}
	log.LogDebugf("flushExt: append obj extent keys with check batch success,ino(%v) count(%v)", inode, len(newExtents))
	return nil
}

// flushExt flushes buffer data with overwrite logic. It handles partial overwrite scenarios by:
// 1. Getting existing extents from metadata
// 2. Computing overwrite requests (which parts to write, which old extents to discard)
// 3. Applying requests sequentially (read old data, merge with new, write, update metadata)
// This function is called when buffer is full during overwrite operations, or write offset not match file offset
func (writer *Writer) flushExt(inode uint64, ctx context.Context, flushFlag bool) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("blobstore-flushExt", err, bgTime, 1)
	}()

	log.LogDebugf("flushExt: ino(%v) buf-len(%v) flushFlag(%v) fileOffset(%v) blockPosition(%v)",
		inode, len(writer.buf), flushFlag, writer.fileOffset, writer.blockPosition)

	defer func() { writer.dirty = false }()

	// Early return if buffer is empty or not dirty (nothing to flush)
	if len(writer.buf) == 0 || !writer.dirty {
		return
	}

	// Get existing extents from metadata to determine overlaps, old data already stored in EBS
	_, _, _, objExtents, err := writer.mw.GetObjExtents(inode)
	if err != nil {
		log.LogErrorf("flushExt: get obj extents fail,ino(%v) err(%v)", inode, err)
		return err
	}
	sort.Slice(objExtents, func(i, j int) bool {
		return objExtents[i].FileOffset < objExtents[j].FileOffset
	})

	// Calculate buffer range in file coordinates bufferSize is the amount of data in buffer (from 0 to blockPosition)
	bufferSize := writer.blockPosition
	if writer.fileOffset < bufferSize {
		err = fmt.Errorf("flushExt: inconsistent state ino(%v) fileOffset(%v) < blockPosition(%v)", inode, writer.fileOffset, bufferSize)
		log.LogErrorf(err.Error())
		return err
	}
	start := uint64(writer.fileOffset - bufferSize)
	end := uint64(writer.fileOffset)

	// Compute overwrite requests: determine which parts of buffer overlap with existing extents
	// This generates a slice of requests, each specifying:
	reqs := computeOverwriteReqs(start, end, objExtents)
	log.LogDebugf("flushExt: ino(%v) start(%v) end(%v) reqsCount(%v)", inode, start, end, len(reqs))

	// Apply overwrite requests: write data and update metadata
	if err = writer.flushOverwriteReqs(ctx, inode, reqs, start, bufferSize, flushFlag); err != nil {
		return
	}

	// Reset buffer after successful flush: clear blockPosition for next write
	writer.resetBuffer()
	return
}

func (writer *Writer) flush(inode uint64, ctx context.Context, flushFlag bool) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("blobstore-flush", err, bgTime, 1)
	}()

	log.LogDebugf("flush: TRACE blobStore flush: ino(%v) buf-len(%v) flushFlag(%v) fileOffset(%v) blockPosition(%v)",
		inode, len(writer.buf), flushFlag, writer.fileOffset, writer.blockPosition)

	writer.Lock()
	defer func() {
		writer.dirty = false
		writer.Unlock()
	}()

	if len(writer.buf) == 0 || !writer.dirty {
		return
	}

	bufferSize := writer.blockPosition
	if writer.fileOffset < bufferSize {
		err = fmt.Errorf("flush: inconsistent state ino(%v) fileOffset(%v) < blockPosition(%v)", inode, writer.fileOffset, bufferSize)
		log.LogErrorf(err.Error())
		return err
	}

	wSlice := &rwSlice{
		fileOffset: uint64(writer.fileOffset - bufferSize),
		size:       uint32(bufferSize),
		Data:       writer.buf,
	}
	err = writer.writeSlice(ctx, wSlice, false)
	if err != nil {
		if flushFlag {
			atomic.AddUint64(&writer.fileSize, -uint64(bufferSize))
		}
		return
	}

	oeks := make([]proto.ObjExtentKey, 0)
	// update meta
	oeks = append(oeks, wSlice.objExtentKey)
	if err = writer.mw.AppendObjExtentKeys(writer.ino, oeks); err != nil {
		log.LogErrorf("flush: slice write error,meta append ebsc extent keys fail,ino(%v) fileOffset(%v) len(%v) err(%v)", inode, wSlice.fileOffset, wSlice.size, err)
		// Rollback: delete orphaned data on EBS to avoid blobstore leak
		if delErr := writer.ebsc.Delete(oeks); delErr != nil {
			log.LogWarnf("flush: rollback delete ebs extent fail,ino(%v) fileOffset(%v) err(%v)", inode, wSlice.fileOffset, delErr)
		}
		return
	}
	writer.resetBuffer()
	return
}

func (writer *Writer) CacheFileSize() int {
	return int(atomic.LoadUint64(&writer.fileSize))
}

// SetFileSize 用于 Truncate 后同步 writer 内部 fileSize，使后续 Append/Write 与 CacheFileSize() 与 meta 一致。
func (writer *Writer) SetFileSize(size uint64) {
	atomic.StoreUint64(&writer.fileSize, size)
}

// TruncateV2 执行 EBS 侧截断：通过 writer.mw 拉取当前 ObjExtents，通过 writer.ebsc 执行读/截断/写/删，返回新 extent 列表供上层调用 meta.TruncateV2。
// 调用链：client/file.doECTruncateV2 → Writer.TruncateV2 → writer.ebsc.TruncateV2Extents（BlobStoreClient）。
func (writer *Writer) TruncateV2(ctx context.Context, targetSize uint64) (newObjExtents []proto.ObjExtentKey, err error) {
	if writer == nil || writer.mw == nil || writer.ebsc == nil {
		return nil, fmt.Errorf("Writer.TruncateV2: writer/mw/ebsc nil")
	}
	_, currentSize, _, objExtents, err := writer.mw.GetObjExtents(writer.ino)
	if err != nil {
		log.LogErrorf("TruncateV2: ino(%v) GetObjExtents err(%v)", writer.ino, err)
		return nil, err
	}
	if targetSize >= currentSize {
		return objExtents, nil
	}
	return writer.ebsc.TruncateV2Extents(ctx, writer.volName, objExtents, targetSize)
}

func (writer *Writer) FreeCache() {
	if writer == nil {
		return
	}
	if buf.CachePool == nil {
		return
	}
	writer.once.Do(func() {
		tmpBuf := writer.buf
		writer.buf = nil
		if tmpBuf != nil {
			buf.CachePool.Put(tmpBuf)
		}
	})
}

func (writer *Writer) allocateCache() {
	if buf.CachePool == nil {
		return
	}
	writer.buf = buf.CachePool.Get()
}
