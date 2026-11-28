package notification

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const ruleColumns = `trigger, COALESCE(node_id, ''), COALESCE(monitor_id, ''), enabled, warn_threshold, crit_threshold,
	for_seconds, recovery_seconds, hysteresis, cooldown_seconds, send_recovery, subject_excludes, updated_at, updated_by`

// globalRule matches a trigger's global rule row; overrideRule any
// override, of a node or a monitor.
const (
	globalRule   = `(node_id IS NULL AND monitor_id IS NULL)`
	overrideRule = `(node_id IS NOT NULL OR monitor_id IS NOT NULL)`
)

// ruleScopeConflict is the ON CONFLICT target of every rule upsert: the
// ux_notification_rules_subject_scope index.
const ruleScopeConflict = `ON CONFLICT (trigger, (COALESCE(node_id, '')), (COALESCE(monitor_id, '')))`

func scanRule(row pgx.Row) (corenotification.RuleSpec, error) {
	var s corenotification.RuleSpec
	var trigger string
	if err := row.Scan(&trigger, &s.NodeID, &s.MonitorID, &s.Enabled, &s.WarnThreshold, &s.CritThreshold, &s.ForSeconds,
		&s.RecoverySeconds, &s.Hysteresis, &s.CooldownSeconds, &s.SendRecovery, &s.SubjectExcludes,
		&s.UpdatedAt, &s.UpdatedBy); err != nil {
		return corenotification.RuleSpec{}, err
	}
	s.Trigger = corenotification.Trigger(trigger)
	return s, nil
}

// ListRules returns every stored rule: each trigger's global default (NodeID
// "") and every node override, ordered by trigger with the global first.
func (r *Repository) ListRules(ctx context.Context) ([]corenotification.RuleSpec, error) {
	const op = "notification.Repository.ListRules"

	rows, err := r.pool.Query(ctx, `SELECT `+ruleColumns+` FROM notification_rules
		ORDER BY trigger, `+overrideRule+`, node_id, monitor_id`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification rules").WithOp(op)
	}
	defer rows.Close()

	rules := []corenotification.RuleSpec{}
	for rows.Next() {
		s, err := scanRule(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a notification rule").WithOp(op)
		}
		rules = append(rules, s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list notification rules").WithOp(op)
	}
	return rules, nil
}

// SetRule creates or replaces the global default (spec.NodeID "") or a node
// override for spec.Trigger. A node override is checked against the current
// global so the effective rule it produces is valid.
func (r *Repository) SetRule(ctx context.Context, spec corenotification.RuleSpec, updatedBy string) error {
	const op = "notification.Repository.SetRule"
	if err := spec.Validate(); err != nil {
		return err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin the rule update").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Serialise every SetRule for this trigger before reading anything.
	// Without this, the global branch's FOR UPDATE locks only the
	// override rows that already exist, and the override branch's FOR
	// SHARE locks only the global row — different rows, so a global
	// change and a brand-new override for the same trigger can each read
	// a state the other hasn't written yet, each validate cleanly against
	// what they read, and both commit: a global row and an override row
	// that are individually valid but whose merge (MergeRule) is not,
	// with nothing left to catch it afterwards. One advisory lock per
	// trigger, held for the whole transaction, makes the read-validate-
	// write sequence atomic regardless of which row either call touches.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, string(spec.Trigger)); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not lock the notification rule for update").WithOp(op)
	}

	if spec.IsGlobal() {
		// Every existing override must still merge cleanly over the new global.
		overrides, err := rulesWhere(ctx, tx, `trigger = $1 AND `+overrideRule+` FOR UPDATE`, string(spec.Trigger))
		if err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not read the node overrides").WithOp(op)
		}
		for _, o := range overrides {
			if _, err := corenotification.MergeRule(spec, &o, o.NodeID); err != nil {
				return errs.Wrap(err, errs.CodeInvalidArgument,
					"the new default conflicts with the override for %s", ruleScopeName(o)).WithOp(op)
			}
		}
	} else {
		global, err := rulesWhere(ctx, tx, `trigger = $1 AND `+globalRule+` FOR SHARE`, string(spec.Trigger))
		if err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not read the global rule").WithOp(op)
		}
		if len(global) == 0 {
			return errs.New(errs.CodeFailedPrecondition, "trigger %q has no global rule to override", spec.Trigger).WithOp(op)
		}
		if _, err := corenotification.MergeRule(global[0], &spec, spec.NodeID); err != nil {
			return err
		}
	}

	const q = `
		INSERT INTO notification_rules (id, trigger, node_id, monitor_id, enabled, warn_threshold, crit_threshold, for_seconds,
			recovery_seconds, hysteresis, cooldown_seconds, send_recovery, subject_excludes, updated_at, updated_by)
		VALUES ($13, $1, $2, $14, $3, $4, $5, $6, $7, $8, $9, $10, $11, now(), $12)
		` + ruleScopeConflict + ` DO UPDATE SET
			enabled = EXCLUDED.enabled, warn_threshold = EXCLUDED.warn_threshold,
			crit_threshold = EXCLUDED.crit_threshold, for_seconds = EXCLUDED.for_seconds,
			recovery_seconds = EXCLUDED.recovery_seconds, hysteresis = EXCLUDED.hysteresis,
			cooldown_seconds = EXCLUDED.cooldown_seconds, send_recovery = EXCLUDED.send_recovery,
			subject_excludes = EXCLUDED.subject_excludes, updated_at = now(), updated_by = EXCLUDED.updated_by`
	if _, err := tx.Exec(ctx, q, string(spec.Trigger), nullable(spec.NodeID), spec.Enabled, spec.WarnThreshold, spec.CritThreshold,
		spec.ForSeconds, spec.RecoverySeconds, spec.Hysteresis, spec.CooldownSeconds, spec.SendRecovery,
		spec.SubjectExcludes, updatedBy, id.New(), nullable(spec.MonitorID)); err != nil {
		if pgCode(err) == "23503" {
			return errs.New(errs.CodeNotFound, "%s is not known", ruleScopeName(spec)).WithOp(op)
		}
		return errs.Wrap(err, errs.CodeUnavailable, "could not save the notification rule").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not save the notification rule").WithOp(op)
	}
	return nil
}

