// Copyright 2026 The CubeFS Authors.
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

package blobstore

import (
	"fmt"
	"os"
	"path"
	"time"

	"github.com/cubefs/cubefs/blobstore/access/controller"
	"github.com/cubefs/cubefs/blobstore/access/stream"
	"github.com/cubefs/cubefs/blobstore/cmd"
	ebsproto "github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/sdk"
	blog "github.com/cubefs/cubefs/blobstore/util/log"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
	"github.com/cubefs/cubefs/util/log"
)

const defaultMaxBlobSize = uint32(16 << 20) // 16777216, align with access max_blob_size

// NewEbsClientSdk creates a BlobStoreClient backed by blobstore/sdk (in-process stream).
// cfs-client fuse calls this once per EC pool.
func NewEbsClientSdk(cfg *sdk.Config, maxTimeoutSec int) (*BlobStoreClient, error) {
	if cfg == nil {
		return nil, errors.New("NewEbsClientSdk: nil sdk config")
	}
	if maxTimeoutSec <= 0 || maxTimeoutSec >= 600 {
		maxTimeoutSec = int(EbsMaxTimeout.Seconds())
	}

	log.LogWarnf("NewEbsClientSdk start: idc(%v) magic(%v) consul(%v) clusters(%d) maxBlob(%v) maxTimeoutSec(%v)",
		cfg.IDC, cfg.ClusterConfig.RegionMagic, cfg.ClusterConfig.ConsulAgentAddr, len(cfg.ClusterConfig.Clusters), cfg.MaxBlobSize, maxTimeoutSec)

	cli, err := sdk.New(cfg)
	if err != nil {
		log.LogErrorf("NewEbsClientSdk FAILED: idc(%v) magic(%v) err(%v)", cfg.IDC, cfg.ClusterConfig.RegionMagic, err)
		return nil, err
	}

	log.LogWarnf("NewEbsClientSdk OK: idc(%v) magic(%v) consul(%v) clusters(%d) maxBlob(%v)",
		cfg.IDC, cfg.ClusterConfig.RegionMagic, cfg.ClusterConfig.ConsulAgentAddr, len(cfg.ClusterConfig.Clusters), cfg.MaxBlobSize)
	return &BlobStoreClient{
		client:        cli,
		maxTimeoutSec: time.Duration(maxTimeoutSec) * time.Second,
	}, nil
}

// BuildSdkConfig maps fuse --ebsConfig into blobstore/sdk.Config.
// Required --ebsConfig (minimal): idc, region_magic (or region), max_blob_size (optional),
// and either consul_address or static clusters.
//  1. {"idc":"z0","region":"nvme-cubefs","region_magic":"nvme-cubefs","consul_address":"idc_consul_addr:8500"}
//  2. {"idc":"z0","region":"nvme-cubefs","region_magic":"nvme-cubefs","clusters":[{"cluster_id":50,"hosts":["http://cm1:9998","http://cm2:9998"]}]}
//
// consul_address is ClusterMgr consul (KV ebs/{region}/clusters/).
// If set, sdk NewClusterController uses loadWithConsul; static clusters are optional.
// poolECAddr(volume pool Access consul == CM consul). region falls back to region_magic.
func BuildSdkConfig(c proto.EbsClientConfig, poolECAddr, logPath string) (*sdk.Config, error) {
	if c.Idc == "" {
		return nil, fmt.Errorf("ebsConfig.idc empty")
	}
	regionMagic := c.RegionMagic
	if regionMagic == "" {
		regionMagic = c.Region
	}
	if regionMagic == "" {
		return nil, fmt.Errorf("ebsConfig.region_magic empty")
	}
	region := c.Region
	if region == "" {
		region = regionMagic
	}

	clusters := make([]controller.Cluster, 0, len(c.Clusters))
	for _, cl := range c.Clusters {
		if cl.ClusterID == 0 || len(cl.Hosts) == 0 {
			continue
		}
		clusters = append(clusters, controller.Cluster{
			ClusterID: ebsproto.ClusterID(cl.ClusterID),
			Hosts:     append([]string(nil), cl.Hosts...),
		})
	}

	consulAddr := poolECAddr
	if c.ConsulAddress != "" {
		consulAddr = c.ConsulAddress
	}
	if consulAddr == "" && len(clusters) == 0 {
		return nil, fmt.Errorf("ebsConfig: need consul_address or clusters")
	}

	maxBlob := defaultMaxBlobSize
	if c.MaxBlobSize != nil && *c.MaxBlobSize > 0 {
		maxBlob = *c.MaxBlobSize
	}

	// Same dir as audit/output: <logDir>/client/client/ebs_sdk.log
	// (opt.Logpath is already <logDir>/client).
	ebsLog := path.Join(logPath, "client", "ebs_sdk.log")
	if err := os.MkdirAll(path.Dir(ebsLog), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir ebs_sdk log dir failed: %v", err)
	}

	// Default: follow cfs-client --logLevel (GetBlobLogLevel after InitLog).
	// Override only when ebsConfig explicitly sets log_level (Default leaves it nil).
	logLevel := log.GetBlobLogLevel()
	if c.LogLevel != nil {
		logLevel = blog.Level(*c.LogLevel)
	}

	f, err := os.OpenFile(ebsLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open ebs_sdk log failed: %v", err)
	}

	cfg := &sdk.Config{
		StreamConfig: stream.StreamConfig{
			IDC:         c.Idc,
			MaxBlobSize: maxBlob,
			ClusterConfig: controller.ClusterConfig{
				Region:          region,
				RegionMagic:     regionMagic,
				ConsulAgentAddr: consulAddr,
				Clusters:        clusters,
			},
		},
		LogConf: cmd.LogConfig{
			Level: logLevel,
		},
		Logger: f, // same as demo -log=sdk.log; takes precedence over LogConf.Filename
	}
	log.LogWarnf("BuildSdkConfig: idc(%v) region(%v) magic(%v) consul(%v) clusters(%d) maxBlob(%v) sdkLogLevel(%v) ebsLogLevelSet(%v) log(%v)",
		cfg.IDC, region, regionMagic, consulAddr, len(clusters), maxBlob, logLevel, c.LogLevel != nil, ebsLog)
	return cfg, nil
}
