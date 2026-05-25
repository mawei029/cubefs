package stream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/stretchr/testify/require"
)

func TestStreamer_evict_blockedByPendingRequest(t *testing.T) {
	ec := &ExtentClient{
		streamers: make(map[uint64]*Streamer),
	}
	s := &Streamer{
		client:  ec,
		inode:   42,
		refcnt:  0,
		request: make(chan interface{}, 2),
	}
	s.request <- &OpenRequest{done: make(chan struct{})}

	err := s.evict()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "refcnt(0)"))
	require.False(t, strings.Contains(err.Error(), "requestQLen"))
}

func TestNewExtentClientCopiesUpdateInodeMetaOnOverwrite(t *testing.T) {
	master := newExtentClientTestMaster(t)
	defer master.Close()

	client, err := NewExtentClient(&ExtentConfig{
		Volume:                     "testvol",
		Masters:                    []string{strings.TrimPrefix(master.URL, "http://")},
		VolStorageClass:            proto.StorageClass_Replica_HDD,
		VolAllowedStorageClass:     []uint32{proto.StorageClass_Replica_HDD},
		UpdateInodeMetaOnOverwrite: true,
		NeedRemoteCache:            false,
	})
	require.NoError(t, err)
	defer client.Close()
	require.True(t, client.updateInodeMetaOnOverwrite)
}

func TestStreamerDoOverwriteSkipsInodeMetaUpdateWhenDisabled(t *testing.T) {
	s := &Streamer{
		client: &ExtentClient{
			updateInodeMetaOnOverwrite: false,
		},
		inode:     1,
		extents:   NewExtentCache(1),
		dirtylist: NewDirtyExtentList(),
	}

	_, err := s.doOverwrite(NewExtentRequest(0, 1, nil, nil), false, 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "extent key not exist")
}

func newExtentClientTestMaster(t *testing.T) *httptest.Server {
	t.Helper()

	writeReply := func(w http.ResponseWriter, data interface{}) {
		payload, err := json.Marshal(data)
		require.NoError(t, err)
		reply, err := json.Marshal(proto.HTTPReplyRaw{
			Code: proto.ErrCodeSuccess,
			Data: payload,
		})
		require.NoError(t, err)
		_, err = w.Write(reply)
		require.NoError(t, err)
	}

	handler := http.NewServeMux()
	handler.HandleFunc(proto.AdminGetIP, func(w http.ResponseWriter, r *http.Request) {
		writeReply(w, &proto.ClusterInfo{Cluster: "test-cluster", Ip: "127.0.0.1"})
	})
	handler.HandleFunc(proto.AdminGetVol, func(w http.ResponseWriter, r *http.Request) {
		writeReply(w, &proto.SimpleVolView{
			Name:                "testvol",
			Status:              proto.VolStatusNormal,
			VolStorageClass:     proto.StorageClass_Replica_HDD,
			AllowedStorageClass: []uint32{proto.StorageClass_Replica_HDD},
		})
	})
	handler.HandleFunc(proto.QosUpload, func(w http.ResponseWriter, r *http.Request) {
		writeReply(w, &proto.LimitRsp2Client{ID: 1})
	})
	handler.HandleFunc(proto.ClientDataPartitions, func(w http.ResponseWriter, r *http.Request) {
		writeReply(w, proto.NewDataPartitionsView())
	})
	handler.HandleFunc(proto.AdminGetCluster, func(w http.ResponseWriter, r *http.Request) {
		writeReply(w, &proto.ClusterView{
			DataNodes: []proto.NodeView{{Addr: "127.0.0.1:9000", Status: true}},
		})
	})

	return httptest.NewServer(handler)
}
