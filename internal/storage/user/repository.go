// Package user is the PostgreSQL-backed implementation of the storage
// interfaces github.com/hexane/atlas/internal/core/user defines: Store,
// SessionStore, and AuthzStore.
//
// It exists as its own package, separate from core/user, for the same reason
// internal/storage/fleet is separate from internal/core/fleet: the domain
// rules (which password verifies, which role holds which permission) should
// be readable and testable with no database, and the SQL that persists them
// is a different kind of correctness problem entirely.
package user

import (
	"context"
	"time"

	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the user storage surface: identities, sessions, and node-role
// grants.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// ByUsername implements [coreuser.Store]. The lookup is case-insensitive,
// matching the unique index in migrations/0012_users.sql.
func (r *Repository) ByUsername(ctx context.Context, username string) (coreuser.User, error) {
	const op = "user.Repository.ByUsername"

	const q = `
		SELECT id, username, password_hash, COALESCE(email, ''), disabled_at, created_at, updated_at
		FROM users
		WHERE lower(username) = lower($1)`

	var u coreuser.User
	err := r.pool.QueryRow(ctx, q, username).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.DisabledAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return coreuser.User{}, errs.New(errs.CodeNotFound, "no such user").WithOp(op)
		}
		return coreuser.User{}, errs.Wrap(err, errs.CodeUnavailable, "could not look up the user").WithOp(op)
	}
	return u, nil
}

// CreateUser implements [coreuser.Store]. ID is minted here rather than
// accepted from the caller, the same convention as every other application-
// generated identifier in Atlas.
func (r *Repository) CreateUser(ctx context.Context, u coreuser.User) error {
	const op = "user.Repository.CreateUser"

	if u.ID == "" {
		u.ID = id.New()
	}

	const q = `
		INSERT INTO users (id, username, password_hash, email)
		VALUES ($1, $2, $3, NULLIF($4, ''))`

	if _, err := r.pool.Exec(ctx, q, u.ID, u.Username, u.PasswordHash, u.Email); err != nil {
		if isUniqueViolation(err) {
			return errs.New(errs.CodeAlreadyExists, "a user with that username already exists").WithOp(op)
		}
		return errs.Wrap(err, errs.CodeUnavailable, "could not create the user").WithOp(op)
	}
	return nil
}

// ListUsers implements [coreuser.Store].
func (r *Repository) ListUsers(ctx context.Context) ([]coreuser.User, error) {
	const op = "user.Repository.ListUsers"

	const q = `
		SELECT id, username, password_hash, COALESCE(email, ''), disabled_at, created_at, updated_at
		FROM users ORDER BY created_at`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list users").WithOp(op)
	}
	defer rows.Close()

	var out []coreuser.User
	for rows.Next() {
		var u coreuser.User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.DisabledAt, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a user").WithOp(op)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list users").WithOp(op)
	}
	return out, nil
}

// CreateSession implements [coreuser.SessionStore].
func (r *Repository) CreateSession(ctx context.Context, s coreuser.Session) error {
	const op = "user.Repository.CreateSession"

	const q = `
		INSERT INTO sessions (token_hash, user_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4)`

	if _, err := r.pool.Exec(ctx, q, s.TokenHash, s.UserID, s.CreatedAt, s.ExpiresAt); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not create the session").WithOp(op)
	}
	return nil
}

// Resolve implements [coreuser.SessionStore].
//
// The join to users happens here rather than as a second query: a session
// whose user has since been deleted (ON DELETE CASCADE in
// migrations/0012_users.sql makes that the only way this can occur) must
// resolve as invalid, not as a principal with an empty username.
func (r *Repository) Resolve(ctx context.Context, tokenHash string, now time.Time) (coreuser.Principal, error) {
	const op = "user.Repository.Resolve"

	const q = `
		SELECT u.id, u.username
		FROM sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1
		  AND s.revoked_at IS NULL
		  AND s.expires_at > $2
		  AND u.disabled_at IS NULL`

	var p coreuser.Principal
	err := r.pool.QueryRow(ctx, q, tokenHash, now).Scan(&p.UserID, &p.Username)
	if err != nil {
		if err == pgx.ErrNoRows {
			return coreuser.Principal{}, coreuser.ErrSessionInvalid
		}
		return coreuser.Principal{}, errs.Wrap(err, errs.CodeUnavailable, "could not resolve the session").WithOp(op)
	}
	return p, nil
}

