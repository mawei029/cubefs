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

const fsmInodeQuotaID uint32 = 42

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

func TestFsmUpdateExtentKeyAfterMigrationBumpsGeneration(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	const ino = 8802
	prepareInodeForFsmInodeTest(t, mp, ino)

	before, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, before)
	genBefore := before.Generation

	param := NewInode(ino, FileModeType)
	param.UpdateHybridCloudParams(before)
	param.HybridCloudExtentsMigration.storageClass = proto.StorageClass_Replica_HDD
	param.HybridCloudExtentsMigration.poolId = proto.DefaultHDDPoolId
	param.HybridCloudExtentsMigration.sortedEks = NewSortedExtents()
	param.HybridCloudExtentsMigration.expiredTime = time.Now().Add(time.Hour).Unix()

	resp := mp.fsmUpdateExtentKeyAfterMigration(param)
	require.EqualValues(t, proto.OpOk, resp.Status)

	after, err := mp.inodeTree.Get(&Inode{Inode: ino})
	require.NoError(t, err)
	require.NotNil(t, after)
	require.EqualValues(t, genBefore+1, after.Generation,
		"successful migration must bump generation so clients refresh stale extent cache")
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

		mp.fsmRaftApplyIndex = 42
		require.Equal(t, 0, mp.objExtentDelTree.Len())

		status, _ := mp.fsmAppendObjExtentsWithCheck(handle, inoParam)
		require.Equal(t, proto.OpOk, status)
		require.Equal(t, 1, mp.objExtentDelTree.Len())
		peek := mp.objExtentDelTree.PeekFirstN(1)
		require.Len(t, peek.Items, 1)
		require.Equal(t, inoId, peek.Items[0].Inode)
		require.Equal(t, uint64(42), peek.Items[0].RaftIdx)
		require.True(t, peek.Items[0].Oeks[0].IsEquals(&existingExtent))

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

	// Raft replay carries the same log payload as first apply: (newExtent, oldDiscard).
	// After success inode holds newExtent; stale oldDiscard must not trigger OpConflictExtentsErr.
	t.Run("success - exact replace raft replay with stale oldDiscard", func(t *testing.T) {
		handle, err := mp.inodeTree.CreateBatchWriteHandle()
		require.NoError(t, err)
		defer func() {
			err := mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
			require.NoError(t, err)
		}()

		inoId := uint64(6003)
		oldDiscard := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 1, CodeMode: 1}
		newExtent := proto.ObjExtentKey{FileOffset: 0, Size: 100, Cid: 2, CodeMode: 2}
		fsmIno := NewInode(inoId, 0)
		fsmIno.StorageClass = proto.StorageClass_BlobStore
		fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{oldDiscard})
		mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)

		// Simulates committed Raft log entry (BatchObjExtentAppendWithCheck marshals [new, discard]).
		raftPayload := NewInode(inoId, 0)
		raftPayload.StorageClass = proto.StorageClass_BlobStore
		raftPayload.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
			newExtent,
			oldDiscard,
		})

		const raftIdx = uint64(100)
		mp.fsmRaftApplyIndex = raftIdx
		delTreeBefore := mp.objExtentDelTree.Len()

		status1, _ := mp.fsmAppendObjExtentsWithCheck(handle, raftPayload)
		require.Equal(t, proto.OpOk, status1)

		afterApply, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		genAfterApply := afterApply.Generation
		extentsAfterApply := afterApply.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents()
		require.Len(t, extentsAfterApply, 1)
		require.True(t, extentsAfterApply[0].IsEquals(&newExtent))
		require.Equal(t, delTreeBefore+1, mp.objExtentDelTree.Len())

		// Replay: identical payload and Raft apply index (stale oldDiscard still present).
		mp.fsmRaftApplyIndex = raftIdx
		status2, _ := mp.fsmAppendObjExtentsWithCheck(handle, raftPayload)
		require.Equal(t, proto.OpOk, status2, "raft replay must not conflict on stale oldDiscard")

		afterReplay, err := mp.inodeTree.CopyGet(fsmIno)
		require.NoError(t, err)
		extentsAfterReplay := afterReplay.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents()
		require.Len(t, extentsAfterReplay, 1)
		require.True(t, extentsAfterReplay[0].IsEquals(&newExtent))
		require.Greater(t, afterReplay.Generation, genAfterApply)
		require.Equal(t, delTreeBefore+1, mp.objExtentDelTree.Len(), "delTree enqueue is idempotent by RaftIdx")
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
	return ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{
		{FileOffset: 10, Size: 20, Cid: 1}, // [10,30)
		{FileOffset: 35, Size: 10, Cid: 2}, // [35,45)
		{FileOffset: 60, Size: 10, Cid: 3}, // [60,70)
	})
}

const testTruncateV2SyntheticCid = 424242

func testTruncateV2BaseCrc(fileOffset uint64) uint32 {
	return 1000 + uint32(fileOffset)
}

func testTruncateV2PartialNewCrc(anchorCrc uint32, keepSize uint64) uint32 {
	return anchorCrc + 10000 + uint32(keepSize)
}

func withTruncateV2TestExtentCrc(o proto.ObjExtentKey) proto.ObjExtentKey {
	if o.IsEmpty() {
		return o
	}
	if o.Crc == 0 {
		o.Crc = testTruncateV2BaseCrc(o.FileOffset)
	}
	return o
}

func ensureTruncateV2ExtentSliceCrcs(eks []proto.ObjExtentKey) []proto.ObjExtentKey {
	if len(eks) == 0 {
		return nil
	}
	out := make([]proto.ObjExtentKey, len(eks))
	for i, ek := range eks {
		out[i] = withTruncateV2TestExtentCrc(ek)
	}
	return out
}

func truncateV2NewObjFromPartial(keepOffset, keepSize uint64, anchor proto.ObjExtentKey) proto.ObjExtentKey {
	anchor = withTruncateV2TestExtentCrc(anchor)
	return proto.ObjExtentKey{
		FileOffset: keepOffset,
		Size:       keepSize,
		Cid:        testTruncateV2SyntheticCid,
		Crc:        testTruncateV2PartialNewCrc(anchor.Crc, keepSize),
	}
}

