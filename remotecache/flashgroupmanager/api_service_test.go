package flashgroupmanager

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	raftProto "github.com/cubefs/cubefs/depends/tiglabs/raft/proto"
	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/raftstore"
	"github.com/stretchr/testify/require"
)

type apiServiceSubmitPartition struct {
	err error
}

func (p *apiServiceSubmitPartition) Submit(cmd []byte) (interface{}, error) {
	return nil, p.err
}

func (p *apiServiceSubmitPartition) ChangeMember(changeType raftProto.ConfChangeType, peer raftProto.Peer, context []byte) (interface{}, error) {
	return nil, nil
}

func (p *apiServiceSubmitPartition) Stop() error {
	return nil
}

func (p *apiServiceSubmitPartition) Delete() error {
	return nil
}

func (p *apiServiceSubmitPartition) Status() *raftstore.PartitionStatus {
	return nil
}

func (p *apiServiceSubmitPartition) IsRestoring() bool {
	return false
}

func (p *apiServiceSubmitPartition) LeaderTerm() (leaderID, term uint64) {
	return 1, 1
}

func (p *apiServiceSubmitPartition) IsRaftLeader() bool {
	return true
}

func (p *apiServiceSubmitPartition) AppliedIndex() uint64 {
	return 0
}

func (p *apiServiceSubmitPartition) CommittedIndex() uint64 {
	return 0
}

func (p *apiServiceSubmitPartition) Truncate(index uint64) {
}

func (p *apiServiceSubmitPartition) TryToLeader(nodeID uint64) error {
	return nil
}

func (p *apiServiceSubmitPartition) IsOfflinePeer() bool {
	return false
}

func (p *apiServiceSubmitPartition) CloseAndBackup() error {
	return nil
}

func (p *apiServiceSubmitPartition) Closed() bool {
	return false
}

func newAPIServiceTestManager(t *testing.T) *FlashGroupManager {
	t.Helper()

	partition := &apiServiceSubmitPartition{}
	cluster := &Cluster{
		Name:          "test-cluster",
		cfg:           newClusterConfig(),
		flashNodeTopo: new(sync.Map),
		idAlloc:       newIDAllocator(nil, partition),
		partition:     partition,
	}
	defaultTopo := newAPIServiceTestTopo(t, proto.DefaultTopoName, proto.DefaultRegion, 1, 101)
	otherTopo := newAPIServiceTestTopo(t, "topo-a", proto.DefaultRegion, 2, 201)
	idleTopo := NewFlashNodeTopology(proto.IdleTopoName, proto.DefaultRegion, 3, proto.TopoStatusNormal)
	cluster.flashNodeTopo.Store(defaultTopo.Name, defaultTopo)
	cluster.flashNodeTopo.Store(otherTopo.Name, otherTopo)
	cluster.flashNodeTopo.Store(idleTopo.Name, idleTopo)

	return &FlashGroupManager{
		metaReady:  true,
		config:     cluster.cfg,
		cluster:    cluster,
		leaderInfo: &LeaderInfo{addr: "127.0.0.1:17010"},
	}
}

func newAPIServiceTestTopo(t *testing.T, name, region string, topoID, fgID uint64) *FlashNodeTopology {
	t.Helper()

	topo := NewFlashNodeTopology(name, region, topoID, proto.TopoStatusNormal)
	topo.SyncFlashGroupFunc = tSyncUpdateFlashGroup
	fg := newFlashGroup(fgID, []uint32{uint32(fgID)}, proto.SlotStatus_Completed, nil, 1,
		proto.FlashGroupStatus_Active, 1, name, region)
	node := &FlashNode{
		FlashNodeValue: FlashNodeValue{
			ID:                fgID,
			Addr:              fmt.Sprintf("127.0.0.1:%d", 10000+topoID),
			ZoneName:          proto.DefaultZoneName,
			FlashGroupID:      fgID,
			IsEnable:          true,
			FlashNodeTopoName: name,
			Region:            region,
		},
		ReportTime: time.Now(),
		IsActive:   true,
		TaskManager: &AdminTaskManager{
			exitCh: make(chan struct{}, 1),
		},
	}
	fg.putFlashNode(node)
	topo.flashGroupMap.Store(fg.ID, fg)
	topo.slotsMap[uint32(fgID)] = fg.ID
	require.NoError(t, topo.PutFlashNode(node))
	return topo
}

