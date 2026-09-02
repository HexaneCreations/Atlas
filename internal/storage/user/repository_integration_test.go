//go:build integration

// These tests exercise user storage against a real PostgreSQL server, behind
// the `integration` build tag so `go test ./...` stays hermetic — see
// internal/storage/fleet/repository_integration_test.go for the same
// convention and its rationale.
//
//	make db-up && make test-integration
package user_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/user"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

func newRepository(t *testing.T) *user.Repository {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}

	parsed, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse %s: %v", testDatabaseURLEnv, err)
	}

	admin, err := pgxpool.NewWithConfig(context.Background(), parsed)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close()

	name := fmt.Sprintf("atlas_user_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := pgxpool.NewWithConfig(context.Background(), parsed)
		if err != nil {
			return
		}
		defer cleanup.Close()
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})

	cfg := config.Default()
	cfg.Database.Host = parsed.ConnConfig.Host
	cfg.Database.Port = int(parsed.ConnConfig.Port)
	cfg.Database.Name = name
	cfg.Database.User = parsed.ConnConfig.User
	cfg.Database.Password = parsed.ConnConfig.Password
	cfg.Database.SSLMode = "disable"
	cfg.Database.MigrateOnStart = true

	pool := postgres.NewPool(cfg.Database, log.Discard())
	ctx := context.Background()
	if err := pool.Start(ctx); err != nil {
		t.Fatalf("pool start: %v", err)
	}
	t.Cleanup(func() { _ = pool.Stop(context.Background()) })

	migrator := postgres.NewMigrator(pool.DB(), migrations.FS, log.Discard())
	if _, err := migrator.Apply(ctx); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	return user.NewRepository(pool.DB())
}

func createTestUser(t *testing.T, repo *user.Repository, username string) coreuser.User {
	t.Helper()
	ctx := context.Background()

	hash, err := coreuser.HashPassword("a-long-enough-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := repo.CreateUser(ctx, coreuser.User{Username: username, PasswordHash: hash}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := repo.ByUsername(ctx, username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	return u
}

func TestCreateUserThenByUsernameRoundTrips(t *testing.T) {
	repo := newRepository(t)
	u := createTestUser(t, repo, "alice")

	if u.Username != "alice" {
		t.Errorf("username = %q, want alice", u.Username)
	}
	if u.ID == "" {
		t.Error("CreateUser did not assign an id")
	}
}

// The login identifier must be case-insensitive, per
// migrations/0012_users.sql's index — an operator typing "Alice" must reach
// the same account as "alice".
func TestByUsernameIsCaseInsensitive(t *testing.T) {
	repo := newRepository(t)
	createTestUser(t, repo, "Bob")

	u, err := repo.ByUsername(context.Background(), "bob")
	if err != nil {
		t.Fatalf("ByUsername(bob): %v", err)
	}
	if u.Username != "Bob" {
		t.Errorf("username = %q, want Bob", u.Username)
	}
}

func TestByUsernameNotFoundForUnknownUser(t *testing.T) {
	repo := newRepository(t)

	_, err := repo.ByUsername(context.Background(), "nobody")
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("code = %v, want not_found", errs.CodeOf(err))
	}
}

func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	repo := newRepository(t)
	createTestUser(t, repo, "carol")

	hash, err := coreuser.HashPassword("a-long-enough-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	err = repo.CreateUser(context.Background(), coreuser.User{Username: "carol", PasswordHash: hash})
	if errs.CodeOf(err) != errs.CodeAlreadyExists {
		t.Errorf("code = %v, want already_exists", errs.CodeOf(err))
	}
}

func TestCreateSessionThenResolveRoundTrips(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "dave")
	now := time.Now().UTC()

	generated, err := coreuser.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess := coreuser.Session{TokenHash: generated.Hash, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := repo.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	principal, err := repo.Resolve(ctx, generated.Hash, now)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if principal.UserID != u.ID || principal.Username != u.Username {
		t.Errorf("principal = %+v, want user %s/%s", principal, u.ID, u.Username)
	}
}

