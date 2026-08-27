//go:build integration

// These tests exercise page-access storage against a real PostgreSQL
// server, behind the `integration` build tag so `go test ./...` stays
// hermetic — see internal/storage/user/repository_integration_test.go for
// the same convention.
//
//	make db-up && make test-integration
package pageauthz_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/storage/pageauthz"
	storageuser "github.com/hexane/atlas/internal/storage/user"
	"github.com/hexane/atlas/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDatabaseURLEnv = "ATLAS_TEST_DATABASE_URL"

// newRepositories boots a fresh, throwaway database — the same convention
// internal/storage/user's integration tests use — and returns both this
// package's Repository and the user-identity one, since every grant here
// needs a real users row to satisfy the foreign key.
func newRepositories(t *testing.T) (*pageauthz.Repository, *storageuser.Repository) {
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

	name := fmt.Sprintf("atlas_pageauthz_test_%d", time.Now().UnixNano())
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

	return pageauthz.NewRepository(pool.DB()), storageuser.NewRepository(pool.DB())
}

func createTestUser(t *testing.T, users *storageuser.Repository, username string) coreuser.User {
	t.Helper()
	ctx := context.Background()

	hash, err := coreuser.HashPassword("a-long-enough-password")
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	if err := users.CreateUser(ctx, coreuser.User{Username: username, PasswordHash: hash}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := users.ByUsername(ctx, username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	return u
}

// --- RoleAccess bundle definitions -----------------------------------------

func TestCreateRoleAccessThenListReturnsIt(t *testing.T) {
	repo, _ := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.CreateRoleAccess(ctx, "container-related", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}

	defs, err := repo.ListRoleAccessDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListRoleAccessDefinitions: %v", err)
	}
	var found bool
	for _, d := range defs {
		if d.Name == "container-related" {
			found = true
		}
	}
	if !found {
		t.Errorf("definitions = %+v, want container-related present", defs)
	}
}

func TestCreateRoleAccessRejectsADuplicateName(t *testing.T) {
	repo, _ := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.CreateRoleAccess(ctx, "dup-bundle", "operator", now); err != nil {
		t.Fatalf("first CreateRoleAccess: %v", err)
	}
	err := repo.CreateRoleAccess(ctx, "dup-bundle", "operator", now)
	if errs.CodeOf(err) != errs.CodeAlreadyExists {
		t.Errorf("code = %v, want already_exists", errs.CodeOf(err))
	}
}

func TestAddPageToRoleAccessThenListReturnsThePages(t *testing.T) {
	repo, _ := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.CreateRoleAccess(ctx, "bundle-a", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "bundle-a", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess(containers): %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "bundle-a", corepageauthz.PagePorts); err != nil {
		t.Fatalf("AddPageToRoleAccess(ports): %v", err)
	}

	defs, err := repo.ListRoleAccessDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListRoleAccessDefinitions: %v", err)
	}
	var pages []corepageauthz.Page
	for _, d := range defs {
		if d.Name == "bundle-a" {
			pages = d.Pages
		}
	}
	if len(pages) != 2 {
		t.Fatalf("bundle-a pages = %v, want 2", pages)
	}
}

// The gap explicitly required to be closed: a fleet-only page must be
// rejected from a bundle outright, not silently accepted or coerced.
func TestAddPageToRoleAccessRejectsEveryFleetOnlyPage(t *testing.T) {
	repo, _ := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if err := repo.CreateRoleAccess(ctx, "fleet-only-attempt", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	for page := range corepageauthz.FleetOnlyPages {
		err := repo.AddPageToRoleAccess(ctx, "fleet-only-attempt", page)
		if err == nil {
			t.Errorf("page %q: AddPageToRoleAccess accepted a fleet-only page into a bundle", page)
			continue
		}
		if !errs.Is(err, corepageauthz.ErrPageHasNoNodeConcept) {
			t.Errorf("page %q: error = %v, want ErrPageHasNoNodeConcept", page, err)
		}
	}

	defs, err := repo.ListRoleAccessDefinitions(ctx)
	if err != nil {
		t.Fatalf("ListRoleAccessDefinitions: %v", err)
	}
	for _, d := range defs {
		if d.Name == "fleet-only-attempt" && len(d.Pages) != 0 {
			t.Errorf("bundle pages = %v, want none of the rejected fleet-only pages to have been persisted", d.Pages)
		}
	}
}

func TestAddPageToRoleAccessNotFoundForAnUnknownBundle(t *testing.T) {
	repo, _ := newRepositories(t)
	err := repo.AddPageToRoleAccess(context.Background(), "does-not-exist", corepageauthz.PageContainers)
	if errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("code = %v, want not_found", errs.CodeOf(err))
	}
}