// DeleteRule removes the global default (nodeID "") or a node override.
// Deleting a global default that still has overrides is refused: the
// overrides would have nothing to inherit from.
func (r *Repository) DeleteRule(ctx context.Context, trigger corenotification.Trigger, nodeID string) error {
	const op = "notification.Repository.DeleteRule"

	q := `DELETE FROM notification_rules WHERE trigger = $1 AND node_id = $2`
	args := []any{string(trigger), nodeID}
	if nodeID == "" {
		q = `DELETE FROM notification_rules WHERE trigger = $1 AND ` + globalRule + `
			AND NOT EXISTS (SELECT 1 FROM notification_rules o WHERE o.trigger = $1
				AND (o.node_id IS NOT NULL OR o.monitor_id IS NOT NULL))`
		args = args[:1]
	}
	tag, err := r.pool.Exec(ctx, q, args...)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the notification rule").WithOp(op)
	}
	if tag.RowsAffected() > 0 {
		return nil
	}
	if nodeID == "" {
		var exists bool
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM notification_rules WHERE trigger = $1 AND `+globalRule+`)`,
			string(trigger)).Scan(&exists); err == nil && exists {
			return errs.New(errs.CodeFailedPrecondition, "trigger %q still has overrides; delete them first", trigger).WithOp(op)
		}
	}
	return errs.New(errs.CodeNotFound, "notification rule not found").WithOp(op)
}

// EffectiveRule resolves trigger for nodeID: the global default with the
// node's override, if any, merged over it per field. nodeID "" returns the
// global rule itself.
func (r *Repository) EffectiveRule(ctx context.Context, trigger corenotification.Trigger, nodeID string) (corenotification.Rule, error) {
	const op = "notification.Repository.EffectiveRule"

	specs, err := rulesWhere(ctx, r.pool, `trigger = $1 AND (`+globalRule+` OR node_id = $2)`, string(trigger), nodeID)
	if err != nil {
		return corenotification.Rule{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification rules").WithOp(op)
	}
	var global, override *corenotification.RuleSpec
	for i := range specs {
		if specs[i].IsGlobal() {
			global = &specs[i]
		} else {
			override = &specs[i]
		}
	}
	if global == nil {
		return corenotification.Rule{}, errs.New(errs.CodeNotFound, "trigger %q has no global rule", trigger).WithOp(op)
	}
	return corenotification.MergeRule(*global, override, nodeID)
}

// nullable is s, or NULL when it is empty.
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ruleScopeName names a rule override's subject in an error.
func ruleScopeName(s corenotification.RuleSpec) string {
	if s.MonitorID != "" {
		return "monitor " + s.MonitorID
	}
	return "node " + s.NodeID
}

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

func rulesWhere(ctx context.Context, q querier, where string, args ...any) ([]corenotification.RuleSpec, error) {
	rows, err := q.Query(ctx, `SELECT `+ruleColumns+` FROM notification_rules WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []corenotification.RuleSpec
	for rows.Next() {
		s, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const stateColumns = `trigger, COALESCE(node_id, ''), COALESCE(monitor_id, ''), subject, state, severity, value, condition_since, state_since,
	last_notified_at, last_notified_kind, flap_count, flap_window_start, baseline, updated_at`

func scanState(row pgx.Row) (corenotification.State, error) {
	var s corenotification.State
	var trigger, state, severity, kind string
	var baseline []byte
	if err := row.Scan(&trigger, &s.NodeID, &s.MonitorID, &s.Subject, &state, &severity, &s.Value, &s.ConditionSince,
		&s.StateSince, &s.LastNotifiedAt, &kind, &s.FlapCount, &s.FlapWindowStart, &baseline, &s.UpdatedAt); err != nil {
		return corenotification.State{}, err
	}
	s.Trigger = corenotification.Trigger(trigger)
	s.State = corenotification.StateKind(state)
	s.Severity = corenotification.Severity(severity)
	s.LastNotifiedKind = corenotification.Kind(kind)
	if err := json.Unmarshal(baseline, &s.Baseline); err != nil {
		return corenotification.State{}, err
	}
	return s, nil
}

// GetState returns the evaluator state for one (trigger, node, subject);
// found is false when none has been recorded.
func (r *Repository) GetState(ctx context.Context, trigger corenotification.Trigger, nodeID, subject string) (corenotification.State, bool, error) {
	const op = "notification.Repository.GetState"

	s, err := scanState(r.pool.QueryRow(ctx, `SELECT `+stateColumns+` FROM notification_state
		WHERE trigger = $1 AND node_id = $2 AND subject = $3`, string(trigger), nodeID, subject))
	if errors.Is(err, pgx.ErrNoRows) {
		return corenotification.State{}, false, nil
	}
	if err != nil {
		return corenotification.State{}, false, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification state").WithOp(op)
	}
	return s, true, nil
}

// ListNodeStates returns every state row of one node for the given triggers.
func (r *Repository) ListNodeStates(ctx context.Context, nodeID string, triggers ...corenotification.Trigger) ([]corenotification.State, error) {
	const op = "notification.Repository.ListNodeStates"

	names := make([]string, len(triggers))
	for i, t := range triggers {
		names[i] = string(t)
	}
	rows, err := r.pool.Query(ctx, `SELECT `+stateColumns+` FROM notification_state
		WHERE node_id = $1 AND trigger = ANY($2) ORDER BY trigger, subject`, nodeID, names)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list the node's notification states").WithOp(op)
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
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list the node's notification states").WithOp(op)
	}
	return states, nil
}

