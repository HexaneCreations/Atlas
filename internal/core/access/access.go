// Package access is the access editor's domain model: one user's editable
// role grants and direct page grants taken as a whole, the rules a requested
// replacement must satisfy, and the minimal set of grants and revokes that
// turns the current state into it.
//
// It is pure — no storage, no HTTP — so every rule is table-testable. The
// transaction that applies a [Changes] lives in internal/storage/user.
//
// What the editor covers: user_node_roles rows other than superadmin, and
// user_page_access rows. Superadmin grants are never read or changed here
// (internal/storage/user excludes them before a [State] is built, and
// [Plan] refuses a request naming the role), and bundle assignments
// (user_role_access) are read-only: they are reported so a client can show
// the pages they cover, and checked so a direct grant never duplicates one.
package access

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Role is one role on one scope.
type Role struct {
	Scope pageauthz.Scope
	Role  string
}

// Page is one direct page grant on one scope.
type Page struct {
	Scope pageauthz.Scope
	Page  pageauthz.Page
}

// Desired is a requested replacement of a user's editable access: every role
// and direct page grant the user should hold afterwards. Anything editable
// the user holds that is not listed is revoked.
type Desired struct {
	Roles []Role
	Pages []Page
}

// RoleGrant is an active role grant (a user_node_roles row).
type RoleGrant struct {
	ID string
	Role
	GrantedAt time.Time
	GrantedBy string
}

// PageGrant is an active direct page grant (a user_page_access row).
type PageGrant struct {
	ID string
	Page
	GrantedAt time.Time
	GrantedBy string
}

// Bundle is an active bundle assignment (a user_role_access row) with the
// pages its bundle currently contains. Read-only to the editor.
type Bundle struct {
	ID        string
	Name      string
	Scope     pageauthz.Scope
	Pages     []pageauthz.Page
	GrantedAt time.Time
	GrantedBy string
}

// State is a user's current access as the editor sees it.
type State struct {
	Roles   []RoleGrant
	Pages   []PageGrant
	Bundles []Bundle
}

