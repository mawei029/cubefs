// Copyright 2026 The CubeFS Authors.
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

package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/master"
	"github.com/stretchr/testify/require"
)

// mockFlashGroupAddSlotsServer creates a mock HTTP server that handles flashGroup addSlots requests.
func mockFlashGroupAddSlotsServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupAddSlots {
			fgView := proto.FlashGroupAdminView{
				ID:    1,
				Slots: []uint32{100, 200, 500, 600},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		if r.URL.Path == proto.AdminFlashGroupList {
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{ID: 1, Status: proto.FlashGroupStatus_Active, Slots: []uint32{100, 2000000000}},
					{ID: 2, Status: proto.FlashGroupStatus_Active, Slots: []uint32{500, 200000000}},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		// Default: return success with empty data
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
}

// TestFlashGroupAddSlotsCmd_NoSlotsParam tests that addSlots command fails when --slots is not provided.
func TestFlashGroupAddSlotsCmd_NoSlotsParam(t *testing.T) {
	server := mockFlashGroupAddSlotsServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupAddSlots(client)
	err := cmd.ParseFlags([]string{})
	require.NoError(t, err)

	// Run without --slots flag — should fail with "param slots is required"
	err = cmd.RunE(cmd, []string{"1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "param slots is required")
}

// TestFlashGroupAddSlotsCmd_InvalidID tests that addSlots command fails with invalid flash group ID.
func TestFlashGroupAddSlotsCmd_InvalidID(t *testing.T) {
	server := mockFlashGroupAddSlotsServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupAddSlots(client)
	err := cmd.ParseFlags([]string{"--slots", "500,600"})
	require.NoError(t, err)

	// Run with non-numeric ID — should fail
	err = cmd.RunE(cmd, []string{"abc"})
	require.Error(t, err)
}

// TestFlashGroupAddSlotsCmd_Success tests that addSlots command succeeds with valid parameters.
func TestFlashGroupAddSlotsCmd_Success(t *testing.T) {
	server := mockFlashGroupAddSlotsServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupAddSlots(client)
	err := cmd.ParseFlags([]string{"--slots", "500,600"})
	require.NoError(t, err)

	// Run with valid ID and slots
	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupAddSlotsCmd_WithTopoName tests that addSlots command works with --topoName flag.
func TestFlashGroupAddSlotsCmd_WithTopoName(t *testing.T) {
	server := mockFlashGroupAddSlotsServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupAddSlots(client)
	err := cmd.ParseFlags([]string{"--slots", "500", "--topoName", proto.DefaultTopoName})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupAddSlotsCmd_EmptyTopoName tests that addSlots command works with empty topoName (uses default).
func TestFlashGroupAddSlotsCmd_EmptyTopoName(t *testing.T) {
	server := mockFlashGroupAddSlotsServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupAddSlots(client)
	err := cmd.ParseFlags([]string{"--slots", "500", "--topoName", ""})
	require.NoError(t, err)

	// When topoName is empty, the code sets it to DefaultTopoName
	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupSuggestSlotsCmd_EmptyTopoName tests suggestSlots with empty topoName (uses default).
// It verifies that when no other active groups can donate slots, the command completes without error.
func TestFlashGroupSuggestSlotsCmd_EmptyTopoName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
			// Return target group with small slots, but no large groups to donate from
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{ID: 1, Status: proto.FlashGroupStatus_Active, Slots: []uint32{1000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
		} else {
			w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
		}
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "3", "--errorRate", "0.05", "--topoName", ""})
	require.NoError(t, err)

	// When topoName is empty, the code sets it to DefaultTopoName
	// With no large groups, it prints "No suitable large flash groups found"
	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupSuggestSlotsCmd_NoActiveGroups tests suggestSlots when there are no groups to donate slots.
func TestFlashGroupSuggestSlotsCmd_NoActiveGroups(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
			// Target group exists but no large groups to donate from
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{ID: 1, Status: proto.FlashGroupStatus_Active, Slots: []uint32{1000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "3", "--errorRate", "0.05"})
	require.NoError(t, err)

	// Should succeed (no active groups, prints a message)
	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupSuggestSlotsCmd_InvalidID tests suggestSlots with invalid flash group ID.
func TestFlashGroupSuggestSlotsCmd_InvalidID(t *testing.T) {
	server := mockFlashGroupAddSlotsServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{})
	require.NoError(t, err)

	// Run with non-numeric ID — should fail
	err = cmd.RunE(cmd, []string{"abc"})
	require.Error(t, err)
}

// TestFlashGroupSuggestSlotsCmd_WithActiveGroups tests suggestSlots with mock groups that have slots.
func TestFlashGroupSuggestSlotsCmd_WithActiveGroups(t *testing.T) {
	server := mockFlashGroupAddSlotsServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "3", "--errorRate", "0.05"})
	require.NoError(t, err)

	// Should succeed and print suggestions
	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupSuggestSlotsCmd_WithManySlots tests suggestSlots with a mock scenario that triggers
// the full suggestion algorithm (groups with unequal slot distribution).
func TestFlashGroupSuggestSlotsCmd_WithManySlots(t *testing.T) {
	// Strategy: create groups where one group has way more slots than others.
	// Use low errorRate so the threshold is close to average, ensuring some groups exceed it.
	// With 3 active groups, avgPct=33.3%, threshold with errorRate=0.01 ≈ 33.6%
	// Group with many slots spread across uint32 space will exceed threshold.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
			// Group 1 (target): 2 slots, small percentage
			// Group 2: many slots spread evenly, large percentage
			// Group 3: many slots spread evenly, large percentage
			// This creates an imbalance where group 1 needs more slots
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{
						ID:             1,
						Status:         proto.FlashGroupStatus_Active,
						Slots:          []uint32{1000000, 2000000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
					{
						ID:             2,
						Status:         proto.FlashGroupStatus_Active,
						Slots:          []uint32{100000000, 200000000, 300000000, 400000000, 500000000, 600000000, 700000000, 800000000, 900000000, 1000000000, 1100000000, 1200000000, 1300000000, 1400000000, 1500000000, 1600000000, 1700000000, 1800000000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
					{
						ID:             3,
						Status:         proto.FlashGroupStatus_Active,
						Slots:          []uint32{2000000000, 2200000000, 2400000000, 2600000000, 2800000000, 3000000000, 3200000000, 3400000000, 3600000000, 3800000000, 4000000000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	// Use low errorRate so threshold is close to average (33.3%), making groups 2 and 3 clearly exceed it
	err := cmd.ParseFlags([]string{"--count", "3", "--errorRate", "0.01"})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupSuggestSlotsCmd_WrapAroundRange tests suggestSlots with a scenario that has
// wrap-around slot ranges (r.end < r.start), which triggers the circular uint32 distance calculation.
func TestFlashGroupSuggestSlotsCmd_WrapAroundRange(t *testing.T) {
	// Create a scenario where a slot range wraps around from near MaxUint32 to near 0.
	// This triggers the r.end < r.start branch (line 467-468).
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
			// Use slots near the uint32 boundary (4294967295 = MaxUint32)
			// to create wrap-around ranges.
			// Group 1 (target): 1 small slot near 0
			// Group 2: 2 slots, one near max uint32 and one in the middle — creates a wrap-around range
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{
						ID:             1,
						Status:         proto.FlashGroupStatus_Active,
						Slots:          []uint32{5000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
					{
						ID:             2,
						Status:         proto.FlashGroupStatus_Active,
						Slots:          []uint32{4294967290, 2000000000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "2", "--errorRate", "0.01"})
	require.NoError(t, err)

	// This should trigger wrap-around calculations
	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestFlashGroupSuggestSlotsCmd_InactiveGroupsIgnored tests suggestSlots ignores inactive groups even with many slots.
func TestFlashGroupSuggestSlotsCmd_InactiveGroupsIgnored(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{
						ID:             1,
						Status:         proto.FlashGroupStatus_Active,
						Slots:          []uint32{1000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
					{
						ID:             2,
						Status:         proto.FlashGroupStatus_Active,
						Slots:          []uint32{1000000000, 2000000000, 3000000000, 4000000000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
					{
						ID:     3,
						Status: proto.FlashGroupStatus_Inactive,
						// Holds a lot of slots which would mess up distances if included
						Slots:          []uint32{100000, 200000, 300000, 400000, 500000, 600000, 700000, 800000, 900000},
						Weight:         1,
						SlotStatus:     proto.SlotStatus_Completed,
						FlashNodeCount: 1,
					},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "1", "--errorRate", "0.01"})
	require.NoError(t, err)

	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)

	w.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	io.Copy(&buf, r)
	output := buf.String()

	require.NotContains(t, output, "No suitable large flash groups found")
	require.Contains(t, output, "Command to execute:")
}

// TestFlashGroupSuggestSlotsCmd_DefaultTopoNameOutput tests that suggestSlots output command
// does NOT include -n when topoName is the default.
func TestFlashGroupSuggestSlotsCmd_DefaultTopoNameOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
			// Reuse data proven to trigger suggestSlots in InactiveGroupsIgnored test
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{ID: 1, Status: proto.FlashGroupStatus_Active, Slots: []uint32{1000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
					{ID: 2, Status: proto.FlashGroupStatus_Active, Slots: []uint32{1000000000, 2000000000, 3000000000, 4000000000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
					{ID: 3, Status: proto.FlashGroupStatus_Inactive, Slots: []uint32{100000, 200000, 300000, 400000, 500000, 600000, 700000, 800000, 900000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "1", "--errorRate", "0.01"})
	require.NoError(t, err)

	oldStdout := os.Stdout
	rPipe, wPipe, _ := os.Pipe()
	os.Stdout = wPipe

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)

	wPipe.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	io.Copy(&buf, rPipe)
	output := buf.String()

	require.Contains(t, output, "cli flashgroup addSlots")
	require.NotContains(t, output, "-n")
}

// TestFlashGroupSuggestSlotsCmd_NonDefaultTopoNameOutput tests that suggestSlots output command
// includes -n when topoName is NOT the default.
func TestFlashGroupSuggestSlotsCmd_NonDefaultTopoNameOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
			// Reuse data proven to trigger suggestSlots
			fgView := proto.FlashGroupsAdminView{
				FlashGroups: []proto.FlashGroupAdminView{
					{ID: 1, Status: proto.FlashGroupStatus_Active, Slots: []uint32{1000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
					{ID: 2, Status: proto.FlashGroupStatus_Active, Slots: []uint32{1000000000, 2000000000, 3000000000, 4000000000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
					{ID: 3, Status: proto.FlashGroupStatus_Inactive, Slots: []uint32{100000, 200000, 300000, 400000, 500000, 600000, 700000, 800000, 900000}, Weight: 1, SlotStatus: proto.SlotStatus_Completed, FlashNodeCount: 1},
				},
			}
			reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
			data, _ := json.Marshal(reply)
			w.Write(data)
			return
		}
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "1", "--errorRate", "0.01", "--topoName", "topo-a"})
	require.NoError(t, err)

	oldStdout := os.Stdout
	rPipe, wPipe, _ := os.Pipe()
	os.Stdout = wPipe

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)

	wPipe.Close()
	os.Stdout = oldStdout

	var buf bytes.Buffer
	io.Copy(&buf, rPipe)
	output := buf.String()

	require.Contains(t, output, "cli flashgroup addSlots")
	require.Contains(t, output, "-n topo-a")
}

// TestFlashGroupListCmd_PercentFlag tests listFlashGroups with --percent flag.
func TestFlashGroupListCmd_PercentFlag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fgView := proto.FlashGroupsAdminView{
			FlashGroups: []proto.FlashGroupAdminView{
				{
					ID: 1, Status: proto.FlashGroupStatus_Active, Slots: []uint32{4294967290, 2000000000},
					ReservedSlots: []uint32{300000000},
				},
				{ID: 2, Status: proto.FlashGroupStatus_Inactive, Slots: []uint32{}},
			},
		}
		reply := &proto.HTTPReply{Code: proto.ErrCodeSuccess, Data: fgView}
		data, _ := json.Marshal(reply)
		w.Write(data)
	}))
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupList(client)
	err := cmd.ParseFlags([]string{"--percent"})
	require.NoError(t, err)
	err = cmd.RunE(cmd, []string{})
	require.NoError(t, err)
}
