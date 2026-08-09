package app

import (
	"context"
	"log/slog"
	"sync"
	"time"

	corealert "github.com/hexane/atlas/internal/core/alert"
	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/postgres"
	storagenotification "github.com/hexane/atlas/internal/storage/notification"
)

// notificationPipeline runs the notification dispatcher: alert transitions
// in, enqueued deliveries out, delivered on its own schedule.
//
// Registered before alert, the same registration-order guarantee
// [incidentPipeline] relies on, so Notify is backed by a live engine before
// the alert engine can produce a transition to notify about.
type notificationPipeline struct {
	logger *slog.Logger
	pool   *postgres.Pool

	mu     sync.RWMutex
	engine *corenotification.Engine
}

func newNotificationPipeline(logger *slog.Logger, pool *postgres.Pool) *notificationPipeline {
	return &notificationPipeline{logger: logger, pool: pool}
}

func (p *notificationPipeline) Name() string { return "notification.pipeline" }

func (p *notificationPipeline) Start(ctx context.Context) error {
	store := lazyNotificationStore{pool: p.pool}
	engine := corenotification.NewEngine(corenotification.Options{
		Channels: store, Deliveries: store, Logger: p.logger,
		Senders: map[corenotification.ChannelType]corenotification.Sender{
			corenotification.ChannelWebhook: corenotification.NewWebhookSender(),
		},
	})

	p.mu.Lock()
	p.engine = engine
	p.mu.Unlock()

	go engine.Run(ctx)

	p.logger.InfoContext(ctx, "notification dispatcher ready")
	return nil
}

func (p *notificationPipeline) Stop(context.Context) error {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine != nil {
		engine.Stop()
	}
	return nil
}

// HandleAlertTransition enqueues a notification for a rule firing or
// resolving. Wired as the alert engine's OnTransition hook, alongside
// [incidentPipeline.HandleAlertTransition] — this only ever does a fast
// durable write, never a network call, so it cannot make the alert engine
// wait on, or fail because of, notification delivery.
func (p *notificationPipeline) HandleAlertTransition(ctx context.Context, entry corealert.HistoryEntry) {
	p.mu.RLock()
	engine := p.engine
	p.mu.RUnlock()
	if engine == nil {
		return
	}

	title := "Alert resolved"
	if entry.State == corealert.StateFiring {
		title = "Alert firing"
	}
	err := engine.Notify(ctx, corenotification.Event{
		ID: entry.ID, Trigger: corenotification.TriggerAlertTransition,
		NodeID: entry.NodeID, Severity: string(entry.Severity), Title: title, Message: entry.Message, Time: entry.Time,
	})
	if err != nil {
		p.logger.ErrorContext(ctx, "could not enqueue a notification for an alert transition",
			slog.String("history_id", entry.ID), slog.String("error", err.Error()))
	}
}

// lazyNotificationStore defers repository construction to call time, for
// the same reason as [lazyInventoryStore]. It satisfies both
// [corenotification.ChannelStore] and [corenotification.DeliveryStore]:
// one Postgres repository backs both, the same as
// [storagenotification.Repository] itself.
type lazyNotificationStore struct{ pool *postgres.Pool }

func (l lazyNotificationStore) repo() *storagenotification.Repository {
	return storagenotification.NewRepository(l.pool.DB())
}

func (l lazyNotificationStore) ListChannels(ctx context.Context) ([]corenotification.Channel, error) {
	return l.repo().ListChannels(ctx)
}
func (l lazyNotificationStore) GetChannel(ctx context.Context, id string) (corenotification.Channel, error) {
	return l.repo().GetChannel(ctx, id)
}
func (l lazyNotificationStore) CreateChannel(ctx context.Context, c corenotification.Channel) (corenotification.Channel, error) {
	return l.repo().CreateChannel(ctx, c)
}
func (l lazyNotificationStore) UpdateChannel(ctx context.Context, c corenotification.Channel) (corenotification.Channel, error) {
	return l.repo().UpdateChannel(ctx, c)
}
func (l lazyNotificationStore) DeleteChannel(ctx context.Context, id string) error {
	return l.repo().DeleteChannel(ctx, id)
}

func (l lazyNotificationStore) Enqueue(ctx context.Context, event corenotification.Event, channels []corenotification.Channel) error {
	return l.repo().Enqueue(ctx, event, channels)
}
func (l lazyNotificationStore) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]corenotification.Delivery, error) {
	return l.repo().DueDeliveries(ctx, now, limit)
}
func (l lazyNotificationStore) MarkDelivered(ctx context.Context, id string, at time.Time) error {
	return l.repo().MarkDelivered(ctx, id, at)
}
func (l lazyNotificationStore) MarkRetry(ctx context.Context, id string, attempts int, next time.Time, lastErr string) error {
	return l.repo().MarkRetry(ctx, id, attempts, next, lastErr)
}
func (l lazyNotificationStore) MarkExhausted(ctx context.Context, id string, attempts int, lastErr string) error {
	return l.repo().MarkExhausted(ctx, id, attempts, lastErr)
}
func (l lazyNotificationStore) ListDeliveries(ctx context.Context, filter corenotification.DeliveryFilter) ([]corenotification.Delivery, error) {
	return l.repo().ListDeliveries(ctx, filter)
}
