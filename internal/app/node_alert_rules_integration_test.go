//go:build integration

package app_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The per-node alert rule routes against the real role seed and schema.
func TestNotificationNodeAlertRuleRoutes(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t) + "/api/v1"
	root := strings.TrimSuffix(base, "/api/v1")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	nodeID := fmt.Sprintf("it-rules-%d", time.Now().UnixNano())
	if _, err := pool.Exec(ctx, `INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, now())`, nodeID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			return
		}
		defer p.Close()
		_, _ = p.Exec(context.Background(), `DELETE FROM nodes WHERE node_id = $1`, nodeID)
	})

	rules := "/nodes/" + nodeID + "/alert-rules"
	routes := []struct{ method, path, body string }{
		{http.MethodGet, rules, ""},
		{http.MethodPut, rules + "/disk_pct", `{"enabled":false}`},
		{http.MethodDelete, rules + "/disk_pct", ""},
	}
	for name, c := range map[string]struct {
		client *http.Client
		want   int
	}{
		"admin":     {authenticatedTestClient(t, root, "admin"), http.StatusForbidden},
		"operator":  {authenticatedTestClient(t, root, "operator"), http.StatusForbidden},
		"viewer":    {authenticatedTestClient(t, root, "viewer"), http.StatusForbidden},
		"anonymous": {http.DefaultClient, http.StatusUnauthorized},
	} {
		for _, r := range routes {
			if code, _ := notificationCall(t, c.client, r.method, base+r.path, r.body); code != c.want {
				t.Errorf("%s: %s %s = %d, want %d", name, r.method, r.path, code, c.want)
			}
		}
	}

	super := authenticatedTestClient(t, root, "superadmin")
	source := func(key string) (any, any) {
		t.Helper()
		code, out := notificationCall(t, super, http.MethodGet, base+rules, "")
		if code != http.StatusOK {
			t.Fatalf("GET: %d %v", code, out)
		}
		list := out["rules"].([]any)
		if len(list) != 6 {
			t.Fatalf("rules = %d, want the 6 catalog entries", len(list))
		}
		for _, r := range list {
			if m := r.(map[string]any); m["key"] == key {
				return m["enabled"], m["source"]
			}
		}
		t.Fatalf("no rule %q", key)
		return nil, nil
	}
	if enabled, src := source("disk_pct"); enabled != true || src != "global" {
		t.Errorf("before: %v %v, want true from global", enabled, src)
	}
	if code, out := notificationCall(t, super, http.MethodPut, base+rules+"/disk_pct", `{"enabled":false}`); code != http.StatusOK ||
		out["enabled"] != false || out["source"] != "node" {
		t.Errorf("PUT: %d %v", code, out)
	}
	if enabled, src := source("disk_pct"); enabled != false || src != "node" {
		t.Errorf("after PUT: %v %v, want false from node", enabled, src)
	}
	for range 2 {
		if code, _ := notificationCall(t, super, http.MethodDelete, base+rules+"/disk_pct", ""); code != http.StatusNoContent {
			t.Errorf("DELETE: %d, want 204 every time", code)
		}
	}
	if enabled, src := source("disk_pct"); enabled != true || src != "global" {
		t.Errorf("after DELETE: %v %v, want true from global", enabled, src)
	}

	for _, c := range []struct {
		method, path, body string
		want               int
	}{
		{http.MethodPut, rules + "/cpu_pct", `{"enabled":false}`, http.StatusNotFound},
		{http.MethodDelete, rules + "/cpu_pct", "", http.StatusNotFound},
		{http.MethodPut, rules + "/disk_pct", `{}`, http.StatusBadRequest},
		{http.MethodPut, rules + "/disk_pct", `{"enabled":"no"}`, http.StatusBadRequest},
		{http.MethodGet, "/nodes/missing-node/alert-rules", "", http.StatusNotFound},
		{http.MethodPut, "/nodes/missing-node/alert-rules/disk_pct", `{"enabled":false}`, http.StatusNotFound},
	} {
		if code, _ := notificationCall(t, super, c.method, base+c.path, c.body); code != c.want {
			t.Errorf("%s %s %s = %d, want %d", c.method, c.path, c.body, code, c.want)
		}
	}
}
