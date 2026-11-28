package notification

import (
	"context"
	"errors"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
)

var _ corenotification.MonitorStateStore = (*Repository)(nil)

// GetMonitorState returns the evaluator state for one (trigger, monitor,
// subject); found is false when none has been recorded.
func (r *Repository) GetMonitorState(ctx context.Context, trigger corenotification.Trigger, monitorID, subject string) (corenotification.State, bool, error) {
	const op = "notification.Repository.GetMonitorState"
	s, err := scanState(r.pool.QueryRow(ctx, `SELECT `+stateColumns+` FROM notification_state
		WHERE trigger = $1 AND monitor_id = $2 AND subject = $3`, string(trigger), monitorID, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return corenotification.State{}, false, nil
	}
	if err != nil {
		return corenotification.State{}, false, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification state").WithOp(op)
	}
	return s, true, nil
}

// ListActiveMonitorStates returns every monitor state that is not ok.
func (r *Repository) ListActiveMonitorStates(ctx context.Context) ([]corenotification.State, error) {
	const op = "notification.Repository.ListActiveMonitorStates"
	rows, err := r.pool.Query(ctx, `SELECT `+stateColumns+` FROM notification_state
		WHERE state <> 'ok' AND monitor_id IS NOT NULL ORDER BY state_since, trigger, monitor_id, subject`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list active notification states").WithOp(op)
	}
	defer rows.Close()
	states := []corenotification.State{}
	for rows.Next() {
		s, err := scanState(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a notification state").WithOp(op)
		}
		states = append(states, s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list active notification states").WithOp(op)
	}
	return states, nil
}

// resetMonitorStates is resetNodeStates for a monitor: every non-ok state
// back to ok, silently. last_notified_* are kept, so cooldown still spans
// the reset.
func resetMonitorStates(ctx context.Context, exec execer, monitorID string, trigger corenotification.Trigger) error {
	q := `
		UPDATE notification_state SET
			state = 'ok', severity = '', value = NULL, condition_since = NULL, state_since = now(),
			flap_count = 0, flap_window_start = NULL, baseline = '{}', updated_at = now()
		WHERE monitor_id = $1 AND state <> 'ok'`
	args := []any{monitorID}
	if trigger != "" {
		q += ` AND trigger = $2`
		args = append(args, string(trigger))
	}
	_, err := exec.Exec(ctx, q, args...)
	return err
}

// ResetMonitorStates implements [corenotification.MonitorStateStore].
func (r *Repository) ResetMonitorStates(ctx context.Context, monitorID string) error {
	const op = "notification.Repository.ResetMonitorStates"
	if err := resetMonitorStates(ctx, r.pool, monitorID, ""); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not reset the website's notification state").WithOp(op)
	}
	return nil
}

// endMonitorMute clears monitors.alerts_muted_until and, only if that matched
// a row, resets the monitor's states in the same transaction.
func endMonitorMute(ctx context.Context, exec execer, monitorID, guard string, guardArgs ...any) (bool, error) {
	q := `UPDATE monitors SET alerts_muted_until = NULL WHERE id = $1`
	if guard != "" {
		q += " AND (" + guard + ")"
	}
	tag, err := exec.Exec(ctx, q, append([]any{monitorID}, guardArgs...)...)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	return true, resetMonitorStates(ctx, exec, monitorID, "")
}

// EndMonitorMuteAfterExpiry is [Repository.EndMuteAfterExpiry] for a
// monitor: guarded against a concurrent re-mute.
func (r *Repository) EndMonitorMuteAfterExpiry(ctx context.Context, monitorID string, now time.Time) (bool, error) {
	const op = "notification.Repository.EndMonitorMuteAfterExpiry"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not begin ending the website's mute").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	did, err := endMonitorMute(ctx, tx, monitorID, "alerts_muted_until IS NULL OR alerts_muted_until <= $2", now)
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the website's mute").WithOp(op)
	}
	if !did {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the website's mute").WithOp(op)
	}
	return true, nil
}

// EndMonitorMuteExplicitly is [Repository.EndMuteExplicitly] for a monitor:
// the early unmute. A monitor that is not muted is left alone. ok is false
// only for an unknown monitor.
func (r *Repository) EndMonitorMuteExplicitly(ctx context.Context, monitorID string) (bool, error) {
	const op = "notification.Repository.EndMonitorMuteExplicitly"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not begin ending the website's mute").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var muted bool
	err = tx.QueryRow(ctx, `SELECT alerts_muted_until IS NOT NULL FROM monitors WHERE id = $1 FOR NO KEY UPDATE`, monitorID).Scan(&muted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the website's mute").WithOp(op)
	}
	if !muted {
		return true, nil
	}
	if _, err := endMonitorMute(ctx, tx, monitorID, ""); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the website's mute").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the website's mute").WithOp(op)
	}
	return true, nil
}

// MuteMonitor sets monitorID's mute window, replacing any existing one, and
// returns it.
func (r *Repository) MuteMonitor(ctx context.Context, monitorID string, until time.Time) (*time.Time, error) {
	const op = "notification.Repository.MuteMonitor"
	var out *time.Time
	err := r.pool.QueryRow(ctx, `UPDATE monitors SET alerts_muted_until = $2 WHERE id = $1 RETURNING alerts_muted_until`,
		monitorID, until).Scan(&out)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errs.New(errs.CodeNotFound, "monitor not found").WithOp(op).WithDetail("monitor_id", monitorID)
	}
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not mute the website").WithOp(op)
	}
	return out, nil
}

