package stream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubefs/cubefs/proto"
	"github.com/cubefs/cubefs/sdk/data/wrapper"
	"github.com/cubefs/cubefs/sdk/remotecache"
)

func newBackupTestExtentClient(primary, backup *remotecache.RemoteCacheClient, backupTopo string) *ExtentClient {
	return &ExtentClient{
		volumeName: "testvol",
		extentConfig: &ExtentConfig{
			RemoteCacheName: "primary-topo",
			HDDAccCache:     backupTopo,
		},
		RemoteCache: RemoteCache{
			remoteCacheClient: primary,
			hddAccCacheClient: backup,
		},
		dataWrapper: &wrapper.Wrapper{VolName: "testvol"},
	}
}

func newBackupTestStreamer(primary, backup *remotecache.RemoteCacheClient, backupTopo string) *Streamer {
	return &Streamer{
		inode:  42,
		client: newBackupTestExtentClient(primary, backup, backupTopo),
	}
}

func writeMockMasterReply(w http.ResponseWriter, data interface{}) {
	payload, _ := json.Marshal(data)
	rsp, _ := json.Marshal(proto.HTTPReplyRaw{Code: proto.ErrCodeSuccess, Data: payload})
	_, _ = w.Write(rsp)
}

func newTestRemoteCacheClient(t *testing.T, topoName string) *remotecache.RemoteCacheClient {
	t.Helper()
	if proto.Buffers == nil {
		proto.InitBufferPool(32768)
	}
	handler := http.NewServeMux()
	handler.HandleFunc(proto.ClientFlashGroups, func(w http.ResponseWriter, r *http.Request) {
		writeMockMasterReply(w, &proto.FlashGroupView{Enable: true, FlashGroups: []*proto.FlashGroupInfo{}})
	})
	handler.HandleFunc(proto.AdminGetRemoteCacheConfig, func(w http.ResponseWriter, r *http.Request) {
		writeMockMasterReply(w, &proto.RemoteCacheConfig{RemoteCacheTTL: 3600, RemoteCacheReadTimeout: 1000})
	})
	mockMaster := httptest.NewServer(handler)
	t.Cleanup(mockMaster.Close)

	rc, err := remotecache.NewRemoteCacheClient(&remotecache.ClientConfig{
		Masters:         []string{strings.TrimPrefix(mockMaster.URL, "http://")},
		BlockSize:       proto.CACHE_BLOCK_SIZE,
		FromFuse:        true,
		RemoteCacheName: topoName,
	})
	if err != nil {
		t.Fatalf("NewRemoteCacheClient: %v", err)
	}
	rc.SetClusterEnable(true)
	t.Cleanup(func() { rc.Stop() })
	return rc
}

func holeOnlyCacheReadRequests(size uint64) []*remotecache.CacheReadRequest {
	return []*remotecache.CacheReadRequest{{
		CacheReadRequest: proto.CacheReadRequest{
			CacheRequest: &proto.CacheRequest{},
			Size_:        size,
		},
	}}
}

