//go:build integration

// A node-scoped viewer must see only the node(s) they hold a grant for on
// GET /api/v1/nodes and GET /api/v1/nodes/{id} — the gap this file proves
// closed was a real production visibility leak: any authenticated user, no
// matter how narrowly scoped their grant, saw the entire fleet's hostnames,
// environments, and inventory metadata via the list endpoint.
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/id"
	storagepageauthz "github.com/hexane/atlas/internal/storage/pageauthz"
	storageuser "github.com/hexane/atlas/internal/storage/user"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scopedTestClient is [authenticatedTestClient] with control over the
// node.read grant's scope — fleet-wide, one specific node, or
// (grantRole == "") no grant at all — which the fleet-wide-only helper in
// app_integration_test.go cannot express.
//
// Every caller here is testing ListNodes/GetNode's node.read-based
// filtering specifically, not the separate page-visibility layer (see
// internal/core/pageauthz) — so this always grants fleet-wide access to the
// Nodes page itself, including for the deliberately-no-node.read-grant
// case, so a 403 in these tests always means what it's meant to mean:
// node.read's own filtering, not an unrelated page-access denial masking
// it.
func scopedTestClient(t *testing.T, base, grantRole, nodeID string, fleetWide bool) *http.Client {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	username := "it-nodeauthz-" + strings.ToLower(grantRole) + "-" + id.New()
	if grantRole == "" {
		username = "it-nodeauthz-nogrant-" + id.New()
	}
	const password = "integration-test-password"

	hash, err := coreuser.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	repo := storageuser.NewRepository(pool)
	if err := repo.CreateUser(context.Background(), coreuser.User{Username: username, PasswordHash: hash}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := repo.ByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}

	if grantRole != "" {
		grant := coreuser.GrantSpec{UserID: u.ID, NodeID: nodeID, FleetWide: fleetWide, Role: grantRole, GrantedBy: "integration-test"}
		if err := repo.Grant(context.Background(), grant, time.Now()); err != nil {
			t.Fatalf("Grant: %v", err)
		}
	}

	pageRepo := storagepageauthz.NewRepository(pool)
	pageSpec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageNodes, FleetWide: true, GrantedBy: "integration-test"}
	if err := pageRepo.GrantPageAccess(context.Background(), pageSpec, time.Now()); err != nil {
		t.Fatalf("GrantPageAccess(nodes): %v", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}

	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	return client
}

// seedNode inserts a minimal node row directly, the same convention
// goldensignals_integration_test.go and its siblings use — ListNodes/GetNode
// read straight from this table.
func seedNode(t *testing.T, dsn, nodeID string) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, $2)`,
		nodeID, time.Now().UTC()); err != nil {
		t.Fatalf("seed node: %v", err)
	}
}

func listNodeIDs(t *testing.T, client *http.Client, base string) []string {
	t.Helper()
	resp, err := client.Get(base + "/api/v1/nodes")
	if err != nil {
		t.Fatalf("GET /nodes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Nodes []struct {
			NodeID string `json:"node_id"`
		} `json:"nodes"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Total != len(body.Nodes) {
		t.Errorf("total = %d, len(nodes) = %d — must agree", body.Total, len(body.Nodes))
	}
	ids := make([]string, len(body.Nodes))
	for i, n := range body.Nodes {
		ids[i] = n.NodeID
	}
	return ids
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// This is the exact production scenario reported: a node-scoped viewer's
// grant names one node, and GET /api/v1/nodes must not include any other —
// full fleet enumeration was the leak, not the per-node 403s downstream.
func TestListNodesShowsOnlyTheGrantedNodeForANodeScopedViewer(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	granted := fmt.Sprintf("nodeauthz-granted-%d", time.Now().UnixNano())
	other := fmt.Sprintf("nodeauthz-other-%d", time.Now().UnixNano())
	seedNode(t, dsn, granted)
	seedNode(t, dsn, other)

	client := scopedTestClient(t, base, "viewer", granted, false)
	ids := listNodeIDs(t, client, base)

	if !contains(ids, granted) {
		t.Errorf("nodes = %v, want it to contain the granted node %s", ids, granted)
	}
	if contains(ids, other) {
		t.Errorf("nodes = %v, leaked a node this viewer was never granted (%s)", ids, other)
	}
}

func TestListNodesShowsEveryNodeForAFleetWideViewer(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	a := fmt.Sprintf("nodeauthz-fleetwide-a-%d", time.Now().UnixNano())
	b := fmt.Sprintf("nodeauthz-fleetwide-b-%d", time.Now().UnixNano())
	seedNode(t, dsn, a)
	seedNode(t, dsn, b)

	client := scopedTestClient(t, base, "viewer", "", true)
	ids := listNodeIDs(t, client, base)

	if !contains(ids, a) || !contains(ids, b) {
		t.Errorf("nodes = %v, want a fleet-wide grant to see both seeded nodes (%s, %s)", ids, a, b)
	}
}

// The other half of the same gap: a user with no grant at all must see an
// empty list — not every node (the bug), and not a 403 (list filtering, not
// pass/fail gating, is the correct shape for "which nodes can you see").
func TestListNodesReturnsEmptyForAUserWithNoGrantAtAll(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	seedNode(t, dsn, fmt.Sprintf("nodeauthz-nogrant-visible-%d", time.Now().UnixNano()))

	client := scopedTestClient(t, base, "", "", false)
	ids := listNodeIDs(t, client, base)

	if len(ids) != 0 {
		t.Errorf("nodes = %v, want empty for a user with no grant at all", ids)
	}
}