func addAPIServiceTestUnusedNode(t *testing.T, topo *FlashNodeTopology, addr string, id uint64, active bool) *FlashNode {
	t.Helper()

	node := &FlashNode{
		FlashNodeValue: FlashNodeValue{
			ID:                id,
			Addr:              addr,
			ZoneName:          proto.DefaultZoneName,
			FlashGroupID:      UnusedFlashNodeFlashGroupID,
			IsEnable:          true,
			FlashNodeTopoName: topo.Name,
			Region:            topo.Region,
		},
		ReportTime: time.Now(),
		IsActive:   active,
		TaskManager: &AdminTaskManager{
			exitCh: make(chan struct{}, 1),
		},
	}
	require.NoError(t, topo.PutFlashNode(node))
	return node
}

func runAPIServiceRequest(t *testing.T, handler http.HandlerFunc, rawQuery string) proto.HTTPReplyRaw {
	t.Helper()

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin?"+rawQuery, nil)
	recorder := httptest.NewRecorder()
	handler(recorder, req)

	var reply proto.HTTPReplyRaw
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &reply))
	return reply
}

func decodeAPIServiceReplyData[T any](t *testing.T, reply proto.HTTPReplyRaw) T {
	t.Helper()

	var data T
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)
	require.NoError(t, json.Unmarshal(reply.Data, &data))
	return data
}

func TestFlashGroupManagerClientFlashGroupsSelectsTopo(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.clientFlashGroups, "")
	defaultView := decodeAPIServiceReplyData[proto.FlashGroupView](t, reply)
	require.Equal(t, proto.DefaultTopoName, defaultView.TopoName)
	require.Len(t, defaultView.FlashGroups, 1)
	require.Equal(t, uint64(101), defaultView.FlashGroups[0].ID)

	reply = runAPIServiceRequest(t, manager.clientFlashGroups, "name=topo-a")
	otherView := decodeAPIServiceReplyData[proto.FlashGroupView](t, reply)
	require.Equal(t, "topo-a", otherView.TopoName)
	require.Len(t, otherView.FlashGroups, 1)
	require.Equal(t, uint64(201), otherView.FlashGroups[0].ID)
}

