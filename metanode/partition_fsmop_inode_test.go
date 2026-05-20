// Copyright 2024 The CubeFS Authors.
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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	raftstoremock "github.com/cubefs/cubefs/metanode/mocktest/raftstore"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

const RocksdbInodeTestDir = "/tmp/cfs/fsm_inode_test"

func getMpConfigForFsmInodeTest(storeMode proto.StoreMode) (config *MetaPartitionConfig) {
	config = &MetaPartitionConfig{
		PartitionId:   10001,
		VolName:       VolNameForTest,
		PartitionType: proto.VolumeTypeHot,
		StoreMode:     storeMode,
	}
	if config.StoreMode == proto.StoreModeRocksDb {
		config.RocksDBDir = fmt.Sprintf("%v/%v_%v", RocksdbInodeTestDir, partitionId, time.Now().UnixMilli())
	}
	return
}

func newMpForFsmInodeTest(t *testing.T, storeMode proto.StoreMode) (mp *metaPartition) {
	var _ interface{} = t
	config := getMpConfigForFsmInodeTest(storeMode)
	mp = newPartition(config, newManager())
	mp.uniqChecker = newUniqChecker()
	return
}

func mockPartitionRaftForFsmInodeTest(t *testing.T, ctrl *gomock.Controller, storeMode proto.StoreMode) *metaPartition {
	partition := newMpForFsmInodeTest(t, storeMode)
	raft := raftstoremock.NewMockPartition(ctrl)
	idx := uint64(0)
	raft.EXPECT().Submit(gomock.Any()).DoAndReturn(func(cmd []byte) (resp interface{}, err error) {
		idx++
		return partition.Apply(cmd, idx)
	}).AnyTimes()

	raft.EXPECT().IsRaftLeader().DoAndReturn(func() bool {
		return true
	}).AnyTimes()

	raft.EXPECT().LeaderTerm().Return(uint64(1), uint64(1)).AnyTimes()
	partition.raftPartition = raft
	return partition
}

func prepareInodeForFsmInodeTest(t *testing.T, mp *metaPartition, ino uint64) {
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	inode.PoolId = proto.DefaultSSDPoolId
	status, err := mp.fsmCreateInode(handle, inode)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
}

func prepareDirInodeForFsmInodeTest(t *testing.T, mp *metaPartition, ino uint64) {
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, DirModeType)
	status, err := mp.fsmCreateInode(handle, inode)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
}

func checkInodeLinkForFsmInodeTest(t *testing.T, mp *metaPartition, ino uint64, link uint64) {
	inode, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	if inode == nil {
		require.EqualValues(t, 0, link)
		return
	}
	require.EqualValues(t, link, inode.NLink)
}

func testFsmCreateInode(t *testing.T, mp *metaPartition) {
	const ino = 1000
	prepareInodeForFsmInodeTest(t, mp, ino)

	inode, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, inode)
	require.EqualValues(t, ino, inode.Inode)
}

func TestFsmCreateInode(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmCreateInode(t, mp)
}

func TestFsmCreateInode_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmCreateInode(t, mp)
}

func testFsmLinkInode(t *testing.T, mp *metaPartition) {
	const ino = 1000
	prepareInodeForFsmInodeTest(t, mp, ino)

	inode := NewInodeTest(ino, FileModeType)
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	resp, err := mp.fsmCreateLinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	checkInodeLinkForFsmInodeTest(t, mp, ino, 2)

	const dirIno = 1001
	prepareDirInodeForFsmInodeTest(t, mp, dirIno)
	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(dirIno, DirModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	resp, err = mp.fsmCreateLinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	checkInodeLinkForFsmInodeTest(t, mp, dirIno, 3)
}

func TestFsmLinkInode(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmLinkInode(t, mp)
}

func TestFsmLinkInode_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmLinkInode(t, mp)
}

func testFsmUnlinkInode(t *testing.T, mp *metaPartition) {
	const ino = 1000
	const dirIno = 1001
	prepareInodeForFsmInodeTest(t, mp, ino)
	prepareDirInodeForFsmInodeTest(t, mp, dirIno)

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	resp, err := mp.fsmCreateLinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	inode = NewInodeTest(dirIno, DirModeType)
	resp, err = mp.fsmCreateLinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	checkInodeLinkForFsmInodeTest(t, mp, ino, 2)
	checkInodeLinkForFsmInodeTest(t, mp, dirIno, 3)

	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(ino, FileModeType)
	resp, err = mp.fsmUnlinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	inode = NewInodeTest(dirIno, DirModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	resp, err = mp.fsmUnlinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	checkInodeLinkForFsmInodeTest(t, mp, ino, 1)
	checkInodeLinkForFsmInodeTest(t, mp, dirIno, 2)

	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	resp, err = mp.fsmUnlinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	inode = NewInodeTest(dirIno, DirModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	resp, err = mp.fsmUnlinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	// NOTE: unlink empty dir, will delete it
	checkInodeLinkForFsmInodeTest(t, mp, ino, 0)
	checkInodeLinkForFsmInodeTest(t, mp, dirIno, 0)
}

func TestFsmUnlinkInode(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmUnlinkInode(t, mp)
}

func TestFsmUnlinkInode_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmUnlinkInode(t, mp)
}

func testFsmAppendInode(t *testing.T, mp *metaPartition) {
	const ino = 1000
	prepareInodeForFsmInodeTest(t, mp, ino)

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	status, err := mp.fsmAppendExtentsWithCheck(handle, inode, false)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	inode, err = mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)

	// NOTE: random write to hole
	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	status, err = mp.fsmAppendExtentsWithCheck(handle, inode, false)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	_, err = mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
}

func TestFsmAppendInode(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmAppendInode(t, mp)
}

func TestFsmAppendInode_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmAppendInode(t, mp)
}

func testFsmAppendInodeRandomWrite(t *testing.T, mp *metaPartition) {
	const ino = 1000
	prepareInodeForFsmInodeTest(t, mp, ino)

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	status, err := mp.fsmAppendExtentsWithCheck(handle, inode, false)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	_, err = mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)

	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	status, err = mp.fsmAppendExtentsWithCheck(handle, inode, false)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	_, err = mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)

	// NOTE: random write to first extent
	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	status, err = mp.fsmAppendExtentsWithCheck(handle, inode, false)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	_, err = mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
}

func TestFsmAppendInodeRandomWrite(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmAppendInodeRandomWrite(t, mp)
}

func TestFsmAppendInodeRandomWrite_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmAppendInodeRandomWrite(t, mp)
}

