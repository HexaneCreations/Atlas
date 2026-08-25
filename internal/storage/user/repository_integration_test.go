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
