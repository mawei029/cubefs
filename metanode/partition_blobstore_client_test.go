package metanode

import (
	"os"
	"reflect"
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

func TestNewBlobStoreClientWrapperPassesAccessConfig(t *testing.T) {
	oldClusterInfo := gClusterInfo
	t.Cleanup(func() { gClusterInfo = oldClusterInfo })
	gClusterInfo = &proto.ClusterInfo{EbsAddr: "127.0.0.1:1"}

	const objBlockSize = 33554432
	cfg := access.Config{
		ConnMode: access.NoLimitConnMode,
		Consul: access.ConsulConfig{
			Address: gClusterInfo.EbsAddr,
		},
		MaxSizePutOnce:     objBlockSize,
		ServiceIntervalS:   60,
		FailRetryIntervalS: -1,
		MaxHostRetry:       6,
		BodyBaseTimeoutMs:  6000,
		BodyBandwidthMBPs:  2,
	}

	var gotAccessCfg access.Config
	patches := gomonkey.ApplyFunc(blobstore.NewEbsClient, func(c access.Config, maxTimeoutSec int) (*blobstore.BlobStoreClient, error) {
		gotAccessCfg = c
		return &blobstore.BlobStoreClient{}, nil
	})
	defer patches.Reset()

	wrapper, err := NewBlobStoreClientWrapper(cfg)
	require.NoError(t, err)
	require.NotNil(t, wrapper)
	require.Equal(t, access.NoLimitConnMode, gotAccessCfg.ConnMode)
	require.Equal(t, gClusterInfo.EbsAddr, gotAccessCfg.Consul.Address)
	require.Equal(t, int64(objBlockSize), gotAccessCfg.MaxSizePutOnce)
	require.Equal(t, 60, gotAccessCfg.ServiceIntervalS)
	require.Equal(t, -1, gotAccessCfg.FailRetryIntervalS)
	require.Equal(t, 6, gotAccessCfg.MaxHostRetry)
	require.Equal(t, int64(6000), gotAccessCfg.BodyBaseTimeoutMs)
	require.Equal(t, float64(2), gotAccessCfg.BodyBandwidthMBPs)
}

func TestOnStartPassesBlobAccessConfig(t *testing.T) {
	oldClusterInfo := gClusterInfo
	t.Cleanup(func() { gClusterInfo = oldClusterInfo })
	gClusterInfo = &proto.ClusterInfo{EbsAddr: "127.0.0.1:1"}

	const objBlockSize = 33554432
	rootDir, err := os.MkdirTemp("", "onstart_blob_access_cfg")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(rootDir) })

	mp := newTestMetaPartition(t, rootDir, nil)
	mp.vol.info = &proto.SimpleVolView{
		VolType:             proto.VolumeTypeCold,
		VolStorageClass:     proto.StorageClass_BlobStore,
		AllowedStorageClass: []uint32{uint32(proto.StorageClass_BlobStore)},
		ObjBlockSize:        objBlockSize,
	}

	var gotAccessCfg access.Config
	patches := gomonkey.NewPatches()
	t.Cleanup(func() { patches.Reset() })

	mpType := reflect.TypeOf(mp)
	patches.ApplyPrivateMethod(mpType, "load", func(_ *metaPartition, _ bool) error { return nil })
	patches.ApplyPrivateMethod(mpType, "startFreeList", func(_ *metaPartition) error { return nil })
	patches.ApplyPrivateMethod(mpType, "startRaft", func(_ *metaPartition, _ bool) error { return nil })
	patches.ApplyPrivateMethod(reflect.TypeOf(mp.manager), "forceUpdateVolumeView",
		func(_ *metadataManager, _ MetaPartition) error { return nil })
	patches.ApplyFunc(blobstore.NewEbsClient, func(cfg access.Config, _ int) (*blobstore.BlobStoreClient, error) {
		gotAccessCfg = cfg
		return &blobstore.BlobStoreClient{}, nil
	})

	err = mp.onStart(false)
	require.NoError(t, err)
	require.NotNil(t, mp.blobClientWrapper)
	require.Equal(t, access.NoLimitConnMode, gotAccessCfg.ConnMode)
	require.Equal(t, gClusterInfo.EbsAddr, gotAccessCfg.Consul.Address)
	require.Equal(t, int64(objBlockSize), gotAccessCfg.MaxSizePutOnce)
	require.Equal(t, 60, gotAccessCfg.ServiceIntervalS)
	require.Equal(t, -1, gotAccessCfg.FailRetryIntervalS)
	require.Equal(t, 6, gotAccessCfg.MaxHostRetry)
	require.Equal(t, int64(6000), gotAccessCfg.BodyBaseTimeoutMs)
	require.Equal(t, float64(2), gotAccessCfg.BodyBandwidthMBPs)
}