func testFsmLinkInodeUniqIDIdempotent(t *testing.T, mp *metaPartition) {
	const ino = 2000
	prepareInodeForFsmInodeTest(t, mp, ino)
	mp.applyID = 100

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	resp, err := mp.fsmCreateLinkInode(handle, inode, 111)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
	checkInodeLinkForFsmInodeTest(t, mp, ino, 2)

	mp.applyID = 101
	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(ino, FileModeType)
	resp, err = mp.fsmCreateLinkInode(handle, inode, 111)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
	checkInodeLinkForFsmInodeTest(t, mp, ino, 2)
}

func TestFsmLinkInodeUniqIDIdempotent(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmLinkInodeUniqIDIdempotent(t, mp)
}

func TestFsmLinkInodeUniqIDIdempotent_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmLinkInodeUniqIDIdempotent(t, mp)
}

func testFsmUnlinkInodeUniqIDIdempotent(t *testing.T, mp *metaPartition) {
	const ino = 3000
	prepareInodeForFsmInodeTest(t, mp, ino)
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	resp, err := mp.fsmCreateLinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
	checkInodeLinkForFsmInodeTest(t, mp, ino, 2)

	mp.applyID = 200

	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(ino, FileModeType)
	resp, err = mp.fsmUnlinkInode(handle, inode, 222)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
	checkInodeLinkForFsmInodeTest(t, mp, ino, 1)

	mp.applyID = 201
	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode = NewInodeTest(ino, FileModeType)
	resp, err = mp.fsmUnlinkInode(handle, inode, 222)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
	checkInodeLinkForFsmInodeTest(t, mp, ino, 1)
}

func TestFsmUnlinkInodeUniqIDIdempotent(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmUnlinkInodeUniqIDIdempotent(t, mp)
}

func TestFsmUnlinkInodeUniqIDIdempotent_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmUnlinkInodeUniqIDIdempotent(t, mp)
}

func testFsmUnlinkFileInode(t *testing.T, mp *metaPartition) {
	const ino = 1000
	prepareInodeForFsmInodeTest(t, mp, ino)

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_Replica_SSD
	status, err := mp.fsmAppendExtentsWithCheck(handle, inode, false)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	resp, err := mp.fsmUnlinkInode(handle, inode, 0)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
}

func TestFsmUnlinkFileInode(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmUnlinkFileInode(t, mp)
}

func TestFsmUnlinkFileInode_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmUnlinkFileInode(t, mp)
}

func TestCleanRocksdbInodeTestDir(t *testing.T) {
	os.RemoveAll(RocksdbInodeTestDir)
}

func TestFsmUpdateExtentKeyAfterMigrationRejectsLeaseExpireMismatch(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	const ino = 8801
	prepareInodeForFsmInodeTest(t, mp, ino)

	param := NewInode(ino, 0)
	param.LeaseExpireTime = 999
	param.Generation = 2
	resp := mp.fsmUpdateExtentKeyAfterMigration(param)
	require.EqualValues(t, proto.OpLeaseOccupiedByOthers, resp.Status)
}

func applyUpdateInodeMetaForTest(t *testing.T, mp *metaPartition, req *UpdateInodeMetaRequest, index uint64) (resp interface{}, err error) {
	t.Helper()
	data, err := json.Marshal(req)
	require.NoError(t, err)
	item := NewMetaItem(0, nil, data)
	item.Op = opFSMUpdateInodeMeta
	cmd, err := item.MarshalJson()
	require.NoError(t, err)
	return mp.Apply(cmd, index)
}

func testFsmUpdateInodeMetaSuccess(t *testing.T, mp *metaPartition) {
	const ino = 20001
	prepareInodeForFsmInodeTest(t, mp, ino)
	before, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, before)
	genBefore := before.Generation

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	status := mp.fsmUpdateInodeMeta(handle, &UpdateInodeMetaRequest{
		Inode:       ino,
		PartitionID: mp.config.PartitionId,
	})
	require.EqualValues(t, proto.OpOk, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	after, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, after)
	require.EqualValues(t, genBefore+1, after.Generation)
}

func TestFsmUpdateInodeMetaSuccess(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmUpdateInodeMetaSuccess(t, mp)
}

func TestFsmUpdateInodeMetaSuccess_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmUpdateInodeMetaSuccess(t, mp)
}

func testFsmUpdateInodeMetaInodeNotExist(t *testing.T, mp *metaPartition) {
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	status := mp.fsmUpdateInodeMeta(handle, &UpdateInodeMetaRequest{
		Inode:       99999,
		PartitionID: mp.config.PartitionId,
	})
	require.EqualValues(t, proto.OpNotExistErr, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
}

func TestFsmUpdateInodeMetaInodeNotExist(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmUpdateInodeMetaInodeNotExist(t, mp)
}

func TestFsmUpdateInodeMetaInodeNotExist_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmUpdateInodeMetaInodeNotExist(t, mp)
}

func testFsmUpdateInodeMetaMarkedDelete(t *testing.T, mp *metaPartition) {
	const ino = 20002
	prepareInodeForFsmInodeTest(t, mp, ino)
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, inode)
	inode.Flag |= DeleteMarkFlag
	err = mp.inodeTree.Update(handle, inode)
	require.NoError(t, err)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	status := mp.fsmUpdateInodeMeta(handle, &UpdateInodeMetaRequest{
		Inode:       ino,
		PartitionID: mp.config.PartitionId,
	})
	require.EqualValues(t, proto.OpNotExistErr, status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)
}

func TestFsmUpdateInodeMetaMarkedDelete(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmUpdateInodeMetaMarkedDelete(t, mp)
}

func TestFsmUpdateInodeMetaApplyInodeNotExist(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	resp, err := applyUpdateInodeMetaForTest(t, mp, &UpdateInodeMetaRequest{
		Inode:       88888,
		PartitionID: mp.config.PartitionId,
	}, 1)
	require.NoError(t, err)
	msg, ok := resp.(*InodeResponse)
	require.True(t, ok)
	require.EqualValues(t, proto.OpNotExistErr, msg.Status)
}

func TestFsmUpdateInodeMetaApplySuccess(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	const ino = 20003
	prepareInodeForFsmInodeTest(t, mp, ino)

	resp, err := applyUpdateInodeMetaForTest(t, mp, &UpdateInodeMetaRequest{
		Inode:       ino,
		PartitionID: mp.config.PartitionId,
	}, 2)
	require.NoError(t, err)
	msg, ok := resp.(*InodeResponse)
	require.True(t, ok)
	require.EqualValues(t, proto.OpOk, msg.Status)
}

