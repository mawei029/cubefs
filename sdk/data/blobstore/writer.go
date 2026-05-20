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
	"syscall"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/manager"
	"github.com/cubefs/cubefs/util"
	"github.com/cubefs/cubefs/util/buf"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/stat"
)

const (
	MaxBufferSize = 512 * util.MB
	// slowOpInfoThreshold 仅当单次操作超过该时长时打一条 LogInfof（LTP/rwtest 排障，正常路径无输出）。
	slowOpInfoThreshold = 10 * time.Second
)

var errPutNoKeys = errors.New("ebs put returned no extent keys")

// overwriteReq represents an overwrite request that contains both the new extent and the old extent
// Same structure used by computeOverwriteReqs/flushOverwriteReqs; TruncateV2 reuses this type.
// overwriteReq describes one overwrite request: new written range NewExtent and old range to discard DiscardExtent (optional).
// Shared by writer flushExt/computeOverwriteReqs; in TruncateV2 reuse there is at most one partially-overlapped request.
type overwriteReq struct {
	NewExtent     proto.ObjExtentKey // Newly written range (trimmed range when partial overlap happens).
	DiscardExtent proto.ObjExtentKey // Old extent to discard (optional).
}

// truncateReq is TruncateV2 planning result: kept extents, at most one partially-overlapped OverwriteReq, and delete-only extents.
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
	err           chan *wSliceErr
	wConcurrency  int
	wg            sync.WaitGroup
	once          sync.Once
	buf           []byte
	fileOffset    int
	blockPosition int
	limitManager  *manager.LimitManager
	ecStreamer    *ECStreamer // 与所属 ECStreamer 绑定；须非 nil（见 NewWriter）。
}

func NewWriter(config ClientConfig) (writer *Writer) {
	if config.ECStreamer == nil {
		panic("blobstore.NewWriter: ClientConfig.ECStreamer is required")
	}
	writer = new(Writer)

	writer.err = nil
	writer.wConcurrency = config.WConcurrency
	writer.wg = sync.WaitGroup{}
	writer.once = sync.Once{}
	writer.allocateCache()
	writer.limitManager = config.LimitManager
	writer.ecStreamer = config.ECStreamer

	return
}

func (writer *Writer) notifyAfterWrite() {
	// 顺序写缓冲：抬高 fileSize（max）+ dirty；oeks 在 flush 后由 updateMetaInfo 刷新。
	// writer.ecStreamer.noteWriteFinished(uint64(writer.fileOffset))
	writer.ecStreamer.raiseFileSize(uint64(writer.fileOffset))
	writer.ecStreamer.markDirty()
	writer.ecStreamer.invalidateReaderPrefetchBuf()
}

// notifyCompleteFlushMeta 在 EBS/meta 已提交后调用：resetBuffer → updateMetaInfo → cleanDirty（统一 flush 收尾，只维护 dirty 即可）。
func (writer *Writer) notifyCompleteFlushMeta() error {
	writer.resetBuffer()
	if len(writer.buf) > 0 {
		writer.resetBufferWithoutPool()
	}
	if err := writer.ecStreamer.updateMetaInfo(false, nil); err != nil {
		return err
	}
	writer.ecStreamer.raiseFileSize(uint64(writer.fileOffset))
	writer.ecStreamer.cleanDirty()
	writer.ecStreamer.invalidateReaderPrefetchBuf()
	return nil
}

func (writer *Writer) String() string {
	return fmt.Sprintf("Writer{address(%v),volName(%v),ino(%v),blockSize(%v),fileSize(%v)},wConcurrency(%v)",
		&writer, writer.ecStreamer.Volume(), writer.ecStreamer.Inode(), writer.ecStreamer.BlockSize(), writer.ecStreamer.fileSizeView(), writer.wConcurrency)
}

func (writer *Writer) WriteWithoutPool(ctx context.Context, offset int, data []byte) (size int, err error) {
	// atomic.StoreInt32(&writer.idle, 0)
	if writer == nil {
		log.LogErrorf("Writer WriteWithoutPool: writer is nil")
		return 0, fmt.Errorf("writer is not opened yet")
	}
	log.LogDebugf("TRACE blobStore WriteWithoutPool Enter: ino(%v) offset(%v) len(%v) fileSize(%v)",
		writer.ecStreamer.Inode(), offset, len(data), writer.CacheFileSize())

	if len(data) > MaxBufferSize || offset != writer.CacheFileSize() {
		log.LogErrorf("TRACE blobStore WriteWithoutPool error,may be len(%v)>512MB,offset(%v)!=fileSize(%v)",
			len(data), offset, writer.CacheFileSize())
		err = syscall.EOPNOTSUPP
		return
	}
	// write buffer
	log.LogDebugf("TRACE blobStore WriteWithoutPool: ino(%v) offset(%v) len(%v)",
		writer.ecStreamer.Inode(), offset, len(data))

	size, err = writer.doBufferWriteWithoutPool(ctx, data, offset)
	if err == nil {
		writer.notifyAfterWrite()
	}
	return
}

