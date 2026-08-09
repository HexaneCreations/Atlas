// Package notification delivers events to configured external channels —
// a webhook today — through a durable, retried queue.
//
// Nothing here decides what is worth notifying about; a caller (the alert
// engine's OnTransition hook today) builds an [Event] and calls
// [Engine.Notify]. Notify only enqueues: it does one fast, durable write
// and returns, the same way the incident correlator's own OnTransition
// hook already writes to Postgres synchronously. The actual network
// delivery — the part that can be slow or fail — happens later, on
// [Engine.Run]'s own schedule, in a goroutine nothing else waits on.
package notification

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Trigger identifies what produced an [Event]. Only alert transitions
// exist today; incident and SLO triggers are future values on this same
// type — Event, the store, and Engine do not change to add them.
type Trigger string

const TriggerAlertTransition Trigger = "alert_transition"

// Event is one thing worth notifying about, reduced to what any channel
// needs regardless of what triggered it.
type Event struct {
	// ID is the stable id of the source record (an alert.HistoryEntry.ID
	// today) — the dedup key a retry can never duplicate past.
	ID       string
	Trigger  Trigger
	NodeID   string
	Severity string
	Title    string
	Message  string
	Time     time.Time
}

// ChannelType identifies a delivery mechanism.
type ChannelType string

const ChannelWebhook ChannelType = "webhook"

// WebhookConfig configures a [ChannelWebhook] channel.
type WebhookConfig struct {
	URL string
	// Secret HMAC-signs each delivery when set. Never returned by the API
	// — see internal/api/v1/notifications.go.
	Secret string
}

// Channel is a configured delivery destination.
type Channel struct {
	ID      string
	Name    string
	Type    ChannelType
	Enabled bool
	// Webhook holds configuration for Type == ChannelWebhook. A future
	// channel type gets its own field, the same way alert.Rule branches
	// its threshold/event fields on Kind.
	Webhook WebhookConfig

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate reports whether the channel is well formed for its type.
func (c Channel) Validate() error {
	const op = "notification.Channel.Validate"

	if c.Name == "" {
		return errs.New(errs.CodeInvalidArgument, "a name is required").WithOp(op)
	}
	switch c.Type {
	case ChannelWebhook:
		if c.Webhook.URL == "" {
			return errs.New(errs.CodeInvalidArgument, "a webhook channel requires a url").WithOp(op)
		}
	default:
		return errs.New(errs.CodeInvalidArgument, "unknown channel type %q", c.Type).WithOp(op)
	}
	return nil
}

// Status is where a delivery attempt stands. It stays Pending across
// retries — only a terminal outcome moves it to Delivered or Failed — so
// "due for retry" is just "pending, past its next attempt time."
type Status string

const (
	StatusPending   Status = "pending"
	StatusDelivered Status = "delivered"
	StatusFailed    Status = "failed"
)

// Delivery is one (Event, Channel) delivery attempt record — the durable
// unit retries operate on, so a retry can never create a duplicate
// notification, only advance this same row.
type Delivery struct {
	ID        string
	EventID   string
	ChannelID string

	Trigger   Trigger
	NodeID    string
	Severity  string
	Title     string
	Message   string
	EventTime time.Time

	Status        Status
	Attempts      int
	NextAttemptAt time.Time
	LastError     string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChannelStore persists notification channels. Satisfied by
// [github.com/hexane/atlas/internal/storage/notification.Repository].
type ChannelStore interface {
	ListChannels(ctx context.Context) ([]Channel, error)
	GetChannel(ctx context.Context, id string) (Channel, error)
	CreateChannel(ctx context.Context, c Channel) (Channel, error)
	UpdateChannel(ctx context.Context, c Channel) (Channel, error)
	DeleteChannel(ctx context.Context, id string) error
}

// DeliveryStore persists delivery attempts — the durable retry queue.
// Satisfied by [github.com/hexane/atlas/internal/storage/notification.Repository].
type DeliveryStore interface {
	// Enqueue inserts one pending delivery per channel for event.
	// Idempotent on (event.ID, channel.ID): offering the same event to the
	// same channel twice is a no-op, the same convention
	// [github.com/hexane/atlas/internal/core/incident.Store.AddMember] uses.
	Enqueue(ctx context.Context, event Event, channels []Channel) error
	// DueDeliveries returns pending deliveries at or past their next
	// attempt time, oldest first, up to limit.
	DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error)
	MarkDelivered(ctx context.Context, id string, at time.Time) error
	// MarkRetry records a failed attempt that has not yet exhausted its
	// retries: attempts increments, the row stays Pending, next retry at
	// nextAttempt.
	MarkRetry(ctx context.Context, id string, attempts int, nextAttempt time.Time, lastErr string) error
	// MarkExhausted records a failed attempt that has used its last retry.
	MarkExhausted(ctx context.Context, id string, attempts int, lastErr string) error
	// ListDeliveries returns recent deliveries, newest first — the
	// observability surface for delivery status.
	ListDeliveries(ctx context.Context, filter DeliveryFilter) ([]Delivery, error)
}

// DeliveryFilter selects deliveries for [DeliveryStore.ListDeliveries].
type DeliveryFilter struct {
	ChannelID string
	Status    Status
	Limit     int
}
