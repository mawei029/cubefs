package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cubefs/cubefs/proto"
)

func writeFlashTopoListReply(w http.ResponseWriter, topos []proto.FlashTopologyAdminView) {
	payload, _ := json.Marshal(topos)
	rsp, _ := json.Marshal(proto.HTTPReplyRaw{Code: proto.ErrCodeSuccess, Data: payload})
	_, _ = w.Write(rsp)
}

func newFlashTopoMockMaster(t *testing.T, topos []proto.FlashTopologyAdminView, listErr bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != proto.AdminFlashTopoList {
			http.NotFound(w, r)
			return
		}
		if listErr {
			http.Error(w, "list failed", http.StatusInternalServerError)
			return
		}
		writeFlashTopoListReply(w, topos)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestValidateFlashTopoNames_noCustomTopo(t *testing.T) {
	if err := validateFlashTopoNames("", proto.DefaultTopoName, ""); err != nil {
		t.Fatalf("unexpected error for default topologies: %v", err)
	}
	if err := validateFlashTopoNames("", "", proto.DefaultTopoName); err != nil {
		t.Fatalf("unexpected error for default topologies: %v", err)
	}
}

func TestValidateFlashTopoNames_sameTopoRejected(t *testing.T) {
	err := validateFlashTopoNames("", "cloud-topo", "cloud-topo")
	if err == nil {
		t.Fatal("expected error when primary and backup topo are the same")
	}
	if !strings.Contains(err.Error(), "must differ") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFlashTopoNames_emptySameTopoSkipped(t *testing.T) {
	if err := validateFlashTopoNames("", "", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFlashTopoNames_primaryExists(t *testing.T) {
	addr := newFlashTopoMockMaster(t, []proto.FlashTopologyAdminView{
		{Name: "cloud-topo"},
	}, false)
	if err := validateFlashTopoNames(addr, "cloud-topo", ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFlashTopoNames_primaryNotFound(t *testing.T) {
	addr := newFlashTopoMockMaster(t, []proto.FlashTopologyAdminView{
		{Name: "other-topo"},
	}, false)
	err := validateFlashTopoNames(addr, "cloud-topo", "")
	if err == nil || !strings.Contains(err.Error(), "not exist") {
		t.Fatalf("expected not exist error, got: %v", err)
	}
}

func TestValidateFlashTopoNames_backupExists(t *testing.T) {
	addr := newFlashTopoMockMaster(t, []proto.FlashTopologyAdminView{
		{Name: "cloud-topo"},
		{Name: "private-topo"},
	}, false)
	if err := validateFlashTopoNames(addr, "cloud-topo", "private-topo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateFlashTopoNames_backupNotFound(t *testing.T) {
	addr := newFlashTopoMockMaster(t, []proto.FlashTopologyAdminView{
		{Name: "cloud-topo"},
	}, false)
	err := validateFlashTopoNames(addr, "cloud-topo", "private-topo")
	if err == nil || !strings.Contains(err.Error(), "not exist") {
		t.Fatalf("expected not exist error, got: %v", err)
	}
}

func TestValidateFlashTopoNames_listAPIError(t *testing.T) {
	addr := newFlashTopoMockMaster(t, nil, true)
	err := validateFlashTopoNames(addr, "cloud-topo", "")
	if err == nil || !strings.Contains(err.Error(), "list flashTopo err") {
		t.Fatalf("expected list error, got: %v", err)
	}
}

func TestValidateFlashTopoNames_backupOnlyCustom(t *testing.T) {
	addr := newFlashTopoMockMaster(t, []proto.FlashTopologyAdminView{
		{Name: "private-topo"},
	}, false)
	if err := validateFlashTopoNames(addr, "", "private-topo"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
