package user

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/core/access"
	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
)

// accessEditorVia tags every audit entry the access editor writes, so the
// Activity view can tell an editor save from a per-grant action.
const accessEditorVia = "access_editor"

// UserAccess returns userID's access as the access editor sees it: active
// role grants other than superadmin, active direct page grants, and active
// bundle assignments with their pages, all read in one snapshot so the
// [access.State.Version] matches exactly the rows returned.
func (r *Repository) UserAccess(ctx context.Context, userID string) (access.State, error) {
	const op = "user.Repository.UserAccess"

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin reading the user's access").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM users WHERE id = $1)`, userID).Scan(&exists); err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not look up the user").WithOp(op)
	}
	if !exists {
		return access.State{}, errs.New(errs.CodeNotFound, "no such user").WithOp(op)
	}
	return loadAccess(ctx, tx, userID, false)
}

// ReplaceUserAccess applies desired to userID's editable access in one
// transaction, provided version still matches the state the caller read
// ([access.ErrStaleVersion] otherwise). It refuses with
// [coreuser.ErrLastAdminGrant] when the change would remove the last enabled
// fleet-wide admin, and with whatever [access.Plan] refuses; any refusal
// rolls back everything. Each grant and revoke writes one audit entry; a
// request that changes nothing writes none.
//
// The users row is locked for the whole transaction, so two saves for the
// same user serialize and the second sees the first's version change.
func (r *Repository) ReplaceUserAccess(ctx context.Context, userID, version string, desired access.Desired, actorUserID string, now time.Time) (access.State, access.Changes, error) {
	const op = "user.Repository.ReplaceUserAccess"

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return access.State{}, access.Changes{}, errs.Wrap(err, errs.CodeUnavailable, "could not begin the access change").WithOp(op)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&one)
	switch {
	case err == pgx.ErrNoRows:
		return access.State{}, access.Changes{}, errs.New(errs.CodeNotFound, "no such user").WithOp(op)
	case err != nil:
		return access.State{}, access.Changes{}, errs.Wrap(err, errs.CodeUnavailable, "could not look up the user").WithOp(op)
	}

	current, err := loadAccess(ctx, tx, userID, true)
	if err != nil {
		return access.State{}, access.Changes{}, err
	}
	if current.Version() != version {
		return access.State{}, access.Changes{}, access.ErrStaleVersion
	}

	changes, err := applyAccess(ctx, tx, userID, current, desired, actorUserID, now)
	if err != nil {
		return access.State{}, access.Changes{}, err
	}
	after, err := loadAccess(ctx, tx, userID, false)
	if err != nil {
		return access.State{}, access.Changes{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return access.State{}, access.Changes{}, errs.Wrap(err, errs.CodeUnavailable, "could not commit the access change").WithOp(op)
	}
	return after, changes, nil
}