func TestResolveRejectsAnExpiredSession(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "erin")
	now := time.Now().UTC()

	generated, err := coreuser.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess := coreuser.Session{TokenHash: generated.Hash, UserID: u.ID, CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	if err := repo.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	_, err = repo.Resolve(ctx, generated.Hash, now)
	if !errs.Is(err, coreuser.ErrSessionInvalid) {
		t.Errorf("Resolve(expired) = %v, want ErrSessionInvalid", err)
	}
}

func TestRevokeSessionThenResolveFails(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "frank")
	now := time.Now().UTC()

	generated, err := coreuser.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess := coreuser.Session{TokenHash: generated.Hash, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := repo.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := repo.RevokeSession(ctx, generated.Hash, now); err != nil {
		t.Fatalf("RevokeSession: %v", err)
	}

	_, err = repo.Resolve(ctx, generated.Hash, now)
	if !errs.Is(err, coreuser.ErrSessionInvalid) {
		t.Errorf("Resolve(revoked) = %v, want ErrSessionInvalid", err)
	}
}

// Revoking an unknown or already-revoked session must not error — the
// caller's intent holds either way, the same idempotency
// [coreuser.SessionStore.RevokeSession] documents.
func TestRevokeSessionIsIdempotent(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.RevokeSession(ctx, "does-not-exist", now); err != nil {
		t.Errorf("RevokeSession(unknown) = %v, want nil", err)
	}
}

func TestHasPermissionFalseWithNoGrant(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "grace")

	ok, err := repo.HasPermission(ctx, u.ID, "node-1", coreuser.PermissionNodeRead)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if ok {
		t.Error("HasPermission true for a user with no grant at all")
	}
}

func TestGrantThenHasPermissionRoundTripsForANodeScopedGrant(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "heidi")
	now := time.Now().UTC()

	spec := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-1", Role: coreuser.RoleViewer, GrantedBy: "test"}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	ok, err := repo.HasPermission(ctx, u.ID, "node-1", coreuser.PermissionNodeRead)
	if err != nil {
		t.Fatalf("HasPermission(node-1): %v", err)
	}
	if !ok {
		t.Error("HasPermission false immediately after Grant for the same node")
	}

	// A viewer role grants node.read but not node.logs.read — the two are
	// deliberately separate permissions per docs/adr/0011-deferred-rbac.md.
	ok, err = repo.HasPermission(ctx, u.ID, "node-1", coreuser.PermissionNodeLogsRead)
	if err != nil {
		t.Fatalf("HasPermission(logs): %v", err)
	}
	if ok {
		t.Error("HasPermission(node.logs.read) true for a viewer grant — viewer must not include log content")
	}

	// A grant scoped to node-1 must not leak to node-2.
	ok, err = repo.HasPermission(ctx, u.ID, "node-2", coreuser.PermissionNodeRead)
	if err != nil {
		t.Fatalf("HasPermission(node-2): %v", err)
	}
	if ok {
		t.Error("HasPermission true for a node the grant does not name")
	}
}

// The central case docs/adr/0011-deferred-rbac.md requires: node_id NULL
// (fleet-wide) applies to every node, not just the one it happened to be
// checked against first.
func TestFleetWideGrantAppliesToAnyNode(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "ivan")
	now := time.Now().UTC()

	spec := coreuser.GrantSpec{UserID: u.ID, FleetWide: true, Role: coreuser.RoleOperator, GrantedBy: "test"}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	for _, nodeID := range []string{"node-a", "node-b", "some-other-node"} {
		ok, err := repo.HasPermission(ctx, u.ID, nodeID, coreuser.PermissionNodeRead)
		if err != nil {
			t.Fatalf("HasPermission(%s): %v", nodeID, err)
		}
		if !ok {
			t.Errorf("fleet-wide grant did not apply to %s", nodeID)
		}
	}
}

