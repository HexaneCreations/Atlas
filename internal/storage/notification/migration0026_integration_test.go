//go:build integration

package notification_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/metric"
	"github.com/hexane/atlas/internal/storage/notification"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const monitorAlertsVersion = 26

// snapshot0026 renders every row 0026 could touch, minus the columns it
// adds and the global rules it seeds.
func snapshot0026(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for table, rest := range map[string]string{
		"notification_state":      "ORDER BY trigger, node_id NULLS FIRST, monitor_id NULLS FIRST, subject",
		"notification_rules":      "WHERE id NOT IN ('global-website_down', 'global-website_cert_expiry') ORDER BY id",
		"notification_deliveries": "ORDER BY id",
		"notification_channels":   "ORDER BY id",
		"monitor_events":          "ORDER BY id",
		"monitor_channels":        "ORDER BY monitor_id, channel_id",
		"monitors":                "ORDER BY id",
		"nodes":                   "ORDER BY node_id",
	} {
		rows, err := pool.Query(context.Background(), fmt.Sprintf(
			`SELECT (to_jsonb(t) - 'details' - 'cert_expires_at' - 'monitor_id')::text,
				COALESCE(to_jsonb(t)->>'monitor_id', '') FROM %s t %s`, table, rest))
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		for rows.Next() {
			var s, monitorID string
			if err := rows.Scan(&s, &monitorID); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], s+" monitor="+monitorID)
		}
		rows.Close()
	}
	return out
}

