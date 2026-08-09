package v1_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/service"
)

// testGraph mirrors a small but realistic slice of a systemd host: a web
// server that hard-requires a database, a cache it merely wants, and an
// ordering-only edge to a target it does not depend on.
func testGraph() *service.Graph {
	return service.NewGraph(
		[]service.Node{
			{Name: "nginx.service", Type: "service", ActiveState: service.ActiveStateActive, SubState: "running", LoadState: "loaded", Known: true},
			{Name: "db.service", Type: "service", ActiveState: service.ActiveStateFailed, SubState: "failed", LoadState: "loaded", Known: true},
			{Name: "cache.service", Type: "service", ActiveState: service.ActiveStateFailed, SubState: "failed", LoadState: "loaded", Known: true},
			{Name: "network.target", Type: "target", ActiveState: service.ActiveStateActive, LoadState: "loaded", Known: true},
		},
		[]service.Edge{
			{From: "nginx.service", To: "db.service", Kind: service.EdgeRequires},
			{From: "nginx.service", To: "cache.service", Kind: service.EdgeWants},
			{From: "nginx.service", To: "network.target", Kind: service.EdgeAfter},
		},
		time.Now(),
	)
}

func newGraphServer(t *testing.T, g *service.Graph) *httptest.Server {
	t.Helper()

	cfg := config.Default()
	reg := health.NewRegistry(nil)
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     reg,
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: fakeCollection{serviceGraph: g, activePlugins: []string{"service"}},
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func getJSON[T any](t *testing.T, url string) (T, int) {
	t.Helper()

	var out T
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode %s: %v", url, err)
		}
	}
	return out, resp.StatusCode
}

func TestServiceGraphReturnsNodesAndEdges(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	got, status := getJSON[v1.ServiceGraphResponse](t, srv.URL+"/api/v1/services/graph")

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(got.Nodes) != 4 {
		t.Errorf("nodes = %d, want 4", len(got.Nodes))
	}
	if len(got.Edges) != 3 {
		t.Errorf("edges = %d, want 3", len(got.Edges))
	}
	if got.TotalNodes != 4 || got.TotalEdges != 3 {
		t.Errorf("totals = %d/%d, want 4/3", got.TotalNodes, got.TotalEdges)
	}

	// Every edge must carry its class, so a client can filter and style
	// without re-implementing the kind→class mapping.
	for _, e := range got.Edges {
		if e.Class == "" {
			t.Errorf("edge %+v has no class", e)
		}
	}
}

// A unit whose hard dependency has failed is degraded, and the response must
// name the failure so the client can explain it rather than merely colour it.
func TestServiceGraphPropagatesHealth(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	got, _ := getJSON[v1.ServiceGraphResponse](t, srv.URL+"/api/v1/services/graph")

	byID := map[string]v1.GraphNodeResponse{}
	for _, n := range got.Nodes {
		byID[n.ID] = n
	}

	if h := byID["nginx.service"].Health; h != "degraded" {
		t.Errorf("nginx health = %q, want degraded", h)
	}
	if len(byID["nginx.service"].FailedDependencies) == 0 {
		t.Error("degraded node does not name its failed dependencies")
	}
	if h := byID["db.service"].Health; h != "failed" {
		t.Errorf("db health = %q, want failed", h)
	}
	if h := byID["network.target"].Health; h != "healthy" {
		t.Errorf("target health = %q, want healthy", h)
	}
}

func TestServiceGraphRootedTraversalCarriesDepth(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	got, status := getJSON[v1.ServiceGraphResponse](t,
		srv.URL+"/api/v1/services/graph?root=nginx.service&depth=1")

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if got.Root != "nginx.service" {
		t.Errorf("root = %q", got.Root)
	}
	for _, n := range got.Nodes {
		if n.Depth == nil {
			t.Fatalf("node %s has no depth on a rooted traversal", n.ID)
		}
		if n.ID == "nginx.service" && *n.Depth != 0 {
			t.Errorf("root depth = %d, want 0", *n.Depth)
		}
	}
}