func TestRevokeGrantRemovesThePermission(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "judy")
	now := time.Now().UTC()

	spec := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-1", Role: coreuser.RoleViewer, GrantedBy: "test"}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	grants, err := repo.ListGrants(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}

	if err := repo.RevokeGrant(ctx, grants[0].ID, "operator", now); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}

	ok, err := repo.HasPermission(ctx, u.ID, "node-1", coreuser.PermissionNodeRead)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if ok {
		t.Error("HasPermission true after RevokeGrant")
	}
}

// The invariant migrations/0012_users.sql's ON CONFLICT depends on: granting
// the same (user, node, role) twice must not create a second active row.
func TestGrantIsIdempotentForAnIdenticalActiveGrant(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "kevin")
	now := time.Now().UTC()

	spec := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-1", Role: coreuser.RoleViewer, GrantedBy: "test"}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("second Grant: %v", err)
	}

	grants, err := repo.ListGrants(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if len(grants) != 1 {
		t.Errorf("grants = %d, want 1 — a repeated identical grant must not duplicate the row", len(grants))
	}
}

// Mirrors fleet's TestGrantDoesNotResurrectARevokedGrant: re-granting after
// a revocation must produce a new, separately revocable row, not silently
// undo the operator's revocation.
func TestGrantAfterRevokeCreatesANewActiveGrant(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	u := createTestUser(t, repo, "laura")
	now := time.Now().UTC()

	spec := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-1", Role: coreuser.RoleViewer, GrantedBy: "test"}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	grants, err := repo.ListGrants(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if err := repo.RevokeGrant(ctx, grants[0].ID, "operator", now); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}

	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("second Grant: %v", err)
	}

	ok, err := repo.HasPermission(ctx, u.ID, "node-1", coreuser.PermissionNodeRead)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if !ok {
		t.Error("HasPermission false after re-granting a previously revoked role")
	}
}

// grantFleetAdmin is a small helper so the last-admin-guard tests below read
// as "given these admins exist" rather than repeating Grant/ListGrants
// boilerplate.
func grantFleetAdmin(t *testing.T, repo *user.Repository, userID string, now time.Time) {
	t.Helper()
	spec := coreuser.GrantSpec{UserID: userID, FleetWide: true, Role: coreuser.RoleAdmin, GrantedBy: "test"}
	if err := repo.Grant(context.Background(), spec, now); err != nil {
		t.Fatalf("Grant(admin, fleet-wide): %v", err)
	}
}

// This is the guard item 3 of the admin-users-page addendum asks to be
// proven directly: revoking the sole remaining fleet-wide admin grant must
// be refused, not silently leave every user locked out of user management.
func TestRevokeGrantRefusesToRemoveTheLastFleetWideAdmin(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	admin := createTestUser(t, repo, "mallory-admin")
	grantFleetAdmin(t, repo, admin.ID, now)

	grants, err := repo.ListGrants(ctx, admin.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}

	err = repo.RevokeGrant(ctx, grants[0].ID, "operator", now)
	if !errs.Is(err, coreuser.ErrLastAdminGrant) {
		t.Fatalf("RevokeGrant(last admin) = %v, want ErrLastAdminGrant", err)
	}

	ok, err := repo.HasPermission(ctx, admin.ID, "", coreuser.PermissionUserManage)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if !ok {
		t.Error("the refused revoke still removed the grant")
	}
}

// With a second enabled fleet-wide admin present, revoking the first must
// succeed — the guard blocks only the *last* one, not every revoke of an
// admin grant.
func TestRevokeGrantAllowsRemovingAnAdminWhenAnotherRemains(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first := createTestUser(t, repo, "nathan-admin")
	second := createTestUser(t, repo, "olivia-admin")
	grantFleetAdmin(t, repo, first.ID, now)
	grantFleetAdmin(t, repo, second.ID, now)

	grants, err := repo.ListGrants(ctx, first.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if err := repo.RevokeGrant(ctx, grants[0].ID, "operator", now); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}

	ok, err := repo.HasPermission(ctx, first.ID, "", coreuser.PermissionUserManage)
	if err != nil {
		t.Fatalf("HasPermission: %v", err)
	}
	if ok {
		t.Error("HasPermission true after a successful revoke")
	}
}

