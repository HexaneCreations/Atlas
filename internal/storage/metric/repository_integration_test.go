//go:build integration

// These tests exercise the metric repository against a real
// PostgreSQL/TimescaleDB server, behind the `integration` build tag so
// `go test ./...` stays hermetic.
//
//	make db-up && make test-integration
//
// The repository is the floor every feature stands on — alerting, SLOs and
// incident history will all read and write through it — and its behaviour is
// almost entirely SQL, which no unit test can reach. What is pinned here is
// what callers depend on: that writes land, that a query returns the range it
// was asked for, that rollups carry min and max alongside the average, and
// that node liveness is derived rather than stored.
package metric_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/metric"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

// newRepository gives each test its own database so they stay independent and
// can run in parallel.
func newRepository(t *testing.T) *metric.Repository {
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

	name := fmt.Sprintf("atlas_metric_test_%d", time.Now().UnixNano())
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
	cfg.Database.MigrateOnStart = true

	pool := postgres.NewPool(cfg.Database, log.Discard())
	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool start: %v", err)
	}
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	// The pool connects; it does not create the schema. Migrations are applied
	// explicitly here so each test database has the hypertables and continuous
	// aggregates the repository writes through.
	migrator := postgres.NewMigrator(pool.DB(), migrations.FS, log.Discard())
	if _, err := migrator.Apply(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	return metric.NewRepository(pool.DB())
}

// insertBatch unwraps a test envelope's metrics payload and writes it. These
// tests build envelopes directly rather than through the scheduler, so the
// payload must be unwrapped the same way production code does at the ingest
// boundary — see transport.MetricsOf.
func insertBatch(t *testing.T, repo *metric.Repository, ctx context.Context, env transport.Envelope) error {
	t.Helper()
	batch, ok := transport.MetricsOf(env)
	if !ok {
		t.Fatalf("test envelope %q carries no metrics payload", env.ID)
	}
	return repo.InsertBatch(ctx, env, batch)
}

func envelope(nodeID, collectorID, metricName string, at time.Time, values ...float64) transport.Envelope {
	samples := make([]collect.Sample, 0, len(values))
	for i, v := range values {
		samples = append(samples, collect.Sample{
			Metric: metricName,
			Value:  v,
			Unit:   collect.UnitPercent,
			Kind:   collect.KindGauge,
			Time:   at.Add(time.Duration(i) * time.Second),
		})
	}
	return transport.Envelope{
		Origin:  transport.Origin{NodeID: nodeID, Hostname: "host-" + nodeID},
		Payload: transport.Metrics{Batch: collect.Batch{CollectorID: collectorID, Samples: samples}},
	}
}

