package notification

import (
	"context"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/hexane/atlas/internal/platform/redact"
	"github.com/jackc/pgx/v5"
)

const (
	defaultDeliveryListLimit = 100
	maxDeliveryListLimit     = 500

	// MaxLastErrorLen is the notification_deliveries.last_error CHECK length.
	MaxLastErrorLen = 512

	// DeliveryRetention is how long a finished delivery is kept.
	DeliveryRetention = 30 * 24 * time.Hour
	// PendingExpiry is how long a delivery may stay pending before it is
	// marked failed: a day-old notification is no longer worth sending.
	PendingExpiry = 24 * time.Hour
)

// urlQueryPattern matches the query string of an http(s) URL. Webhook URLs
// (Teams' sig=, and the like) carry their credential in the query, and Go's
// *url.Error embeds the full URL in its message.
var urlQueryPattern = regexp.MustCompile(`(?i)(https?://[^\s?#"'<>]+)\?[^\s#"'<>]*`)

// sanitizeError makes a send error safe to store: URL query strings and
// credential-shaped values are redacted, then the result is truncated to
// fit the column.
func sanitizeError(msg string) string {
	msg = urlQueryPattern.ReplaceAllString(msg, "${1}?"+redact.Placeholder)
	msg = redact.String(msg)
	if utf8.RuneCountInString(msg) <= MaxLastErrorLen {
		return msg
	}
	const ellipsis = "…"
	runes := []rune(msg)
	return string(runes[:MaxLastErrorLen-utf8.RuneCountInString(ellipsis)]) + ellipsis
}

const deliveryColumns = `id, event_id, channel_id, channel_type, kind, trigger, node_id, subject, severity,
	title, message, status, attempts, next_attempt_at, last_error, delivered_at, created_at, updated_at`

func scanDelivery(row pgx.Row) (corenotification.Delivery, error) {
	var d corenotification.Delivery
	var channelType, kind, trigger, severity, status string
	if err := row.Scan(&d.ID, &d.EventID, &d.ChannelID, &channelType, &kind, &trigger, &d.NodeID, &d.Subject,
		&severity, &d.Title, &d.Message, &status, &d.Attempts, &d.NextAttemptAt, &d.LastError, &d.DeliveredAt,
		&d.CreatedAt, &d.UpdatedAt); err != nil {
		return corenotification.Delivery{}, err
	}
	d.ChannelType = corenotification.ChannelType(channelType)
	d.Kind = corenotification.Kind(kind)
	d.Trigger = corenotification.Trigger(trigger)
	d.Severity = corenotification.Severity(severity)
	d.Status = corenotification.Status(status)
	return d, nil
}

// Enqueue inserts one pending delivery of n per channel, due now, and
// returns how many were new. Idempotent on (event_id, channel_id): offering
// the same event to a channel again leaves the first delivery untouched.
func (r *Repository) Enqueue(ctx context.Context, n corenotification.Notification, channels []corenotification.Channel) (int, error) {
	const op = "notification.Repository.Enqueue"
	if len(channels) == 0 {
		return 0, nil
	}
	if n.EventID == "" {
		return 0, errs.New(errs.CodeInvalidArgument, "a notification needs an event id").WithOp(op)
	}

	const q = `
		INSERT INTO notification_deliveries (id, event_id, channel_id, channel_type, kind, trigger, node_id,
			subject, severity, title, message, status, next_attempt_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'pending', now())
		ON CONFLICT (event_id, channel_id) DO NOTHING`

	batch := &pgx.Batch{}
	for _, c := range channels {
		batch.Queue(q, id.New(), n.EventID, c.ID, string(c.Type), string(n.Kind), string(n.Trigger), n.NodeID,
			n.Subject, string(n.Severity), n.Title, n.Message)
	}
	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()

	inserted := 0
	for range channels {
		tag, err := br.Exec()
		if err != nil {
			return inserted, errs.Wrap(err, errs.CodeUnavailable, "could not enqueue a notification delivery").WithOp(op)
		}
		inserted += int(tag.RowsAffected())
	}
	return inserted, nil
}