func TestFlashGroupManagerClientFlashGroupsErrors(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	manager.metaReady = false

	reply := runAPIServiceRequest(t, manager.clientFlashGroups, "")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager.metaReady = true
	reply = runAPIServiceRequest(t, manager.clientFlashGroups, "name=missing")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerTurnFlashGroupUsesRequestedTopo(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.turnFlashGroup, "enable=true")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.turnFlashGroup, "name=topo-a&enable=false")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	topo, err := manager.cluster.PeekFlashTopo("topo-a")
	require.NoError(t, err)
	var cacheReply proto.HTTPReplyRaw
	require.NoError(t, json.Unmarshal(topo.GetClientResponse(), &cacheReply))
	view := decodeAPIServiceReplyData[proto.FlashGroupView](t, cacheReply)
	require.False(t, view.Enable)
	require.Empty(t, view.FlashGroups)

	reply = runAPIServiceRequest(t, manager.turnFlashGroup, "name=missing&enable=false")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerGetFlashGroupFallbacksToOwningTopo(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.getFlashGroup, "id=201")
	view := decodeAPIServiceReplyData[proto.FlashGroupAdminView](t, reply)
	require.Equal(t, uint64(201), view.ID)
	require.Equal(t, "topo-a", view.FlashNodeTopoName)

	reply = runAPIServiceRequest(t, manager.getFlashGroup, "id=201&name="+proto.DefaultTopoName)
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.getFlashGroup, "id=201&name=missing")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.getFlashGroup, "id=999")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerListFlashGroupsSupportsAllTopos(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Store("bad-value", "not-a-topo")
	manager.cluster.flashNodeTopo.Store("nil-topo", (*FlashNodeTopology)(nil))

	reply := runAPIServiceRequest(t, manager.listFlashGroups, "showAllTopo=true")
	all := decodeAPIServiceReplyData[proto.FlashGroupsAdminView](t, reply)
	require.Len(t, all.FlashGroups, 2)
	require.ElementsMatch(t, []uint64{101, 201}, []uint64{all.FlashGroups[0].ID, all.FlashGroups[1].ID})

	reply = runAPIServiceRequest(t, manager.listFlashGroups, "name=topo-a")
	oneTopo := decodeAPIServiceReplyData[proto.FlashGroupsAdminView](t, reply)
	require.Len(t, oneTopo.FlashGroups, 1)
	require.Equal(t, uint64(201), oneTopo.FlashGroups[0].ID)

	reply = runAPIServiceRequest(t, manager.listFlashGroups, "showAllTopo=not-bool")
	defaultTopo := decodeAPIServiceReplyData[proto.FlashGroupsAdminView](t, reply)
	require.Len(t, defaultTopo.FlashGroups, 1)
	require.Equal(t, uint64(101), defaultTopo.FlashGroups[0].ID)

	reply = runAPIServiceRequest(t, manager.listFlashGroups, "name=missing")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerListFlashNodesSupportsAllTopos(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Store("bad-value", "not-a-topo")
	manager.cluster.flashNodeTopo.Store("nil-topo", (*FlashNodeTopology)(nil))

	reply := runAPIServiceRequest(t, manager.listFlashNodes, "showAllTopo=true&active=1")
	nodesByZone := decodeAPIServiceReplyData[map[string][]*proto.FlashNodeViewInfo](t, reply)
	require.Len(t, nodesByZone[proto.DefaultZoneName], 2)
	require.ElementsMatch(t, []string{proto.DefaultTopoName, "topo-a"}, []string{
		nodesByZone[proto.DefaultZoneName][0].FlashNodeTopoName,
		nodesByZone[proto.DefaultZoneName][1].FlashNodeTopoName,
	})

	reply = runAPIServiceRequest(t, manager.listFlashNodes, "name=topo-a&active=1")
	oneTopo := decodeAPIServiceReplyData[map[string][]*proto.FlashNodeViewInfo](t, reply)
	require.Len(t, oneTopo[proto.DefaultZoneName], 1)
	require.Equal(t, "topo-a", oneTopo[proto.DefaultZoneName][0].FlashNodeTopoName)

	reply = runAPIServiceRequest(t, manager.listFlashNodes, "name=missing")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerSetFlashGroupFallbacksToOwningTopo(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.setFlashGroup, "id=201&enable=true")
	view := decodeAPIServiceReplyData[proto.FlashGroupAdminView](t, reply)
	require.Equal(t, uint64(201), view.ID)
	require.Equal(t, proto.FlashGroupStatus_Active, view.Status)

	reply = runAPIServiceRequest(t, manager.setFlashGroup, "id=201&name="+proto.DefaultTopoName+"&enable=true")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.setFlashGroup, "id=999&enable=true")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager.cluster.flashNodeTopo.Delete(proto.DefaultTopoName)
	reply = runAPIServiceRequest(t, manager.setFlashGroup, "id=999&enable=true")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerCreateFlashGroupTopoSelection(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.createFlashGroup, "name="+proto.IdleTopoName+"&weight=1")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.createFlashGroup, "weight=1")
	view := decodeAPIServiceReplyData[proto.FlashGroupAdminView](t, reply)
	require.Equal(t, proto.DefaultTopoName, view.FlashNodeTopoName)

	reply = runAPIServiceRequest(t, manager.createFlashGroup, "name=topo-a&weight=1")
	view = decodeAPIServiceReplyData[proto.FlashGroupAdminView](t, reply)
	require.Equal(t, "topo-a", view.FlashNodeTopoName)
	require.Equal(t, proto.DefaultRegion, view.Region)
}