func (writer *Writer) Write(ctx context.Context, offset int, data []byte, flags int) (size int, err error) {
	if writer == nil {
		log.LogErrorf("Writer Write: writer is nil")
		return 0, fmt.Errorf("writer is not opened yet")
	}
	log.LogDebugf("TRACE blobStore Write Enter: ino(%v) offset(%v) len(%v) flags&proto.FlagsAppend(%v) fileSize(%v) seqAppend(%t)",
		writer.ecStreamer.Inode(), offset, len(data), flags&proto.FlagsAppend, writer.CacheFileSize(), offset == writer.CacheFileSize())

	// Case 1: Validate write request: data too large
	if len(data) > MaxBufferSize {
		log.LogErrorf("blobStore Write error,may be len(%v)>512MB,offset(%v) fileSize(%v)",
			len(data), offset, writer.CacheFileSize())
		return 0, syscall.EOPNOTSUPP
	}

	// Case 1.1: O_APPEND - kernel guarantees append-to-end, so offset must equal CacheFileSize.
	if flags&proto.FlagsAppend != 0 && offset != writer.CacheFileSize() {
		log.LogErrorf("filesize need reset. blobStore Write: ino(%v) offset(%v) len(%v) flags&proto.FlagsAppend(%v) fileSize(%v) seqAppend(%t)",
			writer.ecStreamer.Inode(), offset, len(data), flags&proto.FlagsAppend, writer.CacheFileSize(), offset == writer.CacheFileSize())
		return 0, syscall.EOPNOTSUPP
	}

	// Case 2: pwrite - offset < current size (overwrite) or > current size (sparse / hole-then-write) both go through tryOverWrite to merge with extents or create new range after hole.
	// Only offset == CacheFileSize() is sequential append, which goes through buffered/direct-write path.
	if offset != writer.CacheFileSize() {
		// tryOverWrite 内 flushExt/notifyCompleteFlushMeta 已刷新 meta；勿在 Write 外层重复 updateMetaInfo。
		return writer.tryOverWrite(ctx, offset, data, flags)
	}

	// Case 3: Sequential append write: data is appended to the end of file
	// Case 3.1: with buffer: Use buffered write for better performance (data stays in buffer until flush)
	if flags&proto.FlagsSyncWrite == 0 {
		size, err = writer.doBufferWrite(ctx, data, offset)
		// 这里不做flush，所以只标记dirty为true，更新版本号，fileSize。不更新oeks 。 为了fileVersion2拿到gen和size
		if err == nil {
			writer.notifyAfterWrite()
		}
		return
	}

	// Case 3.2: Synchronous write: write directly to EBS without buffering
	// This ensures data is immediately persisted but has lower throughput
	size, err = writer.doParallelWrite(ctx, data, offset)
	if err == nil {
		if err = writer.notifyCompleteFlushMeta(); err != nil {
			return size, err
		}
	}
	return
}

