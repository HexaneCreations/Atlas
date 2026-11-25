package app

import (
	"context"
	"log/slog"
	"sync"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/platform/secretbox"
	storagenotification "github.com/hexane/atlas/internal/storage/notification"
)

// notificationPipeline owns the notification subsystem's lifecycle: the
// background dispatcher that turns queued notification_deliveries rows
// into sent messages. The evaluator that decides when to queue one (step
// 9) is not wired in yet; this pipeline only drains whatever already
// lands in the queue (today, nothing does).
type notificationPipeline struct {
	logger *slog.Logger
	pool   *postgres.Pool
	// key encrypts channel secrets. Nil disables notifications entirely —
	// the dispatcher never starts, and there is nowhere safe to keep a
	// channel's secret, so none is ever read or sent.
	key *secretbox.Key

	mu     sync.RWMutex
	engine *corenotification.Engine
}

func newNotificationPipeline(logger *slog.Logger, pool *postgres.Pool, key *secretbox.Key) *notificationPipeline {
	return &notificationPipeline{logger: logger, pool: pool, key: key}
}

func (p *notificationPipeline) Name() string { return "notification.pipeline" }

func (p *notificationPipeline) Start(ctx context.Context) error {
	if p.key == nil {
		p.logger.WarnContext(ctx, "notifications are disabled: ATLAS_NOTIFICATIONS_ENCRYPTION_KEY is not set; "+
			"channel endpoints answer 503 and no notification is sent")
		return nil
	}

	repo := storagenotification.NewRepository(p.pool.DB(), p.key)
	engine := corenotification.NewEngine(corenotification.Options{
		Channels: repo, Deliveries: repo, Logger: p.logger,
		Senders: map[corenotification.ChannelType]corenotification.Sender{
			corenotification.ChannelTeams:   corenotification.NewTeamsSender(),
			corenotification.ChannelWebhook: corenotification.NewWebhookSender(),
		},
	})

	p.mu.Lock()
	p.engine = engine
	p.mu.Unlock()

	go engine.Run(ctx)

	p.logger.InfoContext(ctx, "notifications ready", slog.String("encryption_key_id", p.key.ID()))
	return nil
}

func (p *notificationPipeline) Stop(ctx context.Context) error {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine != nil {
		engine.Stop(ctx)
	}
	return nil
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
