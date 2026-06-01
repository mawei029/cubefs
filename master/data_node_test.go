package master

import (
	"fmt"
	"testing"
	"time"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util"
	"github.com/stretchr/testify/require"
)

func TestDataNode(t *testing.T) {
	// /dataNode/add and /dataNode/response processed by mock data server
	var err error
	addr := "127.0.0.1:9096"
	func() {
		mockServerLock.Lock()
		defer mockServerLock.Unlock()
		mockDataServers = append(mockDataServers, addDataServer(addr, "test-add-zone", defaultMediaType))
	}()
	server.cluster.checkDataNodeHeartbeat()
	time.Sleep(5 * time.Second)
	getDataNodeInfo(addr, t)
	updateDisks(addr, t)
	decommissionDataNode(addr, t)
	for i := 0; i < 10; i++ { // decommission is async process
		_, err = server.cluster.dataNode(addr)
		if err == nil {
			time.Sleep(time.Second)
			continue
		}
		break
	}
	if err != nil {
		t.Errorf("decommission datanode [%v] failed", addr)
	}
	server.cluster.dataNodes.Delete(addr)
}

func getDataNodeInfo(addr string, t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?addr=%v", hostAddr, proto.GetDataNode, addr)
	process(reqURL, t)
}

func decommissionDataNode(addr string, t *testing.T) {
	reqURL := fmt.Sprintf("%v%v?addr=%v", hostAddr, proto.DecommissionDataNode, addr)
	process(reqURL, t)
}

func updateDisks(addr string, t *testing.T) {
	dn, err := server.cluster.dataNode(addr)
	require.NoError(t, err)

	dn.AllDisks = []string{"/data1"}
	allDisk := []string{"/data1", "/data2", "/data3"}
	badDisk := []string{"/data1"}
	updated, _ := dn.updateDisks(allDisk, badDisk)
	require.Equal(t, updated, true)
	require.Equal(t, allDisk, dn.AllDisks)
	require.Equal(t, badDisk, dn.BadDisks)
}

func TestDataNodeDecommissionTargetTagLifecycle(t *testing.T) {
	dn := newDataNode("127.0.0.1:17310", "17320", "17330", "zone1", "rack1", "cluster1", proto.MediaType_HDD)

	dn.markDecommission("", false, 0, lowPriorityDecommissionWeight, "target")
	require.Equal(t, "target", dn.DecommissionTargetTag)

	dn.resetDecommissionStatus()
	require.Empty(t, dn.DecommissionTargetTag)
}

func TestDecommissionDiskTargetTagLifecycle(t *testing.T) {
	disk := &DecommissionDisk{}

	disk.markDecommission("", false, 0, "target")
	require.Equal(t, "target", disk.DecommissionTargetTag)
}

