package metanode

import (
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/stretchr/testify/require"
)

func TestBlobStoreClientWrapperGetBlobStoreClientDefaultTimeout(t *testing.T) {
	oldClusterInfo := gClusterInfo
	t.Cleanup(func() { gClusterInfo = oldClusterInfo })
	gClusterInfo = &proto.ClusterInfo{EbsAddr: "127.0.0.1:1"}

	ew := &BlobStoreClientWrapper{
		cfg: &access.Config{
			Consul: access.ConsulConfig{Address: "127.0.0.1:1"},
		},
		lastTryCreateTime: time.Now().Unix() - DefaultCreateBlobClientIntervalSec - 1,
	}

	gotTimeout := -1
	patches := gomonkey.ApplyFunc(blobstore.NewEbsClient, func(_ access.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotTimeout = maxTimeoutSec
		return &blobstore.BlobStoreClient{}, nil
	})
	defer patches.Reset()

	cli, create, err := ew.getBlobStoreClient()
	require.NoError(t, err)
	require.True(t, create)
	require.NotNil(t, cli)
	require.Equal(t, 0, gotTimeout)
	require.Same(t, cli, ew.blobClient)
}
