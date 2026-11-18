package metric

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
)

// LatestRetention is how long a series that has stopped reporting keeps its
// metric_latest row. Far longer than any staleness window a reader applies,
// so a vanished series stays visible as stale rather than disappearing.
const LatestRetention = 24 * time.Hour

// seriesKeyOf returns the metric_latest key for a label set: its canonical
// JSON encoding. encoding/json sorts map keys and escapes values, so two
// label sets produce the same key exactly when they are equal — unlike a
// plain k=v join, where a value containing "," or "=" could collide with a
// different label set and silently merge two series.
func seriesKeyOf(labels map[string]string) (string, error) {
	raw, err := marshalLabels(labels)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

const upsertLatestSQL = `
	INSERT INTO metric_latest (node_id, metric, collector_id, series_key, labels, unit, kind, value, time)
	SELECT $1, s.metric, $2, s.series_key, s.labels::jsonb, s.unit, s.kind, s.value, s.time
	FROM unnest($3::text[], $4::text[], $5::text[], $6::text[], $7::text[], $8::float8[], $9::timestamptz[])
	     AS s(metric, series_key, labels, unit, kind, value, time)
	ON CONFLICT (node_id, metric, collector_id, series_key) DO UPDATE SET
		labels = EXCLUDED.labels,
		unit   = EXCLUDED.unit,
		kind   = EXCLUDED.kind,
		value  = EXCLUDED.value,
		time   = EXCLUDED.time
	WHERE EXCLUDED.time > metric_latest.time`

// upsertLatest records the newest value of each series in batch, in one
// statement. The batch is first reduced to one sample per series: a single
// INSERT ... ON CONFLICT may not touch the same row twice, and only the
// newest sample matters. The WHERE on the conflict branch keeps an
// out-of-order batch from regressing a newer stored value.
func upsertLatest(ctx context.Context, tx pgx.Tx, nodeID string, batch collect.Batch) error {
	const op = "metric.upsertLatest"

	type key struct{ metric, series string }
	newest := make(map[key]int, len(batch.Samples))
	keys := make([]string, len(batch.Samples))
	order := make([]key, 0, len(batch.Samples))

	for i, s := range batch.Samples {
		sk, err := seriesKeyOf(s.Labels)
		if err != nil {
			return errs.Wrap(err, errs.CodeInvalidArgument,
				"sample %q has labels that cannot be stored", s.Metric).
				WithOp(op).WithDetail("metric", s.Metric)
		}
		keys[i] = sk
		k := key{s.Metric, sk}
		j, seen := newest[k]
		if !seen {
			order = append(order, k)
			newest[k] = i
			continue
		}
		if !s.Time.Before(batch.Samples[j].Time) {
			newest[k] = i
		}
	}

	// Fixed lock order: every batch touches its rows sorted the same way, so
	// two concurrent transactions with overlapping series wait on each other
	// instead of deadlocking. unnest feeds the INSERT in array order.
	slices.SortFunc(order, func(a, b key) int {
		return cmp.Or(cmp.Compare(a.metric, b.metric), cmp.Compare(a.series, b.series))
	})

	n := len(order)
	metrics, seriesKeys, labels := make([]string, n), make([]string, n), make([]string, n)
	units, kinds := make([]string, n), make([]string, n)
	values, times := make([]float64, n), make([]time.Time, n)
	for i, k := range order {
		s := batch.Samples[newest[k]]
		metrics[i], seriesKeys[i], labels[i] = s.Metric, keys[newest[k]], keys[newest[k]]
		units[i], kinds[i] = string(s.Unit), string(s.Kind)
		values[i], times[i] = s.Value, s.Time
	}

	if _, err := tx.Exec(ctx, upsertLatestSQL, nodeID, batch.CollectorID,
		metrics, seriesKeys, labels, units, kinds, values, times); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record latest metric values").
			WithOp(op).WithDetail("collector_id", batch.CollectorID)
	}
	return nil
}

// PruneLatest deletes metric_latest rows whose newest sample is older than
// before, returning how many were removed. metric_latest is a plain table, so
// retention is application-driven, the same as ingested_envelopes.
func (r *Repository) PruneLatest(ctx context.Context, before time.Time) (int64, error) {
	const op = "metric.Repository.PruneLatest"
	tag, err := r.pool.Exec(ctx, `DELETE FROM metric_latest WHERE time < $1`, before)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeUnavailable, "could not prune latest metric values").WithOp(op)
	}
	return tag.RowsAffected(), nil
}