// DeleteState removes one state row; a missing row is not an error.
func (r *Repository) DeleteState(ctx context.Context, trigger corenotification.Trigger, nodeID, subject string) error {
	const op = "notification.Repository.DeleteState"
	if _, err := r.pool.Exec(ctx, `DELETE FROM notification_state WHERE trigger = $1 AND node_id = $2 AND subject = $3`,
		string(trigger), nodeID, subject); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the notification state").WithOp(op)
	}
	return nil
}

// BeginPending records s unless the stored row has already left ok, then
// returns the row as it stands, so concurrent evaluators starting the same
// transition all continue from the first one's condition_since.
func (r *Repository) BeginPending(ctx context.Context, s corenotification.State) (corenotification.State, error) {
	const op = "notification.Repository.BeginPending"
	if err := r.upsertState(ctx, op, s, ` WHERE notification_state.state = 'ok'`); err != nil {
		return corenotification.State{}, err
	}
	get := r.GetState
	key := s.NodeID
	if s.MonitorID != "" {
		get, key = r.GetMonitorState, s.MonitorID
	}
	stored, found, err := get(ctx, s.Trigger, key, s.Subject)
	if err != nil {
		return corenotification.State{}, err
	}
	if !found {
		return corenotification.State{}, errs.New(errs.CodeUnavailable, "the notification state was deleted concurrently").WithOp(op)
	}
	return stored, nil
}

