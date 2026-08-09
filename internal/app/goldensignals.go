package app

import (
	"context"

	corecapacity "github.com/hexane/atlas/internal/core/capacityplanning"
	coregoldensignals "github.com/hexane/atlas/internal/core/goldensignals"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/metric"
)

const (
	metricNetworkRxErrors  = "system.network.rx.errors"
	metricNetworkTxErrors  = "system.network.tx.errors"
	metricNetworkRxDropped = "system.network.rx.dropped"
	metricNetworkTxDropped = "system.network.tx.dropped"
)

// newGoldenSignalsEngine builds the Golden Signals engine over the same
// lazy, pool-backed adapter style as the other engines in this file, and
// reuses capacityEngine directly for Saturation — [capacityplanning.Engine]
// already satisfies [coregoldensignals.CapacitySource], so no adapter is
// needed for that dependency.
func newGoldenSignalsEngine(pool *postgres.Pool, capacityEngine *corecapacity.Engine) *coregoldensignals.Engine {
	return coregoldensignals.NewEngine(coregoldensignals.Options{
		Snapshots: lazyGoldenSignalsSnapshotSource{pool: pool},
		Capacity:  capacityEngine,
		Providers: []coregoldensignals.Provider{
			coregoldensignals.LatencyProvider{},
			coregoldensignals.TrafficProvider{},
			coregoldensignals.ErrorsProvider{},
			coregoldensignals.SaturationProvider{},
		},
	})
}

// lazyGoldenSignalsSnapshotSource adapts the metric repository to
// [coregoldensignals.SnapshotSource], deferring repository construction to
// call time — for the same reason as [lazyInventoryStore].
type lazyGoldenSignalsSnapshotSource struct{ pool *postgres.Pool }

func (l lazyGoldenSignalsSnapshotSource) Snapshot(ctx context.Context, nodeID string) (coregoldensignals.Snapshot, error) {
	repo := metric.NewRepository(l.pool.DB())

	values, err := repo.Latest(ctx, nodeID, usageLookback)
	if err != nil {
		return coregoldensignals.Snapshot{}, err
	}

	s := coregoldensignals.Snapshot{NodeID: nodeID}
	for _, v := range values {
		switch v.Metric {
		case metricNetworkRxBytes:
			s.NetworkRxBytesPerSec, s.HasNetworkTraffic = s.NetworkRxBytesPerSec+v.Value, true
		case metricNetworkTxBytes:
			s.NetworkTxBytesPerSec, s.HasNetworkTraffic = s.NetworkTxBytesPerSec+v.Value, true
		case metricNetworkRxErrors, metricNetworkTxErrors, metricNetworkRxDropped, metricNetworkTxDropped:
			s.NetworkErrorsPerSec, s.HasNetworkErrors = s.NetworkErrorsPerSec+v.Value, true
		}
	}
	return s, nil
}
