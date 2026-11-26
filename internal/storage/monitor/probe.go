package monitor

import (
	"context"
	"time"

	coremonitor "github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
)

var _ coremonitor.Store = (*Repository)(nil)

// SpreadOverdue implements [coremonitor.Store].
func (r *Repository) SpreadOverdue(ctx context.Context, maxSpread time.Duration) (int, error) {
	const op = "monitor.Repository.SpreadOverdue"
	tag, err := r.pool.Exec(ctx, `
		UPDATE monitor_status s
		SET next_check_at = now() + make_interval(secs => random() * LEAST(m.interval_seconds, $1::float8))
		FROM monitors m
		WHERE m.id = s.monitor_id AND m.active AND s.next_check_at < now()`, maxSpread.Seconds())
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeUnavailable, "could not reschedule overdue checks").WithOp(op)
	}
	return int(tag.RowsAffected()), nil
}

// ClaimDue implements [coremonitor.Store]. SKIP LOCKED plus the lease make
// the claim safe with several servers on one database.
func (r *Repository) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]coremonitor.Due, error) {
	const op = "monitor.Repository.ClaimDue"
	rows, err := r.pool.Query(ctx, `
		WITH due AS (
			SELECT s.monitor_id FROM monitor_status s JOIN monitors m ON m.id = s.monitor_id
			WHERE m.active AND s.next_check_at <= now()
			ORDER BY s.next_check_at
			LIMIT $1
			FOR UPDATE OF s SKIP LOCKED
		)
		UPDATE monitor_status s SET next_check_at = now() + make_interval(secs => $2::float8)
		FROM due WHERE s.monitor_id = due.monitor_id
		RETURNING s.monitor_id, s.status, s.consecutive_failures, s.consecutive_successes, s.status_since, s.down_since`,
		limit, lease.Seconds())
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not claim due checks").WithOp(op)
	}
	counters := map[string]coremonitor.Counters{}
	var ids []string
	for rows.Next() {
		var monitorID, status string
		var c coremonitor.Counters
		if err := rows.Scan(&monitorID, &status, &c.ConsecutiveFailures, &c.ConsecutiveSuccesses, &c.StatusSince, &c.DownSince); err != nil {
			rows.Close()
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a claimed check").WithOp(op)
		}
		c.Status = coremonitor.Status(status)
		counters[monitorID] = c
		ids = append(ids, monitorID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not claim due checks").WithOp(op)
	}
	if len(ids) == 0 {
		return nil, nil
	}

	rows, err = r.pool.Query(ctx, `SELECT `+monitorColumns+`, m.secret_ciphertext, m.secret_key_id
		FROM monitors m WHERE m.id = ANY($1)`, ids)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the claimed monitors").WithOp(op)
	}
	defer rows.Close()
	out := make([]coremonitor.Due, 0, len(ids))
	for rows.Next() {
		var d coremonitor.Due
		var method, bodyEncoding, authType string
		var ciphertext []byte
		var keyID *string
		if err := rows.Scan(append(monitorDest(&d.Monitor, &method, &bodyEncoding, &authType), &ciphertext, &keyID)...); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a claimed monitor").WithOp(op)
		}
		finishMonitor(&d.Monitor, method, bodyEncoding, authType)
		var ok bool
		d.Secret, ok = r.open(d.Monitor.ID, ciphertext, keyID)
		d.SecretUnreadable = !ok
		d.Counters = counters[d.Monitor.ID]
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordCheck implements [coremonitor.Store]: the live state, the heartbeat
// and any event commit together, and only while the monitor still exists
// and is active.
func (r *Repository) RecordCheck(ctx context.Context, rec coremonitor.CheckRecord) error {
	const op = "monitor.Repository.RecordCheck"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin recording the check").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	res := rec.Result
	var code, ms *int
	if res.StatusCode != 0 {
		code = &res.StatusCode
	}
	if res.StatusCode != 0 || res.Up {
		ms = &res.ResponseMS
	}
	c := rec.Counters
	tag, err := tx.Exec(ctx, `
		UPDATE monitor_status s SET status = $2, consecutive_failures = $3, consecutive_successes = $4,
			status_since = $5, down_since = $6, next_check_at = $7, last_check_at = $8, last_status_code = $9,
			last_response_ms = $10, last_failure = $11, cert_expires_at = COALESCE($12, s.cert_expires_at),
			updated_at = now()
		FROM monitors m WHERE s.monitor_id = $1 AND m.id = s.monitor_id AND m.active`,
		rec.MonitorID, string(c.Status), c.ConsecutiveFailures, c.ConsecutiveSuccesses, c.StatusSince, c.DownSince,
		rec.NextCheckAt, rec.At, code, ms, string(res.Failure), rec.CertExpiresAt)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the check").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return nil // deleted or paused while the check ran
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO monitor_heartbeats (monitor_id, time, status, status_code, response_ms, failure)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		rec.MonitorID, rec.At, string(rec.Heartbeat), code, ms, string(res.Failure)); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the heartbeat").WithOp(op)
	}
	if e := rec.Event; e != nil {
		if _, err := tx.Exec(ctx, `
			INSERT INTO monitor_events (id, monitor_id, time, kind, status_code, failure, duration_seconds)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id.New(), rec.MonitorID, e.Time, string(e.Kind), e.StatusCode, string(e.Failure), e.DurationSeconds); err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not record the monitor event").WithOp(op)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the check").WithOp(op)
	}
	return nil
}

// Reschedule implements [coremonitor.Store].
func (r *Repository) Reschedule(ctx context.Context, monitorID string, at time.Time) error {
	const op = "monitor.Repository.Reschedule"
	if _, err := r.pool.Exec(ctx, `UPDATE monitor_status SET next_check_at = $2, updated_at = now() WHERE monitor_id = $1`,
		monitorID, at); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not reschedule the check").WithOp(op)
	}
	return nil
}

// PruneEvents implements [coremonitor.Store].
func (r *Repository) PruneEvents(ctx context.Context, before time.Time) (int64, error) {
	const op = "monitor.Repository.PruneEvents"
	tag, err := r.pool.Exec(ctx, `DELETE FROM monitor_events WHERE time < $1`, before)
	if err != nil {
		return 0, errs.Wrap(err, errs.CodeUnavailable, "could not prune monitor events").WithOp(op)
	}
	return tag.RowsAffected(), nil
}