// Version identifies this exact set of active rows. Any grant or revoke
// through any path (this editor, the per-grant endpoints, the CLI) changes
// it, so a client that read one version cannot overwrite a state it never
// saw — see [ErrStaleVersion].
func (s State) Version() string {
	ids := make([]string, 0, len(s.Roles)+len(s.Pages)+len(s.Bundles))
	for _, g := range s.Roles {
		ids = append(ids, "r:"+g.ID)
	}
	for _, g := range s.Pages {
		ids = append(ids, "p:"+g.ID)
	}
	for _, b := range s.Bundles {
		ids = append(ids, "b:"+b.ID)
	}
	slices.Sort(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return hex.EncodeToString(sum[:16])
}

// Changes is what applying a [Desired] to a [State] does.
type Changes struct {
	GrantRoles  []Role
	RevokeRoles []RoleGrant
	GrantPages  []Page
	RevokePages []PageGrant
}

// Empty reports whether the request changes nothing.
func (c Changes) Empty() bool {
	return len(c.GrantRoles)+len(c.RevokeRoles)+len(c.GrantPages)+len(c.RevokePages) == 0
}

// RevokesFleetWideAdmin reports whether the change removes the user's
// fleet-wide admin grant, which the caller must check against
// [user.ErrLastAdminGrant]. A desired fleet-wide admin matches the current
// row by key and is never revoked, so any revoke of one is a removal.
func (c Changes) RevokesFleetWideAdmin() bool {
	for _, g := range c.RevokeRoles {
		if g.Role.Role == user.RoleAdmin && g.Scope.FleetWide {
			return true
		}
	}
	return false
}

// GrantsAdmin reports whether d asks for the admin role on any scope.
func (d Desired) GrantsAdmin() bool {
	for _, r := range d.Roles {
		if r.Role == user.RoleAdmin {
			return true
		}
	}
	return false
}

// ChangesAdmin reports whether applying desired to current would change
// where the user holds the admin role: gaining or losing it fleet-wide, or,
// without a fleet-wide admin role on either side, on any node. Revoking an
// admin row that another admin row still covers (a node-scoped admin under
// a fleet-wide one) is not a change.
func ChangesAdmin(current State, desired Desired) bool {
	before := adminCoverage{nodes: map[string]bool{}}
	for _, g := range current.Roles {
		before.add(g.Role)
	}
	after := adminCoverage{nodes: map[string]bool{}}
	roles, _ := normalize(desired)
	for _, r := range roles {
		after.add(r)
	}
	if before.fleet || after.fleet {
		return before.fleet != after.fleet
	}
	return !maps.Equal(before.nodes, after.nodes)
}

type adminCoverage struct {
	fleet bool
	nodes map[string]bool
}

func (c *adminCoverage) add(r Role) {
	if r.Role != user.RoleAdmin {
		return
	}
	if r.Scope.FleetWide {
		c.fleet = true
		return
	}
	c.nodes[r.Scope.NodeID] = true
}

// ErrOwnAccess refuses an edit of the caller's own access, for every caller
// including the superadmin: an admin who removes their own admin role mid-edit
// loses the page they are editing on, and nobody can grant themselves more.
var ErrOwnAccess = errs.New(errs.CodePermissionDenied, "you cannot change your own access")

// ErrStaleVersion is returned when the version a client read no longer
// matches the user's current access.
var ErrStaleVersion = errs.New(errs.CodeAlreadyExists, "access changed since you opened it; reload")

// roleRank orders the editable roles by strict permission nesting
// (viewer ⊂ operator ⊂ admin, per role_permissions), so a fleet-wide role
// makes any node-scoped role at or below it redundant.
var roleRank = map[string]int{user.RoleViewer: 1, user.RoleOperator: 2, user.RoleAdmin: 3}

// Plan validates desired and returns the changes that turn current into it.
//
// knownNodes holds every node id that exists; a grant may only be added on
// one of them. Rows already held on a node that has since gone stay visible
// and can be revoked. Requests that are malformed or inconsistent are
// refused with 400 rather than partially applied; a direct page grant that
// would duplicate a bundle's coverage is refused with
// [pageauthz.ErrPageAccessConflict] (409).
func Plan(current State, desired Desired, knownNodes map[string]bool) (Changes, error) {
	const op = "access.Plan"
	if err := validate(desired); err != nil {
		return Changes{}, err
	}
	roles, pages := normalize(desired)
	if err := checkPagesHaveRoles(roles, pages); err != nil {
		return Changes{}, err
	}

	var c Changes
	wantRoles := make(map[string]bool, len(roles))
	for _, r := range roles {
		wantRoles[roleKey(r)] = true
	}
	haveRoles := make(map[string]bool, len(current.Roles))
	for _, g := range current.Roles {
		haveRoles[roleKey(g.Role)] = true
		if !wantRoles[roleKey(g.Role)] {
			c.RevokeRoles = append(c.RevokeRoles, g)
		}
	}
	for _, r := range roles {
		if !haveRoles[roleKey(r)] {
			c.GrantRoles = append(c.GrantRoles, r)
		}
	}

	wantPages := make(map[string]bool, len(pages))
	for _, p := range pages {
		wantPages[pageKey(p)] = true
	}
	havePages := make(map[string]bool, len(current.Pages))
	for _, g := range current.Pages {
		havePages[pageKey(g.Page)] = true
		if !wantPages[pageKey(g.Page)] {
			c.RevokePages = append(c.RevokePages, g)
		}
	}
	for _, p := range pages {
		if !havePages[pageKey(p)] {
			c.GrantPages = append(c.GrantPages, p)
		}
	}

	var unknown []string
	for _, r := range c.GrantRoles {
		if !r.Scope.FleetWide && !knownNodes[r.Scope.NodeID] {
			unknown = append(unknown, r.Scope.NodeID)
		}
	}
	for _, p := range c.GrantPages {
		if !p.Scope.FleetWide && !knownNodes[p.Scope.NodeID] {
			unknown = append(unknown, p.Scope.NodeID)
		}
	}
	if len(unknown) > 0 {
		slices.Sort(unknown)
		return Changes{}, errs.New(errs.CodeInvalidArgument, "unknown node %s", strings.Join(slices.Compact(unknown), ", ")).WithOp(op)
	}

	for _, p := range c.GrantPages {
		var bundleScopes []pageauthz.Scope
		for _, b := range current.Bundles {
			if slices.Contains(b.Pages, p.Page) {
				bundleScopes = append(bundleScopes, b.Scope)
			}
		}
		if pageauthz.HasConflict(bundleScopes, p.Scope) {
			return Changes{}, errs.Wrap(pageauthz.ErrPageAccessConflict, errs.CodeAlreadyExists,
				"page %q on %s is already granted by a role bundle; it cannot also be granted directly", p.Page, scopeLabel(p.Scope)).WithOp(op)
		}
	}

	sortChanges(&c)
	return c, nil
}

func validate(d Desired) error {
	const op = "access.validate"
	seenRole := map[string]bool{}
	for _, r := range d.Roles {
		if err := validateScope(r.Scope); err != nil {
			return err
		}
		switch {
		case r.Role == user.RoleSuperadmin:
			return user.ErrSuperadminNotGrantable
		case roleRank[r.Role] == 0:
			return errs.New(errs.CodeInvalidArgument, "unknown role %q", r.Role).WithOp(op)
		}
		k := scopeKey(r.Scope)
		if seenRole[k] {
			return errs.New(errs.CodeInvalidArgument, "more than one role on %s; send one role per scope", scopeLabel(r.Scope)).WithOp(op)
		}
		seenRole[k] = true
	}
	seenPage := map[string]bool{}
	for _, p := range d.Pages {
		if err := validateScope(p.Scope); err != nil {
			return err
		}
		switch {
		case !pageauthz.KnownPages[p.Page]:
			return errs.New(errs.CodeInvalidArgument, "unknown page %q", p.Page).WithOp(op)
		case pageauthz.FleetOnlyPages[p.Page] && !p.Scope.FleetWide:
			return errs.Wrap(pageauthz.ErrPageHasNoNodeConcept, errs.CodeInvalidArgument,
				"page %q has no per-node concept; grants must be fleet-wide", p.Page).WithOp(op)
		}
		k := pageKey(p)
		if seenPage[k] {
			return errs.New(errs.CodeInvalidArgument, "page %q on %s is listed twice", p.Page, scopeLabel(p.Scope)).WithOp(op)
		}
		seenPage[k] = true
	}
	return nil
}

func validateScope(s pageauthz.Scope) error {
	const op = "access.validateScope"
	switch {
	case !s.FleetWide && strings.TrimSpace(s.NodeID) == "":
		return errs.New(errs.CodeInvalidArgument,
			"every row must name a node id, or explicitly request fleet-wide — there is no default between them").WithOp(op)
	case s.FleetWide && s.NodeID != "":
		return errs.New(errs.CodeInvalidArgument, "a row cannot name a node id and request fleet-wide at the same time").WithOp(op)
	}
	return nil
}

// normalize drops rows another row in the same request already covers: a
// node-scoped role at or below the fleet-wide role, and a node-scoped page
// the same page's fleet-wide grant covers. validate has already ensured at
// most one role per scope.
func normalize(d Desired) ([]Role, []Page) {
	fleetRank := 0
	for _, r := range d.Roles {
		if r.Scope.FleetWide {
			fleetRank = roleRank[r.Role]
		}
	}
	roles := make([]Role, 0, len(d.Roles))
	for _, r := range d.Roles {
		if !r.Scope.FleetWide && roleRank[r.Role] <= fleetRank {
			continue
		}
		roles = append(roles, r)
	}

	fleetPages := map[pageauthz.Page]bool{}
	for _, p := range d.Pages {
		if p.Scope.FleetWide {
			fleetPages[p.Page] = true
		}
	}
	pages := make([]Page, 0, len(d.Pages))
	for _, p := range d.Pages {
		if !p.Scope.FleetWide && fleetPages[p.Page] {
			continue
		}
		pages = append(pages, p)
	}
	return roles, pages
}

// checkPagesHaveRoles refuses a direct page grant the user could never use: a
// page on a node needs a role on that node or a fleet-wide role, and a
// fleet-wide page other than Users needs at least one role somewhere. Users
// carries no role requirement here; its user.manage check is enforced
// separately on every request.
func checkPagesHaveRoles(roles []Role, pages []Page) error {
	const op = "access.checkPagesHaveRoles"
	fleetRole := false
	onNode := map[string]bool{}
	for _, r := range roles {
		if r.Scope.FleetWide {
			fleetRole = true
		} else {
			onNode[r.Scope.NodeID] = true
		}
	}
	anyRole := len(roles) > 0

	var bad []string
	for _, p := range pages {
		switch {
		case p.Page == pageauthz.PageUsers:
		case p.Scope.FleetWide && !anyRole:
			bad = append(bad, string(p.Page)+" (fleet-wide)")
		case !p.Scope.FleetWide && !fleetRole && !onNode[p.Scope.NodeID]:
			bad = append(bad, string(p.Page)+" on node "+p.Scope.NodeID)
		}
	}
	if len(bad) > 0 {
		return errs.New(errs.CodeInvalidArgument,
			"these pages need a role on the same node or a fleet-wide role: %s", strings.Join(bad, ", ")).WithOp(op)
	}
	return nil
}

func scopeKey(s pageauthz.Scope) string {
	if s.FleetWide {
		return "*"
	}
	return "n:" + s.NodeID
}

func roleKey(r Role) string { return scopeKey(r.Scope) + "|" + r.Role }
func pageKey(p Page) string { return scopeKey(p.Scope) + "|" + string(p.Page) }

func scopeLabel(s pageauthz.Scope) string {
	if s.FleetWide {
		return "fleet-wide"
	}
	return "node " + s.NodeID
}

func sortChanges(c *Changes) {
	slices.SortFunc(c.GrantRoles, func(a, b Role) int { return strings.Compare(roleKey(a), roleKey(b)) })
	slices.SortFunc(c.RevokeRoles, func(a, b RoleGrant) int { return strings.Compare(roleKey(a.Role), roleKey(b.Role)) })
	slices.SortFunc(c.GrantPages, func(a, b Page) int { return strings.Compare(pageKey(a), pageKey(b)) })
	slices.SortFunc(c.RevokePages, func(a, b PageGrant) int { return strings.Compare(pageKey(a.Page), pageKey(b.Page)) })
}