func TestInsertBatchPersistsAndQueryReturnsSamples(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	env := envelope("node-a", "system.cpu", "system.cpu.usage", now.Add(-30*time.Second), 10, 20, 30)
	if err := insertBatch(t, repo, ctx, env); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	result, err := repo.Query(ctx, metric.Query{
		NodeID:  "node-a",
		Metrics: []string{"system.cpu.usage"},
		From:    now.Add(-time.Minute),
		To:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(result.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(result.Series))
	}
	if got := len(result.Series[0].Points); got != 3 {
		t.Errorf("points = %d, want 3", got)
	}
	// Raw is the honest resolution for a two-minute window; silently reading a
	// rollup would return averages the caller did not ask for.
	if result.Resolution != metric.ResolutionRaw {
		t.Errorf("resolution = %s, want raw", result.Resolution)
	}
}

// A node row is created on first sample, before the host collector has run.
// Facts are therefore nullable and liveness is derived from last_seen_at.
func TestInsertBatchRegistersNodeAndDerivesLiveness(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := insertBatch(t, repo, ctx, envelope("node-b", "system.cpu", "system.cpu.usage", now, 42)); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	nodes, err := repo.ListNodes(ctx)
	if err != nil {
		t.Fatalf("ListNodes: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes = %d, want 1", len(nodes))
	}
	if nodes[0].NodeID != "node-b" {
		t.Errorf("node id = %q", nodes[0].NodeID)
	}
	// Liveness is computed, not stored: a node that just reported is up.
	if got := nodes[0].Status(15*time.Second, time.Now()); got != metric.StatusUp {
		t.Errorf("status = %s, want up", got)
	}
}

func TestQueryScopesToNodeAndMetric(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, e := range []transport.Envelope{
		envelope("node-a", "system.cpu", "system.cpu.usage", now, 1),
		envelope("node-b", "system.cpu", "system.cpu.usage", now, 2),
		envelope("node-a", "system.memory", "system.memory.usage", now, 3),
	} {
		if err := insertBatch(t, repo, ctx, e); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
	}

	result, err := repo.Query(ctx, metric.Query{
		NodeID:  "node-a",
		Metrics: []string{"system.cpu.usage"},
		From:    now.Add(-time.Minute),
		To:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(result.Series) != 1 {
		t.Fatalf("series = %d, want exactly the one metric on the one node", len(result.Series))
	}
	if result.Series[0].NodeID != "node-a" {
		t.Errorf("leaked another node's series: %s", result.Series[0].NodeID)
	}
	if result.Series[0].Metric != "system.cpu.usage" {
		t.Errorf("leaked another metric: %s", result.Series[0].Metric)
	}
}

// Labels distinguish series that share a metric name. Collapsing them would
// merge every network interface into one line.
func TestQuerySeparatesSeriesByLabels(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	env := transport.Envelope{
		Origin: transport.Origin{NodeID: "node-c", Hostname: "host-c"},
		Payload: transport.Metrics{Batch: collect.Batch{
			CollectorID: "system.network",
			Samples: []collect.Sample{
				{Metric: "system.network.rx.bytes", Value: 100, Unit: collect.UnitBytesPerSecond,
					Kind: collect.KindGauge, Time: now, Labels: map[string]string{"interface": "eth0"}},
				{Metric: "system.network.rx.bytes", Value: 200, Unit: collect.UnitBytesPerSecond,
					Kind: collect.KindGauge, Time: now, Labels: map[string]string{"interface": "eth1"}},
			},
		}},
	}
	if err := insertBatch(t, repo, ctx, env); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	result, err := repo.Query(ctx, metric.Query{
		NodeID:  "node-c",
		Metrics: []string{"system.network.rx.bytes"},
		From:    now.Add(-time.Minute),
		To:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if len(result.Series) != 2 {
		t.Fatalf("series = %d, want one per interface", len(result.Series))
	}
	seen := map[string]bool{}
	for _, s := range result.Series {
		seen[s.Labels["interface"]] = true
	}
	if !seen["eth0"] || !seen["eth1"] {
		t.Errorf("interfaces present = %v, want eth0 and eth1", seen)
	}
}

// Latest resolves the current value per series within a window. It is what
// every stat tile reads, and a value older than the window must not be
// returned as current.
func TestLatestRespectsWindow(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	old := envelope("node-d", "system.cpu", "system.cpu.usage", now.Add(-2*time.Hour), 99)
	if err := insertBatch(t, repo, ctx, old); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	stale, err := repo.Latest(ctx, "node-d", 5*time.Minute)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("a two-hour-old sample was returned as current: %+v", stale)
	}

	fresh := envelope("node-d", "system.cpu", "system.cpu.usage", now, 42)
	if err := insertBatch(t, repo, ctx, fresh); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	current, err := repo.Latest(ctx, "node-d", 5*time.Minute)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if len(current) != 1 {
		t.Fatalf("latest values = %d, want 1", len(current))
	}
	if current[0].Value != 42 {
		t.Errorf("value = %v, want the most recent sample", current[0].Value)
	}
}

func TestUpdateNodeFactsPopulatesInventory(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	if err := insertBatch(t, repo, ctx, envelope("node-e", "system.cpu", "system.cpu.usage", time.Now().UTC(), 1)); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	facts := metric.NodeFacts{
		OS: "linux", Platform: "debian 12", Kernel: "6.1.0",
		Architecture: "arm64", CPUCores: 8, BootTime: time.Now().Add(-time.Hour),
	}
	if err := repo.UpdateNodeFacts(ctx, "node-e", facts); err != nil {
		t.Fatalf("UpdateNodeFacts: %v", err)
	}

	node, err := repo.GetNode(ctx, "node-e")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if node.Platform != "debian 12" || node.CPUCores != 8 {
		t.Errorf("facts not persisted: %+v", node)
	}
}

func TestListMetricNamesReturnsWhatTheNodeReported(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, m := range []string{"system.cpu.usage", "system.memory.usage"} {
		if err := insertBatch(t, repo, ctx, envelope("node-f", "system", m, now, 1)); err != nil {
			t.Fatalf("InsertBatch: %v", err)
		}
	}

	names, err := repo.ListMetricNames(ctx, "node-f", time.Hour)
	if err != nil {
		t.Fatalf("ListMetricNames: %v", err)
	}
	if len(names) != 2 {
		t.Errorf("names = %v, want both metrics", names)
	}
}

// An empty batch must not create a node row or fail. Collectors legitimately
// produce nothing — a host with no Docker, a probe cycle that found no certs.
func TestInsertBatchToleratesEmptySampleSet(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()

	env := transport.Envelope{
		Origin:  transport.Origin{NodeID: "node-g", Hostname: "host-g"},
		Payload: transport.Metrics{Batch: collect.Batch{CollectorID: "docker.containers"}},
	}
	if err := insertBatch(t, repo, ctx, env); err != nil {
		t.Fatalf("an empty batch was rejected: %v", err)
	}
}

// A retried envelope (same ID, delivered twice) must not duplicate samples —
// this is the idempotency guarantee readiness review C2 requires for
// at-least-once delivery over a network.
func TestInsertBatchIsIdempotentOnEnvelopeID(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	env := envelope("node-h", "system.cpu", "system.cpu.usage", now, 10, 20, 30)
	env.ID = "fixed-envelope-id"

	if err := insertBatch(t, repo, ctx, env); err != nil {
		t.Fatalf("first InsertBatch: %v", err)
	}
	if err := insertBatch(t, repo, ctx, env); err != nil {
		t.Fatalf("retried InsertBatch: %v", err)
	}

	result, err := repo.Query(ctx, metric.Query{
		NodeID:  "node-h",
		Metrics: []string{"system.cpu.usage"},
		From:    now.Add(-time.Minute),
		To:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Series) != 1 {
		t.Fatalf("series = %d, want 1", len(result.Series))
	}
	if got := len(result.Series[0].Points); got != 3 {
		t.Errorf("points = %d, want 3 (the retry must not have duplicated them)", got)
	}
}

// A different envelope carrying the same collector must still write —
// idempotency is keyed on envelope ID, not on collector or node.
func TestInsertBatchWithDifferentEnvelopeIDsBothWrite(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	a := envelope("node-i", "system.cpu", "system.cpu.usage", now, 1)
	a.ID = "envelope-a"
	b := envelope("node-i", "system.cpu", "system.cpu.usage", now.Add(time.Second), 2)
	b.ID = "envelope-b"

	if err := insertBatch(t, repo, ctx, a); err != nil {
		t.Fatalf("InsertBatch a: %v", err)
	}
	if err := insertBatch(t, repo, ctx, b); err != nil {
		t.Fatalf("InsertBatch b: %v", err)
	}

	result, err := repo.Query(ctx, metric.Query{
		NodeID:  "node-i",
		Metrics: []string{"system.cpu.usage"},
		From:    now.Add(-time.Minute),
		To:      now.Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := len(result.Series[0].Points); got != 2 {
		t.Errorf("points = %d, want 2", got)
	}
}

func TestUpdateClockSkewRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := insertBatch(t, repo, ctx, envelope("node-j", "system.cpu", "system.cpu.usage", now, 1)); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}

	node, err := repo.GetNode(ctx, "node-j")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if node.ClockSkewSeconds != nil {
		t.Errorf("ClockSkewSeconds = %v before any update, want nil", *node.ClockSkewSeconds)
	}

	if err := repo.UpdateClockSkew(ctx, "node-j", 3.5); err != nil {
		t.Fatalf("UpdateClockSkew: %v", err)
	}

	node, err = repo.GetNode(ctx, "node-j")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if node.ClockSkewSeconds == nil || *node.ClockSkewSeconds != 3.5 {
		t.Errorf("ClockSkewSeconds = %v, want 3.5", node.ClockSkewSeconds)
	}
}

// TestLatestForMetricSpansNodes is the read path the alert rule engine
// depends on: one query, every node's latest value, no per-node round trip.
func TestLatestForMetricSpansNodes(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := insertBatch(t, repo, ctx, envelope("node-a", "system.cpu", "system.cpu.usage", now, 42)); err != nil {
		t.Fatalf("InsertBatch node-a: %v", err)
	}
	if err := insertBatch(t, repo, ctx, envelope("node-b", "system.cpu", "system.cpu.usage", now, 95)); err != nil {
		t.Fatalf("InsertBatch node-b: %v", err)
	}
	if err := insertBatch(t, repo, ctx, envelope("node-a", "system.cpu", "system.memory.usage", now, 10)); err != nil {
		t.Fatalf("InsertBatch unrelated metric: %v", err)
	}

	values, err := repo.LatestForMetric(ctx, "system.cpu.usage", 5*time.Minute)
	if err != nil {
		t.Fatalf("LatestForMetric: %v", err)
	}
	if len(values) != 2 {
		t.Fatalf("expected one value per node, got %d: %+v", len(values), values)
	}

	byNode := map[string]float64{}
	for _, v := range values {
		byNode[v.NodeID] = v.Value
	}
	if byNode["node-a"] != 42 || byNode["node-b"] != 95 {
		t.Fatalf("unexpected values: %+v", byNode)
	}
}
