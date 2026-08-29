package pageauthz_test

import (
	"context"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/platform/errs"
)

func fleetWide() pageauthz.Scope     { return pageauthz.Scope{FleetWide: true} }
func node(id string) pageauthz.Scope { return pageauthz.Scope{NodeID: id} }

// --- Scope.Overlaps ----------------------------------------------------

func TestScopeOverlapsFleetWideOverlapsEverything(t *testing.T) {
	t.Parallel()
	if !fleetWide().Overlaps(node("node-1")) {
		t.Error("fleet-wide did not overlap a specific node")
	}
	if !node("node-1").Overlaps(fleetWide()) {
		t.Error("a specific node did not overlap fleet-wide (order should not matter)")
	}
	if !fleetWide().Overlaps(fleetWide()) {
		t.Error("fleet-wide did not overlap fleet-wide")
	}
}

func TestScopeOverlapsSameNodeOverlaps(t *testing.T) {
	t.Parallel()
	if !node("node-1").Overlaps(node("node-1")) {
		t.Error("identical node scopes did not overlap")
	}
}

func TestScopeOverlapsDifferentNodesDoNotOverlap(t *testing.T) {
	t.Parallel()
	if node("node-1").Overlaps(node("node-2")) {
		t.Error("different node scopes overlapped")
	}
}

// --- HasConflict — the 6 scenarios + the unstated 5th case -------------
//
// Scenario numbering matches the audit report: 1-4 are the ones the design
// review named explicitly (including "the reverse"), 5 is the
// Role-node-N/requested-fleet-wide case confirmed in follow-up, 6 proves
// the union-across-roles requirement doesn't false-positive.

// 1. Role fleet-wide, page P -> UserAccess any node for P -> conflict.
func TestHasConflictRoleFleetWideBlocksAnyNodeRequest(t *testing.T) {
	t.Parallel()
	roleScopes := []pageauthz.Scope{fleetWide()}
	if !pageauthz.HasConflict(roleScopes, node("node-7")) {
		t.Error("fleet-wide role grant did not conflict with a specific-node request")
	}
	if !pageauthz.HasConflict(roleScopes, fleetWide()) {
		t.Error("fleet-wide role grant did not conflict with a fleet-wide request")
	}
}

// 2. Role node-N, page P -> UserAccess node-M (M != N) for P -> no conflict.
func TestHasConflictRoleSpecificNodeAllowsADifferentNode(t *testing.T) {
	t.Parallel()
	roleScopes := []pageauthz.Scope{node("node-1")}
	if pageauthz.HasConflict(roleScopes, node("node-2")) {
		t.Error("a role grant for node-1 conflicted with a request for the unrelated node-2")
	}
}

// 3. No role grants at all -> no conflict regardless of scope.
func TestHasConflictNoRoleGrantsNeverConflicts(t *testing.T) {
	t.Parallel()
	var roleScopes []pageauthz.Scope
	if pageauthz.HasConflict(roleScopes, node("node-1")) {
		t.Error("an empty role-scope set reported a conflict")
	}
	if pageauthz.HasConflict(roleScopes, fleetWide()) {
		t.Error("an empty role-scope set reported a conflict for a fleet-wide request")
	}
}

// 4. The reverse: Role node-N, page P -> UserAccess node-N (same node) -> conflict.
func TestHasConflictRoleSpecificNodeBlocksTheExactSameNode(t *testing.T) {
	t.Parallel()
	roleScopes := []pageauthz.Scope{node("node-1")}
	if !pageauthz.HasConflict(roleScopes, node("node-1")) {
		t.Error("a role grant for node-1 did not conflict with a request for that same node")
	}
}

// 5. Role node-N, page P -> UserAccess fleet-wide for P -> conflict (confirmed
// in the design follow-up: a fleet-wide grant would still partially
// duplicate the role's node-N coverage, and a later revoke of the role
// would silently leave the fleet-wide grant covering node-N anyway).
func TestHasConflictRoleSpecificNodeBlocksAFleetWideRequest(t *testing.T) {
	t.Parallel()
	roleScopes := []pageauthz.Scope{node("node-1")}
	if !pageauthz.HasConflict(roleScopes, fleetWide()) {
		t.Error("a role grant for node-1 did not conflict with a fleet-wide request")
	}
}

// 6. Two roles, neither alone covering node-M -> UserAccess node-M -> no
// conflict. Proves checking each role scope independently (no separate
// union step) does not false-positive across roles that don't individually
// overlap the request.
func TestHasConflictUnionsAcrossMultipleRolesWithoutFalsePositives(t *testing.T) {
	t.Parallel()
	roleScopes := []pageauthz.Scope{node("node-1"), node("node-2")}
	if pageauthz.HasConflict(roleScopes, node("node-3")) {
		t.Error("roles covering node-1 and node-2 conflicted with an unrelated request for node-3")
	}
	// And the union DOES catch a real duplicate against the second role.
	if !pageauthz.HasConflict(roleScopes, node("node-2")) {
		t.Error("a duplicate of the second role's grant (node-2) was not caught")
	}
}

