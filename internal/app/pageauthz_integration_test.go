//go:build integration

// The page-visibility layer, proven over real HTTP end to end: a user can
// hold node.read fleet-wide (the existing operation-level layer, untouched)
// and still be refused a specific page — the exact capability node.read
// alone cannot express (see internal/core/pageauthz's doc) — and the admin
// Users page's new endpoints (role-access bundles, direct page grants, the
// conflict check, the fleet-only violation check) work through the real
// router, not just the storage layer repository_integration_test.go already
// covers directly.
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"os"
	"testing"
	"time"

	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/id"
	storagepageauthz "github.com/hexane/atlas/internal/storage/pageauthz"
	storageuser "github.com/hexane/atlas/internal/storage/user"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bareUser creates a user holding role fleet-wide (so the existing
// operation-level layer always passes) but grants no page access at all —
// the deliberately minimal fixture these tests build specific page grants
// on top of via the real HTTP admin endpoints, to isolate what the
// page-visibility layer alone is doing.
func bareUser(t *testing.T, base, role string) (client *http.Client, userID string) {
	t.Helper()

	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	username := "it-pageauthz-" + role + "-" + id.New()
	const password = "integration-test-password"
	hash, err := coreuser.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	repo := storageuser.NewRepository(pool)
	if err := repo.CreateUser(context.Background(), coreuser.User{Username: username, PasswordHash: hash}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	u, err := repo.ByUsername(context.Background(), username)
	if err != nil {
		t.Fatalf("ByUsername: %v", err)
	}
	grant := coreuser.GrantSpec{UserID: u.ID, FleetWide: true, Role: role, GrantedBy: "integration-test"}
	if err := repo.Grant(context.Background(), grant, time.Now()); err != nil {
		t.Fatalf("Grant: %v", err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client = &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	return client, u.ID
}

func doJSON(t *testing.T, client *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// The central capability this whole layer exists for: node.read fleet-wide
// is not enough to reach a page without also holding page access for it,
// and holding page access for one page never leaks into another.
func TestPageAccessNarrowsBeyondNodeReadFleetWide(t *testing.T) {
	base := bootServer(t)
	admin := authenticatedTestClient(t, base, "admin")
	target, targetID := bareUser(t, base, "viewer")

	resp := doJSON(t, admin, http.MethodPost, base+"/api/v1/users/"+targetID+"/page-access",
		map[string]any{"page": "containers", "fleet_wide": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("grant page access status = %d, want 204", resp.StatusCode)
	}

	resp = doJSON(t, target, http.MethodGet, base+"/api/v1/containers", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /containers status = %d, want 200 (granted)", resp.StatusCode)
	}

	resp = doJSON(t, target, http.MethodGet, base+"/api/v1/processes", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("GET /processes status = %d, want 403 (never granted, despite fleet-wide node.read) — page access must not leak between pages", resp.StatusCode)
	}
}

func TestPageAccessDeniesEveryPageWithNoGrantAtAll(t *testing.T) {
	base := bootServer(t)
	target, _ := bareUser(t, base, "viewer")

	for _, path := range []string{"/api/v1/containers", "/api/v1/processes", "/api/v1/services", "/api/v1/cron", "/api/v1/ports", "/api/v1/mounts"} {
		resp := doJSON(t, target, http.MethodGet, base+path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s status = %d, want 403 (fleet-wide node.read but zero page access)", path, resp.StatusCode)
		}
	}
}

// A RoleAccess bundle, assigned through the real admin endpoint, grants
// every page it contains.
func TestAssignRoleAccessThroughTheRealAPIGrantsItsPages(t *testing.T) {
	base := bootServer(t)
	admin := authenticatedTestClient(t, base, "admin")
	target, targetID := bareUser(t, base, "viewer")

	// Bundle definitions are CLI/fixture-only this round — no HTTP endpoint
	// creates one, so this test builds it the way the CLI eventually will,
	// directly against storage, then proves *assignment* through the real
	// API the admin Users page will actually call.
	dsn := os.Getenv(testDatabaseURLEnv)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	pageRepo := storagepageauthz.NewRepository(pool)
	bundleName := "it-bundle-" + id.New()
	if err := pageRepo.CreateRoleAccess(context.Background(), bundleName, "integration-test", time.Now()); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	// Processes, not Services or Cron: both of those need a platform daemon
	// (systemd, a cron scheduler) this test's host may not have — see
	// web/vite.config.ts's own comment on exactly that for Services — and a
	// resulting 501 "plugin not available" would be a real, separate,
	// environment-specific failure this test has no business asserting
	// about. Processes runs on gopsutil, with no such platform dependency.
	if err := pageRepo.AddPageToRoleAccess(context.Background(), bundleName, corepageauthz.PageProcesses); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}

	resp := doJSON(t, admin, http.MethodPost, base+"/api/v1/users/"+targetID+"/role-access",
		map[string]any{"role_access_name": bundleName, "fleet_wide": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("assign role access status = %d, want 204", resp.StatusCode)
	}

	resp = doJSON(t, target, http.MethodGet, base+"/api/v1/processes", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /processes status = %d, want 200 (granted via the assigned bundle)", resp.StatusCode)
	}
}

// The conflict check, through the real API: granting a page directly when
// an assigned bundle already covers it for an overlapping scope is
// refused, not silently duplicated.
func TestGrantPageAccessConflictThroughTheRealAPIReturns409(t *testing.T) {
	base := bootServer(t)
	admin := authenticatedTestClient(t, base, "admin")
	_, targetID := bareUser(t, base, "viewer")

	dsn := os.Getenv(testDatabaseURLEnv)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	pageRepo := storagepageauthz.NewRepository(pool)
	bundleName := "it-conflict-bundle-" + id.New()
	if err := pageRepo.CreateRoleAccess(context.Background(), bundleName, "integration-test", time.Now()); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := pageRepo.AddPageToRoleAccess(context.Background(), bundleName, corepageauthz.PageContainers); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}

	resp := doJSON(t, admin, http.MethodPost, base+"/api/v1/users/"+targetID+"/role-access",
		map[string]any{"role_access_name": bundleName, "fleet_wide": true})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("assign role access status = %d, want 204", resp.StatusCode)
	}

	resp = doJSON(t, admin, http.MethodPost, base+"/api/v1/users/"+targetID+"/page-access",
		map[string]any{"page": "containers", "node_id": "some-node"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("grant page access status = %d, want 409 (overlaps the fleet-wide bundle grant)", resp.StatusCode)
	}
}

// The fleet_only violation, through the real API: a node-specific grant
// for a page with no per-node concept must be rejected outright.
func TestGrantPageAccessFleetOnlyViolationThroughTheRealAPIReturns400(t *testing.T) {
	base := bootServer(t)
	admin := authenticatedTestClient(t, base, "admin")
	_, targetID := bareUser(t, base, "viewer")

	resp := doJSON(t, admin, http.MethodPost, base+"/api/v1/users/"+targetID+"/page-access",
		map[string]any{"page": "users", "node_id": "some-node"})
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("grant page access status = %d, want 400 (users has no per-node concept)", resp.StatusCode)
	}
}

// GET /role-access and GET /users/{id}/page-access, through the real API —
// the reads the admin Users page's assignment picker and per-user view
// will use.
func TestListRoleAccessAndListUserPageAccessThroughTheRealAPI(t *testing.T) {
	base := bootServer(t)
	admin := authenticatedTestClient(t, base, "admin")
	_, targetID := bareUser(t, base, "viewer")

	dsn := os.Getenv(testDatabaseURLEnv)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	pageRepo := storagepageauthz.NewRepository(pool)
	bundleName := "it-list-bundle-" + id.New()
	if err := pageRepo.CreateRoleAccess(context.Background(), bundleName, "integration-test", time.Now()); err != nil {
		t.Fatalf("CreateRoleAccess: %v", err)
	}
	if err := pageRepo.AddPageToRoleAccess(context.Background(), bundleName, corepageauthz.PagePorts); err != nil {
		t.Fatalf("AddPageToRoleAccess: %v", err)
	}

	resp := doJSON(t, admin, http.MethodGet, base+"/api/v1/role-access", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /role-access status = %d, want 200", resp.StatusCode)
	}
	var listResp struct {
		RoleAccess []struct {
			Name  string   `json:"name"`
			Pages []string `json:"pages"`
		} `json:"role_access"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found bool
	for _, ra := range listResp.RoleAccess {
		if ra.Name == bundleName {
			found = true
		}
	}
	if !found {
		t.Errorf("role-access list = %+v, want %q present", listResp.RoleAccess, bundleName)
	}

	if resp := doJSON(t, admin, http.MethodPost, base+"/api/v1/users/"+targetID+"/role-access",
		map[string]any{"role_access_name": bundleName, "fleet_wide": true}); resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("assign role access status = %d, want 204", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := doJSON(t, admin, http.MethodPost, base+"/api/v1/users/"+targetID+"/page-access",
		map[string]any{"page": "disks", "fleet_wide": true}); resp.StatusCode != http.StatusNoContent {
		resp.Body.Close()
		t.Fatalf("grant page access status = %d, want 204", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp = doJSON(t, admin, http.MethodGet, base+"/api/v1/users/"+targetID+"/page-access", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /users/{id}/page-access status = %d, want 200", resp.StatusCode)
	}
	var userResp struct {
		RoleAccess []struct {
			RoleAccess string `json:"role_access"`
		} `json:"role_access"`
		Pages []struct {
			Page string `json:"page"`
		} `json:"pages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&userResp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(userResp.RoleAccess) != 1 || userResp.RoleAccess[0].RoleAccess != bundleName {
		t.Errorf("role_access = %+v, want exactly %q", userResp.RoleAccess, bundleName)
	}
	if len(userResp.Pages) != 1 || userResp.Pages[0].Page != "disks" {
		t.Errorf("pages = %+v, want exactly disks", userResp.Pages)
	}
}

// The bootstrap deadlock this whole file's last test proves closed: every
// /users/*/page-access and /users/*/role-access endpoint requires
// PermissionUserManage AND PageUsers (see internal/api/v1/pageaccess.go),
// so a user holding admin (user.manage) but zero page-access grants — the
// state a freshly created admin starts in — cannot call the one HTTP
// endpoint that would grant themselves PageUsers. cmd/atlas-server's
// `page-access grant` CLI command exists precisely because the HTTP API
// alone has no escape hatch here; this proves the same core call that CLI
// makes (Repository.GrantPageAccess) actually breaks the deadlock, on the
// exact same live session, with no re-login required.
func TestPageAccessGrantBreaksTheBootstrapDeadlock(t *testing.T) {
	base := bootServer(t)
	// admin operation-level role (user.manage), zero page-access grants —
	// exactly what migrations/0016_page_access_bootstrap.sql's own doc
	// describes as the state a newly created admin starts in today.
	client, userID := bareUser(t, base, "admin")

	resp := doJSON(t, client, http.MethodGet, base+"/api/v1/users", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET /users before the CLI grant = %d, want 403 (the deadlock this test proves exists before proving the fix)", resp.StatusCode)
	}

	dsn := os.Getenv(testDatabaseURLEnv)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	pageRepo := storagepageauthz.NewRepository(pool)
	// The exact call cmd/atlas-server's `page-access grant` CLI command
	// makes — not a re-implementation of it.
	spec := corepageauthz.PageGrantSpec{UserID: userID, Page: corepageauthz.PageUsers, FleetWide: true, GrantedBy: "operator"}
	if err := pageRepo.GrantPageAccess(context.Background(), spec, time.Now()); err != nil {
		t.Fatalf("GrantPageAccess: %v", err)
	}

	resp = doJSON(t, client, http.MethodGet, base+"/api/v1/users", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /users after the CLI grant = %d, want 200 — same session, no re-login, deadlock broken", resp.StatusCode)
	}
}