func finalizeTruncateV2ReqFromInodeSnapshot(req *proto.TruncateRequest, preEks []proto.ObjExtentKey) {
	if req == nil {
		return
	}
	if !req.ToDelete.IsEmpty() {
		if anchor := inodeOekAtOffset(preEks, req.ToDelete.FileOffset); !anchor.IsEmpty() {
			req.ToDelete = anchor
		} else {
			req.ToDelete = withTruncateV2TestExtentCrc(req.ToDelete)
		}
	}
	if !req.NewObjExtent.IsEmpty() {
		req.NewObjExtent.Cid = testTruncateV2SyntheticCid
		if req.NewObjExtent.Crc == 0 && !req.ToDelete.IsEmpty() {
			req.NewObjExtent.Crc = testTruncateV2PartialNewCrc(req.ToDelete.Crc, req.NewObjExtent.Size)
		}
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
		if newObj.IsEmpty() && lastEk.FileOffset+lastEk.Size <= target {
			return append([]proto.ObjExtentKey(nil), eks...), nil
		}
		return nil, nil
	}
	if extent.FileOffset != toDelete.FileOffset || extent.Size != toDelete.Size ||
		target > extent.FileOffset+extent.Size || toDelete.Crc != extent.Crc {
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
	eks = ensureTruncateV2ExtentSliceCrcs(eks)
	plan := blobstore.ComputeTruncateReqs(target, blobstore.NewReadOnlyOeks(eks))
	req := &proto.TruncateRequest{Inode: ino, Size: target}
	if !plan.KeepExtent.IsEmpty() {
		req.NewObjExtent = plan.KeepExtent
		req.ToDelete = inodeOekAtOffset(eks, plan.DiscardFrom.FileOffset)
		if req.ToDelete.IsEmpty() {
			req.ToDelete = withTruncateV2TestExtentCrc(plan.DiscardFrom)
		}
	} else if !plan.DiscardFrom.IsEmpty() {
		req.ToDelete = inodeOekAtOffset(eks, plan.DiscardFrom.FileOffset)
		if req.ToDelete.IsEmpty() {
			req.ToDelete = withTruncateV2TestExtentCrc(plan.DiscardFrom)
		}
	}
	finalizeTruncateV2ReqFromInodeSnapshot(req, eks)
	return req
}

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

// truncInodeSizePreApply estimates inode logical size before TruncateV2 apply (for checkTruncateV2Conflict UT).
func truncInodeSizePreApply(req *proto.TruncateRequest, eks []proto.ObjExtentKey) uint64 {
	if len(eks) == 0 {
		return req.Size
	}
	var maxEnd uint64
	for _, o := range eks {
		if e := o.FileOffset + o.Size; e > maxEnd {
			maxEnd = e
		}
	}
	if maxEnd > req.Size {
		return maxEnd
	}
	return req.Size
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
			newObj:     truncateV2NewObjFromPartial(10, 5, base[0]),
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
			newObj:     truncateV2NewObjFromPartial(60, 5, base[2]),
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
			eks := append([]proto.ObjExtentKey(nil), base...)
			st, final, enqueue := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
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
			newObj:   truncateV2NewObjFromPartial(10, 5, base[0]),
			toDelete: base[0],
		},
		{name: "target=50", target: 50, toDelete: base[2]},
		{
			name:     "target=65",
			target:   65,
			newObj:   truncateV2NewObjFromPartial(60, 5, base[2]),
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
		st, final, _ := mp.checkTruncateV2Conflict(req, nil, truncInodeSizePreApply(req, nil))
		require.Equal(t, proto.OpOk, st)
		require.Nil(t, final)
	})

	t.Run("logical hole past last end", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 50}, {FileOffset: 50, Size: 50}})
		req := buildTruncateV2ReqFromCompute(ino, 100, eks)
		plan := blobstore.ComputeTruncateReqs(100, blobstore.NewReadOnlyOeks(eks))
		require.True(t, plan.KeepExtent.IsEmpty())
		require.True(t, plan.DiscardFrom.IsEmpty())
		st, final, _ := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
		require.Equal(t, proto.OpOk, st)
		assertObjExtentsEqual(t, eks, final)
	})

	t.Run("partial span target 150", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{FileOffset: 100, Size: 100},
			{FileOffset: 200, Size: 50},
		})
		target := uint64(150)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		plan := blobstore.ComputeTruncateReqs(target, blobstore.NewReadOnlyOeks(eks))
		require.Equal(t, uint64(100), plan.KeepExtent.FileOffset)
		require.Equal(t, uint64(50), plan.KeepExtent.Size)
		require.Equal(t, uint64(100), plan.DiscardFrom.FileOffset)
		require.Equal(t, uint64(100), plan.DiscardFrom.Size)

		st, final, enqueue := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
		require.Equal(t, proto.OpOk, st)
		wantFinal, wantEnqueue := applyTruncateV2Contract(eks, target, req.NewObjExtent, req.ToDelete)
		assertObjExtentsEqual(t, wantFinal, final)
		require.Len(t, enqueue, len(wantEnqueue))
	})

	t.Run("integer boundary target equals oek end", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}})
		target := uint64(100)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		plan := blobstore.ComputeTruncateReqs(target, blobstore.NewReadOnlyOeks(eks))
		require.True(t, plan.KeepExtent.IsEmpty())
		require.Equal(t, uint64(100), plan.DiscardFrom.FileOffset)

		st, final, _ := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
		require.Equal(t, proto.OpOk, st)
		wantFinal, _ := applyTruncateV2Contract(eks, target, req.NewObjExtent, req.ToDelete)
		assertObjExtentsEqual(t, wantFinal, final)
	})

	t.Run("unsorted input sorted by compute", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{
			{FileOffset: 10, Size: 10},
			{FileOffset: 0, Size: 10},
			{FileOffset: 20, Size: 10},
		})
		target := uint64(15)
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		sorted := append([]proto.ObjExtentKey(nil), eks...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].FileOffset < sorted[j].FileOffset })
		st, final, _ := mp.checkTruncateV2Conflict(req, sorted, truncInodeSizePreApply(req, sorted))
		require.Equal(t, proto.OpOk, st)
		wantFinal, _ := applyTruncateV2Contract(sorted, target, req.NewObjExtent, req.ToDelete)
		assertObjExtentsEqual(t, wantFinal, final)
	})

	t.Run("reject New without ToDelete when plan has partial", func(t *testing.T) {
		eks := []proto.ObjExtentKey{{FileOffset: 0, Size: 100}}
		req := &proto.TruncateRequest{Inode: ino, Size: 50, NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 50, Cid: 1}}
		st, _, _ := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
		require.Equal(t, proto.OpConflictExtentsErr, st)
	})

	t.Run("reject ToDelete size mismatch", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}})
		req := &proto.TruncateRequest{Inode: ino, Size: 100, ToDelete: proto.ObjExtentKey{FileOffset: 100, Size: 99, Crc: testTruncateV2BaseCrc(100)}}
		st, _, _ := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
		require.Equal(t, proto.OpConflictExtentsErr, st)
	})

	t.Run("reject ToDelete crc mismatch", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}})
		req := &proto.TruncateRequest{
			Inode: ino, Size: 100,
			ToDelete: proto.ObjExtentKey{FileOffset: 100, Size: 100, Crc: testTruncateV2BaseCrc(100) + 1},
		}
		st, _, _ := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
		require.Equal(t, proto.OpConflictExtentsErr, st)
	})

	t.Run("reject target beyond ToDelete end", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 60, Size: 10}})
		req := &proto.TruncateRequest{Inode: ino, Size: 80, ToDelete: eks[0]}
		st, _, _ := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
		require.Equal(t, proto.OpConflictExtentsErr, st)
	})

	t.Run("idempotent last end le target", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 50}})
		req := &proto.TruncateRequest{Inode: ino, Size: 100, ToDelete: proto.ObjExtentKey{FileOffset: 999, Size: 1}}
		st, final, _ := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
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

	initial := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{
		{FileOffset: 0, Size: 8 * KiB, Cid: 1},
		{FileOffset: 8 * KiB, Size: 8 * KiB, Cid: 2},
	})
	setupInodeWithObjExtents(t, mp, ino, initial, 16*KiB)

	eks := append([]proto.ObjExtentKey(nil), initial...)
	seenDel := make(map[string]struct{})
	var allEnqueued []proto.ObjExtentKey

	round := func(name string, target uint64) {
		t.Helper()
		mp.fsmRaftApplyIndex++

		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		req.Timestamp = int64(target)

		st, wantFinal, wantEnqueue := mp.checkTruncateV2Conflict(req, eks, truncInodeSizePreApply(req, eks))
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
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 100},
			{FileOffset: 100, Size: 100},
			{FileOffset: 200, Size: 50},
		})
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
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}})
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
	const ino = uint64(9014)

	st, final, del := mp.checkTruncateV2Conflict(&proto.TruncateRequest{Inode: ino, Size: 100}, nil, truncInodeSizePreApply(&proto.TruncateRequest{Inode: ino, Size: 100}, nil))
	require.Equal(t, proto.OpOk, st)
	require.Nil(t, final)
	require.Nil(t, del)

	st, final, del = mp.checkTruncateV2Conflict(&proto.TruncateRequest{
		Inode: ino, Size: 100,
		ToDelete: proto.ObjExtentKey{FileOffset: 0, Size: 1},
	}, nil, 50)
	require.Equal(t, proto.OpConflictExtentsErr, st)
	require.Nil(t, final)
	require.Nil(t, del)

	// A1: empty oeks + target already applied (inodeSize==target or target==0); replay tolerates stale ToDelete.
	for name, tc := range map[string]struct {
		req       *proto.TruncateRequest
		inodeSize uint64
	}{
		"truncate_to_0_with_todelete": {
			req: &proto.TruncateRequest{
				Inode: ino, Size: 0,
				ToDelete: proto.ObjExtentKey{FileOffset: 50, Size: 50},
			},
			inodeSize: 0,
		},
		"truncate_to_0_empty_todelete": {
			req:       &proto.TruncateRequest{Inode: ino, Size: 0},
			inodeSize: 0,
		},
		"replay_100_to_10_tail_sweep": {
			req: &proto.TruncateRequest{
				Inode: ino, Size: 10,
				ToDelete: proto.ObjExtentKey{FileOffset: 50, Size: 50},
			},
			inodeSize: 10,
		},
		"replay_10_to_2_logical_hole": {
			req:       &proto.TruncateRequest{Inode: ino, Size: 2},
			inodeSize: 2,
		},
		"replay_10_to_2_with_stale_todelete": {
			req: &proto.TruncateRequest{
				Inode: ino, Size: 2,
				ToDelete: proto.ObjExtentKey{FileOffset: 50, Size: 50},
			},
			inodeSize: 2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			st, final, del = mp.checkTruncateV2Conflict(tc.req, nil, tc.inodeSize)
			require.Equal(t, proto.OpOk, st)
			require.Nil(t, final)
			require.Nil(t, del)
		})
	}
}

