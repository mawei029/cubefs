package stream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/util/errors"
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

// Mirrors loadInodeInfoTimer branch in Streamer.server() without starting the goroutine.
func applyLoadInodeInfoTick(s *Streamer) {
	if s.client == nil || s.client.loadInodeInfo == nil {
		return
	}
	_, err := s.client.loadInodeInfo(s.inode)
	if err != nil {
		s.markNeedReloadInode()
	} else {
		s.clearNeedReloadInode()
	}
}

func TestLoadInodeInfoTickSuccessClearsReloadFlag(t *testing.T) {
	t.Parallel()
	s := &Streamer{
		inode: 2001,
		client: &ExtentClient{
			loadInodeInfo: func(uint64) (*proto.InodeInfo, error) {
				return &proto.InodeInfo{Inode: 2001}, nil
			},
		},
	}
	s.markNeedReloadInode()
	applyLoadInodeInfoTick(s)
	require.Equal(t, int32(0), atomic.LoadInt32(&s.needReloadInode))
}

func TestLoadInodeInfoTickFailureMarksReloadFlag(t *testing.T) {
	t.Parallel()
	s := &Streamer{
		inode: 2002,
		client: &ExtentClient{
			loadInodeInfo: func(uint64) (*proto.InodeInfo, error) {
				return nil, errors.New("tick load failed")
			},
		},
	}
	applyLoadInodeInfoTick(s)
	require.Equal(t, int32(1), atomic.LoadInt32(&s.needReloadInode))
}

// Mirrors renewalForbiddenMigration branch when openForWrite is true.
func applyRenewalForbiddenMigrationTick(s *Streamer) error {
	if !s.openForWrite {
		return nil
	}
	err := s.client.renewalForbiddenMigration(s.inode)
	if err != nil {
		s.setError()
	}
	return err
}

func TestRenewalForbiddenMigrationTickReadOnlySkips(t *testing.T) {
	t.Parallel()
	var calls int32
	s := &Streamer{
		inode:        2003,
		openForWrite: false,
		client: &ExtentClient{
			renewalForbiddenMigration: func(uint64) error {
				atomic.AddInt32(&calls, 1)
				return nil
			},
		},
	}
	require.NoError(t, applyRenewalForbiddenMigrationTick(s))
	require.Equal(t, int32(0), atomic.LoadInt32(&calls))
}

func TestRenewalForbiddenMigrationTickErrorSetsStreamerError(t *testing.T) {
	t.Parallel()
	s := &Streamer{
		inode:        2004,
		openForWrite: true,
		client: &ExtentClient{
			renewalForbiddenMigration: func(uint64) error {
				return errors.New("renewal failed")
			},
		},
	}
	err := applyRenewalForbiddenMigrationTick(s)
	require.Error(t, err)
	require.Equal(t, int32(StreamerError), atomic.LoadInt32(&s.status))
}