// applyAccess plans and writes the change from current to desired within
// tx, including the last-admin check and one audit entry per change.
func applyAccess(ctx context.Context, tx pgx.Tx, userID string, current access.State, desired access.Desired, actorUserID string, now time.Time) (access.Changes, error) {
	const op = "user.applyAccess"

	known, err := knownNodes(ctx, tx, desired)
	if err != nil {
		return access.Changes{}, err
	}
	changes, err := access.Plan(current, desired, known)
	if err != nil {
		return access.Changes{}, err
	}

	if changes.RevokesFleetWideAdmin() {
		otherExists, err := otherEnabledFleetWideAdminExists(ctx, tx, userID)
		if err != nil {
			return access.Changes{}, err
		}
		if !otherExists {
			return access.Changes{}, coreuser.ErrLastAdminGrant
		}
	}

	for _, g := range changes.RevokeRoles {
		if _, err := tx.Exec(ctx, `
			UPDATE user_node_roles SET revoked_at = $2, revoked_by = NULLIF($3, '')
			WHERE id = $1 AND revoked_at IS NULL`, g.ID, now, actorUserID); err != nil {
			return access.Changes{}, errs.Wrap(err, errs.CodeUnavailable, "could not revoke a role grant").WithOp(op)
		}
		if err := insertAudit(ctx, tx, actorUserID, userID, coreuser.AuditActionRevokeRole,
			auditDetail("role", g.Role.Role, g.Scope), now); err != nil {
			return access.Changes{}, err
		}
	}
	for _, g := range changes.RevokePages {
		if _, err := tx.Exec(ctx, `
			UPDATE user_page_access SET revoked_at = $2, revoked_by = NULLIF($3, '')
			WHERE id = $1 AND revoked_at IS NULL`, g.ID, now, actorUserID); err != nil {
			return access.Changes{}, errs.Wrap(err, errs.CodeUnavailable, "could not revoke a page grant").WithOp(op)
		}
		if err := insertAudit(ctx, tx, actorUserID, userID, corepageauthz.AuditActionRevokePageAccess,
			auditDetail("page", string(g.Page.Page), g.Scope), now); err != nil {
			return access.Changes{}, err
		}
	}
	for _, rl := range changes.GrantRoles {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_node_roles (id, user_id, node_id, role_name, granted_at, granted_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
			id.New(), userID, scopeNodeID(rl.Scope), rl.Role, now, actorUserID); err != nil {
			return access.Changes{}, errs.Wrap(err, errs.CodeUnavailable, "could not record a role grant").WithOp(op)
		}
		if err := insertAudit(ctx, tx, actorUserID, userID, coreuser.AuditActionGrantRole,
			auditDetail("role", rl.Role, rl.Scope), now); err != nil {
			return access.Changes{}, err
		}
	}
	for _, p := range changes.GrantPages {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_page_access (id, user_id, page_key, node_id, granted_at, granted_by)
			VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''))`,
			id.New(), userID, string(p.Page), scopeNodeID(p.Scope), now, actorUserID); err != nil {
			return access.Changes{}, errs.Wrap(err, errs.CodeUnavailable, "could not record a page grant").WithOp(op)
		}
		if err := insertAudit(ctx, tx, actorUserID, userID, corepageauthz.AuditActionGrantPageAccess,
			auditDetail("page", string(p.Page), p.Scope), now); err != nil {
			return access.Changes{}, err
		}
	}
	return changes, nil
}

// loadAccess reads userID's editor-visible access within tx. With lock set,
// every returned row is locked FOR UPDATE — the bundle rows too, so a
// concurrent bundle assignment cannot create an overlap between the
// conflict check and the insert (the same pattern GrantPageAccess uses).
func loadAccess(ctx context.Context, tx pgx.Tx, userID string, lock bool) (access.State, error) {
	const op = "user.loadAccess"
	forUpdate := ""
	if lock {
		forUpdate = " FOR UPDATE"
	}

	var s access.State
	rows, err := tx.Query(ctx, `
		SELECT id, node_id, role_name, granted_at, COALESCE(granted_by, '')
		FROM user_node_roles
		WHERE user_id = $1 AND revoked_at IS NULL AND role_name <> $2
		ORDER BY granted_at, id`+forUpdate, userID, coreuser.RoleSuperadmin)
	if err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read role grants").WithOp(op)
	}
	for rows.Next() {
		var g access.RoleGrant
		var nodeID *string
		if err := rows.Scan(&g.ID, &nodeID, &g.Role.Role, &g.GrantedAt, &g.GrantedBy); err != nil {
			rows.Close()
			return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read a role grant").WithOp(op)
		}
		g.Scope = scopeOf(nodeID)
		s.Roles = append(s.Roles, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read role grants").WithOp(op)
	}

	rows, err = tx.Query(ctx, `
		SELECT id, node_id, page_key, granted_at, COALESCE(granted_by, '')
		FROM user_page_access
		WHERE user_id = $1 AND revoked_at IS NULL
		ORDER BY granted_at, id`+forUpdate, userID)
	if err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read page grants").WithOp(op)
	}
	for rows.Next() {
		var g access.PageGrant
		var nodeID *string
		var pageKey string
		if err := rows.Scan(&g.ID, &nodeID, &pageKey, &g.GrantedAt, &g.GrantedBy); err != nil {
			rows.Close()
			return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read a page grant").WithOp(op)
		}
		g.Scope = scopeOf(nodeID)
		g.Page.Page = corepageauthz.Page(pageKey)
		s.Pages = append(s.Pages, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read page grants").WithOp(op)
	}

	if lock {
		// FOR UPDATE cannot be combined with the GROUP BY below, so the
		// assignment rows are locked by a separate statement first.
		if _, err := tx.Exec(ctx, `
			SELECT id FROM user_role_access
			WHERE user_id = $1 AND revoked_at IS NULL
			FOR UPDATE`, userID); err != nil {
			return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not lock bundle assignments").WithOp(op)
		}
	}
	rows, err = tx.Query(ctx, `
		SELECT ura.id, ura.role_access_name, ura.node_id, ura.granted_at, COALESCE(ura.granted_by, ''),
		       COALESCE(array_agg(rap.page_key ORDER BY rap.page_key) FILTER (WHERE rap.page_key IS NOT NULL), '{}')
		FROM user_role_access ura
		LEFT JOIN role_access_pages rap ON rap.role_access_name = ura.role_access_name
		WHERE ura.user_id = $1 AND ura.revoked_at IS NULL
		GROUP BY ura.id
		ORDER BY ura.granted_at, ura.id`, userID)
	if err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read bundle assignments").WithOp(op)
	}
	for rows.Next() {
		var b access.Bundle
		var nodeID *string
		var pages []string
		if err := rows.Scan(&b.ID, &b.Name, &nodeID, &b.GrantedAt, &b.GrantedBy, &pages); err != nil {
			rows.Close()
			return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read a bundle assignment").WithOp(op)
		}
		b.Scope = scopeOf(nodeID)
		for _, p := range pages {
			b.Pages = append(b.Pages, corepageauthz.Page(p))
		}
		s.Bundles = append(s.Bundles, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return access.State{}, errs.Wrap(err, errs.CodeUnavailable, "could not read bundle assignments").WithOp(op)
	}
	return s, nil
}

// knownNodes returns which of the node ids desired names exist.
func knownNodes(ctx context.Context, tx pgx.Tx, desired access.Desired) (map[string]bool, error) {
	var ids []string
	for _, r := range desired.Roles {
		if !r.Scope.FleetWide && r.Scope.NodeID != "" {
			ids = append(ids, r.Scope.NodeID)
		}
	}
	for _, p := range desired.Pages {
		if !p.Scope.FleetWide && p.Scope.NodeID != "" {
			ids = append(ids, p.Scope.NodeID)
		}
	}
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT node_id FROM nodes WHERE node_id = ANY($1)`, ids)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not look up nodes")
	}
	defer rows.Close()
	for rows.Next() {
		var nodeID string
		if err := rows.Scan(&nodeID); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a node")
		}
		out[nodeID] = true
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not look up nodes")
	}
	return out, nil
}

func scopeOf(nodeID *string) corepageauthz.Scope {
	if nodeID == nil {
		return corepageauthz.Scope{FleetWide: true}
	}
	return corepageauthz.Scope{NodeID: *nodeID}
}

func scopeNodeID(s corepageauthz.Scope) any {
	if s.FleetWide {
		return nil
	}
	return s.NodeID
}

// auditDetail is the detail shape the per-grant paths already write
// ({"role"|"page", "fleet_wide", "node_id"}), plus "via".
func auditDetail(kind, value string, s corepageauthz.Scope) map[string]any {
	d := map[string]any{kind: value, "fleet_wide": s.FleetWide, "via": accessEditorVia}
	if !s.FleetWide {
		d["node_id"] = s.NodeID
	}
	return d
}