func TestFsmUpdateInodeMetaApplySuccess_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	const ino = 20004
	prepareInodeForFsmInodeTest(t, mp, ino)
	before, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, before)

	resp, err := applyUpdateInodeMetaForTest(t, mp, &UpdateInodeMetaRequest{
		Inode:       ino,
		PartitionID: mp.config.PartitionId,
	}, 2)
	require.NoError(t, err)
	msg, ok := resp.(*InodeResponse)
	require.True(t, ok)
	require.EqualValues(t, proto.OpOk, msg.Status)

	after, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, after)
	require.EqualValues(t, before.Generation+1, after.Generation)
}

func TestFsmUpdateInodeMetaCopyGetError(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	const ino = 20010
	prepareInodeForFsmInodeTest(t, mp, ino)
	base := mp.inodeTree
	mp.inodeTree = &errInjectInodeTree{
		InodeTree:  base,
		copyGetErr: fmt.Errorf("copyget failed"),
	}
	defer func() { mp.inodeTree = base }()

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	status := mp.fsmUpdateInodeMeta(handle, &UpdateInodeMetaRequest{
		Inode:       ino,
		PartitionID: mp.config.PartitionId,
	})
	require.EqualValues(t, proto.OpErr, status)
}

func TestFsmUpdateInodeMetaUpdateError(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	const ino = 20011
	prepareInodeForFsmInodeTest(t, mp, ino)
	base := mp.inodeTree
	mp.inodeTree = &errInjectInodeTree{
		InodeTree: base,
		updateErr: fmt.Errorf("update failed"),
	}
	defer func() { mp.inodeTree = base }()

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	status := mp.fsmUpdateInodeMeta(handle, &UpdateInodeMetaRequest{
		Inode:       ino,
		PartitionID: mp.config.PartitionId,
	})
	require.EqualValues(t, proto.OpErr, status)
}

