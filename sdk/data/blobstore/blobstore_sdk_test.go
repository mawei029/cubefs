// Copyright 2026 The CubeFS Authors.
package blobstore

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"testing"
	"time"

	"github.com/agiledragon/gomonkey/v2"
	"github.com/cubefs/cubefs/blobstore/api/access"
	"github.com/cubefs/cubefs/blobstore/api/shardnode"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/sdk"
	blog "github.com/cubefs/cubefs/blobstore/util/log"
	cproto "github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/log"
	"github.com/stretchr/testify/require"
)

type stubSdkAccessClient struct{}

func (stubSdkAccessClient) Put(context.Context, *access.PutArgs) (proto.Location, access.HashSumMap, error) {
	return proto.Location{}, nil, nil
}

func (stubSdkAccessClient) Get(context.Context, *access.GetArgs) (io.ReadCloser, error) {
	return nil, nil
}

func (stubSdkAccessClient) Delete(context.Context, *access.DeleteArgs) ([]proto.Location, error) {
	return nil, nil
}

func (stubSdkAccessClient) ListBlob(context.Context, *access.ListBlobArgs) (shardnode.ListBlobRet, error) {
	return shardnode.ListBlobRet{}, nil
}

func (stubSdkAccessClient) GetBlob(context.Context, *access.GetBlobArgs) (io.ReadCloser, error) {
	return nil, nil
}
func (stubSdkAccessClient) DeleteBlob(context.Context, *access.DelBlobArgs) error { return nil }
func (stubSdkAccessClient) PutBlob(context.Context, *access.PutBlobArgs) (proto.ClusterID, access.HashSumMap, error) {
	return 0, nil, nil
}

func validSdkCfg() *sdk.Config {
	return &sdk.Config{}
}

func minimalEbsConfig() cproto.EbsClientConfig {
	return cproto.EbsClientConfig{
		Idc:         "z0",
		RegionMagic: "nvme-cubefs",
		Clusters: []cproto.EbsSdkCluster{{
			ClusterID: 50,
			Hosts:     []string{"http://localhost:9998"},
		}},
	}
}

func TestBuildSdkConfigMinimal(t *testing.T) {
	maxBlob := uint32(16777216)
	ec := cproto.EbsClientConfig{
		Idc:         "z0",
		RegionMagic: "nvme-cubefs",
		MaxBlobSize: &maxBlob,
		Clusters: []cproto.EbsSdkCluster{{
			ClusterID: 50,
			Hosts:     []string{"http://localhost:9998", "http://localhost2:9998", "http://localhost3:9998"},
		}},
	}
	cfg, err := BuildSdkConfig(ec, "consul_address", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "z0", cfg.IDC)
	require.Equal(t, "nvme-cubefs", cfg.ClusterConfig.Region)
	require.Equal(t, "nvme-cubefs", cfg.ClusterConfig.RegionMagic)
	require.Equal(t, "consul_address", cfg.ClusterConfig.ConsulAgentAddr)
	require.Equal(t, uint32(16777216), cfg.MaxBlobSize)
	require.Len(t, cfg.ClusterConfig.Clusters, 1)
	require.Equal(t, uint32(50), uint32(cfg.ClusterConfig.Clusters[0].ClusterID))
	require.NotNil(t, cfg.Logger)
}

func TestBuildSdkConfigDefaultMaxBlobSize(t *testing.T) {
	ec := minimalEbsConfig()
	cfg, err := BuildSdkConfig(ec, "xxx", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, defaultMaxBlobSize, cfg.MaxBlobSize)

	zero := uint32(0)
	ec.MaxBlobSize = &zero
	cfg, err = BuildSdkConfig(ec, "xxx", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, defaultMaxBlobSize, cfg.MaxBlobSize)
}

func TestBuildSdkConfigRegionFallback(t *testing.T) {
	ec := cproto.EbsClientConfig{
		Idc:      "z0",
		Region:   "from-region",
		Clusters: []cproto.EbsSdkCluster{{ClusterID: 1, Hosts: []string{"http://localhost:9998"}}},
	}
	cfg, err := BuildSdkConfig(ec, "xxx", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "from-region", cfg.ClusterConfig.RegionMagic)
	require.Equal(t, "from-region", cfg.ClusterConfig.Region)

	ec.RegionMagic = "magic"
	ec.Region = "explicit-region"
	cfg, err = BuildSdkConfig(ec, "consul_address", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "magic", cfg.ClusterConfig.RegionMagic)
	require.Equal(t, "explicit-region", cfg.ClusterConfig.Region)
}

func TestBuildSdkConfigSkipsInvalidClusters(t *testing.T) {
	ec := cproto.EbsClientConfig{
		Idc:         "z0",
		RegionMagic: "rm",
		Clusters: []cproto.EbsSdkCluster{
			{ClusterID: 0, Hosts: []string{"http://bad"}},
			{ClusterID: 2, Hosts: nil},
			{ClusterID: 50, Hosts: []string{"http://localhost:9998"}},
		},
	}
	cfg, err := BuildSdkConfig(ec, "consul_address", t.TempDir())
	require.NoError(t, err)
	require.Len(t, cfg.ClusterConfig.Clusters, 1)
	require.Equal(t, uint32(50), uint32(cfg.ClusterConfig.Clusters[0].ClusterID))
}

