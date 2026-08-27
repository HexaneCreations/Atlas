// Package pageauthz is the PostgreSQL-backed implementation of
// github.com/hexane/atlas/internal/core/pageauthz.Store — the page-
// visibility access-control layer, storage-side. Separate from
// internal/storage/user for the same reason internal/core/pageauthz is
// separate from internal/core/user: this is a second, independent
// authorization axis, not an extension of the first.
//
// It shares the users table (foreign keys) and the user_audit_log table
// (writes) with internal/storage/user rather than duplicating either — a
// RoleAccess/UserAccess grant is, for audit purposes, the same kind of fact
// ("who granted what, to whom, when") the existing grant_role/revoke_role
// actions already record.
package pageauthz

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the page-access storage surface.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// CreateRoleAccess implements [pageauthz.Store].
func (r *Repository) CreateRoleAccess(ctx context.Context, name, createdBy string, now time.Time) error {
	const op = "pageauthz.Repository.CreateRoleAccess"

	const q = `INSERT INTO role_access (name, created_at, created_by) VALUES ($1, $2, NULLIF($3, ''))`
	if _, err := r.pool.Exec(ctx, q, name, now, createdBy); err != nil {
		if isUniqueViolation(err) {
			return errs.New(errs.CodeAlreadyExists, "a role bundle named %q already exists", name).WithOp(op)
		}
		return errs.Wrap(err, errs.CodeUnavailable, "could not create the role bundle").WithOp(op)
	}
	return nil
}

