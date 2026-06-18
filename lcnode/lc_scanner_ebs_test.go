package lcnode

import (
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/sdk/data/stream"
	masterSDK "github.com/cubefs/cubefs/sdk/master"
	"github.com/cubefs/cubefs/sdk/meta"
	"github.com/stretchr/testify/require"
)

func TestNewS3ScannerNewEbsClientDefaultTimeout(t *testing.T) {
	patches := gomonkey.NewPatches()
	defer patches.Reset()

	gotTimeout := -1
	patches.ApplyFunc(blobstore.NewEbsClient, func(_ access.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotTimeout = maxTimeoutSec
		return &blobstore.BlobStoreClient{}, nil
	})
	patches.ApplyFunc(meta.NewMetaWrapper, func(_ *meta.MetaConfig) (*meta.MetaWrapper, error) {
		return &meta.MetaWrapper{}, nil
	})
	patches.ApplyFunc(stream.NewExtentClient, func(_ *stream.ExtentConfig) (*stream.ExtentClient, error) {
		return &stream.ExtentClient{}, nil
	})

	mc := masterSDK.NewMasterClient([]string{"127.0.0.1:1"}, false)
	admin := mc.AdminAPI()
	patches.ApplyMethod(admin, "GetVolumeSimpleInfo",
		func(_ *masterSDK.AdminAPI, _ string) (*proto.SimpleVolView, error) {
			return &proto.SimpleVolView{
				VolStorageClass:     proto.StorageClass_Replica_HDD,
				AllowedStorageClass: []uint32{proto.StorageClass_Replica_HDD},
			}, nil
		})

	lc := &LcNode{
		masters:   []string{"127.0.0.1:1"},
		mc:        mc,
		ebsAddr:   "127.0.0.1:1",
		logDir:    t.TempDir(),
		ioLimiter: NewLcNodeIoLimiter(0, 0),
	}
	adminTask := &proto.AdminTask{
		Request: &proto.LcNodeRuleTaskRequest{
			Task: &proto.RuleTask{
				Id:      "task-1",
				VolName: "test-vol",
				Rule: &proto.Rule{
					ID: "rule-1",
					Transitions: []*proto.Transition{
						{StorageClass: proto.OpTypeStorageClassEBS},
					},
				},
			},
		},
	}

	scanner, err := NewS3Scanner(adminTask, lc)
	require.NoError(t, err)
	require.NotNil(t, scanner)
	require.Equal(t, 0, gotTimeout)
	require.NotNil(t, scanner.transitionMgr.ebsClient)
}
