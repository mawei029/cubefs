// Copyright 2018 The CubeFS Authors.
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

package metanode

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/cubefs/cubefs/blobstore/util/taskpool"
	"github.com/cubefs/cubefs/depends/tiglabs/raft/util"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	"github.com/cubefs/cubefs/util/synclist"
)

const (
	prefixDelExtent     = "EXTENT_DEL"
	prefixDelExtentV2   = "EXTENT_DEL_V2"
	prefixDelObjExtent  = "OBJ_EXTENT_DEL"
	maxDeleteExtentSize = 10 * MB

	// ObjExtentKey binary format field sizes (bytes)
	// Order matches MarshalBinary: BlobsLen(4) + FileOffset(8) + Size(8) + Crc(4) + CodeMode(1) + Cid(8) + BlobSize(4) + Blobs
	sizeOfBlob          = 24                        // Blob: MinBid(8) + Count(8) + Vid(8)
	sizeOfBlobsLen      = 4                         // BlobsLen uint32
	minObjExtentKeySize = 4 + 8 + 8 + 4 + 1 + 8 + 4 // 37 bytes (without Blobs)
)

var extentsFileHeader = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x08}

// start metapartition delete extents work
func (mp *metaPartition) startToDeleteExtents() {
	fileList := synclist.New()
	go mp.appendDelExtentsToFile(fileList)
	go mp.deleteExtentsFromList(fileList)
}

// create extent delete file
func (mp *metaPartition) createExtentDeleteFile(prefix string, idx int64, fileList *synclist.SyncList) (fp *os.File, fileName string, fileSize int64, err error) {
	fileName = fmt.Sprintf("%s_%d", prefix, idx)
	fp, err = os.OpenFile(path.Join(mp.config.RootDir, fileName),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.LogErrorf("[metaPartition] createExtentDeletFile openFile %v %v error %v", mp.config.RootDir, fileName, err)
		return
	}

	var info fs.FileInfo
	info, err = fp.Stat()
	if err != nil {
		log.LogErrorf("createExtentDeleteFile: stat new file failed, dir %s, name %s, err %v", mp.config.RootDir, fileName, err.Error())
		return
	}

	if info.Size() > 0 {
		log.LogWarnf("createExtentDeleteFile: file are already exist, no need to write header again, dir %s, name %s， size %d", mp.config.RootDir, fileName, info.Size())
	} else {
		if _, err = fp.Write(extentsFileHeader); err != nil {
			log.LogErrorf("[metaPartition] createExtentDeletFile Write %v %v error %v", mp.config.RootDir, fileName, err)
			fp.Close()
		}
	}
	fileSize = int64(len(extentsFileHeader))
	fileList.PushBack(fileName)
	return
}

// append delete extents from extDelCh to EXTENT_DEL_N files
func (mp *metaPartition) appendDelExtentsToFile(fileList *synclist.SyncList) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("[metaPartition] appendDelExtentsToFile pid(%v) panic (%v)", mp.config.PartitionId, r)
		}
	}()
	var (
		fileName string
		fileSize int64
		idx      int64
		fp       *os.File
		err      error
	)
