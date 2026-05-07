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
	"fmt"
	"os"
	"testing"
	"time"

	raftstoremock "github.com/cubefs/cubefs/metanode/mocktest/raftstore"
	"github.com/cubefs/cubefs/proto"
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

// TestFsmExtentsTruncateV2 verifies EC TruncateV2 FSM: update inode.Size and ObjExtents,
// and enqueue ToDeletes into objExtentDelTree.
func TestFsmExtentsTruncateV2(t *testing.T) {
	mpC := &MetaPartitionConfig{
		PartitionId:   10001,
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
	mp.fsmRaftApplyIndex = 12345

	const inoId = 9001
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	fsmIno := NewInode(inoId, 0)
	fsmIno.StorageClass = proto.StorageClass_BlobStore
	fsmIno.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks([]proto.ObjExtentKey{
		{FileOffset: 0, Size: 100},
		{FileOffset: 100, Size: 100},
	})
	fsmIno.Size = 200
	mp.inodeTree.ReplaceOrInsert(handle, fsmIno, true)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false)
	require.NoError(t, err)

	// TruncateV2: truncate to 150, new extents become [0,100) + [100,150) (the latter comes from client-side EBS truncation).
	newObjExtents := []proto.ObjExtentKey{
		{FileOffset: 0, Size: 100},
		{FileOffset: 100, Size: 50},
	}
	toDeletes := []proto.ObjExtentKey{
		{FileOffset: 100, Size: 100},
	}
	truncReq := &proto.TruncateRequest{
		Inode:         inoId,
		Size:          150,
		NewObjExtents: newObjExtents,
		ToDeletes:     toDeletes,
	}
	handle2, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	resp, err := mp.fsmExtentsTruncateV2(handle2, truncReq)
	require.NoError(t, err)
	require.Equal(t, proto.OpOk, resp.Status)
	err = mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle2, false)
	require.NoError(t, err)

	updatedIno, err := mp.inodeTree.CopyGet(&Inode{Inode: inoId})
	require.NoError(t, err)
	require.NotNil(t, updatedIno)
	require.Equal(t, uint64(150), updatedIno.Size)
	sortedEks := updatedIno.HybridCloudExtents.sortedEks.(*SortedObjExtents)
	extents := sortedEks.CopyExtents()
	require.Len(t, extents, 2)
	require.Equal(t, uint64(0), extents[0].FileOffset)
	require.Equal(t, uint64(100), extents[0].Size)
	require.Equal(t, uint64(100), extents[1].FileOffset)
	require.Equal(t, uint64(50), extents[1].Size)
	require.NotNil(t, mp.objExtentDelTree)
	require.Equal(t, 1, mp.objExtentDelTree.Len())
	items := mp.objExtentDelTree.PeekFirstN(1)
	require.Len(t, items, 1)
	require.Equal(t, int64(mp.fsmRaftApplyIndex), items[0].TsMs)
	require.Equal(t, mp.fsmRaftApplyIndex<<20, items[0].Uniq)
	require.True(t, items[0].Oek.IsEquals(&toDeletes[0]))
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
			Inode:         7777,
			Size:          10,
			NewObjExtents: []proto.ObjExtentKey{{FileOffset: 0, Size: 10}},
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
			Inode:         8888,
			Size:          10,
			NewObjExtents: []proto.ObjExtentKey{{FileOffset: 0, Size: 10}},
		})
		require.NoError(t, err)
		require.Equal(t, proto.OpArgMismatchErr, resp.Status)
		require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle2, false))
	})
}