// --- PageGrantSpec.Validate ----------------------------------------------

func TestPageGrantSpecValidateRejectsNeitherNodeNorFleetWide(t *testing.T) {
	t.Parallel()
	spec := pageauthz.PageGrantSpec{UserID: "u1", Page: pageauthz.PageContainers}
	if err := spec.Validate(); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("code = %v, want invalid_argument", errs.CodeOf(err))
	}
}

func TestPageGrantSpecValidateRejectsBothNodeAndFleetWide(t *testing.T) {
	t.Parallel()
	spec := pageauthz.PageGrantSpec{UserID: "u1", Page: pageauthz.PageContainers, NodeID: "node-1", FleetWide: true}
	if err := spec.Validate(); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("code = %v, want invalid_argument", errs.CodeOf(err))
	}
}

func TestPageGrantSpecValidateRejectsUnknownPage(t *testing.T) {
	t.Parallel()
	spec := pageauthz.PageGrantSpec{UserID: "u1", Page: "not-a-real-page", FleetWide: true}
	if err := spec.Validate(); errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("code = %v, want invalid_argument", errs.CodeOf(err))
	}
}

// The gap explicitly called out for closing: a fleet-only page must reject
// a node-specific grant outright, not silently coerce it to fleet-wide.
func TestPageGrantSpecValidateRejectsANodeSpecificGrantForAFleetOnlyPage(t *testing.T) {
	t.Parallel()
	for page := range pageauthz.FleetOnlyPages {
		spec := pageauthz.PageGrantSpec{UserID: "u1", Page: page, NodeID: "node-1"}
		err := spec.Validate()
		if err == nil {
			t.Errorf("page %q: Validate accepted a node-specific grant for a fleet-only page", page)
			continue
		}
		if !errs.Is(err, pageauthz.ErrPageHasNoNodeConcept) {
			t.Errorf("page %q: error = %v, want ErrPageHasNoNodeConcept", page, err)
		}
	}
}

func TestPageGrantSpecValidateAcceptsAFleetWideGrantForAFleetOnlyPage(t *testing.T) {
	t.Parallel()
	spec := pageauthz.PageGrantSpec{UserID: "u1", Page: pageauthz.PageUsers, FleetWide: true}
	if err := spec.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}

func TestPageGrantSpecValidateAcceptsANodeSpecificGrantForANodeScopedPage(t *testing.T) {
	t.Parallel()
	spec := pageauthz.PageGrantSpec{UserID: "u1", Page: pageauthz.PageContainers, NodeID: "node-1"}
	if err := spec.Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}

// --- ValidateBundleMembership --------------------------------------------

func TestValidateBundleMembershipRejectsEveryFleetOnlyPage(t *testing.T) {
	t.Parallel()
	for page := range pageauthz.FleetOnlyPages {
		err := pageauthz.ValidateBundleMembership(page)
		if err == nil {
			t.Errorf("page %q: ValidateBundleMembership accepted a fleet-only page into a bundle", page)
			continue
		}
		if !errs.Is(err, pageauthz.ErrPageHasNoNodeConcept) {
			t.Errorf("page %q: error = %v, want ErrPageHasNoNodeConcept", page, err)
		}
	}
}

func TestValidateBundleMembershipAcceptsEveryNodeScopedPage(t *testing.T) {
	t.Parallel()
	for page := range pageauthz.KnownPages {
		if pageauthz.FleetOnlyPages[page] {
			continue
		}
		if err := pageauthz.ValidateBundleMembership(page); err != nil {
			t.Errorf("page %q: ValidateBundleMembership = %v, want nil", page, err)
		}
	}
}

// --- Authorizer.Require ---------------------------------------------------

// effectiveAccessStore only needs to answer EffectiveAccess meaningfully —
// that's the only method Authorizer.Require calls; the rest of
// pageauthz.Store is exercised against real Postgres in
// internal/storage/pageauthz's integration tests, the same split
// internal/core/user/user_test.go's stubAuthzStore uses for Authorizer.
type effectiveAccessStore struct {
	fleetWide bool
	nodeIDs   []string
	err       error
}

