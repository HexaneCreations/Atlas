package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/process"
	"github.com/hexane/atlas/internal/storage/metric"
)

const remoteTestNode = "remote-node-1"

type fakeInventoryStore struct {
	snapshots map[string]coreinventory.StoredSnapshot
	reported  map[string]bool
	getErr    error
}

func newFakeInventoryStore() *fakeInventoryStore {
	return &fakeInventoryStore{
		snapshots: map[string]coreinventory.StoredSnapshot{},
		reported:  map[string]bool{},
	}
}

func snapKey(nodeID string, subject coreinventory.Subject) string {
	return nodeID + "/" + string(subject)
}

func (f *fakeInventoryStore) Put(_ context.Context, s coreinventory.StoredSnapshot) error {
	f.snapshots[snapKey(s.NodeID, s.Subject)] = s
	f.reported[s.NodeID] = true
	return nil
}

func (f *fakeInventoryStore) Get(_ context.Context, nodeID string, subject coreinventory.Subject) (coreinventory.StoredSnapshot, error) {
	if f.getErr != nil {
		return coreinventory.StoredSnapshot{}, f.getErr
	}
	s, ok := f.snapshots[snapKey(nodeID, subject)]
	if !ok {
		return coreinventory.StoredSnapshot{}, errs.New(errs.CodeNotFound, "no snapshot")
	}
	return s, nil
}

func (f *fakeInventoryStore) HasReported(_ context.Context, nodeID string) (bool, error) {
	return f.reported[nodeID], nil
}

type fakeNodeExistence struct{ known map[string]bool }

func (f *fakeNodeExistence) GetNode(_ context.Context, nodeID string) (metric.Node, error) {
	if !f.known[nodeID] {
		return metric.Node{}, errs.New(errs.CodeNotFound, "no such node")
	}
	return metric.Node{NodeID: nodeID}, nil
}

func newRemoteTestServer(t *testing.T, inv *fakeInventoryStore, nodes *fakeNodeExistence) *httptest.Server {
	t.Helper()

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     health.NewRegistry(nil),
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: &scopeCollection{},
		Inventory:  inv,
		Nodes:      nodes,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

type apiErrorBody struct {
	Error struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	} `json:"error"`
}

func getError(t *testing.T, url string, status int) apiErrorBody {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("status = %d, want %d", resp.StatusCode, status)
	}
	var body apiErrorBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

type processesResponse struct {
	NodeID    string `json:"node_id"`
	Processes []struct {
		PID  int32  `json:"pid"`
		Name string `json:"name"`
	} `json:"processes"`
	ObservedAt time.Time `json:"observed_at"`
	Live       bool      `json:"live"`
}

func TestRemoteInventoryUnknownNodeIsNotFound(t *testing.T) {
	t.Parallel()

	srv := newRemoteTestServer(t, newFakeInventoryStore(), &fakeNodeExistence{known: map[string]bool{}})
	body := getError(t, srv.URL+"/api/v1/processes?node=ghost-node", http.StatusNotFound)
	if body.Error.Code != "not_found" {
		t.Errorf("code = %q, want not_found", body.Error.Code)
	}
}

func TestRemoteInventoryKnownNodeNeverReportedIsUnavailable(t *testing.T) {
	t.Parallel()

	srv := newRemoteTestServer(t, newFakeInventoryStore(), &fakeNodeExistence{known: map[string]bool{remoteTestNode: true}})
	body := getError(t, srv.URL+"/api/v1/processes?node="+remoteTestNode, http.StatusServiceUnavailable)
	if body.Error.Code != "unavailable" {
		t.Errorf("code = %q, want unavailable", body.Error.Code)
	}
	if body.Error.Details["reason"] != "no_agent" {
		t.Errorf("details.reason = %v, want no_agent", body.Error.Details["reason"])
	}
}