LOOP:
	// scan existed EXTENT_DEL_* files to fill fileList
	finfos, err := ioutil.ReadDir(mp.config.RootDir)
	if err != nil {
		panic(err)
	}

	finfos = sortDelExtFileInfo(finfos)
	for _, info := range finfos {
		fileList.PushBack(info.Name())
		fileSize = info.Size()
	}

	// check
	lastItem := fileList.Back()
	if lastItem != nil {
		fileName = lastItem.Value.(string)
	}
	if lastItem == nil || !strings.HasPrefix(fileName, prefixDelExtentV2) {
		// if no exist EXTENT_DEL_*, create one
		log.LogDebugf("action[appendDelExtentsToFile] verseq [%v]", mp.verSeq)
		fp, fileName, fileSize, err = mp.createExtentDeleteFile(prefixDelExtentV2, idx, fileList)
		log.LogDebugf("action[appendDelExtentsToFile] verseq [%v] fileName %v", mp.verSeq, fileName)
		if err != nil {
			panic(err)
		}
	} else {
		// exist, open last file
		fp, err = os.OpenFile(path.Join(mp.config.RootDir, fileName),
			os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			panic(err)
		}
		// continue from last item
		idx = getDelExtFileIdx(fileName)
	}

	log.LogDebugf("action[appendDelExtentsToFile] verseq [%v] fileName %v", mp.verSeq, fileName)
	// TODO Unhandled errors
	buf := make([]byte, 0)
	for {
		select {
		case <-mp.stopC:
			fp.Close()
			return
		case <-mp.extReset:
			// TODO Unhandled errors
			fp.Close()
			// reset fileList
			fileList.Init()
			goto LOOP
		case eks := <-mp.extDelCh:
			var data []byte
			buf = buf[:0]
			if len(eks) == 0 {
				log.LogDebugf("[appendDelExtentsToFile] vol(%v) mp(%v) eks cnt is 0", mp.GetVolName(), mp.config.PartitionId)
				continue
			}
			log.LogDebugf("[appendDelExtentsToFile] mp(%v) del eks [%v]", mp.config.PartitionId, eks)
			for _, ek := range eks {
				if ek.IsSplit() || ek.GetSeq() > 0 {
					data, err = ek.MarshalBinaryWithCheckSum(true)
				} else {
					data, err = ek.MarshalBinaryWithCheckSum(false)
				}
				if err != nil {
					log.LogWarnf("[appendDelExtentsToFile] partitionId=%d, extentKey marshal: %v", mp.config.PartitionId, err)
					break
				}
				buf = append(buf, data...)
			}

			if err != nil {
				err = mp.sendExtentsToChan(eks)
				if err != nil {
					log.LogErrorf("[appendDelExtentsToFile] mp[%v] sendExtentsToChan fail, err(%s)", mp.config.PartitionId, err.Error())
				}
				continue
			}
			if fileSize >= maxDeleteExtentSize {
				// TODO Unhandled errors
				// close old File
				fp.Close()
				idx += 1
				fp, fileName, fileSize, err = mp.createExtentDeleteFile(prefixDelExtentV2, idx, fileList)
				if err != nil {
					panic(err)
				}
				log.LogDebugf("appendDelExtentsToFile. volname [%v] mp[%v] createExtentDeleteFile %v", mp.GetVolName(), mp.config.PartitionId, fileName)
			}
			// write delete extents into file
			if _, err = fp.Write(buf); err != nil {
				fp.Close()
				panic(err)
			}
			fileSize += int64(len(buf))
			log.LogDebugf("action[appendDelExtentsToFile] filesize now %v", fileSize)
		}
	}
}

func (mp *metaPartition) batchDeleteExtentsByDp(dpId uint64, extents []*proto.DelExtentParam) (retryExtents []*proto.DelExtentParam, err error) {
	dp := mp.vol.GetPartition(dpId)
	if dp == nil {
		log.LogErrorf("[batchDeleteExtentsByDp] mp(%v) dp(%v) not found", mp.config.PartitionId, dpId)
		err = fmt.Errorf("dp %v is not found", dpId)
		retryExtents = extents
		return
	}
	batchCnt := DeleteBatchCount()
	for i := uint64(0); i < uint64(len(extents)); i += batchCnt {
		limit := i + batchCnt
		if limit > uint64(len(extents)) {
			limit = uint64(len(extents))
		}
		batchEk := extents[i:limit]
		err = mp.doBatchDeleteExtentsByPartition(dpId, batchEk)
		if err != nil {
			msg := fmt.Sprintf("[batchDeleteExtentsByDp] vol(%v) mp(%v) failed to delete dp(%v) extents cnt(%v), err %s", mp.GetVolName(), mp.config.PartitionId, dp.PartitionID, len(batchEk), err.Error())
			if strings.Contains(msg, "OpLimitedIoErr") || strings.Contains(msg, "OpTinyRecoverErr") || strings.Contains(msg, "OpDpDecommissionRepairErr") {
				log.LogInfo(msg)
			} else {
				log.LogError(msg)
			}
			retryExtents = append(retryExtents, extents[i:]...)
			return
		}
		log.LogDebugf("[batchDeleteExtentsByDp] vol(%v) mp(%v) delete eks cnt(%v) from dp(%v)", mp.GetVolName(), mp.config.PartitionId, len(extents), dpId)
	}
	return
}