// ListMonitorRules resolves every monitor catalog rule for monitorID.
func (r *Repository) ListMonitorRules(ctx context.Context, monitorID string) ([]corenotification.SubjectRule, error) {
	const op = "notification.Repository.ListMonitorRules"
	if err := monitorKnown(ctx, r.pool, monitorID, op); err != nil {
		return nil, err
	}
	specs, err := rulesWhere(ctx, r.pool, globalRule+` OR monitor_id = $1`, monitorID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification rules").WithOp(op)
	}
	return corenotification.MonitorRules(specs, monitorID), nil
}

// SetMonitorRuleEnabled stores monitorID's enabled override for trigger,
// keeping any other field the override already carries. When the rule ends
// up disabled, the monitor's state for it is reset in the same transaction.
func (r *Repository) SetMonitorRuleEnabled(
	ctx context.Context, monitorID string, trigger corenotification.Trigger, enabled bool, updatedBy string,
) (corenotification.SubjectRule, error) {
	const op = "notification.Repository.SetMonitorRuleEnabled"
	return r.changeMonitorRule(ctx, op, monitorID, trigger, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO notification_rules (id, trigger, monitor_id, enabled, updated_at, updated_by)
			VALUES ($1, $2, $3, $4, now(), $5)
			`+ruleScopeConflict+` DO UPDATE SET
				enabled = EXCLUDED.enabled, updated_at = now(), updated_by = EXCLUDED.updated_by`,
			id.New(), string(trigger), monitorID, enabled, updatedBy)
		return err
	})
}

// ClearMonitorRuleEnabled removes monitorID's enabled override for trigger;
// the row goes once nothing else is overridden. Clearing one that does not
// exist is not an error.
func (r *Repository) ClearMonitorRuleEnabled(
	ctx context.Context, monitorID string, trigger corenotification.Trigger, updatedBy string,
) (corenotification.SubjectRule, error) {
	const op = "notification.Repository.ClearMonitorRuleEnabled"
	return r.changeMonitorRule(ctx, op, monitorID, trigger, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE notification_rules SET enabled = NULL, updated_at = now(), updated_by = $3
			WHERE trigger = $1 AND monitor_id = $2 AND enabled IS NOT NULL`,
			string(trigger), monitorID, updatedBy); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM notification_rules
			WHERE trigger = $1 AND monitor_id = $2
			  AND enabled IS NULL AND warn_threshold IS NULL AND crit_threshold IS NULL
			  AND for_seconds IS NULL AND recovery_seconds IS NULL AND hysteresis IS NULL
			  AND cooldown_seconds IS NULL AND send_recovery IS NULL AND subject_excludes IS NULL`,
			string(trigger), monitorID)
		return err
	})
}

// changeMonitorRule is changeNodeRule for a monitor: the write runs under
// the trigger's exclusive lock, and a rule left disabled has the monitor's
// state for it reset before commit, so no notification for it can still be
// enqueued once the change is visible.
func (r *Repository) changeMonitorRule(
	ctx context.Context, op, monitorID string, trigger corenotification.Trigger, write func(pgx.Tx) error,
) (corenotification.SubjectRule, error) {
	if _, err := corenotification.ParseRuleKeyFor(corenotification.SubjectMonitor, string(trigger)); err != nil {
		return corenotification.SubjectRule{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return corenotification.SubjectRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin the rule change").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err = tx.Exec(ctx, lockRuleExclusive, string(trigger)); err != nil {
		return corenotification.SubjectRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not lock the notification rule").WithOp(op)
	}
	if err = monitorKnown(ctx, tx, monitorID, op); err != nil {
		return corenotification.SubjectRule{}, err
	}
	if err = write(tx); err != nil {
		if pgCode(err) == "23503" {
			return corenotification.SubjectRule{}, errs.New(errs.CodeNotFound, "monitor not found").WithOp(op).WithDetail("monitor_id", monitorID)
		}
		return corenotification.SubjectRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not save the website's alert rule").WithOp(op)
	}
	rule, err := monitorRule(ctx, tx, monitorID, trigger)
	if err != nil {
		return corenotification.SubjectRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the website's alert rule").WithOp(op)
	}
	if !rule.Enabled {
		if err := resetMonitorStates(ctx, tx, monitorID, trigger); err != nil {
			return corenotification.SubjectRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not reset the website's alert state").WithOp(op)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return corenotification.SubjectRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not save the website's alert rule").WithOp(op)
	}
	return rule, nil
}

func monitorRule(ctx context.Context, q querier, monitorID string, trigger corenotification.Trigger) (corenotification.SubjectRule, error) {
	specs, err := rulesWhere(ctx, q, `trigger = $1 AND (`+globalRule+` OR monitor_id = $2)`, string(trigger), monitorID)
	if err != nil {
		return corenotification.SubjectRule{}, err
	}
	for _, rule := range corenotification.MonitorRules(specs, monitorID) {
		if rule.Key == trigger {
			return rule, nil
		}
	}
	return corenotification.SubjectRule{}, errs.New(errs.CodeNotFound, "unknown alert rule %q", trigger)
}

func monitorKnown(ctx context.Context, q rowQuerier, monitorID, op string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM monitors WHERE id = $1)`, monitorID).Scan(&exists); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not read the monitor").WithOp(op)
	}
	if !exists {
		return errs.New(errs.CodeNotFound, "monitor not found").WithOp(op).WithDetail("monitor_id", monitorID)
	}
	return nil
}
