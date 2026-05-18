// Copyright 2026 The CFS Authors.
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

package flashgroupmanager

import (
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/require"
)

func TestFlashNodeTopology_DeleteRemoteCacheFlowsForVol(t *testing.T) {
	t.Run("emptyVolNameNoOp", func(t *testing.T) {
		topo := NewFlashNodeTopology("t", proto.DefaultRegion, 1, proto.TopoStatusNormal)
		topo.SetRemoteCacheReadFlow("v1", 10)
		topo.SetRemoteCacheWriteFlow("v1", 20)
		require.False(t, topo.DeleteRemoteCacheFlowsForVol(""))
		require.Contains(t, topo.GetRemoteCacheReadFlowMap(), "v1")
		require.Contains(t, topo.GetRemoteCacheWriteFlowMap(), "v1")
	})

	t.Run("removesReadOnly", func(t *testing.T) {
		topo := NewFlashNodeTopology("t", proto.DefaultRegion, 1, proto.TopoStatusNormal)
		topo.SetRemoteCacheReadFlow("onlyRead", 100)
		require.True(t, topo.DeleteRemoteCacheFlowsForVol("onlyRead"))
		require.Empty(t, topo.GetRemoteCacheReadFlowMap())
		require.Empty(t, topo.GetRemoteCacheWriteFlowMap())
	})

	t.Run("removesWriteOnly", func(t *testing.T) {
		topo := NewFlashNodeTopology("t", proto.DefaultRegion, 1, proto.TopoStatusNormal)
		topo.SetRemoteCacheWriteFlow("onlyWrite", 200)
		require.True(t, topo.DeleteRemoteCacheFlowsForVol("onlyWrite"))
		require.Empty(t, topo.GetRemoteCacheReadFlowMap())
		require.Empty(t, topo.GetRemoteCacheWriteFlowMap())
	})

	t.Run("removesBothMaps", func(t *testing.T) {
		topo := NewFlashNodeTopology("t", proto.DefaultRegion, 1, proto.TopoStatusNormal)
		topo.SetRemoteCacheReadFlow("x", 1)
		topo.SetRemoteCacheWriteFlow("x", 2)
		require.True(t, topo.DeleteRemoteCacheFlowsForVol("x"))
		_, okR := topo.GetRemoteCacheReadFlowMap()["x"]
		_, okW := topo.GetRemoteCacheWriteFlowMap()["x"]
		require.False(t, okR)
		require.False(t, okW)
	})

	t.Run("unknownVolReturnsFalse", func(t *testing.T) {
		topo := NewFlashNodeTopology("t", proto.DefaultRegion, 1, proto.TopoStatusNormal)
		topo.SetRemoteCacheReadFlow("keep", 5)
		require.False(t, topo.DeleteRemoteCacheFlowsForVol("nosuch"))
		require.EqualValues(t, 5, topo.GetRemoteCacheReadFlowMap()["keep"])
	})

	t.Run("nilMapsSafe", func(t *testing.T) {
		topo := &FlashNodeTopology{}
		topo.RemoteCacheReadFlowMap = nil
		topo.RemoteCacheWriteFlowMap = nil
		require.False(t, topo.DeleteRemoteCacheFlowsForVol("any"))
	})
}

func TestFlashNodeTopology_CreateFlashNodeHeartBeatTasksUsesTopoConfig(t *testing.T) {
	topoA := NewFlashNodeTopology("topo-a", proto.DefaultRegion, 1, proto.TopoStatusNormal)
	topoA.SetHeartbeatConfig(FlashNodeHeartbeatConfig{
		FlashNodeHandleReadTimeout:   101,
		FlashNodeReadDataNodeTimeout: 201,
		FlashHotKeyMissCount:         301,
		FlashReadFlowLimit:           401,
		FlashWriteFlowLimit:          501,
		FlashKeyFlowLimit:            0,
		FlashNodeConnectionLimit:     601,
	})
	topoA.PutZoneIfAbsent(NewFlashNodeZone("zone-a"))
	nodeA := NewFlashNode("127.0.0.1:10001", "zone-a", "c1", "v1", "topo-a", proto.DefaultRegion, true)
	require.NoError(t, topoA.PutFlashNode(nodeA))

	topoB := NewFlashNodeTopology("topo-b", proto.DefaultRegion, 2, proto.TopoStatusNormal)
	topoB.SetHeartbeatConfig(FlashNodeHeartbeatConfig{
		FlashNodeHandleReadTimeout:   102,
		FlashNodeReadDataNodeTimeout: 202,
		FlashHotKeyMissCount:         302,
		FlashReadFlowLimit:           402,
		FlashWriteFlowLimit:          502,
		FlashKeyFlowLimit:            1,
		FlashNodeConnectionLimit:     602,
	})
	topoB.PutZoneIfAbsent(NewFlashNodeZone("zone-b"))
	nodeB := NewFlashNode("127.0.0.1:10002", "zone-b", "c1", "v1", "topo-b", proto.DefaultRegion, true)
	require.NoError(t, topoB.PutFlashNode(nodeB))

	tasksA := topoA.CreateFlashNodeHeartBeatTasks("leader-a", nil, nil, nil)
	tasksB := topoB.CreateFlashNodeHeartBeatTasks("leader-b", nil, nil, nil)
	require.Len(t, tasksA, 1)
	require.Len(t, tasksB, 1)

	reqA, ok := tasksA[0].Request.(*proto.HeartBeatRequest)
	require.True(t, ok)
	reqB, ok := tasksB[0].Request.(*proto.HeartBeatRequest)
	require.True(t, ok)

	require.Equal(t, 101, reqA.FlashNodeHandleReadTimeout)
	require.Equal(t, int64(401), reqA.FlashReadFlowLimit)
	require.Equal(t, int64(0), reqA.FlashKeyFlowLimit)
	require.Equal(t, int64(601), reqA.FlashNodeConnectionLimit)
	require.Equal(t, "topo-a", reqA.TopoName)

	require.Equal(t, 102, reqB.FlashNodeHandleReadTimeout)
	require.Equal(t, int64(402), reqB.FlashReadFlowLimit)
	require.Equal(t, int64(1), reqB.FlashKeyFlowLimit)
	require.Equal(t, int64(602), reqB.FlashNodeConnectionLimit)
	require.Equal(t, "topo-b", reqB.TopoName)
}