// Delete all the extents of a file.
func (mp *metaPartition) deleteExtentsFromList(fileList *synclist.SyncList) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("deleteExtentsFromList(%v) deleteExtentsFromList panic (%v)", mp.config.PartitionId, r)
		}
	}()

	pool := taskpool.New(10, 20)
	defer pool.Close()

	var (
		element  *list.Element
		fileName string
		file     string
		fileInfo os.FileInfo
		err      error
	)
	cursorBuf := make([]byte, 8)
	readBuf := make([]byte, 512*util.KB)
	for {
		// DeleteWorkerSleepMs()
		time.Sleep(1 * time.Minute)
		select {
		case <-mp.stopC:
			return
		default:
		}
		element = fileList.Front()
		if element == nil {
			continue
		}
		fileName = element.Value.(string)
		file = path.Join(mp.config.RootDir, fileName)
		if fileInfo, err = os.Stat(file); err != nil {
			log.LogDebugf("[deleteExtentsFromList] mp(%v) skip file(%v)", mp.config.PartitionId, fileName)
			fileList.Remove(element)
			continue
		}
		log.LogDebugf("[deleteExtentsFromList] mp(%v) reading file(%v)", mp.config.PartitionId, fileName)
		// if not leader, ignore delete
		if _, ok := mp.IsLeader(); !ok {
			log.LogDebugf("[deleteExtentsFromList] partitionId=%d, "+
				"not raft leader,please ignore", mp.config.PartitionId)
			continue
		}
		// leader do delete extent for EXTENT_DEL_* file

		// read delete extents from file
		fp, err := os.OpenFile(file, os.O_RDWR, 0o644)
		if err != nil {
			if !os.IsNotExist(err) {
				log.LogErrorf("[deleteExtentsFromList] volname [%v] mp[%v] openFile %v error: %v", mp.GetVolName(), mp.config.PartitionId, file, err)
			} else {
				log.LogDebugf("[deleteExtentsFromList] mp(%v) delete extents file(%v) deleted", mp.config.PartitionId, fileName)
			}
			fileList.Remove(element)
			continue
		}

		// get delete extents cursor at file header 8 bytes
		if _, err = fp.ReadAt(cursorBuf, 0); err != nil {
			log.LogWarnf("[deleteExtentsFromList] partitionId=%d, "+
				"read cursor least 8bytes, retry later", mp.config.PartitionId)
			// TODO Unhandled errors
			fp.Close()
			continue
		}
		extentV2 := false
		extentKeyLen := uint64(proto.ExtentLength)
		if strings.HasPrefix(fileName, prefixDelExtentV2) {
			extentV2 = true
			extentKeyLen = uint64(proto.ExtentV2Length)
		}
		cursor := binary.BigEndian.Uint64(cursorBuf)
		stat, err := fp.Stat()
		if err != nil {
			log.LogErrorf("[deleteExtentsFromList] mp(%v) stat file(%v) err(%v)", mp.config.PartitionId, fileName, err)
			fp.Close()
			continue
		}

		log.LogDebugf("[deleteExtentsFromList] volname [%v] mp[%v] openFile %v file len %v cursor %v",
			mp.GetVolName(), mp.config.PartitionId, file, stat.Size(), cursor)

		if fileInfo.Size() > int64(cursor) && fileInfo.Size() < int64(cursor)+int64(extentKeyLen) {
			log.LogErrorf("[deleteExtentsFromList] mp(%d), file(%v) corrupted!", mp.config.PartitionId, fileName)
			fileList.Remove(element)
			fp.Close()
			continue
		}

		deleteCnt := 0
		errExts := make([]proto.ExtentKey, 0)
		needDeleteExtents := make(map[uint64][]*proto.DelExtentParam)
		err = func() (err error) {
			// read extents from cursor
			defer fp.Close()
			// NOTE: read 256kb at once
			rLen, err := fp.ReadAt(readBuf, int64(cursor))
			log.LogDebugf("[deleteExtentsFromList] mp(%v) read len(%v) cursor(%v), err(%v)", mp.config.PartitionId, rLen, cursor, err)
			if err != nil {
				if err != io.EOF {
					log.LogErrorf("[deleteExtentsFromList] mp(%v) failed to read file(%v), err(%v)", mp.config.PartitionId, fileName, err)
					return
				}
				err = nil
				if rLen == 0 {
					log.LogDebugf("[deleteExtentsFromList] mp(%v) file list cnt(%v)", mp.config.PartitionId, fileList.Len())
					if fileList.Len() <= 1 {
						log.LogDebugf("[deleteExtentsFromList] mp(%v) skip delete file(%v), free list count(%v)", mp.config.PartitionId, fileName, fileList.Len())
						return
					}
					status := mp.raftPartition.Status()
					_, isLeader := mp.IsLeader()
					if isLeader && !status.RestoringSnapshot {
						// delete old delete extents file for metapartition
						if _, err = mp.submit(opFSMInternalDelExtentFile, []byte(fileName)); err != nil {
							log.LogErrorf("[deleteExtentsFromList] mp(%v), delete old file(%v), err(%v)", mp.config.PartitionId, fileName, err)
							return
						}
						log.LogDebugf("[deleteExtentsFromList] mp(%v), delete old file(%v)", mp.config.PartitionId, fileName)
						return
					}
					log.LogDebugf("[deleteExtentsFromList] partitionId=%d, delete old file status: %s", mp.config.PartitionId, status.State)
				}
			}
			cursor += uint64(rLen)
			buff := bytes.NewBuffer(readBuf[:rLen])
			for buff.Len() != 0 {
				lastUnread := buff.Len()
				// NOTE: audjust cursor
				if uint64(buff.Len()) < extentKeyLen {
					cursor -= uint64(lastUnread)
					break
				}
				if extentV2 && uint64(buff.Len()) < uint64(proto.ExtentV3Length) {
					if r := bytes.Compare(buff.Bytes()[:4], proto.ExtentKeyHeaderV3); r == 0 {
						cursor -= uint64(lastUnread)
						break
					}
				}
				// NOTE: read ek
				ek := proto.ExtentKey{}
				if extentV2 {
					if err = ek.UnmarshalBinaryWithCheckSum(buff); err != nil {
						if err == proto.InvalidKeyHeader || err == proto.InvalidKeyCheckSum {
							log.LogErrorf("[deleteExtentsFromList] invalid extent key header %v, %v, %v", fileName, mp.config.PartitionId, err)
							return
						}
						log.LogErrorf("[deleteExtentsFromList] mp: %v Unmarshal extentkey from %v unresolved error: %v", mp.config.PartitionId, fileName, err)
						return
					}
				} else {
					// ek for del no need to get version
					tmpBuff := GetReadBuf(buff.Next(proto.ExtentLength))
					if err = ek.UnmarshalBinary(tmpBuff, false); err != nil {
						log.LogErrorf("[deleteExtentsFromList] mp(%v) failed to unmarshal extent", mp.config.PartitionId)
						PutReadBuf(tmpBuff)
						return
					}
					PutReadBuf(tmpBuff)
				}

				// NOTE: add to current batch
				dpId := ek.PartitionId
				eks := needDeleteExtents[dpId]
				if eks == nil {
					eks = make([]*proto.DelExtentParam, 0)
				}
				eks = append(eks, &proto.DelExtentParam{
					ExtentKey:          &ek,
					IsSnapshotDeletion: ek.IsSplit(),
				})
				deleteCnt++
				needDeleteExtents[dpId] = eks
				log.LogDebugf("[deleteExtentsFromList] mp(%v) append extent(%v) to batch, cnt(%v)", mp.config.PartitionId, ek, deleteCnt)
			}
			log.LogDebugf("[deleteExtentsFromList] mp(%v) reach the end of buffer", mp.config.PartitionId)
			return
		}()
		if err != nil {
			log.LogErrorf("[deleteExtentsFromList] mp(%v) failed to read delete file(%v), err(%v)", mp.config.PartitionId, fileName, err)
			continue
		}

		if deleteCnt == 0 {
			log.LogDebugf("[deleteExtentsFromList] mp(%v) delete cnt is 0, sleep", mp.config.PartitionId)
			continue
		}

		successCnt := 0

		wg := sync.WaitGroup{}
		mux := sync.Mutex{}

		delFunc := func(dpId uint64, eks []*proto.DelExtentParam) {
			log.LogDebugf("[deleteExtentsFromList] mp(%v) delete dp(%v) eks count(%v)", mp.config.PartitionId, dpId, len(eks))
			var retry []*proto.DelExtentParam
			retry, err = mp.batchDeleteExtentsByDp(dpId, eks)
			if err != nil {
				msg := fmt.Sprintf("[deleteExtentsFromList] mp(%v) failed to delete dp(%v) err(%v)", mp.config.PartitionId, dpId, err)
				if strings.Contains(msg, "OpLimitedIoErr") || strings.Contains(msg, "OpTinyRecoverErr") || strings.Contains(msg, "OpDpDecommissionRepairErr") {
					log.LogInfo(msg)
				} else {
					log.LogError(msg)
				}
			}

			mux.Lock()
			successCnt += len(eks) - len(retry)
			for _, dek := range retry {
				errExts = append(errExts, *dek.ExtentKey)
			}
			mux.Unlock()
		}

		for dpId, eks := range needDeleteExtents {

			dpIdCopy := dpId
			eksCopy := make([]*proto.DelExtentParam, len(eks))
			copy(eksCopy, eks)

			wg.Add(1)
			pool.Run(func() {
				defer wg.Done()
				delFunc(dpIdCopy, eksCopy)
			})
		}

		wg.Wait()

		log.LogDebugf("[deleteExtentsFromList] mp(%v) delete success cnt(%v), err cnt(%v)", mp.config.PartitionId, successCnt, len(errExts))

		if successCnt == 0 {
			log.LogWarnf("[deleteExtentsFromList] no extents delete successfully, sleep")
		}

		if len(errExts) != 0 {
			log.LogDebugf("[deleteExtentsFromList] mp(%v) sync errExts(%v)", mp.config.PartitionId, errExts)
			err = mp.sendExtentsToChan(errExts)
			if err != nil {
				log.LogErrorf("[deleteExtentsFromList] sendExtentsToChan by raft error, mp[%v], err(%v), ek(%v)", mp.config.PartitionId, err, len(errExts))
			}
		}

		buff := bytes.NewBuffer([]byte{})
		buff.WriteString(fmt.Sprintf("%s %d", fileName, cursor))
		log.LogDebugf("[deleteExtentsFromList] mp(%v) delete eks(%v) from file(%v)", mp.config.PartitionId, deleteCnt, fileName)
		if _, err = mp.submit(opFSMInternalDelExtentCursor, buff.Bytes()); err != nil {
			log.LogWarnf("[deleteExtentsFromList] partitionId=%d, %s",
				mp.config.PartitionId, err.Error())
		}

		log.LogDebugf("[deleteExtentsFromList] mp(%v) file(%v), cursor(%v), size(%v)", mp.config.PartitionId, fileName, cursor, len(readBuf))
	}
}