// RevokeSession implements [coreuser.SessionStore]. Revoking an unknown or
// already-revoked session is not an error: the caller's intent — that this
// session must no longer be usable — holds either way.
func (r *Repository) RevokeSession(ctx context.Context, tokenHash string, now time.Time) error {
	const op = "user.Repository.RevokeSession"

	const q = `UPDATE sessions SET revoked_at = $2 WHERE token_hash = $1 AND revoked_at IS NULL`
	if _, err := r.pool.Exec(ctx, q, tokenHash, now); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the session").WithOp(op)
	}
	return nil
}

// HasPermission implements [coreuser.AuthzStore].
//
// A grant matches when it is scoped to nodeID directly or is fleet-wide
// (node_id IS NULL) — see migrations/0012_users.sql's doc on
// user_node_roles. The permission itself comes from the static
// role_permissions mapping, joined rather than duplicated in Go, so the
// mapping has exactly one source of truth.
func (r *Repository) HasPermission(ctx context.Context, userID, nodeID string, permission coreuser.Permission) (bool, error) {
	const op = "user.Repository.HasPermission"

	const q = `
		SELECT 1
		FROM user_node_roles unr
		JOIN role_permissions rp ON rp.role_name = unr.role_name
		WHERE unr.user_id = $1
		  AND unr.revoked_at IS NULL
		  AND (unr.node_id = $2 OR unr.node_id IS NULL)
		  AND rp.permission_key = $3
		LIMIT 1`

	var one int
	err := r.pool.QueryRow(ctx, q, userID, nodeID, string(permission)).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case err == pgx.ErrNoRows:
		return false, nil
	default:
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not check the permission grant").WithOp(op)
	}
}

// Grant implements [coreuser.AuthzStore]. ON CONFLICT DO NOTHING against the
// partial unique index on (user_id, node_id, role_name) WHERE revoked_at IS
// NULL is the idempotency an identical, already-active grant requires — a
// re-run of the same `user grant` command does not create a second row.
func (r *Repository) Grant(ctx context.Context, spec coreuser.GrantSpec, now time.Time) error {
	const op = "user.Repository.Grant"

	var nodeID any
	if !spec.FleetWide {
		nodeID = spec.NodeID
	}

	const q = `
		INSERT INTO user_node_roles (id, user_id, node_id, role_name, granted_at, granted_by)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
		ON CONFLICT (user_id, COALESCE(node_id, ''), role_name) WHERE revoked_at IS NULL DO NOTHING`

	if _, err := r.pool.Exec(ctx, q, id.New(), spec.UserID, nodeID, spec.Role, now, spec.GrantedBy); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the role grant").WithOp(op)
	}
	return nil
}

// RevokeGrant implements [coreuser.AuthzStore].
func (r *Repository) RevokeGrant(ctx context.Context, grantID, revokedBy string, now time.Time) error {
	const op = "user.Repository.RevokeGrant"

	const q = `
		UPDATE user_node_roles
		SET revoked_at = $2, revoked_by = NULLIF($3, '')
		WHERE id = $1 AND revoked_at IS NULL`

	if _, err := r.pool.Exec(ctx, q, grantID, now, revokedBy); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the role grant").WithOp(op)
	}
	return nil
}

// ListGrants implements [coreuser.AuthzStore].
func (r *Repository) ListGrants(ctx context.Context, userID string) ([]coreuser.NodeRole, error) {
	const op = "user.Repository.ListGrants"

	const q = `
		SELECT id, user_id, node_id, role_name, granted_at, COALESCE(granted_by, ''), revoked_at, COALESCE(revoked_by, '')
		FROM user_node_roles
		WHERE user_id = $1
		ORDER BY granted_at DESC`

	rows, err := r.pool.Query(ctx, q, userID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role grants").WithOp(op)
	}
	defer rows.Close()

	var out []coreuser.NodeRole
	for rows.Next() {
		var g coreuser.NodeRole
		if err := rows.Scan(&g.ID, &g.UserID, &g.NodeID, &g.Role, &g.GrantedAt, &g.GrantedBy, &g.RevokedAt, &g.RevokedBy); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a role grant").WithOp(op)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role grants").WithOp(op)
	}
	return out, nil
}

// isUniqueViolation reports whether err is a Postgres unique_violation
// (SQLSTATE 23505).
func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errs.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

var (
	_ coreuser.Store        = (*Repository)(nil)
	_ coreuser.SessionStore = (*Repository)(nil)
	_ coreuser.AuthzStore   = (*Repository)(nil)
)