// 0026 on a database holding 0001-0025 data, node and monitor subjects
// alike: every existing row is unchanged apart from the added columns
// (node event ids included), re-running it is a no-op, the previous
// release's delivery and event inserts still work, and two monitors can
// now override one trigger.
func TestMigration0026KeepsExistingNotificationDataIntact(t *testing.T) {
	ctx := context.Background()
	pool := freshUnmigrated(t)
	before, target := migrationFiles(t, monitorAlertsVersion)
	if _, err := postgres.NewMigrator(pool, before, log.Discard()).Apply(ctx); err != nil {
		t.Fatalf("apply 0001-0025: %v", err)
	}

	key := newKey(t)
	repo := notification.NewRepository(pool, key)
	now := time.Now().UTC().Truncate(time.Second)
	conditionSince := now.Add(-time.Hour)
	secret := "webhook-signing-secret"
	channel, err := repo.CreateChannel(ctx, channelInput("ops", &secret))
	if err != nil {
		t.Fatalf("CreateChannel on 0025: %v", err)
	}
	ts := conditionSince.Format(time.RFC3339)
	for _, q := range []string{
		`INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ('mig-node', 'mig-node', now() - interval '2 hours')`,
		`INSERT INTO monitors (id, name, url) VALUES ('mon-a', 'A', 'https://a.example.com/'), ('mon-b', 'B', 'https://b.example.com/')`,
		`INSERT INTO monitor_status (monitor_id, status, next_check_at, down_since) VALUES ('mon-a', 'down', now(), now()), ('mon-b', 'up', now(), NULL)`,
		`INSERT INTO monitor_channels (monitor_id, channel_id) VALUES ('mon-a', '` + channel.ID + `')`,
		`INSERT INTO monitor_events (id, monitor_id, time, kind, status_code, failure) VALUES ('ev1', 'mon-a', now(), 'down', 503, 'http_status')`,
		`INSERT INTO notification_rules (id, trigger, node_id, enabled, warn_threshold, crit_threshold, updated_by)
		 VALUES ('ov-disk', 'disk_pct', 'mig-node', false, 70, 80, 'alice')`,
		`INSERT INTO notification_state (trigger, node_id, subject, state, severity, condition_since, state_since, baseline)
		 VALUES ('node_down', 'mig-node', '', 'pending', '', '` + ts + `', '` + ts + `', '{}')`,
		`INSERT INTO notification_state (trigger, node_id, subject, state, severity, value, condition_since, state_since,
		 last_notified_at, last_notified_kind, flap_count, baseline)
		 VALUES ('disk_pct', 'mig-node', '/var', 'firing', 'critical', 93.5, now() - interval '2 hours', now() - interval '1 hour',
		 now() - interval '1 hour', 'firing', 2, '{"x": [1, 2]}')`,
		`INSERT INTO notification_state (trigger, monitor_id, subject, state, severity, state_since, baseline)
		 VALUES ('website_down', 'mon-a', '', 'firing', 'critical', now(), '{"down_since": "2026-01-01T00:00:00Z"}')`,
		`INSERT INTO notification_deliveries (id, event_id, channel_id, channel_type, kind, trigger, node_id, subject, severity,
		 title, message, status, attempts, next_attempt_at, last_error, delivered_at)
		 VALUES ('d1', 'notif-disk_pct-mig-node-/var-1764000000-firing-critical', '` + channel.ID + `', 'email', 'firing',
		 'disk_pct', 'mig-node', '/var', 'critical', 'Disk', 'Disk at 93%', 'delivered', 1, now(), '', now())`,
	} {
		if _, err = pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	want := snapshot0026(t, pool)
	if len(want["notification_state"]) != 3 || len(want["notification_rules"]) != 7 || len(want["monitor_events"]) != 1 {
		t.Fatalf("seeded snapshot is not what the test set up: %v", want)
	}

	if _, err = postgres.NewMigrator(pool, migrations.FS, log.Discard()).Apply(ctx); err != nil {
		t.Fatalf("apply 0026: %v", err)
	}
	assertSnapshot(t, want, snapshot0026(t, pool))

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, target); err != nil {
		t.Fatalf("re-run 0026: %v", err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, want, snapshot0026(t, pool))

	var seeded, oldIndex, details int
	if err = pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM notification_rules WHERE id IN ('global-website_down', 'global-website_cert_expiry')
			AND node_id IS NULL AND monitor_id IS NULL AND for_seconds = 0 AND recovery_seconds = 0),
		(SELECT count(*) FROM pg_indexes WHERE indexname = 'ux_notification_rules_scope'),
		(SELECT count(*) FROM notification_deliveries WHERE details <> '{}')`).Scan(&seeded, &oldIndex, &details); err != nil {
		t.Fatal(err)
	}
	if seeded != 2 || oldIndex != 0 || details != 0 {
		t.Errorf("seeded %d website rules (want 2), old index present %d, non-empty details %d", seeded, oldIndex, details)
	}

	// The previous release's inserts, which name none of the new columns.
	if _, err = pool.Exec(ctx, `INSERT INTO notification_deliveries (id, event_id, channel_id, channel_type, kind, trigger,
		node_id, subject, severity, title, message, status, next_attempt_at)
		VALUES ('d2', 'old-release', $1, 'email', 'firing', 'node_down', 'mig-node', '', 'critical', 't', 'm', 'pending', now())`,
		channel.ID); err != nil {
		t.Errorf("v1.1.5 delivery insert after 0026: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO monitor_events (id, monitor_id, time, kind, status_code, failure, duration_seconds)
		VALUES ('ev2', 'mon-a', now(), 'up', 200, '', 60)`); err != nil {
		t.Errorf("v1.1.5 event insert after 0026: %v", err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO notification_rules (id, trigger, node_id, monitor_id, enabled)
		VALUES ('both', 'website_down', 'mig-node', 'mon-a', false)`); err == nil {
		t.Error("a rule scoped to a node and a monitor at once was accepted")
	}

	// Two monitors may now override one trigger, beside node overrides.
	for _, id := range []string{"mon-a", "mon-b"} {
		if _, err = repo.SetMonitorRuleEnabled(ctx, id, corenotification.TriggerWebsiteDown, false, "carol"); err != nil {
			t.Fatalf("SetMonitorRuleEnabled(%s): %v", id, err)
		}
	}
	if _, err = repo.SetNodeRuleEnabled(ctx, "mig-node", corenotification.TriggerRAMPct, false, "carol"); err != nil {
		t.Fatalf("SetNodeRuleEnabled after 0026: %v", err)
	}
	if err = repo.SetRule(ctx, corenotification.RuleSpec{Trigger: corenotification.TriggerDiskPct, NodeID: "mig-node",
		Enabled: ptr(true), WarnThreshold: ptr(75.0), CritThreshold: ptr(85.0)}, "carol"); err != nil {
		t.Fatalf("SetRule on an existing node override after 0026: %v", err)
	}
	disk, err := repo.EffectiveRule(ctx, corenotification.TriggerDiskPct, "mig-node")
	if err != nil || !disk.Enabled || *disk.WarnThreshold != 75 {
		t.Errorf("node override after 0026 = %+v, %v", disk, err)
	}
	if s, found, _ := repo.GetMonitorState(ctx, corenotification.TriggerWebsiteDown, "mon-a", ""); !found || s.State != corenotification.StateOK {
		t.Errorf("disabling website_down for mon-a left its state %+v", s)
	}

	// A tick fires the pending node_down with the event id the previous
	// release would have built.
	engine := corenotification.NewEngine(corenotification.Options{Channels: repo, Deliveries: repo,
		Senders: map[corenotification.ChannelType]corenotification.Sender{channel.Type: &countingFakeSender{}}})
	metricRepo := metric.NewRepository(pool)
	eval := corenotification.NewEvaluator(corenotification.EvaluatorOptions{
		Rules: repo, States: repo, Notifier: engine, Now: func() time.Time { return now },
		Nodes: evalNodeAdapter{repo: metricRepo}, Metrics: evalMetricAdapter{repo: metricRepo},
	})
	eval.EvaluateOnce(ctx)
	wantID := fmt.Sprintf("notif-node_down-mig-node--%d-firing-critical", conditionSince.Unix())
	var ids []string
	rows, err := pool.Query(ctx, `SELECT event_id FROM notification_deliveries WHERE trigger = 'node_down' AND kind = 'firing'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if !slices.Contains(ids, wantID) {
		t.Errorf("node_down firing event ids = %v, want %s", ids, wantID)
	}
}