// A fleet-wide admin grant held by a *disabled* user must not count as
// "someone else still has it" — a disabled account cannot log in to use
// that access, so it provides no real recovery path.
func TestRevokeGrantTreatsADisabledAdminAsNotCounting(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	active := createTestUser(t, repo, "peter-admin")
	disabled := createTestUser(t, repo, "quinn-admin")
	grantFleetAdmin(t, repo, active.ID, now)
	grantFleetAdmin(t, repo, disabled.ID, now)
	if err := repo.DisableUser(ctx, disabled.ID, "operator", now); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}

	grants, err := repo.ListGrants(ctx, active.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}

	err = repo.RevokeGrant(ctx, grants[0].ID, "operator", now)
	if !errs.Is(err, coreuser.ErrLastAdminGrant) {
		t.Fatalf("RevokeGrant = %v, want ErrLastAdminGrant (the other admin is disabled)", err)
	}
}

// Item 3's other half: DisableUser must hit the same guard, not a separate
// and possibly inconsistent implementation of it.
func TestDisableUserRefusesToDisableTheLastFleetWideAdmin(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	admin := createTestUser(t, repo, "rachel-admin")
	grantFleetAdmin(t, repo, admin.ID, now)

	err := repo.DisableUser(ctx, admin.ID, "operator", now)
	if !errs.Is(err, coreuser.ErrLastAdminGrant) {
		t.Fatalf("DisableUser(last admin) = %v, want ErrLastAdminGrant", err)
	}

	got, err := repo.GetUser(ctx, admin.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.Disabled() {
		t.Error("the refused disable still disabled the user")
	}
}

// Disabling a non-admin, or an admin while another remains, must both
// succeed and terminate the user's live sessions.
func TestDisableUserTerminatesSessionsAndIsIdempotent(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "sam-viewer")

	generated, err := coreuser.NewSession()
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	sess := coreuser.Session{TokenHash: generated.Hash, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	if err := repo.CreateSession(ctx, sess); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := repo.DisableUser(ctx, u.ID, "operator", now); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if _, err := repo.Resolve(ctx, generated.Hash, now); !errs.Is(err, coreuser.ErrSessionInvalid) {
		t.Errorf("Resolve after DisableUser = %v, want ErrSessionInvalid", err)
	}

	// Disabling an already-disabled user is a no-op, not an error.
	if err := repo.DisableUser(ctx, u.ID, "operator", now); err != nil {
		t.Errorf("DisableUser(already disabled) = %v, want nil", err)
	}
}

func TestEnableUserReversesDisableAndIsIdempotent(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "tara-viewer")

	if err := repo.DisableUser(ctx, u.ID, "operator", now); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if err := repo.EnableUser(ctx, u.ID, "operator", now); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}

	got, err := repo.GetUser(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if got.Disabled() {
		t.Error("user still disabled after EnableUser")
	}

	if err := repo.EnableUser(ctx, u.ID, "operator", now); err != nil {
		t.Errorf("EnableUser(already enabled) = %v, want nil", err)
	}
}

func TestResetPasswordIssuesAWorkingNewPassword(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "ursula-viewer")

	plaintext, err := repo.ResetPassword(ctx, u.ID, "operator", now)
	if err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if plaintext == "" {
		t.Fatal("ResetPassword returned an empty password")
	}

	got, err := repo.ByUsername(ctx, u.Username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	if !coreuser.VerifyPassword(got.PasswordHash, plaintext) {
		t.Error("the returned plaintext does not verify against the stored hash")
	}
	if coreuser.VerifyPassword(got.PasswordHash, "a-long-enough-password") {
		t.Error("the original password still verifies after ResetPassword")
	}
}

func TestRevokeAllSessionsTerminatesEveryLiveSession(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "victor-viewer")

	var hashes []string
	for range 3 {
		generated, err := coreuser.NewSession()
		if err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		hashes = append(hashes, generated.Hash)
		sess := coreuser.Session{TokenHash: generated.Hash, UserID: u.ID, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := repo.CreateSession(ctx, sess); err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
	}

	if err := repo.RevokeAllSessions(ctx, u.ID, "operator", now); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}
	for _, h := range hashes {
		if _, err := repo.Resolve(ctx, h, now); !errs.Is(err, coreuser.ErrSessionInvalid) {
			t.Errorf("Resolve(%s) after RevokeAllSessions = %v, want ErrSessionInvalid", h, err)
		}
	}
}