// --- RoleAccess assignment (user_role_access) ------------------------------

func TestAssignRoleAccessThenEffectiveAccessReflectsIt(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "assign-alice")

	if err := repo.CreateRoleAccess(ctx, "assign-bundle", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "assign-bundle", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}

	spec := corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "assign-bundle", NodeID: "node-1"}
	if err := repo.AssignRoleAccess(ctx, spec, now); err != nil {
		t.Fatalf("AssignRoleAccess: %v", err)
	}

	fleetWide, nodeIDs, err := repo.EffectiveAccess(ctx, u.ID, corepageauthz.PageContainers)
	if err != nil {
		t.Fatalf("EffectiveAccess: %v", err)
	}
	if fleetWide {
		t.Error("fleetWide = true, want false for a node-scoped assignment")
	}
	if len(nodeIDs) != 1 || nodeIDs[0] != "node-1" {
		t.Errorf("nodeIDs = %v, want [node-1]", nodeIDs)
	}
}

func TestAssignRoleAccessIsIdempotentForAnIdenticalActiveAssignment(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "assign-idempotent")

	if err := repo.CreateRoleAccess(ctx, "idem-bundle", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	spec := corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "idem-bundle", FleetWide: true}
	if err := repo.AssignRoleAccess(ctx, spec, now); err != nil {
		t.Fatalf("first AssignRoleAccess: %v", err)
	}
	if err := repo.AssignRoleAccess(ctx, spec, now); err != nil {
		t.Fatalf("second AssignRoleAccess: %v", err)
	}

	assignments, err := repo.ListRoleAccessAssignments(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListRoleAccessAssignments: %v", err)
	}
	if len(assignments) != 1 {
		t.Errorf("assignments = %d, want 1 (repeated identical assignment must not duplicate)", len(assignments))
	}
}

func TestRevokeRoleAccessAssignmentRemovesTheCoverage(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "revoke-bob")

	if err := repo.CreateRoleAccess(ctx, "revoke-bundle", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "revoke-bundle", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}
	spec := corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "revoke-bundle", FleetWide: true}
	if err := repo.AssignRoleAccess(ctx, spec, now); err != nil {
		t.Fatalf("AssignRoleAccess: %v", err)
	}
	assignments, err := repo.ListRoleAccessAssignments(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListRoleAccessAssignments: %v", err)
	}
	if len(assignments) != 1 {
		t.Fatalf("assignments = %d, want 1", len(assignments))
	}

	if err := repo.RevokeRoleAccessAssignment(ctx, assignments[0].ID, "operator", now); err != nil {
		t.Fatalf("RevokeRoleAccessAssignment: %v", err)
	}

	fleetWide, nodeIDs, err := repo.EffectiveAccess(ctx, u.ID, corepageauthz.PageContainers)
	if err != nil {
		t.Fatalf("EffectiveAccess: %v", err)
	}
	if fleetWide || len(nodeIDs) != 0 {
		t.Errorf("effective access after revoke: fleetWide=%v nodeIDs=%v, want none", fleetWide, nodeIDs)
	}
}

// --- Direct UserAccess grants (user_page_access) + the conflict check -----

func TestGrantPageAccessThenEffectiveAccessReflectsIt(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "grant-carol")

	spec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageProcesses, NodeID: "node-5", GrantedBy: "operator"}
	if err := repo.GrantPageAccess(ctx, spec, now); err != nil {
		t.Fatalf("GrantPageAccess: %v", err)
	}

	fleetWide, nodeIDs, err := repo.EffectiveAccess(ctx, u.ID, corepageauthz.PageProcesses)
	if err != nil {
		t.Fatalf("EffectiveAccess: %v", err)
	}
	if fleetWide || len(nodeIDs) != 1 || nodeIDs[0] != "node-5" {
		t.Errorf("effective access = fleetWide=%v nodeIDs=%v, want node-5 only", fleetWide, nodeIDs)
	}
}

// Scenario 1: role fleet-wide -> direct grant for any node is a conflict.
func TestGrantPageAccessConflictsWithAFleetWideRoleGrant(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "conflict-fleetwide")

	if err := repo.CreateRoleAccess(ctx, "cf-bundle", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "cf-bundle", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}
	roleSpec := corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "cf-bundle", FleetWide: true}
	if err := repo.AssignRoleAccess(ctx, roleSpec, now); err != nil {
		t.Fatalf("AssignRoleAccess: %v", err)
	}

	grantSpec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageContainers, NodeID: "node-9", GrantedBy: "operator"}
	err := repo.GrantPageAccess(ctx, grantSpec, now)
	if !errs.Is(err, corepageauthz.ErrPageAccessConflict) {
		t.Fatalf("GrantPageAccess = %v, want ErrPageAccessConflict", err)
	}
}

