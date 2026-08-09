// Package notification is the PostgreSQL-backed implementation of
// [github.com/hexane/atlas/internal/core/notification.ChannelStore] and
// [github.com/hexane/atlas/internal/core/notification.DeliveryStore].
package notification

import (
	"context"
	"fmt"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultDeliveryListLimit = 100
	maxDeliveryListLimit     = 500
)

// Repository reads and writes notification channels and deliveries.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const channelColumns = `id, name, type, enabled, webhook_url, webhook_secret, created_at, updated_at`

func scanChannel(row pgx.Row) (corenotification.Channel, error) {
	var c corenotification.Channel
	var typ string
	err := row.Scan(&c.ID, &c.Name, &typ, &c.Enabled, &c.Webhook.URL, &c.Webhook.Secret, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return corenotification.Channel{}, err
	}
	c.Type = corenotification.ChannelType(typ)
	return c, nil
}

// ListChannels returns every channel, newest first.
func (r *Repository) ListChannels(ctx context.Context) ([]corenotification.Channel, error) {
	const op = "notification.Repository.ListChannels"

	rows, err := r.pool.Query(ctx, `SELECT `+channelColumns+` FROM notification_channels ORDER BY created_at DESC`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification channels").WithOp(op)
	}
	defer rows.Close()

	channels := []corenotification.Channel{}
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a notification channel").WithOp(op)
		}
		channels = append(channels, c)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification channels").WithOp(op)
	}
	return channels, nil
}

// GetChannel returns one channel by id.
func (r *Repository) GetChannel(ctx context.Context, channelID string) (corenotification.Channel, error) {
	const op = "notification.Repository.GetChannel"

	row := r.pool.QueryRow(ctx, `SELECT `+channelColumns+` FROM notification_channels WHERE id = $1`, channelID)
	c, err := scanChannel(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return corenotification.Channel{}, errs.New(errs.CodeNotFound, "notification channel not found").WithOp(op).WithDetail("id", channelID)
		}
		return corenotification.Channel{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification channel").WithOp(op)
	}
	return c, nil
}

// CreateChannel persists a new channel, assigning it an id.
func (r *Repository) CreateChannel(ctx context.Context, c corenotification.Channel) (corenotification.Channel, error) {
	const op = "notification.Repository.CreateChannel"

	c.ID = id.New()
	now := time.Now()
	c.CreatedAt, c.UpdatedAt = now, now

	const q = `
		INSERT INTO notification_channels (id, name, type, enabled, webhook_url, webhook_secret, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`

	_, err := r.pool.Exec(ctx, q, c.ID, c.Name, string(c.Type), c.Enabled, c.Webhook.URL, c.Webhook.Secret, c.CreatedAt, c.UpdatedAt)
	if err != nil {
		return corenotification.Channel{}, errs.Wrap(err, errs.CodeUnavailable, "could not create the notification channel").WithOp(op)
	}
	return c, nil
}

// UpdateChannel replaces an existing channel's fields.
func (r *Repository) UpdateChannel(ctx context.Context, c corenotification.Channel) (corenotification.Channel, error) {
	const op = "notification.Repository.UpdateChannel"

	c.UpdatedAt = time.Now()

	const q = `
		UPDATE notification_channels SET
			name = $2, type = $3, enabled = $4, webhook_url = $5, webhook_secret = $6, updated_at = $7
		WHERE id = $1
		RETURNING created_at`

	row := r.pool.QueryRow(ctx, q, c.ID, c.Name, string(c.Type), c.Enabled, c.Webhook.URL, c.Webhook.Secret, c.UpdatedAt)
	if err := row.Scan(&c.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return corenotification.Channel{}, errs.New(errs.CodeNotFound, "notification channel not found").WithOp(op).WithDetail("id", c.ID)
		}
		return corenotification.Channel{}, errs.Wrap(err, errs.CodeUnavailable, "could not update the notification channel").WithOp(op)
	}
	return c, nil
}

// DeleteChannel removes a channel. Its deliveries cascade with it.
func (r *Repository) DeleteChannel(ctx context.Context, channelID string) error {
	const op = "notification.Repository.DeleteChannel"

	tag, err := r.pool.Exec(ctx, `DELETE FROM notification_channels WHERE id = $1`, channelID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the notification channel").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "notification channel not found").WithOp(op).WithDetail("id", channelID)
	}
	return nil
}

const deliveryColumns = `id, event_id, channel_id, trigger, node_id, severity, title, message,
	event_time, status, attempts, next_attempt_at, last_error, created_at, updated_at`

func scanDelivery(row pgx.Row) (corenotification.Delivery, error) {
	var d corenotification.Delivery
	var trigger, status string
	err := row.Scan(&d.ID, &d.EventID, &d.ChannelID, &trigger, &d.NodeID, &d.Severity, &d.Title, &d.Message,
		&d.EventTime, &status, &d.Attempts, &d.NextAttemptAt, &d.LastError, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return corenotification.Delivery{}, err
	}
	d.Trigger = corenotification.Trigger(trigger)
	d.Status = corenotification.Status(status)
	return d, nil
}

