package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	coremonitor "github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/platform/secretbox"
	storagemonitor "github.com/hexane/atlas/internal/storage/monitor"
)

// monitorPipeline runs the website prober. With no monitors configured it
// only polls an empty table, so it is safe to run on every instance.
type monitorPipeline struct {
	logger *slog.Logger
	pool   *postgres.Pool
	key    *secretbox.Key
	cfg    config.Monitoring
	prober *coremonitor.Prober

	mu        sync.RWMutex
	scheduler *coremonitor.Scheduler
}

func newMonitorPipeline(logger *slog.Logger, pool *postgres.Pool, key *secretbox.Key, cfg config.Monitoring) *monitorPipeline {
	return &monitorPipeline{logger: logger, pool: pool, key: key, cfg: cfg,
		prober: coremonitor.NewProber(coremonitor.ProberOptions{AllowPrivate: cfg.AllowPrivateTargets})}
}

// Tester runs on-demand tests through the scheduler's own prober, so a test
// gets exactly the address guard, timeouts and redirect policy of a check.
func (p *monitorPipeline) Tester() *coremonitor.Tester {
	return coremonitor.NewTester(p.prober, coremonitor.MaxConcurrentTests)
}

func (p *monitorPipeline) Name() string { return "monitor.prober" }

func (p *monitorPipeline) Start(ctx context.Context) error {
	scheduler := coremonitor.NewScheduler(coremonitor.SchedulerOptions{
		Store:         storagemonitor.NewRepository(p.pool.DB(), p.key),
		Checker:       p.prober,
		Outage:        coremonitor.NewOutageDetector(p.cfg.Canaries),
		Logger:        p.logger,
		MaxConcurrent: p.cfg.MaxConcurrentChecks,
	})
	p.mu.Lock()
	p.scheduler = scheduler
	p.mu.Unlock()
	go scheduler.Run(ctx)
	p.logger.InfoContext(ctx, "website prober ready",
		slog.Bool("allow_private_targets", p.cfg.AllowPrivateTargets),
		slog.Int("max_concurrent_checks", p.cfg.MaxConcurrentChecks),
		slog.Int("canaries", len(p.cfg.Canaries)))
	return nil
}

func (p *monitorPipeline) Stop(context.Context) error {
	p.mu.RLock()
	s := p.scheduler
	p.mu.RUnlock()
	if s != nil {
		s.Stop()
	}
	return nil
}

// ProbeState implements [v1.MonitorProbe]; before Start the probe is not
// known to be offline.
func (p *monitorPipeline) ProbeState() coremonitor.ProbeState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.scheduler == nil {
		return coremonitor.ProbeState{}
	}
	return p.scheduler.ProbeState()
}

// lazyMonitorStore builds the repository per call, after the pool started.
type lazyMonitorStore struct {
	pool *postgres.Pool
	key  *secretbox.Key
}

func (l lazyMonitorStore) repo() *storagemonitor.Repository {
	return storagemonitor.NewRepository(l.pool.DB(), l.key)
}

func (l lazyMonitorStore) ListMonitors(ctx context.Context) ([]coremonitor.View, error) {
	return l.repo().ListMonitors(ctx)
}
func (l lazyMonitorStore) GetMonitor(ctx context.Context, id string) (coremonitor.View, error) {
	return l.repo().GetMonitor(ctx, id)
}
func (l lazyMonitorStore) StoredMonitor(ctx context.Context, id string) (coremonitor.Monitor, coremonitor.Secret, error) {
	return l.repo().StoredMonitor(ctx, id)
}
func (l lazyMonitorStore) CreateMonitor(ctx context.Context, m coremonitor.Monitor, s coremonitor.Secret, by string) (coremonitor.View, error) {
	return l.repo().CreateMonitor(ctx, m, s, by)
}
func (l lazyMonitorStore) UpdateMonitor(ctx context.Context, id string, apply coremonitor.UpdateFunc, by string) (coremonitor.View, error) {
	return l.repo().UpdateMonitor(ctx, id, apply, by)
}
func (l lazyMonitorStore) SetMonitorActive(ctx context.Context, id string, active bool, by string) (coremonitor.View, error) {
	return l.repo().SetMonitorActive(ctx, id, active, by)
}
func (l lazyMonitorStore) DeleteMonitor(ctx context.Context, id string) error {
	return l.repo().DeleteMonitor(ctx, id)
}
func (l lazyMonitorStore) SetMonitorChannels(ctx context.Context, id string, channelIDs []string) ([]string, error) {
	return l.repo().SetMonitorChannels(ctx, id, channelIDs)
}
func (l lazyMonitorStore) RecentHeartbeats(ctx context.Context, ids []string, n int) (map[string][]coremonitor.Heartbeat, error) {
	return l.repo().RecentHeartbeats(ctx, ids, n)
}
func (l lazyMonitorStore) UptimeSince(ctx context.Context, ids []string, since time.Time) (map[string]coremonitor.UptimeCount, error) {
	return l.repo().UptimeSince(ctx, ids, since)
}
func (l lazyMonitorStore) Uptime(ctx context.Context, id string, now time.Time) (coremonitor.Uptime, error) {
	return l.repo().Uptime(ctx, id, now)
}
func (l lazyMonitorStore) Heartbeats(ctx context.Context, id string, since time.Time, limit int) ([]coremonitor.Heartbeat, error) {
	return l.repo().Heartbeats(ctx, id, since, limit)
}
func (l lazyMonitorStore) HourlyHistory(ctx context.Context, id string, since time.Time) ([]coremonitor.HourlyBucket, error) {
	return l.repo().HourlyHistory(ctx, id, since)
}
func (l lazyMonitorStore) Events(ctx context.Context, id string, before time.Time, limit int) ([]coremonitor.Event, error) {
	return l.repo().Events(ctx, id, before, limit)
}
func (l lazyMonitorStore) ListGroups(ctx context.Context) ([]coremonitor.Group, error) {
	return l.repo().ListGroups(ctx)
}
func (l lazyMonitorStore) CreateGroup(ctx context.Context, g coremonitor.Group) (coremonitor.Group, error) {
	return l.repo().CreateGroup(ctx, g)
}
func (l lazyMonitorStore) UpdateGroup(ctx context.Context, g coremonitor.Group) (coremonitor.Group, error) {
	return l.repo().UpdateGroup(ctx, g)
}
func (l lazyMonitorStore) DeleteGroup(ctx context.Context, id string) error {
	return l.repo().DeleteGroup(ctx, id)
}
