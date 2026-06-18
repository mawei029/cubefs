package objectnode

import (
	"testing"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/blobstore"
	"github.com/cubefs/cubefs/util/config"
	"github.com/stretchr/testify/require"
)

func TestNewEbsClientUsesDefaultTimeout(t *testing.T) {
	gotTimeout := -1
	patches := gomonkey.ApplyFunc(blobstore.NewEbsClient, func(_ access.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotTimeout = maxTimeoutSec
		return &blobstore.BlobStoreClient{}, nil
	})
	defer patches.Reset()

	cfg := config.NewConfig()
	cfg.SetString("logDir", t.TempDir())
	err := newEbsClient(&proto.ClusterInfo{EbsAddr: "127.0.0.1:1"}, cfg)
	require.NoError(t, err)
	require.Equal(t, 0, gotTimeout)
}
