package user_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

func TestHashPasswordThenVerifyPasswordRoundTrips(t *testing.T) {
	t.Parallel()

	hash, err := user.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if hash == "correct horse battery staple" {
		t.Fatal("HashPassword returned the plaintext unchanged")
	}
	if !user.VerifyPassword(hash, "correct horse battery staple") {
		t.Error("VerifyPassword false for the correct password")
	}
}

func TestVerifyPasswordRejectsWrongPassword(t *testing.T) {
	t.Parallel()

	hash, err := user.HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if user.VerifyPassword(hash, "wrong password") {
		t.Error("VerifyPassword true for the wrong password")
	}
}

func TestCreateSpecValidateRejectsBlankUsername(t *testing.T) {
	t.Parallel()

	if err := (user.CreateSpec{Username: "  ", Password: "a-long-enough-password"}).Validate(); err == nil {
		t.Error("Validate succeeded with a blank username")
	}
}

func TestCreateSpecValidateRejectsShortPassword(t *testing.T) {
	t.Parallel()

	if err := (user.CreateSpec{Username: "alice", Password: "short"}).Validate(); err == nil {
		t.Error("Validate succeeded with a too-short password")
	}
}

func TestCreateSpecValidateAcceptsAWellFormedSpec(t *testing.T) {
	t.Parallel()

	if err := (user.CreateSpec{Username: "alice", Password: "a-long-enough-password"}).Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}

func TestNewSessionProducesDeterministicHashOfItsOwnPlaintext(t *testing.T) {
	t.Parallel()

	s, err := user.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if s.Plaintext == "" {
		t.Fatal("empty plaintext")
	}
	if s.Hash != user.HashSessionToken(s.Plaintext) {
		t.Error("Hash does not match HashSessionToken(Plaintext) — Resolve would never find this session again")
	}
}

func TestNewSessionIsUnique(t *testing.T) {
	t.Parallel()

	a, err := user.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	b, err := user.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if a.Plaintext == b.Plaintext {
		t.Error("two calls to NewSession produced the same token")
	}
}

