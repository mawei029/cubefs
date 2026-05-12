package flashgroupmanager

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"

	raftProto "github.com/cubefs/cubefs/depends/tiglabs/raft/proto"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/cubefs/cubefs/raftstore/raftstore_db"
	"github.com/stretchr/testify/require"
)

type testPartition struct {
	fsm   *MetadataFsm
	index uint64
}

func (p *testPartition) Submit(cmd []byte) (interface{}, error) {
	p.index++
	return p.fsm.Apply(cmd, p.index)
}

func (p *testPartition) ChangeMember(changeType raftProto.ConfChangeType, peer raftProto.Peer, context []byte) (interface{}, error) {
	return nil, nil
}

func (p *testPartition) Stop() error {
	return nil
}

func (p *testPartition) Delete() error {
	return nil
}

func (p *testPartition) Status() *raftstore.PartitionStatus {
	return nil
}

func (p *testPartition) IsRestoring() bool {
	return false
}

func (p *testPartition) LeaderTerm() (leaderID, term uint64) {
	return 1, 1
}

func (p *testPartition) IsRaftLeader() bool {
	return true
}

func (p *testPartition) AppliedIndex() uint64 {
	return p.index
}

func (p *testPartition) CommittedIndex() uint64 {
	return p.index
}

func (p *testPartition) Truncate(index uint64) {
}

func (p *testPartition) TryToLeader(nodeID uint64) error {
	return nil
}

func (p *testPartition) IsOfflinePeer() bool {
	return false
}

func (p *testPartition) CloseAndBackup() error {
	return nil
}

func (p *testPartition) Closed() bool {
	return false
}

func newClusterForLoadFlashToposTest(t *testing.T) (*Cluster, func()) {
	t.Helper()

	storeDir, err := os.MkdirTemp("", "flashgroupmanager-load-topos-*")
	require.NoError(t, err)

	db, err := raftstore_db.NewRocksDBStoreAndRecovery(storeDir, LRUCacheSize, WriteBufferSize)
	require.NoError(t, err)

	fsm := &MetadataFsm{
		store:      db,
		retainLogs: 1024,
	}
	partition := &testPartition{fsm: fsm}
	cluster := &Cluster{
		Name:          "test-cluster",
		cfg:           newClusterConfig(),
		flashNodeTopo: new(sync.Map),
		fsm:           fsm,
		partition:     partition,
	}
	cluster.idAlloc = newIDAllocator(db, partition)
	cluster.idAlloc.restore()

	cleanup := func() {
		db.Close()
		_ = os.RemoveAll(storeDir)
	}

	return cluster, cleanup
}

func TestLoadFlashToposPersistsDefaultAndIdleTopos(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()

	cluster.cfg.FlashNodeHandleReadTimeout = 111
	cluster.cfg.FlashNodeReadDataNodeTimeout = 222
	cluster.cfg.FlashHotKeyMissCount = 333
	cluster.cfg.FlashReadFlowLimit = 444
	cluster.cfg.FlashWriteFlowLimit = 555
	cluster.cfg.FlashKeyFlowLimit = 0

	require.NoError(t, cluster.loadFlashTopos())

	defaultTopo, err := cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	idleTopo, err := cluster.PeekFlashTopo(proto.IdleTopoName)
	require.NoError(t, err)
	require.NotZero(t, defaultTopo.ID)
	require.NotZero(t, idleTopo.ID)
	require.NotEqual(t, defaultTopo.ID, idleTopo.ID)
	require.Equal(t, FlashNodeHeartbeatConfig{
		FlashNodeHandleReadTimeout:   111,
		FlashNodeReadDataNodeTimeout: 222,
		FlashHotKeyMissCount:         333,
		FlashReadFlowLimit:           444,
		FlashWriteFlowLimit:          555,
		FlashKeyFlowLimit:            0,
	}, defaultTopo.GetHeartbeatConfig())

	result, err := cluster.fsm.store.SeekForPrefix([]byte(flashTopoPrefix))
	require.NoError(t, err)
	require.Len(t, result, 2)

	reloadedCluster := &Cluster{
		Name:          cluster.Name,
		cfg:           newClusterConfig(),
		flashNodeTopo: new(sync.Map),
		fsm:           cluster.fsm,
	}
	require.NoError(t, reloadedCluster.loadFlashTopos())

	reloadedDefaultTopo, err := reloadedCluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	reloadedIdleTopo, err := reloadedCluster.PeekFlashTopo(proto.IdleTopoName)
	require.NoError(t, err)
	require.Equal(t, defaultTopo.ID, reloadedDefaultTopo.ID)
	require.Equal(t, idleTopo.ID, reloadedIdleTopo.ID)
	require.Equal(t, defaultTopo.GetHeartbeatConfig(), reloadedDefaultTopo.GetHeartbeatConfig())
}

