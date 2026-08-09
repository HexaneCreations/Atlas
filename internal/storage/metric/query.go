package metric

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
)

// seriesKey identifies one series while rows are being grouped.
//
// Labels are compared by their raw JSON text rather than by the decoded map,
// because Go maps are not comparable and re-encoding per row to build a key
// would cost an allocation on the hottest loop in the read path. Postgres
// renders jsonb deterministically, so equal labels always produce equal text.
type seriesKey struct {
	metric      string
	collectorID string
	labelsJSON  string
}

// Query returns the series matching q.
//
// Raw samples and the two rollups are read by separate statements rather than
// one parameterised over the table name. The rollups carry min and max columns
// that raw storage does not, and a single query pretending otherwise would
// either fabricate those values or discard them.
func (r *Repository) Query(ctx context.Context, q Query) (Result, error) {
	const op = "metric.Repository.Query"

	if err := q.Normalize(time.Now()); err != nil {
		return Result{}, err
	}

	rows, err := r.queryRows(ctx, q)
	if err != nil {
		return Result{}, err
	}
	defer rows.Close()

	// Insertion order is preserved so that series come back in the order the
	// database returned them, which the ORDER BY makes deterministic.
	var (
		order  []seriesKey
		bucket = map[seriesKey]*Series{}
	)

	truncated := false
	for rows.Next() {
		var (
			ts         time.Time
			metric     string
			collector  string
			unit, kind string
			labelsRaw  []byte
			value      float64
			minValue   *float64
			maxValue   *float64
		)

		if q.Resolution == ResolutionRaw {
			err = rows.Scan(&ts, &metric, &collector, &unit, &kind, &labelsRaw, &value)
		} else {
			err = rows.Scan(&ts, &metric, &collector, &unit, &kind, &labelsRaw, &value, &minValue, &maxValue)
		}
		if err != nil {
			return Result{}, errs.Wrap(err, errs.CodeInternal, "could not read a sample").WithOp(op)
		}

		key := seriesKey{metric: metric, collectorID: collector, labelsJSON: string(labelsRaw)}
		series, ok := bucket[key]
		if !ok {
			labels, err := unmarshalLabels(labelsRaw)
			if err != nil {
				return Result{}, errs.Wrap(err, errs.CodeInternal, "could not read sample labels").WithOp(op)
			}
			u, k := unitAndKind(unit, kind)
			series = &Series{
				NodeID: q.NodeID, CollectorID: collector, Metric: metric,
				Unit: u, Kind: k, Labels: labels,
				Points: make([]Point, 0, 64),
			}
			bucket[key] = series
			order = append(order, key)
		}

		// The cap is applied per series rather than to the whole result, so
		// one dense series cannot starve the others of points.
		if len(series.Points) >= q.MaxPoints {
			truncated = true
			continue
		}
		series.Points = append(series.Points, Point{Time: ts, Value: value, Min: minValue, Max: maxValue})
	}
	if err := rows.Err(); err != nil {
		return Result{}, errs.Wrap(err, errs.CodeUnavailable, "could not read samples").WithOp(op)
	}

	out := Result{
		Series:     make([]Series, 0, len(order)),
		Resolution: q.Resolution,
		From:       q.From,
		To:         q.To,
		Truncated:  truncated,
	}
	for _, key := range order {
		out.Series = append(out.Series, *bucket[key])
	}
	return out, nil
}

// rawQuerySQL reads individual samples.
//
// The trailing ORDER BY makes both the series grouping above and the point
// ordering within each series deterministic, which matters because a chart
// drawn from unordered points is nonsense.
const rawQuerySQL = `
	SELECT time, metric, collector_id, unit, kind, labels, value
	FROM metric_samples
	WHERE node_id = $1
	  AND time >= $2 AND time <= $3
	  AND ($4::text[] IS NULL OR metric = ANY($4))
	ORDER BY metric, collector_id, labels, time`

const minuteQuerySQL = `
	SELECT bucket, metric, collector_id, unit, kind, labels, avg_value, min_value, max_value
	FROM metric_samples_1m
	WHERE node_id = $1
	  AND bucket >= $2 AND bucket <= $3
	  AND ($4::text[] IS NULL OR metric = ANY($4))
	ORDER BY metric, collector_id, labels, bucket`

