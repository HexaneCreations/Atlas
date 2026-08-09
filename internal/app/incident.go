package app

import (
	"context"
	"log/slog"
	"sync"

	corealert "github.com/hexane/atlas/internal/core/alert"
	coreeventstore "github.com/hexane/atlas/internal/core/eventstore"
	coreincident "github.com/hexane/atlas/internal/core/incident"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/postgres"
	storageincident "github.com/hexane/atlas/internal/storage/incident"
	"github.com/hexane/atlas/internal/storage/metric"
)

// incidentPipeline runs the incident correlator: events and alert firings in,
// grouped incidents out.
//
// Registered before alert and collection so that by the time either starts,
// HandleEvent and HandleAlertTransition are backed by a live engine — the
// same registration-order guarantee [alertPipeline] relies on.
type incidentPipeline struct {
	logger *slog.Logger
	pool   *postgres.Pool

	mu     sync.RWMutex
	engine *coreincident.Engine
}

func newIncidentPipeline(logger *slog.Logger, pool *postgres.Pool) *incidentPipeline {
	return &incidentPipeline{logger: logger, pool: pool}
}

func (p *incidentPipeline) Name() string { return "incident.pipeline" }

func (p *incidentPipeline) Start(ctx context.Context) error {
	repo := storageincident.NewRepository(p.pool.DB())
	metricRepo := metric.NewRepository(p.pool.DB())
	engine := coreincident.NewEngine(coreincident.Options{
		Store: repo, Environments: nodeEnvironmentAdapter{repo: metricRepo}, Logger: p.logger,
	})

	p.mu.Lock()
	p.engine = engine
	p.mu.Unlock()

	go engine.Run(ctx)

	p.logger.InfoContext(ctx, "incident correlator ready")
	return nil
}

func (p *incidentPipeline) Stop(context.Context) error {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine != nil {
		engine.Stop()
	}
	return nil
}

// HandleEvent correlates one durably stored event. Safe to call from the
// moment this component's Start returns.
func (p *incidentPipeline) HandleEvent(ctx context.Context, rec coreeventstore.Record) {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine != nil {
		engine.HandleEvent(ctx, rec)
	}
}

// HandleAlertTransition correlates a rule firing. Wired as the alert
// engine's OnTransition hook.
func (p *incidentPipeline) HandleAlertTransition(ctx context.Context, entry corealert.HistoryEntry) {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine != nil {
		engine.HandleAlertTransition(ctx, entry)
	}
}

// nodeEnvironmentAdapter adapts the metric repository to
// [coreincident.NodeEnvironments], the environment correlation tier's read
// path — the same pattern as [metricSourceAdapter] for the alert engine.
type nodeEnvironmentAdapter struct{ repo *metric.Repository }

func (a nodeEnvironmentAdapter) Environment(ctx context.Context, nodeID string) (string, error) {
	node, err := a.repo.GetNode(ctx, nodeID)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return "", nil
		}
		return "", err
	}
	return node.Environment, nil
}

// lazyIncidentStore defers repository construction to call time, for the
// same reason as [lazyInventoryStore].
type lazyIncidentStore struct{ pool *postgres.Pool }

func (l lazyIncidentStore) repo() *storageincident.Repository {
	return storageincident.NewRepository(l.pool.DB())
}

func (l lazyIncidentStore) ListIncidents(ctx context.Context, filter coreincident.Filter) ([]coreincident.Incident, error) {
	return l.repo().ListIncidents(ctx, filter)
}
func (l lazyIncidentStore) GetDetail(ctx context.Context, id string) (coreincident.Detail, error) {
	return l.repo().GetDetail(ctx, id)
}