func TestClusterLoadClusterValueSyncsMaxDisablePercentToTopos(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()

	topo := NewFlashNodeTopology("topo-a", proto.DefaultRegion, 100, proto.TopoStatusNormal)
	cluster.flashNodeTopo.Store(topo.Name, topo)
	cluster.flashNodeTopo.Store("bad-value", "not-a-topo")
	cluster.flashNodeTopo.Store("nil-topo", (*FlashNodeTopology)(nil))
	cluster.cfg.MaxDisableFlashGroupPercent = 55
	cluster.cfg.FlashHotKeyMissCount = 66
	require.NoError(t, cluster.syncPutCluster())

	cluster.cfg.MaxDisableFlashGroupPercent = 1
	cluster.cfg.FlashHotKeyMissCount = 1
	require.NoError(t, cluster.loadClusterValue())

	require.Equal(t, 55, cluster.cfg.MaxDisableFlashGroupPercent)
	require.Equal(t, 66, cluster.cfg.FlashHotKeyMissCount)
	require.Equal(t, uint32(55), atomic.LoadUint32(&topo.maxDisableFlashGroupPercent))
}

func TestClusterLoadFlashToposRestoresPersistedTopoAndCreatesIdle(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()

	topo := NewFlashNodeTopology("topo-a", proto.DefaultRegion, 101, proto.TopoStatusMarkDelete)
	topo.DeleteStep = 3
	topo.DeleteGradualFlag = true
	topo.RemoteCacheReadFlowMap = map[string]int64{"vol-a": 11}
	topo.RemoteCacheWriteFlowMap = map[string]int64{"vol-a": 22}
	topo.SetHeartbeatConfig(FlashNodeHeartbeatConfig{
		FlashNodeHandleReadTimeout:   111,
		FlashNodeReadDataNodeTimeout: 222,
		FlashHotKeyMissCount:         333,
		FlashReadFlowLimit:           444,
		FlashWriteFlowLimit:          555,
		FlashKeyFlowLimit:            666,
	})
	require.NoError(t, cluster.syncAddFlashTopo(topo))

	cluster.flashNodeTopo = new(sync.Map)
	require.NoError(t, cluster.loadFlashTopos())

	restored, err := cluster.PeekFlashTopo("topo-a")
	require.NoError(t, err)
	require.Equal(t, proto.TopoStatusMarkDelete, atomic.LoadUint32(&restored.Status))
	require.Equal(t, uint32(3), restored.DeleteStep)
	require.True(t, restored.DeleteGradualFlag)
	require.Equal(t, int64(11), restored.GetRemoteCacheReadFlowMap()["vol-a"])
	require.Equal(t, int64(22), restored.GetRemoteCacheWriteFlowMap()["vol-a"])
	require.Equal(t, int64(666), restored.GetHeartbeatConfig().FlashKeyFlowLimit)

	_, err = cluster.PeekFlashTopo(proto.IdleTopoName)
	require.NoError(t, err)
}

func TestClusterLoadFlashNodesFallbacksAndLoadsZones(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()
	require.NoError(t, cluster.loadFlashTopos())

	nodes := []*FlashNode{
		{FlashNodeValue: FlashNodeValue{ID: 201, Addr: "127.0.0.1:201", ZoneName: "zone-new", FlashGroupID: 1, IsEnable: true, FlashNodeTopoName: proto.DefaultTopoName, Region: proto.DefaultRegion}},
		{FlashNodeValue: FlashNodeValue{ID: 202, Addr: "127.0.0.1:202", ZoneName: proto.DefaultZoneName, FlashGroupID: 1, IsEnable: true, FlashNodeTopoName: "missing-topo", Region: proto.DefaultRegion}},
		{FlashNodeValue: FlashNodeValue{ID: 203, Addr: "127.0.0.1:203", ZoneName: proto.DefaultZoneName, FlashGroupID: UnusedFlashNodeFlashGroupID, IsEnable: true, Region: "other-region"}},
	}
	for _, node := range nodes {
		require.NoError(t, cluster.syncAddFlashNode(node))
	}

	require.NoError(t, cluster.loadFlashTopos())
	require.NoError(t, cluster.loadFlashNodes())

	defaultTopo, err := cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	_, err = defaultTopo.GetZone("zone-new")
	require.NoError(t, err)
	_, err = defaultTopo.PeekFlashNode("127.0.0.1:201")
	require.NoError(t, err)
	_, err = defaultTopo.PeekFlashNode("127.0.0.1:202")
	require.NoError(t, err)

	idleTopo, err := cluster.PeekFlashTopo(proto.IdleTopoName)
	require.NoError(t, err)
	node, err := idleTopo.PeekFlashNode("127.0.0.1:203")
	require.NoError(t, err)
	require.Equal(t, proto.IdleTopoName, node.FlashNodeTopoName)
}

