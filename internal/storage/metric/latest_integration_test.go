//go:build integration

package metric_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/storage/metric"
	"github.com/jackc/pgx/v5/pgxpool"
)

func labelledEnvelope(id, nodeID, collectorID string, samples ...collect.Sample) transport.Envelope {
	return transport.Envelope{
		ID:      id,
		Origin:  transport.Origin{NodeID: nodeID, Hostname: "host-" + nodeID},
		Payload: transport.Metrics{Batch: collect.Batch{CollectorID: collectorID, Samples: samples}},
	}
}

func sample(metricName string, labels map[string]string, at time.Time, v float64) collect.Sample {
	return collect.Sample{
		Metric: metricName, Value: v, Unit: collect.UnitPercent, Kind: collect.KindGauge,
		Time: at, Labels: labels,
	}
}

func latestRowCount(t *testing.T, pool *pgxpool.Pool, nodeID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM metric_latest WHERE node_id = $1`, nodeID).Scan(&n); err != nil {
		t.Fatalf("count metric_latest: %v", err)
	}
	return n
}

func mustLatest(t *testing.T, repo *metric.Repository, nodeID string, within time.Duration) []metric.LatestValue {
	t.Helper()
	values, err := repo.Latest(context.Background(), nodeID, within)
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	return values
}

// A batch that arrives late — a replayed spool, a slow relationship — must
// not overwrite a newer value already recorded for the same series.
func TestLatestOutOfOrderBatchDoesNotRegress(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := insertBatch(t, repo, ctx, envelope("node-o", "system.memory", "system.memory.usage", now, 50)); err != nil {
		t.Fatalf("InsertBatch newer: %v", err)
	}
	if err := insertBatch(t, repo, ctx, envelope("node-o", "system.memory", "system.memory.usage", now.Add(-time.Minute), 10)); err != nil {
		t.Fatalf("InsertBatch older: %v", err)
	}

	got := mustLatest(t, repo, "node-o", 5*time.Minute)
	if len(got) != 1 || got[0].Value != 50 {
		t.Fatalf("latest = %+v, want the newer value 50", got)
	}
}

// A retried envelope is skipped by the ingested_envelopes dedup, and that
// skip must cover metric_latest too: the retry is not applied a second time
// even when it would otherwise look newer.
func TestLatestDeduplicatedEnvelopeDoesNotReupsert(t *testing.T) {
	repo, _ := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	first := envelope("node-p", "system.memory", "system.memory.usage", now.Add(-20*time.Second), 10)
	first.ID = "envelope-p1"
	if err := insertBatch(t, repo, ctx, first); err != nil {
		t.Fatalf("InsertBatch first: %v", err)
	}
	second := envelope("node-p", "system.memory", "system.memory.usage", now.Add(-10*time.Second), 20)
	second.ID = "envelope-p2"
	if err := insertBatch(t, repo, ctx, second); err != nil {
		t.Fatalf("InsertBatch second: %v", err)
	}

	// Same ID as first, carrying a sample newer than anything stored.
	replay := envelope("node-p", "system.memory", "system.memory.usage", now, 99)
	replay.ID = "envelope-p1"
	if err := insertBatch(t, repo, ctx, replay); err != nil {
		t.Fatalf("InsertBatch replay: %v", err)
	}

	got := mustLatest(t, repo, "node-p", 5*time.Minute)
	if len(got) != 1 || got[0].Value != 20 {
		t.Fatalf("latest = %+v, want 20 (the deduplicated replay must not upsert)", got)
	}
}

// An empty batch still refreshes liveness, and writes nothing to
// metric_latest.
func TestLatestEmptyBatchTouchesNodeOnly(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := insertBatch(t, repo, ctx, envelope("node-q", "system.cpu", "system.cpu.usage", now, 1)); err != nil {
		t.Fatalf("InsertBatch: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE nodes SET last_seen_at = now() - interval '1 hour' WHERE node_id = 'node-q'`); err != nil {
		t.Fatalf("backdate last_seen_at: %v", err)
	}

	if err := insertBatch(t, repo, ctx, labelledEnvelope("", "node-q", "docker.containers")); err != nil {
		t.Fatalf("InsertBatch empty: %v", err)
	}

	node, err := repo.GetNode(ctx, "node-q")
	if err != nil {
		t.Fatalf("GetNode: %v", err)
	}
	if time.Since(node.LastSeenAt) > time.Minute {
		t.Errorf("last_seen_at = %v, want refreshed by the empty batch", node.LastSeenAt)
	}
	if n := latestRowCount(t, pool, "node-q"); n != 1 {
		t.Errorf("metric_latest rows = %d, want 1 (an empty batch writes none)", n)
	}
}