// tryOverWrite handles overwrite by buffering data and flushing with extent merge logic
func (writer *Writer) tryOverWrite(ctx context.Context, offset int, data []byte, flags int) (size int, err error) {
	if writer == nil {
		log.LogErrorf("Writer tryOverWrite: writer is nil")
		return 0, fmt.Errorf("writer is not opened yet")
	}

	writer.ecStreamer.markDirty()

	if offset != writer.fileOffset {
		// Flush existing buffer data before starting new write at different offset
		if err = writer.flushExt(writer.ecStreamer.Inode(), ctx, false); err != nil {
			log.LogErrorf("TRACE blobStore tryOverWrite error,flush ext fail,ino(%v) offset(%v) len(%v) flags(%v) err(%v)",
				writer.ecStreamer.Inode(), offset, len(data), flags, err)
			return 0, err
		}
		writer.fileOffset = offset
	}
	if buf.CachePool != nil && writer.buf == nil {
		writer.allocateCache()
	}
	writer.reshapeBufForCopyPath()

	remainSize, position := len(data), 0
	log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.ecStreamer.BlockSize())

	// The loop will write data to buffer in blocks, flushing when buffer is full
	// Process data in chunks until all data is written to buffer
	for remainSize > 0 {
		freeSize := writer.ecStreamer.BlockSize() - writer.blockPosition
		if freeSize <= 0 {
			log.LogErrorf("tryOverWrite: invalid freeSize ino(%v) blockPosition(%v) blockSize(%v) bufLen(%v)",
				writer.ecStreamer.Inode(), writer.blockPosition, writer.ecStreamer.BlockSize(), len(writer.buf))
			return 0, syscall.EINVAL
		}
		if remainSize < freeSize {
			freeSize = remainSize
		}

		// Copy data and update position: advance in both input data and buffer
		if writer.buf == nil || len(writer.buf) < writer.blockPosition+freeSize {
			log.LogErrorf("tryOverWrite: buf too short ino(%v) bufLen(%v) needEnd(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.blockPosition+freeSize)
			return 0, syscall.EINVAL
		}
		copy(writer.buf[writer.blockPosition:], data[position:position+freeSize])
		position += freeSize             // Move forward in input data
		writer.blockPosition += freeSize // Move forward in buffer
		remainSize -= freeSize           // Decrease remaining data to process
		writer.fileOffset += freeSize
		writer.ecStreamer.markDirty()
		writer.ecStreamer.raiseFileSize(uint64(writer.fileOffset))

		log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) cacheFileSize(%v) writer.fileOffset(%v) writer.blockPosition(%v) position(%v) freeSize(%v)",
			writer.ecStreamer.Inode(), writer.CacheFileSize(), writer.fileOffset, writer.blockPosition, position, freeSize)

		// Check buffer is full: when position are equal, buffer is completely filled. we flush buffer and continue
		if writer.blockPosition == writer.ecStreamer.BlockSize() {
			log.LogDebugf("TRACE blobStore tryOverWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.ecStreamer.BlockSize())
			// Flush buffer with overwrite logic: this will handle extent overlap and discard old data
			err = writer.flushExt(writer.ecStreamer.Inode(), ctx, false)
			if err != nil {
				// Rollback the state to maintain consistency, remove the failed buffer and revert position pointers
				writer.buf = writer.buf[:writer.blockPosition-freeSize]
				writer.fileOffset -= freeSize
				writer.ecStreamer.setFileSize(uint64(writer.fileOffset))
				writer.blockPosition -= freeSize
				return 0, err
			}
			// After successful flush, blockPosition is reset to 0 (in flushExt -> resetBuffer). buffer is empty and ready for next chunk
		}
	}

	// Partial block data still in buf must be flushed, otherwise Read (which uses EBS/meta) will not observe this pwrite/sparse write.
	if writer.blockPosition > 0 {
		if err = writer.flushExt(writer.ecStreamer.Inode(), ctx, false); err != nil {
			log.LogErrorf("TRACE blobStore tryOverWrite error, final flush ext fail,ino(%v) offset(%v) len(%v) err(%v)",
				writer.ecStreamer.Inode(), offset, len(data), err)
			return 0, err
		}
	}

	// 循环内 flushExt 已在 notifyCompleteFlushMeta 中 updateMetaInfo + 清 dirty。
	if writer.ecStreamer.isDirty() {
		if err = writer.notifyCompleteFlushMeta(); err != nil {
			return 0, err
		}
	}

	log.LogDebugf("TRACE blobStore tryOverWrite Exit: ino(%v) cacheFileSize(%v) writer.fileOffset(%v)",
		writer.ecStreamer.Inode(), writer.CacheFileSize(), writer.fileOffset)
	return len(data), nil
}

