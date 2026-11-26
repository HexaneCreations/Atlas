//go:build integration

package monitor_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coremonitor "github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/platform/secretbox"
	"github.com/hexane/atlas/internal/storage/monitor"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}
	admin, err := pgxpool.NewWithConfig(context.Background(), parsed)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()
	name := fmt.Sprintf("atlas_monitor_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.NewWithConfig(context.Background(), parsed)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	cfg := config.Default()
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = name
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"
	pool := postgres.NewPool(cfg.Database, log.Discard())
	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool start: %v", err)
	}
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })
	if _, err := postgres.NewMigrator(pool.DB(), migrations.FS, log.Discard()).Apply(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return pool.DB()
}

func newKey(t *testing.T) *secretbox.Key {
	t.Helper()
	raw := make([]byte, secretbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	k, err := secretbox.NewKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func newMonitor(name string) coremonitor.Monitor {
	m := coremonitor.Defaults()
	m.Name, m.URL = name, "https://"+strings.ToLower(name)+".example.com/health?token=abc"
	return m
}

func code(err error) errs.Code { return errs.CodeOf(err) }

const sentinel = "sentinel-secret-9f3a"

func sentinelSecret() coremonitor.Secret {
	return coremonitor.Secret{Headers: map[string]string{"X-Api-Key": sentinel}, BearerToken: sentinel + "-bearer", Body: sentinel + "-body"}
}

// noSentinelAnywhere fails if the plaintext secret appears in any monitor
// or notification table.
func noSentinelAnywhere(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, table := range []string{"monitors", "monitor_status", "monitor_heartbeats", "monitor_events", "notification_deliveries", "notification_state"} {
		var hits int
		q := fmt.Sprintf(`SELECT count(*) FROM %s t WHERE to_jsonb(t)::text LIKE '%%' || $1 || '%%'`, table)
		if err := pool.QueryRow(context.Background(), q, sentinel).Scan(&hits); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		if hits > 0 {
			t.Errorf("the plaintext secret is stored in %s", table)
		}
	}
	var hits int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM monitors WHERE position($1::bytea IN secret_ciphertext) > 0`,
		[]byte(sentinel)).Scan(&hits); err != nil || hits > 0 {
		t.Errorf("ciphertext contains the plaintext (%d, %v)", hits, err)
	}
}

func TestCreateGetAndSecretHandling(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()

	m := newMonitor("Shop")
	m.HeaderNames, m.AuthType, m.Method = []string{"X-Api-Key"}, coremonitor.AuthBearer, coremonitor.MethodPost
	before := time.Now()
	v, err := repo.CreateMonitor(ctx, m, sentinelSecret(), "alice")
	if err != nil {
		t.Fatalf("CreateMonitor: %v", err)
	}
	if v.ID == "" || !v.SecretConfigured || v.Live.Status != coremonitor.StatusUnknown || v.CreatedBy != "alice" {
		t.Fatalf("created = %+v", v)
	}
	if !slices.Equal(v.HeaderNames, []string{"X-Api-Key"}) {
		t.Errorf("header names = %v", v.HeaderNames)
	}
	if v.Live.NextCheckAt.Before(before.Add(-time.Second)) || v.Live.NextCheckAt.After(before.Add(11*time.Second)) {
		t.Errorf("first check due at %v, want within 10s", v.Live.NextCheckAt)
	}
	got, err := repo.GetMonitor(ctx, v.ID)
	if err != nil || got.Name != "Shop" || got.URL != m.URL {
		t.Fatalf("GetMonitor = %+v, %v", got, err)
	}
	noSentinelAnywhere(t, pool)

	// The prober gets the decrypted secret.
	if _, err := pool.Exec(ctx, `UPDATE monitor_status SET next_check_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	due, err := repo.ClaimDue(ctx, 10, time.Minute)
	if err != nil || len(due) != 1 {
		t.Fatalf("ClaimDue = %v, %v", due, err)
	}
	if due[0].SecretUnreadable || due[0].Secret.Headers["X-Api-Key"] != sentinel || due[0].Secret.Body != sentinel+"-body" {
		t.Error("the claimed monitor did not carry its decrypted secret")
	}

	// Another key cannot read it: the check is skipped, an update refused.
	other := monitor.NewRepository(pool, newKey(t))
	if _, err := pool.Exec(ctx, `UPDATE monitor_status SET next_check_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	due, err = other.ClaimDue(ctx, 10, time.Minute)
	if err != nil || len(due) != 1 || !due[0].SecretUnreadable || !due[0].Secret.Empty() {
		t.Fatalf("other key ClaimDue = %+v, %v", due, err)
	}
	_, err = other.UpdateMonitor(ctx, v.ID, func(m coremonitor.Monitor, s coremonitor.Secret) (coremonitor.Monitor, coremonitor.Secret, error) {
		return m, s, nil
	}, "bob")
	if code(err) != errs.CodeFailedPrecondition || strings.Contains(err.Error(), sentinel) {
		t.Errorf("update with another key = %v", err)
	}

	// No key: a monitor without a secret is fine, one with a secret is refused.
	nokey := monitor.NewRepository(pool, nil)
	if _, err := nokey.CreateMonitor(ctx, newMonitor("Plain"), coremonitor.Secret{}, "alice"); err != nil {
		t.Errorf("no key, no secret: %v", err)
	}
	if _, err := nokey.CreateMonitor(ctx, newMonitor("Secret"), sentinelSecret(), "alice"); code(err) != errs.CodeUnavailable {
		t.Errorf("no key, with secret = %v", err)
	}
}

func TestUpdateMonitor(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	v, err := repo.CreateMonitor(ctx, newMonitor("Api"), sentinelSecret(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE monitor_status SET next_check_at = now() + interval '1 hour', consecutive_failures = 2`); err != nil {
		t.Fatal(err)
	}

	var seen coremonitor.Secret
	got, err := repo.UpdateMonitor(ctx, v.ID, func(m coremonitor.Monitor, s coremonitor.Secret) (coremonitor.Monitor, coremonitor.Secret, error) {
		seen = s
		m.Name, m.Tags = "Api v2", []string{"prod"}
		s.Body = ""
		return m, s, nil
	}, "bob")
	if err != nil {
		t.Fatalf("UpdateMonitor: %v", err)
	}
	if seen.Headers["X-Api-Key"] != sentinel {
		t.Error("apply did not receive the stored secret")
	}
	if got.Name != "Api v2" || !slices.Equal(got.Tags, []string{"prod"}) || got.UpdatedBy != "bob" || !got.SecretConfigured {
		t.Errorf("updated = %+v", got)
	}
	if time.Until(got.Live.NextCheckAt) > 6*time.Second || got.Live.ConsecutiveFailures != 2 {
		t.Errorf("next check %v, failures %d: want brought forward, streak kept", got.Live.NextCheckAt, got.Live.ConsecutiveFailures)
	}

	// Clearing every secret field removes the ciphertext.
	got, err = repo.UpdateMonitor(ctx, v.ID, func(m coremonitor.Monitor, _ coremonitor.Secret) (coremonitor.Monitor, coremonitor.Secret, error) {
		return m, coremonitor.Secret{}, nil
	}, "bob")
	if err != nil || got.SecretConfigured {
		t.Errorf("cleared secret: %+v, %v", got.SecretConfigured, err)
	}

	_, err = repo.UpdateMonitor(ctx, v.ID, func(m coremonitor.Monitor, s coremonitor.Secret) (coremonitor.Monitor, coremonitor.Secret, error) {
		m.GroupID = "no-such-group"
		return m, s, nil
	}, "bob")
	if code(err) != errs.CodeNotFound {
		t.Errorf("unknown group = %v", err)
	}
	if _, err := repo.UpdateMonitor(ctx, "missing", nil, "bob"); code(err) != errs.CodeNotFound {
		t.Errorf("missing monitor = %v", err)
	}
}

func makeDue(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `UPDATE monitor_status SET next_check_at = now() - interval '1 second'`); err != nil {
		t.Fatal(err)
	}
}

func TestClaimRecordAndLease(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	var ids []string
	for i := range 6 {
		v, err := repo.CreateMonitor(ctx, newMonitor(fmt.Sprintf("Site%d", i)), coremonitor.Secret{}, "alice")
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, v.ID)
	}
	paused, err := repo.SetMonitorActive(ctx, ids[5], false, "alice")
	if err != nil || paused.Active {
		t.Fatalf("pause: %v", err)
	}
	makeDue(t, pool)

	// Concurrent claims never share a monitor; paused ones are never claimed.
	var mu sync.Mutex
	claimed := map[string]int{}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			due, err := repo.ClaimDue(ctx, 2, time.Minute)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, d := range due {
				claimed[d.Monitor.ID]++
			}
		}()
	}
	wg.Wait()
	if len(claimed) != 5 {
		t.Errorf("claimed %d monitors, want the 5 active ones", len(claimed))
	}
	for id, n := range claimed {
		if n != 1 || id == ids[5] {
			t.Errorf("monitor %s claimed %d times", id, n)
		}
	}
	// The lease holds them.
	if due, _ := repo.ClaimDue(ctx, 10, time.Minute); len(due) != 0 {
		t.Errorf("re-claimed %d leased monitors", len(due))
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	down := now.Add(-time.Minute)
	code503 := 503
	rec := coremonitor.CheckRecord{
		MonitorID: ids[0], At: now, Heartbeat: coremonitor.StatusDown,
		Counters:    coremonitor.Counters{Status: coremonitor.StatusDown, ConsecutiveFailures: 4, StatusSince: now, DownSince: &down},
		Result:      coremonitor.Result{StatusCode: 503, ResponseMS: 120, Failure: coremonitor.FailureHTTPStatus},
		Event:       &coremonitor.Event{Kind: coremonitor.EventDown, Time: now, StatusCode: &code503, Failure: coremonitor.FailureHTTPStatus},
		NextCheckAt: now.Add(time.Minute),
	}
	if err := repo.RecordCheck(ctx, rec); err != nil {
		t.Fatalf("RecordCheck: %v", err)
	}
	v, err := repo.GetMonitor(ctx, ids[0])
	if err != nil {
		t.Fatal(err)
	}
	if v.Live.Status != coremonitor.StatusDown || v.Live.ConsecutiveFailures != 4 || *v.Live.LastStatusCode != 503 ||
		*v.Live.LastResponseMS != 120 || v.Live.LastFailure != coremonitor.FailureHTTPStatus || !v.Live.NextCheckAt.Equal(rec.NextCheckAt) {
		t.Errorf("live = %+v", v.Live)
	}
	events, err := repo.Events(ctx, ids[0], now.Add(time.Second), 10)
	if err != nil || len(events) != 1 || events[0].Kind != coremonitor.EventDown || *events[0].StatusCode != 503 {
		t.Errorf("events = %+v, %v", events, err)
	}
	beats, err := repo.Heartbeats(ctx, ids[0], now.Add(-time.Hour), 10)
	if err != nil || len(beats) != 1 || beats[0].Status != coremonitor.StatusDown || *beats[0].StatusCode != 503 {
		t.Errorf("heartbeats = %+v, %v", beats, err)
	}

	// A network failure stores no status code or response time; cert expiry is kept.
	exp := now.Add(30 * 24 * time.Hour)
	if err := repo.RecordCheck(ctx, coremonitor.CheckRecord{MonitorID: ids[1], At: now, Heartbeat: coremonitor.StatusUp,
		Counters: coremonitor.Counters{Status: coremonitor.StatusUp, StatusSince: now}, Result: coremonitor.Result{Up: true, StatusCode: 200, ResponseMS: 80},
		NextCheckAt: now.Add(time.Minute), CertExpiresAt: &exp}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordCheck(ctx, coremonitor.CheckRecord{MonitorID: ids[1], At: now.Add(time.Second), Heartbeat: coremonitor.StatusPending,
		Counters: coremonitor.Counters{Status: coremonitor.StatusPending, ConsecutiveFailures: 1, StatusSince: now}, Result: coremonitor.Result{Failure: coremonitor.FailureDNS},
		NextCheckAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	v, _ = repo.GetMonitor(ctx, ids[1])
	if v.Live.LastStatusCode != nil || v.Live.LastResponseMS != nil || v.Live.CertExpiresAt == nil || !v.Live.CertExpiresAt.Equal(exp) {
		t.Errorf("after a dns failure: %+v", v.Live)
	}

	// Paused or deleted meanwhile: nothing is written.
	if err := repo.RecordCheck(ctx, coremonitor.CheckRecord{MonitorID: ids[5], At: now, Heartbeat: coremonitor.StatusUp,
		Counters: coremonitor.Counters{Status: coremonitor.StatusUp, StatusSince: now}, NextCheckAt: now}); err != nil {
		t.Fatal(err)
	}
	if beats, _ := repo.Heartbeats(ctx, ids[5], now.Add(-time.Hour), 10); len(beats) != 0 {
		t.Error("a paused monitor's check was recorded")
	}

	if err := repo.Reschedule(ctx, ids[2], now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, _ = repo.GetMonitor(ctx, ids[2])
	if !v.Live.NextCheckAt.Equal(now.Add(time.Hour)) {
		t.Errorf("rescheduled to %v", v.Live.NextCheckAt)
	}
}

func TestSpreadOverdueAndResume(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	for i := range 20 {
		if _, err := repo.CreateMonitor(ctx, newMonitor(fmt.Sprintf("S%d", i)), coremonitor.Secret{}, "a"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE monitor_status SET next_check_at = now() - interval '2 hours'`); err != nil {
		t.Fatal(err)
	}
	n, err := repo.SpreadOverdue(ctx, 30*time.Second)
	if err != nil || n != 20 {
		t.Fatalf("SpreadOverdue = %d, %v", n, err)
	}
	var minAt, maxAt time.Time
	var distinct int
	if err := pool.QueryRow(ctx, `SELECT min(next_check_at), max(next_check_at), count(DISTINCT next_check_at) FROM monitor_status`).
		Scan(&minAt, &maxAt, &distinct); err != nil {
		t.Fatal(err)
	}
	if time.Until(minAt) < -time.Second || time.Until(maxAt) > 31*time.Second || distinct < 15 {
		t.Errorf("spread %v..%v with %d distinct times", minAt, maxAt, distinct)
	}

	// Resuming a down monitor starts clean.
	v, _ := repo.ListMonitors(ctx)
	id := v[0].ID
	if _, err := pool.Exec(ctx, `UPDATE monitor_status SET status = 'down', consecutive_failures = 9, down_since = now() WHERE monitor_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.SetMonitorActive(ctx, id, false, "a"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.SetMonitorActive(ctx, id, true, "a")
	if err != nil || !got.Active || got.Live.Status != coremonitor.StatusUnknown || got.Live.ConsecutiveFailures != 0 || got.Live.DownSince != nil {
		t.Errorf("resumed = %+v, %v", got.Live, err)
	}
	if _, err := repo.SetMonitorActive(ctx, "missing", true, "a"); code(err) != errs.CodeNotFound {
		t.Errorf("missing = %v", err)
	}
}

func TestUptimeAndHistoryQueries(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	a, _ := repo.CreateMonitor(ctx, newMonitor("A"), coremonitor.Secret{}, "x")
	b, _ := repo.CreateMonitor(ctx, newMonitor("B"), coremonitor.Secret{}, "x")
	now := time.Now().UTC().Truncate(time.Second)
	insert := func(id string, ago time.Duration, status string, ms int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO monitor_heartbeats (monitor_id, time, status, response_ms) VALUES ($1, $2, $3, $4)`,
			id, now.Add(-ago), status, ms); err != nil {
			t.Fatal(err)
		}
	}
	// Last day: up, pending, down, unknown. Older: two more downs.
	insert(a.ID, time.Minute, "up", 100)
	insert(a.ID, 2*time.Minute, "pending", 200)
	insert(a.ID, 3*time.Minute, "down", 0)
	insert(a.ID, 4*time.Minute, "unknown", 0)
	insert(a.ID, 3*24*time.Hour, "down", 0)
	insert(a.ID, 20*24*time.Hour, "down", 0)
	for i := range 60 {
		insert(b.ID, time.Duration(i+1)*time.Minute, "up", 50)
	}

	up, err := repo.Uptime(ctx, a.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if up.Day != (coremonitor.UptimeCount{Up: 2, Counted: 3}) || up.Week != (coremonitor.UptimeCount{Up: 2, Counted: 4}) ||
		up.Month != (coremonitor.UptimeCount{Up: 2, Counted: 5}) {
		t.Errorf("uptime = %+v", up)
	}
	if p := up.Day.Percent(); p == nil || fmt.Sprintf("%.2f", *p) != "66.67" {
		t.Errorf("day percent = %v", p)
	}
	day, err := repo.UptimeSince(ctx, []string{a.ID, b.ID, "none"}, now.Add(-24*time.Hour))
	if err != nil || day[a.ID] != (coremonitor.UptimeCount{Up: 2, Counted: 3}) || day[b.ID].Counted != 60 || day["none"].Percent() != nil {
		t.Errorf("UptimeSince = %+v, %v", day, err)
	}

	recent, err := repo.RecentHeartbeats(ctx, []string{a.ID, b.ID}, 50)
	if err != nil || len(recent[a.ID]) != 6 || len(recent[b.ID]) != 50 {
		t.Fatalf("recent = %d/%d, %v", len(recent[a.ID]), len(recent[b.ID]), err)
	}
	bs := recent[b.ID]
	if !bs[len(bs)-1].Time.Equal(now.Add(-time.Minute)) || !bs[0].Time.Before(bs[1].Time) {
		t.Error("the heartbeat bar must hold the newest beats, oldest first")
	}
	beats, err := repo.Heartbeats(ctx, b.ID, now.Add(-time.Hour), 10)
	if err != nil || len(beats) != 10 || !beats[9].Time.Equal(now.Add(-time.Minute)) {
		t.Errorf("Heartbeats limit = %d, %v", len(beats), err)
	}

	hourly, err := repo.HourlyHistory(ctx, a.ID, now.Add(-30*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var total coremonitor.HourlyBucket
	for _, h := range hourly {
		total.Up += h.Up
		total.Pending += h.Pending
		total.Down += h.Down
		total.Unknown += h.Unknown
	}
	if total.Up != 1 || total.Pending != 1 || total.Down != 3 || total.Unknown != 1 {
		t.Errorf("hourly totals = %+v (%d buckets)", total, len(hourly))
	}
}

func TestEventsPruneAndDelete(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	v, _ := repo.CreateMonitor(ctx, newMonitor("Gone"), sentinelSecret(), "x")
	keep, _ := repo.CreateMonitor(ctx, newMonitor("Kept"), coremonitor.Secret{}, "x")
	now := time.Now().UTC()
	for i, ago := range []time.Duration{time.Hour, 2 * time.Hour, 400 * 24 * time.Hour} {
		if _, err := pool.Exec(ctx, `INSERT INTO monitor_events (id, monitor_id, time, kind) VALUES ($1, $2, $3, 'down')`,
			fmt.Sprintf("e%d", i), v.ID, now.Add(-ago)); err != nil {
			t.Fatal(err)
		}
	}
	events, err := repo.Events(ctx, v.ID, now, 2)
	if err != nil || len(events) != 2 || events[0].ID != "e0" {
		t.Errorf("events page = %+v, %v", events, err)
	}
	more, _ := repo.Events(ctx, v.ID, events[1].Time, 10)
	if len(more) != 1 || more[0].ID != "e2" {
		t.Errorf("next page = %+v", more)
	}
	n, err := repo.PruneEvents(ctx, now.Add(-365*24*time.Hour))
	if err != nil || n != 1 {
		t.Errorf("pruned %d, %v", n, err)
	}

	for _, id := range []string{v.ID, keep.ID} {
		if _, err := pool.Exec(ctx, `INSERT INTO monitor_heartbeats (monitor_id, time, status) VALUES ($1, now(), 'up')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO notification_state (trigger, monitor_id, subject, state, state_since)
		VALUES ('website_down', $1, '', 'firing', now())`, v.ID); err != nil {
		t.Fatalf("a monitor notification state row: %v", err)
	}
	if err := repo.DeleteMonitor(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"monitor_heartbeats", "monitor_events", "monitor_status", "notification_state"} {
		var n int
		_ = pool.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s WHERE monitor_id = $1`, table), v.ID).Scan(&n)
		if n != 0 {
			t.Errorf("%s keeps %d rows of the deleted monitor", table, n)
		}
	}
	var kept int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM monitor_heartbeats WHERE monitor_id = $1`, keep.ID).Scan(&kept)
	if kept != 1 {
		t.Error("deleting one monitor removed another's heartbeats")
	}
	if err := repo.DeleteMonitor(ctx, v.ID); code(err) != errs.CodeNotFound {
		t.Errorf("second delete = %v", err)
	}
}

func TestGroups(t *testing.T) {
	pool := newDatabase(t)
	repo := monitor.NewRepository(pool, newKey(t))
	ctx := context.Background()
	apps, err := repo.CreateGroup(ctx, coremonitor.Group{Name: "Application services", Position: 1})
	if err != nil {
		t.Fatal(err)
	}
	servers, _ := repo.CreateGroup(ctx, coremonitor.Group{Name: "Servers", Position: 0})
	if _, err := repo.CreateGroup(ctx, coremonitor.Group{Name: "Servers"}); code(err) != errs.CodeAlreadyExists {
		t.Errorf("duplicate name = %v", err)
	}
	groups, _ := repo.ListGroups(ctx)
	if len(groups) != 2 || groups[0].ID != servers.ID {
		t.Errorf("groups = %+v", groups)
	}
	apps.Name = "Apps"
	if got, err := repo.UpdateGroup(ctx, apps); err != nil || got.Name != "Apps" {
		t.Errorf("rename = %+v, %v", got, err)
	}
	if _, err := repo.UpdateGroup(ctx, coremonitor.Group{ID: "missing", Name: "x"}); code(err) != errs.CodeNotFound {
		t.Errorf("missing group = %v", err)
	}

	m := newMonitor("InApps")
	m.GroupID = apps.ID
	inApps, _ := repo.CreateMonitor(ctx, m, coremonitor.Secret{}, "x")
	m = newMonitor("InServers")
	m.GroupID = servers.ID
	if _, err := repo.CreateMonitor(ctx, m, coremonitor.Secret{}, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateMonitor(ctx, newMonitor("Loose"), coremonitor.Secret{}, "x"); err != nil {
		t.Fatal(err)
	}
	list, _ := repo.ListMonitors(ctx)
	var names []string
	for _, v := range list {
		names = append(names, v.Name)
	}
	if strings.Join(names, ",") != "InServers,InApps,Loose" {
		t.Errorf("order = %v", names)
	}
	if err := repo.DeleteGroup(ctx, apps.ID); err != nil {
		t.Fatal(err)
	}
	if v, _ := repo.GetMonitor(ctx, inApps.ID); v.GroupID != "" {
		t.Error("deleting a group must ungroup its monitors")
	}
	if err := repo.DeleteGroup(ctx, apps.ID); code(err) != errs.CodeNotFound {
		t.Errorf("second delete = %v", err)
	}
}
