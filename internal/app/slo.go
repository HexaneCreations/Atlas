package app

import (
	"context"
	"time"

	coreslo "github.com/hexane/atlas/internal/core/slo"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/metric"
	storageslo "github.com/hexane/atlas/internal/storage/slo"
)

// newSLOEngine builds the SLO engine over the same lazy, pool-backed
// adapter style as the other engines in this file.
func newSLOEngine(pool *postgres.Pool) *coreslo.Engine {
	return coreslo.NewEngine(coreslo.Options{Samples: lazySLOSampleSource{pool: pool}})
}

// lazySLOSampleSource adapts the metric repository's range query to
// [coreslo.SampleSource], deferring repository construction to call time —
// for the same reason as [lazyInventoryStore]. Points across every series
// the metric name returns (e.g. one per network interface) are pooled into
// one slice; see [coreslo.SampleSource].
type lazySLOSampleSource struct{ pool *postgres.Pool }

func (l lazySLOSampleSource) Samples(ctx context.Context, nodeID, metricName string, from, to time.Time) ([]float64, error) {
	result, err := metric.NewRepository(l.pool.DB()).Query(ctx, metric.Query{
		NodeID: nodeID, Metrics: []string{metricName}, From: from, To: to, MaxPoints: metric.MaxAllowedPoints,
	})
	if err != nil {
		return nil, err
	}

	var values []float64
	for _, series := range result.Series {
		for _, p := range series.Points {
			values = append(values, p.Value)
		}
	}
	return values, nil
}

// lazySLOStore defers repository construction to call time, for the same
// reason as [lazyInventoryStore].
type lazySLOStore struct{ pool *postgres.Pool }

func (l lazySLOStore) repo() *storageslo.Repository { return storageslo.NewRepository(l.pool.DB()) }

func (l lazySLOStore) ListSLOs(ctx context.Context) ([]coreslo.Definition, error) {
	return l.repo().ListSLOs(ctx)
}
func (l lazySLOStore) GetSLO(ctx context.Context, id string) (coreslo.Definition, error) {
	return l.repo().GetSLO(ctx, id)
}
func (l lazySLOStore) CreateSLO(ctx context.Context, def coreslo.Definition) (coreslo.Definition, error) {
	return l.repo().CreateSLO(ctx, def)
}
func (l lazySLOStore) UpdateSLO(ctx context.Context, def coreslo.Definition) (coreslo.Definition, error) {
	return l.repo().UpdateSLO(ctx, def)
}
func (l lazySLOStore) DeleteSLO(ctx context.Context, id string) error {
	return l.repo().DeleteSLO(ctx, id)
}
