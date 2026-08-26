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
	"encoding/json"
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

// GetUser implements [coreuser.Store].
func (r *Repository) GetUser(ctx context.Context, userID string) (coreuser.User, error) {
	const op = "user.Repository.GetUser"

	const q = `
		SELECT id, username, password_hash, COALESCE(email, ''), disabled_at, created_at, updated_at
		FROM users
		WHERE id = $1`

	var u coreuser.User
	err := r.pool.QueryRow(ctx, q, userID).
		Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Email, &u.DisabledAt, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return coreuser.User{}, errs.New(errs.CodeNotFound, "no such user").WithOp(op)
		}
		return coreuser.User{}, errs.Wrap(err, errs.CodeUnavailable, "could not look up the user").WithOp(op)
	}
	return u, nil
}

// AdminCreateUser creates a user with a generated one-time password, for the
// admin Users page's "Create user" flow — unlike [Repository.CreateUser],
// which the CLI uses with a caller-supplied, already-hashed password, this
// always generates the credential and records who created the account.
func (r *Repository) AdminCreateUser(ctx context.Context, username, email, actorUserID string, now time.Time) (coreuser.User, string, error) {
	const op = "user.Repository.AdminCreateUser"

	spec := coreuser.CreateSpec{Username: username, Email: email}
	plaintext, err := coreuser.GenerateOneTimePassword()
	if err != nil {
		return coreuser.User{}, "", err
	}
	spec.Password = plaintext
	if err := spec.Validate(); err != nil {
		return coreuser.User{}, "", err
	}
	hash, err := coreuser.HashPassword(plaintext)
	if err != nil {
		return coreuser.User{}, "", err
	}

	u := coreuser.User{ID: id.New(), Username: username, Email: email, PasswordHash: hash, CreatedAt: now, UpdatedAt: now}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return coreuser.User{}, "", errs.Wrap(err, errs.CodeUnavailable, "could not begin creating the user").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insertUser = `
		INSERT INTO users (id, username, password_hash, email, created_at, updated_at)
		VALUES ($1, $2, $3, NULLIF($4, ''), $5, $5)`
	if _, err := tx.Exec(ctx, insertUser, u.ID, u.Username, u.PasswordHash, u.Email, now); err != nil {
		if isUniqueViolation(err) {
			return coreuser.User{}, "", errs.New(errs.CodeAlreadyExists, "a user with that username already exists").WithOp(op)
		}
		return coreuser.User{}, "", errs.Wrap(err, errs.CodeUnavailable, "could not create the user").WithOp(op)
	}

	if err := insertAudit(ctx, tx, actorUserID, u.ID, coreuser.AuditActionCreateUser,
		map[string]any{"username": username}, now); err != nil {
		return coreuser.User{}, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return coreuser.User{}, "", errs.Wrap(err, errs.CodeUnavailable, "could not create the user").WithOp(op)
	}
	return u, plaintext, nil
}

// DisableUser marks a user unable to authenticate and immediately
// terminates every session they currently hold — see the admin Users page's
// "will terminate all active sessions right away" warning. Refuses, per
// [coreuser.ErrLastAdminGrant], to disable the last enabled user holding an
// active fleet-wide admin grant. Disabling an already-disabled user is a
// no-op, not an error, and writes no additional audit entry.
func (r *Repository) DisableUser(ctx context.Context, userID, actorUserID string, now time.Time) error {
	const op = "user.Repository.DisableUser"

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin disabling the user").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var disabledAt *time.Time
	err = tx.QueryRow(ctx, `SELECT disabled_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&disabledAt)
	switch {
	case err == pgx.ErrNoRows:
		return errs.New(errs.CodeNotFound, "no such user").WithOp(op)
	case err != nil:
		return errs.Wrap(err, errs.CodeUnavailable, "could not look up the user").WithOp(op)
	}
	if disabledAt != nil {
		return nil
	}

	if isFleetAdmin, err := isActiveFleetWideAdmin(ctx, tx, userID); err != nil {
		return err
	} else if isFleetAdmin {
		otherExists, err := otherEnabledFleetWideAdminExists(ctx, tx, userID)
		if err != nil {
			return err
		}
		if !otherExists {
			return coreuser.ErrLastAdminGrant
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE users SET disabled_at = $2, updated_at = $2 WHERE id = $1`, userID, now); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not disable the user").WithOp(op)
	}
	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, now); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not terminate the user's sessions").WithOp(op)
	}
	if err := insertAudit(ctx, tx, actorUserID, userID, coreuser.AuditActionDisableUser, map[string]any{}, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not disable the user").WithOp(op)
	}
	return nil
}