func TestHasHDDAccCache(t *testing.T) {
	backupRC := newTestRemoteCacheClient(t, "backup-topo")

	tests := []struct {
		name     string
		cfg      *ExtentConfig
		backup   *remotecache.RemoteCacheClient
		backupOn bool
		want     bool
	}{
		{
			name:   "no config name",
			cfg:    &ExtentConfig{},
			backup: backupRC,
			want:   false,
		},
		{
			name:   "nil extent config",
			cfg:    nil,
			backup: backupRC,
			want:   false,
		},
		{
			name:   "backup client nil",
			cfg:    &ExtentConfig{HDDAccCache: "private-topo"},
			backup: nil,
			want:   false,
		},
		{
			name:     "backup disabled cluster",
			cfg:      &ExtentConfig{HDDAccCache: "private-topo"},
			backup:   backupRC,
			backupOn: false,
			want:     false,
		},
		{
			name:     "backup ready",
			cfg:      &ExtentConfig{HDDAccCache: "private-topo"},
			backup:   backupRC,
			backupOn: true,
			want:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &ExtentClient{
				extentConfig: tc.cfg,
				RemoteCache: RemoteCache{
					hddAccCacheClient: tc.backup,
				},
			}
			if tc.backup != nil {
				tc.backup.SetClusterEnable(tc.backupOn)
			}
			if got := client.HasHDDAccCache(); got != tc.want {
				t.Fatalf("HasHDDAccCache() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldTryHDDAccCache(t *testing.T) {
	backupRC := newTestRemoteCacheClient(t, "backup-topo")
	s := newBackupTestStreamer(nil, backupRC, "private-topo")

	tests := []struct {
		name          string
		backupTopo    string
		backup        *remotecache.RemoteCacheClient
		backupEnabled bool
		storageClass  uint32
		want          bool
	}{
		{
			name:         "no backup topo configured",
			backupTopo:   "",
			backup:       backupRC,
			storageClass: proto.StorageClass_Replica_HDD,
			want:         false,
		},
		{
			name:          "ssd skips backup",
			backupTopo:    "private-topo",
			backup:        backupRC,
			backupEnabled: true,
			storageClass:  proto.StorageClass_Replica_SSD,
			want:          false,
		},
		{
			name:          "hdd uses backup when available",
			backupTopo:    "private-topo",
			backup:        backupRC,
			backupEnabled: true,
			storageClass:  proto.StorageClass_Replica_HDD,
			want:          true,
		},
		{
			name:          "backup cluster disabled",
			backupTopo:    "private-topo",
			backup:        backupRC,
			backupEnabled: false,
			storageClass:  proto.StorageClass_Replica_HDD,
			want:          false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s.client.extentConfig = &ExtentConfig{HDDAccCache: tc.backupTopo}
			s.client.RemoteCache.hddAccCacheClient = tc.backup
			if tc.backup != nil {
				tc.backup.SetClusterEnable(tc.backupEnabled)
			}
			if got := s.shouldTryHDDAccCache(tc.storageClass); got != tc.want {
				t.Fatalf("shouldTryHDDAccCache() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGetFlashGroupByClient_nilClient(t *testing.T) {
	s := newBackupTestStreamer(nil, nil, "")
	slot, fg, owner := s.getFlashGroupByClient(0, nil)
	if fg != nil {
		t.Fatalf("expected nil flash group, got %v", fg)
	}
	if owner != 0 {
		t.Fatalf("expected owner slot 0, got %d", owner)
	}
	if slot == 0 {
		t.Fatalf("expected non-zero computed slot")
	}
}

func TestReadFromRemoteCache_primaryHitSkipsBackup(t *testing.T) {
	primary := newTestRemoteCacheClient(t, "primary-topo")
	backup := newTestRemoteCacheClient(t, "backup-topo")
	s := newBackupTestStreamer(primary, backup, "private-topo")

	const size = uint64(4096)
	total, err := s.readFromRemoteCache(context.Background(), 0, size, holeOnlyCacheReadRequests(size), proto.StorageClass_Replica_HDD)
	if err != nil {
		t.Fatalf("readFromRemoteCache: %v", err)
	}
	if int(total) != int(size) {
		t.Fatalf("total = %d, want %d", total, size)
	}
}

func TestReadFromRemoteCache_ssdSkipsBackupOnPrimaryMiss(t *testing.T) {
	backup := newTestRemoteCacheClient(t, "backup-topo")
	s := newBackupTestStreamer(nil, backup, "private-topo")

	_, err := s.readFromRemoteCache(context.Background(), 0, 1024, holeOnlyCacheReadRequests(1024), proto.StorageClass_Replica_SSD)
	if err == nil {
		t.Fatal("expected error when primary client is nil")
	}
	if err != proto.ErrorNoFlashGroup {
		t.Fatalf("expected ErrorNoFlashGroup, got %v", err)
	}
}

func TestReadFromRemoteCache_hddFallsBackToBackup(t *testing.T) {
	backup := newTestRemoteCacheClient(t, "backup-topo")
	s := newBackupTestStreamer(nil, backup, "private-topo")

	const size = uint64(2048)
	total, err := s.readFromRemoteCache(context.Background(), 0, size, holeOnlyCacheReadRequests(size), proto.StorageClass_Replica_HDD)
	if err != nil {
		t.Fatalf("readFromRemoteCache: %v", err)
	}
	if int(total) != int(size) {
		t.Fatalf("total = %d, want %d", total, size)
	}
}

func TestReadFromRemoteCache_noBackupConfigured(t *testing.T) {
	s := newBackupTestStreamer(nil, nil, "")

	_, err := s.readFromRemoteCache(context.Background(), 0, 512, holeOnlyCacheReadRequests(512), proto.StorageClass_Replica_HDD)
	if err == nil {
		t.Fatal("expected error when primary client is nil and backup is not configured")
	}
	if err != proto.ErrorNoFlashGroup {
		t.Fatalf("expected ErrorNoFlashGroup, got %v", err)
	}
}