// sparse5050Extent is a single tail extent used for tail-sweep truncate chains in UT.
func sparse5050Extent() []proto.ObjExtentKey {
	return ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 50, Size: 50, Cid: 2}})
}

// tripleSparseExtents200: size=200, data spans [50,70),[100,120),[150,180) with holes at front/middle/tail.
func tripleSparseExtents200() []proto.ObjExtentKey {
	return []proto.ObjExtentKey{
		{FileOffset: 50, Size: 20, Cid: 1, Crc: testTruncateV2BaseCrc(50)},
		{FileOffset: 100, Size: 20, Cid: 2, Crc: testTruncateV2BaseCrc(100)},
		{FileOffset: 150, Size: 30, Cid: 3, Crc: testTruncateV2BaseCrc(150)},
	}
}

const tripleSparseInitialSize = uint64(200)

// assertTruncateV2FirstApplyAndIdempotentReplay runs checkTruncateV2Conflict on pre-apply snapshot, then replay on post-apply.
func assertTruncateV2FirstApplyAndIdempotentReplay(
	t *testing.T, mp *metaPartition, ino uint64,
	preEks []proto.ObjExtentKey, preInodeSize, target uint64,
) (postEks []proto.ObjExtentKey, req *proto.TruncateRequest) {
	t.Helper()
	preEks = ensureTruncateV2ExtentSliceCrcs(preEks)
	req = buildTruncateV2ReqFromCompute(ino, target, preEks)
	if !req.ToDelete.IsEmpty() {
		require.NotZero(t, req.ToDelete.Crc, "ToDelete crc required target=%d", target)
	}
	if !req.NewObjExtent.IsEmpty() {
		require.NotZero(t, req.NewObjExtent.Crc, "NewObjExtent crc required target=%d", target)
	}

	pre := append([]proto.ObjExtentKey(nil), preEks...)
	st, final, enqueue := mp.checkTruncateV2Conflict(req, pre, preInodeSize)
	require.Equal(t, proto.OpOk, st, "first apply target=%d", target)

	wantFinal, wantEnqueue := applyTruncateV2Contract(pre, target, req.NewObjExtent, req.ToDelete)
	assertObjExtentsEqual(t, wantFinal, final)
	require.Len(t, enqueue, len(wantEnqueue))

	postEks = append([]proto.ObjExtentKey(nil), wantFinal...)
	assertCheckTruncateV2IdempotentReplay(t, mp, req, postEks, target)
	return postEks, req
}