func (writer *Writer) doParallelWrite(ctx context.Context, data []byte, offset int) (size int, err error) {
	log.LogDebugf("TRACE blobStore doDirectWrite: ino(%v) offset(%v) len(%v)", writer.ecStreamer.Inode(), offset, len(data))

	// O_SYNC 与其它 fd 的缓冲写共享同一 Writer：必须先落盘缓冲尾，再追加本次直写 extent，否则 meta/EBS 顺序错乱会导致 Append 失败或 EIO（LTP rwtest O_SYNC）。
	// 注意：此处已持 Lock，禁止调用 flush()（其内部会再 Lock 死锁）；flushExt 不加锁，且覆盖纯追加与覆盖写缓冲。
	// flushFlag 与 Writer.Flush 一致用 true，保证失败回滚 fileSize 与显式 Flush 路径一致。
	if writer.ecStreamer.isDirty() {
		if err = writer.flushExt(writer.ecStreamer.Inode(), ctx, true); err != nil {
			log.LogErrorf("doParallelWrite: flush buffered data before sync write fail ino(%v) offset(%v) err(%v)", writer.ecStreamer.Inode(), offset, err)
			return 0, err
		}
	}

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
				writer.ecStreamer.Inode(), wErr.fileOffset, wErr.size, wErr.err)
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
	if err = writer.ecStreamer.Mw().AppendObjExtentKeys(writer.ecStreamer.Inode(), oeks); err != nil {
		log.LogErrorf("slice write error,meta append ebsc extent keys fail,ino(%v) fileOffset(%v) len(%v) err(%v)", writer.ecStreamer.Inode(), offset, len(data), err)
		return
	}
	writer.fileOffset = offset + size
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
		if bufSize == writer.ecStreamer.BlockSize() || (leftToWrite == 0 && err == io.EOF) {
			wSlice := &rwSlice{
				fileOffset: uint64(writer.fileOffset - bufSize),
				size:       uint32(bufSize),
			}
			wSlice.Data = make([]byte, bufSize)
			copy(wSlice.Data, writer.buf)
			writer.resetBufferWithoutPool()
			if (err == nil || err == io.EOF) && h != nil {
				h.Write(wSlice.Data)
				log.LogDebugf("writeBuff: bufSize(%v), md5", bufSize)
			}
			writer.wg.Add(1)

			write := func() {
				defer writer.wg.Done()
				err := writer.writeSlice(ctx, wSlice, false)
				if err != nil {
					if len(writer.err) > 0 {
						return
					}
					wErr := &wSliceErr{
						err:        err,
						fileOffset: wSlice.fileOffset,
						size:       wSlice.size,
					}
					writer.err <- wErr
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
			errNum := len(writer.err)
			if errNum > 0 {
				break LOOP
			}

			freeSize := writer.ecStreamer.BlockSize() - len(writer.buf)
			writeSize := util.Min(leftToWrite, freeSize)
			writer.buf = append(writer.buf, tmp[position:position+writeSize]...)
			position += writeSize
			leftToWrite -= writeSize
			writer.fileOffset += writeSize
			writer.ecStreamer.markDirty()
			writer.ecStreamer.raiseFileSize(uint64(writer.fileOffset))

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
				log.LogErrorf("slice write error,ino(%v) fileoffset(%v)  sliceSize(%v) err(%v)", writer.ecStreamer.Inode(), wErr.fileOffset, wErr.size, err)
			}
			break
		}
	}

	log.LogDebugf("WriteFromReader before sort: %v", oeks)
	sort.Slice(oeks, func(i, j int) bool {
		return oeks[i].FileOffset < oeks[j].FileOffset
	})
	log.LogDebugf("WriteFromReader after sort: %v", oeks)
	if err = writer.ecStreamer.Mw().AppendObjExtentKeys(writer.ecStreamer.Inode(), oeks); err != nil {
		log.LogErrorf("WriteFromReader error,meta append ebsc extent keys fail,ino(%v), err(%v)", writer.ecStreamer.Inode(), err)
		return
	}

	if err == nil {
		if uerr := writer.notifyCompleteFlushMeta(); uerr != nil {
			return size, uerr
		}
	}
	return
}

func (writer *Writer) doBufferWriteWithoutPool(ctx context.Context, data []byte, offset int) (size int, err error) {
	log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool Enter: ino(%v) offset(%v) len(%v)", writer.ecStreamer.Inode(), offset, len(data))

	writer.fileOffset = offset
	dataSize := len(data)
	position := 0
	log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.ecStreamer.BlockSize())

	for dataSize > 0 {
		freeSize := writer.ecStreamer.BlockSize() - len(writer.buf)
		if dataSize < freeSize {
			freeSize = dataSize
		}
		log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool: ino(%v) writer.fileSize(%v) writer.fileOffset(%v) position(%v) freeSize(%v)",
			writer.ecStreamer.Inode(), writer.CacheFileSize(), writer.fileOffset, position, freeSize)
		writer.buf = append(writer.buf, data[position:position+freeSize]...)
		log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool:ino(%v) writer.buf.len(%v)", writer.ecStreamer.Inode(), len(writer.buf))
		position += freeSize
		dataSize -= freeSize
		writer.fileOffset += freeSize
		writer.ecStreamer.markDirty()
		writer.ecStreamer.raiseFileSize(uint64(writer.fileOffset))

		if len(writer.buf) == writer.ecStreamer.BlockSize() {
			log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.ecStreamer.BlockSize())
			err = writer.flushWithoutPool(writer.ecStreamer.Inode(), ctx, false)
			if err != nil {
				// Revert only this iteration's append (already-flushed prefixes must not be rolled back).
				if freeSize > len(writer.buf) {
					log.LogErrorf("doBufferWriteWithoutPool: rollback len err ino(%v) freeSize(%v) bufLen(%v)", writer.ecStreamer.Inode(), freeSize, len(writer.buf))
					return 0, err
				}
				writer.buf = writer.buf[:len(writer.buf)-freeSize]
				writer.fileOffset -= freeSize
				writer.ecStreamer.setFileSize(uint64(writer.fileOffset))
				return 0, err
			}
		}
	}

	size = len(data)

	log.LogDebugf("TRACE blobStore doBufferWriteWithoutPool Exit: ino(%v) writer.fileSize(%v) writer.fileOffset(%v)",
		writer.ecStreamer.Inode(), writer.CacheFileSize(), writer.fileOffset)
	return size, nil
}