// UpsertState records the evaluator state for its (trigger, node, subject).
func (r *Repository) UpsertState(ctx context.Context, s corenotification.State) error {
	return r.upsertState(ctx, "notification.Repository.UpsertState", s, "")
}

// upsertState writes s; guard, when set, is appended to the ON CONFLICT
// branch and limits which existing rows may be replaced.
func (r *Repository) upsertState(ctx context.Context, op string, s corenotification.State, guard string) error {
	baseline := s.Baseline
	if baseline == nil {
		baseline = map[string]any{}
	}
	raw, err := json.Marshal(baseline)
	if err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "the state baseline is not valid JSON").WithOp(op)
	}
	// A node row keeps the exact statement the previous release ran; a
	// monitor row conflicts on ux_notification_state_monitor instead.
	subjectColumn, conflict, subjectID := "node_id", "(trigger, node_id, subject)", s.NodeID
	if s.MonitorID != "" {
		subjectColumn, conflict, subjectID = "monitor_id", "(trigger, monitor_id, subject) WHERE monitor_id IS NOT NULL", s.MonitorID
	}
	q := `
		INSERT INTO notification_state (trigger, ` + subjectColumn + `, subject, state, severity, value, condition_since, state_since,
			last_notified_at, last_notified_kind, flap_count, flap_window_start, baseline, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
		ON CONFLICT ` + conflict + ` DO UPDATE SET
			state = EXCLUDED.state, severity = EXCLUDED.severity, value = EXCLUDED.value,
			condition_since = EXCLUDED.condition_since, state_since = EXCLUDED.state_since,
			last_notified_at = EXCLUDED.last_notified_at, last_notified_kind = EXCLUDED.last_notified_kind,
			flap_count = EXCLUDED.flap_count, flap_window_start = EXCLUDED.flap_window_start,
			baseline = EXCLUDED.baseline, updated_at = now()`
	if _, err := r.pool.Exec(ctx, q+guard, string(s.Trigger), subjectID, s.Subject, string(s.State), string(s.Severity),
		s.Value, s.ConditionSince, s.StateSince, s.LastNotifiedAt, string(s.LastNotifiedKind), s.FlapCount,
		s.FlapWindowStart, raw); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the notification state").WithOp(op)
	}
	return nil
}