// TestCheckTruncateV2Conflict_TripleSparse200_Idempotent covers truncate variants on a 3-stripe sparse file and replay idempotency.
func TestCheckTruncateV2Conflict_TripleSparse200_Idempotent(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10023)
	const ino = uint64(9050)
	initial := tripleSparseExtents200()

	cases := []struct {
		name   string
		target uint64
	}{
		{name: "truncate_to_zero", target: 0},
		{name: "front_hole_before_first_extent", target: 40},
		{name: "partial_inside_first_extent", target: 60},
		{name: "boundary_end_of_first_extent", target: 70},
		{name: "middle_hole_between_first_and_second", target: 80},
		{name: "partial_inside_second_extent", target: 110},
		{name: "boundary_end_of_second_extent", target: 120},
		{name: "middle_hole_between_second_and_third", target: 130},
		{name: "partial_inside_third_extent", target: 165},
		{name: "boundary_end_of_all_data", target: 180},
		{name: "tail_logical_hole_past_data", target: 190},
		{name: "noop_at_inode_size", target: 200},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertTruncateV2FirstApplyAndIdempotentReplay(t, mp, ino, initial, tripleSparseInitialSize, tc.target)
		})
	}
}

// TestCheckTruncateV2Conflict_TripleSparse200_ChainedIdempotent chains truncates and replays each step after apply.
func TestCheckTruncateV2Conflict_TripleSparse200_ChainedIdempotent(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10024)
	const ino = uint64(9051)

	eks := tripleSparseExtents200()
	size := tripleSparseInitialSize
	targets := []uint64{130, 60, 0}
	names := []string{"200_to_130", "130_to_60", "60_to_0"}

	for i, target := range targets {
		t.Run(names[i], func(t *testing.T) {
			var req *proto.TruncateRequest
			eks, req = assertTruncateV2FirstApplyAndIdempotentReplay(t, mp, ino, eks, size, target)
			size = target
			_ = req
		})
	}
}

// TestFsmExtentsTruncateV2_TripleSparse200_ChainedIdempotent runs FSM apply+replay on the same chain.
func TestFsmExtentsTruncateV2_TripleSparse200_ChainedIdempotent(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10025)
	const ino = uint64(9052)

	setupInodeWithObjExtents(t, mp, ino, tripleSparseExtents200(), tripleSparseInitialSize)

	applyReplay := func(req *proto.TruncateRequest, label string) {
		t.Helper()
		mp.fsmRaftApplyIndex++
		resp := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp.Status, "apply %s", label)
		mp.fsmRaftApplyIndex++
		resp = runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp.Status, "replay %s", label)
	}

	eks := tripleSparseExtents200()
	for i, target := range []uint64{130, 60, 0} {
		req := buildTruncateV2ReqFromCompute(ino, target, eks)
		applyReplay(req, fmt.Sprintf("step%d_target%d", i, target))
		inoObj := getInodeOrFail(t, mp, ino)
		require.Equal(t, target, inoObj.Size)
		eks = inoObj.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents()
	}
}

// TestCheckTruncateV2Conflict_Shrink100To10To2ToZero: {50,50} tail-sweep chain and per-step idempotent replay.
func TestCheckTruncateV2Conflict_Shrink100To10To2ToZero(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10020)
	const ino = uint64(9040)
	initial := sparse5050Extent()

	req10 := buildTruncateV2ReqFromCompute(ino, 10, initial)
	require.True(t, req10.NewObjExtent.IsEmpty())
	require.False(t, req10.ToDelete.IsEmpty())

	req2 := buildTruncateV2ReqFromCompute(ino, 2, nil)
	require.True(t, req2.NewObjExtent.IsEmpty())
	require.True(t, req2.ToDelete.IsEmpty())

	req0 := &proto.TruncateRequest{Inode: ino, Size: 0}
	req0WithAnchor := &proto.TruncateRequest{
		Inode: ino, Size: 0, ToDelete: initial[0],
	}

	t.Run("first_apply_100_to_10", func(t *testing.T) {
		st, final, _ := mp.checkTruncateV2Conflict(req10, initial, truncInodeSizePreApply(req10, initial))
		require.Equal(t, proto.OpOk, st)
		require.Empty(t, final)
	})

	t.Run("replay_after_each_stage", func(t *testing.T) {
		assertCheckTruncateV2IdempotentReplay(t, mp, req10, nil, 10)
		assertCheckTruncateV2IdempotentReplay(t, mp, req2, nil, 2)
		assertCheckTruncateV2IdempotentReplay(t, mp, req0, nil, 0)
		assertCheckTruncateV2IdempotentReplay(t, mp, req0WithAnchor, nil, 0)
	})

	t.Run("replay_10_to_2_with_stale_todelete_from_step1", func(t *testing.T) {
		req2Stale := &proto.TruncateRequest{
			Inode: ino, Size: 2, ToDelete: req10.ToDelete,
		}
		st, final, del := mp.checkTruncateV2Conflict(req2Stale, nil, 2)
		require.Equal(t, proto.OpOk, st)
		require.Nil(t, final)
		require.Nil(t, del)
	})
}

