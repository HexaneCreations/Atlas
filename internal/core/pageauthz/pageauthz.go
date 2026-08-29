// Package pageauthz is the page-visibility authorization layer: a second,
// independent access-control axis from internal/core/user's operation-level
// permissions (node.read, node.logs.read, fleet.write, user.manage).
//
// The existing layer answers "may this principal perform this operation
// against this node" — it says nothing about which pages a user should even
// be offered. Today every operator holding node.read reaches every
// node-scoped page (Containers, Processes, Services, Scheduled jobs, Ports,
// Disks) at once, because they all check the same permission. This package
// answers a narrower, additional question — "may this principal reach this
// page at all, for this node" — checked alongside, never instead of, the
// existing per-operation check. A user still needs both: page access AND
// the underlying node.read/node.logs.read grant for that node. This layer
// can only narrow what is reachable; it never substitutes for the existing
// checks, and disabling it (a nil Authorizer, the same "not wired in on
// this instance" convention internal/core/user's own Authorizer follows)
// leaves every route exactly as permissive as it was before this package
// existed.
package pageauthz

import (
	"context"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Page is a gateable page in the Atlas frontend — see web/src/shell/pages.ts,
// the single source of truth this mirrors. Not an open registry: adding a
// tenth page is a code change here and in the seed rows of
// migrations/0015_page_access.sql, the same fixed-set convention
// internal/core/user.Role and .Permission already follow.
type Page string

// PageNone is the sentinel a handler passes when its route maps to no
// dedicated page yet (capacity/summary, cost/estimate, signals, health/score
// — none of which back a nav entry today; see docs/context's page audit).
// Passing it skips the page-access check entirely, the same way permission
// is skipped when h.deps.PageAuthz is nil — an explicit "this route isn't
// covered yet" rather than a page a caller forgot to name.
const PageNone Page = ""

const (
	PageOverview   Page = "overview"
	PageNodes      Page = "nodes"
	PageContainers Page = "containers"
	PageProcesses  Page = "processes"
	PageServices   Page = "services"
	PageCron       Page = "cron"
	PagePorts      Page = "ports"
	PageDisks      Page = "disks"
	PageUsers      Page = "users"
)

// FleetOnlyPages have no per-node concept: a grant naming one of these must
// always be fleet-wide, never scoped to a specific node, and — per
// [ValidateBundleMembership] — none of them may be added to a reusable
// RoleAccess bundle at all.
//
// Overview and Nodes join Users here even though both technically show
// per-node data: which nodes appear in either is already controlled
// entirely by the existing node.read grant (see internal/core/user's
// AuthorizedNodes, and internal/api/v1.Handler.ListNodes's filtering) —
// independent of this layer. "Can this user open the Nodes page at all" has
// no per-node answer to give; the per-node narrowing that page's own
// content gets happens one layer down, unaffected by this one.
var FleetOnlyPages = map[Page]bool{
	PageOverview: true,
	PageNodes:    true,
	PageUsers:    true,
}

// KnownPages are every page a grant or bundle may name.
var KnownPages = map[Page]bool{
	PageOverview: true, PageNodes: true, PageContainers: true, PageProcesses: true,
	PageServices: true, PageCron: true, PagePorts: true, PageDisks: true, PageUsers: true,
}

// Audit action labels recorded in user_audit_log.detail — see
// internal/core/user.AuditEntry, whose table this package shares rather
// than duplicating: a RoleAccess/UserAccess grant is, for audit purposes,
// the same kind of fact ("who granted what, to whom, when") the existing
// grant_role/revoke_role actions already record, not a different domain
// the way internal/core/eventstore's monitored-infrastructure events are.
const (
	AuditActionCreateRoleAccess         = "create_role_access"
	AuditActionAddPageToRoleAccess      = "add_page_to_role_access"
	AuditActionRemovePageFromRoleAccess = "remove_page_from_role_access"
	AuditActionAssignRoleAccess         = "assign_role_access"
	AuditActionRevokeRoleAccess         = "revoke_role_access"
	AuditActionGrantPageAccess          = "grant_page_access"
	AuditActionRevokePageAccess         = "revoke_page_access"
)

// Scope is the set of nodes a grant covers: every node (FleetWide) or
// exactly one (NodeID). Mirrors internal/core/user.GrantSpec's node_id/
// fleet_wide shape — NULL-means-fleet-wide at the storage layer, this pair
// at the domain layer — rather than inventing a different convention.
type Scope struct {
	FleetWide bool
	NodeID    string // meaningful only when !FleetWide
}

// Overlaps reports whether s and other cover at least one node in common.
//
// Fleet-wide is the universal set of nodes: it overlaps everything,
// including another fleet-wide scope. Two node-scoped scopes overlap only
// when they name the same node. This is the entire scope-overlap
// conflict-check algorithm; [HasConflict] is just this applied across a
// list.
func (s Scope) Overlaps(other Scope) bool {
	if s.FleetWide || other.FleetWide {
		return true
	}
	return s.NodeID == other.NodeID
}

// HasConflict reports whether requested would duplicate coverage already
// provided by any of roleScopes — the union of every active RoleAccess
// grant covering one page, across all of a user's active role assignments
// (see [Store.RoleAccessScopesForPage], which fetches that union; checking
// each entry independently for overlap is equivalent to checking their
// union and needs no separate union step).
//
// This is a data-hygiene rule, not a security boundary: rejecting a
// redundant UserAccess grant does not change effective access (that stays
// the union of RoleAccess and UserAccess pages either way) — it exists so a
// later revoke of one grant cannot silently leave the other still covering
// the same page for the same node, which is confusing to display and to
// reason about, not unsafe on its own.
func HasConflict(roleScopes []Scope, requested Scope) bool {
	for _, rs := range roleScopes {
		if rs.Overlaps(requested) {
			return true
		}
	}
	return false
}

// ErrPageAccessConflict is returned by [Store.GrantPageAccess] when
// [HasConflict] finds an overlapping RoleAccess grant already covering the
// requested page and scope.
var ErrPageAccessConflict = errs.New(errs.CodeAlreadyExists,
	"a role already grants this page for an overlapping scope; revoke or narrow that role assignment instead of adding a duplicate direct grant")

// ErrPageHasNoNodeConcept is returned when a grant or bundle-membership
// request names a specific node for a page in [FleetOnlyPages].
var ErrPageHasNoNodeConcept = errs.New(errs.CodeInvalidArgument,
	"this page has no per-node concept; grants must be fleet-wide")

// PageGrantSpec describes a direct UserAccess grant (user_page_access row)
// to create.
type PageGrantSpec struct {
	UserID    string
	Page      Page
	NodeID    string
	FleetWide bool
	GrantedBy string
}

// Validate reports whether the spec is usable — the same no-default-between-
// node-and-fleet-wide rule internal/core/user.GrantSpec.Validate enforces,
// plus the fleet-only constraint this package adds.
func (s PageGrantSpec) Validate() error {
	const op = "pageauthz.PageGrantSpec.Validate"
	switch {
	case strings.TrimSpace(s.UserID) == "":
		return errs.New(errs.CodeInvalidArgument, "a grant must name the user it applies to").WithOp(op)
	case !KnownPages[s.Page]:
		return errs.New(errs.CodeInvalidArgument, "unknown page %q", s.Page).WithOp(op)
	case !s.FleetWide && strings.TrimSpace(s.NodeID) == "":
		return errs.New(errs.CodeInvalidArgument,
			"a grant must name a node id, or explicitly request fleet-wide — there is no default between them").WithOp(op)
	case s.FleetWide && s.NodeID != "":
		return errs.New(errs.CodeInvalidArgument,
			"a grant cannot name a node id and request fleet-wide at the same time").WithOp(op)
	case FleetOnlyPages[s.Page] && !s.FleetWide:
		return errs.Wrap(ErrPageHasNoNodeConcept, errs.CodeInvalidArgument, "page %q has no per-node concept; grants must be fleet-wide", s.Page).WithOp(op)
	}
	return nil
}

// ValidateBundleMembership reports whether page may be added to a reusable
// RoleAccess bundle. A fleet-only page never may: a bundle's own assignment
// (user_role_access) carries one scope applied uniformly to every page it
// contains, and a fleet-only page cannot honor a node-scoped assignment —
// there is no way to represent "fleet-wide for this one page, node-5 for
// the rest" in a single grant row. Fleet-only pages are reachable only via
// a direct [PageGrantSpec], never through a bundle.
func ValidateBundleMembership(page Page) error {
	const op = "pageauthz.ValidateBundleMembership"
	if !KnownPages[page] {
		return errs.New(errs.CodeInvalidArgument, "unknown page %q", page).WithOp(op)
	}
	if FleetOnlyPages[page] {
		return errs.Wrap(ErrPageHasNoNodeConcept, errs.CodeInvalidArgument,
			"page %q has no per-node concept and cannot be added to a reusable role bundle; grant it directly to a user instead", page).WithOp(op)
	}
	return nil
}

// RoleAccessAssignmentSpec describes assigning an existing RoleAccess
// bundle to a user (a user_role_access row) to create.
type RoleAccessAssignmentSpec struct {
	UserID         string
	RoleAccessName string
	NodeID         string
	FleetWide      bool
	GrantedBy      string
}

// Validate reports whether the spec is usable.
func (s RoleAccessAssignmentSpec) Validate() error {
	const op = "pageauthz.RoleAccessAssignmentSpec.Validate"
	switch {
	case strings.TrimSpace(s.UserID) == "":
		return errs.New(errs.CodeInvalidArgument, "an assignment must name the user it applies to").WithOp(op)
	case strings.TrimSpace(s.RoleAccessName) == "":
		return errs.New(errs.CodeInvalidArgument, "an assignment must name the role bundle it applies to").WithOp(op)
	case !s.FleetWide && strings.TrimSpace(s.NodeID) == "":
		return errs.New(errs.CodeInvalidArgument,
			"an assignment must name a node id, or explicitly request fleet-wide — there is no default between them").WithOp(op)
	case s.FleetWide && s.NodeID != "":
		return errs.New(errs.CodeInvalidArgument,
			"an assignment cannot name a node id and request fleet-wide at the same time").WithOp(op)
	}
	return nil
}

// RoleAccess is a named, reusable bundle of pages — a role_access row plus
// its role_access_pages membership.
type RoleAccess struct {
	Name      string
	Pages     []Page
	CreatedAt time.Time
	CreatedBy string
}

// RoleAccessAssignment is one user_role_access row — a user holding a
// RoleAccess bundle, scoped to a node or fleet-wide.
type RoleAccessAssignment struct {
	ID             string
	UserID         string
	RoleAccessName string
	// NodeID is nil for a fleet-wide assignment.
	NodeID    *string
	GrantedAt time.Time
	GrantedBy string
	RevokedAt *time.Time
	RevokedBy string
}

// PageGrant is one user_page_access row — a page granted directly to a
// user, independent of any RoleAccess.
type PageGrant struct {
	ID     string
	UserID string
	Page   Page
	// NodeID is nil for a fleet-wide grant.
	NodeID    *string
	GrantedAt time.Time
	GrantedBy string
	RevokedAt *time.Time
	RevokedBy string
}

// Store is the storage surface this package needs. Implemented by
// internal/storage/pageauthz.Repository.
type Store interface {
	// CreateRoleAccess defines a new, initially-empty bundle.
	CreateRoleAccess(ctx context.Context, name, createdBy string, now time.Time) error
	// AddPageToRoleAccess adds one page to a bundle's membership. Must
	// reject via [ValidateBundleMembership] before persisting.
	AddPageToRoleAccess(ctx context.Context, roleAccessName string, page Page) error
	// RemovePageFromRoleAccess removes one page from a bundle's membership.
	RemovePageFromRoleAccess(ctx context.Context, roleAccessName string, page Page) error
	// ListRoleAccessDefinitions returns every bundle and its pages, for
	// operator tooling and the assignment UI's picker.
	ListRoleAccessDefinitions(ctx context.Context) ([]RoleAccess, error)

	// AssignRoleAccess grants a user an existing bundle at a scope. Must be
	// idempotent for an identical, already-active assignment.
	AssignRoleAccess(ctx context.Context, spec RoleAccessAssignmentSpec, now time.Time) error
	// RevokeRoleAccessAssignment withdraws a previously assigned bundle.
	RevokeRoleAccessAssignment(ctx context.Context, assignmentID, revokedBy string, now time.Time) error
	// ListRoleAccessAssignments returns every bundle assignment for a user.
	ListRoleAccessAssignments(ctx context.Context, userID string) ([]RoleAccessAssignment, error)

	// GrantPageAccess creates a direct UserAccess grant. Must run
	// [HasConflict] against [RoleAccessScopesForPage] before persisting,
	// atomically with the insert (locked against a concurrent RoleAccess
	// assignment the same way internal/storage/user's last-admin guard
	// locks against a concurrent revoke), and return
	// [ErrPageAccessConflict] on a hit.
	GrantPageAccess(ctx context.Context, spec PageGrantSpec, now time.Time) error
	// RevokePageAccess withdraws a previously granted direct page access.
	RevokePageAccess(ctx context.Context, grantID, revokedBy string, now time.Time) error
	// ListPageAccessGrants returns every direct grant for a user.
	ListPageAccessGrants(ctx context.Context, userID string) ([]PageGrant, error)

	// RoleAccessScopesForPage returns the scope of every active RoleAccess
	// assignment that covers page, across every bundle the user holds —
	// the union [HasConflict] checks against.
	RoleAccessScopesForPage(ctx context.Context, userID string, page Page) ([]Scope, error)
	// EffectiveAccess reports whether userID may reach page at all, and for
	// a non-fleet-only page, which nodes — the union of every active
	// RoleAccess bundle covering page and every active direct grant for it.
	EffectiveAccess(ctx context.Context, userID string, page Page) (fleetWide bool, nodeIDs []string, err error)

	// AccessibleNodeIDs returns the distinct nodes userID holds any active
	// node-scoped page-access grant or bundle assignment for, across every
	// page. Fleet-wide rows (which name no node) are excluded. Backs
	// [Handler.ListNodes]'s node-visibility union — see [Authorizer.AccessibleNodeIDs].
	AccessibleNodeIDs(ctx context.Context, userID string) ([]string, error)

	// EffectiveAccessByPage reports every page userID can reach and its
	// scope — the union of active bundle coverage and active direct grants,
	// computed for all pages in one shot. Backs GET /auth/me's nav/route
	// hints. A page the user cannot reach at all has no entry.
	EffectiveAccessByPage(ctx context.Context, userID string) (map[Page]PageReach, error)
}

// PageReach is one page's effective access: fleet-wide, or a set of nodes.
// FleetWide true means every node (and NodeIDs is not consulted).
type PageReach struct {
	FleetWide bool
	NodeIDs   []string
}

// ErrPageAccessDenied is returned by [Authorizer.Require] when the
// principal is known but this layer does not grant them page, for the
// requested node.
var ErrPageAccessDenied = errs.New(errs.CodePermissionDenied, "you do not have access to this page")

// Authorizer is the page-visibility policy layer handlers call, the same
// shape as internal/core/user.Authorizer for the operation-level layer —
// deliberately parallel, never merged: see this package's own doc.
type Authorizer struct {
	store Store
}

// NewAuthorizer builds an Authorizer over store.
func NewAuthorizer(store Store) Authorizer { return Authorizer{store: store} }

// Require returns nil when principal may reach page for nodeID (or for a
// fleet-only page, at all — nodeID is ignored), and a typed, client-safe
// error otherwise. Mirrors internal/core/user.Authorizer.Require's
// fail-closed shape: the store's own error surfaces as-is (CodeUnavailable
// on a DB failure, never misread as a false denial); only a clean "no" from
// the store becomes [ErrPageAccessDenied].
func (a Authorizer) Require(ctx context.Context, userID string, page Page, nodeID string) error {
	if page == PageNone {
		return nil
	}
	fleetWide, nodeIDs, err := a.store.EffectiveAccess(ctx, userID, page)
	if err != nil {
		return err
	}
	if fleetWide {
		return nil
	}
	if FleetOnlyPages[page] {
		// A fleet-only page's effective access is always all-or-nothing —
		// fleetWide=false here means no grant at all, not "granted for some
		// other node", since a fleet-only page can never have a node-scoped
		// grant to begin with.
		return ErrPageAccessDenied
	}
	for _, id := range nodeIDs {
		if id == nodeID {
			return nil
		}
	}
	return ErrPageAccessDenied
}

// AccessibleNodeIDs returns every node userID holds a node-scoped
// page-access grant or bundle assignment for — the node-scoped rows only; a
// fleet-wide page grant names no node and contributes nothing.
//
// [Handler.ListNodes] unions this with the operation-level node.read filter
// so a user provisioned only through the page-access axis (e.g. a Containers
// grant for one node, no role grant at all) still resolves that node in the
// node switcher, which has no other source for it. This never widens
// node.read: a fleet-wide page grant does not become a fleet-wide node
// override, and GetNode stays gated per node.
func (a Authorizer) AccessibleNodeIDs(ctx context.Context, userID string) ([]string, error) {
	return a.store.AccessibleNodeIDs(ctx, userID)
}

// EffectiveAccessByPage reports every page userID can reach and its scope,
// for GET /auth/me's nav-hide and route-guard hints. This is a display
// convenience only: every page's own data endpoints enforce the same access
// independently, and the frontend keeps Overview reachable regardless (its
// data comes from already-gated per-page calls) whatever this reports.
func (a Authorizer) EffectiveAccessByPage(ctx context.Context, userID string) (map[Page]PageReach, error) {
	return a.store.EffectiveAccessByPage(ctx, userID)
}