// The class filter is what keeps ordering edges out of a dependency view.
func TestServiceGraphFiltersByClass(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	got, _ := getJSON[v1.ServiceGraphResponse](t,
		srv.URL+"/api/v1/services/graph?root=nginx.service&class=requirement")

	for _, n := range got.Nodes {
		if n.ID == "network.target" {
			t.Error("an ordering-only neighbour was returned under a requirement filter")
		}
	}
	for _, e := range got.Edges {
		if e.Class != "requirement" {
			t.Errorf("edge %+v is not a requirement edge", e)
		}
	}
}

func TestServiceGraphRejectsBadParameters(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())

	for _, url := range []string{
		"/api/v1/services/graph?class=nonsense",
		"/api/v1/services/graph?direction=sideways",
		"/api/v1/services/graph?depth=-1",
	} {
		if _, status := getJSON[v1.ServiceGraphResponse](t, srv.URL+url); status != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", url, status)
		}
	}
}

func TestServiceGraphUnknownRootIs404(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	if _, status := getJSON[v1.ServiceGraphResponse](t,
		srv.URL+"/api/v1/services/graph?root=ghost.service"); status != http.StatusNotFound {
		t.Errorf("status = %d, want 404", status)
	}
}

// Hard and soft impact must stay separate: a Wants dependent keeps running.
func TestServiceDetailSeparatesHardFromSoftImpact(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	got, status := getJSON[v1.ServiceDetailResponse](t, srv.URL+"/api/v1/services/db.service")

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(got.Impact.Hard) != 1 || got.Impact.Hard[0] != "nginx.service" {
		t.Errorf("hard impact = %v, want [nginx.service]", got.Impact.Hard)
	}

	// cache.service is only *wanted* by nginx, so its failure is soft.
	cache, _ := getJSON[v1.ServiceDetailResponse](t, srv.URL+"/api/v1/services/cache.service")
	if len(cache.Impact.Hard) != 0 {
		t.Errorf("a wanted unit reported hard impact: %v", cache.Impact.Hard)
	}
	if len(cache.Impact.Soft) != 1 || cache.Impact.Soft[0] != "nginx.service" {
		t.Errorf("soft impact = %v, want [nginx.service]", cache.Impact.Soft)
	}
}

// The ordering edge must never appear in an impact set: network.target is
// ordered before nginx but nothing depends on it here.
func TestServiceImpactExcludesOrdering(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	got, status := getJSON[v1.ServiceImpactResponse](t,
		srv.URL+"/api/v1/services/network.target/impact")

	if status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	if len(got.Hard) != 0 || len(got.Soft) != 0 {
		t.Errorf("ordering-only dependents were reported as impacted: %+v", got)
	}
}

func TestServiceDetailReportsDirectRelationships(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, testGraph())
	got, _ := getJSON[v1.ServiceDetailResponse](t, srv.URL+"/api/v1/services/nginx.service")

	if len(got.Dependencies) != 3 {
		t.Errorf("dependencies = %d, want 3", len(got.Dependencies))
	}
	if len(got.Dependents) != 0 {
		t.Errorf("dependents = %d, want 0", len(got.Dependents))
	}
	if got.Node.Dependencies != 3 {
		t.Errorf("degree count = %d, want 3", got.Node.Dependencies)
	}
}

// A host with no service manager must say so rather than returning an empty
// graph, which would read as "nothing depends on anything".
func TestServiceGraphWithoutManagerIsNotImplemented(t *testing.T) {
	t.Parallel()

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     health.NewRegistry(nil),
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: fakeCollection{}, // no service plugin active
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	if _, status := getJSON[v1.ServiceGraphResponse](t, srv.URL+"/api/v1/services/graph"); status != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", status)
	}
}