func TestFlashGroupManagerRemoveFlashGroupTopoSelection(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.removeFlashGroup, "id=201")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	manager = newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Delete(proto.IdleTopoName)
	reply = runAPIServiceRequest(t, manager.removeFlashGroup, "id=201")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	reply = runAPIServiceRequest(t, manager.removeFlashGroup, "id=999")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Delete(proto.DefaultTopoName)
	reply = runAPIServiceRequest(t, manager.removeFlashGroup, "id=999")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerAddFlashNodeDefaults(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.addFlashNode, "addr=127.0.0.1:12000&zoneName="+proto.DefaultZoneName+"&detail=true")
	resp := decodeAPIServiceReplyData[proto.FlashNodeRegisterResponse](t, reply)
	require.NotZero(t, resp.NodeID)
	require.Equal(t, proto.IdleTopoName, resp.TopoName)
}

func TestFlashGroupManagerSetFlashNodePaths(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.setFlashNode, "addr=127.0.0.1:10001&enable=false")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.setFlashNode, "name=topo-a&addr=127.0.0.1:10002&enable=false&workRole=read")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.setFlashNode, "name=missing&addr=127.0.0.1:10002&enable=false")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.setFlashNode, "name=topo-a&addr=127.0.0.1:12999&enable=false")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerRemoveFlashNodePaths(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	defaultTopo, err := manager.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	addAPIServiceTestUnusedNode(t, defaultTopo, "127.0.0.1:13000", 300, false)

	reply := runAPIServiceRequest(t, manager.removeFlashNode, "addr=127.0.0.1:13000")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.removeFlashNode, "addr=127.0.0.1:10001")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.removeFlashNode, "addr=127.0.0.1:12999")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.removeFlashNode, "name=missing&addr=127.0.0.1:13000")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerRemoveAllInactiveFlashNodes(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	defaultTopo, err := manager.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	addAPIServiceTestUnusedNode(t, defaultTopo, "127.0.0.1:13100", 301, false)

	reply := runAPIServiceRequest(t, manager.removeAllInactiveFlashNodes, "")
	addrs := decodeAPIServiceReplyData[[]string](t, reply)
	require.Contains(t, addrs, "127.0.0.1:13100")

	reply = runAPIServiceRequest(t, manager.removeAllInactiveFlashNodes, "name=missing")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerGetFlashNodePaths(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.getFlashNode, "addr=127.0.0.1:10001")
	view := decodeAPIServiceReplyData[proto.FlashNodeViewInfo](t, reply)
	require.Equal(t, "127.0.0.1:10001", view.Addr)

	reply = runAPIServiceRequest(t, manager.getFlashNode, "name=missing&addr=127.0.0.1:10001")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerFlashGroupAddFlashNodePaths(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	idleTopo, err := manager.cluster.PeekFlashTopo(proto.IdleTopoName)
	require.NoError(t, err)
	addAPIServiceTestUnusedNode(t, idleTopo, "127.0.0.1:13200", 302, true)

	reply := runAPIServiceRequest(t, manager.flashGroupAddFlashNode, "id=201&addr=127.0.0.1:13200")
	view := decodeAPIServiceReplyData[proto.FlashGroupAdminView](t, reply)
	require.Equal(t, uint64(201), view.ID)

	manager = newAPIServiceTestManager(t)
	reply = runAPIServiceRequest(t, manager.flashGroupAddFlashNode, "id=201&addr=127.0.0.1:13299")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Delete(proto.IdleTopoName)
	reply = runAPIServiceRequest(t, manager.flashGroupAddFlashNode, "id=201&zoneName="+proto.DefaultZoneName+"&count=1")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Delete(proto.DefaultTopoName)
	reply = runAPIServiceRequest(t, manager.flashGroupAddFlashNode, "id=999&addr=127.0.0.1:13299")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	reply = runAPIServiceRequest(t, manager.flashGroupAddFlashNode, "id=999&addr=127.0.0.1:13299")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerFlashGroupRemoveFlashNodePaths(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.flashGroupRemoveFlashNode, "id=201&addr=127.0.0.1:10002")
	view := decodeAPIServiceReplyData[proto.FlashGroupAdminView](t, reply)
	require.Equal(t, uint64(201), view.ID)

	manager = newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Delete(proto.IdleTopoName)
	reply = runAPIServiceRequest(t, manager.flashGroupRemoveFlashNode, "id=201&addr=127.0.0.1:10002")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	manager.cluster.flashNodeTopo.Delete(proto.DefaultTopoName)
	reply = runAPIServiceRequest(t, manager.flashGroupRemoveFlashNode, "id=999&addr=127.0.0.1:10002")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	reply = runAPIServiceRequest(t, manager.flashGroupRemoveFlashNode, "id=999&addr=127.0.0.1:10002")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerFlashTopoHandlers(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.listFlashTopo, "")
	topos := decodeAPIServiceReplyData[[]*proto.FlashTopologyAdminView](t, reply)
	require.NotEmpty(t, topos)

	reply = runAPIServiceRequest(t, manager.addFlashTopo, "")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.addFlashTopo, "name=topo-new&region=region-new")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.addFlashTopo, "name=topo-a")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	managerWithoutIDAlloc := newAPIServiceTestManager(t)
	managerWithoutIDAlloc.cluster.idAlloc = nil
	reply = runAPIServiceRequest(t, managerWithoutIDAlloc.addFlashTopo, "name=topo-no-alloc")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	submitFailTopo := NewFlashNodeTopology("topo-submit-fail", proto.DefaultRegion, 88, proto.TopoStatusNormal)
	manager.cluster.flashNodeTopo.Store(submitFailTopo.Name, submitFailTopo)
	failPartition := &apiServiceSubmitPartition{err: fmt.Errorf("submit failed")}
	manager.cluster.partition = failPartition
	manager.cluster.idAlloc.partition = failPartition
	reply = runAPIServiceRequest(t, manager.deleteFlashTopo, "name=topo-submit-fail")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	reply = runAPIServiceRequest(t, manager.addFlashTopo, "name=topo-new&region=region-new")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.deleteFlashTopo, "")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.deleteFlashTopo, "name=missing")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.deleteFlashTopo, "name=topo-new&gradualFlag=bad")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.deleteFlashTopo, "name=topo-new&step=bad")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.deleteFlashTopo, "name=topo-new&gradualFlag=true")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.deleteFlashTopo, "name=topo-new")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	submitFailTopo = NewFlashNodeTopology("topo-rename-fail", proto.DefaultRegion, 89, proto.TopoStatusNormal)
	manager.cluster.flashNodeTopo.Store(submitFailTopo.Name, submitFailTopo)
	failPartition = &apiServiceSubmitPartition{err: fmt.Errorf("submit failed")}
	manager.cluster.partition = failPartition
	manager.cluster.idAlloc.partition = failPartition
	reply = runAPIServiceRequest(t, manager.renameFlashTopo, "name=topo-rename-fail&newName=topo-rename-failed")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	reply = runAPIServiceRequest(t, manager.renameFlashTopo, "")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.renameFlashTopo, "name="+proto.IdleTopoName+"&newName=idle-new")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.renameFlashTopo, "name=topo-a")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.renameFlashTopo, "name=missing&newName=topo-renamed")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.renameFlashTopo, "name=topo-a&newName="+proto.DefaultTopoName)
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.renameFlashTopo, "name=topo-a&newName=topo-renamed")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)
}