func (writer *Writer) doBufferWrite(ctx context.Context, data []byte, offset int) (size int, err error) {
	log.LogDebugf("TRACE blobStore doBufferWrite Enter: ino(%v) offset(%v) len(%v)", writer.ecStreamer.Inode(), offset, len(data))

	writer.ecStreamer.markDirty()
	writer.fileOffset = offset
	dataSize := len(data)
	position := 0
	log.LogDebugf("TRACE blobStore doBufferWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.ecStreamer.BlockSize())
	if buf.CachePool != nil && writer.buf == nil {
		writer.allocateCache()
	}

	// 需要区分新写buffer，跨block的，generate需要变化. 每一次进来，都判writer.blockPosition是否为0，为0，则需要更新streamer里的version
	if writer.blockPosition == 0 {
		writer.ecStreamer.raiseInodeVersion()
	}

	writer.reshapeBufForCopyPath()
	for dataSize > 0 {
		freeSize := writer.ecStreamer.BlockSize() - writer.blockPosition
		if freeSize <= 0 {
			log.LogErrorf("doBufferWrite: invalid freeSize ino(%v) blockPosition(%v) blockSize(%v) bufLen(%v)",
				writer.ecStreamer.Inode(), writer.blockPosition, writer.ecStreamer.BlockSize(), len(writer.buf))
			return 0, syscall.EINVAL
		}
		if dataSize < freeSize {
			freeSize = dataSize
		}
		log.LogDebugf("TRACE blobStore doBufferWrite: ino(%v) writer.fileSize(%v) writer.fileOffset(%v) writer.blockPosition(%v) position(%v) freeSize(%v)",
			writer.ecStreamer.Inode(), writer.CacheFileSize(), writer.fileOffset, writer.blockPosition, position, freeSize)
		if writer.buf == nil || len(writer.buf) < writer.blockPosition+freeSize {
			log.LogErrorf("doBufferWrite: buf too short ino(%v) bufLen(%v) needEnd(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.blockPosition+freeSize)
			return 0, syscall.EINVAL
		}
		copy(writer.buf[writer.blockPosition:], data[position:position+freeSize])
		log.LogDebugf("TRACE blobStore doBufferWrite:ino(%v) writer.buf.len(%v)", writer.ecStreamer.Inode(), len(writer.buf))
		position += freeSize
		writer.blockPosition += freeSize
		dataSize -= freeSize
		writer.fileOffset += freeSize
		writer.ecStreamer.raiseFileSize(uint64(writer.fileOffset))

		if writer.blockPosition == writer.ecStreamer.BlockSize() {
			log.LogDebugf("TRACE blobStore doBufferWrite: ino(%v) writer.buf.len(%v) writer.blocksize(%v)", writer.ecStreamer.Inode(), len(writer.buf), writer.ecStreamer.BlockSize())
			err = writer.flushExt(writer.ecStreamer.Inode(), ctx, false)
			if err != nil {
				writer.buf = writer.buf[:writer.blockPosition-freeSize]
				writer.fileOffset -= freeSize
				writer.ecStreamer.setFileSize(uint64(writer.fileOffset))
				writer.blockPosition -= freeSize
				log.LogWarnf("doBufferWrite: flush error ino(%v) fileOffset(%v) blockPosition(%v) freeSize(%v)", writer.ecStreamer.Inode(), writer.fileOffset, writer.blockPosition, freeSize)
				return
			}
		}
	}

	size = len(data)
	// logical 尾由 Write 成功返回后 notifyAfterWrite 抬高。

	log.LogDebugf("TRACE blobStore doBufferWrite Exit: ino(%v) writer.fileSize(%v) writer.fileOffset(%v)",
		writer.ecStreamer.Inode(), writer.CacheFileSize(), writer.fileOffset)
	return size, nil
}

func (writer *Writer) FlushWithoutPool(ino uint64, ctx context.Context) (err error) {
	if writer == nil {
		log.LogErrorf("Writer FlushWithoutPool: writer is nil, ino(%v)", ino)
		return
	}
	return writer.flushWithoutPool(ino, ctx, true)
}

func (writer *Writer) Flush(ino uint64, ctx context.Context) (err error) {
	if writer == nil {
		log.LogErrorf("Writer Flush: writer is nil, ino(%v)", ino)
		return
	}
	if !writer.ecStreamer.isDirty() {
		return nil
	}
	// dirty=1 时统一走 flushExt：有缓冲落盘，无缓冲则在 flushExt 内仅 updateMetaInfo。
	return writer.flushExt(ino, ctx, true)
}

func (writer *Writer) prepareWriteSlice(offset int, data []byte) []*rwSlice {
	size := len(data)
	wSlices := make([]*rwSlice, 0)
	wSliceCount := size / writer.ecStreamer.BlockSize()
	remainSize := size % writer.ecStreamer.BlockSize()
	for index := 0; index < wSliceCount; index++ {
		offset := offset + index*writer.ecStreamer.BlockSize()
		wSlice := &rwSlice{
			index:      index,
			fileOffset: uint64(offset),
			size:       uint32(writer.ecStreamer.BlockSize()),
			Data:       data[index*writer.ecStreamer.BlockSize() : (index+1)*writer.ecStreamer.BlockSize()],
		}
		wSlices = append(wSlices, wSlice)
	}
	offset = offset + wSliceCount*writer.ecStreamer.BlockSize()
	if remainSize > 0 {
		wSlice := &rwSlice{
			index:      wSliceCount,
			fileOffset: uint64(offset),
			size:       uint32(remainSize),
			Data:       data[wSliceCount*writer.ecStreamer.BlockSize():],
		}
		wSlices = append(wSlices, wSlice)
	}

	return wSlices
}

func (writer *Writer) writeSlice(ctx context.Context, wSlice *rwSlice, wg bool) (err error) {
	if wg {
		defer writer.wg.Done()
		// 子 goroutine panic 或未发送 err 时，主协程会在 wg.Wait 之后永久阻塞在 <-writer.err，且 doParallelWrite 持 Writer 锁导致全文件卡死。
		defer func() {
			if r := recover(); r != nil {
				if writer.err != nil {
					select {
					case writer.err <- &wSliceErr{err: fmt.Errorf("blobstore writeSlice panic: %v", r), fileOffset: wSlice.fileOffset, size: wSlice.size}:
					default:
					}
				}
				panic(r)
			}
		}()
	}

	if writer.limitManager != nil {
		writer.limitManager.WriteAlloc(ctx, int(wSlice.size))
	}
	log.LogDebugf("TRACE blobStore,writeSlice to ebs. ino(%v) fileOffset(%v) len(%v)", writer.ecStreamer.Inode(), wSlice.fileOffset, wSlice.size)
	t0 := time.Now()
	location, err := writer.ecStreamer.Ebsc().Write(ctx, writer.ecStreamer.Volume(), wSlice.Data, wSlice.size)
	if d := time.Since(t0); d >= slowOpInfoThreshold {
		log.LogInfof("blobstore slow ebsc.Write ino(%v) fileOff(%v) size(%v) dur(%v) err(%v)",
			writer.ecStreamer.Inode(), wSlice.fileOffset, wSlice.size, d, err)
	}
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
	// 必须与 len(buf) 一致：doBufferWriteWithoutPool/flushWithoutPool 走 [:0] 后若不清零 blockPosition，
	// 后续 tryOverWrite/doBufferWrite 会在 copy(buf[blockPosition:]) 处 panic（LTP rwtest 多 fd 交错写）。
	writer.blockPosition = 0
}

// reshapeBufForCopyPath doBufferWrite/tryOverWrite 依赖 len(buf)==blockSize 的池化块；flushWithoutPool 等
// 仅 [:0] 保留 cap 时，在进入 copy 前恢复长度，避免 copy 写 0 字节却推进 blockPosition。
// 从 len==0 扩回整块时逻辑上为空，必须清零 blockPosition；否则 stale blockPosition 会使 freeSize 为负或
// copy 不写数据却累加 position，最终在 data[position:position+freeSize] 处 panic（LTP append 多 fd）。
func (writer *Writer) reshapeBufForCopyPath() {
	if writer == nil || writer.ecStreamer.BlockSize() <= 0 {
		log.LogErrorf("Writer reshapeBufForCopyPath: writer is nil or blockSize is 0")
		return
	}
	if writer.blockPosition > writer.ecStreamer.BlockSize() {
		writer.blockPosition = 0
	}
	if len(writer.buf) == 0 && cap(writer.buf) >= writer.ecStreamer.BlockSize() {
		writer.buf = writer.buf[:writer.ecStreamer.BlockSize()]
		writer.blockPosition = 0
	}
}

func (writer *Writer) resetBuffer() {
	// writer.buf = writer.buf[:0]
	writer.blockPosition = 0
}

// 脏语义：对外仅使用 ECStreamer.isDirty()。
//
//   - dirty=1：读/Flush 前须同步；写缓冲后 notifyAfterWrite → markDirty。
//   - dirty=0：completeFlushMeta / updateMetaInfo 已在 resetBuffer 后清 dirty。
//
// 不变式：未落盘字节存在 ⇒ dirty=1；cleanDirty 仅出现在 notifyCompleteFlushMeta 或 updateMetaInfo（无缓冲）路径。
// bufferDirtyLen 仅用于 flushExt/flush/flushWithoutPool 计算落盘字节数，不表示“是否要同步”。

// bufferDirtyLen 返回待落盘字节数（仅 flush 路径使用；是否同步看 isDirty）。
func (writer *Writer) bufferDirtyLen() int {
	if writer == nil {
		log.LogErrorf("Writer bufferDirtyLen: writer is nil")
		return 0
	}
	if writer.blockPosition > 0 {
		return writer.blockPosition
	}
	if len(writer.buf) == 0 {
		return 0
	}
	// flushWithoutPool / [:0] 追加路径无 blockPosition，以 len(buf) 为准。
	if cap(writer.buf) >= writer.ecStreamer.BlockSize() && len(writer.buf) >= writer.ecStreamer.BlockSize() {
		return 0
	}
	return len(writer.buf)
}

func (writer *Writer) flushWithoutPool(inode uint64, ctx context.Context, flushFlag bool) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("blobstore-flush", err, bgTime, 1)
	}()

	log.LogDebugf("TRACE blobStore flushWithoutPool: ino(%v) buf-len(%v) flushFlag(%v)", inode, len(writer.buf), flushFlag)

	if writer.bufferDirtyLen() == 0 {
		return nil
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
		return
	}

	oeks := make([]proto.ObjExtentKey, 0)
	// update meta
	oeks = append(oeks, wSlice.objExtentKey)
	if err = writer.ecStreamer.Mw().AppendObjExtentKeys(writer.ecStreamer.Inode(), oeks); err != nil {
		log.LogErrorf("slice write error,meta append ebsc extent keys fail,ino(%v) fileOffset(%v) len(%v) err(%v)", inode, wSlice.fileOffset, wSlice.size, err)
		// Rollback: delete orphaned data on EBS to avoid blobstore leak
		if delErr := writer.ecStreamer.Ebsc().Delete(oeks); delErr != nil {
			log.LogWarnf("flushWithoutPool: rollback delete ebs extent fail,ino(%v) fileOffset(%v) err(%v)", inode, wSlice.fileOffset, delErr)
		}
		return
	}
	writer.resetBufferWithoutPool()
	return writer.notifyCompleteFlushMeta()
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
			if delErr := writer.ecStreamer.Ebsc().Delete([]proto.ObjExtentKey{written}); delErr != nil {
				log.LogWarnf("flushExt: rollback delete ebs extent fail,ino(%v) fileOffset(%v) err(%v)", writer.ecStreamer.Inode(), written.FileOffset, delErr)
			}
		}
	}

	for _, req := range reqs {
		ek := req.NewExtent
		if ek.Size == 0 {
			continue
		}
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

			readN, readErr := writer.ecStreamer.Ebsc().Read(ctx, writer.ecStreamer.Volume(), data, 0, discardExtent.Size, discardExtent)
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
			_ = flushFlag
			// Rollback: delete already-written extents (use newExtents: real EBS location keys, not req.NewExtent)
			rollbackFn()
			return
		}

		newExtents = append(newExtents, wSlice.objExtentKey)
		discardExtents = append(discardExtents, req.DiscardExtent)
	}

	if len(newExtents) == 0 {
		return nil
	}

	// Atomic metadata update: add batch new extent and discard old extent
	if err = writer.ecStreamer.Mw().AppendObjExtentKeysWithCheck(writer.ecStreamer.Inode(), newExtents, discardExtents); err != nil {
		log.LogErrorf("flushExt: append obj extent keys with check batch fail,ino(%v) count(%v) err(%v)", inode, len(newExtents), err)
		rollbackFn()
		return err
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

	bufferSize := writer.bufferDirtyLen()
	if bufferSize == 0 {
		// 无缓冲：仅刷新 meta（Flush 入口已分流；tryOverWrite 切换 offset 前空 buf 也可能进入）。
		// 与 flush() 不同：flush() 仅由下方 len(reqs)==0 且 bufferSize>0 时调用，空 buf 不在此处理。
		if writer.ecStreamer.isDirty() {
			writer.blockPosition = 0
			return writer.ecStreamer.updateMetaInfo(false, nil)
		}
		return nil
	}

	objExtents := writer.ecStreamer.OeksLocked()
	sort.Slice(objExtents, func(i, j int) bool {
		return objExtents[i].FileOffset < objExtents[j].FileOffset
	})

	if writer.fileOffset < bufferSize {
		err = fmt.Errorf("flushExt: inconsistent state ino(%v) fileOffset(%v) < bufferSize(%v)", inode, writer.fileOffset, bufferSize)
		log.LogErrorf(err.Error())
		return err
	}
	start := uint64(writer.fileOffset - bufferSize)
	end := uint64(writer.fileOffset)
	if start >= end {
		writer.blockPosition = 0
		return writer.ecStreamer.updateMetaInfo(false, nil)
	}

	// Compute overwrite requests: determine which parts of buffer overlap with existing extents
	// This generates a slice of requests, each specifying:
	reqs := computeOverwriteReqs(start, end, objExtents)
	log.LogDebugf("flushExt: ino(%v) start(%v) end(%v) reqsCount(%v) bufferSize(%v)", inode, start, end, len(reqs), bufferSize)

	if len(reqs) == 0 {
		err = writer.flush(inode, ctx, flushFlag)
	} else {
		err = writer.flushOverwriteReqs(ctx, inode, reqs, start, bufferSize, flushFlag)
	}
	if err != nil {
		return
	}

	return writer.notifyCompleteFlushMeta()
}

