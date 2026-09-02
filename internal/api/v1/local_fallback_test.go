package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/hexane/atlas/internal/plugin/process"
)

// The control plane is "local" for its own configured node id, but the
// process it runs in may have no Docker socket and no active plugins — a
// co-located agent reports that host's inventory instead. These tests pin
// that the local read path falls through to the pushed snapshot rather than
// dead-ending on "not available on this host", and that when nothing was
// pushed the honest local absence still wins, now carrying a reason the UI
// can word.

func newFallbackServer(t *testing.T, coll v1.CollectionSource, inv *fakeInventoryStore) *httptest.Server {
	t.Helper()

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     health.NewRegistry(nil),
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: coll,
		Inventory:  inv,
		Nodes:      &fakeNodeExistence{known: map[string]bool{scopeTestNode: true}},
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestLocalInventoryFallsBackToCoLocatedAgentSnapshot(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal([]process.Process{{PID: 7, Name: "from-agent"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	inv := newFakeInventoryStore()
	if err := inv.Put(context.Background(), coreinventory.StoredSnapshot{
		NodeID: scopeTestNode, Subject: coreinventory.SubjectProcesses,
		ObservedAt: time.Now().Add(-30 * time.Second), ContentHash: "h", Data: payload,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// scopeNoPlugins: Identity() is the local node, PluginActive() is false.
	srv := newFallbackServer(t, &scopeNoPlugins{}, inv)

	resp, err := http.Get(srv.URL + "/api/v1/processes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a pushed snapshot for the local node must be served", resp.StatusCode)
	}

	var body processesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Live {
		t.Error("Live = true; a snapshot fallback is not a live read")
	}
	if len(body.Processes) != 1 || body.Processes[0].Name != "from-agent" {
		t.Errorf("processes = %+v, want the pushed snapshot", body.Processes)
	}
}

func TestLocalInventoryWithoutPluginOrSnapshotIsNotImplementedWithReason(t *testing.T) {
	t.Parallel()

	srv := newFallbackServer(t, &scopeNoPlugins{}, newFakeInventoryStore())

	body := getError(t, srv.URL+"/api/v1/processes", http.StatusNotImplemented)
	if body.Error.Code != "not_implemented" {
		t.Errorf("code = %q, want not_implemented", body.Error.Code)
	}
	if body.Error.Details["reason"] != "no_local_plugin" {
		t.Errorf("details.reason = %v, want no_local_plugin", body.Error.Details["reason"])
	}
}

func TestLocalContainersFallBackToCoLocatedAgentSnapshot(t *testing.T) {
	t.Parallel()

	payload, err := json.Marshal([]docker.Container{{ID: "deadbeefcafe0000", Name: "from-agent"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	inv := newFakeInventoryStore()
	if err := inv.Put(context.Background(), coreinventory.StoredSnapshot{
		NodeID: scopeTestNode, Subject: coreinventory.SubjectContainers,
		ObservedAt: time.Now().Add(-30 * time.Second), ContentHash: "h", Data: payload,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// scopeCollection: Identity() is the local node, DockerClient() is nil
	// (the docker field is unset) — no local Docker in this process.
	srv := newFallbackServer(t, &scopeCollection{}, inv)

	resp, err := http.Get(srv.URL + "/api/v1/containers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a pushed container snapshot for the local node must be served", resp.StatusCode)
	}

	var body struct {
		NodeID     string `json:"node_id"`
		Live       bool   `json:"live"`
		Containers []struct {
			Name string `json:"name"`
		} `json:"containers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Live {
		t.Error("Live = true; a snapshot fallback is not a live read")
	}
	if len(body.Containers) != 1 || body.Containers[0].Name != "from-agent" {
		t.Errorf("containers = %+v, want the pushed snapshot", body.Containers)
	}
}

func TestLocalContainersWithoutDockerOrSnapshotIsNotImplementedWithReason(t *testing.T) {
	t.Parallel()

	srv := newFallbackServer(t, &scopeCollection{}, newFakeInventoryStore())

	body := getError(t, srv.URL+"/api/v1/containers", http.StatusNotImplemented)
	if body.Error.Code != "not_implemented" {
		t.Errorf("code = %q, want not_implemented", body.Error.Code)
	}
	if body.Error.Details["reason"] != "no_local_docker" {
		t.Errorf("details.reason = %v, want no_local_docker", body.Error.Details["reason"])
	}
}
