//go:build integration

package notification_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"testing/fstest"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/metric"
	"github.com/hexane/atlas/internal/storage/notification"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const websiteMonitoringVersion = 25

// migrationFiles returns the embedded migrations, those before version
// separately.
func migrationFiles(t *testing.T, version int64) (before fstest.MapFS, target string) {
	t.Helper()
	all, err := postgres.NewMigrator(nil, migrations.FS, nil).Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	before = fstest.MapFS{}
	for _, m := range all {
		switch {
		case m.Version < version:
			before[m.Filename()] = &fstest.MapFile{Data: []byte(m.SQL)}
		case m.Version == version:
			target = m.SQL
		}
	}
	if target == "" {
		t.Fatalf("migration %04d not found", version)
	}
	return before, target
}

// snapshot renders every row of the notification tables, minus the columns
// 0025 and 0026 add and 0026's seeded website rules, in a stable order.
func snapshot(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for table, order := range map[string]string{
		// Filtered through to_jsonb so the same query runs before and after
		// the monitor_id columns exist.
		"notification_state":      "WHERE to_jsonb(t)->>'monitor_id' IS NULL ORDER BY trigger, node_id, subject",
		"notification_rules":      "WHERE to_jsonb(t)->>'monitor_id' IS NULL AND trigger NOT LIKE 'website%' ORDER BY trigger, node_id NULLS FIRST",
		"notification_deliveries": "WHERE COALESCE(to_jsonb(t)->>'monitor_id', '') = '' ORDER BY id",
		"notification_channels":   "ORDER BY id",
		"nodes":                   "ORDER BY node_id",
	} {
		rows, err := pool.Query(context.Background(), fmt.Sprintf(
			`SELECT (to_jsonb(t) - 'monitor_id' - 'monitor_default' - 'details')::text FROM %s t %s`, table, order))
		if err != nil {
			t.Fatalf("snapshot %s: %v", table, err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out[table] = append(out[table], s)
		}
		rows.Close()
	}
	return out
}

// 0025 on a database holding 0001-0024 data: every node-scoped state,
// rule override, delivery (event ids included) and channel is
// byte-identical afterwards; the unchanged node-subject SQL (the previous
// release's) still upserts, dedupes and fires with the same event ids; and
// re-running 0025 is a no-op.
func TestMigration0025KeepsNodeNotificationDataIntact(t *testing.T) {
	ctx := context.Background()
	pool := freshUnmigrated(t)
	before, target := migrationFiles(t, websiteMonitoringVersion)
	if _, err := postgres.NewMigrator(pool, before, log.Discard()).Apply(ctx); err != nil {
		t.Fatalf("apply 0001-0024: %v", err)
	}

	key := newKey(t)
	repo := notification.NewRepository(pool, key)
	now := time.Now().UTC().Truncate(time.Second)
	conditionSince := now.Add(-time.Hour)
	for _, n := range []string{"mig-node", "mig-other"} {
		if _, err := pool.Exec(ctx, `INSERT INTO nodes (node_id, hostname, last_seen_at) VALUES ($1, $1, $2)`, n, now.Add(-2*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// Written as the 0024-era release wrote it: today's CreateChannel also
	// sets monitor_default, which 0024 does not have.
	channel := corenotification.Channel{ID: "ch-mig", Type: corenotification.ChannelEmail}
	sealed, err := key.Seal([]byte("webhook-signing-secret"), []byte(channel.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `INSERT INTO notification_channels (id, name, type, config, secret_ciphertext, secret_key_id)
		VALUES ($1, 'ops', 'email', '{}', $2, $3)`, channel.ID, sealed.Ciphertext, sealed.KeyID); err != nil {
		t.Fatalf("insert a channel on 0024: %v", err)
	}
	for _, q := range []string{
		`INSERT INTO notification_rules (id, trigger, node_id, enabled, warn_threshold, crit_threshold, updated_by)
		 VALUES ('ov-disk', 'disk_pct', 'mig-node', false, 70, 80, 'alice')`,
		`INSERT INTO notification_rules (id, trigger, node_id, for_seconds, subject_excludes, updated_by)
		 VALUES ('ov-down', 'node_down', 'mig-other', 600, '{/boot}', 'bob')`,
		`INSERT INTO notification_state (trigger, node_id, subject, state, severity, condition_since, state_since, baseline)
		 VALUES ('node_down', 'mig-node', '', 'pending', '', '` + conditionSince.Format(time.RFC3339) + `', '` + conditionSince.Format(time.RFC3339) + `', '{}')`,
		`INSERT INTO notification_state (trigger, node_id, subject, state, severity, value, condition_since, state_since,
		 last_notified_at, last_notified_kind, flap_count, flap_window_start, baseline)
		 VALUES ('disk_pct', 'mig-other', '/var', 'firing', 'critical', 93.5, now() - interval '2 hours', now() - interval '1 hour',
		 now() - interval '1 hour', 'firing', 2, now() - interval '3 hours', '{"x": [1, 2]}')`,
		`INSERT INTO notification_state (trigger, node_id, subject, state, state_since, baseline)
		 VALUES ('container_exit', 'mig-other', 'web', 'ok', now() - interval '1 day', '{"seen_up": true, "container_id": "abc"}')`,
		`INSERT INTO notification_deliveries (id, event_id, channel_id, channel_type, kind, trigger, node_id, subject, severity,
		 title, message, status, attempts, next_attempt_at, last_error, delivered_at)
		 VALUES ('d1', 'notif-disk_pct-mig-other-/var-1764000000-firing-critical', '` + channel.ID + `', 'email', 'firing',
		 'disk_pct', 'mig-other', '/var', 'critical', 'Disk', 'Disk at 93%', 'delivered', 1, now(), '', now())`,
		`INSERT INTO notification_deliveries (id, event_id, channel_id, channel_type, kind, trigger, node_id, title, message,
		 status, attempts, next_attempt_at, last_error)
		 VALUES ('d2', 'notif-node_down-mig-node--1764000100-recovered-critical', '` + channel.ID + `', 'email', 'recovered',
		 'node_down', 'mig-node', 'Up', 'Back', 'failed', 5, now(), 'timeout')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("seed: %v\n%s", err, q)
		}
	}
	want := snapshot(t, pool)
	if len(want["notification_state"]) != 3 || len(want["notification_rules"]) != 8 || len(want["notification_deliveries"]) != 2 {
		t.Fatalf("seeded snapshot is not what the test set up: %v", want)
	}

	if _, err := postgres.NewMigrator(pool, migrations.FS, log.Discard()).Apply(ctx); err != nil {
		t.Fatalf("apply 0025: %v", err)
	}
	var applied int64
	if err := pool.QueryRow(ctx, `SELECT max(version) FROM atlas_schema_migrations`).Scan(&applied); err != nil || applied < websiteMonitoringVersion {
		t.Fatalf("ledger at %d, %v", applied, err)
	}
	assertSnapshot(t, want, snapshot(t, pool))

	// Re-running 0025 changes nothing.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, target); err != nil {
		t.Fatalf("re-run 0025: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(t, want, snapshot(t, pool))

	// Node rows got NULL monitor_id and the deliveries ''.
	var bad int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM notification_state WHERE monitor_id IS NOT NULL)
		+ (SELECT count(*) FROM notification_deliveries WHERE monitor_id <> '')`).Scan(&bad); err != nil || bad != 0 {
		t.Errorf("rows with a monitor subject after the migration: %d, %v", bad, err)
	}

	// The node-subject statements, unchanged from the previous release.
	stateCount := func() int {
		var n int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_state`).Scan(&n)
		return n
	}
	n0 := stateCount()
	s, found, err := repo.GetState(ctx, corenotification.TriggerDiskPct, "mig-other", "/var")
	if err != nil || !found || s.State != corenotification.StateFiring || *s.Value != 93.5 || s.FlapCount != 2 {
		t.Fatalf("GetState after 0025 = %+v, %v, %v", s, found, err)
	}
	s.FlapCount = 3
	if err := repo.UpsertState(ctx, s); err != nil {
		t.Fatalf("UpsertState (ON CONFLICT (trigger, node_id, subject)) after 0025: %v", err)
	}
	if stateCount() != n0 {
		t.Error("an upsert of an existing node state inserted a duplicate row")
	}
	active, err := repo.ListActiveStates(ctx)
	if err != nil || len(active) != 2 {
		t.Errorf("ListActiveStates = %d, %v", len(active), err)
	}
	if err := repo.SetRule(ctx, corenotification.RuleSpec{Trigger: corenotification.TriggerDiskPct, NodeID: "mig-node",
		Enabled: ptr(true), WarnThreshold: ptr(75.0), CritThreshold: ptr(85.0)}, "carol"); err != nil {
		t.Fatalf("SetRule on an existing override after 0025: %v", err)
	}
	if _, err := repo.SetNodeRuleEnabled(ctx, "mig-other", corenotification.TriggerRAMPct, false, "carol"); err != nil {
		t.Fatalf("SetNodeRuleEnabled after 0025: %v", err)
	}
	var overrides int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM notification_rules WHERE node_id IS NOT NULL`).Scan(&overrides)
	if overrides != 3 {
		t.Errorf("overrides = %d, want 3 (two updated in place, one new)", overrides)
	}
	dup := corenotification.Notification{EventID: "notif-disk_pct-mig-other-/var-1764000000-firing-critical", Kind: corenotification.KindFiring,
		Trigger: corenotification.TriggerDiskPct, NodeID: "mig-other", Subject: "/var", Severity: corenotification.SeverityCritical, Title: "t", Message: "m"}
	channels, _ := repo.ListChannels(ctx)
	if n, err := repo.Enqueue(ctx, dup, channels); err != nil || n != 0 {
		t.Errorf("re-enqueueing a stored event id = %d, %v; want deduped", n, err)
	}

	// A real tick fires the pending node_down with the event id the
	// previous release would have built.
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

func assertSnapshot(t *testing.T, want, got map[string][]string) {
	t.Helper()
	for table, rows := range want {
		if !slices.Equal(rows, got[table]) {
			t.Errorf("%s changed:\nbefore %v\nafter  %v", table, rows, got[table])
		}
	}
}
