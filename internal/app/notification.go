package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/platform/secretbox"
	"github.com/hexane/atlas/internal/storage/metric"
	storagenotification "github.com/hexane/atlas/internal/storage/notification"
)

// notificationPipeline owns the notification subsystem's lifecycle: the
// evaluator that watches every trigger and the background dispatcher that
// turns what it queues into sent messages.
type notificationPipeline struct {
	logger *slog.Logger
	pool   *postgres.Pool
	// key encrypts channel secrets. Nil disables notifications entirely —
	// neither the evaluator nor the dispatcher starts, and there is
	// nowhere safe to keep a channel's secret, so none is ever read or
	// sent.
	key *secretbox.Key
	// localNodeID is this server's own node id (internal/platform/hostid,
	// resolved once in app.New) — the evaluator never evaluates it, the
	// same way the local collection pipeline is the one writing its data
	// in the first place. See evaluator.go's EvalNode doc comment.
	localNodeID string
	// defaultInterval is collection.default_interval, which a collector
	// declaring no interval of its own runs at.
	defaultInterval time.Duration

	mu        sync.RWMutex
	engine    *corenotification.Engine
	evaluator *corenotification.Evaluator
}

func newNotificationPipeline(logger *slog.Logger, pool *postgres.Pool, key *secretbox.Key, localNodeID string, defaultInterval time.Duration) *notificationPipeline {
	return &notificationPipeline{logger: logger, pool: pool, key: key, localNodeID: localNodeID, defaultInterval: defaultInterval}
}

func (p *notificationPipeline) Name() string { return "notification.pipeline" }

func (p *notificationPipeline) Start(ctx context.Context) error {
	if p.key == nil {
		p.logger.WarnContext(ctx, "notifications are disabled: ATLAS_NOTIFICATIONS_ENCRYPTION_KEY is not set; "+
			"channel endpoints answer 503 and no notification is sent")
		return nil
	}

	intervals, err := registeredIntervals(ctx, p.defaultInterval, p.logger)
	if err != nil {
		return err
	}

	repo := storagenotification.NewRepository(p.pool.DB(), p.key)
	engine := corenotification.NewEngine(corenotification.Options{
		Channels: repo, Deliveries: repo, Logger: p.logger,
		Senders: map[corenotification.ChannelType]corenotification.Sender{
			corenotification.ChannelTeams:   corenotification.NewTeamsSender(),
			corenotification.ChannelWebhook: corenotification.NewWebhookSender(),
		},
	})

	metricRepo := metric.NewRepository(p.pool.DB())
	evaluator := corenotification.NewEvaluator(corenotification.EvaluatorOptions{
		Rules: repo, States: repo, Notifier: engine, Logger: p.logger,
		Nodes:       evaluatorNodeAdapter{repo: metricRepo},
		Metrics:     evaluatorMetricAdapter{repo: metricRepo},
		LocalNodeID: p.localNodeID,

		ContainerCollectorInterval: intervals[containerCollectorID],
		ServiceCollectorInterval:   intervals[serviceCollectorID],
	})

	p.mu.Lock()
	p.engine, p.evaluator = engine, evaluator
	p.mu.Unlock()

	go engine.Run(ctx)
	go evaluator.Run(ctx)

	p.logger.InfoContext(ctx, "notifications ready", slog.String("encryption_key_id", p.key.ID()))
	return nil
}

func (p *notificationPipeline) Stop(ctx context.Context) error {
	p.mu.RLock()
	engine, evaluator := p.engine, p.evaluator
	p.mu.RUnlock()
	// The evaluator first: its own work is a quick DB read/write with no
	// real bound needed (see its Stop's doc comment), and stopping it
	// before the dispatcher means nothing new gets queued while the
	// dispatcher is draining what is already there.
	if evaluator != nil {
		evaluator.Stop()
	}
	if engine != nil {
		engine.Stop(ctx)
	}
	return nil
}

// runningEngine returns the dispatcher, or a 503 before Start.
func (p *notificationPipeline) runningEngine() (*corenotification.Engine, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.engine == nil {
		return nil, errs.New(errs.CodeUnavailable, "notifications are not running").WithOp("app.notificationPipeline")
	}
	return p.engine, nil
}

// SendTest implements [v1.NotificationTester].
func (p *notificationPipeline) SendTest(ctx context.Context, channelID string) (corenotification.SendTestOutcome, error) {
	engine, err := p.runningEngine()
	if err != nil {
		return corenotification.SendTestOutcome{}, err
	}
	return engine.SendTest(ctx, channelID)
}