// ListDeliveries returns a page of the delivery log, newest first, keyset
// paginated on (created_at, id).
func (r *Repository) ListDeliveries(ctx context.Context, filter corenotification.DeliveryFilter) (corenotification.DeliveryPage, error) {
	const op = "notification.Repository.ListDeliveries"

	limit := filter.Limit
	if limit <= 0 {
		limit = defaultDeliveryListLimit
	}
	if limit > maxDeliveryListLimit {
		limit = maxDeliveryListLimit
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
	if filter.NodeID != "" {
		q += " AND node_id = " + arg(filter.NodeID)
	}
	if filter.Trigger != "" {
		q += " AND trigger = " + arg(string(filter.Trigger))
	}
	if filter.Before != nil {
		q += fmt.Sprintf(" AND (created_at, id) < (%s, %s)", arg(filter.Before.CreatedAt), arg(filter.Before.ID))
	}
	// One extra row says whether another page exists.
	q += " ORDER BY created_at DESC, id DESC LIMIT " + arg(limit+1)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return corenotification.DeliveryPage{}, errs.Wrap(err, errs.CodeUnavailable, "could not list notification deliveries").WithOp(op)
	}
	defer rows.Close()

	page := corenotification.DeliveryPage{Deliveries: []corenotification.Delivery{}}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return corenotification.DeliveryPage{}, errs.Wrap(err, errs.CodeInternal, "could not read a notification delivery").WithOp(op)
		}
		page.Deliveries = append(page.Deliveries, d)
	}
	if err := rows.Err(); err != nil {
		return corenotification.DeliveryPage{}, errs.Wrap(err, errs.CodeUnavailable, "could not list notification deliveries").WithOp(op)
	}
	if len(page.Deliveries) > limit {
		page.Deliveries = page.Deliveries[:limit]
		last := page.Deliveries[limit-1]
		page.Next = &corenotification.DeliveryCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func (r *Repository) markDelivery(ctx context.Context, op, q string, args ...any) error {
	tag, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not update the notification delivery").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "pending notification delivery not found").WithOp(op)
	}
	return nil
}

// MarkDelivered records a successful send of a pending delivery.
func (r *Repository) MarkDelivered(ctx context.Context, deliveryID string, at time.Time) error {
	return r.markDelivery(ctx, "notification.Repository.MarkDelivered", `
		UPDATE notification_deliveries
		SET status = 'delivered', delivered_at = $2, last_error = '', updated_at = now()
		WHERE id = $1 AND status = 'pending'`, deliveryID, at)
}

// MarkRetry records a failed attempt that will be retried at next. lastErr
// is redacted and truncated before it is stored.
func (r *Repository) MarkRetry(ctx context.Context, deliveryID string, attempts int, next time.Time, lastErr string) error {
	return r.markDelivery(ctx, "notification.Repository.MarkRetry", `
		UPDATE notification_deliveries
		SET attempts = $2, next_attempt_at = $3, last_error = $4, updated_at = now()
		WHERE id = $1 AND status = 'pending'`, deliveryID, attempts, next, sanitizeError(lastErr))
}

// MarkExhausted records a failed attempt that used the last retry. lastErr
// is redacted and truncated before it is stored.
func (r *Repository) MarkExhausted(ctx context.Context, deliveryID string, attempts int, lastErr string) error {
	return r.markDelivery(ctx, "notification.Repository.MarkExhausted", `
		UPDATE notification_deliveries
		SET status = 'failed', attempts = $2, last_error = $3, updated_at = now()
		WHERE id = $1 AND status = 'pending'`, deliveryID, attempts, sanitizeError(lastErr))
}

// SweepResult reports what [Repository.SweepDeliveries] changed.
type SweepResult struct {
	Deleted int64
	Expired int64
}

// SweepDeliveries applies delivery retention as of now: finished deliveries
// older than [DeliveryRetention] are deleted, and deliveries still pending
// after [PendingExpiry] are marked failed with last_error "expired".
func (r *Repository) SweepDeliveries(ctx context.Context, now time.Time) (SweepResult, error) {
	const op = "notification.Repository.SweepDeliveries"

	var res SweepResult
	tag, err := r.pool.Exec(ctx, `DELETE FROM notification_deliveries WHERE status <> 'pending' AND created_at < $1`,
		now.Add(-DeliveryRetention))
	if err != nil {
		return res, errs.Wrap(err, errs.CodeUnavailable, "could not delete old notification deliveries").WithOp(op)
	}
	res.Deleted = tag.RowsAffected()

	tag, err = r.pool.Exec(ctx, `
		UPDATE notification_deliveries SET status = 'failed', last_error = 'expired', updated_at = now()
		WHERE status = 'pending' AND created_at < $1`, now.Add(-PendingExpiry))
	if err != nil {
		return res, errs.Wrap(err, errs.CodeUnavailable, "could not expire stale notification deliveries").WithOp(op)
	}
	res.Expired = tag.RowsAffected()
	return res, nil
}
