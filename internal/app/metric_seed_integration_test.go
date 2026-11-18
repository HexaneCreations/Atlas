//go:build integration

package app_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedMetricSample writes one sample the way metric.Repository.InsertBatch
// would: into metric_samples, and into metric_latest when it is the newest
// value of its series. Fixtures that seed metrics with raw SQL use this so the
// latest-value read path sees what they seeded.
func seedMetricSample(t *testing.T, pool *pgxpool.Pool, at time.Time, nodeID, metric string, value float64, unit, labelsJSON string) {
	t.Helper()
	ctx := context.Background()

	var labels map[string]string
	if err := json.Unmarshal([]byte(labelsJSON), &labels); err != nil {
		t.Fatalf("seed labels %q: %v", labelsJSON, err)
	}
	seriesKey := "{}"
	if len(labels) > 0 {
		raw, err := json.Marshal(labels)
		if err != nil {
			t.Fatalf("seed series key: %v", err)
		}
		seriesKey = string(raw)
	}

	if _, err := pool.Exec(ctx, `
		INSERT INTO metric_samples (time, node_id, collector_id, metric, value, unit, kind, labels)
		VALUES ($1, $2, 'test', $3, $4, $5, 'gauge', $6::jsonb)`,
		at, nodeID, metric, value, unit, labelsJSON); err != nil {
		t.Fatalf("seed metric_samples: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO metric_latest (node_id, metric, collector_id, series_key, labels, unit, kind, value, time)
		VALUES ($1, $2, 'test', $3, $4::jsonb, $5, 'gauge', $6, $7)
		ON CONFLICT (node_id, metric, collector_id, series_key) DO UPDATE SET
			value = EXCLUDED.value, time = EXCLUDED.time
		WHERE EXCLUDED.time > metric_latest.time`,
		nodeID, metric, seriesKey, labelsJSON, unit, value, at); err != nil {
		t.Fatalf("seed metric_latest: %v", err)
	}
}
