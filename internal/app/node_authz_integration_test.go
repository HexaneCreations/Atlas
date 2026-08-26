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

	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/id"
	storageuser "github.com/hexane/atlas/internal/storage/user"
	"github.com/jackc/pgx/v5/pgxpool"
)

// scopedTestClient is [authenticatedTestClient] with control over the
// grant's scope — fleet-wide, one specific node, or (grantRole == "") no
// grant at all — which the fleet-wide-only helper in app_integration_test.go
// cannot express.
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
