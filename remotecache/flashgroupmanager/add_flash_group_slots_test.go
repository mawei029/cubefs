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
	"fmt"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddFlashGroupSlots_EmptySlots tests that AddFlashGroupSlots returns an error when setSlots is empty.
func TestAddFlashGroupSlots_EmptySlots(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)
	_, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "slots parameter cannot be empty")
}

// TestAddFlashGroupSlots_NonexistentGroup tests that AddFlashGroupSlots returns an error when the flash group ID doesn't exist.
func TestAddFlashGroupSlots_NonexistentGroup(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)
	_, err := topo.AddFlashGroupSlots(999, tSyncUpdateFlashGroup, []uint32{100})
	require.Error(t, err)
	assert.Equal(t, proto.ErrorNoFlashGroup, err)
}

// TestAddFlashGroupSlots_SlotAlreadyOwnedByAnotherGroup tests that AddFlashGroupSlots returns an error
// when a slot already belongs to another flash group.
func TestAddFlashGroupSlots_SlotAlreadyOwnedByAnotherGroup(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	// Create first group with slot 100
	fg1, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg1)

	// Create second group
	fg2, err := topo.CreateFlashGroup(2, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{200}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg2)

	// Try to add slot 100 (owned by fg1) to fg2 — should fail
	_, err = topo.AddFlashGroupSlots(2, tSyncUpdateFlashGroup, []uint32{100})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already belongs to flashGroup")
}

// TestAddFlashGroupSlots_SlotAlreadyInSameGroup tests that AddFlashGroupSlots succeeds when a slot
// is already in the target group (it should just skip it).
func TestAddFlashGroupSlots_SlotAlreadyInSameGroup(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg1, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100, 200}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg1)

	// Add slot 100 which is already in group 1 — should succeed, all slots already in group
	result, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{100})
	require.NoError(t, err)
	require.NotNil(t, result)
	// Slot list unchanged since 100 was already in group
	assert.Equal(t, []uint32{100, 200}, result.Slots)
}

// TestAddFlashGroupSlots_AllSlotsAlreadyInGroup tests that AddFlashGroupSlots returns the group
// unchanged when all specified slots are already present.
func TestAddFlashGroupSlots_AllSlotsAlreadyInGroup(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100, 200, 300}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg)

	// Try adding all slots already in group
	result, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{100, 200, 300})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, []uint32{100, 200, 300}, result.Slots)
}

// TestAddFlashGroupSlots_Success tests the normal case of adding new slots to a flash group.
func TestAddFlashGroupSlots_Success(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100, 200}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg)

	// Add new slots 300, 400
	result, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{300, 400})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Slots should now be sorted: [100, 200, 300, 400]
	assert.Equal(t, []uint32{100, 200, 300, 400}, result.Slots)

	// Check that the slots map was updated
	assert.Equal(t, uint64(1), topo.slotsMap[300])
	assert.Equal(t, uint64(1), topo.slotsMap[400])
}

// TestAddFlashGroupSlots_MixedNewAndExistingSlots tests adding a mix of new and already-existing slots.
func TestAddFlashGroupSlots_MixedNewAndExistingSlots(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100, 200}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg)

	// Add slot 200 (already present) and 300 (new)
	result, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{200, 300})
	require.NoError(t, err)
	require.NotNil(t, result)

	// 200 is deduplicated, 300 is added; sorted result: [100, 200, 300]
	assert.Equal(t, []uint32{100, 200, 300}, result.Slots)
	assert.Equal(t, uint64(1), topo.slotsMap[300])
}

// TestAddFlashGroupSlots_SyncErrorRollback tests that AddFlashGroupSlots rolls back when the sync function fails.
func TestAddFlashGroupSlots_SyncErrorRollback(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100, 200}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg)

	// Create a sync function that always fails
	failingSyncFunc := func(flashGroup *FlashGroup) error {
		return fmt.Errorf("sync update failed")
	}

	// Try to add slots with failing sync — should rollback
	_, err = topo.AddFlashGroupSlots(1, failingSyncFunc, []uint32{300})
	require.Error(t, err)

	// Verify that the group's slots are unchanged (rolled back)
	// fg is the same pointer stored in flashGroupMap, so rollback should be reflected
	assert.Equal(t, []uint32{100, 200}, fg.Slots)

	// Verify that the new slot was NOT added to slotsMap
	_, slotExists := topo.slotsMap[300]
	assert.False(t, slotExists)
}

// TestAddFlashGroupSlots_DuplicateSlotsInInput tests that duplicate slots in the input are deduplicated.
func TestAddFlashGroupSlots_DuplicateSlotsInInput(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg)

	// Add duplicate slots 300, 300 in the input
	result, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{300, 300})
	require.NoError(t, err)
	require.NotNil(t, result)

	// Only one instance of 300 should be in the slots
	assert.Equal(t, []uint32{100, 300}, result.Slots)
}

// TestAddFlashGroupSlots_AddToMultipleGroups tests that different groups can each add their own slots.
func TestAddFlashGroupSlots_AddToMultipleGroups(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg1, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg1)

	fg2, err := topo.CreateFlashGroup(2, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{200}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg2)

	// Add slot 300 to group 1
	result1, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{300})
	require.NoError(t, err)
	assert.Equal(t, []uint32{100, 300}, result1.Slots)

	// Add slot 400 to group 2
	result2, err := topo.AddFlashGroupSlots(2, tSyncUpdateFlashGroup, []uint32{400})
	require.NoError(t, err)
	assert.Equal(t, []uint32{200, 400}, result2.Slots)

	// Verify ownership in slotsMap
	assert.Equal(t, uint64(1), topo.slotsMap[300])
	assert.Equal(t, uint64(2), topo.slotsMap[400])
}

// TestAddFlashGroupSlots_SlotOwnedBySameGroupSkipped tests that a slot already owned by the
// same group in slotsMap is accepted (not rejected as conflict).
func TestAddFlashGroupSlots_SlotOwnedBySameGroupSkipped(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	fg, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100}, 10, false, 0)
	require.NoError(t, err)
	require.NotNil(t, fg)

	// Slot 100 is already owned by group 1 in slotsMap. Adding 100 again to group 1 should not fail.
	result, err := topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{100, 300})
	require.NoError(t, err)
	assert.Equal(t, []uint32{100, 300}, result.Slots)
}

// TestAddFlashGroupSlots_SlotNotCompleted tests that AddFlashGroupSlots returns an error
// when the flash group's slot status is not Completed (e.g. Creating or Deleting).
func TestAddFlashGroupSlots_SlotNotCompleted(t *testing.T) {
	topo := NewFlashNodeTopology(proto.DefaultTopoName, proto.DefaultRegion, 1, proto.TopoStatusNormal)

	// Create a group with SlotStatus_Creating (gradual create with step=1)
	fg, err := topo.CreateFlashGroup(1, tSyncUpdateFlashGroup, tSyncAddFlashGroup, []uint32{100, 200}, 10, true, 1)
	require.NoError(t, err)
	require.NotNil(t, fg)
	require.Equal(t, proto.SlotStatus_Creating, fg.GetSlotStatus())

	// Try to add slots to a group with SlotStatus_Creating — should fail
	_, err = topo.AddFlashGroupSlots(1, tSyncUpdateFlashGroup, []uint32{300})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "addSlots not allowed")
}

// tSyncAddFlashGroup is a no-op sync function for testing
func tSyncAddFlashGroup(flashGroup *FlashGroup) (err error) {
	return nil
}
