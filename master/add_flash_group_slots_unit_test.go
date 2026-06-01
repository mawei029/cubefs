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
// WITHOUT WARRANTIES OR CONDITIONS of ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package master

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/remotecache/flashgroupmanager"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAddSlotsTestServer creates a minimal Server+Cluster for addFlashGroupSlots unit tests.
// No TestMain or full server setup is needed.
func newAddSlotsTestServer(t *testing.T) *Server {
	t.Helper()

	partition := &mockPartition{isLeader: true}
	cluster := &Cluster{
		Name:      "test-cluster",
		partition: partition,
	}
	cluster.flashNodeTopo = new(sync.Map)

	// Create default topology with one flash group (id=1, slots=[100,200])
	defaultTopo := flashgroupmanager.NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)
	fgv := &flashgroupmanager.FlashGroupValue{
		ID:                1,
		Slots:             []uint32{100, 200},
		SlotStatus:        proto.SlotStatus_Completed,
		PendingSlots:      []uint32{},
		ReservedSlots:     []uint32{},
		Step:              1,
		Weight:            1,
		Status:            proto.FlashGroupStatus_Active,
		FlashNodeTopoName: proto.DefaultTopoName,
		Region:            proto.DefaultRegion,
	}
	fg := flashgroupmanager.NewFlashGroupFromFgv(fgv)
	require.NoError(t, defaultTopo.SaveFlashGroup(fg))
	defaultTopo.SyncFlashGroupFunc = func(fg *flashgroupmanager.FlashGroup) error { return nil }
	cluster.flashNodeTopo.Store(proto.DefaultTopoName, defaultTopo)

	// Create idle topology
	idleTopo := flashgroupmanager.NewFlashNodeTopology(proto.IdleTopoName, proto.DefaultRegion, 2, proto.TopoStatusNormal)
	cluster.flashNodeTopo.Store(proto.IdleTopoName, idleTopo)

	return &Server{cluster: cluster}
}

// callAddSlotsHandler invokes the addFlashGroupSlots handler and returns the response.
func callAddSlotsHandler(t *testing.T, srv *Server, rawQuery string) *proto.HTTPReply {
	t.Helper()

	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1/admin?"+rawQuery, nil)
	w := httptest.NewRecorder()
	srv.addFlashGroupSlots(w, r)

	reply := &proto.HTTPReply{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), reply))
	return reply
}

// TestAddFlashGroupSlotsUnit_ParseArgsError tests that addFlashGroupSlots returns an error
// when the id parameter is missing or invalid.
func TestAddFlashGroupSlotsUnit_ParseArgsError(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	// Missing id parameter
	reply := callAddSlotsHandler(t, srv, "slots=300")
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)

	// Invalid id (non-numeric)
	reply = callAddSlotsHandler(t, srv, "id=abc&slots=300")
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsUnit_GetSetSlotsError tests that addFlashGroupSlots returns an error
// when the slots parameter is invalid.
func TestAddFlashGroupSlotsUnit_GetSetSlotsError(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	reply := callAddSlotsHandler(t, srv, "id=1&slots=not-a-number")
	require.Equal(t, int32(proto.ErrCodeParamError), reply.Code)
}

// TestAddFlashGroupSlotsUnit_IdleTopoName tests that addFlashGroupSlots returns an error
// when the resolved topoName is IdleTopoName.
func TestAddFlashGroupSlotsUnit_IdleTopoName(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	// Use a nonexistent fgID so PeekFlashTopoByFgId fails, falling back to name=idle
	reply := callAddSlotsHandler(t, srv, "id=999&slots=300&name=idle")
	require.Equal(t, int32(proto.ErrCodeParamError), reply.Code)
	assert.Contains(t, reply.Msg, "idle topo")
}

