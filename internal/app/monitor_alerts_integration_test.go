//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/config"
	storagemonitor "github.com/hexane/atlas/internal/storage/monitor"
	storagenotification "github.com/hexane/atlas/internal/storage/notification"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A website going down and back up, end to end against the real prober:
// the alert reaches only the monitor's selected channel, carries the
// prober's facts, and no notification row or log line holds the monitor's
// secret, path or query.
func TestWebsiteMonitorAlertsReachOnlySelectedChannels(t *testing.T) {
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	const secret = "it-alert-sentinel-7d21"
	var failing atomic.Bool
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failing.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok"))
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

	suffix := time.Now().UnixNano()
	channel := func(name string, monitorDefault bool) string {
		t.Helper()
		code, got := notificationCall(t, super, http.MethodPost, base+"/notifications/channels",
			fmt.Sprintf(`{"name":"%s-%d","type":"email","secret":"pw-%s","monitor_default":%v}`, name, suffix, secret, monitorDefault))
		if code != http.StatusCreated || got["monitor_default"] != monitorDefault {
			t.Fatalf("create channel %s: %d %v", name, code, got)
		}
		id, _ := got["id"].(string)
		t.Cleanup(func() { notificationCall(t, super, http.MethodDelete, base+"/notifications/channels/"+id, "") })
		return id
	}
	picked, other := channel("picked", true), channel("other", false)

	body := fmt.Sprintf(`{"name":"it-alert-%d","url":"%s/status/private-path?token=%s","bearer_token":"%s","auth_type":"bearer",
		"interval_seconds":20,"retry_interval_seconds":20,"timeout_seconds":5,"retries":0}`, suffix, site.URL, secret, secret)
	code, created := notificationCall(t, super, http.MethodPost, base+"/monitors", body)
	if code != http.StatusCreated {
		t.Fatalf("create monitor: %d %v", code, created)
	}
	id, _ := created["id"].(string)
	t.Cleanup(func() { notificationCall(t, super, http.MethodDelete, base+"/monitors/"+id, "") })
	ids, _ := json.Marshal(created["channel_ids"])
	if !strings.Contains(string(ids), picked) || strings.Contains(string(ids), other) {
		t.Fatalf("new monitor channels = %s; want the monitor_default one preselected", ids)
	}
	// Other tests' monitor_default channels may exist: select exactly ours.
	if code, got := notificationCall(t, super, http.MethodPut, base+"/monitors/"+id+"/channels",
		`{"channel_ids":["`+picked+`"]}`); code != http.StatusOK {
		t.Fatalf("set channels: %d %v", code, got)
	}

	// The evaluator this server runs ticks every 30s; this one ticks on demand.
	repo := storagenotification.NewRepository(pool, nil)
	monitors := storagemonitor.NewRepository(pool, nil)
	engine := corenotification.NewEngine(corenotification.Options{Channels: repo, Deliveries: repo,
		MonitorChannels: monitors, MonitorInfo: monitors})
	// No node store: only monitors are evaluated.
	eval := corenotification.NewEvaluator(corenotification.EvaluatorOptions{Rules: repo, States: repo, Notifier: engine,
		Monitors: monitors, MonitorStates: repo})

	status := func(want string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE monitor_status SET next_check_at = now() WHERE monitor_id = $1`, id); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(20 * time.Second)
		for {
			_, got := notificationCall(t, super, http.MethodGet, base+"/monitors/"+id, "")
			if live, _ := got["live"].(map[string]any); live["status"] == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("monitor never became %s: %v", want, got)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	type row struct {
		channel, kind, title, message string
		details                       map[string]any
	}
	deliveries := func() []row {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT channel_id, kind, title, message, details FROM notification_deliveries
			WHERE monitor_id = $1 ORDER BY created_at, kind`, id)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			var raw []byte
			if err := rows.Scan(&r.channel, &r.kind, &r.title, &r.message, &raw); err != nil {
				t.Fatal(err)
			}
			_ = json.Unmarshal(raw, &r.details)
			out = append(out, r)
		}
		return out
	}

	status("up")
	failing.Store(true)
	status("down")
	eval.EvaluateOnce(ctx)
	got := deliveries()
	if len(got) != 1 || got[0].channel != picked || got[0].kind != "firing" ||
		got[0].details["failure"] != "http_status" || got[0].details["status_code"] != 503.0 {
		t.Fatalf("down deliveries = %+v; want one firing to the selected channel", got)
	}

	time.Sleep(time.Second) // a measurable outage
	failing.Store(false)
	status("up")
	eval.EvaluateOnce(ctx)
	got = deliveries()
	if len(got) != 2 || got[1].kind != "recovered" || got[1].channel != picked {
		t.Fatalf("deliveries after recovery = %+v", got)
	}
	if secs, ok := got[1].details["outage_seconds"].(float64); !ok || secs < 1 {
		t.Errorf("recovery details = %v; want the outage duration", got[1].details)
	}
	var stateID string
	if err := pool.QueryRow(ctx, `SELECT COALESCE(node_id, '') FROM notification_state WHERE monitor_id = $1`, id).Scan(&stateID); err != nil || stateID != "" {
		t.Errorf("monitor state row: node_id %q, %v", stateID, err)
	}

	// Nothing stored or logged carries the secret, the path or the query.
	for _, table := range []string{"notification_deliveries", "notification_state"} {
		var hits int
		q := fmt.Sprintf(`SELECT count(*) FROM %s t WHERE to_jsonb(t)::text LIKE '%%' || $1 || '%%' OR to_jsonb(t)::text LIKE '%%private-path%%'`, table)
		if err := pool.QueryRow(ctx, q, secret).Scan(&hits); err != nil || hits != 0 {
			t.Errorf("%s holds the secret or path (%d, %v)", table, hits, err)
		}
	}
	if strings.Contains(logs.String(), secret) || strings.Contains(logs.String(), "private-path") {
		t.Error("server logs carry the monitor's secret or path")
	}

	// A muted monitor stays quiet; deleting it removes its state.
	if code, _ := notificationCall(t, super, http.MethodPut, base+"/monitors/"+id+"/mute", `{"duration":"1h"}`); code != http.StatusOK {
		t.Errorf("mute: %d", code)
	}
	failing.Store(true)
	status("down")
	eval.EvaluateOnce(ctx)
	if n := len(deliveries()); n != 2 {
		t.Errorf("a muted monitor enqueued: %d deliveries", n)
	}
	if code, _ := notificationCall(t, super, http.MethodDelete, base+"/monitors/"+id, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	var left int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_state WHERE monitor_id = $1`, id).Scan(&left)
	if left != 0 {
		t.Errorf("a deleted monitor left %d state rows", left)
	}
}