func TestFlashGroupManagerSetFlashNodeIOLimits(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	emptyTopo := NewFlashNodeTopology("empty-io", proto.DefaultRegion, 90, proto.TopoStatusNormal)
	manager.cluster.flashNodeTopo.Store(emptyTopo.Name, emptyTopo)
	emptyDefaultTopo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 91, proto.TopoStatusNormal)
	manager.cluster.flashNodeTopo.Store(emptyDefaultTopo.Name, emptyDefaultTopo)

	reply := runAPIServiceRequest(t, manager.setFlashNodeReadIOLimits, "name=empty-io&flow=1&iocc=2&factor=3")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.setFlashNodeReadIOLimits, "flow=1&iocc=2&factor=3")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.setFlashNodeReadIOLimits, "name=missing&flow=1")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.setFlashNodeWriteIOLimits, "name=empty-io&flow=4&iocc=5&factor=6")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.setFlashNodeWriteIOLimits, "flow=4&iocc=5&factor=6")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code, reply.Msg)

	reply = runAPIServiceRequest(t, manager.setFlashNodeWriteIOLimits, "name=missing&flow=1")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestFlashGroupManagerSetConfigSyncsDisablePercentToTopos(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	require.NoError(t, manager.setConfig(cfgMaxDisableFlashGroupPercent, "77"))
	defaultTopo, err := manager.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	require.Equal(t, uint32(77), atomic.LoadUint32(&defaultTopo.maxDisableFlashGroupPercent))
}