func (writer *Writer) flush(inode uint64, ctx context.Context, flushFlag bool) (err error) {
	bgTime := stat.BeginStat()
	defer func() {
		stat.EndStat("blobstore-flush", err, bgTime, 1)
	}()

	log.LogDebugf("flush: TRACE blobStore flush: ino(%v) buf-len(%v) flushFlag(%v) fileOffset(%v) blockPosition(%v)",
		inode, len(writer.buf), flushFlag, writer.fileOffset, writer.blockPosition)

	// 仅由 flushExt 在 bufferSize>0 且 computeOverwriteReqs 得 reqs==0 时调用（纯尾部追加落盘）。
	// 空 buf 的 meta 刷新在 flushExt 开头或 Flush 入口处理；此处 bufferSize==0 为契约违反的防御早退。
	bufferSize := writer.bufferDirtyLen()
	if bufferSize == 0 {
		return nil
	}

	if writer.fileOffset < bufferSize {
		err = fmt.Errorf("flush: inconsistent state ino(%v) fileOffset(%v) < bufferSize(%v)", inode, writer.fileOffset, bufferSize)
		log.LogErrorf(err.Error())
		return err
	}

	wSlice := &rwSlice{
		fileOffset: uint64(writer.fileOffset - bufferSize),
		size:       uint32(bufferSize),
		Data:       writer.buf[:bufferSize],
	}
	err = writer.writeSlice(ctx, wSlice, false)
	if err != nil {
		return
	}

	oeks := make([]proto.ObjExtentKey, 0)
	// update meta
	oeks = append(oeks, wSlice.objExtentKey)
	if err = writer.ecStreamer.Mw().AppendObjExtentKeys(writer.ecStreamer.Inode(), oeks); err != nil {
		log.LogErrorf("flush: slice write error,meta append ebsc extent keys fail,ino(%v) fileOffset(%v) len(%v) err(%v)", inode, wSlice.fileOffset, wSlice.size, err)
		// Rollback: delete orphaned data on EBS to avoid blobstore leak
		if delErr := writer.ecStreamer.Ebsc().Delete(oeks); delErr != nil {
			log.LogWarnf("flush: rollback delete ebs extent fail,ino(%v) fileOffset(%v) err(%v)", inode, wSlice.fileOffset, delErr)
		}
		return
	}
	return writer.notifyCompleteFlushMeta()
}