// PrepareDraftTest implements [v1.NotificationTester].
func (p *notificationPipeline) PrepareDraftTest(ctx context.Context, in corenotification.ChannelInput, channelID string) (*corenotification.DraftTest, error) {
	engine, err := p.runningEngine()
	if err != nil {
		return nil, err
	}
	return engine.PrepareDraftTest(ctx, in, channelID)
}

// evaluatorNodeAdapter adapts [*metric.Repository] to [corenotification.
// NodeStore] — the three lines that keep core/notification from depending
// on storage/metric, the same pattern [metricSourceAdapter] in alert.go
// already uses for corealert.
type evaluatorNodeAdapter struct{ repo *metric.Repository }

func (a evaluatorNodeAdapter) ListNodes(ctx context.Context) ([]corenotification.EvalNode, error) {
	nodes, err := a.repo.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]corenotification.EvalNode, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, corenotification.EvalNode{
			NodeID: n.NodeID, LastSeenAt: n.LastSeenAt,
			ArchivedAt: n.ArchivedAt, AlertsMutedUntil: n.AlertsMutedUntil,
		})
	}
	return out, nil
}

// evaluatorMetricAdapter adapts [*metric.Repository] to [corenotification.
// MetricSource].
type evaluatorMetricAdapter struct{ repo *metric.Repository }

func (a evaluatorMetricAdapter) LatestForMetric(ctx context.Context, name string, within time.Duration) ([]corenotification.MetricSample, error) {
	values, err := a.repo.LatestForMetric(ctx, name, within)
	if err != nil {
		return nil, err
	}
	out := make([]corenotification.MetricSample, 0, len(values))
	for _, v := range values {
		out = append(out, corenotification.MetricSample{NodeID: v.NodeID, Labels: v.Labels, Value: v.Value, Time: v.Time})
	}
	return out, nil
}

// lazyNotificationStore defers repository construction to call time, for
// the same reason as [lazyInventoryStore].
type lazyNotificationStore struct {
	pool *postgres.Pool
	key  *secretbox.Key
}

func (l lazyNotificationStore) repo() *storagenotification.Repository {
	return storagenotification.NewRepository(l.pool.DB(), l.key)
}

func (l lazyNotificationStore) ListChannels(ctx context.Context) ([]corenotification.Channel, error) {
	return l.repo().ListChannels(ctx)
}
func (l lazyNotificationStore) GetChannel(ctx context.Context, id string) (corenotification.Channel, error) {
	return l.repo().GetChannel(ctx, id)
}
func (l lazyNotificationStore) ChannelSecret(ctx context.Context, channelID string) (string, error) {
	return l.repo().ChannelSecret(ctx, channelID)
}
func (l lazyNotificationStore) CreateChannel(ctx context.Context, in corenotification.ChannelInput) (corenotification.Channel, error) {
	return l.repo().CreateChannel(ctx, in)
}
func (l lazyNotificationStore) UpdateChannel(ctx context.Context, id string, in corenotification.ChannelInput) (corenotification.Channel, error) {
	return l.repo().UpdateChannel(ctx, id, in)
}
func (l lazyNotificationStore) DeleteChannel(ctx context.Context, id string) error {
	return l.repo().DeleteChannel(ctx, id)
}
func (l lazyNotificationStore) ListDeliveries(ctx context.Context, filter corenotification.DeliveryFilter) (corenotification.DeliveryPage, error) {
	return l.repo().ListDeliveries(ctx, filter)
}
func (l lazyNotificationStore) MuteNode(ctx context.Context, nodeID string, until time.Time) (corenotification.NodeAlerts, error) {
	return l.repo().MuteNode(ctx, nodeID, until)
}
func (l lazyNotificationStore) EndMuteExplicitly(ctx context.Context, nodeID string) (bool, error) {
	return l.repo().EndMuteExplicitly(ctx, nodeID)
}
func (l lazyNotificationStore) ArchiveNode(ctx context.Context, nodeID string, at time.Time) (corenotification.NodeAlerts, error) {
	return l.repo().ArchiveNode(ctx, nodeID, at)
}
func (l lazyNotificationStore) UnarchiveNode(ctx context.Context, nodeID string) error {
	return l.repo().UnarchiveNode(ctx, nodeID)
}