// The node reported other subjects but never processes — e.g. no Docker on
// that host. Must read as not_implemented, matching a local plugin's honest
// absence, not as a permanent unavailable or an empty list.
func TestRemoteInventoryReportedOtherSubjectsIsNotImplemented(t *testing.T) {
	t.Parallel()

	inv := newFakeInventoryStore()
	if err := inv.Put(context.Background(), coreinventory.StoredSnapshot{
		NodeID: remoteTestNode, Subject: coreinventory.SubjectPorts,
		ObservedAt: time.Now(), ContentHash: "h", Data: []byte(`[]`),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	srv := newRemoteTestServer(t, inv, &fakeNodeExistence{known: map[string]bool{remoteTestNode: true}})

	body := getError(t, srv.URL+"/api/v1/processes?node="+remoteTestNode, http.StatusNotImplemented)
	if body.Error.Code != "not_implemented" {
		t.Errorf("code = %q, want not_implemented", body.Error.Code)
	}
}

func TestRemoteInventoryReturnsSnapshotWithProvenance(t *testing.T) {
	t.Parallel()

	observedAt := time.Now().Add(-47 * time.Second).UTC().Truncate(time.Millisecond)
	payload, err := json.Marshal([]process.Process{{PID: 99, Name: "remote-proc"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	inv := newFakeInventoryStore()
	if err := inv.Put(context.Background(), coreinventory.StoredSnapshot{
		NodeID: remoteTestNode, Subject: coreinventory.SubjectProcesses,
		ObservedAt: observedAt, ContentHash: "h", Data: payload,
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	srv := newRemoteTestServer(t, inv, &fakeNodeExistence{known: map[string]bool{remoteTestNode: true}})

	resp, err := http.Get(srv.URL + "/api/v1/processes?node=" + remoteTestNode)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body processesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Live {
		t.Error("Live = true for a remote snapshot, want false")
	}
	if !body.ObservedAt.Equal(observedAt) {
		t.Errorf("ObservedAt = %v, want %v", body.ObservedAt, observedAt)
	}
	if body.NodeID != remoteTestNode {
		t.Errorf("NodeID = %q, want %q", body.NodeID, remoteTestNode)
	}
	if len(body.Processes) != 1 || body.Processes[0].Name != "remote-proc" {
		t.Errorf("Processes = %+v, want the pushed snapshot's content", body.Processes)
	}
}

func TestLocalInventoryReportsLive(t *testing.T) {
	t.Parallel()

	srv := newRemoteTestServer(t, newFakeInventoryStore(), &fakeNodeExistence{known: map[string]bool{}})

	before := time.Now()
	resp, err := http.Get(srv.URL + "/api/v1/processes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	var body processesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !body.Live {
		t.Error("Live = false for a local read, want true")
	}
	if body.ObservedAt.Before(before) {
		t.Errorf("ObservedAt = %v, predates the request", body.ObservedAt)
	}
}

func TestRemoteInventoryCorruptSnapshotIsInternalError(t *testing.T) {
	t.Parallel()

	inv := newFakeInventoryStore()
	if err := inv.Put(context.Background(), coreinventory.StoredSnapshot{
		NodeID: remoteTestNode, Subject: coreinventory.SubjectProcesses,
		ObservedAt: time.Now(), ContentHash: "h", Data: []byte(`not valid json`),
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	srv := newRemoteTestServer(t, inv, &fakeNodeExistence{known: map[string]bool{remoteTestNode: true}})

	body := getError(t, srv.URL+"/api/v1/processes?node="+remoteTestNode, http.StatusInternalServerError)
	if body.Error.Code != "internal" {
		t.Errorf("code = %q, want internal", body.Error.Code)
	}
}

func TestRemoteInventoryWithNoStoreConfiguredIsUnavailable(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config: &cfg, Health: health.NewRegistry(nil),
		Pool: postgres.NewPool(cfg.Database, nil), Bus: bus,
		Collection: &scopeCollection{},
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	body := getError(t, srv.URL+"/api/v1/processes?node="+remoteTestNode, http.StatusServiceUnavailable)
	if body.Error.Code != "unavailable" {
		t.Errorf("code = %q, want unavailable", body.Error.Code)
	}
	if body.Error.Details["reason"] == nil {
		t.Error("no reason in details")
	}
}