const hourQuerySQL = `
	SELECT bucket, metric, collector_id, unit, kind, labels, avg_value, min_value, max_value
	FROM metric_samples_1h
	WHERE node_id = $1
	  AND bucket >= $2 AND bucket <= $3
	  AND ($4::text[] IS NULL OR metric = ANY($4))
	ORDER BY metric, collector_id, labels, bucket`

func (r *Repository) queryRows(ctx context.Context, q Query) (pgx.Rows, error) {
	const op = "metric.Repository.queryRows"

	sql := rawQuerySQL
	switch q.Resolution {
	case ResolutionMinute:
		sql = minuteQuerySQL
	case ResolutionHour:
		sql = hourQuerySQL
	}

	// A nil slice becomes SQL NULL, which the predicate treats as "no filter".
	var metrics any
	if len(q.Metrics) > 0 {
		metrics = q.Metrics
	}

	rows, err := r.pool.Query(ctx, sql, q.NodeID, q.From, q.To, metrics)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not query samples").WithOp(op)
	}
	return rows, nil
}

// LatestValue is the most recent observation of one series.
type LatestValue struct {
	Metric      string            `json:"metric"`
	CollectorID string            `json:"collector_id"`
	Unit        string            `json:"unit"`
	Kind        string            `json:"kind"`
	Labels      map[string]string `json:"labels,omitempty"`
	Value       float64           `json:"value"`
	Time        time.Time         `json:"time"`
}

// Latest returns the most recent value of every series on a node.
//
// This backs the overview page, where the question is "what is happening now"
// rather than "what happened over time". DISTINCT ON is the direct expression
// of that in PostgreSQL, and the lookback window keeps it reading only the
// newest chunk instead of scanning history for series that have stopped
// reporting.
func (r *Repository) Latest(ctx context.Context, nodeID string, within time.Duration) ([]LatestValue, error) {
	const op = "metric.Repository.Latest"

	if within <= 0 {
		within = 5 * time.Minute
	}

	const q = `
		SELECT DISTINCT ON (metric, collector_id, labels)
		       metric, collector_id, unit, kind, labels, value, time
		FROM metric_samples
		WHERE node_id = $1 AND time > now() - $2::interval
		ORDER BY metric, collector_id, labels, time DESC`

	rows, err := r.pool.Query(ctx, q, nodeID, within)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read latest values").WithOp(op)
	}
	defer rows.Close()

	out := []LatestValue{}
	for rows.Next() {
		var (
			v         LatestValue
			labelsRaw []byte
		)
		if err := rows.Scan(&v.Metric, &v.CollectorID, &v.Unit, &v.Kind, &labelsRaw, &v.Value, &v.Time); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a latest value").WithOp(op)
		}
		labels, err := unmarshalLabels(labelsRaw)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read sample labels").WithOp(op)
		}
		v.Labels = labels
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read latest values").WithOp(op)
	}
	return out, nil
}

// FleetLatestValue is a node's most recent value of one metric.
type FleetLatestValue struct {
	NodeID string            `json:"node_id"`
	Labels map[string]string `json:"labels,omitempty"`
	Value  float64           `json:"value"`
	Time   time.Time         `json:"time"`
}

// LatestForMetric returns every node's most recent value of one metric,
// across the whole fleet in a single query.
//
// This is the read path threshold-based alert rules need: "is this metric
// over its threshold, on any node" without first enumerating nodes and
// querying each one.
func (r *Repository) LatestForMetric(ctx context.Context, metric string, within time.Duration) ([]FleetLatestValue, error) {
	const op = "metric.Repository.LatestForMetric"

	if within <= 0 {
		within = 5 * time.Minute
	}

	const q = `
		SELECT DISTINCT ON (node_id, labels)
		       node_id, labels, value, time
		FROM metric_samples
		WHERE metric = $1 AND time > now() - $2::interval
		ORDER BY node_id, labels, time DESC`

	rows, err := r.pool.Query(ctx, q, metric, within)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read latest values").WithOp(op)
	}
	defer rows.Close()

	out := []FleetLatestValue{}
	for rows.Next() {
		var (
			v         FleetLatestValue
			labelsRaw []byte
		)
		if err := rows.Scan(&v.NodeID, &labelsRaw, &v.Value, &v.Time); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a latest value").WithOp(op)
		}
		labels, err := unmarshalLabels(labelsRaw)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read sample labels").WithOp(op)
		}
		v.Labels = labels
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read latest values").WithOp(op)
	}
	return out, nil
}
