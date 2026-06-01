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

// mockToolFlashGroupServer creates a mock HTTP server that handles flashGroup requests for the tool CLI.
func mockToolFlashGroupServer() *httptest.Server {
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

// TestToolFlashGroupAddSlotsCmd_NoSlotsParam tests that addSlots command fails when --slots is not provided.
func TestToolFlashGroupAddSlotsCmd_NoSlotsParam(t *testing.T) {
	server := mockToolFlashGroupServer()
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

// TestToolFlashGroupAddSlotsCmd_InvalidID tests that addSlots command fails with invalid flash group ID.
func TestToolFlashGroupAddSlotsCmd_InvalidID(t *testing.T) {
	server := mockToolFlashGroupServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupAddSlots(client)
	err := cmd.ParseFlags([]string{"--slots", "500,600"})
	require.NoError(t, err)

	// Run with non-numeric ID — should fail
	err = cmd.RunE(cmd, []string{"abc"})
	require.Error(t, err)
}

// TestToolFlashGroupAddSlotsCmd_Success tests that addSlots command succeeds with valid parameters.
func TestToolFlashGroupAddSlotsCmd_Success(t *testing.T) {
	server := mockToolFlashGroupServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupAddSlots(client)
	err := cmd.ParseFlags([]string{"--slots", "500,600"})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestToolFlashGroupSuggestSlotsCmd_NoActiveGroups tests suggestSlots when there are no active groups.
func TestToolFlashGroupSuggestSlotsCmd_NoActiveGroups(t *testing.T) {
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

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestToolFlashGroupSuggestSlotsCmd_InvalidID tests suggestSlots with invalid flash group ID.
func TestToolFlashGroupSuggestSlotsCmd_InvalidID(t *testing.T) {
	server := mockToolFlashGroupServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{"abc"})
	require.Error(t, err)
}

// TestToolFlashGroupSuggestSlotsCmd_WithActiveGroups tests suggestSlots with mock groups.
func TestToolFlashGroupSuggestSlotsCmd_WithActiveGroups(t *testing.T) {
	server := mockToolFlashGroupServer()
	defer server.Close()

	client := master.NewMasterClient([]string{server.URL[7:]}, false)
	cmd := newCmdFlashGroupSuggestSlots(client)
	err := cmd.ParseFlags([]string{"--count", "3", "--errorRate", "0.05"})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestToolFlashGroupSuggestSlotsCmd_WithManySlots tests suggestSlots with unbalanced groups to trigger full algorithm.
func TestToolFlashGroupSuggestSlotsCmd_WithManySlots(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
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
	err := cmd.ParseFlags([]string{"--count", "3", "--errorRate", "0.01"})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestToolFlashGroupSuggestSlotsCmd_WrapAroundRange tests suggestSlots with wrap-around slot ranges.
func TestToolFlashGroupSuggestSlotsCmd_WrapAroundRange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == proto.AdminFlashGroupList {
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

	err = cmd.RunE(cmd, []string{"1"})
	require.NoError(t, err)
}

// TestToolFlashGroupSuggestSlotsCmd_InactiveGroupsIgnored tests suggestSlots ignores inactive groups even with many slots.
func TestToolFlashGroupSuggestSlotsCmd_InactiveGroupsIgnored(t *testing.T) {
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

// TestToolFlashGroupListCmd_PrintGroupedSlots tests the list command with printGroupedSlots logic.
func TestToolFlashGroupListCmd_PrintGroupedSlots(t *testing.T) {
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
	err := cmd.ParseFlags([]string{})
	require.NoError(t, err)

	err = cmd.RunE(cmd, []string{})
	require.NoError(t, err)
}
