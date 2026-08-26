package user

import (
	"context"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Role names — the fixed set docs/adr/0011-deferred-rbac.md names. Not an
// open registry: adding a fourth role is a schema and code change, not a
// value an operator can introduce by typing a new string.
const (
	RoleViewer   = "viewer"
	RoleOperator = "operator"
	RoleAdmin    = "admin"
)

// KnownRoles are every role a grant may name.
var KnownRoles = map[string]bool{
	RoleViewer:   true,
	RoleOperator: true,
	RoleAdmin:    true,
}

// Permission is an action a role may be granted, checked per node. Expressed
// in Atlas's own resource vocabulary rather than a generic CRUD set — see
// migrations/0012_users.sql.
type Permission string

const (
	// PermissionNodeRead covers every node-scoped inventory read: containers,
	// processes, services, ports, mounts, cron jobs, the service graph, and
	// the per-node health/cost/capacity/signals summaries — everything that
	// resolves a scope through [Handler.scopeFrom].
	PermissionNodeRead Permission = "node.read"
	// PermissionNodeLogsRead covers container log content specifically.
	// Separate from PermissionNodeRead because docs/adr/0011-deferred-rbac.md
	// names it as uniquely sensitive: application logs routinely carry
	// credentials and customer data no other endpoint exposes.
	PermissionNodeLogsRead Permission = "node.logs.read"
	// PermissionFleetWrite covers privileged-configuration writes that are
	// not node-scoped: alert rules, SLO definitions, notification channels.
	// Checked with an empty node id — see [Handler.requirePermission] — so
	// only a fleet-wide grant (user_node_roles.node_id IS NULL) can satisfy
	// it; a node-scoped grant never does, since HasPermission's node_id = $2
	// branch cannot match an empty string against a real node id.
	PermissionFleetWrite Permission = "fleet.write"
	// PermissionUserManage covers the admin Users page: creating accounts,
	// granting or revoking roles, disabling/enabling accounts, resetting
	// passwords, and forcing logout. Checked the same fleet-wide-only way as
	// PermissionFleetWrite — user management is not a per-node concept.
	PermissionUserManage Permission = "user.manage"
)

// FleetWide is the sentinel passed to [GrantSpec.NodeID] to mean "every
// node", spelled out rather than left as an empty string so a caller cannot
// produce it by accident — see [GrantSpec.Validate].
const FleetWide = ""

// GrantSpec describes a role grant to create.
//
// There is deliberately no default between "this node" and "every node": a
// caller must choose NodeID or FleetWide explicitly. See [GrantSpec.Validate]
// and cmd/atlas-server's `user grant`, which requires --node-id XOR
// --fleet-wide on the command line for the same reason.
type GrantSpec struct {
	UserID string
	// NodeID is the node this grant is scoped to. Ignored when FleetWide is
	// true.
	NodeID string
	// FleetWide, when true, grants Role across every node rather than one.
	// A distinct field rather than inferring "fleet-wide" from an empty
	// NodeID: an empty string arriving from an un-set form field or a
	// forgotten flag must never silently become the broadest possible grant.
	FleetWide bool
	Role      string
	GrantedBy string
}

// Validate reports whether the spec is usable.
func (s GrantSpec) Validate() error {
	const op = "user.GrantSpec.Validate"
	switch {
	case strings.TrimSpace(s.UserID) == "":
		return errs.New(errs.CodeInvalidArgument, "a grant must name the user it applies to").WithOp(op)
	case !s.FleetWide && strings.TrimSpace(s.NodeID) == "":
		return errs.New(errs.CodeInvalidArgument,
			"a grant must name a node id, or explicitly request fleet-wide — there is no default between them").WithOp(op)
	case s.FleetWide && s.NodeID != "":
		return errs.New(errs.CodeInvalidArgument,
			"a grant cannot name a node id and request fleet-wide at the same time").WithOp(op)
	case !KnownRoles[s.Role]:
		return errs.New(errs.CodeInvalidArgument, "unknown role %q; want one of viewer, operator, admin", s.Role).WithOp(op)
	}
	return nil
}

// NodeRole is a grant row as an operator sees it.
type NodeRole struct {
	ID     string
	UserID string
	// NodeID is nil for a fleet-wide grant.
	NodeID    *string
	Role      string
	GrantedAt time.Time
	GrantedBy string
	RevokedAt *time.Time
	RevokedBy string
}

// AuthzStore is the storage surface for node-role grants and the permission
// check itself. Implemented by internal/storage/user.Repository.
type AuthzStore interface {
	// HasPermission reports whether userID currently holds permission for
	// nodeID, through either a grant scoped to that node or a fleet-wide one.
	HasPermission(ctx context.Context, userID, nodeID string, permission Permission) (bool, error)
	// Grant records a role grant. Must be idempotent for an identical,
	// already-active grant.
	Grant(ctx context.Context, spec GrantSpec, now time.Time) error
	// RevokeGrant withdraws a previously granted role.
	RevokeGrant(ctx context.Context, grantID, revokedBy string, now time.Time) error
	// ListGrants returns every grant for a user, for operator tooling.
	ListGrants(ctx context.Context, userID string) ([]NodeRole, error)
}

// ErrPermissionDenied is returned by [Authorizer.Require] when the principal
// is known but not authorized for the resource.
//
// PermissionDenied rather than Unauthenticated, and the distinction is not
// cosmetic, mirroring [fleet.ErrPeerNotAuthorized]: by the time this error
// can be produced, the session has already proven who the caller is. What is
// missing is permission, and an operator reading a 403 here should go look
// for a missing grant, not a broken login.
var ErrPermissionDenied = errs.New(errs.CodePermissionDenied, "you do not have permission to perform this action")

// ErrLastAdminGrant is returned when revoking a role grant or disabling a
// user would leave no enabled user holding an active fleet-wide admin
// grant. The admin role is what makes PermissionUserManage reachable at
// all, so removing the last one would permanently lock every operator out
// of user management — there being no self-registration or superuser
// escape hatch to recover from that, unlike [ErrPermissionDenied] this
// refuses even an otherwise-valid, otherwise-authorized request.
var ErrLastAdminGrant = errs.New(errs.CodeFailedPrecondition,
	"this is the last user with fleet-wide admin access; grant it to someone else first")

// Authorizer is the authorization policy layer: it answers whether an
// authenticated principal may perform a permission-gated action against a
// node.
//
// Per docs/adr/0011-deferred-rbac.md sec 2, this is called from handlers, not
// installed as middleware — middleware establishes identity before a handler
// has resolved which node a request actually names, and the question this
// answers needs that node.
type Authorizer struct {
	store AuthzStore
}

// NewAuthorizer builds an Authorizer over store.
func NewAuthorizer(store AuthzStore) Authorizer { return Authorizer{store: store} }

// Require returns nil when principal holds permission for nodeID, and a
// typed, client-safe error otherwise — [ErrPermissionDenied] when the
// permission check simply fails, or the store's own error for anything else
// (a database failure surfaces as CodeUnavailable, not as a false denial).
func (a Authorizer) Require(ctx context.Context, principal Principal, permission Permission, nodeID string) error {
	ok, err := a.store.HasPermission(ctx, principal.UserID, nodeID, permission)
	if err != nil {
		return err
	}
	if !ok {
		return ErrPermissionDenied
	}
	return nil
}
