package notification

import (
	"context"
	"encoding/json"
	"errors"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const ruleColumns = `trigger, COALESCE(node_id, ''), enabled, warn_threshold, crit_threshold, for_seconds,
	recovery_seconds, hysteresis, cooldown_seconds, send_recovery, subject_excludes, updated_at, updated_by`

func scanRule(row pgx.Row) (corenotification.RuleSpec, error) {
	var s corenotification.RuleSpec
	var trigger string
	if err := row.Scan(&trigger, &s.NodeID, &s.Enabled, &s.WarnThreshold, &s.CritThreshold, &s.ForSeconds,
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
		ORDER BY trigger, node_id IS NOT NULL, node_id`)
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
		overrides, err := rulesWhere(ctx, tx, `trigger = $1 AND node_id IS NOT NULL FOR UPDATE`, string(spec.Trigger))
		if err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not read the node overrides").WithOp(op)
		}
		for _, o := range overrides {
			if _, err := corenotification.MergeRule(spec, &o, o.NodeID); err != nil {
				return errs.Wrap(err, errs.CodeInvalidArgument,
					"the new default conflicts with the override for node %s", o.NodeID).WithOp(op)
			}
		}
	} else {
		global, err := rulesWhere(ctx, tx, `trigger = $1 AND node_id IS NULL FOR SHARE`, string(spec.Trigger))
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

	var nodeID *string
	if !spec.IsGlobal() {
		nodeID = &spec.NodeID
	}
	const q = `
		INSERT INTO notification_rules (id, trigger, node_id, enabled, warn_threshold, crit_threshold, for_seconds,
			recovery_seconds, hysteresis, cooldown_seconds, send_recovery, subject_excludes, updated_at, updated_by)
		VALUES ($13, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, now(), $12)
		ON CONFLICT (trigger, (COALESCE(node_id, ''))) DO UPDATE SET
			enabled = EXCLUDED.enabled, warn_threshold = EXCLUDED.warn_threshold,
			crit_threshold = EXCLUDED.crit_threshold, for_seconds = EXCLUDED.for_seconds,
			recovery_seconds = EXCLUDED.recovery_seconds, hysteresis = EXCLUDED.hysteresis,
			cooldown_seconds = EXCLUDED.cooldown_seconds, send_recovery = EXCLUDED.send_recovery,
			subject_excludes = EXCLUDED.subject_excludes, updated_at = now(), updated_by = EXCLUDED.updated_by`
	if _, err := tx.Exec(ctx, q, string(spec.Trigger), nodeID, spec.Enabled, spec.WarnThreshold, spec.CritThreshold,
		spec.ForSeconds, spec.RecoverySeconds, spec.Hysteresis, spec.CooldownSeconds, spec.SendRecovery,
		spec.SubjectExcludes, updatedBy, id.New()); err != nil {
		if pgCode(err) == "23503" {
			return errs.New(errs.CodeNotFound, "node %q is not known", spec.NodeID).WithOp(op)
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
		q = `DELETE FROM notification_rules WHERE trigger = $1 AND node_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM notification_rules o WHERE o.trigger = $1 AND o.node_id IS NOT NULL)`
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
		if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM notification_rules WHERE trigger = $1 AND node_id IS NULL)`,
			string(trigger)).Scan(&exists); err == nil && exists {
			return errs.New(errs.CodeFailedPrecondition, "trigger %q still has node overrides; delete them first", trigger).WithOp(op)
		}
	}
	return errs.New(errs.CodeNotFound, "notification rule not found").WithOp(op)
}

// EffectiveRule resolves trigger for nodeID: the global default with the
// node's override, if any, merged over it per field. nodeID "" returns the
// global rule itself.
func (r *Repository) EffectiveRule(ctx context.Context, trigger corenotification.Trigger, nodeID string) (corenotification.Rule, error) {
	const op = "notification.Repository.EffectiveRule"

	specs, err := rulesWhere(ctx, r.pool, `trigger = $1 AND (node_id IS NULL OR node_id = $2)`, string(trigger), nodeID)
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

const stateColumns = `trigger, node_id, subject, state, severity, value, condition_since, state_since,
	last_notified_at, last_notified_kind, flap_count, flap_window_start, baseline, updated_at`

func scanState(row pgx.Row) (corenotification.State, error) {
	var s corenotification.State
	var trigger, state, severity, kind string
	var baseline []byte
	if err := row.Scan(&trigger, &s.NodeID, &s.Subject, &state, &severity, &s.Value, &s.ConditionSince,
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

// UpsertState records the evaluator state for its (trigger, node, subject).
func (r *Repository) UpsertState(ctx context.Context, s corenotification.State) error {
	const op = "notification.Repository.UpsertState"

	baseline := s.Baseline
	if baseline == nil {
		baseline = map[string]any{}
	}
	raw, err := json.Marshal(baseline)
	if err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "the state baseline is not valid JSON").WithOp(op)
	}
	const q = `
		INSERT INTO notification_state (trigger, node_id, subject, state, severity, value, condition_since, state_since,
			last_notified_at, last_notified_kind, flap_count, flap_window_start, baseline, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, now())
		ON CONFLICT (trigger, node_id, subject) DO UPDATE SET
			state = EXCLUDED.state, severity = EXCLUDED.severity, value = EXCLUDED.value,
			condition_since = EXCLUDED.condition_since, state_since = EXCLUDED.state_since,
			last_notified_at = EXCLUDED.last_notified_at, last_notified_kind = EXCLUDED.last_notified_kind,
			flap_count = EXCLUDED.flap_count, flap_window_start = EXCLUDED.flap_window_start,
			baseline = EXCLUDED.baseline, updated_at = now()`
	if _, err := r.pool.Exec(ctx, q, string(s.Trigger), s.NodeID, s.Subject, string(s.State), string(s.Severity),
		s.Value, s.ConditionSince, s.StateSince, s.LastNotifiedAt, string(s.LastNotifiedKind), s.FlapCount,
		s.FlapWindowStart, raw); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the notification state").WithOp(op)
	}
	return nil
}

// ListActiveStates returns every state that is not ok, oldest first.
func (r *Repository) ListActiveStates(ctx context.Context) ([]corenotification.State, error) {
	const op = "notification.Repository.ListActiveStates"

	rows, err := r.pool.Query(ctx, `SELECT `+stateColumns+` FROM notification_state
		WHERE state <> 'ok' ORDER BY state_since, trigger, node_id, subject`)
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
func resetNodeStates(ctx context.Context, exec execer, nodeID string) error {
	const q = `
		UPDATE notification_state SET
			state = 'ok', severity = '', value = NULL, condition_since = NULL,
			state_since = now(), flap_count = 0, flap_window_start = NULL, updated_at = now()
		WHERE node_id = $1 AND state <> 'ok'`
	_, err := exec.Exec(ctx, q, nodeID)
	return err
}

// ResetNodeStates is [resetNodeStates] over the repository's own pool —
// see corenotification.StateStore, which the evaluator's mute-ended reset
// calls through.
func (r *Repository) ResetNodeStates(ctx context.Context, nodeID string) error {
	const op = "notification.Repository.ResetNodeStates"
	if err := resetNodeStates(ctx, r.pool, nodeID); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not reset the node's notification state").WithOp(op)
	}
	return nil
}
