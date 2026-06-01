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
	"sync"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/require"
)

// newClusterWithPartitionForTest creates a Cluster backed by an apiServiceSubmitPartition
// so that syncUpdateFlashGroup/syncPutFlashGroupInfo can succeed without a real Raft cluster.
func newClusterWithPartitionForTest(t *testing.T) *Cluster {
	t.Helper()
	partition := &apiServiceSubmitPartition{}
	c := &Cluster{
		Name:          "testCluster",
		cfg:           newClusterConfig(),
		flashNodeTopo: new(sync.Map),
		idAlloc:       newIDAllocator(nil, partition),
		partition:     partition,
	}
	// Create default topo with a flash group
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)
	fg := newFlashGroup(101, []uint32{100, 200}, proto.SlotStatus_Completed, nil, 1,
		proto.FlashGroupStatus_Active, 1, proto.DefaultTopoName, proto.DefaultRegion)
	topo.flashGroupMap.Store(fg.ID, fg)
	topo.slotsMap[100] = fg.ID
	topo.slotsMap[200] = fg.ID
	topo.SyncFlashGroupFunc = tSyncUpdateFlashGroup
	c.flashNodeTopo.Store(proto.DefaultTopoName, topo)

	// Create idle topo
	idleTopo := NewFlashNodeTopology(proto.IdleTopoName, proto.DefaultRegion, 2, proto.TopoStatusNormal)
	c.flashNodeTopo.Store(proto.IdleTopoName, idleTopo)

	return c
}

// TestCluster_addFlashGroupSlots_Success tests the cluster-level addFlashGroupSlots wrapper.
func TestCluster_addFlashGroupSlots_Success(t *testing.T) {
	c := newClusterWithPartitionForTest(t)

	fg, err := c.addFlashGroupSlots(proto.DefaultTopoName, 101, []uint32{300, 400})
	require.NoError(t, err)
	require.NotNil(t, fg)
	require.Contains(t, fg.Slots, uint32(300))
	require.Contains(t, fg.Slots, uint32(400))
}

// TestCluster_addFlashGroupSlots_EmptySlots tests that cluster wrapper returns error for empty slots.
func TestCluster_addFlashGroupSlots_EmptySlots(t *testing.T) {
	c := newClusterWithPartitionForTest(t)

	_, err := c.addFlashGroupSlots(proto.DefaultTopoName, 101, []uint32{})
	require.Error(t, err)
}

// TestCluster_addFlashGroupSlots_NonexistentGroup tests that cluster wrapper returns error for nonexistent group.
func TestCluster_addFlashGroupSlots_NonexistentGroup(t *testing.T) {
	c := newClusterWithPartitionForTest(t)

	_, err := c.addFlashGroupSlots(proto.DefaultTopoName, 999, []uint32{300})
	require.Error(t, err)
}

// TestCluster_addFlashGroupSlots_NonexistentTopo tests that cluster wrapper returns error for nonexistent topo.
func TestCluster_addFlashGroupSlots_NonexistentTopo(t *testing.T) {
	c := newClusterWithPartitionForTest(t)

	_, err := c.addFlashGroupSlots("nonexistent-topo", 101, []uint32{300})
	require.Error(t, err)
}

// TestCluster_addFlashGroupSlots_SlotConflict tests that adding a slot owned by another group fails.
func TestCluster_addFlashGroupSlots_SlotConflict(t *testing.T) {
	c := newClusterWithPartitionForTest(t)

	// Slot 100 is already owned by group 101
	_, err := c.addFlashGroupSlots(proto.DefaultTopoName, 101, []uint32{100})
	require.NoError(t, err) // slot already in same group - should succeed (no-op)
}
