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

type reqExtent struct {
	Extents       proto.ObjExtentKey
	DiscardExtent proto.ObjExtentKey
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
	overwrite     bool
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
	// atomic.StoreInt32(&writer.idle, 0)
	if writer == nil {
		return 0, fmt.Errorf("writer is not opened yet")
	}
	log.LogDebugf("TRACE blobStore Write Enter: ino(%v) offset(%v) len(%v) flags&proto.FlagsAppend(%v) fileSize(%v)", writer.ino, offset, len(data), flags&proto.FlagsAppend, writer.CacheFileSize())

	if len(data) > MaxBufferSize || flags&proto.FlagsAppend == 0 || offset != writer.CacheFileSize() || writer.overwrite {
		if len(data) < MaxBufferSize && flags&proto.FlagsAppend != 0 || writer.overwrite {
			return writer.tryOverWrite(ctx, offset, data, flags)
		}

		log.LogErrorf("TRACE blobStore Write error,may be len(%v)>512MB,flags(%v)!=flagAppend,offset(%v)!=fileSize(%v)", len(data), flags&proto.FlagsAppend, offset, writer.CacheFileSize())
		return 0, syscall.EOPNOTSUPP
	}
	// write buffer
	log.LogDebugf("TRACE blobStore Write: ino(%v) offset(%v) len(%v) flags&proto.FlagsSyncWrite(%v)", writer.ino, offset, len(data), flags&proto.FlagsSyncWrite)
	if flags&proto.FlagsSyncWrite == 0 {
		size, err = writer.doBufferWrite(ctx, data, offset)
		return
	}
	// parallel io write ebs direct
	size, err = writer.doParallelWrite(ctx, data, offset)
	return
}

func (writer *Writer) tryOverWrite(ctx context.Context, offset int, data []byte, flags int) (size int, err error) {
	if writer == nil {
		return 0, fmt.Errorf("writer is not opened yet")
	}
	log.LogDebugf("TRACE blobStore WriteWithCheck Enter: ino(%v) offset(%v) len(%v) flags(%v)", writer.ino, offset, len(data), flags)

	writer.overwrite = true

	writer.RLock()
	defer writer.RUnlock()

	if offset != writer.fileOffset {
		// flush first
		err = writer.flushExt(writer.ino, ctx, false, true)
		if err != nil {
			log.LogErrorf("TRACE blobStore tryOverWrite error,flush ext fail,ino(%v) offset(%v) len(%v) flags(%v) err(%v)",
				writer.ino, offset, len(data), flags, err)
			return 0, err
		}
	}

	// write to buffer, if buffer over block size, flush and overwrite
	writer.fileOffset = offset
	dataSize := len(data)
	position := 0
	log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)

	for dataSize > 0 {
		freeSize := writer.blockSize - writer.blockPosition
		if dataSize < freeSize {
			freeSize = dataSize
		}

		copy(writer.buf[writer.blockPosition:], data[position:position+freeSize])
		position += freeSize
		writer.blockPosition += freeSize
		dataSize -= freeSize
		writer.fileOffset += freeSize
		writer.dirty = true

		log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.fileSize(%v) writer.fileOffset(%v) writer.blockPosition(%v) position(%v) freeSize(%v)",
			writer.ino, writer.fileSize, writer.fileOffset, writer.blockPosition, position, freeSize)
		// if buffer over block size, flush and overwrite
		if writer.blockPosition == writer.blockSize {
			log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ino, len(writer.buf), writer.blockSize)
			// writer.Unlock()
			err = writer.flushExt(writer.ino, ctx, false, true)
			// writer.Lock()
			if err != nil {
				writer.buf = writer.buf[:writer.blockPosition-freeSize]
				writer.fileOffset -= freeSize
				writer.blockPosition -= freeSize
				return
			}
		}
	}

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
				writer.buf = writer.buf[:len(writer.buf)-len(data)]
				writer.fileOffset -= len(data)
				return
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

	if writer.overwrite {
		writer.Lock()
		defer writer.Unlock()
		return writer.flushExt(ino, ctx, true, true)
	}

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
		return
	}
	writer.resetBufferWithoutPool()
	return
}