// TestFsmExtentsTruncateV2_Shrink100To10To2ToZero: FSM chain 100→10→2→0 on {50,50}, replay each step without conflict.
func TestFsmExtentsTruncateV2_Shrink100To10To2ToZero(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10021)
	const ino = uint64(9041)
	initial := sparse5050Extent()

	setupInodeWithObjExtents(t, mp, ino, initial, 100)

	req10 := buildTruncateV2ReqFromCompute(ino, 10, initial)
	req2 := buildTruncateV2ReqFromCompute(ino, 2, nil)
	req0 := &proto.TruncateRequest{Inode: ino, Size: 0}

	apply := func(req *proto.TruncateRequest) {
		t.Helper()
		mp.fsmRaftApplyIndex++
		resp := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp.Status)
	}

	replay := func(req *proto.TruncateRequest, label string) {
		t.Helper()
		mp.fsmRaftApplyIndex++
		resp := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp.Status, label)
	}

	apply(req10)
	inoAfter10 := getInodeOrFail(t, mp, ino)
	require.Equal(t, uint64(10), inoAfter10.Size)
	require.Empty(t, inoAfter10.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
	replay(req10, "replay 100→10")

	apply(req2)
	inoAfter2 := getInodeOrFail(t, mp, ino)
	require.Equal(t, uint64(2), inoAfter2.Size)
	require.Empty(t, inoAfter2.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
	replay(req2, "replay 10→2")

	apply(req0)
	require.Equal(t, uint64(0), getInodeOrFail(t, mp, ino).Size)
	replay(req0, "replay 2→0")

	req0WithAnchor := &proto.TruncateRequest{Inode: ino, Size: 0, ToDelete: initial[0]}
	replay(req0WithAnchor, "replay 2→0 with stale ToDelete")
}

// TestCheckTruncateV2Conflict_MultiStageIdempotentMatrix covers replay after each truncate stage (same request, post-apply snapshot).
func TestCheckTruncateV2Conflict_MultiStageIdempotentMatrix(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10022)
	const ino = uint64(9042)

	type stage struct {
		name      string
		preEks    []proto.ObjExtentKey
		inodeSize uint64
		req       *proto.TruncateRequest
		postEks   []proto.ObjExtentKey
		postSize  uint64
	}

	run := func(stages []stage) {
		t.Helper()
		for _, s := range stages {
			s := s
			t.Run(s.name, func(t *testing.T) {
				st, final, enqueue := mp.checkTruncateV2Conflict(s.req, append([]proto.ObjExtentKey(nil), s.preEks...), s.inodeSize)
				require.Equal(t, proto.OpOk, st, "first apply")
				if len(s.preEks) == 0 {
					require.Empty(t, final)
				}
				assertCheckTruncateV2IdempotentReplay(t, mp, s.req, s.postEks, s.postSize)
				_ = enqueue
			})
		}
	}

	t.Run("sparse5050_100_10_2_0", func(t *testing.T) {
		initial := sparse5050Extent()
		req10 := buildTruncateV2ReqFromCompute(ino, 10, initial)
		req2 := buildTruncateV2ReqFromCompute(ino, 2, nil)
		req0 := &proto.TruncateRequest{Inode: ino, Size: 0}
		run([]stage{
			{name: "100_to_10", preEks: initial, inodeSize: 100, req: req10, postEks: nil, postSize: 10},
			{name: "10_to_2", preEks: nil, inodeSize: 10, req: req2, postEks: nil, postSize: 2},
			{name: "2_to_0", preEks: nil, inodeSize: 2, req: req0, postEks: nil, postSize: 0},
		})
	})

	t.Run("partial_prefix_100_50_20", func(t *testing.T) {
		initial := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100, Cid: 1}})
		req50 := buildTruncateV2ReqFromCompute(ino, 50, initial)
		post50, _ := applyTruncateV2Contract(initial, 50, req50.NewObjExtent, req50.ToDelete)
		req20 := buildTruncateV2ReqFromCompute(ino, 20, post50)
		post20, _ := applyTruncateV2Contract(post50, 20, req20.NewObjExtent, req20.ToDelete)
		run([]stage{
			{name: "100_to_50", preEks: initial, inodeSize: 100, req: req50, postEks: post50, postSize: 50},
			{name: "50_to_20", preEks: post50, inodeSize: 50, req: req20, postEks: post20, postSize: 20},
		})
	})

	t.Run("sparse_example_logical_hole_and_tail", func(t *testing.T) {
		base := sparseTruncateV2ExampleExtents()
		req50 := buildTruncateV2ReqFromCompute(ino, 50, base)
		post50, _ := applyTruncateV2Contract(base, 50, req50.NewObjExtent, req50.ToDelete)
		req100 := &proto.TruncateRequest{Inode: ino, Size: 100}
		run([]stage{
			{name: "70_to_50_tail", preEks: base, inodeSize: 70, req: req50, postEks: post50, postSize: 50},
			{name: "50_to_100_hole", preEks: post50, inodeSize: 50, req: req100, postEks: post50, postSize: 100},
		})
	})

	t.Run("integer_boundary_tail_sweep", func(t *testing.T) {
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}})
		req100 := buildTruncateV2ReqFromCompute(ino, 100, eks)
		post100, _ := applyTruncateV2Contract(eks, 100, req100.NewObjExtent, req100.ToDelete)
		run([]stage{
			{name: "200_to_100", preEks: eks, inodeSize: 200, req: req100, postEks: post100, postSize: 100},
		})
	})
}

// assertCheckTruncateV2IdempotentReplay verifies post-apply state accepts the same TruncateRequest without re-enqueue.
func assertCheckTruncateV2IdempotentReplay(t *testing.T, mp *metaPartition, req *proto.TruncateRequest, postApplyEks []proto.ObjExtentKey, inodeSize uint64) {
	t.Helper()
	st, final, enqueue := mp.checkTruncateV2Conflict(req, postApplyEks, inodeSize)
	require.Equal(t, proto.OpOk, st, "idempotent replay must return OpOk")
	require.Empty(t, enqueue, "idempotent replay must not re-enqueue GC keys")
	if len(postApplyEks) == 0 {
		require.Empty(t, final)
		return
	}
	assertObjExtentsEqual(t, postApplyEks, final)
}