// ListActiveStates returns every node state that is not ok, oldest first.
// Monitor states (node_id NULL) belong to the monitor evaluator.
func (r *Repository) ListActiveStates(ctx context.Context) ([]corenotification.State, error) {
	const op = "notification.Repository.ListActiveStates"

	rows, err := r.pool.Query(ctx, `SELECT `+stateColumns+` FROM notification_state
		WHERE state <> 'ok' AND node_id IS NOT NULL ORDER BY state_since, trigger, node_id, subject`)
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

// execer is the subset of *pgxpool.Pool and pgx.Tx that resetNodeStates
// needs — satisfied by either, so the same statement can run standalone
// (ResetNodeStates) or as part of a larger transaction (the step 10
// unmute API, clearing alerts_muted_until and resetting state together).
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// resetNodeStates sets every non-ok notification_state row for nodeID to
// ok, in one statement: state_since now, severity and value cleared, the
// flap counters cleared. last_notified_at / last_notified_kind are left
// as they were — a mute ending is not itself a notification, so there is
// nothing new to record there, and leaving them is simplest where
// nothing requires otherwise.
//
// container_restart rows also drop last_restarts, whatever their state: the
// counter kept moving while the node was muted, and comparing against the
// pre-mute value would announce those restarts as if they had just happened.
// The next observation sets it again without firing.
func resetNodeStates(ctx context.Context, exec execer, nodeID string) error {
	const q = `
		UPDATE notification_state SET
			state = 'ok', severity = '', value = NULL, condition_since = NULL,
			state_since = CASE WHEN state <> 'ok' THEN now() ELSE state_since END,
			flap_count = 0, flap_window_start = NULL, updated_at = now(),
			baseline = CASE WHEN trigger = 'container_restart' THEN baseline - 'last_restarts' ELSE baseline END
		WHERE node_id = $1
		  AND (state <> 'ok' OR (trigger = 'container_restart' AND baseline ? 'last_restarts'))`
	_, err := exec.Exec(ctx, q, nodeID)
	return err
}

// ResetNodeStates is [resetNodeStates] over the repository's own pool: a
// standalone reset with no accompanying nodes-table write. The evaluator
// itself no longer calls this directly — see [Repository.EndMuteAfterExpiry],
// which the mute-ended case needs instead so the reset and the column
// clear it depends on commit together.
func (r *Repository) ResetNodeStates(ctx context.Context, nodeID string) error {
	const op = "notification.Repository.ResetNodeStates"
	if err := resetNodeStates(ctx, r.pool, nodeID); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not reset the node's notification state").WithOp(op)
	}
	return nil
}

// endMute clears nodes.alerts_muted_until for nodeID and, only if that
// clear actually matched a row, resets every non-ok notification_state
// row for it in the same statement's transaction — see resetNodeStates.
// A guard clause (e.g. "alerts_muted_until IS NULL OR alerts_muted_until
// <= $2", with guardArgs supplying $2 on) narrows which row the clear
// may touch; an empty guard clears unconditionally. Either way, nothing
// is reset unless the clear itself matched — see the two exported
// wrappers below for what each guard means and who calls it.
func endMute(ctx context.Context, exec execer, nodeID, guard string, guardArgs ...any) (bool, error) {
	q := `UPDATE nodes SET alerts_muted_until = NULL WHERE node_id = $1`
	args := append([]any{nodeID}, guardArgs...)
	if guard != "" {
		q += " AND (" + guard + ")"
	}
	tag, err := exec.Exec(ctx, q, args...)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	if err := resetNodeStates(ctx, exec, nodeID); err != nil {
		return false, err
	}
	return true, nil
}

// EndMuteAfterExpiry is the evaluator's own path for a mute it has just
// found expired (alerts_muted_until non-nil and not after its own now):
// it clears the column and resets state in one transaction, but only if
// alerts_muted_until still reads as expired at the moment of the write —
// the guard protects against a concurrent re-mute that set a *later*
// alerts_muted_until in the gap between the evaluator's read and this
// call. did is false, with nothing cleared and nothing reset, exactly
// when that race happened; the caller must then treat the node as still
// muted for this tick (see [corenotification.Evaluator]'s own doc for
// why — this is what replaced the fix round's unconditional, unguarded
// reset that ran every tick forever and could never let a node alert
// again after being muted once).
func (r *Repository) EndMuteAfterExpiry(ctx context.Context, nodeID string, now time.Time) (bool, error) {
	const op = "notification.Repository.EndMuteAfterExpiry"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not begin ending the node's mute").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	did, err := endMute(ctx, tx, nodeID, "alerts_muted_until IS NULL OR alerts_muted_until <= $2", now)
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the node's mute").WithOp(op)
	}
	if !did {
		return false, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the node's mute").WithOp(op)
	}
	return true, nil
}