func TestRevokeAllSessionsNotFoundForUnknownUser(t *testing.T) {
	repo := newRepository(t)
	err := repo.RevokeAllSessions(context.Background(), "does-not-exist", "operator", time.Now())
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("code = %v, want not_found", errs.CodeOf(err))
	}
}

func TestAdminCreateUserPersistsAWorkingGeneratedPassword(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	created, plaintext, err := repo.AdminCreateUser(ctx, "wendy", "wendy@example.com", "operator-id", now)
	if err != nil {
		t.Fatalf("AdminCreateUser: %v", err)
	}
	if created.ID == "" || plaintext == "" {
		t.Fatalf("AdminCreateUser returned an empty id or password: %+v", created)
	}
	if !coreuser.VerifyPassword(created.PasswordHash, plaintext) {
		t.Error("the returned plaintext does not verify against the stored hash")
	}

	entries, err := repo.ListAudit(ctx, created.ID)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 || entries[0].Action != coreuser.AuditActionCreateUser {
		t.Fatalf("audit entries = %+v, want exactly one create_user entry", entries)
	}
}

// Item 1 of the admin-users-page addendum: every grant, revoke, disable,
// enable, reset-password and force-logout must be readable back, not just
// written — this walks one user through all of them and checks the full
// trail.
func TestListAuditRecordsEveryUserManagementAction(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	base := time.Now().UTC()
	actor := createTestUser(t, repo, "xavier-admin")
	target := createTestUser(t, repo, "yolanda-viewer")

	// Each action gets a strictly later timestamp: ListAudit orders by
	// created_at DESC, which is only well-defined across distinct instants —
	// real actions never share one to the microsecond the way a single
	// reused `now` would here.
	var step int
	nextNow := func() time.Time {
		step++
		return base.Add(time.Duration(step) * time.Second)
	}

	spec := coreuser.GrantSpec{UserID: target.ID, NodeID: "node-1", Role: coreuser.RoleViewer, GrantedBy: actor.ID}
	if err := repo.Grant(ctx, spec, nextNow()); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	grants, err := repo.ListGrants(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	if err := repo.RevokeGrant(ctx, grants[0].ID, actor.ID, nextNow()); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if err := repo.DisableUser(ctx, target.ID, actor.ID, nextNow()); err != nil {
		t.Fatalf("DisableUser: %v", err)
	}
	if err := repo.EnableUser(ctx, target.ID, actor.ID, nextNow()); err != nil {
		t.Fatalf("EnableUser: %v", err)
	}
	if _, err := repo.ResetPassword(ctx, target.ID, actor.ID, nextNow()); err != nil {
		t.Fatalf("ResetPassword: %v", err)
	}
	if err := repo.RevokeAllSessions(ctx, target.ID, actor.ID, nextNow()); err != nil {
		t.Fatalf("RevokeAllSessions: %v", err)
	}

	entries, err := repo.ListAudit(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	wantActions := []string{
		coreuser.AuditActionForceLogout, coreuser.AuditActionResetPassword, coreuser.AuditActionEnableUser,
		coreuser.AuditActionDisableUser, coreuser.AuditActionRevokeRole, coreuser.AuditActionGrantRole,
	}
	if len(entries) != len(wantActions) {
		t.Fatalf("entries = %d, want %d: %+v", len(entries), len(wantActions), entries)
	}
	for i, want := range wantActions {
		if entries[i].Action != want {
			t.Errorf("entries[%d].Action = %q, want %q (newest first)", i, entries[i].Action, want)
		}
		if entries[i].ActorUsername != actor.Username {
			t.Errorf("entries[%d].ActorUsername = %q, want %q", i, entries[i].ActorUsername, actor.Username)
		}
	}
}

// Re-granting an identical, already-active grant must not write a second
// audit entry — Grant's ON CONFLICT DO NOTHING means no row was actually
// inserted, so the RETURNING-gated audit insert must also produce nothing.
func TestGrantWritesNoAuditEntryWhenAlreadyActive(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "zack-viewer")

	spec := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-1", Role: coreuser.RoleViewer, GrantedBy: "operator"}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("first Grant: %v", err)
	}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("second Grant: %v", err)
	}

	entries, err := repo.ListAudit(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("audit entries = %d, want 1 (the repeated grant must not duplicate it)", len(entries))
	}
}

