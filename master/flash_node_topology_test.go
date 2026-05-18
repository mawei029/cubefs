// Copyright 2023 The CFS Authors.
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

package master

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/remotecache/flashgroupmanager"
	"github.com/stretchr/testify/require"
)

func TestFlash(t *testing.T) {
	t.Run("Node", testFlashNode)
	t.Run("Group", testFlashGroup)
	t.Run("Topology", testFlashTopology)
}

func testFlashTopology(t *testing.T) {
	t.Run("Clear", testFlashTopologyClear)
	t.Run("Load", testFlashTopologyLoad)
	t.Run("LoadHeartbeatConfigDefaults", testFlashTopologyLoadHeartbeatConfigDefaults)
	t.Run("RemoveRemoteCacheFlowLimits", testFlashTopologyRemoveRemoteCacheFlowLimits)
}

func testFlashTopologyClear(t *testing.T) {
	groups := createFlashGroups(t)
	_, err := mc.AdminAPI().FlashGroupAddFlashNode(groups[0].ID, 1, testZone1, mfs1Addr)
	require.NoError(t, err)
	_, err = mc.AdminAPI().FlashGroupAddFlashNode(groups[2].ID, 1, testZone2, "")
	require.NoError(t, err)
	server.cluster.fsm.store.Flush()
	server.cluster.flashNodeTopo.Range(func(key, value interface{}) bool {
		if value == nil {
			return true
		}
		topo, ok := value.(*flashgroupmanager.FlashNodeTopology)
		if !ok {
			return true
		}
		topo.Clear()
		return true
	})
	server.cluster.flashNodeTopo = new(sync.Map)
}

func testFlashTopologyLoad(t *testing.T) {
	// After Clear, flashNodeTopo is empty; repopulate from metadata before Load() on each topo.
	require.NoError(t, server.cluster.loadFlashTopos())
	server.cluster.loadFlashNodes()
	server.cluster.loadFlashGroups()
	require.NoError(t, server.cluster.loadFlashTopology())
}

func testFlashTopologyLoadHeartbeatConfigDefaults(t *testing.T) {
	require.NoError(t, server.cluster.loadFlashTopos())
	topo, err := server.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	cfg := topo.GetHeartbeatConfig()
	require.Equal(t, server.cluster.cfg.flashNodeHandleReadTimeout, cfg.FlashNodeHandleReadTimeout)
	require.Equal(t, server.cluster.cfg.flashNodeReadDataNodeTimeout, cfg.FlashNodeReadDataNodeTimeout)
	require.Equal(t, server.cluster.cfg.flashHotKeyMissCount, cfg.FlashHotKeyMissCount)
	require.Equal(t, server.cluster.cfg.flashReadFlowLimit, cfg.FlashReadFlowLimit)
	require.Equal(t, server.cluster.cfg.flashWriteFlowLimit, cfg.FlashWriteFlowLimit)
	require.Equal(t, server.cluster.cfg.flashKeyFlowLimit, cfg.FlashKeyFlowLimit)
	require.Equal(t, server.cluster.cfg.flashNodeConnectionLimit, cfg.FlashNodeConnectionLimit)
}