func TestParseRequestToUpdateFlashTopo(t *testing.T) {
	args, err := parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet,
		"/?name=topo-a&flashNodeHandleReadTimeout=11&flashNodeReadDataNodeTimeout=12&flashHotKeyMissCount=13"+
			"&flashNodeReadRps=18&flashNodeLruCapacity=100&flashNodeLruFhCapacity=2000"+
			"&flashReadFlowLimit=14&flashWriteFlowLimit=15&flashKeyFlowLimit=16&flashNodeConnectionLimit=17", nil))
	require.NoError(t, err)
	require.Equal(t, "topo-a", args.Name)
	require.Equal(t, 11, *args.FlashNodeHandleReadTimeout)
	require.Equal(t, 12, *args.FlashNodeReadDataNodeTimeout)
	require.Equal(t, 13, *args.FlashHotKeyMissCount)
	require.Equal(t, int64(18), *args.FlashNodeReadRps)
	require.Equal(t, 100, *args.FlashNodeLruCapacity)
	require.Equal(t, 2000, *args.FlashNodeLruFhCapacity)
	require.Equal(t, int64(14), *args.FlashReadFlowLimit)
	require.Equal(t, int64(15), *args.FlashWriteFlowLimit)
	require.Equal(t, int64(16), *args.FlashKeyFlowLimit)
	require.Equal(t, int64(17), *args.FlashNodeConnectionLimit)

	args, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?flashKeyFlowLimit=1", nil))
	require.NoError(t, err)
	require.Equal(t, proto.DefaultTopoName, args.Name)
	require.Equal(t, int64(1), *args.FlashKeyFlowLimit)

	_, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?name=topo-a", nil))
	require.Error(t, err)
	_, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?flashReadFlowLimit=bad", nil))
	require.Error(t, err)
	_, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?flashNodeConnectionLimit=bad", nil))
	require.Error(t, err)
	_, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?flashNodeReadRps=bad", nil))
	require.Error(t, err)
	_, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?flashNodeReadRps=0", nil))
	require.Error(t, err)
	_, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?flashNodeLruCapacity=bad", nil))
	require.Error(t, err)
	_, err = parseRequestToUpdateFlashTopo(httptest.NewRequest(http.MethodGet, "/?flashNodeLruFhCapacity=1000000", nil))
	require.Error(t, err)
}