// GetNode is the direct-lookup counterpart: gated per node, same as every
// other node-scoped endpoint, not exempt just because it addresses one node
// by id instead of returning a list.
func TestGetNodeIsGatedByTheSpecificNodeGrant(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	granted := fmt.Sprintf("nodeauthz-getnode-granted-%d", time.Now().UnixNano())
	other := fmt.Sprintf("nodeauthz-getnode-other-%d", time.Now().UnixNano())
	seedNode(t, dsn, granted)
	seedNode(t, dsn, other)

	client := scopedTestClient(t, base, "viewer", granted, false)

	resp, err := client.Get(base + "/api/v1/nodes/" + granted)
	if err != nil {
		t.Fatalf("GET granted node: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status for the granted node = %d, want 200", resp.StatusCode)
	}

	resp, err = client.Get(base + "/api/v1/nodes/" + other)
	if err != nil {
		t.Fatalf("GET other node: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status for the ungranted node = %d, want 403", resp.StatusCode)
	}
}

// pageAccessOnlyClient logs in a user provisioned entirely through the
// page-access axis: one node-scoped direct page grant for page, for nodeID,
// and nothing else — no user_node_roles grant, no fleet-wide page grant.
// This is the production shape (abhishek-atlas: a single Containers grant
// for one node) that scopedTestClient cannot express, since it always adds
// a fleet-wide Nodes-page grant and drives node.read scoping.
func pageAccessOnlyClient(t *testing.T, base string, page corepageauthz.Page, nodeID string) *http.Client {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	username := "it-pageonly-" + strings.ToLower(string(page)) + "-" + id.New()
	const password = "integration-test-password"

	hash, err := coreuser.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	repo := storageuser.NewRepository(pool)
	if err := repo.CreateUser(context.Background(), coreuser.User{Username: username, PasswordHash: hash}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := repo.ByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}

	pageRepo := storagepageauthz.NewRepository(pool)
	spec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: page, NodeID: nodeID, GrantedBy: "integration-test"}
	if err := pageRepo.GrantPageAccess(context.Background(), spec, time.Now()); err != nil {
		t.Fatalf("GrantPageAccess(%s, %s): %v", page, nodeID, err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar}
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal login body: %v", err)
	}
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	return client
}

// Investigation 2's fix: a user whose only grant is a node-scoped page
// grant (Containers for one node) must now see that node on GET /nodes, so
// usePrimaryNodeID can resolve a node id and the page stops hanging. The
// node they hold no grant of any kind for must still not leak.
func TestListNodesSurfacesANodeAUserOnlyHasPageAccessFor(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	granted := fmt.Sprintf("pageonly-nodes-granted-%d", time.Now().UnixNano())
	other := fmt.Sprintf("pageonly-nodes-other-%d", time.Now().UnixNano())
	seedNode(t, dsn, granted)
	seedNode(t, dsn, other)

	client := pageAccessOnlyClient(t, base, corepageauthz.PageContainers, granted)
	ids := listNodeIDs(t, client, base)

	if !contains(ids, granted) {
		t.Errorf("nodes = %v, want the node this user holds a Containers page grant for (%s)", ids, granted)
	}
	if contains(ids, other) {
		t.Errorf("nodes = %v, leaked a node this user has no grant of any kind for (%s)", ids, other)
	}
}

// Priority 3: GET /auth/me carries the caller's effective page access so
// the frontend can hide nav items and redirect direct-URL visits. A
// page-only user must see exactly their granted page, scoped to their node,
// and nothing else.
func TestAuthMeReportsEffectivePageAccess(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	granted := fmt.Sprintf("authme-pageaccess-%d", time.Now().UnixNano())
	seedNode(t, dsn, granted)

	client := pageAccessOnlyClient(t, base, corepageauthz.PageContainers, granted)

	resp, err := client.Get(base + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("GET /auth/me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		PageAccess []struct {
			Page      string   `json:"page"`
			FleetWide bool     `json:"fleet_wide"`
			NodeIDs   []string `json:"node_ids"`
		} `json:"page_access"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.PageAccess) != 1 {
		t.Fatalf("page_access = %+v, want exactly one entry (containers)", body.PageAccess)
	}
	got := body.PageAccess[0]
	if got.Page != "containers" || got.FleetWide || len(got.NodeIDs) != 1 || got.NodeIDs[0] != granted {
		t.Errorf("page_access[0] = %+v, want {containers, fleet_wide=false, node_ids=[%s]}", got, granted)
	}
}

// Priority 1: the three metrics endpoints did no authorization at all — any
// authenticated caller could read any node's series, latest values and
// metric names by id. They are now gated on node.read for the named node.
func TestMetricsEndpointsAreGatedByNodeReadForTheNamedNode(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)

	granted := fmt.Sprintf("metricsauthz-granted-%d", time.Now().UnixNano())
	other := fmt.Sprintf("metricsauthz-other-%d", time.Now().UnixNano())
	seedNode(t, dsn, granted)
	seedNode(t, dsn, other)

	// node.read viewer scoped to `granted` only.
	client := scopedTestClient(t, base, "viewer", granted, false)

	endpoints := []string{
		"/api/v1/metrics/latest?node=",
		"/api/v1/metrics/names?node=",
		"/api/v1/metrics?range=1h&metric=system.cpu.usage&node=",
	}
	for _, e := range endpoints {
		resp, err := client.Get(base + e + other)
		if err != nil {
			t.Fatalf("GET %s%s: %v", e, other, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s%s = %d, want 403 for a node this viewer lacks node.read on", e, other, resp.StatusCode)
		}

		resp, err = client.Get(base + e + granted)
		if err != nil {
			t.Fatalf("GET %s%s: %v", e, granted, err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusForbidden {
			t.Errorf("GET %s%s = 403, want it allowed for the node this viewer holds node.read on", e, granted)
		}
	}
}