// testFlashTopologyRemoveRemoteCacheFlowLimits checks that Cluster.removeRemoteCacheFlowLimitsForVol
// clears per-volume entries from all flash topologies while leaving other volumes intact.
func testFlashTopologyRemoveRemoteCacheFlowLimits(t *testing.T) {
	// Subtest "Clear" replaces flashNodeTopo with an empty map; loadFlashTopology() only refreshes
	// existing entries. Reload from metadata store so this case works after Clear/Load.
	require.NoError(t, server.cluster.loadFlashTopos())

	const volA = "tmp_rc_flow_cleanup_a"
	const volB = "tmp_rc_flow_cleanup_b"

	var topoCount int
	server.cluster.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*flashgroupmanager.FlashNodeTopology)
		if !ok || topo == nil {
			return true
		}
		topoCount++
		topo.SetRemoteCacheReadFlow(volA, 100)
		topo.SetRemoteCacheReadFlow(volB, 200)
		topo.SetRemoteCacheWriteFlow(volA, 300)
		topo.SetRemoteCacheWriteFlow(volB, 400)
		return true
	})
	require.Greater(t, topoCount, 0, "need at least one flash topology")

	server.cluster.removeRemoteCacheFlowLimitsForVol("")
	server.cluster.removeRemoteCacheFlowLimitsForVol(volA)

	server.cluster.flashNodeTopo.Range(func(_, value interface{}) bool {
		topo, ok := value.(*flashgroupmanager.FlashNodeTopology)
		require.True(t, ok)
		require.NotNil(t, topo)
		rm := topo.GetRemoteCacheReadFlowMap()
		wm := topo.GetRemoteCacheWriteFlowMap()
		_, hasARead := rm[volA]
		_, hasAWrite := wm[volA]
		require.False(t, hasARead, "read map should drop deleted vol")
		require.False(t, hasAWrite, "write map should drop deleted vol")
		require.EqualValues(t, 200, rm[volB], "other vol read limit preserved")
		require.EqualValues(t, 400, wm[volB], "other vol write limit preserved")
		return true
	})

	server.cluster.removeRemoteCacheFlowLimitsForVol(volB)
}

func TestUpdateFlashTopoHandler(t *testing.T) {
	s := &Server{cluster: &Cluster{
		Name: "test-cluster",
		ClusterFlashTopoSubItem: ClusterFlashTopoSubItem{
			flashNodeTopo: new(sync.Map),
		},
		cfg:       newClusterConfig(),
		partition: &mockPartition{isLeader: true},
	}}
	topo := flashgroupmanager.NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)
	s.cluster.flashNodeTopo.Store(proto.DefaultTopoName, topo)

	t.Run("success", func(t *testing.T) {
		values := url.Values{}
		values.Set(flashNodeHandleReadTimeout, "11")
		values.Set(flashNodeReadDataNodeTimeout, "12")
		values.Set(flashHotKeyMissCount, "13")
		values.Set(flashReadFlowLimit, "14")
		values.Set(flashWriteFlowLimit, "15")
		values.Set(flashKeyFlowLimit, "16")
		values.Set(flashNodeConnectionLimit, "17")

		req := httptest.NewRequest(http.MethodGet, "/?"+values.Encode(), nil)
		reply := decodeReply(t, callHandler(s.updateFlashTopo, req))
		require.EqualValues(t, proto.ErrCodeSuccess, reply.Code)
		cfg := topo.GetHeartbeatConfig()
		require.Equal(t, 11, cfg.FlashNodeHandleReadTimeout)
		require.Equal(t, 12, cfg.FlashNodeReadDataNodeTimeout)
		require.Equal(t, 13, cfg.FlashHotKeyMissCount)
		require.Equal(t, int64(14), cfg.FlashReadFlowLimit)
		require.Equal(t, int64(15), cfg.FlashWriteFlowLimit)
		require.Equal(t, int64(16), cfg.FlashKeyFlowLimit)
		require.Equal(t, int64(17), cfg.FlashNodeConnectionLimit)
	})

	t.Run("param_error", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?name="+proto.DefaultTopoName, nil)
		reply := decodeReply(t, callHandler(s.updateFlashTopo, req))
		require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
	})

	t.Run("not_found", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/?name=missing&"+flashKeyFlowLimit+"=1", nil)
		reply := decodeReply(t, callHandler(s.updateFlashTopo, req))
		require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
	})

	t.Run("mark_deleted", func(t *testing.T) {
		deletedTopo := flashgroupmanager.NewFlashNodeTopology("deleted-topo", proto.DefaultRegion, 2, proto.TopoStatusMarkDelete)
		s.cluster.flashNodeTopo.Store(deletedTopo.Name, deletedTopo)

		req := httptest.NewRequest(http.MethodGet, "/?name=deleted-topo&"+flashKeyFlowLimit+"=1", nil)
		reply := decodeReply(t, callHandler(s.updateFlashTopo, req))
		require.NotEqualValues(t, proto.ErrCodeSuccess, reply.Code)
	})
}