func TestListUsersWithGrantsReturnsOnlyActiveGrants(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "amy-viewer")

	active := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-1", Role: coreuser.RoleViewer, GrantedBy: "operator"}
	revoked := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-2", Role: coreuser.RoleOperator, GrantedBy: "operator"}
	if err := repo.Grant(ctx, active, now); err != nil {
		t.Fatalf("Grant(active): %v", err)
	}
	if err := repo.Grant(ctx, revoked, now); err != nil {
		t.Fatalf("Grant(revoked): %v", err)
	}
	grants, err := repo.ListGrants(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	for _, g := range grants {
		if g.Role == coreuser.RoleOperator {
			if err := repo.RevokeGrant(ctx, g.ID, "operator", now); err != nil {
				t.Fatalf("RevokeGrant: %v", err)
			}
		}
	}

	users, err := repo.ListUsersWithGrants(ctx)
	if err != nil {
		t.Fatalf("ListUsersWithGrants: %v", err)
	}
	var found *coreuser.UserWithGrants
	for i := range users {
		if users[i].User.ID == u.ID {
			found = &users[i]
		}
	}
	if found == nil {
		t.Fatalf("user %s not present in ListUsersWithGrants", u.ID)
	}
	if len(found.Grants) != 1 || found.Grants[0].Role != coreuser.RoleViewer {
		t.Errorf("grants = %+v, want exactly the one active viewer grant", found.Grants)
	}
}

// grantFleetSuperadmin mirrors grantFleetAdmin for the protected fourth
// role, added in migrations/0019_superadmin_role.sql.
func grantFleetSuperadmin(t *testing.T, repo *user.Repository, userID string, now time.Time) {
	t.Helper()
	spec := coreuser.GrantSpec{UserID: userID, FleetWide: true, Role: coreuser.RoleSuperadmin, GrantedBy: "test"}
	if err := repo.Grant(context.Background(), spec, now); err != nil {
		t.Fatalf("Grant(superadmin, fleet-wide): %v", err)
	}
}

// A fleet-wide superadmin grant round-trips, is reported by IsSuperadmin
// (fleet-wide only, matching admin's user.manage), and carries every
// permission admin does — it is a strict superset.
func TestSuperadminGrantRoundTripsAndHoldsEveryAdminPermission(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "sofia-super")
	grantFleetSuperadmin(t, repo, u.ID, now)

	is, err := repo.IsSuperadmin(ctx, u.ID)
	if err != nil {
		t.Fatalf("IsSuperadmin: %v", err)
	}
	if !is {
		t.Fatal("IsSuperadmin false right after a fleet-wide superadmin grant")
	}

	for _, p := range []coreuser.Permission{
		coreuser.PermissionNodeRead, coreuser.PermissionNodeLogsRead,
		coreuser.PermissionFleetWrite, coreuser.PermissionUserManage,
	} {
		ok, err := repo.HasPermission(ctx, u.ID, "any-node", p)
		if err != nil {
			t.Fatalf("HasPermission(%s): %v", p, err)
		}
		if !ok {
			t.Errorf("superadmin lacks %s; it must be a strict superset of admin", p)
		}
	}

	other := createTestUser(t, repo, "tariq-plain")
	if is, err := repo.IsSuperadmin(ctx, other.ID); err != nil || is {
		t.Errorf("IsSuperadmin(non-superadmin) = %v, %v; want false, nil", is, err)
	}
}

