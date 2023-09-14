package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"io/ioutil"

	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/common/config"
	"github.com/cubefs/cubefs/blobstore/scheduler"
	"github.com/cubefs/cubefs/blobstore/scheduler/client"
	"github.com/cubefs/cubefs/blobstore/util/log"
)

var (
	confFile = flag.String("f", "blobnode_test.conf", "config file path")

	conf BlobnodeTestConf
)

type BlobnodeTestConf struct {
	ClusterID  proto.ClusterID `json:"cluster_id"` // uint32
	LogLevel   log.Level       `json:"log_level"`  // int
	MyHost     string          `json:"my_host"`
	ClusterMgr cmapi.Config    `json:"cluster_mgr"`
}

type ClusterMgrConf struct {
}

func main() {
	// 1. flag parse
	flag.Parse()
	if err := invalidArgs(); err != nil {
		panic(err)
	}

	ctx := context.Background()
	initConfMgr(ctx)

	// 根据host拿到该节点的disk，拿到vuid，申请chunk，用本地file的构造data数据，生成唯一bid, 开始put, 记录location

	// 根据location, 开始Get
}

func invalidArgs() error {
	return nil
}

func initConfMgr(ctx context.Context) {
	confBytes, err := ioutil.ReadFile(*confFile)
	if err != nil {
		log.Fatalf("read config file failed, filename: %s, err: %v", *confFile, err)
	}

	fmt.Printf("Config file %s:\n%s \n", *confFile, confBytes)
	if err = config.LoadData(&conf, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.SetOutputLevel(conf.LogLevel)
	fmt.Printf("Config: %+v \n", conf)

	clusterMgrCli := client.NewClusterMgrClient(&conf.ClusterMgr)
	clusterMgrCli.ListClusterDisks(ctx)

	topologyMgr := scheduler.NewClusterTopologyMgr1(clusterMgrCli, conf.ClusterID)
	topologyMgr.GetIDCs()
	//mgr, _ = NewBlobDelMgr(topologyMgr)
	////mgr.getHost(vid, idx)
}
