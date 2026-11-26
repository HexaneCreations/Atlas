package notification

import (
	"context"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
)

// lockRuleExclusive and lockRuleShared take the per-trigger advisory lock
// SetRule already uses. Changing a rule holds it exclusively; enqueueing a
// notification holds it shared, so an enqueue either finishes before a rule
// change commits or reads the changed rule.
const (
	lockRuleExclusive = `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`
	lockRuleShared    = `SELECT pg_advisory_xact_lock_shared(hashtextextended($1, 0))`
)

// ListNodeRules resolves every catalog rule for nodeID.
func (r *Repository) ListNodeRules(ctx context.Context, nodeID string) ([]corenotification.NodeRule, error) {
	const op = "notification.Repository.ListNodeRules"
	if err := nodeKnown(ctx, r.pool, nodeID, op); err != nil {
		return nil, err
	}
	specs, err := rulesWhere(ctx, r.pool, `node_id IS NULL OR node_id = $1`, nodeID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read the notification rules").WithOp(op)
	}
	return corenotification.NodeRules(specs, nodeID), nil
}

// SetNodeRuleEnabled stores nodeID's enabled override for trigger, keeping
// any other field the override already carries. When the rule ends up
// disabled, the node's state for it is reset in the same transaction.
func (r *Repository) SetNodeRuleEnabled(
	ctx context.Context, nodeID string, trigger corenotification.Trigger, enabled bool, updatedBy string,
) (corenotification.NodeRule, error) {
	const op = "notification.Repository.SetNodeRuleEnabled"
	return r.changeNodeRule(ctx, op, nodeID, trigger, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO notification_rules (id, trigger, node_id, enabled, updated_at, updated_by)
			VALUES ($1, $2, $3, $4, now(), $5)
			ON CONFLICT (trigger, (COALESCE(node_id, ''))) DO UPDATE SET
				enabled = EXCLUDED.enabled, updated_at = now(), updated_by = EXCLUDED.updated_by`,
			id.New(), string(trigger), nodeID, enabled, updatedBy)
		return err
	})
}

// ClearNodeRuleEnabled removes nodeID's enabled override for trigger. The
// override row is deleted only when no other field is overridden. Clearing
// an override that does not exist is not an error.
func (r *Repository) ClearNodeRuleEnabled(
	ctx context.Context, nodeID string, trigger corenotification.Trigger, updatedBy string,
) (corenotification.NodeRule, error) {
	const op = "notification.Repository.ClearNodeRuleEnabled"
	return r.changeNodeRule(ctx, op, nodeID, trigger, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE notification_rules SET enabled = NULL, updated_at = now(), updated_by = $3
			WHERE trigger = $1 AND node_id = $2 AND enabled IS NOT NULL`,
			string(trigger), nodeID, updatedBy); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
			DELETE FROM notification_rules
			WHERE trigger = $1 AND node_id = $2
			  AND enabled IS NULL AND warn_threshold IS NULL AND crit_threshold IS NULL
			  AND for_seconds IS NULL AND recovery_seconds IS NULL AND hysteresis IS NULL
			  AND cooldown_seconds IS NULL AND send_recovery IS NULL AND subject_excludes IS NULL`,
			string(trigger), nodeID)
		return err
	})
}

// changeNodeRule runs write under the trigger's exclusive lock, then, if the
// rule is now disabled for the node, silently resets the node's state for it
// before committing: once the change is visible, no state of a disabled rule
// is pending or firing and no notification for it can still be enqueued.
func (r *Repository) changeNodeRule(
	ctx context.Context, op, nodeID string, trigger corenotification.Trigger, write func(pgx.Tx) error,
) (corenotification.NodeRule, error) {
	if _, err := corenotification.ParseRuleKey(string(trigger)); err != nil {
		return corenotification.NodeRule{}, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return corenotification.NodeRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin the rule change").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, lockRuleExclusive, string(trigger)); err != nil {
		return corenotification.NodeRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not lock the notification rule").WithOp(op)
	}
	if err := nodeKnown(ctx, tx, nodeID, op); err != nil {
		return corenotification.NodeRule{}, err
	}
	if err := write(tx); err != nil {
		if pgCode(err) == "23503" {
			return corenotification.NodeRule{}, errs.New(errs.CodeNotFound, "node not found").WithOp(op).WithDetail("node_id", nodeID)
		}
		return corenotification.NodeRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not save the node's alert rule").WithOp(op)
	}

	rule, err := nodeRule(ctx, tx, nodeID, trigger)
	if err != nil {
		return corenotification.NodeRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the node's alert rule").WithOp(op)
	}
	if !rule.Enabled {
		if err := resetRuleStates(ctx, tx, nodeID, trigger); err != nil {
			return corenotification.NodeRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not reset the node's alert state").WithOp(op)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return corenotification.NodeRule{}, errs.Wrap(err, errs.CodeUnavailable, "could not save the node's alert rule").WithOp(op)
	}
	return rule, nil
}

func nodeRule(ctx context.Context, q querier, nodeID string, trigger corenotification.Trigger) (corenotification.NodeRule, error) {
	specs, err := rulesWhere(ctx, q, `trigger = $1 AND (node_id IS NULL OR node_id = $2)`, string(trigger), nodeID)
	if err != nil {
		return corenotification.NodeRule{}, err
	}
	for _, rule := range corenotification.NodeRules(specs, nodeID) {
		if rule.Key == trigger {
			return rule, nil
		}
	}
	return corenotification.NodeRule{}, errs.New(errs.CodeNotFound, "unknown alert rule %q", trigger)
}

// ruleEnabled resolves whether trigger is enabled for nodeID as stored now.
func ruleEnabled(ctx context.Context, q querier, nodeID string, trigger corenotification.Trigger) (bool, error) {
	entry, ok := corenotification.CatalogEntryFor(trigger)
	if !ok {
		return true, nil // not a catalog rule: nothing can disable it
	}
	specs, err := rulesWhere(ctx, q, `trigger = $1 AND (node_id IS NULL OR node_id = $2)`, string(trigger), nodeID)
	if err != nil {
		return false, err
	}
	var global, override *bool
	for _, s := range specs {
		if s.IsGlobal() {
			global = s.Enabled
		} else {
			override = s.Enabled
		}
	}
	enabled, _ := corenotification.ResolveEnabled(entry, global, override)
	return enabled, nil
}

// resetRuleStates returns nodeID's states for trigger to ok without a
// notification, and drops the workload baselines so the first observation
// after re-enabling cannot fire.
func resetRuleStates(ctx context.Context, exec execer, nodeID string, trigger corenotification.Trigger) error {
	const q = `
		UPDATE notification_state SET
			state = 'ok', severity = '', value = NULL, condition_since = NULL,
			state_since = CASE WHEN state <> 'ok' THEN now() ELSE state_since END,
			flap_count = 0, flap_window_start = NULL, updated_at = now(),
			baseline = baseline - 'seen_up' - 'last_restarts'
		WHERE node_id = $1 AND trigger = $2
		  AND (state <> 'ok' OR baseline ?| array['seen_up', 'last_restarts'])`
	_, err := exec.Exec(ctx, q, nodeID, string(trigger))
	return err
}

func nodeKnown(ctx context.Context, q rowQuerier, nodeID, op string) error {
	var exists bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM nodes WHERE node_id = $1)`, nodeID).Scan(&exists); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not read the node").WithOp(op)
	}
	if !exists {
		return errs.New(errs.CodeNotFound, "node not found").WithOp(op).WithDetail("node_id", nodeID)
	}
	return nil
}