func TestClusterLoadFlashGroupsAndTopology(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()
	require.NoError(t, cluster.loadFlashTopos())

	require.NoError(t, cluster.syncAddFlashGroup(newFlashGroup(301, []uint32{301}, proto.SlotStatus_Completed, nil, 0,
		proto.FlashGroupStatus_Active, 1, "missing-topo", proto.DefaultRegion)))
	require.NoError(t, cluster.syncAddFlashGroup(newFlashGroup(302, []uint32{302}, proto.SlotStatus_Completed, nil, 0,
		proto.FlashGroupStatus_Active, 1, proto.DefaultTopoName, proto.DefaultRegion)))
	require.NoError(t, cluster.syncAddFlashNode(&FlashNode{FlashNodeValue: FlashNodeValue{
		ID: 302, Addr: "127.0.0.1:302", ZoneName: proto.DefaultZoneName, FlashGroupID: 302,
		IsEnable: true, FlashNodeTopoName: proto.DefaultTopoName, Region: proto.DefaultRegion,
	}}))

	require.NoError(t, cluster.loadFlashTopos())
	require.NoError(t, cluster.loadFlashNodes())
	require.NoError(t, cluster.loadFlashGroups())
	require.NoError(t, cluster.loadFlashTopology())

	defaultTopo, err := cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	loadedGroup, err := defaultTopo.GetFlashGroup(302)
	require.NoError(t, err)
	require.Equal(t, 1, loadedGroup.GetFlashNodesCount())
	_, err = defaultTopo.GetFlashGroup(301)
	require.Error(t, err)

	cluster.flashNodeTopo.Store("bad-value", "not-a-topo")
	cluster.flashNodeTopo.Store("nil-topo", (*FlashNodeTopology)(nil))
	require.NoError(t, cluster.loadFlashTopology())
}

func TestClusterLoadFlashTopoDecodeErrors(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()

	_, err := cluster.fsm.store.Put(flashTopoPrefix+"bad", []byte("{bad-json"), true)
	require.NoError(t, err)
	require.Error(t, cluster.loadFlashTopos())

	_, err = cluster.fsm.store.Put(flashNodePrefix+"bad", []byte("{bad-json"), true)
	require.NoError(t, err)
	require.Error(t, cluster.loadFlashNodes())

	_, err = cluster.fsm.store.Put(flashGroupPrefix+"bad", []byte("{bad-json"), true)
	require.NoError(t, err)
	require.Error(t, cluster.loadFlashGroups())
}

func TestFlashGroupManagerClearAndLoadMetadata(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()

	manager := &FlashGroupManager{clusterName: cluster.Name, cluster: cluster}
	cluster.flashNodeTopo.Store("stale", NewFlashNodeTopology("stale", proto.DefaultRegion, 1, proto.TopoStatusNormal))
	manager.clearMetadata()
	_, err := cluster.PeekFlashTopo("stale")
	require.Error(t, err)

	manager.loadMetadata()
	_, err = cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	_, err = cluster.PeekFlashTopo(proto.IdleTopoName)
	require.NoError(t, err)
}

func TestFlashGroupManagerLoadMetadataPanicsOnFlashTopoError(t *testing.T) {
	cluster, cleanup := newClusterForLoadFlashToposTest(t)
	defer cleanup()

	_, err := cluster.fsm.store.Put(flashTopoPrefix+"bad", []byte("{bad-json"), true)
	require.NoError(t, err)

	manager := &FlashGroupManager{clusterName: cluster.Name, cluster: cluster}
	require.Panics(t, manager.loadMetadata)
}