// TestCheckTruncateV2Conflict_OpConflictExtentsErr_AllBranches maps every OpConflictExtentsErr return in checkTruncateV2Conflict.
func TestCheckTruncateV2Conflict_OpConflictExtentsErr_AllBranches(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10017)
	const ino = uint64(9030)

	cases := []struct {
		name       string
		eks        []proto.ObjExtentKey
		inodeSize  uint64 // 0 = use truncInodeSizePreApply
		req        *proto.TruncateRequest
		idempotent bool
	}{
		{
			name:      "empty_oeks_new_obj",
			eks:       nil,
			inodeSize: 100,
			req: &proto.TruncateRequest{
				Inode: ino, Size: 50,
				NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 50, Cid: 1},
			},
			idempotent: false,
		},
		{
			name:      "empty_oeks_todelete_nonzero_size",
			eks:       nil,
			inodeSize: 50,
			req: &proto.TruncateRequest{
				Inode: ino, Size: 100,
				ToDelete: proto.ObjExtentKey{FileOffset: 0, Size: 1},
			},
			idempotent: false,
		},
		{
			name:      "empty_oeks_new_and_todelete",
			eks:       nil,
			inodeSize: 100,
			req: &proto.TruncateRequest{
				Inode: ino, Size: 50,
				NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 50, Cid: 1},
				ToDelete:     proto.ObjExtentKey{FileOffset: 0, Size: 100},
			},
			idempotent: false,
		},
		{
			name: "no_todelete_with_new_partial",
			eks:  []proto.ObjExtentKey{{FileOffset: 0, Size: 100}},
			req: &proto.TruncateRequest{
				Inode: ino, Size: 50,
				NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 50, Cid: 1},
			},
			idempotent: false,
		},
		{
			name:       "no_todelete_shrink_without_plan",
			eks:        []proto.ObjExtentKey{{FileOffset: 0, Size: 100}},
			req:        &proto.TruncateRequest{Inode: ino, Size: 50},
			idempotent: false,
		},
		{
			name: "todelete_size_mismatch",
			eks:  ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}}),
			req: &proto.TruncateRequest{
				Inode: ino, Size: 100,
				ToDelete: proto.ObjExtentKey{FileOffset: 100, Size: 99, Crc: testTruncateV2BaseCrc(100)},
			},
			idempotent: false,
		},
		{
			name: "todelete_crc_mismatch",
			eks:  ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}}),
			req: &proto.TruncateRequest{
				Inode: ino, Size: 100,
				ToDelete: proto.ObjExtentKey{FileOffset: 100, Size: 100, Crc: testTruncateV2BaseCrc(100) + 7},
			},
			idempotent: false,
		},
		{
			name: "target_beyond_todelete_end",
			eks:  ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 60, Size: 10}}),
			req: &proto.TruncateRequest{
				Inode: ino, Size: 80,
				ToDelete: withTruncateV2TestExtentCrc(proto.ObjExtentKey{FileOffset: 60, Size: 10}),
			},
			idempotent: false,
		},
		{
			name: "todelete_anchor_not_found_with_new",
			eks:  []proto.ObjExtentKey{{FileOffset: 0, Size: 50}},
			req: &proto.TruncateRequest{
				Inode: ino, Size: 30,
				NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 30, Cid: 2},
				ToDelete:     proto.ObjExtentKey{FileOffset: 100, Size: 50},
			},
			idempotent: false,
		},
		{
			name: "todelete_anchor_not_found_shrink",
			eks:  []proto.ObjExtentKey{{FileOffset: 0, Size: 100}},
			req: &proto.TruncateRequest{
				Inode: ino, Size: 50,
				ToDelete: proto.ObjExtentKey{FileOffset: 999, Size: 1},
			},
			idempotent: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eks := append([]proto.ObjExtentKey(nil), tc.eks...)
			inodeSize := tc.inodeSize
			if inodeSize == 0 {
				inodeSize = truncInodeSizePreApply(tc.req, eks)
			}
			st, final, enqueue := mp.checkTruncateV2Conflict(tc.req, eks, inodeSize)
			require.Equal(t, proto.OpConflictExtentsErr, st)
			require.Nil(t, final)
			require.Nil(t, enqueue)
			require.False(t, tc.idempotent, "conflict branch must not be marked idempotent")
		})
	}
}