func TestSessionLive(t *testing.T) {
	t.Parallel()

	now, err := time.Parse(time.RFC3339, "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	tests := []struct {
		name string
		s    user.Session
		want bool
	}{
		{"not expired, not revoked", user.Session{ExpiresAt: now.Add(time.Second)}, true},
		{"expired", user.Session{ExpiresAt: now.Add(-time.Second)}, false},
		{"revoked", user.Session{ExpiresAt: now.Add(time.Second), RevokedAt: &now}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.Live(now); got != tt.want {
				t.Errorf("Live() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGrantSpecValidateRequiresNodeOrFleetWide(t *testing.T) {
	t.Parallel()

	err := (user.GrantSpec{UserID: "u1", Role: user.RoleViewer}).Validate()
	if err == nil {
		t.Fatal("Validate succeeded with neither NodeID nor FleetWide set")
	}
	if errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("code = %v, want invalid_argument", errs.CodeOf(err))
	}
}

func TestGrantSpecValidateRejectsNodeAndFleetWideTogether(t *testing.T) {
	t.Parallel()

	err := (user.GrantSpec{UserID: "u1", NodeID: "node-1", FleetWide: true, Role: user.RoleViewer}).Validate()
	if err == nil {
		t.Fatal("Validate succeeded with both NodeID and FleetWide set")
	}
}

func TestGrantSpecValidateAcceptsFleetWide(t *testing.T) {
	t.Parallel()

	if err := (user.GrantSpec{UserID: "u1", FleetWide: true, Role: user.RoleOperator}).Validate(); err != nil {
		t.Errorf("Validate = %v, want nil", err)
	}
}

func TestGrantSpecValidateRejectsUnknownRole(t *testing.T) {
	t.Parallel()

	err := (user.GrantSpec{UserID: "u1", NodeID: "node-1", Role: "superadmin"}).Validate()
	if err == nil {
		t.Fatal("Validate succeeded with an unknown role")
	}
	if !strings.Contains(err.Error(), "superadmin") {
		t.Errorf("error does not name the offending role: %v", err)
	}
}

// stubAuthzStore is an in-memory [user.AuthzStore] for testing
// [user.Authorizer] without a database — the policy logic in Require is what
// this exercises, not the SQL that backs it (see internal/storage/user's
// integration tests for that).
type stubAuthzStore struct {
	granted bool
	err     error

	// For GrantedNodeIDs — independent of granted/err above, which only
	// drive HasPermission.
	fleetWide      bool
	grantedNodeIDs []string
	grantedNodeErr error
}

func (s stubAuthzStore) HasPermission(context.Context, string, string, user.Permission) (bool, error) {
	return s.granted, s.err
}
func (s stubAuthzStore) Grant(context.Context, user.GrantSpec, time.Time) error { return nil }
func (s stubAuthzStore) RevokeGrant(context.Context, string, string, time.Time) error {
	return nil
}
func (s stubAuthzStore) ListGrants(context.Context, string) ([]user.NodeRole, error) { return nil, nil }
func (s stubAuthzStore) GrantedNodeIDs(context.Context, string, user.Permission) (bool, []string, error) {
	return s.fleetWide, s.grantedNodeIDs, s.grantedNodeErr
}

var _ user.AuthzStore = stubAuthzStore{}

func TestAuthorizerRequireDeniesWhenStoreReportsNoGrant(t *testing.T) {
	t.Parallel()

	az := user.NewAuthorizer(stubAuthzStore{granted: false})
	err := az.Require(context.Background(), user.Principal{UserID: "u1"}, user.PermissionNodeRead, "node-1")
	if !errs.HasCode(err, errs.CodePermissionDenied) {
		t.Errorf("code = %v, want permission_denied", errs.CodeOf(err))
	}
}

func TestAuthorizerRequireAllowsWhenStoreReportsAGrant(t *testing.T) {
	t.Parallel()

	az := user.NewAuthorizer(stubAuthzStore{granted: true})
	if err := az.Require(context.Background(), user.Principal{UserID: "u1"}, user.PermissionNodeRead, "node-1"); err != nil {
		t.Errorf("Require = %v, want nil", err)
	}
}

// Fail-closed, traced explicitly: HasPermission returning any error — DB
// down, timeout, pool exhausted, anything — must make Require return a
// non-nil error every time, never nil (which the caller would read as
// "authorized"). This is the entire correctness property Require has: its
// body is `if err != nil { return err }` before the `!ok` check even runs,
// so a failing store can never be misread as an empty-but-successful
// permission set.
func TestAuthorizerRequirePropagatesStoreFailureRatherThanTreatingItAsDenial(t *testing.T) {
	t.Parallel()

	storeErr := errs.New(errs.CodeUnavailable, "database is down")
	// granted: true deliberately — proves the error check runs before, and
	// independent of, whatever HasPermission's bool result was. A buggy
	// store that returns (true, err) must still be denied.
	az := user.NewAuthorizer(stubAuthzStore{granted: true, err: storeErr})
	err := az.Require(context.Background(), user.Principal{UserID: "u1"}, user.PermissionNodeRead, "node-1")
	if err == nil {
		t.Fatal("Require returned nil for a failing permission store — this is the fall-through-to-allowed case that must never happen")
	}
	if errs.Is(err, storeErr) == false && errs.CodeOf(err) != errs.CodeUnavailable {
		t.Errorf("code = %v, want unavailable — a database failure must not present as a false permission denial", errs.CodeOf(err))
	}
}

// --- AuthorizedNodes ---------------------------------------------------

func TestAuthorizedNodesReturnsFleetWideWithNoNodeIDsWhenTheStoreReportsFleetWide(t *testing.T) {
	t.Parallel()

	az := user.NewAuthorizer(stubAuthzStore{fleetWide: true, grantedNodeIDs: []string{"should-be-ignored"}})
	fleetWide, nodeIDs, err := az.AuthorizedNodes(context.Background(), user.Principal{UserID: "u1"}, user.PermissionNodeRead)
	if err != nil {
		t.Fatalf("AuthorizedNodes: %v", err)
	}
	if !fleetWide {
		t.Error("fleetWide = false, want true")
	}
	if nodeIDs != nil {
		t.Errorf("nodeIDs = %v, want nil — a fleet-wide grant has nothing left to enumerate", nodeIDs)
	}
}

func TestAuthorizedNodesReturnsExactlyTheGrantedSetForANodeScopedPrincipal(t *testing.T) {
	t.Parallel()

	az := user.NewAuthorizer(stubAuthzStore{grantedNodeIDs: []string{"node-1", "node-2"}})
	fleetWide, nodeIDs, err := az.AuthorizedNodes(context.Background(), user.Principal{UserID: "u1"}, user.PermissionNodeRead)
	if err != nil {
		t.Fatalf("AuthorizedNodes: %v", err)
	}
	if fleetWide {
		t.Error("fleetWide = true, want false")
	}
	if !nodeIDs["node-1"] || !nodeIDs["node-2"] || len(nodeIDs) != 2 {
		t.Errorf("nodeIDs = %v, want exactly {node-1, node-2}", nodeIDs)
	}
	if nodeIDs["node-3"] {
		t.Error("nodeIDs contains a node never granted")
	}
}

// The central case this exists for: a principal with no grant at all sees
// an empty set, not an error and not every node.
func TestAuthorizedNodesReturnsAnEmptySetForAPrincipalWithNoGrant(t *testing.T) {
	t.Parallel()

	az := user.NewAuthorizer(stubAuthzStore{grantedNodeIDs: nil})
	fleetWide, nodeIDs, err := az.AuthorizedNodes(context.Background(), user.Principal{UserID: "u1"}, user.PermissionNodeRead)
	if err != nil {
		t.Fatalf("AuthorizedNodes: %v", err)
	}
	if fleetWide {
		t.Error("fleetWide = true for a principal with no grant")
	}
	if len(nodeIDs) != 0 {
		t.Errorf("nodeIDs = %v, want empty", nodeIDs)
	}
}

func TestAuthorizedNodesPropagatesStoreFailure(t *testing.T) {
	t.Parallel()

	storeErr := errs.New(errs.CodeUnavailable, "database is down")
	az := user.NewAuthorizer(stubAuthzStore{fleetWide: true, grantedNodeErr: storeErr})
	_, _, err := az.AuthorizedNodes(context.Background(), user.Principal{UserID: "u1"}, user.PermissionNodeRead)
	if err == nil {
		t.Fatal("AuthorizedNodes returned nil for a failing store")
	}
	if errs.CodeOf(err) != errs.CodeUnavailable {
		t.Errorf("code = %v, want unavailable", errs.CodeOf(err))
	}
}
