package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	corealert "github.com/hexane/atlas/internal/core/alert"
	coreeventstore "github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/platform/postgres"
	storagealert "github.com/hexane/atlas/internal/storage/alert"
	"github.com/hexane/atlas/internal/storage/metric"
)

// alertPipeline runs the alert rule engine: a periodic threshold evaluator
// plus an event-rule hook other pipelines call directly.
//
// It is registered before collection and fleet so that by the time either
// starts, HandleEvent is backed by a live engine — see the registration
// order note in [New].
type alertPipeline struct {
	logger       *slog.Logger
	pool         *postgres.Pool
	onTransition func(context.Context, corealert.HistoryEntry)

	mu     sync.RWMutex
	engine *corealert.Engine
}

func newAlertPipeline(logger *slog.Logger, pool *postgres.Pool, onTransition func(context.Context, corealert.HistoryEntry)) *alertPipeline {
	return &alertPipeline{logger: logger, pool: pool, onTransition: onTransition}
}

func (p *alertPipeline) Name() string { return "alert.pipeline" }

func (p *alertPipeline) Start(ctx context.Context) error {
	repo := storagealert.NewRepository(p.pool.DB())
	metricRepo := metric.NewRepository(p.pool.DB())

	engine := corealert.NewEngine(corealert.Options{
		Store: repo, Metrics: metricSourceAdapter{repo: metricRepo}, Logger: p.logger,
		OnTransition: p.onTransition,
	})

	p.mu.Lock()
	p.engine = engine
	p.mu.Unlock()

	go engine.Run(ctx)

	p.logger.InfoContext(ctx, "alert rule engine ready")
	return nil
}

func (p *alertPipeline) Stop(context.Context) error {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine != nil {
		engine.Stop()
	}
	return nil
}

// HandleEvent evaluates event rules against one durably stored record. Safe
// to call from the moment this component's Start returns, which registration
// order in [New] guarantees happens before collection or fleet can produce an
// event.
func (p *alertPipeline) HandleEvent(ctx context.Context, rec coreeventstore.Record) {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine != nil {
		engine.HandleEvent(ctx, rec)
	}
}

// metricSourceAdapter adapts the metric repository to [corealert.MetricSource],
// the three lines that keep core/alert from depending on storage/metric.
type metricSourceAdapter struct{ repo *metric.Repository }

func (a metricSourceAdapter) LatestForMetric(ctx context.Context, metricName string, within time.Duration) ([]corealert.FleetSample, error) {
	values, err := a.repo.LatestForMetric(ctx, metricName, within)
	if err != nil {
		return nil, err
	}
	out := make([]corealert.FleetSample, 0, len(values))
	for _, v := range values {
		out = append(out, corealert.FleetSample{NodeID: v.NodeID, Labels: v.Labels, Value: v.Value, Time: v.Time})
	}
	return out, nil
}

// lazyAlertStore defers repository construction to call time, for the same
// reason as [lazyInventoryStore].
type lazyAlertStore struct{ pool *postgres.Pool }

func (l lazyAlertStore) repo() *storagealert.Repository {
	return storagealert.NewRepository(l.pool.DB())
}

func (l lazyAlertStore) ListRules(ctx context.Context) ([]corealert.Rule, error) {
	return l.repo().ListRules(ctx)
}
func (l lazyAlertStore) GetRule(ctx context.Context, id string) (corealert.Rule, error) {
	return l.repo().GetRule(ctx, id)
}
func (l lazyAlertStore) CreateRule(ctx context.Context, rule corealert.Rule) (corealert.Rule, error) {
	return l.repo().CreateRule(ctx, rule)
}
func (l lazyAlertStore) UpdateRule(ctx context.Context, rule corealert.Rule) (corealert.Rule, error) {
	return l.repo().UpdateRule(ctx, rule)
}
func (l lazyAlertStore) DeleteRule(ctx context.Context, id string) error {
	return l.repo().DeleteRule(ctx, id)
}
func (l lazyAlertStore) ListActiveStates(ctx context.Context) ([]corealert.AlertState, error) {
	return l.repo().ListActiveStates(ctx)
}
func (l lazyAlertStore) QueryHistory(ctx context.Context, filter corealert.HistoryFilter) ([]corealert.HistoryEntry, error) {
	return l.repo().QueryHistory(ctx, filter)
}