func TestFsmAppendObjExtentsWithCheck(t *testing.T) {
	// Setup test partition
	mpC := &MetaPartitionConfig{
		PartitionId:   1,
		VolName:       "test_vol",
		Start:         0,
		End:           100,
		PartitionType: 1,
		RootDir:       "/tmp/test_mp",
		StoreMode:     proto.StoreModeMem, // Add StoreMode
	}
	metaM := &metadataManager{
		nodeId:          1,
		zoneName:        "test",
		raftStore:       nil,
		partitions:      make(map[uint64]MetaPartition),
		metaNode:        &MetaNode{},
		fileStatsConfig: &fileStatsConfig{},
	}
	partition := NewMetaPartition(mpC, metaM)
	mp := partition.(*metaPartition)

	// Initialize objects (inodeTree, dentryTree, etc.)
	err := mp.initObjects(true)
	require.NoError(t, err)

	// Initialize other required fields
	mp.uidManager = NewUidMgr(mpC.VolName, mpC.PartitionId)
	mp.mqMgr = NewQuotaManager(mpC.VolName, mpC.PartitionId)
	mp.uniqChecker = newUniqChecker()
	mp.vol = NewVol()

	// ==================== Basic error scenarios ====================
	t.Run("error - inode not exist", func(t *testing.T) {
		inoParam := NewInode(9999, 0)
		inoParam.StorageClass = proto.StorageClass_BlobStore
		inoParam.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
		})
		status, _ := mp.fsmAppendObjExtentsWithCheck(nil, inoParam)
		require.Equal(t, proto.OpNotExistErr, status)
	})

	t.Run("error - empty or too many extents", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()
		inoId := uint64(1001)
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		// Empty extents
		inoParam1 := NewInode(inoId, 0)
		inoParam1.StorageClass = proto.StorageClass_BlobStore
		inoParam1.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{})
		status1, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam1)
		require.Equal(t, proto.OpArgMismatchErr, status1)

		// Too many extents
		inoParam2 := NewInode(inoId, 0)
		inoParam2.StorageClass = proto.StorageClass_BlobStore
		inoParam2.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{FileOffset: 100, Size: 100},
			{FileOffset: 200, Size: 100},
		})
		status2, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam2)
		require.Equal(t, proto.OpArgMismatchErr, status2)
	})

	// ==================== Success scenario - insert new data ====================
	t.Run("success - insert to empty extents", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(2001)
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore

		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		inoParam := NewInode(inoId, 0)
		inoParam.StorageClass = proto.StorageClass_BlobStore
		inoParam.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{},
		})

		status, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam)
		require.Equal(t, proto.OpOk, status)

		updatedIno, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		sortedEks := updatedIno.HybridCloudExtents.sortedEks.(*SortedObjExtents)
		extents := sortedEks.CopyExtents()
		require.Equal(t, 1, len(extents))
		require.Equal(t, uint64(0), extents[0].FileOffset)
		require.Equal(t, uint64(100), extents[0].Size)
	})

	t.Run("success - insert before/after/middle", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(2002)
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 200, Size: 100},
		})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		// Insert before
		inoParam1 := NewInode(inoId, 0)
		inoParam1.StorageClass = proto.StorageClass_BlobStore
		inoParam1.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100}, {},
		})
		status1, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam1)
		require.Equal(t, proto.OpOk, status1)

		// Insert after
		inoParam2 := NewInode(inoId, 0)
		inoParam2.StorageClass = proto.StorageClass_BlobStore
		inoParam2.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 500, Size: 100}, {},
		})
		status2, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam2)
		require.Equal(t, proto.OpOk, status2)

		// Insert middle
		inoParam3 := NewInode(inoId, 0)
		inoParam3.StorageClass = proto.StorageClass_BlobStore
		inoParam3.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 350, Size: 100}, {},
		})
		status3, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam3)
		require.Equal(t, proto.OpOk, status3)

		// Verify all extents
		updatedIno, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		sortedEks := updatedIno.HybridCloudExtents.sortedEks.(*SortedObjExtents)
		extents := sortedEks.CopyExtents()
		require.Equal(t, 4, len(extents))
		require.Equal(t, uint64(0), extents[0].FileOffset)
		require.Equal(t, uint64(200), extents[1].FileOffset)
		require.Equal(t, uint64(350), extents[2].FileOffset)
		require.Equal(t, uint64(500), extents[3].FileOffset)
	})

	// ==================== Success scenario - conflict checks (replace and extend) ====================
	t.Run("success - exact match replace", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(3001)
		existingExtent := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 1, CodeMode: 1}
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{existingExtent})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		newExtent := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 2, CodeMode: 2}
		inoParam := NewInode(inoId, 0)
		inoParam.StorageClass = proto.StorageClass_BlobStore
		inoParam.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			newExtent,
			existingExtent, // discard
		})

		status, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam)
		require.Equal(t, proto.OpOk, status)

		updatedIno, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		sortedEks := updatedIno.HybridCloudExtents.sortedEks.(*SortedObjExtents)
		extents := sortedEks.CopyExtents()
		require.Equal(t, 1, len(extents))
		require.True(t, extents[0].IsEquals(&newExtent))
	})

	t.Run("success - extend last extent", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(4001)
		existingExtent := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 1, CodeMode: 1}
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{existingExtent})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		newExtent := proto.ObjExtentKey{FileOffset: 0, Size: 150, Cid: 2, CodeMode: 2}
		inoParam := NewInode(inoId, 0)
		inoParam.StorageClass = proto.StorageClass_BlobStore
		inoParam.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			newExtent,
			existingExtent, // discard
		})

		status, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam)
		require.Equal(t, proto.OpOk, status)

		updatedIno, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		sortedEks := updatedIno.HybridCloudExtents.sortedEks.(*SortedObjExtents)
		extents := sortedEks.CopyExtents()
		require.Equal(t, 1, len(extents))
		require.True(t, extents[0].IsEquals(&newExtent))
		require.Equal(t, uint64(150), extents[0].Size)
	})

	// ==================== Error scenario - conflict checks ====================
	t.Run("error - no overlap but discard provided", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(5001)
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
		})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		inoParam := NewInode(inoId, 0)
		inoParam.StorageClass = proto.StorageClass_BlobStore
		inoParam.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 500, Size: 100}, // no overlap
			{FileOffset: 0, Size: 100},   // discard provided
		})

		status, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam)
		require.Equal(t, proto.OpConflictExtentsErr, status)
	})

	t.Run("error - discard extent mismatch", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(5002)
		existingExtent := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 1, CodeMode: 1}
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{existingExtent})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		// Exact match but discard mismatch
		inoParam1 := NewInode(inoId, 0)
		inoParam1.StorageClass = proto.StorageClass_BlobStore
		inoParam1.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100, Cid: 2, CodeMode: 2},
			{FileOffset: 0, Size: 100, Cid: 3, CodeMode: 3}, // discard mismatch
		})
		status1, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam1)
		require.Equal(t, proto.OpConflictExtentsErr, status1)

		// Extend last but discard mismatch
		inoParam2 := NewInode(inoId, 0)
		inoParam2.StorageClass = proto.StorageClass_BlobStore
		inoParam2.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 150, Cid: 2, CodeMode: 2},
			{FileOffset: 0, Size: 100, Cid: 3, CodeMode: 3}, // discard mismatch
		})
		status2, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam2)
		require.Equal(t, proto.OpConflictExtentsErr, status2)
	})

	t.Run("error - invalid overlap scenarios", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(5003)
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{FileOffset: 200, Size: 100},
		})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		// Non-last extent extended
		inoParam1 := NewInode(inoId, 0)
		inoParam1.StorageClass = proto.StorageClass_BlobStore
		inoParam1.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 150}, // extend non-last
			{FileOffset: 0, Size: 100},
		})
		status1, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam1)
		require.Equal(t, proto.OpConflictExtentsErr, status1)

		// Partial overlap
		inoParam2 := NewInode(inoId, 0)
		inoParam2.StorageClass = proto.StorageClass_BlobStore
		inoParam2.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			{FileOffset: 50, Size: 100}, // partial overlap
			{FileOffset: 0, Size: 100},
		})
		status2, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam2)
		require.Equal(t, proto.OpConflictExtentsErr, status2)
	})

	// ==================== Repeated execution scenario - idempotency test ====================
	t.Run("success - repeat insert same extent", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(6001)
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		newExtent := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 1, CodeMode: 1}

		// First execution
		inoParam1 := NewInode(inoId, 0)
		inoParam1.StorageClass = proto.StorageClass_BlobStore
		inoParam1.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{newExtent, {}})
		status1, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam1)
		require.Equal(t, proto.OpOk, status1)

		// Second execution
		inoParam2 := NewInode(inoId, 0)
		inoParam2.StorageClass = proto.StorageClass_BlobStore
		inoParam2.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{newExtent, {}})
		status2, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam2)
		require.Equal(t, proto.OpOk, status2)

		// Verify extent count remains 1
		updatedIno, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		sortedEks := updatedIno.HybridCloudExtents.sortedEks.(*SortedObjExtents)
		extents := sortedEks.CopyExtents()
		require.Equal(t, 1, len(extents))
		require.True(t, extents[0].IsEquals(&newExtent))
	})

	t.Run("success - repeat replace and extend", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(6002)
		existingExtent := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 1, CodeMode: 1}
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{existingExtent})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		newExtent1 := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 2, CodeMode: 2}
		newExtent2 := proto.ObjExtentKey{FileOffset: 0, Size: 150, Cid: 3, CodeMode: 3}

		// First: exact match replace
		inoParam1 := NewInode(inoId, 0)
		inoParam1.StorageClass = proto.StorageClass_BlobStore
		inoParam1.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			newExtent1,
			existingExtent,
		})
		status1, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam1)
		require.Equal(t, proto.OpOk, status1)

		// Second: repeat replace (should succeed)
		inoParam2 := NewInode(inoId, 0)
		inoParam2.StorageClass = proto.StorageClass_BlobStore
		inoParam2.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			newExtent1,
			newExtent1,
		})
		status2, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam2)
		require.Equal(t, proto.OpOk, status2)

		// Third: extend further
		inoParam3 := NewInode(inoId, 0)
		inoParam3.StorageClass = proto.StorageClass_BlobStore
		inoParam3.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			newExtent2,
			newExtent1,
		})
		status3, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam3)
		require.Equal(t, proto.OpOk, status3)

		// Verify final state
		updatedIno, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		sortedEks := updatedIno.HybridCloudExtents.sortedEks.(*SortedObjExtents)
		extents := sortedEks.CopyExtents()
		require.Equal(t, 1, len(extents))
		require.True(t, extents[0].IsEquals(&newExtent2))
		require.Equal(t, uint64(150), extents[0].Size)
	})
}

// --- TruncateV2 test helpers (contract = blobstore plan + Example comment in fsmExtentsTruncateV2) ---

func sparseTruncateV2ExampleExtents() []proto.ObjExtentKey {
	return []proto.ObjExtentKey{
		{FileOffset: 10, Size: 20}, // [10,30)
		{FileOffset: 35, Size: 10}, // [35,45)
		{FileOffset: 60, Size: 10}, // [60,70)
	}
}