// A series that stops reporting keeps its row — readers decide staleness
// from its time — until it is older than the retention cutoff.
func TestLatestVanishedSeriesStaysUntilPruned(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	recent := labelledEnvelope("", "node-r", "system.disk",
		sample("system.disk.usage", map[string]string{"mountpoint": "/data"}, now.Add(-2*time.Hour), 70))
	if err := insertBatch(t, repo, ctx, recent); err != nil {
		t.Fatalf("InsertBatch recent: %v", err)
	}
	ancient := labelledEnvelope("", "node-r", "system.disk",
		sample("system.disk.usage", map[string]string{"mountpoint": "/old"}, now.Add(-25*time.Hour), 40))
	if err := insertBatch(t, repo, ctx, ancient); err != nil {
		t.Fatalf("InsertBatch ancient: %v", err)
	}

	if got := mustLatest(t, repo, "node-r", 5*time.Minute); len(got) != 0 {
		t.Errorf("window 5m returned %+v, want nothing (both series are silent)", got)
	}
	if got := mustLatest(t, repo, "node-r", 3*time.Hour); len(got) != 1 || got[0].Labels["mountpoint"] != "/data" {
		t.Errorf("window 3h returned %+v, want the /data series", got)
	}

	removed, err := repo.PruneLatest(ctx, now.Add(-metric.LatestRetention))
	if err != nil {
		t.Fatalf("PruneLatest: %v", err)
	}
	if removed != 1 {
		t.Errorf("pruned %d rows, want 1", removed)
	}
	if got := mustLatest(t, repo, "node-r", 48*time.Hour); len(got) != 1 || got[0].Labels["mountpoint"] != "/data" {
		t.Errorf("after prune = %+v, want only the /data series", got)
	}
	if n := latestRowCount(t, pool, "node-r"); n != 1 {
		t.Errorf("metric_latest rows = %d, want 1", n)
	}
}

// The pre-metric_latest queries, verbatim, run against metric_samples. They
// are the reference the new read path must reproduce exactly.
const (
	oldLatestSQL = `
		SELECT DISTINCT ON (metric, collector_id, labels)
		       metric, collector_id, unit, kind, labels, value, time
		FROM metric_samples
		WHERE node_id = $1 AND time > now() - $2::interval
		ORDER BY metric, collector_id, labels, time DESC`
	oldLatestForMetricSQL = `
		SELECT DISTINCT ON (node_id, labels)
		       node_id, labels, value, time
		FROM metric_samples
		WHERE metric = $1 AND time > now() - $2::interval
		ORDER BY node_id, labels, time DESC`
)