// AddPageToRoleAccess implements [pageauthz.Store]. Rejects a fleet-only
// page via [pageauthz.ValidateBundleMembership] before touching the
// database — see migrations/0015_page_access.sql's header for why this is
// an application-level check rather than a CHECK constraint.
func (r *Repository) AddPageToRoleAccess(ctx context.Context, roleAccessName string, page pageauthz.Page) error {
	const op = "pageauthz.Repository.AddPageToRoleAccess"

	if err := pageauthz.ValidateBundleMembership(page); err != nil {
		return err
	}

	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_access WHERE name = $1)`, roleAccessName).Scan(&exists); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not look up the role bundle").WithOp(op)
	}
	if !exists {
		return errs.New(errs.CodeNotFound, "no such role bundle %q", roleAccessName).WithOp(op)
	}

	const q = `
		INSERT INTO role_access_pages (role_access_name, page_key)
		VALUES ($1, $2)
		ON CONFLICT DO NOTHING`
	if _, err := r.pool.Exec(ctx, q, roleAccessName, string(page)); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not add the page to the role bundle").WithOp(op)
	}
	return nil
}

// RemovePageFromRoleAccess implements [pageauthz.Store]. Removing a page
// not in the bundle is not an error — the caller's intent holds either way,
// the same idempotency internal/storage/user's RevokeGrant documents.
func (r *Repository) RemovePageFromRoleAccess(ctx context.Context, roleAccessName string, page pageauthz.Page) error {
	const op = "pageauthz.Repository.RemovePageFromRoleAccess"

	const q = `DELETE FROM role_access_pages WHERE role_access_name = $1 AND page_key = $2`
	if _, err := r.pool.Exec(ctx, q, roleAccessName, string(page)); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not remove the page from the role bundle").WithOp(op)
	}
	return nil
}

// ListRoleAccessDefinitions implements [pageauthz.Store].
func (r *Repository) ListRoleAccessDefinitions(ctx context.Context) ([]pageauthz.RoleAccess, error) {
	const op = "pageauthz.Repository.ListRoleAccessDefinitions"

	const q = `
		SELECT ra.name, ra.created_at, COALESCE(ra.created_by, ''), rap.page_key
		FROM role_access ra
		LEFT JOIN role_access_pages rap ON rap.role_access_name = ra.name
		ORDER BY ra.name, rap.page_key`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role bundles").WithOp(op)
	}
	defer rows.Close()

	byName := make(map[string]*pageauthz.RoleAccess)
	var order []string
	for rows.Next() {
		var name, createdBy string
		var createdAt time.Time
		var pageKey *string
		if err := rows.Scan(&name, &createdAt, &createdBy, &pageKey); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a role bundle").WithOp(op)
		}
		ra, ok := byName[name]
		if !ok {
			ra = &pageauthz.RoleAccess{Name: name, CreatedAt: createdAt, CreatedBy: createdBy}
			byName[name] = ra
			order = append(order, name)
		}
		if pageKey != nil {
			ra.Pages = append(ra.Pages, pageauthz.Page(*pageKey))
		}
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role bundles").WithOp(op)
	}

	out := make([]pageauthz.RoleAccess, 0, len(order))
	for _, name := range order {
		out = append(out, *byName[name])
	}
	return out, nil
}

// AssignRoleAccess implements [pageauthz.Store]. ON CONFLICT DO NOTHING
// against the partial unique index is the idempotency an identical,
// already-active assignment requires; the RETURNING-gated audit insert
// records nothing when it was a no-op, the same pattern
// internal/storage/user.Repository.Grant uses.
func (r *Repository) AssignRoleAccess(ctx context.Context, spec pageauthz.RoleAccessAssignmentSpec, now time.Time) error {
	const op = "pageauthz.Repository.AssignRoleAccess"

	var nodeID any
	if !spec.FleetWide {
		nodeID = spec.NodeID
	}

	detail := map[string]any{"role_access": spec.RoleAccessName, "fleet_wide": spec.FleetWide}
	if !spec.FleetWide {
		detail["node_id"] = spec.NodeID
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not encode the audit entry").WithOp(op)
	}

	const q = `
		WITH ins AS (
			INSERT INTO user_role_access (id, user_id, role_access_name, node_id, granted_at, granted_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
			ON CONFLICT (user_id, role_access_name, COALESCE(node_id, '')) WHERE revoked_at IS NULL DO NOTHING
			RETURNING user_id
		)
		INSERT INTO user_audit_log (id, actor_user_id, target_user_id, action, detail, created_at)
		SELECT $7, $6, ins.user_id, $8, $9, $5
		FROM ins`

	if _, err := r.pool.Exec(ctx, q,
		id.New(), spec.UserID, spec.RoleAccessName, nodeID, now, spec.GrantedBy,
		id.New(), pageauthz.AuditActionAssignRoleAccess, payload,
	); err != nil {
		if isForeignKeyViolation(err) {
			return errs.New(errs.CodeNotFound, "no such user or role bundle").WithOp(op)
		}
		return errs.Wrap(err, errs.CodeUnavailable, "could not assign the role bundle").WithOp(op)
	}
	return nil
}

// RevokeRoleAccessAssignment implements [pageauthz.Store]. Revoking an
// unknown or already-revoked assignment is not an error, the same
// idempotency [Repository.RemovePageFromRoleAccess] documents.
func (r *Repository) RevokeRoleAccessAssignment(ctx context.Context, assignmentID, revokedBy string, now time.Time) error {
	const op = "pageauthz.Repository.RevokeRoleAccessAssignment"

	const q = `
		WITH updated AS (
			UPDATE user_role_access
			SET revoked_at = $2, revoked_by = NULLIF($3, '')
			WHERE id = $1 AND revoked_at IS NULL
			RETURNING user_id, role_access_name, node_id
		)
		INSERT INTO user_audit_log (id, actor_user_id, target_user_id, action, detail, created_at)
		SELECT $4, $3, updated.user_id, $5,
		       jsonb_build_object('role_access', updated.role_access_name, 'fleet_wide', updated.node_id IS NULL, 'node_id', updated.node_id),
		       $2
		FROM updated`

	if _, err := r.pool.Exec(ctx, q, assignmentID, now, revokedBy, id.New(), pageauthz.AuditActionRevokeRoleAccess); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the role bundle assignment").WithOp(op)
	}
	return nil
}

// ListRoleAccessAssignments implements [pageauthz.Store].
func (r *Repository) ListRoleAccessAssignments(ctx context.Context, userID string) ([]pageauthz.RoleAccessAssignment, error) {
	const op = "pageauthz.Repository.ListRoleAccessAssignments"

	const q = `
		SELECT id, user_id, role_access_name, node_id, granted_at, COALESCE(granted_by, ''), revoked_at, COALESCE(revoked_by, '')
		FROM user_role_access
		WHERE user_id = $1
		ORDER BY granted_at DESC`

	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role bundle assignments").WithOp(op)
	}
	defer rows.Close()

	var out []pageauthz.RoleAccessAssignment
	for rows.Next() {
		var a pageauthz.RoleAccessAssignment
		if err := rows.Scan(&a.ID, &a.UserID, &a.RoleAccessName, &a.NodeID, &a.GrantedAt, &a.GrantedBy, &a.RevokedAt, &a.RevokedBy); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a role bundle assignment").WithOp(op)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role bundle assignments").WithOp(op)
	}
	return out, nil
}

// GrantPageAccess implements [pageauthz.Store]. Runs the scope-overlap
// conflict check against the user's active role_access coverage for this
// page, locked (FOR UPDATE) for the rest of this transaction so a
// concurrent AssignRoleAccess cannot create a fresh overlap between the
// check and this insert — the same race-closing pattern
// internal/storage/user's last-admin guard uses.
func (r *Repository) GrantPageAccess(ctx context.Context, spec pageauthz.PageGrantSpec, now time.Time) error {
	const op = "pageauthz.Repository.GrantPageAccess"

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin the grant").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	roleScopes, err := roleAccessScopesForPageTx(ctx, tx, spec.UserID, spec.Page)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not check for an overlapping role grant").WithOp(op)
	}
	requested := pageauthz.Scope{FleetWide: spec.FleetWide, NodeID: spec.NodeID}
	if pageauthz.HasConflict(roleScopes, requested) {
		return pageauthz.ErrPageAccessConflict
	}

	var nodeID any
	if !spec.FleetWide {
		nodeID = spec.NodeID
	}
	detail := map[string]any{"page": string(spec.Page), "fleet_wide": spec.FleetWide}
	if !spec.FleetWide {
		detail["node_id"] = spec.NodeID
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not encode the audit entry").WithOp(op)
	}

	const q = `
		WITH ins AS (
			INSERT INTO user_page_access (id, user_id, page_key, node_id, granted_at, granted_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
			ON CONFLICT (user_id, page_key, COALESCE(node_id, '')) WHERE revoked_at IS NULL DO NOTHING
			RETURNING user_id
		)
		INSERT INTO user_audit_log (id, actor_user_id, target_user_id, action, detail, created_at)
		SELECT $7, $6, ins.user_id, $8, $9, $5
		FROM ins`

	if _, err := tx.Exec(ctx, q,
		id.New(), spec.UserID, string(spec.Page), nodeID, now, spec.GrantedBy,
		id.New(), pageauthz.AuditActionGrantPageAccess, payload,
	); err != nil {
		if isForeignKeyViolation(err) {
			return errs.New(errs.CodeNotFound, "no such user").WithOp(op)
		}
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the page grant").WithOp(op)
	}

	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the page grant").WithOp(op)
	}
	return nil
}

// RevokePageAccess implements [pageauthz.Store]. Revoking an unknown or
// already-revoked grant is not an error, the same idempotency
// [Repository.RevokeRoleAccessAssignment] documents.
func (r *Repository) RevokePageAccess(ctx context.Context, grantID, revokedBy string, now time.Time) error {
	const op = "pageauthz.Repository.RevokePageAccess"

	const q = `
		WITH updated AS (
			UPDATE user_page_access
			SET revoked_at = $2, revoked_by = NULLIF($3, '')
			WHERE id = $1 AND revoked_at IS NULL
			RETURNING user_id, page_key, node_id
		)
		INSERT INTO user_audit_log (id, actor_user_id, target_user_id, action, detail, created_at)
		SELECT $4, $3, updated.user_id, $5,
		       jsonb_build_object('page', updated.page_key, 'fleet_wide', updated.node_id IS NULL, 'node_id', updated.node_id),
		       $2
		FROM updated`

	if _, err := r.pool.Exec(ctx, q, grantID, now, revokedBy, id.New(), pageauthz.AuditActionRevokePageAccess); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the page grant").WithOp(op)
	}
	return nil
}

// ListPageAccessGrants implements [pageauthz.Store].
func (r *Repository) ListPageAccessGrants(ctx context.Context, userID string) ([]pageauthz.PageGrant, error) {
	const op = "pageauthz.Repository.ListPageAccessGrants"

	const q = `
		SELECT id, user_id, page_key, node_id, granted_at, COALESCE(granted_by, ''), revoked_at, COALESCE(revoked_by, '')
		FROM user_page_access
		WHERE user_id = $1
		ORDER BY granted_at DESC`

	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list page grants").WithOp(op)
	}
	defer rows.Close()

	var out []pageauthz.PageGrant
	for rows.Next() {
		var g pageauthz.PageGrant
		var pageKey string
		if err := rows.Scan(&g.ID, &g.UserID, &pageKey, &g.NodeID, &g.GrantedAt, &g.GrantedBy, &g.RevokedAt, &g.RevokedBy); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a page grant").WithOp(op)
		}
		g.Page = pageauthz.Page(pageKey)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list page grants").WithOp(op)
	}
	return out, nil
}

// RoleAccessScopesForPage implements [pageauthz.Store] — the unlocked, read-
// only counterpart to [roleAccessScopesForPageTx], for callers (like
// [Repository.EffectiveAccess]) outside a mutation's transaction.
func (r *Repository) RoleAccessScopesForPage(ctx context.Context, userID string, page pageauthz.Page) ([]pageauthz.Scope, error) {
	const op = "pageauthz.Repository.RoleAccessScopesForPage"

	const q = `
		SELECT ura.node_id
		FROM user_role_access ura
		JOIN role_access_pages rap ON rap.role_access_name = ura.role_access_name
		WHERE ura.user_id = $1 AND rap.page_key = $2 AND ura.revoked_at IS NULL`

	rows, err := r.pool.Query(ctx, q, userID, string(page))
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not check role bundle coverage").WithOp(op)
	}
	defer rows.Close()
	return scanScopes(rows)
}

// roleAccessScopesForPageTx is [Repository.RoleAccessScopesForPage] locked
// against tx via FOR UPDATE OF ura — see [Repository.GrantPageAccess].
func roleAccessScopesForPageTx(ctx context.Context, tx pgx.Tx, userID string, page pageauthz.Page) ([]pageauthz.Scope, error) {
	const q = `
		SELECT ura.node_id
		FROM user_role_access ura
		JOIN role_access_pages rap ON rap.role_access_name = ura.role_access_name
		WHERE ura.user_id = $1 AND rap.page_key = $2 AND ura.revoked_at IS NULL
		FOR UPDATE OF ura`

	rows, err := tx.Query(ctx, q, userID, string(page))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanScopes(rows)
}

func scanScopes(rows pgx.Rows) ([]pageauthz.Scope, error) {
	var out []pageauthz.Scope
	for rows.Next() {
		var nodeID *string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, err
		}
		if nodeID == nil {
			out = append(out, pageauthz.Scope{FleetWide: true})
		} else {
			out = append(out, pageauthz.Scope{NodeID: *nodeID})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// EffectiveAccess implements [pageauthz.Store] — the union of every active
// RoleAccess bundle covering page and every active direct grant for it. A
// fleet-wide row from either source makes the whole result fleet-wide,
// since nothing else can narrow it back down.
func (r *Repository) EffectiveAccess(ctx context.Context, userID string, page pageauthz.Page) (bool, []string, error) {
	const op = "pageauthz.Repository.EffectiveAccess"

	const q = `
		SELECT ura.node_id
		FROM user_role_access ura
		JOIN role_access_pages rap ON rap.role_access_name = ura.role_access_name
		WHERE ura.user_id = $1 AND rap.page_key = $2 AND ura.revoked_at IS NULL
		UNION
		SELECT upa.node_id
		FROM user_page_access upa
		WHERE upa.user_id = $1 AND upa.page_key = $2 AND upa.revoked_at IS NULL`

	rows, err := r.pool.Query(ctx, q, userID, string(page))
	if err != nil {
		return false, nil, errs.Wrap(err, errs.CodeUnavailable, "could not resolve effective page access").WithOp(op)
	}
	defer rows.Close()

	var nodeIDs []string
	for rows.Next() {
		var nodeID *string
		if err := rows.Scan(&nodeID); err != nil {
			return false, nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a page access row").WithOp(op)
		}
		if nodeID == nil {
			return true, nil, nil
		}
		nodeIDs = append(nodeIDs, *nodeID)
	}
	if err := rows.Err(); err != nil {
		return false, nil, errs.Wrap(err, errs.CodeUnavailable, "could not resolve effective page access").WithOp(op)
	}
	return false, nodeIDs, nil
}

// isUniqueViolation reports whether err is a Postgres unique_violation
// (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errs.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// isForeignKeyViolation reports whether err is a Postgres
// foreign_key_violation (SQLSTATE 23503).
func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errs.As(err, &pgErr) && pgErr.SQLState() == "23503"
}

var _ pageauthz.Store = (*Repository)(nil)