func TestBuildSdkConfigLogLevelFollowsFuseUnlessEbsOverride(t *testing.T) {
	ec := minimalEbsConfig()
	cfg, err := BuildSdkConfig(ec, "consul_address", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, log.GetBlobLogLevel(), cfg.LogConf.Level)

	lvl := int(blog.Lerror)
	ec.LogLevel = &lvl
	cfg, err = BuildSdkConfig(ec, "consul_address", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, blog.Lerror, cfg.LogConf.Level)
}

func TestBuildSdkConfigRequiresFields(t *testing.T) {
	_, err := BuildSdkConfig(cproto.EbsClientConfig{}, "consul_address", t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "idc")

	_, err = BuildSdkConfig(cproto.EbsClientConfig{Idc: "z0"}, "", t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "region_magic")

	_, err = BuildSdkConfig(cproto.EbsClientConfig{Idc: "z0", RegionMagic: "rm"}, "", t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "need consul_address or clusters")

	_, err = BuildSdkConfig(cproto.EbsClientConfig{
		Idc:         "z0",
		RegionMagic: "rm",
		Clusters:    []cproto.EbsSdkCluster{},
	}, "", t.TempDir())
	require.Error(t, err)
	require.Contains(t, err.Error(), "need consul_address or clusters")
}

func TestBuildSdkConfigConsulAddress(t *testing.T) {
	ec := cproto.EbsClientConfig{
		Idc:           "z0",
		Region:        "nvme-cubefs",
		RegionMagic:   "nvme-cubefs",
		ConsulAddress: "http://localhost:8500/",
	}
	cfg, err := BuildSdkConfig(ec, "ignored-pool-ec:8500", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "http://localhost:8500/", cfg.ClusterConfig.ConsulAgentAddr)
	require.Empty(t, cfg.ClusterConfig.Clusters)
	require.Equal(t, "nvme-cubefs", cfg.ClusterConfig.Region)

	ec.ConsulAddress = "localhost:8500"
	cfg, err = BuildSdkConfig(ec, "pool-ec:8500", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "localhost:8500", cfg.ClusterConfig.ConsulAgentAddr)
}

func TestBuildSdkConfigConsulFromPoolECAddr(t *testing.T) {
	ec := minimalEbsConfig()
	ec.ConsulAddress = ""
	cfg, err := BuildSdkConfig(ec, "consul_address:8500", t.TempDir())
	require.NoError(t, err)
	require.Equal(t, "consul_address:8500", cfg.ClusterConfig.ConsulAgentAddr)
}

func TestBuildSdkConfigMkdirFail(t *testing.T) {
	fileAsLogPath := path.Join(t.TempDir(), "not-a-dir")
	require.NoError(t, os.WriteFile(fileAsLogPath, []byte("x"), 0o644))

	_, err := BuildSdkConfig(minimalEbsConfig(), "localhost:8500", fileAsLogPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "mkdir ebs_sdk log dir failed")
}

func TestBuildSdkConfigOpenFileFail(t *testing.T) {
	logPath := t.TempDir()
	clientDir := path.Join(logPath, "client")
	require.NoError(t, os.MkdirAll(clientDir, 0o755))
	// Make the log path a directory so OpenFile fails even when CI runs as root
	// (chmod 000 on the parent dir is ignored by root).
	require.NoError(t, os.Mkdir(path.Join(clientDir, "ebs_sdk.log"), 0o755))

	_, err := BuildSdkConfig(minimalEbsConfig(), "localhost:8500", logPath)
	require.Error(t, err)
	require.Contains(t, err.Error(), "open ebs_sdk log failed")
}

func TestNewEbsClientSdkNilConfig(t *testing.T) {
	_, err := NewEbsClientSdk(nil, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "nil sdk config")
}

func TestNewEbsClientSdkSuccessAndTimeoutClamp(t *testing.T) {
	patches := gomonkey.ApplyFunc(sdk.New, func(*sdk.Config) (access.Client, error) {
		return stubSdkAccessClient{}, nil
	})
	defer patches.Reset()

	cfg := validSdkCfg()
	cli, err := NewEbsClientSdk(cfg, 0)
	require.NoError(t, err)
	require.NotNil(t, cli)
	require.Equal(t, EbsMaxTimeout, cli.maxTimeoutSec)

	cli, err = NewEbsClientSdk(cfg, 600)
	require.NoError(t, err)
	require.Equal(t, EbsMaxTimeout, cli.maxTimeoutSec)

	cli, err = NewEbsClientSdk(cfg, 120)
	require.NoError(t, err)
	require.Equal(t, 120*time.Second, cli.maxTimeoutSec)
}

func TestNewEbsClientSdkSdkNewError(t *testing.T) {
	patches := gomonkey.ApplyFunc(sdk.New, func(*sdk.Config) (access.Client, error) {
		return nil, fmt.Errorf("sdk new boom")
	})
	defer patches.Reset()

	_, err := NewEbsClientSdk(validSdkCfg(), 30)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sdk new boom")
}