// startToDeleteObjExtents starts two background tasks: persist to file and delete via EBS
// 1. appendDelObjExtentsToFile: reads from objExtDelCh channel and persists to OBJ_EXTENT_DEL_* files
// 2. deleteObjExtentsFromList: reads from files and deletes via EBS client
func (mp *metaPartition) startToDeleteObjExtents() {
	fileList := synclist.New()
	go mp.appendDelObjExtentsToFile(fileList)
	go mp.deleteObjExtentsFromList(fileList)
}

// appendDelObjExtentsToFile reads batches of obj extents from objExtDelCh channel and persists them to OBJ_EXTENT_DEL_* files
// This ensures data persistence even if channel is full, preventing data loss during overwrite operations
func (mp *metaPartition) appendDelObjExtentsToFile(fileList *synclist.SyncList) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("[appendDelObjExtentsToFile] mp(%v) panic: %v", mp.config.PartitionId, r)
		}
	}()

	var (
		fileName string
		fileSize int64
		idx      int64
		fp       *os.File
	)

	// Scan existing OBJ_EXTENT_DEL_* files and add to fileList
	finfos, err := ioutil.ReadDir(mp.config.RootDir)
	if err != nil {
		log.LogErrorf("[appendDelObjExtentsToFile] mp(%v) read dir failed: %v", mp.config.PartitionId, err)
		return
	}

	finfos = sortDelExtFileInfo(finfos)
	for _, info := range finfos {
		if strings.HasPrefix(info.Name(), prefixDelObjExtent) {
			fileList.PushBack(info.Name())
		}
	}

	// Open last file or create new one
	lastItem := fileList.Back()
	if lastItem != nil {
		fileName = lastItem.Value.(string)
	}
	if lastItem == nil || !strings.HasPrefix(fileName, prefixDelObjExtent) {
		fp, fileSize, err = mp.createObjExtentDeleteFile(idx, fileList)
		if err != nil {
			log.LogErrorf("[appendDelObjExtentsToFile] mp(%v) create file failed: %v", mp.config.PartitionId, err)
			return
		}
	} else {
		filePath := path.Join(mp.config.RootDir, fileName)
		fp, err = os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			log.LogErrorf("[appendDelObjExtentsToFile] mp(%v) open file failed: %v", mp.config.PartitionId, err)
			return
		}
		info, err := fp.Stat()
		if err != nil {
			fp.Close()
			log.LogErrorf("[appendDelObjExtentsToFile] mp(%v) stat file failed: %v", mp.config.PartitionId, err)
			return
		}
		fileSize = info.Size()
		idx = getDelExtFileIdx(fileName)
	}

	// Main loop: read batch from channel and write to file(persistent)
	buf := make([]byte, 0)
	for {
		select {
		case <-mp.stopC:
			if fp != nil {
				fp.Close()
			}
			return
		case oeks := <-mp.objExtDelCh:
			if len(oeks) == 0 {
				continue
			}
			// Marshal batch of obj extents to binary
			buf = buf[:0]
			for _, oek := range oeks {
				data, err := oek.MarshalBinary()
				if err != nil {
					log.LogWarnf("[appendDelObjExtentsToFile] mp(%v) marshal failed: %v", mp.config.PartitionId, err)
					continue
				}
				buf = append(buf, data...)
			}
			if len(buf) == 0 {
				continue
			}

			// Rotate file if size exceeds limit
			if fileSize >= maxDeleteExtentSize {
				fp.Close()
				idx++
				fp, fileSize, err = mp.createObjExtentDeleteFile(idx, fileList)
				if err != nil {
					log.LogErrorf("[appendDelObjExtentsToFile] mp(%v) create file failed: %v", mp.config.PartitionId, err)
					return
				}
			}

			// Write batch to file
			if _, err = fp.Write(buf); err != nil {
				log.LogErrorf("[appendDelObjExtentsToFile] mp(%v) write failed: %v", mp.config.PartitionId, err)
				fp.Close()
				return
			}
			fileSize += int64(len(buf))
		}
	}
}

