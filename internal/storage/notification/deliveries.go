package notification

import (
	"context"
	"errors"
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

	// DeliveredRetention is how long a successfully delivered row is kept.
	DeliveredRetention = 30 * 24 * time.Hour
	// FailedRetention is how long a failed/exhausted row is kept — longer
	// than a delivered one, since a failure is exactly what an operator
	// still wants to see days later, well after a success has stopped
	// mattering.
	FailedRetention = 90 * 24 * time.Hour
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

// deliveryColumnsQualified is deliveryColumns with every column prefixed
// "d.", for ClaimDue's query, which joins notification_deliveries (there,
// aliased d) against a CTE that also has an "id" column — an unqualified
// "id" in that query's RETURNING clause would be ambiguous.
const deliveryColumnsQualified = `d.id, d.event_id, d.channel_id, d.channel_type, d.kind, d.trigger, d.node_id,
	d.subject, d.severity, d.title, d.message, d.status, d.attempts, d.next_attempt_at, d.last_error,
	d.delivered_at, d.created_at, d.updated_at`

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

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeUnavailable, "could not begin the enqueue").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// An alert for a rule disabled on its node is never enqueued, even when
	// the evaluator decided to send it before the rule was disabled: the
	// shared lock orders this against the rule change (see lockRuleShared).
	if n.Kind != corenotification.KindTest && n.Trigger != "" && n.NodeID != "" {
		if _, err := tx.Exec(ctx, lockRuleShared, string(n.Trigger)); err != nil {
			return 0, errs.Wrap(err, errs.CodeUnavailable, "could not lock the notification rule").WithOp(op)
		}
		enabled, err := ruleEnabled(ctx, tx, n.NodeID, n.Trigger)
		if err != nil {
			return 0, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification rule").WithOp(op)
		}
		if !enabled {
			return 0, nil
		}
	}

	batch := &pgx.Batch{}
	for _, c := range channels {
		batch.Queue(q, id.New(), n.EventID, c.ID, string(c.Type), string(n.Kind), string(n.Trigger), n.NodeID,
			n.Subject, string(n.Severity), n.Title, n.Message)
	}
	br := tx.SendBatch(ctx, batch)
	inserted := 0
	for range channels {
		tag, err := br.Exec()
		if err != nil {
			_ = br.Close()
			return 0, errs.Wrap(err, errs.CodeUnavailable, "could not enqueue a notification delivery").WithOp(op)
		}
		inserted += int(tag.RowsAffected())
	}
	if err := br.Close(); err != nil {
		return 0, errs.Wrap(err, errs.CodeUnavailable, "could not enqueue a notification delivery").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, errs.Wrap(err, errs.CodeUnavailable, "could not enqueue a notification delivery").WithOp(op)
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

// ClaimDue atomically claims up to limit pending deliveries at or past
// now, oldest first, in one statement: FOR UPDATE SKIP LOCKED picks rows
// no other claimer currently holds a lock on, and the UPDATE in the same
// statement sets their next_attempt_at to lease before any other claimer
// gets a chance to run the same query — so two callers racing this
// query, in this process or a concurrent one against the same database,
// can never both return the same row. SKIP LOCKED handles two claimers
// running at the same instant; the lease itself (an ordinary future
// next_attempt_at, not a separate status) is what stops a long-dead
// claimer's rows from being skipped forever — once lease passes with no
// MarkDelivered/MarkRetry/MarkExhausted call in between, the row is
// pending and due again like any other, to be claimed and sent again.
func (r *Repository) ClaimDue(ctx context.Context, now, lease time.Time, limit int) ([]corenotification.Delivery, error) {
	const op = "notification.Repository.ClaimDue"

	const q = `
		WITH claimed AS (
			SELECT id FROM notification_deliveries
			WHERE status = 'pending' AND next_attempt_at <= $1
			ORDER BY next_attempt_at
			FOR UPDATE SKIP LOCKED
			LIMIT $3
		)
		UPDATE notification_deliveries d
		SET next_attempt_at = $2, updated_at = now()
		FROM claimed c
		WHERE d.id = c.id
		RETURNING ` + deliveryColumnsQualified

	rows, err := r.pool.Query(ctx, q, now, lease, limit)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not claim due notification deliveries").WithOp(op)
	}
	defer rows.Close()

	claimed := []corenotification.Delivery{}
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a claimed notification delivery").WithOp(op)
		}
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not claim due notification deliveries").WithOp(op)
	}
	return claimed, nil
}

// ClaimByEvent atomically claims the one pending delivery for
// (eventID, channelID) — unique by the table's own constraint — the same
// way ClaimDue does, for [corenotification.Engine.SendTest]'s synchronous
// single-row send. A plain single-table UPDATE already serialises against
// a concurrent claimer of the very same row; no CTE or FOR UPDATE is
// needed when there is only ever one row this call could possibly match.
func (r *Repository) ClaimByEvent(ctx context.Context, eventID, channelID string, lease time.Time) (corenotification.Delivery, bool, error) {
	const op = "notification.Repository.ClaimByEvent"

	const q = `
		UPDATE notification_deliveries
		SET next_attempt_at = $3, updated_at = now()
		WHERE event_id = $1 AND channel_id = $2 AND status = 'pending'
		RETURNING ` + deliveryColumns

	d, err := scanDelivery(r.pool.QueryRow(ctx, q, eventID, channelID, lease))
	if errors.Is(err, pgx.ErrNoRows) {
		return corenotification.Delivery{}, false, nil
	}
	if err != nil {
		return corenotification.Delivery{}, false, errs.Wrap(err, errs.CodeUnavailable, "could not claim the test notification delivery").WithOp(op)
	}
	return d, true, nil
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

// SweepDeliveries applies delivery retention as of now: delivered rows
// older than [DeliveredRetention] and failed rows older than
// [FailedRetention] are deleted, and deliveries still pending after
// [PendingExpiry] are marked failed with last_error "expired". Failed rows
// outlive delivered ones — a success is uninteresting a month on, a
// failure is exactly what an operator still wants to see.
func (r *Repository) SweepDeliveries(ctx context.Context, now time.Time) (corenotification.SweepResult, error) {
	const op = "notification.Repository.SweepDeliveries"

	var res corenotification.SweepResult
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM notification_deliveries
		WHERE (status = 'delivered' AND created_at < $1)
		   OR (status = 'failed' AND created_at < $2)`,
		now.Add(-DeliveredRetention), now.Add(-FailedRetention))
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