// applyTruncateV2Contract mirrors checkTruncateV2Conflict apply: prefix before ToDelete anchor + optional NewObjExtent; enqueue = eks[delIdx:].
func applyTruncateV2Contract(eks []proto.ObjExtentKey, target uint64, newObj, toDelete proto.ObjExtentKey) (final, enqueue []proto.ObjExtentKey) {
	if len(eks) == 0 {
		if !newObj.IsEmpty() || !toDelete.IsEmpty() {
			return nil, nil
		}
		return nil, nil
	}
	lastEk := eks[len(eks)-1]
	if toDelete.IsEmpty() {
		if !newObj.IsEmpty() {
			return nil, nil
		}
		if target < lastEk.FileOffset+lastEk.Size {
			return nil, nil
		}
		return append([]proto.ObjExtentKey(nil), eks...), nil
	}
	delIdx := -1
	var extent proto.ObjExtentKey
	for i, ek := range eks {
		if ek.FileOffset == toDelete.FileOffset {
			extent = ek
			delIdx = i
			break
		}
	}
	if delIdx < 0 {
		return nil, nil
	}
	if extent.FileOffset != toDelete.FileOffset || extent.Size != toDelete.Size || target > extent.FileOffset+extent.Size {
		if newObj.IsEmpty() && extent.IsEmpty() && lastEk.FileOffset+lastEk.Size <= target {
			return append([]proto.ObjExtentKey(nil), eks...), nil
		}
		return nil, nil
	}
	enqueue = append([]proto.ObjExtentKey(nil), eks[delIdx:]...)
	final = append([]proto.ObjExtentKey(nil), eks[:delIdx]...)
	if !newObj.IsEmpty() {
		final = append(final, newObj)
	}
	return final, enqueue
}

func objExtentDelDedupKey(o proto.ObjExtentKey) string {
	return fmt.Sprintf("%d:%d:%d", o.FileOffset, o.Size, o.Cid)
}

func collectAllObjExtentDelOeks(ot ObjExtentDelTreeAPI) []proto.ObjExtentKey {
	if ot == nil || ot.Len() == 0 {
		return nil
	}
	batch := ot.PeekFirstN(ot.Len())
	out := make([]proto.ObjExtentKey, 0)
	for _, item := range batch.Items {
		out = append(out, item.Oeks...)
	}
	return out
}

func inodeOekAtOffset(eks []proto.ObjExtentKey, fileOffset uint64) proto.ObjExtentKey {
	for _, ek := range eks {
		if ek.FileOffset == fileOffset {
			return ek
		}
	}
	return proto.ObjExtentKey{}
}

// buildTruncateV2ReqFromCompute builds a TruncateRequest from blobstore.ComputeTruncateReqs on inode snapshot.
// newObj uses keep offset/size with synthetic Cid (EBS object identity is not compared on partial path).
func buildTruncateV2ReqFromCompute(ino, target uint64, eks []proto.ObjExtentKey) *proto.TruncateRequest {
	plan := blobstore.ComputeTruncateReqs(target, eks)
	req := &proto.TruncateRequest{Inode: ino, Size: target}
	if !plan.KeepExtent.IsEmpty() {
		req.NewObjExtent = plan.KeepExtent
		req.NewObjExtent.Cid = testTruncateV2SyntheticCid
		req.ToDelete = inodeOekAtOffset(eks, plan.DiscardFrom.FileOffset)
		if req.ToDelete.IsEmpty() {
			req.ToDelete = plan.DiscardFrom
		}
	} else if !plan.DiscardFrom.IsEmpty() {
		req.ToDelete = inodeOekAtOffset(eks, plan.DiscardFrom.FileOffset)
		if req.ToDelete.IsEmpty() {
			req.ToDelete = plan.DiscardFrom
		}
	}
	return req
}

const testTruncateV2SyntheticCid = 424242

func assertObjExtentsEqual(t *testing.T, want, got []proto.ObjExtentKey) {
	t.Helper()
	require.Len(t, got, len(want))
	for i := range want {
		require.Equal(t, want[i].FileOffset, got[i].FileOffset, "idx %d FileOffset", i)
		require.Equal(t, want[i].Size, got[i].Size, "idx %d Size", i)
	}
}

func assertObjExtentsWithinInodeSize(t *testing.T, oeks []proto.ObjExtentKey, inodeSize uint64) {
	t.Helper()
	var maxEnd uint64
	for _, o := range oeks {
		if o.Size == 0 {
			continue
		}
		require.LessOrEqual(t, o.FileOffset+o.Size, inodeSize)
		if e := o.FileOffset + o.Size; e > maxEnd {
			maxEnd = e
		}
	}
	require.LessOrEqual(t, maxEnd, inodeSize)
}

// TestCheckTruncateV2Conflict_ExampleSparse exercises checkTruncateV2Conflict against fsmExtentsTruncateV2 Example rows.
func TestCheckTruncateV2Conflict_ExampleSparse(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10010)
	const ino = 9010
	base := sparseTruncateV2ExampleExtents()

	cases := []struct {
		name       string
		target     uint64
		newObj     proto.ObjExtentKey
		toDelete   proto.ObjExtentKey
		wantStatus uint8
	}{
		{
			name:       "target=0 tail anchor first oek",
			target:     0,
			toDelete:   base[0],
			wantStatus: proto.OpOk,
		},
		{
			name:       "target=15 partial plus tail sweep",
			target:     15,
			newObj:     proto.ObjExtentKey{FileOffset: 10, Size: 5, Cid: testTruncateV2SyntheticCid},
			toDelete:   base[0],
			wantStatus: proto.OpOk,
		},
		{
			name:       "target=50 tail only",
			target:     50,
			toDelete:   base[2],
			wantStatus: proto.OpOk,
		},
		{
			name:       "target=65 partial only",
			target:     65,
			newObj:     proto.ObjExtentKey{FileOffset: 60, Size: 5, Cid: testTruncateV2SyntheticCid},
			toDelete:   base[2],
			wantStatus: proto.OpOk,
		},
		{
			name:       "target=100 logical hole",
			target:     100,
			wantStatus: proto.OpOk,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &proto.TruncateRequest{Inode: ino, Size: tc.target, NewObjExtent: tc.newObj, ToDelete: tc.toDelete}
			st, final, enqueue := mp.checkTruncateV2Conflict(req, append([]proto.ObjExtentKey(nil), base...))
			require.Equal(t, tc.wantStatus, st)
			if st != proto.OpOk {
				return
			}
			wantFinal, wantEnqueue := applyTruncateV2Contract(base, tc.target, tc.newObj, tc.toDelete)
			assertObjExtentsEqual(t, wantFinal, final)
			require.Len(t, enqueue, len(wantEnqueue))
		})
	}
}

