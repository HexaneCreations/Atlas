//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	storagepageauthz "github.com/hexane/atlas/internal/storage/pageauthz"
	"github.com/jackc/pgx/v5/pgxpool"
)

func notificationCall(t *testing.T, client *http.Client, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out
}

// Every step-10 route against the real role seed. The channels are email,
// which has no sender, so a test never touches the network.
func TestNotificationTestMuteAndArchiveRoutes(t *testing.T) {
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

	nodeID := fmt.Sprintf("it-alerts-%d", time.Now().UnixNano())
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

	super := authenticatedTestClient(t, root, "superadmin")
	code, channel := notificationCall(t, super, http.MethodPost, base+"/notifications/channels",
		`{"name":"`+nodeID+`","type":"email","secret":"pw"}`)
	if code != http.StatusCreated {
		t.Fatalf("create channel: %d %v", code, channel)
	}
	channelID := channel["id"].(string)
	t.Cleanup(func() { notificationCall(t, super, http.MethodDelete, base+"/notifications/channels/"+channelID, "") })

	draft := `{"name":"draft","type":"email","secret":"pw"}`
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/notifications/channels/" + channelID + "/test", ""},
		{http.MethodPost, "/notifications/channels/test", draft},
		{http.MethodPut, "/nodes/" + nodeID + "/alerts-mute", `{"duration":"1h"}`},
		{http.MethodDelete, "/nodes/" + nodeID + "/alerts-mute", ""},
		{http.MethodPost, "/nodes/" + nodeID + "/archive", ""},
		{http.MethodDelete, "/nodes/" + nodeID + "/archive", ""},
	}

	bundled, bundledID := bareUser(t, root, "admin")
	if err := storagepageauthz.NewRepository(pool).AssignRoleAccess(ctx, corepageauthz.RoleAccessAssignmentSpec{
		UserID: bundledID, RoleAccessName: "full-access", FleetWide: true, GrantedBy: "integration-test",
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		client *http.Client
		want   int
	}{
		"admin":                  {authenticatedTestClient(t, root, "admin"), http.StatusForbidden},
		"admin with full-access": {bundled, http.StatusForbidden},
		"operator":               {authenticatedTestClient(t, root, "operator"), http.StatusForbidden},
		"viewer":                 {authenticatedTestClient(t, root, "viewer"), http.StatusForbidden},
		"anonymous":              {http.DefaultClient, http.StatusUnauthorized},
	} {
		for _, r := range routes {
			if code, _ := notificationCall(t, c.client, r.method, base+r.path, r.body); code != c.want {
				t.Errorf("%s: %s %s = %d, want %d", name, r.method, r.path, code, c.want)
			}
		}
	}

	// Superadmin.
	code, out := notificationCall(t, super, http.MethodPost, base+routes[0].path, "")
	if code != http.StatusOK || out["ok"] != false || !strings.Contains(out["error"].(string), "no sender registered") {
		t.Errorf("saved test: %d %v", code, out)
	}
	if code, out := notificationCall(t, super, http.MethodPost, base+routes[1].path, draft); code != http.StatusTooManyRequests {
		t.Errorf("second test at once: %d %v, want 429", code, out)
	}
	if code, _ := notificationCall(t, super, http.MethodPost, base+"/notifications/channels/missing/test", ""); code != http.StatusNotFound {
		t.Errorf("unknown channel: %d, want 404", code)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification_deliveries WHERE channel_id = $1 AND kind = 'test' AND status = 'failed'`,
		channelID).Scan(&rows); err != nil || rows != 1 {
		t.Errorf("recorded tests = %d (%v), want 1", rows, err)
	}

	// The same two fields on both endpoints the Nodes page reads.
	wantNode := func(field string, check func(v any) bool) {
		t.Helper()
		_, one := notificationCall(t, super, http.MethodGet, base+"/nodes/"+nodeID, "")
		_, list := notificationCall(t, super, http.MethodGet, base+"/nodes", "")
		listed := map[string]any{}
		for _, n := range list["nodes"].([]any) {
			if m := n.(map[string]any); m["node_id"] == nodeID {
				listed = m
			}
		}
		for where, n := range map[string]map[string]any{"GET /nodes/{id}": one, "GET /nodes": listed} {
			if v, present := n[field]; !present || !check(v) {
				t.Errorf("%s: %s = %v (present %v)", where, field, v, present)
			}
		}
	}
	null := func(v any) bool { return v == nil }
	set := func(v any) bool { return v != nil }
	wantNode("alerts_muted_until", null)
	wantNode("archived_at", null)

	if code, out := notificationCall(t, super, http.MethodPut, base+"/nodes/"+nodeID+"/alerts-mute", `{"duration":"until_unmuted"}`); code != http.StatusOK || out["alerts_muted_until"] != "9999-01-01T00:00:00Z" {
		t.Errorf("until_unmuted: %d %v", code, out)
	}
	wantNode("alerts_muted_until", func(v any) bool { return v == "9999-01-01T00:00:00Z" })
	if code, _ := notificationCall(t, super, http.MethodPut, base+"/nodes/"+nodeID+"/alerts-mute", `{"duration":"1h"}`); code != http.StatusOK {
		t.Errorf("re-mute: %d", code)
	}
	for range 2 { // the second is a no-op
		if code, _ := notificationCall(t, super, http.MethodDelete, base+"/nodes/"+nodeID+"/alerts-mute", ""); code != http.StatusNoContent {
			t.Errorf("unmute: %d, want 204", code)
		}
	}
	wantNode("alerts_muted_until", null)

	if code, out := notificationCall(t, super, http.MethodPost, base+"/nodes/"+nodeID+"/archive", ""); code != http.StatusOK || out["archived_at"] == nil {
		t.Errorf("archive: %d %v", code, out)
	}
	wantNode("archived_at", set) // still listed: archiving hides nothing
	if code, _ := notificationCall(t, super, http.MethodDelete, base+"/nodes/"+nodeID+"/archive", ""); code != http.StatusNoContent {
		t.Errorf("unarchive: %d, want 204", code)
	}
	wantNode("archived_at", null)

	// A mute that is over but not yet ended reads as not muted.
	if _, err := pool.Exec(ctx, `UPDATE nodes SET alerts_muted_until = now() - interval '1 second' WHERE node_id = $1`, nodeID); err != nil {
		t.Fatal(err)
	}
	wantNode("alerts_muted_until", null)

	for _, r := range routes[2:] {
		if code, _ := notificationCall(t, super, r.method, base+strings.Replace(r.path, nodeID, "no-such-node", 1), r.body); code != http.StatusNotFound {
			t.Errorf("%s %s on an unknown node: %d, want 404", r.method, r.path, code)
		}
	}
}
