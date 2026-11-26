package monitor

import (
	"context"
	"errors"

	coremonitor "github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
)

func groupNotFound(op, groupID string) error {
	return errs.New(errs.CodeNotFound, "monitor group not found").WithOp(op).WithDetail("group_id", groupID)
}

func groupWriteError(op string, err error) error {
	if pgCode(err) == "23505" {
		return errs.New(errs.CodeAlreadyExists, "a monitor group with that name already exists").WithOp(op).WithDetail("field", "name")
	}
	return errs.Wrap(err, errs.CodeUnavailable, "could not save the monitor group").WithOp(op)
}

// ListGroups returns every group by position, then name.
func (r *Repository) ListGroups(ctx context.Context) ([]coremonitor.Group, error) {
	const op = "monitor.Repository.ListGroups"
	rows, err := r.pool.Query(ctx, `SELECT id, name, position, created_at, updated_at FROM monitor_groups
		ORDER BY position, lower(name), id`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list monitor groups").WithOp(op)
	}
	defer rows.Close()
	out := []coremonitor.Group{}
	for rows.Next() {
		var g coremonitor.Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Position, &g.CreatedAt, &g.UpdatedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not read a monitor group").WithOp(op)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGroup stores a new group.
func (r *Repository) CreateGroup(ctx context.Context, g coremonitor.Group) (coremonitor.Group, error) {
	const op = "monitor.Repository.CreateGroup"
	g.ID = id.New()
	err := r.pool.QueryRow(ctx, `INSERT INTO monitor_groups (id, name, position) VALUES ($1, $2, $3)
		RETURNING created_at, updated_at`, g.ID, g.Name, g.Position).Scan(&g.CreatedAt, &g.UpdatedAt)
	if err != nil {
		return coremonitor.Group{}, groupWriteError(op, err)
	}
	return g, nil
}

// UpdateGroup renames or reorders a group.
func (r *Repository) UpdateGroup(ctx context.Context, g coremonitor.Group) (coremonitor.Group, error) {
	const op = "monitor.Repository.UpdateGroup"
	err := r.pool.QueryRow(ctx, `UPDATE monitor_groups SET name = $2, position = $3, updated_at = now()
		WHERE id = $1 RETURNING created_at, updated_at`, g.ID, g.Name, g.Position).Scan(&g.CreatedAt, &g.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return coremonitor.Group{}, groupNotFound(op, g.ID)
	}
	if err != nil {
		return coremonitor.Group{}, groupWriteError(op, err)
	}
	return g, nil
}

// DeleteGroup removes a group; its monitors become ungrouped.
func (r *Repository) DeleteGroup(ctx context.Context, groupID string) error {
	const op = "monitor.Repository.DeleteGroup"
	tag, err := r.pool.Exec(ctx, `DELETE FROM monitor_groups WHERE id = $1`, groupID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not delete the monitor group").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return groupNotFound(op, groupID)
	}
	return nil
}