// TestCheckTruncateV2Conflict_IdempotentRetryAndRaftReplay exercises checkTruncateV2Conflict on post-apply inode snapshots.
func TestCheckTruncateV2Conflict_IdempotentRetryAndRaftReplay(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10018)
	const ino = uint64(9031)
	base := sparseTruncateV2ExampleExtents()

	cases := []struct {
		name         string
		preApplyEks  []proto.ObjExtentKey
		postApplyEks []proto.ObjExtentKey
		req          *proto.TruncateRequest
	}{
		{
			name:         "truncate_to_zero",
			preApplyEks:  append([]proto.ObjExtentKey(nil), base...),
			postApplyEks: nil,
			req:          &proto.TruncateRequest{Inode: ino, Size: 0, ToDelete: base[0]},
		},
		{
			name:        "partial_with_new_extent",
			preApplyEks: append([]proto.ObjExtentKey(nil), base...),
			postApplyEks: func() []proto.ObjExtentKey {
				newObj := truncateV2NewObjFromPartial(10, 5, base[0])
				final, _ := applyTruncateV2Contract(base, 15, newObj, base[0])
				return final
			}(),
			req: func() *proto.TruncateRequest {
				newObj := truncateV2NewObjFromPartial(10, 5, base[0])
				return &proto.TruncateRequest{
					Inode: ino, Size: 15,
					NewObjExtent: newObj,
					ToDelete:     base[0],
				}
			}(),
		},
		{
			name:        "partial_replay_stale_todelete_crc",
			preApplyEks: append([]proto.ObjExtentKey(nil), base...),
			postApplyEks: func() []proto.ObjExtentKey {
				newObj := truncateV2NewObjFromPartial(10, 5, base[0])
				final, _ := applyTruncateV2Contract(base, 15, newObj, base[0])
				return final
			}(),
			req: func() *proto.TruncateRequest {
				newObj := truncateV2NewObjFromPartial(10, 5, base[0])
				staleToDelete := base[0] // old full anchor size/crc kept on client retry
				return &proto.TruncateRequest{
					Inode: ino, Size: 15,
					NewObjExtent: newObj,
					ToDelete:     staleToDelete,
				}
			}(),
		},
		{
			name:        "tail_sweep_only",
			preApplyEks: append([]proto.ObjExtentKey(nil), base...),
			postApplyEks: func() []proto.ObjExtentKey {
				final, _ := applyTruncateV2Contract(base, 50, proto.ObjExtentKey{}, base[2])
				return final
			}(),
			req: &proto.TruncateRequest{Inode: ino, Size: 50, ToDelete: base[2]},
		},
		{
			name:         "logical_hole_size_only",
			preApplyEks:  append([]proto.ObjExtentKey(nil), base...),
			postApplyEks: append([]proto.ObjExtentKey(nil), base...),
			req:          &proto.TruncateRequest{Inode: ino, Size: 100},
		},
		{
			name: "integer_boundary_at_oek_end",
			preApplyEks: ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{
				{FileOffset: 0, Size: 100},
				{FileOffset: 100, Size: 100},
			}),
			postApplyEks: func() []proto.ObjExtentKey {
				eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}})
				anchor := withTruncateV2TestExtentCrc(proto.ObjExtentKey{FileOffset: 100, Size: 100})
				final, _ := applyTruncateV2Contract(eks, 100, proto.ObjExtentKey{}, anchor)
				return final
			}(),
			req: func() *proto.TruncateRequest {
				eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 100}, {FileOffset: 100, Size: 100}})
				return buildTruncateV2ReqFromCompute(ino, 100, eks)
			}(),
		},
		{
			name:         "already_deleted_tail_anchor",
			preApplyEks:  ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 50}}),
			postApplyEks: ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{{FileOffset: 0, Size: 50}}),
			req: &proto.TruncateRequest{
				Inode: ino, Size: 100,
				ToDelete: withTruncateV2TestExtentCrc(proto.ObjExtentKey{FileOffset: 999, Size: 1}),
			},
		},
		{
			name:        "new_extent_equals_last_oek",
			preApplyEks: append([]proto.ObjExtentKey(nil), base...),
			postApplyEks: func() []proto.ObjExtentKey {
				newObj := truncateV2NewObjFromPartial(60, 5, base[2])
				final, _ := applyTruncateV2Contract(base, 65, newObj, base[2])
				return final
			}(),
			req: func() *proto.TruncateRequest {
				newObj := truncateV2NewObjFromPartial(60, 5, base[2])
				return &proto.TruncateRequest{
					Inode: ino, Size: 65,
					NewObjExtent: newObj,
					ToDelete:     base[2],
				}
			}(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.req.ToDelete.IsEmpty() {
				require.NotZero(t, tc.req.ToDelete.Crc, "ToDelete crc required")
			}
			if !tc.req.NewObjExtent.IsEmpty() {
				require.NotZero(t, tc.req.NewObjExtent.Crc, "NewObjExtent crc required")
			}
			st, _, _ := mp.checkTruncateV2Conflict(tc.req, tc.preApplyEks, truncInodeSizePreApply(tc.req, tc.preApplyEks))
			require.Equal(t, proto.OpOk, st, "first apply conflict check")
			assertCheckTruncateV2IdempotentReplay(t, mp, tc.req, tc.postApplyEks, tc.req.Size)
		})
	}
}

// TestFsmExtentsTruncateV2_IdempotentDuplicateAndRaftReplay runs FSM twice with the same TruncateRequest.
func TestFsmExtentsTruncateV2_IdempotentDuplicateAndRaftReplay(t *testing.T) {
	mp := newTestMetaPartitionForTruncateV2(t, 10019)

	t.Run("truncate_to_zero_client_retry", func(t *testing.T) {
		const ino = uint64(9032)
		eks := sparseTruncateV2ExampleExtents()
		setupInodeWithObjExtents(t, mp, ino, eks, 70)
		req := buildTruncateV2ReqFromCompute(ino, 0, eks)

		resp1 := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp1.Status)
		afterFirst := getInodeOrFail(t, mp, ino)
		require.Equal(t, uint64(0), afterFirst.Size)
		require.Empty(t, afterFirst.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
		genAfterFirst := afterFirst.Generation

		mp.fsmRaftApplyIndex++
		resp2 := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp2.Status)
		afterSecond := getInodeOrFail(t, mp, ino)
		require.Equal(t, uint64(0), afterSecond.Size)
		require.Empty(t, afterSecond.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
		require.Equal(t, genAfterFirst+1, afterSecond.Generation, "FSM replay still bumps Generation on idempotent check")
	})

	t.Run("partial_truncate_raft_replay", func(t *testing.T) {
		const ino = uint64(9033)
		eks := ensureTruncateV2ExtentSliceCrcs([]proto.ObjExtentKey{
			{FileOffset: 0, Size: 8 * 1024, Cid: 1},
			{FileOffset: 8 * 1024, Size: 8 * 1024, Cid: 2},
		})
		setupInodeWithObjExtents(t, mp, ino, eks, 16*1024)
		req := buildTruncateV2ReqFromCompute(ino, 4*1024, eks)

		resp1 := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp1.Status)
		afterFirst := getInodeOrFail(t, mp, ino)
		wantEks := afterFirst.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents()
		delCountAfterFirst := len(collectAllObjExtentDelOeks(mp.objExtentDelTree))

		mp.fsmRaftApplyIndex++
		resp2 := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp2.Status)
		afterSecond := getInodeOrFail(t, mp, ino)
		assertObjExtentsEqual(t, wantEks, afterSecond.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
		require.Equal(t, afterFirst.Size, afterSecond.Size)
		require.Equal(t, delCountAfterFirst, len(collectAllObjExtentDelOeks(mp.objExtentDelTree)),
			"idempotent replay must not enqueue duplicate GC keys")
	})

	t.Run("logical_hole_duplicate_request", func(t *testing.T) {
		const ino = uint64(9034)
		eks := sparseTruncateV2ExampleExtents()
		setupInodeWithObjExtents(t, mp, ino, eks, 70)
		req := &proto.TruncateRequest{Inode: ino, Size: 100}

		resp1 := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp1.Status)

		mp.fsmRaftApplyIndex++
		resp2 := runFsmExtentsTruncateV2(t, mp, req)
		require.Equal(t, proto.OpOk, resp2.Status)
		afterSecond := getInodeOrFail(t, mp, ino)
		assertObjExtentsEqual(t, eks, afterSecond.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents())
		require.Equal(t, uint64(100), afterSecond.Size)
	})
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