func TestFlashGroupManagerReadRpsHandlers(t *testing.T) {
	manager := newAPIServiceTestManager(t)
	manager.config.FlashNodeReadRps = 200000
	manager.cluster.cfg.FlashNodeReadRps = 200000

	cv := decodeAPIServiceReplyData[proto.ClusterView](t, runAPIServiceRequest(t, manager.getCluster, ""))
	require.Equal(t, int64(200000), cv.FlashNodeReadRps)

	cfg := decodeAPIServiceReplyData[proto.RemoteCacheConfig](t, runAPIServiceRequest(t, manager.getRemoteCacheConfig, ""))
	require.Equal(t, int64(200000), cfg.FlashNodeReadRps)

	reply := runAPIServiceRequest(t, manager.setNodeInfoHandler, "flashNodeReadRps=160002")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	require.Error(t, manager.setConfig(cfgFlashNodeReadRps, "150001"))

	reply = runAPIServiceRequest(t, manager.updateFlashTopo,
		"name=topo-a&flashNodeReadRps=170003&flashNodeHandleReadTimeout=11&flashNodeReadDataNodeTimeout=12&flashHotKeyMissCount=13&flashReadFlowLimit=14&flashWriteFlowLimit=15&flashKeyFlowLimit=16&flashNodeConnectionLimit=17")
	view := decodeAPIServiceReplyData[proto.FlashTopologyAdminView](t, reply)
	require.Equal(t, int64(170003), view.FlashNodeReadRps)
	topo, err := manager.cluster.PeekFlashTopo("topo-a")
	require.NoError(t, err)
	require.Equal(t, int64(170003), topo.GetHeartbeatConfig().FlashNodeReadRps)

	reply = runAPIServiceRequest(t, manager.updateFlashTopo,
		"name=topo-a&flashNodeLruCapacity=9000&flashNodeLruFhCapacity=8000&flashNodeReadRps=18&flashNodeHandleReadTimeout=11&flashNodeReadDataNodeTimeout=12&flashHotKeyMissCount=13&flashReadFlowLimit=14&flashWriteFlowLimit=15&flashKeyFlowLimit=16&flashNodeConnectionLimit=17")
	view = decodeAPIServiceReplyData[proto.FlashTopologyAdminView](t, reply)
	require.Equal(t, 9000, view.FlashNodeLruCapacity)
	require.Equal(t, 8000, view.FlashNodeLruFhCapacity)
	topo, err = manager.cluster.PeekFlashTopo("topo-a")
	require.NoError(t, err)
	require.Equal(t, 9000, topo.GetHeartbeatConfig().FlashNodeLruCapacity)
	require.Equal(t, 8000, topo.GetHeartbeatConfig().FlashNodeLruFhCapacity)
}

