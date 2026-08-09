package app

import (
	"context"
	"log/slog"
	"time"

	corealert "github.com/hexane/atlas/internal/core/alert"
	corehealthscore "github.com/hexane/atlas/internal/core/healthscore"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/postgres"
	storageinventory "github.com/hexane/atlas/internal/storage/inventory"
	"github.com/hexane/atlas/internal/storage/metric"
)

// Default provider weights. They sum to 100 for readability, but the engine
// normalises by the total weight of whichever signals were actually
// available for a node, so that is a convention here, not a constraint —
// see [corehealthscore.Engine.Score].
const (
	weightAlerts    = 25.0
	weightIncidents = 25.0
	weightMetrics   = 20.0
	weightHeartbeat = 15.0
	weightInventory = 10.0
	weightEvents    = 5.0
)

// newHealthEngine builds the health score engine over the same lazy,
// pool-backed adapters the rest of the API layer uses — see
// [lazyInventoryStore] for why construction defers to call time.
func newHealthEngine(interval time.Duration, pool *postgres.Pool, logger *slog.Logger) *corehealthscore.Engine {
	alerts := lazyAlertStore{pool: pool}

	return corehealthscore.NewEngine(corehealthscore.Options{
		Logger: logger,
		Providers: []corehealthscore.Weighted{
			{Provider: corehealthscore.AlertProvider{Rules: alerts, States: alerts}, Weight: weightAlerts},
			{Provider: corehealthscore.IncidentProvider{Store: lazyIncidentStore{pool: pool}}, Weight: weightIncidents},
			{Provider: corehealthscore.MetricsProvider{Rules: alerts, Metrics: lazyMetricSource{pool: pool}}, Weight: weightMetrics},
			{Provider: corehealthscore.HeartbeatProvider{Nodes: lazyNodeHeartbeats{pool: pool, interval: interval}}, Weight: weightHeartbeat},
			{Provider: corehealthscore.InventoryProvider{Inventory: lazyInventoryFreshness{pool: pool}}, Weight: weightInventory},
			{Provider: corehealthscore.EventProvider{Events: lazyEventStore{pool: pool}}, Weight: weightEvents},
		},
	})
}

// lazyMetricSource adapts the metric repository to [corealert.MetricSource]
// the same way [metricSourceAdapter] does, but defers repository
// construction to call time — for the same reason as [lazyInventoryStore].
type lazyMetricSource struct{ pool *postgres.Pool }

func (l lazyMetricSource) LatestForMetric(ctx context.Context, metricName string, within time.Duration) ([]corealert.FleetSample, error) {
	values, err := metric.NewRepository(l.pool.DB()).LatestForMetric(ctx, metricName, within)
	if err != nil {
		return nil, err
	}
	out := make([]corealert.FleetSample, 0, len(values))
	for _, v := range values {
		out = append(out, corealert.FleetSample{NodeID: v.NodeID, Labels: v.Labels, Value: v.Value, Time: v.Time})
	}
	return out, nil
}

// lazyNodeHeartbeats adapts the metric repository's node liveness
// classification to [corehealthscore.NodeHeartbeats], deferring repository
// construction to call time — for the same reason as [lazyInventoryStore].
type lazyNodeHeartbeats struct {
	pool     *postgres.Pool
	interval time.Duration
}

func (l lazyNodeHeartbeats) Heartbeat(ctx context.Context, nodeID string) (corehealthscore.Heartbeat, bool, error) {
	node, err := metric.NewRepository(l.pool.DB()).GetNode(ctx, nodeID)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return corehealthscore.Heartbeat{}, false, nil
		}
		return corehealthscore.Heartbeat{}, false, err
	}
	status := node.Status(l.interval, time.Now())
	return corehealthscore.Heartbeat{Status: string(status), LastSeenAt: node.LastSeenAt}, true, nil
}

// lazyInventoryFreshness adapts the inventory repository to
// [corehealthscore.InventoryFreshness], deferring repository construction
// to call time — for the same reason as [lazyInventoryStore].
type lazyInventoryFreshness struct{ pool *postgres.Pool }

func (l lazyInventoryFreshness) LastReceivedAt(ctx context.Context, nodeID string) (time.Time, bool, error) {
	return storageinventory.NewRepository(l.pool.DB()).LastReceivedAt(ctx, nodeID)
}