func TestValidateDataNodeDecommissionTargetTag(t *testing.T) {
	const (
		srcAddr       = "127.0.0.1:17310"
		targetAddr    = "127.0.0.2:17310"
		targetTag     = "target"
		partitionID   = uint64(1)
		volName       = "test-vol"
		srcDiskPath   = "/data1"
		replicaUsedGB = uint64(20)
	)

	newTestDataNode := func(addr, tag string, availableGB uint64) *DataNode {
		return &DataNode{
			Addr:               addr,
			Total:              100 * util.GB,
			Used:               10 * util.GB,
			AvailableSpace:     availableGB * util.GB,
			isActive:           true,
			MediaType:          proto.MediaType_SSD,
			Tag:                tag,
			DpCntLimit:         100,
			AllDisks:           []string{srcDiskPath},
			DataPartitionCount: 1,
		}
	}

	newTestCluster := func(nodes ...*DataNode) *Cluster {
		cluster := &Cluster{
			ClusterVolSubItem: ClusterVolSubItem{
				vols: make(map[string]*Vol),
			},
		}
		for _, node := range nodes {
			cluster.dataNodes.Store(node.Addr, node)
		}
		return cluster
	}

	addTestDataPartition := func(cluster *Cluster, src *DataNode, hosts []string, usedGB uint64) {
		vol := &Vol{
			ID:             1,
			Name:           volName,
			dpReplicaNum:   3,
			dataPartitions: newDataPartitionMap(volName),
		}
		dp := newDataPartition(partitionID, 3, volName, vol.ID, proto.PartitionTypeNormal, proto.MediaType_SSD, defaultPoolId)
		replica := newDataReplica(src)
		replica.DiskPath = srcDiskPath
		replica.Used = usedGB * util.GB
		dp.addReplica(replica)
		dp.Hosts = hosts
		vol.dataPartitions.put(dp)
		cluster.vols[volName] = vol
	}

	t.Run("empty target tag is allowed", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		cluster := newTestCluster(src)

		require.NoError(t, cluster.validateDataNodeDecommissionTargetTag(srcAddr, ""))
	})

	t.Run("invalid target tag", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		cluster := newTestCluster(src)

		err := cluster.validateDataNodeDecommissionTargetTag(srcAddr, "bad-tag")
		require.Error(t, err)
		require.Contains(t, err.Error(), "targetTag invalid")
	})

	t.Run("source node not found", func(t *testing.T) {
		cluster := newTestCluster()

		err := cluster.validateDataNodeDecommissionTargetTag(srcAddr, targetTag)
		require.Error(t, err)
	})

	t.Run("target tag has no datanode", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		cluster := newTestCluster(src)

		err := cluster.validateDataNodeDecommissionTargetTag(srcAddr, targetTag)
		require.Error(t, err)
		require.Contains(t, err.Error(), "has no datanode")
	})

	t.Run("target tag has no writable candidate", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		target := newTestDataNode(targetAddr, targetTag, 90)
		target.RdOnly = true
		cluster := newTestCluster(src, target)

		err := cluster.validateDataNodeDecommissionTargetTag(srcAddr, targetTag)
		require.Error(t, err)
		require.Contains(t, err.Error(), "has no writable datanode")
	})

	t.Run("target tag candidate capacity is not enough", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		target := newTestDataNode(targetAddr, targetTag, 11)
		cluster := newTestCluster(src, target)
		addTestDataPartition(cluster, src, []string{srcAddr}, replicaUsedGB)

		err := cluster.validateDataNodeDecommissionTargetTag(srcAddr, targetTag)
		require.Error(t, err)
		require.Contains(t, err.Error(), "available capacity is not enough")
	})

	t.Run("target tag has no candidate for partition", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		target := newTestDataNode(targetAddr, targetTag, 90)
		cluster := newTestCluster(src, target)
		addTestDataPartition(cluster, src, []string{srcAddr, targetAddr}, replicaUsedGB)

		err := cluster.validateDataNodeDecommissionTargetTag(srcAddr, targetTag)
		require.Error(t, err)
		require.Contains(t, err.Error(), "has no candidate for dp")
	})

	t.Run("target tag precheck success", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		target := newTestDataNode(targetAddr, targetTag, 90)
		cluster := newTestCluster(src, target)
		addTestDataPartition(cluster, src, []string{srcAddr}, replicaUsedGB)

		require.NoError(t, cluster.validateDataNodeDecommissionTargetTag(srcAddr, targetTag))
	})

	t.Run("different media type is excluded from candidates", func(t *testing.T) {
		src := newTestDataNode(srcAddr, "source", 90)
		target := newTestDataNode(targetAddr, targetTag, 90)
		target.MediaType = proto.MediaType_HDD
		cluster := newTestCluster(src, target)

		err := cluster.validateDataNodeDecommissionTargetTag(srcAddr, targetTag)
		require.Error(t, err)
		require.Contains(t, err.Error(), "has no writable datanode")
	})
}

func TestCalculateDpLimitByDiskCapacity(t *testing.T) {
	t.Run("SSD", func(t *testing.T) {
		cfg := newClusterConfig()
		cfg.DpLimitSsdBaseCount = 150
		cfg.DpLimitSsdFactor = 50 // 5.0 in tenths
		cluster := &Cluster{cfg: cfg}

		dn := &DataNode{
			AllDisks:  []string{"/data1", "/data2"},
			Total:     4096 * util.GB, // 4TB in bytes
			MediaType: proto.MediaType_SSD,
		}

		got := dn.calculateDpLimitByDiskCapacity(cluster)
		// expected = base*diskCount + (totalGB*factor)/(120*10) where factor is tenths
		want := uint64(150*2 + (4096*50)/(120*10))
		require.Equal(t, want, got)
	})

	t.Run("HDD", func(t *testing.T) {
		cfg := newClusterConfig()
		cfg.DpLimitHddBaseCount = 100
		cfg.DpLimitHddFactor = 20 // 2.0 in tenths
		cluster := &Cluster{cfg: cfg}

		dn := &DataNode{
			AllDisks:  []string{"/data1"},
			Total:     14336 * util.GB, // 14TB in bytes
			MediaType: proto.MediaType_HDD,
		}

		got := dn.calculateDpLimitByDiskCapacity(cluster)
		// expected = base*diskCount + (totalGB*factor)/(120*10)
		want := uint64(100*1 + (14336*20)/(120*10))
		require.Equal(t, want, got)
	})
}