// EnableUser reverses [Repository.DisableUser]. Enabling an already-enabled
// user is a no-op, not an error, and writes no additional audit entry.
func (r *Repository) EnableUser(ctx context.Context, userID, actorUserID string, now time.Time) error {
	const op = "user.Repository.EnableUser"

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin enabling the user").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var disabledAt *time.Time
	err = tx.QueryRow(ctx, `SELECT disabled_at FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&disabledAt)
	switch {
	case err == pgx.ErrNoRows:
		return errs.New(errs.CodeNotFound, "no such user").WithOp(op)
	case err != nil:
		return errs.Wrap(err, errs.CodeUnavailable, "could not look up the user").WithOp(op)
	}
	if disabledAt == nil {
		return nil
	}

	if _, err := tx.Exec(ctx, `UPDATE users SET disabled_at = NULL, updated_at = $2 WHERE id = $1`, userID, now); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not enable the user").WithOp(op)
	}
	if err := insertAudit(ctx, tx, actorUserID, userID, coreuser.AuditActionEnableUser, map[string]any{}, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not enable the user").WithOp(op)
	}
	return nil
}

// ResetPassword issues a fresh generated password for userID, invalidating
// whatever password they had. The plaintext is returned once — there is no
// "reveal password" API, the same rule [coreuser.GeneratedSession] documents
// for session tokens.
func (r *Repository) ResetPassword(ctx context.Context, userID, actorUserID string, now time.Time) (string, error) {
	const op = "user.Repository.ResetPassword"

	plaintext, err := coreuser.GenerateOneTimePassword()
	if err != nil {
		return "", err
	}
	hash, err := coreuser.HashPassword(plaintext)
	if err != nil {
		return "", err
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeUnavailable, "could not begin the password reset").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2, updated_at = $3 WHERE id = $1`, userID, hash, now)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeUnavailable, "could not reset the password").WithOp(op)
	}
	if tag.RowsAffected() == 0 {
		return "", errs.New(errs.CodeNotFound, "no such user").WithOp(op)
	}
	if err := insertAudit(ctx, tx, actorUserID, userID, coreuser.AuditActionResetPassword, map[string]any{}, now); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", errs.Wrap(err, errs.CodeUnavailable, "could not reset the password").WithOp(op)
	}
	return plaintext, nil
}

// ListUsersWithGrants returns every user alongside their current active role
// grants, for the admin Users page's list view.
func (r *Repository) ListUsersWithGrants(ctx context.Context) ([]coreuser.UserWithGrants, error) {
	const op = "user.Repository.ListUsersWithGrants"

	users, err := r.ListUsers(ctx)
	if err != nil {
		return nil, err
	}

	const q = `
		SELECT id, user_id, node_id, role_name, granted_at, COALESCE(granted_by, ''), revoked_at, COALESCE(revoked_by, '')
		FROM user_node_roles
		WHERE revoked_at IS NULL
		ORDER BY user_id, granted_at DESC`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role grants").WithOp(op)
	}
	defer rows.Close()

	byUser := make(map[string][]coreuser.NodeRole)
	for rows.Next() {
		var g coreuser.NodeRole
		if err := rows.Scan(&g.ID, &g.UserID, &g.NodeID, &g.Role, &g.GrantedAt, &g.GrantedBy, &g.RevokedAt, &g.RevokedBy); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a role grant").WithOp(op)
		}
		byUser[g.UserID] = append(byUser[g.UserID], g)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list role grants").WithOp(op)
	}

	out := make([]coreuser.UserWithGrants, 0, len(users))
	for _, u := range users {
		out = append(out, coreuser.UserWithGrants{User: u, Grants: byUser[u.ID]})
	}
	return out, nil
}