// deleteObjExtentsFromList reads obj extents from OBJ_EXTENT_DEL_* files and deletes them via EBS client
func (mp *metaPartition) deleteObjExtentsFromList(fileList *synclist.SyncList) {
	defer func() {
		if r := recover(); r != nil {
			log.LogErrorf("[deleteObjExtentsFromList] mp(%v) panic: %v", mp.config.PartitionId, r)
		}
	}()

	cursorBuf := make([]byte, 8)
	readBuf := make([]byte, 512*util.KB)
	for {
		time.Sleep(1 * time.Minute)
		select {
		case <-mp.stopC:
			return
		default:
		}

		element := fileList.Front()
		if element == nil {
			continue
		}

		// Check valid
		fileName := element.Value.(string)
		file := path.Join(mp.config.RootDir, fileName)
		if _, err := os.Stat(file); err != nil {
			fileList.Remove(element)
			continue
		}
		if _, ok := mp.IsLeader(); !ok {
			continue
		}

		// Open file and read cursor from header (first 8 bytes)
		fp, err := os.OpenFile(file, os.O_RDWR, 0o644)
		if err != nil {
			if !os.IsNotExist(err) {
				log.LogErrorf("[deleteObjExtentsFromList] mp(%v) open %v error: %v", mp.config.PartitionId, file, err)
			}
			fileList.Remove(element)
			continue
		}

		if _, err = fp.ReadAt(cursorBuf, 0); err != nil {
			log.LogWarnf("[deleteObjExtentsFromList] mp(%v) read cursor failed", mp.config.PartitionId)
			fp.Close()
			continue
		}

		cursor := binary.BigEndian.Uint64(cursorBuf)
		stat, err := fp.Stat()
		if err != nil {
			log.LogErrorf("[deleteObjExtentsFromList] mp(%v) stat failed: %v", mp.config.PartitionId, err)
			fp.Close()
			continue
		}

		if int64(cursor) > stat.Size() {
			log.LogErrorf("[deleteObjExtentsFromList] mp(%v) corrupted: cursor(%v) > size(%v)",
				mp.config.PartitionId, cursor, stat.Size())
			fileList.Remove(element)
			fp.Close()
			continue
		}

		// Parse and delete extents
		oldCursor := cursor
		needDels, deleteCnt, cursor, err := mp.parseDelObjExtent(fp, fileList, cursor, readBuf, element)
		if err != nil {
			log.LogErrorf("[deleteObjExtentsFromList] mp(%v) parse failed: %v", mp.config.PartitionId, err)
			continue
		}
		if deleteCnt == 0 {
			continue
		}

		if err = mp.deleteObjExtents(needDels); err != nil {
			log.LogErrorf("[deleteObjExtentsFromList] mp(%v) delete %d extents failed: %v",
				mp.config.PartitionId, len(needDels), err)
		}

		log.LogDebugf("[deleteObjExtentsFromList] mp(%d) deleteCnt(%d) cursor(%d) oldCursor(%d)",
			mp.config.PartitionId, len(needDels), cursor, oldCursor)

		// Update cursor if progress made
		if cursor != oldCursor {
			binary.BigEndian.PutUint64(cursorBuf, cursor)
			updateFp, err := os.OpenFile(file, os.O_RDWR, 0o644)
			if err == nil {
				if _, err = updateFp.WriteAt(cursorBuf, 0); err == nil {
					updateFp.Sync()
				}
				updateFp.Close()
			}
		}
	}
}

