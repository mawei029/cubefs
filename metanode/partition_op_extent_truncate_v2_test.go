package metanode

import (
	"errors"
	"reflect"
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/proto"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
)

func TestExtentsTruncateV2_Errors(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := mockPartitionRaftForFsmInodeTest(t, ctrl, proto.StoreModeMem)

	t.Run("inode not exist", func(t *testing.T) {
		p := &Packet{}
		err := mp.extentsTruncateV2(&ExtentsTruncateReq{Inode: 99999, Size: 10}, p)
		require.Error(t, err)
		require.Equal(t, proto.OpErr, p.ResultCode)
	})

	t.Run("storage class not blobstore", func(t *testing.T) {
		const ino = 20001
		prepareInodeForFsmInodeTest(t, mp, ino) // replica inode
		p := &Packet{}
		err := mp.extentsTruncateV2(&ExtentsTruncateReq{
			Inode:        ino,
			Size:         10,
			NewObjExtent: proto.ObjExtentKey{FileOffset: 0, Size: 10},
		}, p)
		require.Error(t, err)
		require.Equal(t, proto.OpErr, p.ResultCode)
	})
}

func TestExtentsTruncate_TruncateV2AndNonHot(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := mockPartitionRaftForFsmInodeTest(t, ctrl, proto.StoreModeMem)

	t.Run("truncateV2 branch", func(t *testing.T) {
		p := &Packet{}
		err := mp.ExtentsTruncate(&ExtentsTruncateReq{Inode: 1, TruncateV2: true}, p, "")
		require.Error(t, err)
		require.Equal(t, proto.OpErr, p.ResultCode)
	})

	t.Run("non hot volume rejected", func(t *testing.T) {
		mp.volType = proto.VolumeTypeCold
		p := &Packet{}
		err := mp.ExtentsTruncate(&ExtentsTruncateReq{Inode: 1, Size: 1}, p, "")
		require.Error(t, err)
		require.Equal(t, proto.OpErr, p.ResultCode)
	})
}

func TestExtentsTruncateV2_CopyGetError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := mockPartitionRaftForFsmInodeTest(t, ctrl, proto.StoreModeMem)

	p := &Packet{}
	patches := gomonkey.NewPatches()
	defer patches.Reset()
	patches.ApplyMethod(reflect.TypeOf(mp.inodeTree), "CopyGet",
		func(_ *InodeBTree, _ *Inode) (*Inode, error) {
			return nil, errors.New("copyget failed")
		})

	err := mp.extentsTruncateV2(&ExtentsTruncateReq{Inode: 1, Size: 1}, p)
	require.Error(t, err)
	require.Equal(t, proto.OpErr, p.ResultCode)
}

func prepareBlobStoreInodeWithObjExtents(t *testing.T, mp *metaPartition, ino uint64, eks []proto.ObjExtentKey, size uint64) {
	t.Helper()
	handle, err := mp.inodeTree.CreateBatchWriteHandle()
	require.NoError(t, err)
	inode := NewInodeTest(ino, FileModeType)
	inode.StorageClass = proto.StorageClass_BlobStore
	inode.HybridCloudExtents.sortedEks = NewSortedObjExtentsFromObjEks(eks)
	inode.Size = size
	mp.inodeTree.ReplaceOrInsert(handle, inode, true)
	require.NoError(t, mp.inodeTree.CommitAndReleaseBatchWriteHandle(handle, false))
}

func TestExtentsTruncateV2_ValidateReject(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := mockPartitionRaftForFsmInodeTest(t, ctrl, proto.StoreModeMem)
	const ino = 30001
	prepareBlobStoreInodeWithObjExtents(t, mp, ino, []proto.ObjExtentKey{
		{FileOffset: 0, Size: 100},
		{FileOffset: 100, Size: 100},
	}, 200)

	p := &Packet{}
	// Shrink to 100 requires ToDelete anchor; missing it should fail validation before FSM.
	err := mp.extentsTruncateV2(&ExtentsTruncateReq{Inode: ino, Size: 100}, p)
	require.NoError(t, err)
	require.Equal(t, proto.OpConflictExtentsErr, p.ResultCode)
}

func TestExtentsTruncateV2_SuccessIntegerBoundary(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mp := mockPartitionRaftForFsmInodeTest(t, ctrl, proto.StoreModeMem)
	mp.fsmRaftApplyIndex = 99
	const ino = 30002
	prepareBlobStoreInodeWithObjExtents(t, mp, ino, []proto.ObjExtentKey{
		{FileOffset: 0, Size: 100},
		{FileOffset: 100, Size: 100},
	}, 200)

	p := &Packet{}
	err := mp.extentsTruncateV2(&ExtentsTruncateReq{
		Inode:      ino,
		Size:       100,
		ToDelete:   proto.ObjExtentKey{FileOffset: 100, Size: 100},
		TruncateV2: true,
		Timestamp:  1,
	}, p)
	require.NoError(t, err)
	require.Equal(t, proto.OpOk, p.ResultCode)

	updated, err := mp.inodeTree.CopyGet(&Inode{Inode: ino})
	require.NoError(t, err)
	require.Equal(t, uint64(100), updated.Size)
	exts := updated.HybridCloudExtents.sortedEks.(*SortedObjExtents).CopyExtents()
	require.Len(t, exts, 1)
}
