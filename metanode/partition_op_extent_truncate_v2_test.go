package metanode

import (
	"testing"

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
