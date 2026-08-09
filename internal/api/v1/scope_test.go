package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexane/atlas/internal/api"
	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/hostid"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/process"
)

// Every inventory endpoint is scoped to a node. Atlas can only read its own
// host until agents exist, and these tests pin the two behaviours that must
// survive that change: the local node answers, and any other node is refused
// with a reason rather than answered with an empty list.

const scopeTestNode = "8d7dc1c1d52274c74cb0a569e7774a31"

// scopeCollection records the scope each inventory call received, so the tests
// can assert the parameter is actually threaded through rather than dropped.
type scopeCollection struct {
	fakeCollection
	gotScope inventory.Scope
}

func (f *scopeCollection) Identity() hostid.Identity {
	return hostid.Identity{NodeID: scopeTestNode, Hostname: "test-host"}
}

func (f *scopeCollection) PluginActive(string) bool { return true }

func (f *scopeCollection) Processes(_ context.Context, scope inventory.Scope) ([]process.Process, error) {
	f.gotScope = scope
	if !scope.IsLocal(scopeTestNode) {
		return nil, inventory.ErrRemoteUnavailable("test", scope.NodeID, "process inventory")
	}
	return []process.Process{{PID: 1, Name: "init"}}, nil
}

func newScopeServer(t *testing.T) (*httptest.Server, *scopeCollection) {
	t.Helper()

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	coll := &scopeCollection{}
	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     health.NewRegistry(nil),
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: coll,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv, coll
}

// No `node` parameter must keep working. Existing callers predate scoping and
// must not break when it lands.
func TestInventoryDefaultsToLocalNode(t *testing.T) {
	t.Parallel()

	srv, coll := newScopeServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/processes")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if coll.gotScope.NodeID != "" {
		t.Errorf("scope node = %q, want empty (local)", coll.gotScope.NodeID)
	}

	var body struct {
		NodeID    string `json:"node_id"`
		Processes []struct {
			Name string `json:"name"`
		} `json:"processes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The response names the host it describes, so a caller with a node picker
	// never has to assume which one answered.
	if body.NodeID != scopeTestNode {
		t.Errorf("node_id = %q, want the local node", body.NodeID)
	}
	if len(body.Processes) != 1 {
		t.Errorf("processes = %d, want 1", len(body.Processes))
	}
}

// Addressing the local node by id is what a node picker does on every request.
func TestInventoryAcceptsExplicitLocalNode(t *testing.T) {
	t.Parallel()

	srv, coll := newScopeServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/processes?node=" + scopeTestNode)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if coll.gotScope.NodeID != scopeTestNode {
		t.Errorf("scope was not threaded through: %q", coll.gotScope.NodeID)
	}
}

// The important one. A remote node must produce an error naming the reason,
// never an empty inventory — an empty process list for another host reads as
// "nothing is running there", which Atlas has no basis to claim.
func TestInventoryRefusesRemoteNodeRatherThanReturningEmpty(t *testing.T) {
	t.Parallel()

	srv, _ := newScopeServer(t)

	resp, err := http.Get(srv.URL + "/api/v1/processes?node=some-other-host")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Fatal("a remote node was answered with 200; an empty list would read as 'nothing is running there'")
	}

	var body struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// `unavailable`, not `not_implemented`: reading another node is a
	// capability Atlas will gain with agents, and the frontend renders
	// not_implemented as a permanent absence.
	if body.Error.Code != "unavailable" {
		t.Errorf("code = %q, want unavailable", body.Error.Code)
	}
	if body.Error.Details["node"] != "some-other-host" {
		t.Errorf("details.node = %v", body.Error.Details["node"])
	}
	if body.Error.Details["reason"] == nil {
		t.Error("no reason in details; the UI cannot explain the gap")
	}
}

// scopeNoPlugins has no plugins active — the situation on a host without
// systemd, Docker or a readable crontab.
type scopeNoPlugins struct{ scopeCollection }

func (f *scopeNoPlugins) PluginActive(string) bool { return false }

// Scope is resolved before plugin availability, and the order is the point.
//
// Asked about a remote node, a macOS Atlas answering "the service integration
// is not available on this host" describes the wrong machine: the caller did
// not ask about this host. The answer must be that remote inventory cannot be
// read, which is true regardless of what either machine runs.
func TestRemoteScopeIsRefusedBeforePluginAvailability(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     health.NewRegistry(nil),
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: &scopeNoPlugins{},
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Local: the honest answer is that this host has no such integration.
	local, err := http.Get(srv.URL + "/api/v1/services")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer local.Body.Close()
	if local.StatusCode != http.StatusNotImplemented {
		t.Errorf("local status = %d, want 501 — this host really has no service manager", local.StatusCode)
	}

	// Remote: the honest answer is about reachability, not about this host.
	remote, err := http.Get(srv.URL + "/api/v1/services?node=some-remote-host")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer remote.Body.Close()
	if remote.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("remote status = %d, want 503 — a remote question must not be answered with a local capability report", remote.StatusCode)
	}
}