// createObjExtentDeleteFile creates a new OBJ_EXTENT_DEL* file with 8-byte header for cursor
func (mp *metaPartition) createObjExtentDeleteFile(idx int64, fileList *synclist.SyncList) (fp *os.File, fileSize int64, err error) {
	fileName := fmt.Sprintf("%s_%d", prefixDelObjExtent, idx)
	filePath := path.Join(mp.config.RootDir, fileName)
	fp, err = os.OpenFile(filePath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		log.LogErrorf("[createObjExtentDeleteFile] open %v failed: %v", filePath, err)
		return
	}

	info, err := fp.Stat()
	if err != nil {
		fp.Close()
		log.LogErrorf("[createObjExtentDeleteFile] stat %v failed: %v", filePath, err)
		return
	}

	fileList.PushBack(fileName)

	if info.Size() > 0 {
		return fp, info.Size(), nil
	}

	if _, err = fp.Write(extentsFileHeader); err != nil {
		fp.Close()
		log.LogErrorf("[createObjExtentDeleteFile] write header failed: %v", err)
		return
	}
	return fp, int64(len(extentsFileHeader)), nil
}

func (mp *metaPartition) parseDelObjExtent(fp *os.File, fileList *synclist.SyncList, cursor uint64,
	readBuf []byte, element *list.Element,
) (needDels []proto.ObjExtentKey, deleteCnt int, newCursor uint64, err error) {
	defer fp.Close()

	rLen, err := fp.ReadAt(readBuf, int64(cursor))
	if err != nil && err != io.EOF {
		log.LogErrorf("[parseDelObjExtent] mp(%v) read failed: %v", mp.config.PartitionId, err)
		return nil, 0, cursor, err
	}

	// File fully processed: delete if not the only file
	if err == io.EOF && rLen == 0 {
		if fileList.Len() > 1 {
			status := mp.raftPartition.Status()
			if _, isLeader := mp.IsLeader(); isLeader && !status.RestoringSnapshot {
				fileList.Remove(element)
				os.Remove(fp.Name())
			}
		}
		return nil, 0, cursor, nil
	}

	// Unmarshal obj extents from buffer
	needDels = make([]proto.ObjExtentKey, 0)
	buff := bytes.NewBuffer(readBuf[:rLen])
	for buff.Len() >= sizeOfBlobsLen {
		blobsLen := binary.BigEndian.Uint32(buff.Bytes()[:sizeOfBlobsLen])
		estimatedSize := minObjExtentKeySize + int(blobsLen)*sizeOfBlob
		if buff.Len() < estimatedSize {
			break
		}

		oek := proto.ObjExtentKey{}
		startPos := buff.Len()
		if err = oek.UnmarshalBinary(buff); err != nil {
			log.LogErrorf("[parseDelObjExtent] mp(%v) unmarshal failed: %v", mp.config.PartitionId, err)
			break
		}
		cursor += uint64(startPos - buff.Len())
		needDels = append(needDels, oek)
		deleteCnt++
	}

	return needDels, deleteCnt, cursor, nil
}