// ListAudit implements the admin Users page's per-user activity view:
// every recorded action taken against targetUserID, newest first.
func (r *Repository) ListAudit(ctx context.Context, targetUserID string) ([]coreuser.AuditEntry, error) {
	const op = "user.Repository.ListAudit"

	const q = `
		SELECT a.id, a.actor_user_id, COALESCE(actor.username, ''), a.target_user_id, a.action, a.detail, a.created_at
		FROM user_audit_log a
		LEFT JOIN users actor ON actor.id = a.actor_user_id
		WHERE a.target_user_id = $1
		ORDER BY a.created_at DESC`

	rows, err := r.pool.Query(ctx, q, targetUserID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list the audit trail").WithOp(op)
	}
	defer rows.Close()

	var out []coreuser.AuditEntry
	for rows.Next() {
		var e coreuser.AuditEntry
		var detail []byte
		if err := rows.Scan(&e.ID, &e.ActorUserID, &e.ActorUsername, &e.TargetUserID, &e.Action, &detail, &e.CreatedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read an audit entry").WithOp(op)
		}
		if len(detail) > 0 {
			if err := json.Unmarshal(detail, &e.Detail); err != nil {
				return nil, errs.Wrap(err, errs.CodeInternal, "could not decode an audit entry's detail").WithOp(op)
			}
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list the audit trail").WithOp(op)
	}
	return out, nil
}

// isActiveFleetWideAdmin reports whether userID currently holds an active,
// unrevoked fleet-wide admin grant.
func isActiveFleetWideAdmin(ctx context.Context, tx pgx.Tx, userID string) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1 FROM user_node_roles
			WHERE user_id = $1 AND role_name = $2 AND node_id IS NULL AND revoked_at IS NULL
		)`
	var exists bool
	if err := tx.QueryRow(ctx, q, userID, coreuser.RoleAdmin).Scan(&exists); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not check the user's admin grants")
	}
	return exists, nil
}

// otherEnabledFleetWideAdminExists reports whether some user other than
// excludeUserID currently holds an active fleet-wide admin grant and is
// enabled. FOR UPDATE locks the matching rows for the rest of the caller's
// transaction, so two concurrent attempts to remove the last two admins
// serialize — the second re-evaluates against the first's committed change
// rather than both independently seeing "someone else still has it."
func otherEnabledFleetWideAdminExists(ctx context.Context, tx pgx.Tx, excludeUserID string) (bool, error) {
	const q = `
		SELECT EXISTS (
			SELECT 1
			FROM user_node_roles unr
			JOIN users u ON u.id = unr.user_id
			WHERE unr.role_name = $2
			  AND unr.node_id IS NULL
			  AND unr.revoked_at IS NULL
			  AND u.disabled_at IS NULL
			  AND unr.user_id <> $1
			FOR UPDATE OF unr
		)`
	var exists bool
	if err := tx.QueryRow(ctx, q, excludeUserID, coreuser.RoleAdmin).Scan(&exists); err != nil {
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not check remaining admins")
	}
	return exists, nil
}

// insertAudit records one user-management action within the caller's
// transaction, so the action and its audit entry commit or roll back
// together.
func insertAudit(ctx context.Context, tx pgx.Tx, actorUserID, targetUserID, action string, detail map[string]any, now time.Time) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not encode the audit entry")
	}
	const q = `
		INSERT INTO user_audit_log (id, actor_user_id, target_user_id, action, detail, created_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := tx.Exec(ctx, q, id.New(), actorUserID, targetUserID, action, payload, now); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the audit entry")
	}
	return nil
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