// TestFsmExtentsTruncateV2_ExampleSparse runs full FSM for each row in fsmExtentsTruncateV2 Example comment.
func TestFsmExtentsTruncateV2_ExampleSparse(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10011)
	const ino = 9011
	base := sparseTruncateV2ExampleExtents()

	cases := []struct {
		name     string
		target   uint64
		newObj   proto.ObjExtentKey
		toDelete proto.ObjExtentKey
	}{
		{name: "target=0", target: 0, toDelete: base[0]},
		{
			name:     "target=15",
			target:   15,
			newObj:   proto.ObjExtentKey{FileOffset: 10, Size: 5, Cid: testTruncateV2SyntheticCid},
			toDelete: base[0],
		},
		{name: "target=50", target: 50, toDelete: base[2]},
		{
			name:     "target=65",
			target:   65,
			newObj:   proto.ObjExtentKey{FileOffset: 60, Size: 5, Cid: testTruncateV2SyntheticCid},
			toDelete: base[2],
		},
		{name: "target=100", target: 100},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupInodeWithObjExtents(t, mp, ino, base, 200)
			resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
				Inode: ino, Size: tc.target, NewObjExtent: tc.newObj, ToDelete: tc.toDelete,
			})
			require.Equal(t, proto.OpOk, resp.Status)
			updated := getInodeOrFail(t, mp, ino)
			require.Equal(t, tc.target, updated.Size)
			wantFinal, _ := applyTruncateV2Contract(base, tc.target, tc.newObj, tc.toDelete)
			assertObjExtentsEqual(t, wantFinal, updated.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
		})
	}
}

// TestCheckTruncateV2Conflict_BlobstoreComputeTruncateReqs aligns check with ComputeTruncateReqs plans.
func TestCheckTruncateV2Conflict_BlobstoreComputeTruncateReqs(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10012)
	const ino = 9012

	t.Run("empty extents", func(t *testing.T) {
		req := buildTruncateV2ReqFromCompute(ino, 100, nil)
		st, final, _ := mp.checkTruncateV2Conflict(req, nil)
		require.Equal(t, proto.OpOk, st)
		require.Nil(t, final)
	})

	t.Run("logical hole past last end", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 50}, {FileOffset: 50, Size: 50}}
		req := buildTruncateV2ReqFromCompute(ino, 100, eks)
		plan := blobstore.ComputeTruncateReqs(100, eks)
		require.True(t, plan.KeepExtent.IsEmpty())
		require.True(t, plan.DiscardFrom.IsEmpty())
		st, final, _ := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpOk, st)
		assertObjExtentsEqual(t, eks, final)
	})

	t.Run("partial span target 150", func(t *testing.T) {
		eks := []proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{FileOffset: 100, Size: 100},
			{FileOffset: 200, Size: 50},
		}
		target := uint64(150)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		plan := blobstore.ComputeTruncateReqs(target, eks)
		require.Equal(t, uint64(100), plan.KeepExtent.FileOffset)
		require.Equal(t, uint64(50), plan.KeepExtent.Size)
		require.Equal(t, uint64(100), plan.DiscardFrom.FileOffset)
		require.Equal(t, uint64(100), plan.DiscardFrom.Size)

		st, final, enqueue := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpOk, st)
		wantFinal, wantEnqueue := applyTruncateV2Contract(eks, target, req.NewObjExtent, req.ToDelete)
		assertObjExtentsEqual(t, wantFinal, final)
		require.Len(t, enqueue, len(wantEnqueue))
	})

	t.Run("integer boundary target equals oek end", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}}
		target := uint64(100)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		plan := blobstore.ComputeTruncateReqs(target, eks)
		require.True(t, plan.KeepExtent.IsEmpty())
		require.Equal(t, uint64(100), plan.DiscardFrom.FileOffset)

		st, final, _ := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpOk, st)
		wantFinal, _ := applyTruncateV2Contract(eks, target, req.NewObjExtent, req.ToDelete)
		assertObjExtentsEqual(t, wantFinal, final)
	})

	t.Run("unsorted input sorted by compute", func(t *testing.T) {
		eks := []proto.ObjExtentKey{
			{FileOffset: 10, Size: 10},
			{FileOffset: 0, Size: 10},
			{FileOffset: 20, Size: 10},
		}
		target := uint64(15)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		sorted := append([]proto.ObjExtentKey(nil), eks...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].FileOffset < sorted[j].FileOffset })
		st, final, _ := mp.checkTruncateV2Conflict(req, sorted)
		require.Equal(t, proto.OpOk, st)
		wantFinal, _ := applyTruncateV2Contract(sorted, target, req.NewObjExtent, req.ToDelete)
		assertObjExtentsEqual(t, wantFinal, final)
	})

	t.Run("reject New without ToDelete when plan has partial", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 100}}
		req := &proto.TruncateRequest{Inode: ino, Size: 50, NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 50, Cid: 1}}
		st, _, _ := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpConflictExtentsErr, st)
	})

	t.Run("reject ToDelete size mismatch", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}}
		req := &proto.TruncateRequest{Inode: ino, Size: 100, ToDelete: proto.ObjExtentKey{FileOffset: 100, Size: 99}}
		st, _, _ := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpConflictExtentsErr, st)
	})

	t.Run("reject target beyond ToDelete end", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 60, Size: 10}}
		req := &proto.TruncateRequest{Inode: ino, Size: 80, ToDelete: eks[0]}
		st, _, _ := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpConflictExtentsErr, st)
	})

	t.Run("idempotent last end le target", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 50}}
		req := &proto.TruncateRequest{Inode: ino, Size: 100, ToDelete: proto.ObjExtentKey{FileOffset: 999, Size: 1}}
		st, final, _ := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpOk, st)
		assertObjExtentsEqual(t, eks, final)
	})
}