func (s effectiveAccessStore) EffectiveAccess(context.Context, string, pageauthz.Page) (bool, []string, error) {
	return s.fleetWide, s.nodeIDs, s.err
}
func (s effectiveAccessStore) CreateRoleAccess(context.Context, string, string, time.Time) error {
	return nil
}
func (s effectiveAccessStore) AddPageToRoleAccess(context.Context, string, pageauthz.Page) error {
	return nil
}
func (s effectiveAccessStore) RemovePageFromRoleAccess(context.Context, string, pageauthz.Page) error {
	return nil
}
func (s effectiveAccessStore) ListRoleAccessDefinitions(context.Context) ([]pageauthz.RoleAccess, error) {
	return nil, nil
}
func (s effectiveAccessStore) AssignRoleAccess(context.Context, pageauthz.RoleAccessAssignmentSpec, time.Time) error {
	return nil
}
func (s effectiveAccessStore) RevokeRoleAccessAssignment(context.Context, string, string, time.Time) error {
	return nil
}
func (s effectiveAccessStore) ListRoleAccessAssignments(context.Context, string) ([]pageauthz.RoleAccessAssignment, error) {
	return nil, nil
}
func (s effectiveAccessStore) GrantPageAccess(context.Context, pageauthz.PageGrantSpec, time.Time) error {
	return nil
}
func (s effectiveAccessStore) RevokePageAccess(context.Context, string, string, time.Time) error {
	return nil
}
func (s effectiveAccessStore) ListPageAccessGrants(context.Context, string) ([]pageauthz.PageGrant, error) {
	return nil, nil
}
func (s effectiveAccessStore) RoleAccessScopesForPage(context.Context, string, pageauthz.Page) ([]pageauthz.Scope, error) {
	return nil, nil
}
func (s effectiveAccessStore) AccessibleNodeIDs(context.Context, string) ([]string, error) {
	return nil, nil
}
func (s effectiveAccessStore) EffectiveAccessByPage(context.Context, string) (map[pageauthz.Page]pageauthz.PageReach, error) {
	return nil, nil
}

var _ pageauthz.Store = effectiveAccessStore{}

func TestAuthorizerRequireAllowsPageNoneUnconditionally(t *testing.T) {
	t.Parallel()
	az := pageauthz.NewAuthorizer(effectiveAccessStore{err: errs.New(errs.CodeUnavailable, "should never be reached")})
	if err := az.Require(context.Background(), "u1", pageauthz.PageNone, "node-1"); err != nil {
		t.Errorf("Require(PageNone) = %v, want nil without ever consulting the store", err)
	}
}

func TestAuthorizerRequireAllowsWhenFleetWide(t *testing.T) {
	t.Parallel()
	az := pageauthz.NewAuthorizer(effectiveAccessStore{fleetWide: true})
	if err := az.Require(context.Background(), "u1", pageauthz.PageContainers, "any-node"); err != nil {
		t.Errorf("Require = %v, want nil", err)
	}
}

func TestAuthorizerRequireAllowsWhenTheSpecificNodeIsCovered(t *testing.T) {
	t.Parallel()
	az := pageauthz.NewAuthorizer(effectiveAccessStore{nodeIDs: []string{"node-1", "node-2"}})
	if err := az.Require(context.Background(), "u1", pageauthz.PageContainers, "node-2"); err != nil {
		t.Errorf("Require = %v, want nil", err)
	}
}

func TestAuthorizerRequireDeniesWhenTheNodeIsNotCovered(t *testing.T) {
	t.Parallel()
	az := pageauthz.NewAuthorizer(effectiveAccessStore{nodeIDs: []string{"node-1"}})
	err := az.Require(context.Background(), "u1", pageauthz.PageContainers, "node-9")
	if !errs.Is(err, pageauthz.ErrPageAccessDenied) {
		t.Errorf("Require = %v, want ErrPageAccessDenied", err)
	}
}

func TestAuthorizerRequireDeniesFleetOnlyPageWithNoGrant(t *testing.T) {
	t.Parallel()
	az := pageauthz.NewAuthorizer(effectiveAccessStore{})
	err := az.Require(context.Background(), "u1", pageauthz.PageUsers, "")
	if !errs.Is(err, pageauthz.ErrPageAccessDenied) {
		t.Errorf("Require = %v, want ErrPageAccessDenied", err)
	}
}

// Fail-closed, traced explicitly the same way
// internal/core/user/user_test.go proves it for the operation-level
// Authorizer: a store failure must never be misread as "allowed".
func TestAuthorizerRequirePropagatesStoreFailureRatherThanTreatingItAsDenial(t *testing.T) {
	t.Parallel()
	storeErr := errs.New(errs.CodeUnavailable, "database is down")
	az := pageauthz.NewAuthorizer(effectiveAccessStore{fleetWide: true, err: storeErr})
	err := az.Require(context.Background(), "u1", pageauthz.PageContainers, "node-1")
	if err == nil {
		t.Fatal("Require returned nil for a failing store — this is the fall-through-to-allowed case that must never happen")
	}
	if errs.CodeOf(err) != errs.CodeUnavailable {
		t.Errorf("code = %v, want unavailable — a database failure must not present as a false page-access denial", errs.CodeOf(err))
	}
}