// RevokeAllSessions implements [coreuser.SessionStore], for an operator
// forcing a logout. Unlike the internal session revocation
// [Repository.DisableUser] also performs, this records its own
// force_logout audit entry, since here it is the whole of what the caller
// asked for rather than a side effect of disabling the account.
func (r *Repository) RevokeAllSessions(ctx context.Context, userID, actorUserID string, now time.Time) error {
	const op = "user.Repository.RevokeAllSessions"

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin the logout").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not look up the user").WithOp(op)
	}
	if !exists {
		return errs.New(errs.CodeNotFound, "no such user").WithOp(op)
	}

	if _, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = $2 WHERE user_id = $1 AND revoked_at IS NULL`, userID, now); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not terminate the user's sessions").WithOp(op)
	}
	if err := insertAudit(ctx, tx, actorUserID, userID, coreuser.AuditActionForceLogout, map[string]any{}, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not terminate the user's sessions").WithOp(op)
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
// re-run of the same `user grant` command does not create a second row, and
// (via the RETURNING-gated audit insert below) does not write a second
// audit entry either.
func (r *Repository) Grant(ctx context.Context, spec coreuser.GrantSpec, now time.Time) error {
	const op = "user.Repository.Grant"

	var nodeID any
	if !spec.FleetWide {
		nodeID = spec.NodeID
	}

	detail := map[string]any{"role": spec.Role, "fleet_wide": spec.FleetWide}
	if !spec.FleetWide {
		detail["node_id"] = spec.NodeID
	}
	payload, err := json.Marshal(detail)
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not encode the audit entry").WithOp(op)
	}

	const q = `
		WITH ins AS (
			INSERT INTO user_node_roles (id, user_id, node_id, role_name, granted_at, granted_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))
			ON CONFLICT (user_id, COALESCE(node_id, ''), role_name) WHERE revoked_at IS NULL DO NOTHING
			RETURNING user_id
		)
		INSERT INTO user_audit_log (id, actor_user_id, target_user_id, action, detail, created_at)
		SELECT $7, $6, ins.user_id, $8, $9, $5
		FROM ins`

	if _, err := r.pool.Exec(ctx, q,
		id.New(), spec.UserID, nodeID, spec.Role, now, spec.GrantedBy,
		id.New(), coreuser.AuditActionGrantRole, payload,
	); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the role grant").WithOp(op)
	}
	return nil
}

// RevokeGrant implements [coreuser.AuthzStore]. Refuses, per
// [coreuser.ErrLastAdminGrant], to revoke the last enabled user's active
// fleet-wide admin grant. Revoking an unknown or already-revoked grant is
// not an error — the caller's intent holds either way — and writes no
// audit entry, the same idempotency [Repository.DisableUser] documents.
func (r *Repository) RevokeGrant(ctx context.Context, grantID, revokedBy string, now time.Time) error {
	const op = "user.Repository.RevokeGrant"

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not begin the revoke").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID, roleName string
	var nodeID *string
	err = tx.QueryRow(ctx, `
		SELECT user_id, role_name, node_id FROM user_node_roles
		WHERE id = $1 AND revoked_at IS NULL
		FOR UPDATE`, grantID).Scan(&userID, &roleName, &nodeID)
	switch {
	case err == pgx.ErrNoRows:
		return nil
	case err != nil:
		return errs.Wrap(err, errs.CodeUnavailable, "could not look up the role grant").WithOp(op)
	}

	if roleName == coreuser.RoleAdmin && nodeID == nil {
		otherExists, err := otherEnabledFleetWideAdminExists(ctx, tx, userID)
		if err != nil {
			return err
		}
		if !otherExists {
			return coreuser.ErrLastAdminGrant
		}
	}

	if _, err := tx.Exec(ctx, `
		UPDATE user_node_roles SET revoked_at = $2, revoked_by = NULLIF($3, '') WHERE id = $1`,
		grantID, now, revokedBy); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the role grant").WithOp(op)
	}

	detail := map[string]any{"role": roleName, "fleet_wide": nodeID == nil}
	if nodeID != nil {
		detail["node_id"] = *nodeID
	}
	if err := insertAudit(ctx, tx, revokedBy, userID, coreuser.AuditActionRevokeRole, detail, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
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

// GrantedNodeIDs implements [coreuser.AuthzStore].
func (r *Repository) GrantedNodeIDs(ctx context.Context, userID string, permission coreuser.Permission) (bool, []string, error) {
	const op = "user.Repository.GrantedNodeIDs"

	const q = `
		SELECT unr.node_id
		FROM user_node_roles unr
		JOIN role_permissions rp ON rp.role_name = unr.role_name
		WHERE unr.user_id = $1 AND unr.revoked_at IS NULL AND rp.permission_key = $2`

	rows, err := r.pool.Query(ctx, q, userID, string(permission))
	if err != nil {
		return false, nil, errs.Wrap(err, errs.CodeUnavailable, "could not list granted nodes").WithOp(op)
	}
	defer rows.Close()

	var nodeIDs []string
	for rows.Next() {
		var nodeID *string
		if err := rows.Scan(&nodeID); err != nil {
			return false, nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a granted node").WithOp(op)
		}
		if nodeID == nil {
			// A fleet-wide grant applies to every node; nothing else in the
			// result set can narrow that, so there is nothing left to read.
			return true, nil, nil
		}
		nodeIDs = append(nodeIDs, *nodeID)
	}
	if err := rows.Err(); err != nil {
		return false, nil, errs.Wrap(err, errs.CodeUnavailable, "could not list granted nodes").WithOp(op)
	}
	return false, nodeIDs, nil
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