// TestFsmExtentsTruncateV2_MultiRound_16KB chains truncate on one inode (two 8KiB stripes): 4K → 12K → 6K.
// Asserts per-round extents, objExtentDelTree enqueue, and no duplicate (FileOffset, Size, Cid) discard keys.
func TestFsmExtentsTruncateV2_MultiRound_16KB(t *testing.T) {
	const KiB = uint64(1024)
	const ino = uint64(9020)
	mp := newTestMetaPartitionForTruncateV2(t, 10016)

	initial := []proto.ObjExtentKey{
		{FileOffset: 0, Size: 8 * KiB, Cid: 1},
		{FileOffset: 8 * KiB, Size: 8 * KiB, Cid: 2},
	}
	setupInodeWithObjExtents(t, mp, ino, initial, 16*KiB)

	eks := append([]proto.ObjExtentKey(nil), initial...)
	seenDel := make(map[string]struct{})
	var allEnqueued []proto.ObjExtentKey

	round := func(name string, target uint64) {
		t.Helper()
		mp.fsmRaftApplyIndex++

		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		req.Timestamp = int64(target)

		st, wantFinal, wantEnqueue := mp.checkTruncateV2Conflict(req, eks)
		require.Equal(t, proto.OpOk, st, "%s checkTruncateV2Conflict", name)

		resp := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp.Status, "%s fsmExtentsTruncateV2", name)

		for _, d := range wantEnqueue {
			k := objExtentDelDedupKey(d)
			_, dup := seenDel[k]
			require.False(t, dup, "%s duplicate discard key %v", name, d)
			seenDel[k] = struct{}{}
		}
		allEnqueued = append(allEnqueued, wantEnqueue...)

		updated := getInodeOrFail(t, mp, ino)
		require.Equal(t, target, updated.Size, name)
		gotEks := updated.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents()
		assertObjExtentsEqual(t, wantFinal, gotEks)

		eks = gotEks
	}

	round("to_4KiB", 4*KiB)
	require.NotEmpty(t, eks)
	round("to_12KiB", 12*KiB)
	round("to_6KiB", 6*KiB)

	require.Equal(t, uint64(6*KiB), getInodeOrFail(t, mp, ino).Size)
	assertObjExtentsWithinInodeSize(t, eks, 6*KiB)
	require.NotEmpty(t, allEnqueued)

	treeOeks := collectAllObjExtentDelOeks(mp.objExtentDelTree)
	require.NotEmpty(t, treeOeks)
	seenInTree := make(map[string]struct{})
	for _, d := range treeOeks {
		k := objExtentDelDedupKey(d)
		_, dup := seenInTree[k]
		require.False(t, dup, "objExtentDelTree duplicate discard key %v", d)
		seenInTree[k] = struct{}{}
	}
	for _, d := range allEnqueued {
		_, ok := seenInTree[objExtentDelDedupKey(d)]
		require.True(t, ok, "expected enqueued key in del tree: %v", d)
	}
}

// TestFsmExtentsTruncateV2_BlobstoreComputeTruncateReqs runs FSM with requests derived from ComputeTruncateReqs.
func TestFsmExtentsTruncateV2_BlobstoreComputeTruncateReqs(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10013)
	const ino = 9013

	t.Run("partial span 150", func(t *testing.T) {
		eks := []proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{FileOffset: 100, Size: 100},
			{FileOffset: 200, Size: 50},
		}
		target := uint64(150)
		setupInodeWithObjExtents(t, mp, ino, eks, 250)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		resp := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp.Status)
		wantFinal, _ := applyTruncateV2Contract(eks, target, req.NewObjExtent, req.ToDelete)
		got := getInodeOrFail(t, mp, ino).HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents()
		assertObjExtentsEqual(t, wantFinal, got)
	})

	t.Run("integer boundary 100", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}}
		target := uint64(100)
		setupInodeWithObjExtents(t, mp, ino, eks, 200)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		resp := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp.Status)
		wantFinal, _ := applyTruncateV2Contract(eks, target, req.NewObjExtent, req.ToDelete)
		assertObjExtentsEqual(t, wantFinal, getInodeOrFail(t, mp, ino).HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
	})
}

func TestFsmExtentsTruncateV2_FsmValidationAndApplyErrors(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10007)
	const inoId = 9004
	setupInodeWithObjExtents(t, mp, inoId, []proto.ObjExtentKey{{FileOffset: 0, Size: 100}}, 100)

	t.Run("missing ToDelete when partial New set", func(t *testing.T) {
		resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
			Inode:        inoId,
			Size:         50,
			NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 50, Cid: 1},
		})
		require.Equal(t, proto.OpConflictExtentsErr, resp.Status)
	})

	t.Run("NewObjExtent without ToDelete", func(t *testing.T) {
		resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
			Inode:        inoId,
			Size:         50,
			NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 50, Cid: 1},
		})
		require.Equal(t, proto.OpConflictExtentsErr, resp.Status)
	})

	t.Run("ToDelete size mismatch", func(t *testing.T) {
		setupInodeWithObjExtents(t, mp, inoId, []proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{FileOffset: 100, Size: 100},
		}, 200)
		resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
			Inode:    inoId,
			Size:     100,
			ToDelete: proto.ObjExtentKey{FileOffset: 100, Size: 99},
		})
		require.Equal(t, proto.OpConflictExtentsErr, resp.Status)
	})

	t.Run("inodeTree Put error", func(t *testing.T) {
		setupInodeWithObjExtents(t, mp, inoId, []proto.ObjExtentKey{
			{FileOffset: 0, Size: 50},
			{FileOffset: 50, Size: 50},
		}, 100)
		patches := gomonkey.NewPatches()
		defer patches.Reset()
		patches.ApplyMethod(reflect.TypeOf(mp.inodeTree), "Put",
			func(_ *InodeBTree, _ interface{}, _ *Inode) error {
				return errors.New("put failed")
			})
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		resp, err := mp.fsmExtentsTruncateV2(handle, &proto.TruncateRequest{
			Inode:    inoId,
			Size:     50,
			ToDelete: proto.ObjExtentKey{FileOffset: 50, Size: 50},
		})
		require.Equal(t, proto.OpErr, resp.Status)
		require.Error(t, err)
		_ = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	})

	t.Run("NewObjExtent offset missing in inode", func(t *testing.T) {
		resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
			Inode:        inoId,
			Size:         50,
			NewObjExtent: proto.ObjExtentKey{FileOffset: 999, Size: 1},
		})
		require.Equal(t, proto.OpConflictExtentsErr, resp.Status)
	})

	t.Run("ToDelete anchor not found", func(t *testing.T) {
		setupInodeWithObjExtents(t, mp, inoId, []proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
		}, 100)
		resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
			Inode:    inoId,
			Size:     50,
			ToDelete: proto.ObjExtentKey{FileOffset: 999, Size: 1},
		})
		require.Equal(t, proto.OpConflictExtentsErr, resp.Status)
	})

	t.Run("already deleted returns ok", func(t *testing.T) {
		setupInodeWithObjExtents(t, mp, inoId, []proto.ObjExtentKey{
			{FileOffset: 0, Size: 50},
		}, 50)
		resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
			Inode:    inoId,
			Size:     50,
			ToDelete: proto.ObjExtentKey{FileOffset: 999, Size: 1},
		})
		require.Equal(t, proto.OpOk, resp.Status)
	})

	t.Run("dir inode rejected", func(t *testing.T) {
		const dirIno = 9005
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		dir := NewInode(dirIno, DirModeType)
		dir.StorageClass = proto.StorageClass_BlobStore
		mp.inodeTree.ReplaceOrInsert(handle, dir, true)
		require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))

		h2, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		resp, err := mp.fsmExtentsTruncateV2(h2, &proto.TruncateRequest{Inode: dirIno, Size: 0})
		require.NoError(t, err)
		require.Equal(t, proto.OpArgMismatchErr, resp.Status)
		require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(h2, false))
	})
}

