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
			Inode:         ino,
			Size:          10,
			NewObjExtents: []proto.ObjExtentKey{{FileOffset: 0, Size: 10}},
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