// Enqueue inserts one pending delivery per channel for event. Idempotent on
// (event_id, channel_id): offering the same event to the same channel twice
// leaves the first delivery untouched.
func (r *Repository) Enqueue(ctx context.Context, event corenotification.Event, channels []corenotification.Channel) error {
	const op = "notification.Repository.Enqueue"
	if len(channels) == 0 {
		return nil
	}

	const q = `
		INSERT INTO notification_deliveries
			(id, event_id, channel_id, trigger, node_id, severity, title, message, event_time, status, next_attempt_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (event_id, channel_id) DO NOTHING`

	now := time.Now()
	batch := &pgx.Batch{}
	for _, c := range channels {
		batch.Queue(q, id.New(), event.ID, c.ID, string(event.Trigger), event.NodeID, event.Severity,
			event.Title, event.Message, event.Time, string(corenotification.StatusPending), now)
	}

	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()
	for range channels {
		if _, err := br.Exec(); err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not enqueue a notification delivery").WithOp(op)
		}
	}
	return nil
}

// DueDeliveries returns pending deliveries at or past their next attempt
// time, oldest first.
func (r *Repository) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]corenotification.Delivery, error) {
	const op = "notification.Repository.DueDeliveries"

	const q = `
		SELECT ` + deliveryColumns + ` FROM notification_deliveries
		WHERE status = 'pending' AND next_attempt_at <= $1
		ORDER BY next_attempt_at
		LIMIT $2`

	rows, err := r.pool.Query(ctx, q, now, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list due notification deliveries").WithOp(op)
	}
	defer rows.Close()

	deliveries := []corenotification.Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a notification delivery").WithOp(op)
		}
		deliveries = append(deliveries, d)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list due notification deliveries").WithOp(op)
	}
	return deliveries, nil
}

// MarkDelivered records a successful delivery.
func (r *Repository) MarkDelivered(ctx context.Context, deliveryID string, at time.Time) error {
	const op = "notification.Repository.MarkDelivered"

	const q = `UPDATE notification_deliveries SET status = $2, updated_at = $3 WHERE id = $1`
	if _, err := r.pool.Exec(ctx, q, deliveryID, string(corenotification.StatusDelivered), at); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not mark the notification delivered").WithOp(op)
	}
	return nil
}

// MarkRetry records a failed attempt that has not yet exhausted its
// retries.
func (r *Repository) MarkRetry(ctx context.Context, deliveryID string, attempts int, next time.Time, lastErr string) error {
	const op = "notification.Repository.MarkRetry"

	const q = `
		UPDATE notification_deliveries
		SET attempts = $2, next_attempt_at = $3, last_error = $4, updated_at = now()
		WHERE id = $1`
	if _, err := r.pool.Exec(ctx, q, deliveryID, attempts, next, lastErr); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the notification retry").WithOp(op)
	}
	return nil
}

// MarkExhausted records a failed attempt that has used its last retry.
func (r *Repository) MarkExhausted(ctx context.Context, deliveryID string, attempts int, lastErr string) error {
	const op = "notification.Repository.MarkExhausted"

	const q = `
		UPDATE notification_deliveries
		SET status = $2, attempts = $3, last_error = $4, updated_at = now()
		WHERE id = $1`
	if _, err := r.pool.Exec(ctx, q, deliveryID, string(corenotification.StatusFailed), attempts, lastErr); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the notification failure").WithOp(op)
	}
	return nil
}

// ListDeliveries returns recent deliveries matching filter, newest first.
func (r *Repository) ListDeliveries(ctx context.Context, filter corenotification.DeliveryFilter) ([]corenotification.Delivery, error) {
	const op = "notification.Repository.ListDeliveries"

	limit := filter.Limit
	if limit <= 0 || limit > maxDeliveryListLimit {
		limit = defaultDeliveryListLimit
	}

	q := `SELECT ` + deliveryColumns + ` FROM notification_deliveries WHERE true`
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.ChannelID != "" {
		q += " AND channel_id = " + arg(filter.ChannelID)
	}
	if filter.Status != "" {
		q += " AND status = " + arg(string(filter.Status))
	}
	q += " ORDER BY created_at DESC LIMIT " + arg(limit)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification deliveries").WithOp(op)
	}
	defer rows.Close()

	deliveries := []corenotification.Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a notification delivery").WithOp(op)
		}
		deliveries = append(deliveries, d)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification deliveries").WithOp(op)
	}
	return deliveries, nil
}

var (
	_ corenotification.ChannelStore  = (*Repository)(nil)
	_ corenotification.DeliveryStore = (*Repository)(nil)
)
