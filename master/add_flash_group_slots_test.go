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

package master

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAddFlashGroupSlotsHandler_ParseArgsError tests that addFlashGroupSlots returns an error
// when the id parameter is missing or invalid.
func TestAddFlashGroupSlotsHandler_ParseArgsError(t *testing.T) {
	// Missing id parameter — parseArgs should fail
	r, _ := http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?slots=100", nil)
	w := callHandler(server.addFlashGroupSlots, r)
	reply := decodeReply(t, w)
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)

	// Invalid id parameter (non-numeric)
	r, _ = http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id=abc&slots=100", nil)
	w = callHandler(server.addFlashGroupSlots, r)
	reply = decodeReply(t, w)
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsHandler_GetSetSlotsError tests that addFlashGroupSlots returns an error
// when the slots parameter is invalid (non-numeric).
func TestAddFlashGroupSlotsHandler_GetSetSlotsError(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id=1&slots=abc", nil)
	w := callHandler(server.addFlashGroupSlots, r)
	reply := decodeReply(t, w)
	require.Equal(t, int32(proto.ErrCodeParamError), reply.Code)
}

// TestAddFlashGroupSlotsHandler_IdleTopoName tests that addFlashGroupSlots returns an error
// when topoName resolves to "idle".
func TestAddFlashGroupSlotsHandler_IdleTopoName(t *testing.T) {
	// Use a nonexistent fgID so PeekFlashTopoByFgId fails, falling back to name=idle
	r, _ := http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id=999999&slots=100&name="+proto.IdleTopoName, nil)
	w := callHandler(server.addFlashGroupSlots, r)
	reply := decodeReply(t, w)
	require.Equal(t, int32(proto.ErrCodeParamError), reply.Code)
	assert.Contains(t, reply.Msg, "idle topo doesn't support")
}

// TestAddFlashGroupSlotsHandler_NonexistentGroup tests that addFlashGroupSlots returns an error
// when the flash group ID doesn't exist.
func TestAddFlashGroupSlotsHandler_NonexistentGroup(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id=999999&slots=100", nil)
	w := callHandler(server.addFlashGroupSlots, r)
	reply := decodeReply(t, w)
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsHandler_Success tests the successful case via HTTP handler.
func TestAddFlashGroupSlotsHandler_Success(t *testing.T) {
	groups := createFlashGroups(t)
	defer removeFlashGroups(t, groups)
	g := groups[0]

	r, _ := http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id="+uint64ToStr(g.ID)+"&slots=500,600", nil)
	w := callHandler(server.addFlashGroupSlots, r)
	reply := decodeReply(t, w)
	require.Equal(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsHandler_SlotConflict tests that adding a slot belonging to another group fails.
func TestAddFlashGroupSlotsHandler_SlotConflict(t *testing.T) {
	groups := createFlashGroups(t)
	defer removeFlashGroups(t, groups)

	// Add slot 500 to group 0
	g := groups[0]
	r, _ := http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id="+uint64ToStr(g.ID)+"&slots=500", nil)
	w := callHandler(server.addFlashGroupSlots, r)
	reply := decodeReply(t, w)
	require.Equal(t, int32(proto.ErrCodeSuccess), reply.Code)

	// Try to add same slot 500 to group 1 — should fail
	r, _ = http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id="+uint64ToStr(groups[1].ID)+"&slots=500", nil)
	w = callHandler(server.addFlashGroupSlots, r)
	reply = decodeReply(t, w)
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

// TestAddFlashGroupSlotsHandler_EmptySlots tests that addFlashGroupSlots returns an error
// when no slots parameter is provided (empty slot list).
func TestAddFlashGroupSlotsHandler_EmptySlots(t *testing.T) {
	groups := createFlashGroups(t)
	defer removeFlashGroups(t, groups)

	// No slots param — getSetSlots returns empty, AddFlashGroupSlots returns error
	r, _ := http.NewRequest(http.MethodGet, proto.AdminFlashGroupAddSlots+"?id="+uint64ToStr(groups[0].ID), nil)
	w := callHandler(server.addFlashGroupSlots, r)
	reply := decodeReply(t, w)
	require.NotEqual(t, int32(proto.ErrCodeSuccess), reply.Code)
}

func uint64ToStr(v uint64) string {
	return fmt.Sprintf("%d", v)
}
