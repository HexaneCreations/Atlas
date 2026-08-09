package alert

import (
	"context"
	"fmt"
	"time"

	corealert "github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultHistoryLimit = 100
	maxHistoryLimit     = 500
)

// Repository reads and writes alert rules, their live state, and history.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const ruleColumns = `id, name, description, enabled, kind, severity,
	metric, comparison, threshold, for_seconds, node_id, topic, subject, created_at, updated_at`

func scanRule(row pgx.Row) (corealert.Rule, error) {
	var (
		r                                      corealert.Rule
		metric, comparison, nodeID, topic, sub *string
		threshold                              *float64
	)
	err := row.Scan(&r.ID, &r.Name, &r.Description, &r.Enabled, &r.Kind, &r.Severity,
		&metric, &comparison, &threshold, &r.For, &nodeID, &topic, &sub, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return corealert.Rule{}, err
	}
	r.For *= time.Second
	if metric != nil {
		r.Metric = *metric
	}
	if comparison != nil {
		r.Comparison = corealert.Comparison(*comparison)
	}
	if threshold != nil {
		r.Threshold = *threshold
	}
	if nodeID != nil {
		r.NodeID = *nodeID
	}
	if topic != nil {
		r.Topic = *topic
	}
	if sub != nil {
		r.Subject = *sub
	}
	return r, nil
}

// ListRules returns every rule, newest first.
func (r *Repository) ListRules(ctx context.Context) ([]corealert.Rule, error) {
	const op = "alert.Repository.ListRules"

	rows, err := r.pool.Query(ctx, `SELECT `+ruleColumns+` FROM alert_rules ORDER BY created_at DESC`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list alert rules").WithOp(op)
	}
	defer rows.Close()

	rules := []corealert.Rule{}
	for rows.Next() {
		rule, err := scanRule(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read an alert rule").WithOp(op)
		}
		rules = append(rules, rule)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list alert rules").WithOp(op)
	}
	return rules, nil
}

// GetRule returns one rule by id.
func (r *Repository) GetRule(ctx context.Context, ruleID string) (corealert.Rule, error) {
	const op = "alert.Repository.GetRule"

	row := r.pool.QueryRow(ctx, `SELECT `+ruleColumns+` FROM alert_rules WHERE id = $1`, ruleID)
	rule, err := scanRule(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return corealert.Rule{}, errs.New(errs.CodeNotFound, "alert rule not found").WithOp(op).WithDetail("id", ruleID)
		}
		return corealert.Rule{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the alert rule").WithOp(op)
	}
	return rule, nil
}

// CreateRule persists a new rule, assigning it an id.
func (r *Repository) CreateRule(ctx context.Context, rule corealert.Rule) (corealert.Rule, error) {
	const op = "alert.Repository.CreateRule"

	rule.ID = id.New()
	now := time.Now()
	rule.CreatedAt, rule.UpdatedAt = now, now

	const q = `
		INSERT INTO alert_rules (id, name, description, enabled, kind, severity,
			metric, comparison, threshold, for_seconds, node_id, topic, subject, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`

	_, err := r.pool.Exec(ctx, q, rule.ID, rule.Name, rule.Description, rule.Enabled, rule.Kind, rule.Severity,
		nullString(rule.Metric), nullString(string(rule.Comparison)), nullFloat(rule.Threshold, rule.Kind == corealert.KindThreshold),
		int(rule.For/time.Second), nullString(rule.NodeID), nullString(rule.Topic), nullString(rule.Subject),
		rule.CreatedAt, rule.UpdatedAt)
	if err != nil {
		return corealert.Rule{}, errs.Wrap(err, errs.CodeUnavailable, "could not create the alert rule").WithOp(op)
	}
	return rule, nil
}

// UpdateRule replaces an existing rule's fields.
func (r *Repository) UpdateRule(ctx context.Context, rule corealert.Rule) (corealert.Rule, error) {
	const op = "alert.Repository.UpdateRule"

	rule.UpdatedAt = time.Now()

	// RETURNING created_at rather than trusting the caller's rule: the
	// caller only has what it decoded from the request body, which never
	// carries created_at, and echoing that zero value back would silently
	// corrupt every update response.
	const q = `
		UPDATE alert_rules SET
			name = $2, description = $3, enabled = $4, kind = $5, severity = $6,
			metric = $7, comparison = $8, threshold = $9, for_seconds = $10,
			node_id = $11, topic = $12, subject = $13, updated_at = $14
		WHERE id = $1
		RETURNING created_at`

	row := r.pool.QueryRow(ctx, q, rule.ID, rule.Name, rule.Description, rule.Enabled, rule.Kind, rule.Severity,
		nullString(rule.Metric), nullString(string(rule.Comparison)), nullFloat(rule.Threshold, rule.Kind == corealert.KindThreshold),
		int(rule.For/time.Second), nullString(rule.NodeID), nullString(rule.Topic), nullString(rule.Subject), rule.UpdatedAt)
	if err := row.Scan(&rule.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return corealert.Rule{}, errs.New(errs.CodeNotFound, "alert rule not found").WithOp(op).WithDetail("id", rule.ID)
		}
		return corealert.Rule{}, errs.Wrap(err, errs.CodeUnavailable, "could not update the alert rule").WithOp(op)
	}
	return rule, nil
}

// DeleteRule removes a rule and its live state (cascade), leaving history
// intact as an audit trail.
func (r *Repository) DeleteRule(ctx context.Context, ruleID string) error {
	const op = "alert.Repository.DeleteRule"

	tag, err := r.pool.Exec(ctx, `DELETE FROM alert_rules WHERE id = $1`, ruleID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the alert rule").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "alert rule not found").WithOp(op).WithDetail("id", ruleID)
	}
	return nil
}

const stateColumns = `rule_id, node_id, series_key, state, value, message, pending_since, fired_at, resolved_at, updated_at`

func scanState(row pgx.Row) (corealert.AlertState, error) {
	var (
		s                                 corealert.AlertState
		value                             *float64
		pendingSince, firedAt, resolvedAt *time.Time
	)
	err := row.Scan(&s.RuleID, &s.NodeID, &s.SeriesKey, &s.State, &value, &s.Message,
		&pendingSince, &firedAt, &resolvedAt, &s.UpdatedAt)
	if err != nil {
		return corealert.AlertState{}, err
	}
	if value != nil {
		s.Value = *value
	}
	if pendingSince != nil {
		s.PendingSince = *pendingSince
	}
	if firedAt != nil {
		s.FiredAt = *firedAt
	}
	if resolvedAt != nil {
		s.ResolvedAt = *resolvedAt
	}
	return s, nil
}

// GetState returns the current state for (ruleID, nodeID, seriesKey).
func (r *Repository) GetState(ctx context.Context, ruleID, nodeID, seriesKey string) (corealert.AlertState, bool, error) {
	const op = "alert.Repository.GetState"

	row := r.pool.QueryRow(ctx, `SELECT `+stateColumns+` FROM alert_states WHERE rule_id = $1 AND node_id = $2 AND series_key = $3`,
		ruleID, nodeID, seriesKey)
	state, err := scanState(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return corealert.AlertState{}, false, nil
		}
		return corealert.AlertState{}, false, errs.Wrap(err, errs.CodeUnavailable, "could not read alert state").WithOp(op)
	}
	return state, true, nil
}

// SaveState upserts the current state for one (rule, node, series).
func (r *Repository) SaveState(ctx context.Context, state corealert.AlertState) error {
	const op = "alert.Repository.SaveState"

	const q = `
		INSERT INTO alert_states (rule_id, node_id, series_key, state, value, message, pending_since, fired_at, resolved_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (rule_id, node_id, series_key) DO UPDATE SET
			state = EXCLUDED.state, value = EXCLUDED.value, message = EXCLUDED.message,
			pending_since = EXCLUDED.pending_since, fired_at = EXCLUDED.fired_at,
			resolved_at = EXCLUDED.resolved_at, updated_at = EXCLUDED.updated_at`

	_, err := r.pool.Exec(ctx, q, state.RuleID, state.NodeID, state.SeriesKey, state.State, state.Value, state.Message,
		nullTime(state.PendingSince), nullTime(state.FiredAt), nullTime(state.ResolvedAt), state.UpdatedAt)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not save alert state").WithOp(op)
	}
	return nil
}

// ListActiveStates returns every state that is pending or firing.
func (r *Repository) ListActiveStates(ctx context.Context) ([]corealert.AlertState, error) {
	const op = "alert.Repository.ListActiveStates"

	q := `SELECT ` + stateColumns + ` FROM alert_states WHERE state IN ('pending', 'firing') ORDER BY updated_at DESC`
	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list active alerts").WithOp(op)
	}
	defer rows.Close()

	states := []corealert.AlertState{}
	for rows.Next() {
		s, err := scanState(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read an alert state").WithOp(op)
		}
		states = append(states, s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list active alerts").WithOp(op)
	}
	return states, nil
}

// AppendHistory records one durable transition.
func (r *Repository) AppendHistory(ctx context.Context, entry corealert.HistoryEntry) error {
	const op = "alert.Repository.AppendHistory"

	if entry.ID == "" {
		entry.ID = id.New()
	}
	if entry.Time.IsZero() {
		entry.Time = time.Now()
	}

	if entry.Severity == "" {
		entry.Severity = corealert.SeverityWarning
	}

	const q = `
		INSERT INTO alert_history (id, time, rule_id, node_id, state, severity, value, message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (id, time) DO NOTHING`

	_, err := r.pool.Exec(ctx, q, entry.ID, entry.Time, entry.RuleID, entry.NodeID, entry.State, entry.Severity, entry.Value, entry.Message)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record alert history").WithOp(op)
	}
	return nil
}

// QueryHistory returns history entries matching filter, newest first.
func (r *Repository) QueryHistory(ctx context.Context, filter corealert.HistoryFilter) ([]corealert.HistoryEntry, error) {
	const op = "alert.Repository.QueryHistory"

	limit := filter.Limit
	if limit <= 0 || limit > maxHistoryLimit {
		limit = defaultHistoryLimit
	}

	q := `SELECT id, time, rule_id, node_id, state, severity, value, message FROM alert_history WHERE true`
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if filter.RuleID != "" {
		q += " AND rule_id = " + arg(filter.RuleID)
	}
	if filter.NodeID != "" {
		q += " AND node_id = " + arg(filter.NodeID)
	}
	if !filter.Since.IsZero() {
		q += " AND time >= " + arg(filter.Since)
	}
	if !filter.Before.IsZero() {
		q += " AND time < " + arg(filter.Before)
	}
	q += " ORDER BY time DESC LIMIT " + arg(limit)

	rows, err := r.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not query alert history").WithOp(op)
	}
	defer rows.Close()

	entries := []corealert.HistoryEntry{}
	for rows.Next() {
		var e corealert.HistoryEntry
		var value *float64
		if err := rows.Scan(&e.ID, &e.Time, &e.RuleID, &e.NodeID, &e.State, &e.Severity, &value, &e.Message); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a history entry").WithOp(op)
		}
		if value != nil {
			e.Value = *value
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not query alert history").WithOp(op)
	}
	return entries, nil
}

func nullString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nullFloat(v float64, present bool) *float64 {
	if !present {
		return nil
	}
	return &v
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

var _ corealert.Store = (*Repository)(nil)