func (writer *Writer) CacheFileSize() int {
	return int(writer.ecStreamer.fileSizeView())
}

// TruncateV2 first flushes buffered data (Flush/flushExt), then calls GetObjExtents; otherwise truncate may operate on stale meta/ObjExtents view.
// When targetSize < current logical size, call ebsc.TruncateV2Extents to shrink EBS and return new extent list;
// when targetSize >= current size, return existing list directly (upper-layer MetaWrapper.TruncateV2 updates meta only, no EBS write).
// Call chain: File.doECTruncateV2 -> Writer.TruncateV2 -> (shrink path) BlobStoreClient.TruncateV2Extents -> mw.TruncateV2.
func (writer *Writer) TruncateV2(ctx context.Context, targetSize uint64,
) (newObjExtents []proto.ObjExtentKey, toDelete []proto.ObjExtentKey, err error) {
	if writer == nil || writer.ecStreamer.Mw() == nil || writer.ecStreamer.Ebsc() == nil {
		log.LogErrorf("Writer.TruncateV2: writer/mw/ebsc nil")
		return nil, nil, fmt.Errorf("Writer.TruncateV2: writer/mw/ebsc nil")
	}
	// don't need to flush here, because the truncate is already done in the file.truncateV2 function
	objExtents := writer.ecStreamer.OeksLocked()
	currentSize := writer.ecStreamer.fileSizeView()

	return writer.TruncateV2FromExtents(ctx, targetSize, currentSize, objExtents)
}

// TruncateV2FromExtents 在已持有 currentSize 与 objExtents 时使用，避免重复 GetObjExtents（例如 ECStreamer.truncateV2 缩容路径）。
func (writer *Writer) TruncateV2FromExtents(ctx context.Context, targetSize uint64, currentSize uint64, objExtents []proto.ObjExtentKey,
) (newObjExtents []proto.ObjExtentKey, toDelete []proto.ObjExtentKey, err error) {
	if writer == nil || writer.ecStreamer.Ebsc() == nil {
		log.LogErrorf("Writer.TruncateV2FromExtents: writer/ebsc nil")
		return nil, nil, fmt.Errorf("Writer.TruncateV2FromExtents: writer/ebsc nil")
	}
	if targetSize >= currentSize {
		return objExtents, nil, nil
	}
	return writer.ecStreamer.Ebsc().TruncateV2Extents(ctx, writer.ecStreamer.Volume(), objExtents, targetSize)
}

func (writer *Writer) FreeCache() {
	if writer == nil {
		log.LogErrorf("Writer.FreeCache: writer nil")
		return
	}
	if buf.CachePool == nil {
		return
	}
	writer.once.Do(func() {
		tmpBuf := writer.buf
		writer.buf = nil
		writer.blockPosition = 0
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