func TestFlashGroupManagerUpdateFlashTopoErrorsBeforeSync(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.updateFlashTopo, "name=topo-a&flashNodeReadRps=18&flashNodeLruCapacity=100&flashNodeLruFhCapacity=2000&flashNodeHandleReadTimeout=11&flashNodeReadDataNodeTimeout=12&flashHotKeyMissCount=13&flashReadFlowLimit=14&flashWriteFlowLimit=15&flashKeyFlowLimit=16&flashNodeConnectionLimit=17")
	view := decodeAPIServiceReplyData[proto.FlashTopologyAdminView](t, reply)
	require.Equal(t, "topo-a", view.Name)
	require.Equal(t, 11, view.FlashNodeHandleReadTimeout)
	require.Equal(t, 12, view.FlashNodeReadDataNodeTimeout)
	require.Equal(t, 13, view.FlashHotKeyMissCount)
	require.Equal(t, int64(18), view.FlashNodeReadRps)
	require.Equal(t, 100, view.FlashNodeLruCapacity)
	require.Equal(t, 2000, view.FlashNodeLruFhCapacity)
	require.Equal(t, int64(14), view.FlashReadFlowLimit)
	require.Equal(t, int64(15), view.FlashWriteFlowLimit)
	require.Equal(t, int64(16), view.FlashKeyFlowLimit)
	require.Equal(t, int64(17), view.FlashNodeConnectionLimit)

	reply = runAPIServiceRequest(t, manager.updateFlashTopo, "name=topo-a")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	reply = runAPIServiceRequest(t, manager.updateFlashTopo, "name=missing&flashKeyFlowLimit=1")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	topo, err := manager.cluster.PeekFlashTopo("topo-a")
	require.NoError(t, err)
	atomic.StoreUint32(&topo.Status, proto.TopoStatusMarkDelete)
	reply = runAPIServiceRequest(t, manager.updateFlashTopo, "name=topo-a&flashKeyFlowLimit=1")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	manager = newAPIServiceTestManager(t)
	failPartition := &apiServiceSubmitPartition{err: fmt.Errorf("submit failed")}
	manager.cluster.partition = failPartition
	manager.cluster.idAlloc.partition = failPartition
	reply = runAPIServiceRequest(t, manager.updateFlashTopo, "name=topo-a&flashKeyFlowLimit=1")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

// --- addFlashGroupSlots handler tests ---

func TestAPIService_addFlashGroupSlots_ParseArgsError(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	// Missing id parameter
	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "slots=300")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)

	// Invalid id parameter (non-numeric)
	reply = runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=abc&slots=300")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestAPIService_addFlashGroupSlots_GetSetSlotsError(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=101&slots=not-a-number")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
	require.Contains(t, reply.Msg, "strconv")
}

func TestAPIService_addFlashGroupSlots_IdleTopo(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	// Use a nonexistent fgID so PeekFlashTopoByFgId fails, falling back to name=idle
	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=9999&slots=300&name=idle")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
	require.Contains(t, reply.Msg, "idle topo doesn't support")
}

func TestAPIService_addFlashGroupSlots_MarkDeletedTopo(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	// Mark the default topo as markDeleted
	topo, err := manager.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	atomic.StoreUint32(&topo.Status, proto.TopoStatusMarkDelete)

	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=101&slots=300")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
	require.Contains(t, reply.Msg, "markDeleted")
}

func TestAPIService_addFlashGroupSlots_Success(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=101&slots=300,400")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code)

	fgView := decodeAPIServiceReplyData[proto.FlashGroupAdminView](t, reply)
	require.Contains(t, fgView.Slots, uint32(300))
	require.Contains(t, fgView.Slots, uint32(400))
}

func TestAPIService_addFlashGroupSlots_NonexistentGroup(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=999&slots=300")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestAPIService_addFlashGroupSlots_SlotConflict(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	// Create a second group in the default topo that owns slot 500
	defaultTopo, err := manager.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	fg2 := newFlashGroup(102, []uint32{500}, proto.SlotStatus_Completed, nil, 1,
		proto.FlashGroupStatus_Active, 1, proto.DefaultTopoName, proto.DefaultRegion)
	defaultTopo.flashGroupMap.Store(fg2.ID, fg2)
	defaultTopo.slotsMap[500] = fg2.ID

	// Try to add slot 500 (owned by group 102) to group 101 — should fail
	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=101&slots=500")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
	require.Contains(t, reply.Msg, "already belongs")
}

func TestAPIService_addFlashGroupSlots_EmptySlots(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=101")
	require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
}

func TestAPIService_addFlashGroupSlots_TopoNameFromFgId(t *testing.T) {
	manager := newAPIServiceTestManager(t)

	// When PeekFlashTopoByFgId succeeds, topoName should be resolved from fgID
	reply := runAPIServiceRequest(t, manager.addFlashGroupSlots, "id=101&slots=300")
	require.EqualValues(t, proto.ErrCodeSuccess, reply.Code)
}
