package main

import (
	"errors"
	"flag"
	"fmt"
	cmapi "github.com/cubefs/cubefs/blobstore/api/clustermgr"
	"github.com/cubefs/cubefs/blobstore/common/config"
	"github.com/cubefs/cubefs/blobstore/common/proto"
	"github.com/cubefs/cubefs/blobstore/scheduler"
	"github.com/cubefs/cubefs/blobstore/scheduler/client"
	"github.com/cubefs/cubefs/blobstore/util/log"
	"io/ioutil"
	"strings"
	"time"
)

var (
	errOutOfIndex = errors.New("out of index")
	errWaitUpdate = errors.New("wait 1 min")
)

func (m *BlobDelMgr) getHost(vid proto.Vid, idx int, bid uint64, hosts map[string]int) error {
	volume, err := m.clusterTopology.GetVolume(vid)
	if err != nil {
		return err
	}

	if len(volume.VunitLocations) < 12 {
		return errWaitUpdate
	}

	if idx < 0 || idx >= len(volume.VunitLocations) {
		return errOutOfIndex
	}

	// /shard/markdelete/diskid/298/vuid/193905291165699/bid/4500926655
	//log.Infof("bad vid:%d, index:%d, bid:%d, location: %+v \n", vid, idx, bid, volume.VunitLocations[idx])
	fmt.Printf("curl %s/shard/markdelete/diskid/%d/vuid/%d/bid/%d \t ; bad vid:%d, index:%d \n",
		volume.VunitLocations[idx].Host, volume.VunitLocations[idx].DiskID, volume.VunitLocations[idx].Vuid, bid, vid, idx)
	hosts[volume.VunitLocations[idx].Host]++
	return nil
}

type BlobDelMgr struct {
	clusterTopology scheduler.IClusterTopology
}

func NewBlobDelMgr(cluster scheduler.IClusterTopology) (*BlobDelMgr, error) {
	mgr := &BlobDelMgr{
		clusterTopology: cluster,
	}
	return mgr, nil
}

var (
	conf1     BlobDelConfig
	confFile1 = flag.String("f", "parseKafka.conf", "config filename")
)

type BlobDelConfig struct {
	ClusterID  proto.ClusterID `json:"cluster_id"`
	LogLevel   log.Level       `json:"log_level"`
	EcLen      []int           `json:"ec_len"`
	ClusterMgr cmapi.Config    `json:"cluster_mgr"`
}

//type clusterTopologyConfig struct {
//	ClusterID               proto.ClusterID
//	Leader                  bool
//	UpdateInterval          time.Duration
//	VolumeUpdateInterval    time.Duration
//	FreeChunkCounterBuckets []float64
//}

var mgr *BlobDelMgr

func initMgr() {
	flag.Parse()
	confBytes, err := ioutil.ReadFile(*confFile1)
	if err != nil {
		log.Fatalf("read config file failed, filename: %s, err: %v", *confFile1, err)
	}

	fmt.Printf("Config file %s:\n%s \n", *confFile1, confBytes)
	if err = config.LoadData(&conf1, confBytes); err != nil {
		log.Fatalf("load config failed, error: %+v", err)
	}
	log.SetOutputLevel(conf1.LogLevel)
	fmt.Printf("Config: %+v \n", conf1)

	clusterMgrCli := client.NewClusterMgrClient(&conf1.ClusterMgr)
	//topoConf := &clusterTopologyConfig{
	//	ClusterID:            conf1.ClusterID,
	//	Leader:               true,
	//	UpdateInterval:       1 * time.Minute,
	//	VolumeUpdateInterval: 10 * time.Second,
	//}
	topologyMgr := scheduler.NewClusterTopologyMgr1(clusterMgrCli, conf1.ClusterID)
	mgr, _ = NewBlobDelMgr(topologyMgr)
	//mgr.getHost(vid, idx)
}

// 根据日志解析出来没有两阶段删除的blobnode对应的host
func getAllHost(rets map[uint64]KafkaMsg, mode int) {
	hosts := map[string]int{}

	switch mode {
	case jsonMsgMode:
		for _, v := range rets {
			try := 0
			for idx, val := range v.BlobDelStages.Stages {
				if val == 1 {
					err := mgr.getHost(proto.Vid(v.Vid), int(idx), v.Bid, hosts)
					if err == errWaitUpdate && try < 1 {
						try++
						time.Sleep(time.Minute)
						err = mgr.getHost(proto.Vid(v.Vid), int(idx), v.Bid, hosts)
					}

					if err != nil {
						panic(err)
					}
				}
			}
		}
	case printStructMode:
		for _, v := range rets {
			//blobStages:{Stages:map[0:2 1:2 2:1 3:2 4:2 5:2 6:2 7:2 8:1 9:2 10:2 11:2]}}
			mp := strings.TrimPrefix(v.blobStages, "{Stages:map[")
			mp = strings.TrimRight(mp, "]}")
			strs := strings.Split(mp, " ")
			for idx, val := range strs {
				delFlag := strings.Split(val, ":")[1]
				if delFlag == "1" {
					err := mgr.getHost(proto.Vid(v.Vid), idx, v.Bid, hosts)
					if err != nil {
						panic(err)
					}
				}
			}
		}
	}

	fmt.Printf("all hostLen:%d, hosts: %+v \n", len(hosts), hosts)
}