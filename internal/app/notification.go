package app

import (
	"context"
	"log/slog"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/platform/secretbox"
	storagenotification "github.com/hexane/atlas/internal/storage/notification"
)

// notificationPipeline owns the notification subsystem's lifecycle. Nothing
// produces notifications yet: the old alert-transition path is gone, and the
// evaluator and dispatcher that replace it are not wired in. Today it only
// reports, once, whether notifications are enabled.
type notificationPipeline struct {
	logger *slog.Logger
	pool   *postgres.Pool
	// key encrypts channel secrets. Nil disables notifications entirely.
	key *secretbox.Key
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
	p.logger.InfoContext(ctx, "notifications ready", slog.String("encryption_key_id", p.key.ID()))
	return nil
}

func (p *notificationPipeline) Stop(context.Context) error { return nil }

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