// Scenario 2: role node-1 -> direct grant for node-2 (different node) is
// allowed, genuinely new coverage.
func TestGrantPageAccessAllowsADifferentNodeThanASpecificRoleGrant(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "conflict-diffnode")

	if err := repo.CreateRoleAccess(ctx, "dn-bundle", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "dn-bundle", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}
	roleSpec := corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "dn-bundle", NodeID: "node-1"}
	if err := repo.AssignRoleAccess(ctx, roleSpec, now); err != nil {
		t.Fatalf("AssignRoleAccess: %v", err)
	}

	grantSpec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageContainers, NodeID: "node-2", GrantedBy: "operator"}
	if err := repo.GrantPageAccess(ctx, grantSpec, now); err != nil {
		t.Fatalf("GrantPageAccess = %v, want nil (a different node is genuinely new access)", err)
	}

	fleetWide, nodeIDs, err := repo.EffectiveAccess(ctx, u.ID, corepageauthz.PageContainers)
	if err != nil {
		t.Fatalf("EffectiveAccess: %v", err)
	}
	if fleetWide {
		t.Error("fleetWide = true, want false")
	}
	if len(nodeIDs) != 2 {
		t.Errorf("nodeIDs = %v, want both node-1 (role) and node-2 (direct)", nodeIDs)
	}
}

// Scenario 3: no role grants at all -> direct grant always allowed.
func TestGrantPageAccessAllowedWithNoRoleGrantsAtAll(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "conflict-norole")

	grantSpec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageServices, FleetWide: true, GrantedBy: "operator"}
	if err := repo.GrantPageAccess(ctx, grantSpec, now); err != nil {
		t.Fatalf("GrantPageAccess = %v, want nil", err)
	}
}

// Scenario 4 (the reverse): role node-1 -> direct grant for the exact same
// node-1 is a conflict.
func TestGrantPageAccessConflictsWithTheExactSameNodeAsARoleGrant(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "conflict-samenode")

	if err := repo.CreateRoleAccess(ctx, "sn-bundle", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "sn-bundle", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}
	roleSpec := corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "sn-bundle", NodeID: "node-1"}
	if err := repo.AssignRoleAccess(ctx, roleSpec, now); err != nil {
		t.Fatalf("AssignRoleAccess: %v", err)
	}

	grantSpec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageContainers, NodeID: "node-1", GrantedBy: "operator"}
	err := repo.GrantPageAccess(ctx, grantSpec, now)
	if !errs.Is(err, corepageauthz.ErrPageAccessConflict) {
		t.Fatalf("GrantPageAccess = %v, want ErrPageAccessConflict", err)
	}
}

// Scenario 5: role node-1 -> direct grant fleet-wide is a conflict (the
// case confirmed in the design follow-up).
func TestGrantPageAccessConflictsWhenRequestingFleetWideOverASpecificRoleGrant(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "conflict-fleetreq")

	if err := repo.CreateRoleAccess(ctx, "fr-bundle", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "fr-bundle", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}
	roleSpec := corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "fr-bundle", NodeID: "node-1"}
	if err := repo.AssignRoleAccess(ctx, roleSpec, now); err != nil {
		t.Fatalf("AssignRoleAccess: %v", err)
	}

	grantSpec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageContainers, FleetWide: true, GrantedBy: "operator"}
	err := repo.GrantPageAccess(ctx, grantSpec, now)
	if !errs.Is(err, corepageauthz.ErrPageAccessConflict) {
		t.Fatalf("GrantPageAccess = %v, want ErrPageAccessConflict", err)
	}
}

// Scenario 6: two roles, neither alone covering the requested node -> no
// false-positive conflict; proves the union-across-roles requirement holds
// end to end, not just in the pure HasConflict unit tests.
func TestGrantPageAccessUnionsAcrossMultipleRoleAssignments(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "conflict-union")

	if err := repo.CreateRoleAccess(ctx, "union-a", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess(a): %v", err)
	}
	if err := repo.CreateRoleAccess(ctx, "union-b", "operator", now); err != nil {
		t.Fatalf("CreateRoleAccess(b): %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "union-a", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess(a): %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "union-b", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess(b): %v", err)
	}
	if err := repo.AssignRoleAccess(ctx, corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "union-a", NodeID: "node-1"}, now); err != nil {
		t.Fatalf("AssignRoleAccess(a): %v", err)
	}
	if err := repo.AssignRoleAccess(ctx, corepageauthz.RoleAccessAssignmentSpec{UserID: u.ID, RoleAccessName: "union-b", NodeID: "node-2"}, now); err != nil {
		t.Fatalf("AssignRoleAccess(b): %v", err)
	}

	// node-3 is covered by neither role -> allowed.
	if err := repo.GrantPageAccess(ctx, corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageContainers, NodeID: "node-3", GrantedBy: "operator"}, now); err != nil {
		t.Fatalf("GrantPageAccess(node-3) = %v, want nil", err)
	}
	// node-2 IS covered by union-b -> conflict, proving the union check
	// actually looks at every role, not just the first.
	err := repo.GrantPageAccess(ctx, corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageContainers, NodeID: "node-2", GrantedBy: "operator"}, now)
	if !errs.Is(err, corepageauthz.ErrPageAccessConflict) {
		t.Fatalf("GrantPageAccess(node-2) = %v, want ErrPageAccessConflict", err)
	}
}

