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
	"errors"
	"sort"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
)

// TruncateV2Extents 根据目标大小截断 ObjExtentKey 列表，并执行 EBS 的读/截断/写/删：
// - 完全在 targetSize 之前的 extent 保留；
// - 完全在 targetSize 之后的 extent 删除（数据+元数据由本方法删 EBS 数据，元数据由调用方通过 TruncateV2 更新）；
// - 部分重叠的 extent：读全量 -> 内存截断 -> 新写 EBS -> 删旧 EBS。
// 返回截断后的新 ObjExtentKey 列表（用于 meta TruncateV2）。
func (ebs *BlobStoreClient) TruncateV2Extents(ctx context.Context, volName string, objExtentKeys []proto.ObjExtentKey, targetSize uint64) (newObjExtents []proto.ObjExtentKey, err error) {
	if len(objExtentKeys) == 0 {
		return nil, nil
	}
	// 保证按 FileOffset 有序
	eks := make([]proto.ObjExtentKey, len(objExtentKeys))
	copy(eks, objExtentKeys)
	sort.Slice(eks, func(i, j int) bool { return eks[i].FileOffset < eks[j].FileOffset })

	var toDelete []proto.ObjExtentKey
	for _, oek := range eks {
		end := oek.FileOffset + oek.Size
		if end <= targetSize {
			// 完全保留
			newObjExtents = append(newObjExtents, oek)
			continue
		}
		if oek.FileOffset >= targetSize {
			// 完全超出，仅删 EBS 数据
			toDelete = append(toDelete, oek)
			continue
		}
		// 部分重叠：读全量 -> 截断 -> 新写 -> 删旧
		keepSize := targetSize - oek.FileOffset
		buf := make([]byte, oek.Size)
		readN, err := ebs.Read(ctx, volName, buf, 0, oek.Size, oek)
		if err != nil {
			log.LogErrorf("TruncateV2Extents: read extent ino extent(%v) err(%v)", oek, err)
			return nil, err
		}
		if uint64(readN) != oek.Size {
			log.LogWarnf("TruncateV2Extents: read short ino extent(%v) readN(%v)", oek, readN)
		}
		truncated := buf[:keepSize]
		newOeks, _, err := ebs.Put(ctx, volName, bytes.NewReader(truncated), uint64(len(truncated)))
		if err != nil {
			log.LogErrorf("TruncateV2Extents: put truncated extent err(%v)", err)
			return nil, err
		}
		if len(newOeks) == 0 {
			log.LogErrorf("TruncateV2Extents: put returned no keys")
			return nil, errors.New("ebs put returned no extent keys")
		}
		// Put 返回的 key 的 FileOffset 是写入时的偏移，需改为原 extent 的文件偏移
		newKey := newOeks[0]
		newKey.FileOffset = oek.FileOffset
		newObjExtents = append(newObjExtents, newKey)
		toDelete = append(toDelete, oek)
	}

	if len(toDelete) > 0 {
		if err = ebs.Delete(toDelete); err != nil {
			log.LogErrorf("TruncateV2Extents: delete old extents err(%v)", err)
			return nil, err
		}
	}
	return newObjExtents, nil
}