func oldLatest(t *testing.T, pool *pgxpool.Pool, nodeID string, within time.Duration) []metric.LatestValue {
	t.Helper()
	rows, err := pool.Query(context.Background(), oldLatestSQL, nodeID, within)
	if err != nil {
		t.Fatalf("old Latest: %v", err)
	}
	defer rows.Close()
	out := []metric.LatestValue{}
	for rows.Next() {
		var v metric.LatestValue
		if err := rows.Scan(&v.Metric, &v.CollectorID, &v.Unit, &v.Kind, &v.Labels, &v.Value, &v.Time); err != nil {
			t.Fatalf("scan old Latest: %v", err)
		}
		if len(v.Labels) == 0 {
			v.Labels = nil
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("old Latest rows: %v", err)
	}
	return out
}

func oldLatestForMetric(t *testing.T, pool *pgxpool.Pool, metricName string, within time.Duration) []metric.FleetLatestValue {
	t.Helper()
	rows, err := pool.Query(context.Background(), oldLatestForMetricSQL, metricName, within)
	if err != nil {
		t.Fatalf("old LatestForMetric: %v", err)
	}
	defer rows.Close()
	out := []metric.FleetLatestValue{}
	for rows.Next() {
		var v metric.FleetLatestValue
		if err := rows.Scan(&v.NodeID, &v.Labels, &v.Value, &v.Time); err != nil {
			t.Fatalf("scan old LatestForMetric: %v", err)
		}
		if len(v.Labels) == 0 {
			v.Labels = nil
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("old LatestForMetric rows: %v", err)
	}
	return out
}

// Latest and LatestForMetric over metric_latest return exactly what the old
// DISTINCT ON over metric_samples returned, on a fixture covering several
// nodes, collectors, label sets, a within-batch repeat, an out-of-order
// batch, a series outside the window, and the same series from two
// collectors.
func TestLatestMatchesSampleScan(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)
	root := map[string]string{"mountpoint": "/", "device": "/dev/sda1", "fstype": "ext4"}
	efi := map[string]string{"mountpoint": "/boot/efi", "device": "/dev/sda2", "fstype": "vfat"}

	batches := []transport.Envelope{
		labelledEnvelope("f1", "node-s", "system.disk",
			sample("system.disk.usage", root, now.Add(-90*time.Second), 61),
			sample("system.disk.usage", efi, now.Add(-90*time.Second), 5),
			sample("system.disk.usage", root, now.Add(-30*time.Second), 62)),
		labelledEnvelope("f2", "node-s", "system.disk",
			sample("system.disk.usage", root, now.Add(-2*time.Minute), 1)),
		labelledEnvelope("f3", "node-s", "system.memory",
			sample("system.memory.usage", nil, now.Add(-10*time.Second), 71),
			sample("system.memory.usage", nil, now.Add(-5*time.Second), 72)),
		labelledEnvelope("f4", "node-s", "system.memory.alt",
			sample("system.memory.usage", nil, now.Add(-3*time.Second), 73)),
		labelledEnvelope("f5", "node-s", "system.cpu",
			sample("system.cpu.usage", nil, now.Add(-2*time.Hour), 99)),
		labelledEnvelope("f6", "node-t", "system.disk",
			sample("system.disk.usage", root, now.Add(-20*time.Second), 88)),
		labelledEnvelope("f7", "node-t", "system.memory",
			sample("system.memory.usage", map[string]string{}, now.Add(-4*time.Second), 40)),
	}
	for _, env := range batches {
		if err := insertBatch(t, repo, ctx, env); err != nil {
			t.Fatalf("InsertBatch %s: %v", env.ID, err)
		}
	}

	for _, within := range []time.Duration{5 * time.Minute, 3 * time.Hour, 45 * time.Second} {
		for _, node := range []string{"node-s", "node-t", "node-absent"} {
			want := oldLatest(t, pool, node, within)
			got := mustLatest(t, repo, node, within)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Latest(%s, %v):\n got  %+v\n want %+v", node, within, got, want)
			}
		}
		for _, name := range []string{"system.disk.usage", "system.memory.usage", "system.cpu.usage"} {
			want := oldLatestForMetric(t, pool, name, within)
			got, err := repo.LatestForMetric(ctx, name, within)
			if err != nil {
				t.Fatalf("LatestForMetric: %v", err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("LatestForMetric(%s, %v):\n got  %+v\n want %+v", name, within, got, want)
			}
		}
	}
}

// Two transactions upserting the same series in opposite input orders must
// not deadlock: upsertLatest sorts the series, so both lock rows in one
// order and the second simply waits for the first. This calls upsertLatest
// directly because InsertBatch also takes the node's row lock first, which
// already serializes batches for one node and would hide a lock-order bug.
func TestLatestConcurrentOppositeOrderUpsertsDoNotDeadlock(t *testing.T) {
	_, pool := newRepository(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)

	const series = 2000
	batchAt := func(at time.Time, reverse bool) collect.Batch {
		samples := make([]collect.Sample, series)
		for i := range samples {
			j := i
			if reverse {
				j = series - 1 - i
			}
			samples[i] = sample("system.disk.usage", map[string]string{"mountpoint": fmt.Sprintf("/m%04d", j)}, at, float64(j))
		}
		return collect.Batch{CollectorID: "system.disk", Samples: samples}
	}
	upsert := func(batch collect.Batch) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := metric.UpsertLatest(ctx, tx, "node-lock", batch); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	if err := upsert(batchAt(base, false)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for round := 1; round <= 10; round++ {
		at := base.Add(time.Duration(round) * time.Minute)
		start := make(chan struct{})
		errc := make(chan error, 2)
		for i, reverse := range []bool{false, true} {
			batch := batchAt(at.Add(time.Duration(i)*time.Second), reverse)
			go func() {
				<-start
				errc <- upsert(batch)
			}()
		}
		close(start)
		for range 2 {
			if err := <-errc; err != nil {
				t.Fatalf("round %d: concurrent upsert: %v", round, err)
			}
		}
	}

	if n := latestRowCount(t, pool, "node-lock"); n != series {
		t.Fatalf("metric_latest rows = %d, want %d", n, series)
	}
}