// TestAddFlashGroupSlotsUnit_MarkDeletedTopo tests that addFlashGroupSlots returns an error
// when the topology is markDeleted.
func TestAddFlashGroupSlotsUnit_MarkDeletedTopo(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	// Mark the default topo as markDeleted
	topo, err := srv.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	atomic.StoreUint32(&topo.Status, proto.TopoStatusMarkDelete)

	reply := callAddSlotsHandler(t, srv, "id=1&slots=300")
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
	assert.Contains(t, reply.Msg, "markDeleted")
}

// TestAddFlashGroupSlotsUnit_Success tests the successful case.
func TestAddFlashGroupSlotsUnit_Success(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	reply := callAddSlotsHandler(t, srv, "id=1&slots=500,600")
	require.Equal(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsUnit_NonexistentGroup tests that addFlashGroupSlots returns an error
// for a nonexistent flash group.
func TestAddFlashGroupSlotsUnit_NonexistentGroup(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	reply := callAddSlotsHandler(t, srv, "id=9999&slots=300")
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsUnit_SlotConflict tests that adding a slot owned by another group fails.
func TestAddFlashGroupSlotsUnit_SlotConflict(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	// Create a second group (id=2) owning slot 500
	topo, err := srv.cluster.PeekFlashTopo(proto.DefaultTopoName)
	require.NoError(t, err)
	fgv2 := &flashgroupmanager.FlashGroupValue{
		ID:                2,
		Slots:             []uint32{500},
		SlotStatus:        proto.SlotStatus_Completed,
		PendingSlots:      []uint32{},
		ReservedSlots:     []uint32{},
		Step:              1,
		Weight:            1,
		Status:            proto.FlashGroupStatus_Active,
		FlashNodeTopoName: proto.DefaultTopoName,
		Region:            proto.DefaultRegion,
	}
	fg2 := flashgroupmanager.NewFlashGroupFromFgv(fgv2)
	require.NoError(t, topo.SaveFlashGroup(fg2))

	// Try to add slot 500 to group 1 — should fail (already owned by group 2)
	reply := callAddSlotsHandler(t, srv, "id=1&slots=500")
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsUnit_EmptySlots tests that addFlashGroupSlots returns an error
// when no slots parameter is provided.
func TestAddFlashGroupSlotsUnit_EmptySlots(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	reply := callAddSlotsHandler(t, srv, "id=1")
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsUnit_TopoNameResolved tests that when PeekFlashTopoByFgId succeeds,
// the topoName is resolved from the group ID.
func TestAddFlashGroupSlotsUnit_TopoNameResolved(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	// Group 1 is in the default topo; PeekFlashTopoByFgId should resolve it
	reply := callAddSlotsHandler(t, srv, "id=1&slots=300")
	require.Equal(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestClusterAddFlashGroupSlotsUnit_Success tests the Cluster-level addFlashGroupSlots method.
func TestClusterAddFlashGroupSlotsUnit_Success(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	fg, err := srv.cluster.addFlashGroupSlots(1, []uint32{300, 400}, proto.DefaultTopoName)
	require.NoError(t, err)
	require.NotNil(t, fg)
	require.Contains(t, fg.GetSlots(), uint32(300))
	require.Contains(t, fg.GetSlots(), uint32(400))
}

// TestClusterAddFlashGroupSlotsUnit_NonexistentTopo tests that addFlashGroupSlots returns an error
// for a nonexistent topology.
func TestClusterAddFlashGroupSlotsUnit_NonexistentTopo(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	_, err := srv.cluster.addFlashGroupSlots(1, []uint32{300}, "nonexistent-topo")
	require.Error(t, err)
}

// TestClusterAddFlashGroupSlotsUnit_NonexistentGroup tests that addFlashGroupSlots returns an error
// for a nonexistent flash group.
func TestClusterAddFlashGroupSlotsUnit_NonexistentGroup(t *testing.T) {
	srv := newAddSlotsTestServer(t)

	_, err := srv.cluster.addFlashGroupSlots(999, []uint32{300}, proto.DefaultTopoName)
	require.Error(t, err)
}