func newTestMetaPartitionForTruncateV2(t *testing.T, partID uint64) *metaPartition {
	t.Helper()
	mpC := &MetaPartitionConfig{
		PartitionId:   partID,
		VolName:       VolNameForTest,
		PartitionType: proto.VolumeTypeHot,
		StoreMode:     proto.StoreModeMem,
	}
	metaM := &metadataManager{
		nodeId:          1,
		zoneName:        "test",
		partitions:      make(map[uint64]MetaPartition),
		metaNode:        &MetaNode{},
		fileStatsConfig: &fileStatsConfig{},
	}
	partition := NewMetaPartition(mpC, metaM)
	mp := partition.(*metaPartition)
	require.NoError(t, mp.initObjects(true))
	mp.uidManager = NewUidMgr(mpC.VolName, mpC.PartitionId)
	mp.mqMgr = NewQuotaManager(mpC.VolName, mpC.PartitionId)
	mp.uniqChecker = newUniqChecker()
	mp.vol = NewVol()
	mp.fsmRaftApplyIndex = 12345
	return mp
}

func setupInodeWithObjExtents(t *testing.T, mp *metaPartition, inoId uint64, eks []proto.ObjExtentKey, size uint64) {
	t.Helper()
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	fsmIno := NewInode(inoId, 0)
	fsmIno.StorageClass = proto.StorageClass_BlobStore
	fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks(eks)
	fsmIno.Size = size
	mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))
}

func runFsmExtentsTruncateV2(t *testing.T, mp *metaPartition, truncReq *proto.TruncateRequest) *InodeResponse {
	t.Helper()
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	resp, err := mp.fsmExtentsTruncateV2(handle, truncReq)
	require.NoError(t, err)
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))
	return resp
}

func getInodeOrFail(t *testing.T, mp *metaPartition, inoId uint64) *Inode {
	t.Helper()
	ino, err := mp.inodeTree.CopyGet(&Inode{Inode: inoId})
	require.NoError(t, err)
	require.NotNil(t, ino)
	return ino
}

func TestCheckTruncateV2Conflict_EmptyEks(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10014)

	st, final, del := mp.checkTruncateV2Conflict(&proto.TruncateRequest{Inode: 1, Size: 100}, nil)
	require.Equal(t, proto.OpOk, st)
	require.Nil(t, final)
	require.Nil(t, del)

	st, final, del = mp.checkTruncateV2Conflict(&proto.TruncateRequest{
		Inode:    1,
		Size:     100,
		ToDelete: proto.ObjExtentKey{FileOffset: 0, Size: 1},
	}, nil)
	require.Equal(t, proto.OpConflictExtentsErr, st)
	require.Nil(t, final)
	require.Nil(t, del)
}

func TestFsmExtentsTruncateV2_EmptyObjExtents(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10015)
	const inoId = 9100

	setupInodeWithObjExtents(t, mp, inoId, nil, 0)
	resp := runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{Inode: inoId, Size: 128})
	require.Equal(t, proto.OpOk, resp.Status)
	ino := getInodeOrFail(t, mp, inoId)
	require.Equal(t, uint64(128), ino.Size)
	require.Equal(t, uint64(2), ino.Generation)
	require.Len(t, ino.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents(), 0)

	resp = runFsmExtentsTruncateV2(t, mp, &proto.TruncateRequest{
		Inode:    inoId,
		Size:     64,
		ToDelete: proto.ObjExtentKey{FileOffset: 0, Size: 1},
	})
	require.Equal(t, proto.OpConflictExtentsErr, resp.Status)
	ino = getInodeOrFail(t, mp, inoId)
	require.Equal(t, uint64(128), ino.Size)
}

func TestFsmExtentsTruncateV2_Errors(t *testing.T) {
	mpC := &MetaPartitionConfig{
		PartitionId:   10002,
		VolName:       VolNameForTest,
		PartitionType: proto.VolumeTypeHot,
		StoreMode:     proto.StoreModeMem,
	}
	metaM := &metadataManager{
		nodeId:          1,
		zoneName:        "test",
		raftStore:       nil,
		partitions:      make(map[uint64]MetaPartition),
		metaNode:        &MetaNode{},
		fileStatsConfig: &fileStatsConfig{},
	}
	partition := NewMetaPartition(mpC, metaM)
	mp := partition.(*metaPartition)
	err := mp.initObjects(true)
	require.NoError(t, err)
	mp.uidManager = NewUidMgr(mpC.VolName, mpC.PartitionId)
	mp.mqMgr = NewQuotaManager(mpC.VolName, mpC.PartitionId)
	mp.uniqChecker = newUniqChecker()
	mp.vol = NewVol()

	t.Run("inode not exist", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		resp, err := mp.fsmExtentsTruncateV2(handle, &proto.TruncateRequest{
			Inode:        7777,
			Size:         10,
			NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 10},
		})
		require.NoError(t, err)
		require.Equal(t, proto.OpNotExistErr, resp.Status)
		require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))
	})

	t.Run("inode storage class is not blobstore", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		ino := NewInode(8888, 0)
		ino.StorageClass = proto.StorageClass_Replica_SSD
		mp.inodeTree.ReplaceOrInsert(handle, ino, true)
		require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))

		handle2, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		resp, err := mp.fsmExtentsTruncateV2(handle2, &proto.TruncateRequest{
			Inode:        8888,
			Size:         10,
			NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 10},
		})
		require.NoError(t, err)
		require.Equal(t, proto.OpArgMismatchErr, resp.Status)
		require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle2, false))
	})
}