func (writer *Writer) flushExt(inode uint64, ctx context.Context, flushFlag bool, overwrite bool) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("blobstore-flushExt", err, bgTime, 1)
	}()

	log.LogDebugf("flushExt: TRACE blobStore flush: ino(%v) buf-len(%v) flushFlag(%v) overwrite(%v) fileOffset(%v) blockPosition(%v)",
		inode, len(writer.buf), flushFlag, overwrite, writer.fileOffset, writer.blockPosition)

	// writer.Lock()
	defer func() {
		writer.dirty = false
		// writer.Unlock()
	}()

	if len(writer.buf) == 0 || !writer.dirty {
		return
	}

	// get exist extents from meta
	_, _, _, objExtents, err1 := writer.mw.GetObjExtents(inode)
	if err1 != nil {
		log.LogErrorf("flushExt: get obj extents fail,ino(%v) err(%v)", inode, err1)
		return err1
	}

	sort.Slice(objExtents, func(i, j int) bool {
		return objExtents[i].FileOffset < objExtents[j].FileOffset
	})

	bufferSize := writer.blockPosition
	reqs := make([]reqExtent, 0)
	start := uint64(writer.fileOffset - writer.blockPosition)
	end := uint64(writer.fileOffset)

	// find overlap extent from exist extents
	for _, ek := range objExtents {
		if end <= ek.FileOffset {
			reqs = append(reqs, reqExtent{
				Extents: proto.ObjExtentKey{
					FileOffset: start,
					Size:       end - start,
				},
				DiscardExtent: proto.ObjExtentKey{},
			})
			log.LogDebugf("flushExt: TRACE blobStore flushExt: ino(%v) start(%v) end(%v) ek(%v) reqs(%v)", inode, start, end, ek, reqs)
			break
		}

		if ek.FileOffset > start {
			reqs = append(reqs, reqExtent{
				Extents: proto.ObjExtentKey{
					FileOffset: start,
					Size:       ek.FileOffset - start,
				},
				DiscardExtent: proto.ObjExtentKey{},
			})
			log.LogDebugf("flushExt: TRACE blobStore flushExt: ino(%v) start(%v) end(%v) ek(%v) reqs(%v)", inode, start, end, ek, reqs)
			start = ek.FileOffset
		}

		reqSize := end - start
		if end > ek.FileOffset+ek.Size {
			reqSize = ek.FileOffset + ek.Size - start
		}

		reqs = append(reqs, reqExtent{
			Extents: proto.ObjExtentKey{
				FileOffset: start,
				Size:       reqSize,
			},
			DiscardExtent: ek,
		})
		log.LogDebugf("flushExt: TRACE blobStore flushExt: ino(%v) start(%v) end(%v) ek(%v) reqSize(%v)", inode, start, end, ek, reqSize)
		start = start + reqSize
		if end <= ek.FileOffset+ek.Size {
			break
		}
	}

	if start < end {
		log.LogDebugf("flushExt: TRACE blobStore flushExt: add last ek, ino(%v) start(%v) end(%v)", inode, start, end)
		reqs = append(reqs, reqExtent{
			Extents: proto.ObjExtentKey{
				FileOffset: start,
				Size:       end - start,
			},
			DiscardExtent: proto.ObjExtentKey{},
		})
	}

	log.LogDebugf("flushExt: TRACE blobStore flushExt: ino(%v) start(%v) end(%v) reqs(%v)", inode, start, end, reqs)

	bufOff := uint64(writer.fileOffset - bufferSize)
	for _, req := range reqs {
		ek := req.Extents
		off := ek.FileOffset - bufOff

		wSlice := &rwSlice{
			fileOffset: ek.FileOffset,
			size:       uint32(ek.Size),
			Data:       writer.buf[off : off+ek.Size],
		}

		if !req.DiscardExtent.IsEmpty() {
			// read discard extent from ebs
			discardExtent := req.DiscardExtent
			data := make([]byte, discardExtent.Size)
			readN, err := writer.ebsc.Read(ctx, writer.volName, data, 0, discardExtent.Size, discardExtent)
			if err != nil || readN != int(discardExtent.Size) {
				msg := fmt.Sprintf("flushExt: read discard extent from ebs fail,ino(%v) fileOffset(%v) len(%v) readN(%v) err(%v)",
					inode, discardExtent.FileOffset, discardExtent.Size, readN, err)
				log.LogError(msg)
				return errors.New(msg)
			}

			log.LogDebugf("flushExt: read discard extent from ebs success,ino(%v) fileOffset(%v) len(%v) readN(%v)",
				inode, discardExtent.FileOffset, discardExtent.Size, readN)

			ret := copy(data, wSlice.Data)
			if ret != int(ek.Size) {
				msg := fmt.Sprintf("flushExt: copy discard extent data fail,ino(%v) fileOffset(%v) len(%v) readN(%v) ret(%v)",
					inode, discardExtent.FileOffset, discardExtent.Size, readN, ret)
				log.LogError(msg)
				return errors.New(msg)
			}

			wSlice.size = uint32(discardExtent.Size)
			wSlice.Data = data
			wSlice.fileOffset = discardExtent.FileOffset
		}

		log.LogDebugf("flushExt: TRACE blobStore flushExt: write slice, ino(%v) fileOffset(%v) len(%v) objExtentKey(%v) discardExtent(%v)",
			inode, wSlice.fileOffset, wSlice.size, wSlice.objExtentKey, req.DiscardExtent)
		err = writer.writeSlice(ctx, wSlice, false)
		if err != nil {
			if flushFlag {
				atomic.AddUint64(&writer.fileSize, -uint64(bufferSize))
			}
			return
		}

		log.LogDebugf("flushExt: write slice success,ino(%v) fileOffset(%v) len(%v) objExtentKey(%v) discardExtent(%v)",
			inode, wSlice.fileOffset, wSlice.size, wSlice.objExtentKey, req.DiscardExtent)

		err = writer.mw.AppendObjExtentKeysWithCheck(writer.ino, wSlice.objExtentKey, req.DiscardExtent)
		if err != nil {
			log.LogErrorf("flushExt: append obj extent keys with check fail,ino(%v) fileOffset(%v) len(%v) err(%v)", inode, wSlice.fileOffset, wSlice.size, err)
			return
		}

		log.LogDebugf("flushExt: append obj extent keys with check success,ino(%v) fileOffset(%v) len(%v) objExtentKey(%v) discardExtent(%v)",
			inode, wSlice.fileOffset, wSlice.size, wSlice.objExtentKey, req.DiscardExtent)
	}

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
		return
	}
	writer.resetBuffer()
	return
}

func (writer *Writer) CacheFileSize() int {
	return int(atomic.LoadUint64(&writer.fileSize))
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