// EndMuteExplicitly is the early-unmute path, DELETE
// /nodes/{nodeID}/alerts-mute: alerts_muted_until is cleared whatever it
// holds, because the caller is the one ending the mute on purpose, right
// now, not reacting to an expiry someone else might have raced ahead of.
// The same transaction resets state, so a reset failure rolls the clear
// back too: a node is never left reading unmuted with stale non-ok rows
// still on it.
//
// A node that is not muted is left alone: its states are live, and
// resetting them would silence an alert that is firing right now. ok is
// false only if nodeID does not exist.
func (r *Repository) EndMuteExplicitly(ctx context.Context, nodeID string) (bool, error) {
	const op = "notification.Repository.EndMuteExplicitly"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not begin ending the node's mute").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var muted bool
	err = tx.QueryRow(ctx, `SELECT alerts_muted_until IS NOT NULL FROM nodes WHERE node_id = $1 FOR NO KEY UPDATE`, nodeID).Scan(&muted)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the node's mute").WithOp(op)
	}
	if !muted {
		return true, nil
	}
	if _, err := endMute(ctx, tx, nodeID, ""); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the node's mute").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not end the node's mute").WithOp(op)
	}
	return true, nil
}

// MuteNode sets nodeID's mute window, replacing any existing one.
func (r *Repository) MuteNode(ctx context.Context, nodeID string, until time.Time) (corenotification.NodeAlerts, error) {
	const op = "notification.Repository.MuteNode"
	return updateNodeAlerts(ctx, r.pool, op, `alerts_muted_until = $2`, nodeID, until)
}

// ArchiveNode retires nodeID from evaluation and silently resets its
// non-ok states, in one transaction. Archiving an archived node keeps the
// original time.
func (r *Repository) ArchiveNode(ctx context.Context, nodeID string, at time.Time) (corenotification.NodeAlerts, error) {
	const op = "notification.Repository.ArchiveNode"
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return corenotification.NodeAlerts{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin archiving the node").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	out, err := updateNodeAlerts(ctx, tx, op, `archived_at = COALESCE(archived_at, $2)`, nodeID, at)
	if err != nil {
		return corenotification.NodeAlerts{}, err
	}
	if err := resetNodeStates(ctx, tx, nodeID); err != nil {
		return corenotification.NodeAlerts{}, errs.Wrap(err, errs.CodeUnavailable, "could not reset the node's notification state").WithOp(op)
	}
	if err := tx.Commit(ctx); err != nil {
		return corenotification.NodeAlerts{}, errs.Wrap(err, errs.CodeUnavailable, "could not archive the node").WithOp(op)
	}
	return out, nil
}

// UnarchiveNode returns nodeID to evaluation. Its states were reset when it
// was archived, so nothing is reset here.
func (r *Repository) UnarchiveNode(ctx context.Context, nodeID string) error {
	const op = "notification.Repository.UnarchiveNode"
	_, err := updateNodeAlerts(ctx, r.pool, op, `archived_at = NULL`, nodeID)
	return err
}

type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// updateNodeAlerts applies set (a constant fragment) to one node; a missing
// node is CodeNotFound.
func updateNodeAlerts(ctx context.Context, q rowQuerier, op, set, nodeID string, args ...any) (corenotification.NodeAlerts, error) {
	var out corenotification.NodeAlerts
	err := q.QueryRow(ctx, `UPDATE nodes SET `+set+` WHERE node_id = $1 RETURNING alerts_muted_until, archived_at`,
		append([]any{nodeID}, args...)...).Scan(&out.MutedUntil, &out.ArchivedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, errs.New(errs.CodeNotFound, "node not found").WithOp(op).WithDetail("node_id", nodeID)
	}
	if err != nil {
		return out, errs.Wrap(err, errs.CodeUnavailable, "could not update the node").WithOp(op)
	}
	return out, nil
}