func TestRevokePageAccessRemovesTheCoverage(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	now := time.Now().UTC()
	u := createTestUser(t, users, "revoke-dave")

	spec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: corepageauthz.PageDisks, FleetWide: true, GrantedBy: "operator"}
	if err := repo.GrantPageAccess(ctx, spec, now); err != nil {
		t.Fatalf("GrantPageAccess: %v", err)
	}
	grants, err := repo.ListPageAccessGrants(ctx, u.ID)
	if err != nil {
		t.Fatalf("ListPageAccessGrants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants = %d, want 1", len(grants))
	}

	if err := repo.RevokePageAccess(ctx, grants[0].ID, "operator", now); err != nil {
		t.Fatalf("RevokePageAccess: %v", err)
	}

	fleetWide, nodeIDs, err := repo.EffectiveAccess(ctx, u.ID, corepageauthz.PageDisks)
	if err != nil {
		t.Fatalf("EffectiveAccess: %v", err)
	}
	if fleetWide || len(nodeIDs) != 0 {
		t.Errorf("effective access after revoke: fleetWide=%v nodeIDs=%v, want none", fleetWide, nodeIDs)
	}
}

func TestEffectiveAccessReturnsNothingForAUserWithNoGrantsAtAll(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	u := createTestUser(t, users, "no-grants-erin")

	fleetWide, nodeIDs, err := repo.EffectiveAccess(ctx, u.ID, corepageauthz.PageContainers)
	if err != nil {
		t.Fatalf("EffectiveAccess: %v", err)
	}
	if fleetWide || len(nodeIDs) != 0 {
		t.Errorf("effective access = fleetWide=%v nodeIDs=%v, want none", fleetWide, nodeIDs)
	}
}

// --- Audit trail ------------------------------------------------------------

func TestGrantAndRevokeWriteAuditEntries(t *testing.T) {
	repo, users := newRepositories(t)
	ctx := context.Background()
	base := time.Now().UTC()
	actor := createTestUser(t, users, "audit-actor")
	target := createTestUser(t, users, "audit-target")

	var step int
	nextNow := func() time.Time {
		step++
		return base.Add(time.Duration(step) * time.Second)
	}

	if err := repo.CreateRoleAccess(ctx, "audit-bundle", actor.ID, nextNow()); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := repo.AddPageToRoleAccess(ctx, "audit-bundle", corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}
	roleSpec := corepageauthz.RoleAccessAssignmentSpec{UserID: target.ID, RoleAccessName: "audit-bundle", FleetWide: true, GrantedBy: actor.ID}
	if err := repo.AssignRoleAccess(ctx, roleSpec, nextNow()); err != nil {
		t.Fatalf("AssignRoleAccess: %v", err)
	}
	assignments, err := repo.ListRoleAccessAssignments(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListRoleAccessAssignments: %v", err)
	}
	if err := repo.RevokeRoleAccessAssignment(ctx, assignments[0].ID, actor.ID, nextNow()); err != nil {
		t.Fatalf("RevokeRoleAccessAssignment: %v", err)
	}
	grantSpec := corepageauthz.PageGrantSpec{UserID: target.ID, Page: corepageauthz.PageDisks, FleetWide: true, GrantedBy: actor.ID}
	if err := repo.GrantPageAccess(ctx, grantSpec, nextNow()); err != nil {
		t.Fatalf("GrantPageAccess: %v", err)
	}
	pageGrants, err := repo.ListPageAccessGrants(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListPageAccessGrants: %v", err)
	}
	if err := repo.RevokePageAccess(ctx, pageGrants[0].ID, actor.ID, nextNow()); err != nil {
		t.Fatalf("RevokePageAccess: %v", err)
	}

	entries, err := users.ListAudit(ctx, target.ID)
	if err != nil {
		t.Fatalf("ListAudit: %v", err)
	}
	wantActions := []string{
		corepageauthz.AuditActionRevokePageAccess, corepageauthz.AuditActionGrantPageAccess,
		corepageauthz.AuditActionRevokeRoleAccess, corepageauthz.AuditActionAssignRoleAccess,
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
