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

package master

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/require"
)

func mockSDKFlashGroupAddSlotsServer() *httptest.Server {
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
		w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	}))
}

func TestAdminAPI_FlashGroupAddSlots(t *testing.T) {
	server := mockSDKFlashGroupAddSlotsServer()
	defer server.Close()

	client := NewMasterClient([]string{server.URL[7:]}, false)
	api := &AdminAPI{mc: client}

	fgView, err := api.FlashGroupAddSlots(1, "500,600")
	require.NoError(t, err)
	require.Equal(t, uint64(1), fgView.ID)
	require.Contains(t, fgView.Slots, uint32(500))
	require.Contains(t, fgView.Slots, uint32(600))
}

func TestAdminAPI_FlashGroupAddSlotsByName(t *testing.T) {
	server := mockSDKFlashGroupAddSlotsServer()
	defer server.Close()

	client := NewMasterClient([]string{server.URL[7:]}, false)
	api := &AdminAPI{mc: client}

	fgView, err := api.FlashGroupAddSlotsByName(proto.DefaultTopoName, 1, "500,600")
	require.NoError(t, err)
	require.Equal(t, uint64(1), fgView.ID)
	require.Contains(t, fgView.Slots, uint32(500))
	require.Contains(t, fgView.Slots, uint32(600))
}
