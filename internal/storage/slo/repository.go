// Package slo is the PostgreSQL-backed implementation of
// [github.com/hexane/atlas/internal/core/slo.Store].
package slo

import (
	"context"
	"time"

	corealert "github.com/hexane/atlas/internal/core/alert"
	coreslo "github.com/hexane/atlas/internal/core/slo"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository reads and writes SLO definitions.
type Repository struct{ pool *pgxpool.Pool }

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

const sloColumns = `id, name, node_id, signal, metric, comparison, threshold,
	target_percentage, window_seconds, warning_budget_percent, created_at, updated_at`

func scanSLO(row pgx.Row) (coreslo.Definition, error) {
	var (
		d             coreslo.Definition
		comparison    string
		windowSeconds int
	)
	err := row.Scan(&d.ID, &d.Name, &d.NodeID, &d.Signal, &d.Metric, &comparison, &d.Threshold,
		&d.TargetPercentage, &windowSeconds, &d.WarningBudgetPercent, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return coreslo.Definition{}, err
	}
	d.Comparison = corealert.Comparison(comparison)
	d.Window = time.Duration(windowSeconds) * time.Second
	return d, nil
}

// ListSLOs returns every SLO, newest first.
func (r *Repository) ListSLOs(ctx context.Context) ([]coreslo.Definition, error) {
	const op = "slo.Repository.ListSLOs"

	rows, err := r.pool.Query(ctx, `SELECT `+sloColumns+` FROM slos ORDER BY created_at DESC`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list SLOs").WithOp(op)
	}
	defer rows.Close()

	defs := []coreslo.Definition{}
	for rows.Next() {
		d, err := scanSLO(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read an SLO").WithOp(op)
		}
		defs = append(defs, d)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list SLOs").WithOp(op)
	}
	return defs, nil
}

// GetSLO returns one SLO by id.
func (r *Repository) GetSLO(ctx context.Context, sloID string) (coreslo.Definition, error) {
	const op = "slo.Repository.GetSLO"

	row := r.pool.QueryRow(ctx, `SELECT `+sloColumns+` FROM slos WHERE id = $1`, sloID)
	d, err := scanSLO(row)
	if err != nil {
		if err == pgx.ErrNoRows {
			return coreslo.Definition{}, errs.New(errs.CodeNotFound, "SLO not found").WithOp(op).WithDetail("id", sloID)
		}
		return coreslo.Definition{}, errs.Wrap(err, errs.CodeUnavailable, "could not read the SLO").WithOp(op)
	}
	return d, nil
}

// CreateSLO persists a new SLO, assigning it an id.
func (r *Repository) CreateSLO(ctx context.Context, def coreslo.Definition) (coreslo.Definition, error) {
	const op = "slo.Repository.CreateSLO"

	def.ID = id.New()
	now := time.Now()
	def.CreatedAt, def.UpdatedAt = now, now

	const q = `
		INSERT INTO slos (id, name, node_id, signal, metric, comparison, threshold,
			target_percentage, window_seconds, warning_budget_percent, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`

	_, err := r.pool.Exec(ctx, q, def.ID, def.Name, def.NodeID, def.Signal, def.Metric, string(def.Comparison), def.Threshold,
		def.TargetPercentage, int(def.Window/time.Second), def.WarningBudgetPercent, def.CreatedAt, def.UpdatedAt)
	if err != nil {
		return coreslo.Definition{}, errs.Wrap(err, errs.CodeUnavailable, "could not create the SLO").WithOp(op)
	}
	return def, nil
}

// UpdateSLO replaces an existing SLO's fields.
func (r *Repository) UpdateSLO(ctx context.Context, def coreslo.Definition) (coreslo.Definition, error) {
	const op = "slo.Repository.UpdateSLO"

	def.UpdatedAt = time.Now()

	const q = `
		UPDATE slos SET
			name = $2, node_id = $3, signal = $4, metric = $5, comparison = $6, threshold = $7,
			target_percentage = $8, window_seconds = $9, warning_budget_percent = $10, updated_at = $11
		WHERE id = $1
		RETURNING created_at`

	row := r.pool.QueryRow(ctx, q, def.ID, def.Name, def.NodeID, def.Signal, def.Metric, string(def.Comparison), def.Threshold,
		def.TargetPercentage, int(def.Window/time.Second), def.WarningBudgetPercent, def.UpdatedAt)
	if err := row.Scan(&def.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return coreslo.Definition{}, errs.New(errs.CodeNotFound, "SLO not found").WithOp(op).WithDetail("id", def.ID)
		}
		return coreslo.Definition{}, errs.Wrap(err, errs.CodeUnavailable, "could not update the SLO").WithOp(op)
	}
	return def, nil
}

// DeleteSLO removes an SLO.
func (r *Repository) DeleteSLO(ctx context.Context, sloID string) error {
	const op = "slo.Repository.DeleteSLO"

	tag, err := r.pool.Exec(ctx, `DELETE FROM slos WHERE id = $1`, sloID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the SLO").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.CodeNotFound, "SLO not found").WithOp(op).WithDetail("id", sloID)
	}
	return nil
}

var _ coreslo.Store = (*Repository)(nil)