func prepareInodeWithExtentsForFsmInodeTest(t *testing.T, mp *metaPartition, ino uint64, size uint64) {
	t.Helper()
	prepareInodeForFsmInodeTest(t, mp, ino)
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	i, err := mp.inodeTree.CopyGet(NewInode(ino, 0))
	require.NoError(t, err)
	require.NotNil(t, i)
	se := NewSortedExtents()
	se.Append(proto.ExtentKey{FileOffset: 0, Size: uint32(size), ExtentId: 1, PartitionId: 1})
	i.HybridCloudExtents.sortedEks = se
	i.Size = size
	require.NoError(t, mp.inodeTree.Put(handle, i))
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))
}

func inodeSizeFromSnap(t *testing.T, snap Snapshot, ino uint64) (size uint64, found bool) {
	t.Helper()
	err := snap.Range(InodeType, func(item interface{}) bool {
		inode := item.(*Inode)
		if inode.Inode == ino {
			size = inode.Size
			found = true
		}
		return true
	})
	require.NoError(t, err)
	return size, found
}

func testFsmExtentsTruncateCopyGetSnapshot(t *testing.T, mp *metaPartition) {
	const ino = 21001
	const beforeSize = uint64(2048)
	const afterSize = uint64(1024)

	if mp.multiVersionList == nil {
		mp.multiVersionList = &proto.VolVersionInfoList{
			TemporaryVerMap: make(map[uint64]*proto.VolVersionInfo),
		}
	}
	prepareInodeWithExtentsForFsmInodeTest(t, mp, ino, beforeSize)
	snap, err := mp.GetSnapShot()
	require.NoError(t, err)
	defer snap.Close()

	snapSize, found := inodeSizeFromSnap(t, snap, ino)
	require.True(t, found)
	require.Equal(t, beforeSize, snapSize)

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	req := NewInode(ino, 0)
	req.Size = afterSize
	req.ModifyTime = time.Now().Unix()
	resp, err := mp.fsmExtentsTruncate(handle, req)
	require.NoError(t, err)
	require.EqualValues(t, proto.OpOk, resp.Status)
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))

	live, err := mp.inodeTree.Get(NewInode(ino, 0))
	require.NoError(t, err)
	require.NotNil(t, live)
	require.Equal(t, afterSize, live.Size)

	snapSizeAfter, _ := inodeSizeFromSnap(t, snap, ino)
	require.Equal(t, beforeSize, snapSizeAfter,
		"mem snapshot must not observe truncate after CopyGet")
}

func TestFsmExtentsTruncateCopyGetSnapshot(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmExtentsTruncateCopyGetSnapshot(t, mp)
}

func TestFsmExtentsTruncateCopyGetSnapshot_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmExtentsTruncateCopyGetSnapshot(t, mp)
}

func TestFsmExtentsTruncateCopyGetError(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	const ino = 21002
	prepareInodeForFsmInodeTest(t, mp, ino)
	base := mp.inodeTree
	mp.inodeTree = &errInjectInodeTree{
		InodeTree:  base,
		copyGetErr: fmt.Errorf("copyget failed"),
	}
	defer func() { mp.inodeTree = base }()

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	req := NewInode(ino, 0)
	req.Size = 512
	resp, _ := mp.fsmExtentsTruncate(handle, req)
	require.EqualValues(t, proto.OpErr, resp.Status)
}

func testFsmSetInodeQuotaBatchCopyGet(t *testing.T, mp *metaPartition) {
	const ino = 21010
	mp.mqMgr = NewQuotaManager(mp.config.VolName, mp.config.PartitionId)
	prepareInodeForFsmInodeTest(t, mp, ino)

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	resp := mp.fsmSetInodeQuotaBatch(handle, &proto.BatchSetMetaserverQuotaReuqest{
		QuotaId: fsmInodeQuotaID,
		Inodes:  []uint64{ino},
		IsRoot:  true,
	})
	require.EqualValues(t, proto.OpOk, resp.InodeRes[ino])
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))

	extend, err := mp.extendTree.Get(NewExtendWithQuota(ino))
	require.NoError(t, err)
	require.NotNil(t, extend)
	require.NotEmpty(t, extend.Quota)

	var quotaMap map[uint32]*proto.MetaQuotaInfo
	require.NoError(t, json.Unmarshal(extend.Quota, &quotaMap))
	require.NotNil(t, quotaMap[fsmInodeQuotaID])
}

func TestFsmSetInodeQuotaBatchCopyGet(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmSetInodeQuotaBatchCopyGet(t, mp)
}

func TestFsmSetInodeQuotaBatchCopyGet_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmSetInodeQuotaBatchCopyGet(t, mp)
}

func testFsmDeleteInodeQuotaBatchCopyGet(t *testing.T, mp *metaPartition) {
	const ino = 21011
	mp.mqMgr = NewQuotaManager(mp.config.VolName, mp.config.PartitionId)
	prepareInodeForFsmInodeTest(t, mp, ino)

	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	setResp := mp.fsmSetInodeQuotaBatch(handle, &proto.BatchSetMetaserverQuotaReuqest{
		QuotaId: fsmInodeQuotaID,
		Inodes:  []uint64{ino},
		IsRoot:  true,
	})
	require.EqualValues(t, proto.OpOk, setResp.InodeRes[ino])
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))

	handle, err = mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	_ = mp.fsmDeleteInodeQuotaBatch(handle, &proto.BatchDeleteMetaserverQuotaReuqest{
		QuotaId: fsmInodeQuotaID,
		Inodes:  []uint64{ino},
	})
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))

	extend, err := mp.extendTree.Get(NewExtendWithQuota(ino))
	require.NoError(t, err)
	if extend != nil {
		require.Nil(t, extend.Quota)
	}
}

func TestFsmDeleteInodeQuotaBatchCopyGet(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeMem)
	testFsmDeleteInodeQuotaBatchCopyGet(t, mp)
}

func TestFsmDeleteInodeQuotaBatchCopyGet_Rocksdb(t *testing.T) {
	mp := newMpForFsmInodeTest(t, proto.StoreModeRocksDb)
	testFsmDeleteInodeQuotaBatchCopyGet(t, mp)
}