// A node-scoped superadmin grant (which the manual bootstrap never creates,
// but nothing at the storage layer forbids) does not satisfy IsSuperadmin —
// the protected tier is a fleet concept, the same shape as the last-admin
// guard's isActiveFleetWideAdmin.
func TestIsSuperadminIgnoresANodeScopedGrant(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "uma-node-super")
	spec := coreuser.GrantSpec{UserID: u.ID, NodeID: "node-1", Role: coreuser.RoleSuperadmin, GrantedBy: "test"}
	if err := repo.Grant(ctx, spec, now); err != nil {
		t.Fatalf("Grant(superadmin, node-scoped): %v", err)
	}

	is, err := repo.IsSuperadmin(ctx, u.ID)
	if err != nil {
		t.Fatalf("IsSuperadmin: %v", err)
	}
	if is {
		t.Error("IsSuperadmin true for a node-scoped grant; it must require fleet-wide")
	}
}

// GrantOwner resolves a grant id back to its user, and returns "" (not an
// error) for an unknown or already-revoked id — the same caller-intent
// no-op RevokeGrant itself applies.
func TestGrantOwnerResolvesActiveGrantsOnly(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, repo, "victor-owner")
	grantFleetAdmin(t, repo, u.ID, now)

	grants, err := repo.ListGrants(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	gid := grants[0].ID

	owner, err := repo.GrantOwner(ctx, gid)
	if err != nil {
		t.Fatalf("GrantOwner: %v", err)
	}
	if owner != u.ID {
		t.Errorf("GrantOwner = %q, want %q", owner, u.ID)
	}

	if owner, err := repo.GrantOwner(ctx, "no-such-grant-id"); err != nil || owner != "" {
		t.Errorf("GrantOwner(unknown) = %q, %v; want \"\", nil", owner, err)
	}

	// A second fleet admin so the revoke below is not blocked by the
	// last-admin guard.
	second := createTestUser(t, repo, "wendy-owner")
	grantFleetAdmin(t, repo, second.ID, now)
	if err := repo.RevokeGrant(ctx, gid, "operator", now); err != nil {
		t.Fatalf("RevokeGrant: %v", err)
	}
	if owner, err := repo.GrantOwner(ctx, gid); err != nil || owner != "" {
		t.Errorf("GrantOwner(revoked) = %q, %v; want \"\", nil", owner, err)
	}
}

// Point 6 of the superadmin feature request, confirmed against real SQL: the
// last-admin guard counts role_name = 'admin' rows only. A superadmin grant
// is a different role string, so it does NOT satisfy "at least one fleet-wide
// admin remains" — revoking the sole admin's grant still trips
// ErrLastAdminGrant even while a superadmin exists.
//
// This is deliberately left as-is (the request said flag, do not change
// without approval). The practical guidance: keep an admin grant on the
// superadmin user too, or follow up to teach otherEnabledFleetWideAdminExists
// about superadmin.
func TestSuperadminDoesNotCountTowardTheLastAdminGuard(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	now := time.Now().UTC()

	onlyAdmin := createTestUser(t, repo, "xena-admin")
	grantFleetAdmin(t, repo, onlyAdmin.ID, now)
	superOnly := createTestUser(t, repo, "yuri-super")
	grantFleetSuperadmin(t, repo, superOnly.ID, now)

	grants, err := repo.ListGrants(ctx, onlyAdmin.ID)
	if err != nil {
		t.Fatalf("ListGrants: %v", err)
	}
	err = repo.RevokeGrant(ctx, grants[0].ID, "operator", now)
	if !errs.Is(err, coreuser.ErrLastAdminGrant) {
		t.Fatalf("RevokeGrant(sole admin, with a superadmin present) = %v, want ErrLastAdminGrant — superadmin must not count as an admin here", err)
	}

	// And DisableUser hits the same guard for the same reason.
	if err := repo.DisableUser(ctx, onlyAdmin.ID, "operator", now); !errs.Is(err, coreuser.ErrLastAdminGrant) {
		t.Fatalf("DisableUser(sole admin) = %v, want ErrLastAdminGrant", err)
	}
}
