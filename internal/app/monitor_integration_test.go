//go:build integration

package app_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// Website monitors end to end: the real role seed and page grants, the real
// prober checking a local site, and the secret never leaving the server.
func TestWebsiteMonitorRoutesAndProbe(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	const secret = "it-monitor-sentinel-51c0"

	var failing atomic.Bool
	var gotAuth atomic.Value
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization") + "|" + r.Header.Get("X-Api-Key"))
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("maintenance " + secret))
			return
		}
		_, _ = w.Write([]byte("healthy"))
	}))
	defer site.Close()

	logs := &lockedLog{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	root := bootServerWith(t, func(c *config.Config) { c.Monitoring.AllowPrivateTargets = true }, logger)
	base := root + "/api/v1"
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	super := authenticatedTestClient(t, root, "superadmin")
	body := fmt.Sprintf(`{"name":"it-site-%d","url":"%s/health?token=%s","keyword":"healthy","auth_type":"bearer",
		"bearer_token":"%s","headers":[{"name":"X-Api-Key","value":"%s"}],"interval_seconds":20,"retry_interval_seconds":20,
		"timeout_seconds":5,"retries":0}`, time.Now().UnixNano(), site.URL, secret, secret, secret)
	code, created := notificationCall(t, super, http.MethodPost, base+"/monitors", body)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	id, _ := created["id"].(string)
	t.Cleanup(func() { notificationCall(t, super, http.MethodDelete, base+"/monitors/"+id, "") })
	monitorPath := base + "/monitors/" + id

	due := func() {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE monitor_status SET next_check_at = now() WHERE monitor_id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	waitStatus := func(want string) map[string]any {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			_, got := notificationCall(t, super, http.MethodGet, monitorPath, "")
			live, _ := got["live"].(map[string]any)
			if live["status"] == want {
				return got
			}
			if time.Now().After(deadline) {
				t.Fatalf("monitor never became %s: %v", want, got)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	due()
	up := waitStatus("up")
	if auth, _ := gotAuth.Load().(string); auth != "Bearer "+secret+"|"+secret {
		t.Errorf("the site received %q, want the bearer token and header", auth)
	}
	if u, _ := up["uptime"].(map[string]any); u["day"] != 100.0 {
		t.Errorf("uptime = %v", up["uptime"])
	}

	failing.Store(true)
	due()
	down := waitStatus("down")
	if live := down["live"].(map[string]any); live["last_failure"] != "http_status" || live["last_status_code"] != 503.0 {
		t.Errorf("down live = %v", live)
	}
	_, events := notificationCall(t, super, http.MethodGet, monitorPath+"/events", "")
	if list, _ := events["events"].([]any); len(list) != 1 || list[0].(map[string]any)["kind"] != "down" {
		t.Errorf("events = %v", events)
	}
	_, list := notificationCall(t, super, http.MethodGet, base+"/monitors", "")
	found := false
	for _, m := range list["monitors"].([]any) {
		if mm := m.(map[string]any); mm["id"] == id {
			found = len(mm["heartbeats"].([]any)) >= 2
		}
	}
	if !found {
		t.Error("the list does not show this monitor's heartbeats")
	}

	// Roles: everyone with the websites page reads; only superadmin writes.
	writes := []struct{ method, path, body string }{
		{http.MethodPost, base + "/monitors", `{"name":"x","url":"https://x.example.com/"}`},
		{http.MethodPut, monitorPath, `{"name":"y"}`},
		{http.MethodPut, monitorPath + "/active", `{"active":false}`},
		{http.MethodDelete, monitorPath, ""},
		{http.MethodPost, base + "/monitor-groups", `{"name":"g"}`},
	}
	for _, role := range []string{"admin", "operator", "viewer"} {
		c := authenticatedTestClient(t, root, role)
		code, got := notificationCall(t, c, http.MethodGet, monitorPath, "")
		if code != http.StatusOK || got["redacted"] != true || got["url"] != site.URL+"/health" || got["header_names"] != nil {
			t.Errorf("%s read: %d %v", role, code, got)
		}
		for _, w := range writes {
			if code, _ := notificationCall(t, c, w.method, w.path, w.body); code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403", role, w.method, w.path, code)
			}
		}
	}
	if code, _ := notificationCall(t, http.DefaultClient, http.MethodGet, base+"/monitors", ""); code != http.StatusUnauthorized {
		t.Errorf("anonymous: %d", code)
	}

	// Without the websites page, a viewer is refused.
	viewer := authenticatedTestClient(t, root, "viewer")
	if _, err := pool.Exec(ctx, `UPDATE user_page_access SET revoked_at = now()
		WHERE page_key = 'websites' AND revoked_at IS NULL
		  AND user_id = (SELECT id FROM users WHERE username LIKE 'it-operator-viewer-%' ORDER BY created_at DESC LIMIT 1)`); err != nil {
		t.Fatal(err)
	}
	if code, _ := notificationCall(t, viewer, http.MethodGet, base+"/monitors", ""); code != http.StatusForbidden {
		t.Errorf("viewer without the websites page: %d, want 403", code)
	}

	// The secret is nowhere but in the ciphertext.
	if out := logs.String(); !strings.Contains(out, "website prober ready") || !strings.Contains(out, `"msg":"website check"`) {
		t.Errorf("the server's own log was not captured: %.300s", out)
	} else if strings.Contains(out, secret) {
		t.Error("server logs carry the monitor secret")
	}
	for _, table := range []string{"monitors", "monitor_status", "monitor_heartbeats", "monitor_events"} {
		var hits int
		q := fmt.Sprintf(`SELECT count(*) FROM %s t WHERE to_jsonb(t)::text LIKE '%%' || $1 || '%%'`, table)
		if table == "monitors" {
			q = `SELECT count(*) FROM monitors t WHERE (to_jsonb(t) - 'url')::text LIKE '%' || $1 || '%'`
		}
		if err := pool.QueryRow(ctx, q, secret).Scan(&hits); err != nil || hits != 0 {
			t.Errorf("%s holds the plaintext secret (%d, %v)", table, hits, err)
		}
	}
	for _, out := range []map[string]any{created, up, down, list} {
		if strings.Contains(fmt.Sprint(out["bearer_token"], out["headers"], out["live"]), secret) {
			t.Error("a response carries the secret")
		}
	}
	if code, _ := notificationCall(t, super, http.MethodDelete, monitorPath, ""); code != http.StatusNoContent {
		t.Errorf("delete: %d", code)
	}
}

// A test of a saved monitor, while it is up and while it is down, reaches
// the site but leaves the monitor, its live state, counters, heartbeats and
// events exactly as they were.
func TestWebsiteMonitorTestLeavesStateAlone(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	var failing atomic.Bool
	var hits atomic.Int64
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("healthy"))
	}))
	defer site.Close()

	root := bootServerWith(t, func(c *config.Config) { c.Monitoring.AllowPrivateTargets = true }, nil)
	base := root + "/api/v1"
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	super := authenticatedTestClient(t, root, "superadmin")
	code, created := notificationCall(t, super, http.MethodPost, base+"/monitors", fmt.Sprintf(
		`{"name":"it-test-%d","url":"%s/","bearer_token":"tok","auth_type":"bearer","interval_seconds":20,
		"retry_interval_seconds":20,"timeout_seconds":5,"retries":0}`, time.Now().UnixNano(), site.URL))
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	id, _ := created["id"].(string)
	t.Cleanup(func() { notificationCall(t, super, http.MethodDelete, base+"/monitors/"+id, "") })

	exec := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, id); err != nil {
			t.Fatal(err)
		}
	}
	settle := func(want string) {
		t.Helper()
		exec(`UPDATE monitor_status SET next_check_at = now() WHERE monitor_id = $1`)
		deadline := time.Now().Add(20 * time.Second)
		for {
			var status string
			_ = pool.QueryRow(ctx, `SELECT status FROM monitor_status WHERE monitor_id = $1`, id).Scan(&status)
			if status == want {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("monitor never settled %s", want)
			}
			time.Sleep(100 * time.Millisecond)
		}
		// No scheduled check may run while the state is compared.
		exec(`UPDATE monitor_status SET next_check_at = now() + interval '1 day' WHERE monitor_id = $1`)
	}
	snapshot := func() string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, `SELECT concat_ws(' | ',
			(SELECT (to_jsonb(m) - 'secret_ciphertext')::text || md5(m.secret_ciphertext) FROM monitors m WHERE m.id = $1),
			(SELECT to_jsonb(s)::text FROM monitor_status s WHERE s.monitor_id = $1),
			(SELECT count(*)::text || ' ' || coalesce(max(time)::text, '') FROM monitor_heartbeats WHERE monitor_id = $1),
			(SELECT count(*)::text || ' ' || coalesce(max(time)::text, '') FROM monitor_events WHERE monitor_id = $1))`, id).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	var lastTest time.Time
	test := func(want string) {
		t.Helper()
		// The per-target limit allows one test of a host every few seconds.
		time.Sleep(time.Until(lastTest.Add(5100 * time.Millisecond)))
		before, n := snapshot(), hits.Load()
		code, out := notificationCall(t, super, http.MethodPost, base+"/monitors/test", `{"monitor_id":"`+id+`"}`)
		lastTest = time.Now()
		if code != http.StatusOK || out["status"] != want {
			t.Fatalf("test: %d %v, want %s", code, out, want)
		}
		if hits.Load() != n+1 {
			t.Errorf("the site was reached %d times by one test", hits.Load()-n)
		}
		if after := snapshot(); after != before {
			t.Errorf("a test changed the monitor's stored state:\nbefore %s\nafter  %s", before, after)
		}
	}

	settle("up")
	failing.Store(true)
	test("down") // while up, a failing test
	failing.Store(false)
	test("up")

	failing.Store(true)
	settle("down")
	failing.Store(false)
	test("up") // while down, a passing test
	failing.Store(true)
	test("down")
}
