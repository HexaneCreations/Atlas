//go:build integration

// The superadmin protection rule against real Postgres, through the real
// HTTP chain: an admin can never change anything of a superadmin (roles,
// page access, bundles, password, sessions, audit), while admin-on-admin and
// superadmin-on-admin keep working, and the API never revokes or disables
// the superadmin, whoever asks. Every
// refusal is checked for "no change" by re-reading every table the action
// could touch, including user_audit_log row counts.
package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"testing"
	"time"

	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/id"
	storagepageauthz "github.com/hexane/atlas/internal/storage/pageauthz"
	storageuser "github.com/hexane/atlas/internal/storage/user"
	"github.com/jackc/pgx/v5/pgxpool"
)

const protectedSuperadminMessage = "superadmin accounts can only be changed by a superadmin"

type roleGrant struct {
	role   string
	nodeID string // "" means fleet-wide
}

// protectionFixture creates users directly in the database and logs actors
// in over the real auth endpoint.
type protectionFixture struct {
	t     *testing.T
	base  string
	pool  *pgxpool.Pool
	users *storageuser.Repository
	pages *storagepageauthz.Repository
}

func newProtectionFixture(t *testing.T) *protectionFixture {
	t.Helper()
	dsn := os.Getenv(testDatabaseURLEnv)
	if dsn == "" {
		t.Skipf("%s is not set; run `make db-up` first", testDatabaseURLEnv)
	}
	base := bootServer(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return &protectionFixture{
		t: t, base: base, pool: pool,
		users: storageuser.NewRepository(pool), pages: storagepageauthz.NewRepository(pool),
	}
}

func (f *protectionFixture) createUser(label string, grants []roleGrant, pages []corepageauthz.Page) (userID, password string) {
	f.t.Helper()
	ctx := context.Background()
	username := "it-saprot-" + label + "-" + id.New()
	password = "integration-test-password"
	hash, err := coreuser.HashPassword(password)
	if err != nil {
		f.t.Fatalf("HashPassword: %v", err)
	}
	if err := f.users.CreateUser(ctx, coreuser.User{Username: username, PasswordHash: hash}); err != nil {
		f.t.Fatalf("CreateUser: %v", err)
	}
	u, err := f.users.ByUsername(ctx, username)
	if err != nil {
		f.t.Fatalf("ByUsername: %v", err)
	}
	for _, g := range grants {
		if g.role == coreuser.RoleSuperadmin {
			releaseSuperadminSlot(f.t, f.pool)
		}
		spec := coreuser.GrantSpec{UserID: u.ID, Role: g.role, NodeID: g.nodeID, FleetWide: g.nodeID == "", GrantedBy: "integration-test"}
		if err := f.users.Grant(ctx, spec, time.Now()); err != nil {
			f.t.Fatalf("Grant(%s): %v", g.role, err)
		}
	}
	for _, p := range pages {
		spec := corepageauthz.PageGrantSpec{UserID: u.ID, Page: p, FleetWide: true, GrantedBy: "integration-test"}
		if err := f.pages.GrantPageAccess(ctx, spec, time.Now()); err != nil {
			f.t.Fatalf("GrantPageAccess(%s): %v", p, err)
		}
	}
	return u.ID, password
}

func allPages() []corepageauthz.Page {
	out := make([]corepageauthz.Page, 0, len(corepageauthz.KnownPages))
	for p := range corepageauthz.KnownPages {
		out = append(out, p)
	}
	return out
}

// actor creates a user holding grants plus fleet-wide access to every page,
// and logs them in over POST /auth/login.
func (f *protectionFixture) actor(label string, grants ...roleGrant) (*http.Client, string) {
	f.t.Helper()
	userID, password := f.createUser(label, grants, allPages())
	var username string
	if err := f.pool.QueryRow(context.Background(), `SELECT username FROM users WHERE id = $1`, userID).Scan(&username); err != nil {
		f.t.Fatalf("lookup username: %v", err)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		f.t.Fatalf("cookiejar: %v", err)
	}
	client := &http.Client{Jar: jar}
	body, _ := json.Marshal(map[string]string{"username": username, "password": password})
	resp, err := client.Post(f.base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		f.t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		f.t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	return client, userID
}

// targetUser is an account with something on every axis an action can
// change: role grants, a bundle assignment, direct page grants, and a live
// session.
type targetUser struct {
	id            string
	adminGrant    string
	superGrant    string // "" for a non-superadmin target
	assignment    string
	pageGrant     string
	assignBundle  string // a bundle the target does not hold yet
	grantablePage corepageauthz.Page
	accessVersion string // the access editor's version of the target's state
}

func (f *protectionFixture) target(superadmin, disabled bool) targetUser {
	f.t.Helper()
	ctx := context.Background()
	now := time.Now()

	grants := []roleGrant{{role: coreuser.RoleAdmin}}
	if superadmin {
		grants = append(grants, roleGrant{role: coreuser.RoleSuperadmin})
	}
	// Every page directly except containers (covered by a bundle below) and
	// disks (left free so a page grant would succeed if not refused).
	var pages []corepageauthz.Page
	for _, p := range allPages() {
		if p != corepageauthz.PageContainers && p != corepageauthz.PageDisks {
			pages = append(pages, p)
		}
	}
	userID, _ := f.createUser("target", grants, pages)

	held := "it-saprot-held-" + id.New()
	free := "it-saprot-free-" + id.New()
	for _, b := range []struct {
		name string
		page corepageauthz.Page
	}{{held, corepageauthz.PageContainers}, {free, corepageauthz.PagePorts}} {
		if err := f.pages.CreateRoleAccess(ctx, b.name, "integration-test", now); err != nil {
			f.t.Fatalf("CreateRoleAccess: %v", err)
		}
		if err := f.pages.AddPageToRoleAccess(ctx, b.name, b.page); err != nil {
			f.t.Fatalf("AddPageToRoleAccess: %v", err)
		}
	}
	if err := f.pages.AssignRoleAccess(ctx, corepageauthz.RoleAccessAssignmentSpec{
		UserID: userID, RoleAccessName: held, FleetWide: true, GrantedBy: "integration-test",
	}, now); err != nil {
		f.t.Fatalf("AssignRoleAccess: %v", err)
	}

	gen, err := coreuser.NewSession()
	if err != nil {
		f.t.Fatalf("NewSession: %v", err)
	}
	if err := f.users.CreateSession(ctx, coreuser.Session{
		TokenHash: gen.Hash, UserID: userID, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}); err != nil {
		f.t.Fatalf("CreateSession: %v", err)
	}
	if disabled {
		if _, err := f.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, userID); err != nil {
			f.t.Fatalf("disable target: %v", err)
		}
	}

	tu := targetUser{id: userID, assignBundle: free, grantablePage: corepageauthz.PageDisks}
	q := func(sql string, dest *string, args ...any) {
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(dest); err != nil {
			f.t.Fatalf("%s: %v", sql, err)
		}
	}
	q(`SELECT id FROM user_node_roles WHERE user_id = $1 AND role_name = 'admin' AND revoked_at IS NULL`, &tu.adminGrant, userID)
	if superadmin {
		q(`SELECT id FROM user_node_roles WHERE user_id = $1 AND role_name = 'superadmin' AND revoked_at IS NULL`, &tu.superGrant, userID)
	}
	q(`SELECT id FROM user_role_access WHERE user_id = $1 AND revoked_at IS NULL`, &tu.assignment, userID)
	q(`SELECT id FROM user_page_access WHERE user_id = $1 AND page_key = 'users' AND revoked_at IS NULL`, &tu.pageGrant, userID)
	st, err := f.users.UserAccess(ctx, userID)
	if err != nil {
		f.t.Fatalf("UserAccess: %v", err)
	}
	tu.accessVersion = st.Version()
	return tu
}

// userState is everything an action against userID could change, read back
// from the database.
type userState struct {
	account, roles, bundles, pages string
	liveSessions                   int
	auditAsTarget, auditAsActor    int
}

func (f *protectionFixture) state(userID, actorID string) userState {
	f.t.Helper()
	const q = `
		SELECT
			(SELECT password_hash || '|' || coalesce(disabled_at::text, '') || '|' || updated_at::text FROM users WHERE id = $1),
			(SELECT coalesce(string_agg(id || ':' || role_name || ':' || coalesce(node_id, '*') || ':' || coalesce(revoked_at::text, ''), ',' ORDER BY id), '')
			   FROM user_node_roles WHERE user_id = $1),
			(SELECT coalesce(string_agg(id || ':' || role_access_name || ':' || coalesce(node_id, '*') || ':' || coalesce(revoked_at::text, ''), ',' ORDER BY id), '')
			   FROM user_role_access WHERE user_id = $1),
			(SELECT coalesce(string_agg(id || ':' || page_key || ':' || coalesce(node_id, '*') || ':' || coalesce(revoked_at::text, ''), ',' ORDER BY id), '')
			   FROM user_page_access WHERE user_id = $1),
			(SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL),
			(SELECT count(*) FROM user_audit_log WHERE target_user_id = $1),
			(SELECT count(*) FROM user_audit_log WHERE actor_user_id = $2)`
	var s userState
	if err := f.pool.QueryRow(context.Background(), q, userID, actorID).Scan(
		&s.account, &s.roles, &s.bundles, &s.pages, &s.liveSessions, &s.auditAsTarget, &s.auditAsActor,
	); err != nil {
		f.t.Fatalf("read state: %v", err)
	}
	return s
}

func (f *protectionFixture) do(client *http.Client, method, path, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.base+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatalf("new request: %v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

type protectedAction struct {
	name           string
	method         string
	path           func(targetUser) string
	body           func(targetUser) string
	okStatus       int
	readOnly       bool // succeeds without changing state
	superadminOnly bool // only meaningful against a superadmin target
	needsDisabled  bool // the target must start disabled for the action to change anything
}

func protectedActions() []protectedAction {
	none := func(targetUser) string { return "" }
	return []protectedAction{
		{name: "grant role", method: http.MethodPost, okStatus: http.StatusNoContent,
			path: func(u targetUser) string { return "/users/" + u.id + "/grants" },
			body: func(targetUser) string { return `{"role":"viewer","fleet_wide":true}` }},
		{name: "revoke admin grant", method: http.MethodDelete, okStatus: http.StatusNoContent, body: none,
			path: func(u targetUser) string { return "/users/" + u.id + "/grants/" + u.adminGrant }},
		{name: "revoke superadmin grant", method: http.MethodDelete, okStatus: http.StatusNoContent, body: none, superadminOnly: true,
			path: func(u targetUser) string { return "/users/" + u.id + "/grants/" + u.superGrant }},
		{name: "disable", method: http.MethodPost, okStatus: http.StatusNoContent, body: none,
			path: func(u targetUser) string { return "/users/" + u.id + "/disable" }},
		{name: "enable", method: http.MethodPost, okStatus: http.StatusNoContent, body: none, needsDisabled: true,
			path: func(u targetUser) string { return "/users/" + u.id + "/enable" }},
		{name: "reset password", method: http.MethodPost, okStatus: http.StatusOK, body: none,
			path: func(u targetUser) string { return "/users/" + u.id + "/reset-password" }},
		{name: "force logout", method: http.MethodPost, okStatus: http.StatusNoContent, body: none,
			path: func(u targetUser) string { return "/users/" + u.id + "/force-logout" }},
		{name: "view audit", method: http.MethodGet, okStatus: http.StatusOK, body: none, readOnly: true,
			path: func(u targetUser) string { return "/users/" + u.id + "/audit" }},
		{name: "assign bundle", method: http.MethodPost, okStatus: http.StatusNoContent,
			path: func(u targetUser) string { return "/users/" + u.id + "/role-access" },
			body: func(u targetUser) string { return `{"role_access_name":"` + u.assignBundle + `","fleet_wide":true}` }},
		{name: "revoke bundle", method: http.MethodDelete, okStatus: http.StatusNoContent, body: none,
			path: func(u targetUser) string { return "/users/" + u.id + "/role-access/" + u.assignment }},
		{name: "grant page", method: http.MethodPost, okStatus: http.StatusNoContent,
			path: func(u targetUser) string { return "/users/" + u.id + "/page-access" },
			body: func(u targetUser) string { return `{"page":"` + string(u.grantablePage) + `","fleet_wide":true}` }},
		{name: "revoke page", method: http.MethodDelete, okStatus: http.StatusNoContent, body: none,
			path: func(u targetUser) string { return "/users/" + u.id + "/page-access/" + u.pageGrant }},
		// Keeps the admin role and every direct page except users: exactly
		// one revoke when it is allowed.
		{name: "edit access", method: http.MethodPut, okStatus: http.StatusOK,
			path: func(u targetUser) string { return "/users/" + u.id + "/access" },
			body: func(u targetUser) string {
				return `{"version":"` + u.accessVersion + `","roles":[{"role":"admin","fleet_wide":true}],"pages":[` +
					`{"page":"overview","fleet_wide":true},{"page":"nodes","fleet_wide":true},{"page":"processes","fleet_wide":true},` +
					`{"page":"services","fleet_wide":true},{"page":"cron","fleet_wide":true},{"page":"ports","fleet_wide":true}]}`
			}},
	}
}

func TestAdminCannotChangeASuperadmin(t *testing.T) {
	f := newProtectionFixture(t)
	fleetAdmin, fleetAdminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	nodeAdmin, nodeAdminID := f.actor("node-admin", roleGrant{role: coreuser.RoleAdmin, nodeID: "saprot-node-" + id.New()})

	callers := []struct {
		name    string
		client  *http.Client
		actorID string
		message string // required in the 403 body; "" when the refusal is the ordinary permission check
	}{
		{"fleet-wide admin", fleetAdmin, fleetAdminID, protectedSuperadminMessage},
		{"node-scoped admin", nodeAdmin, nodeAdminID, ""},
	}
	for _, c := range callers {
		for _, a := range protectedActions() {
			t.Run(c.name+"/"+a.name, func(t *testing.T) {
				target := f.target(true, a.needsDisabled)
				before := f.state(target.id, c.actorID)
				code, body := f.do(c.client, a.method, a.path(target), a.body(target))
				if code != http.StatusForbidden {
					t.Errorf("status = %d, want 403; body %s", code, body)
				}
				if c.message != "" && !strings.Contains(body, c.message) {
					t.Errorf("body = %s, want it to contain %q", body, c.message)
				}
				if after := f.state(target.id, c.actorID); after != before {
					t.Errorf("superadmin state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
				}
			})
		}
	}
}

// The guard's precondition is "target holds superadmin": admin-on-admin and
// superadmin-on-admin succeed and actually change the target. There is no
// superadmin-on-another-superadmin pair: migrations/0022_single_superadmin.sql
// makes a second superadmin impossible (the guard-logic pin lives in
// internal/api/v1's TestSuperadminCanActOnAnotherSuperadmin).
func TestProtectionLeavesOtherPairsWorking(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	super, superID := f.actor("superadmin", roleGrant{role: coreuser.RoleSuperadmin})

	pairs := []struct {
		name             string
		client           *http.Client
		actorID          string
		targetSuperadmin bool
	}{
		{"admin on admin", admin, adminID, false},
		{"superadmin on admin", super, superID, false},
	}
	for _, p := range pairs {
		for _, a := range protectedActions() {
			if a.superadminOnly && !p.targetSuperadmin {
				continue
			}
			t.Run(p.name+"/"+a.name, func(t *testing.T) {
				target := f.target(p.targetSuperadmin, a.needsDisabled)
				before := f.state(target.id, p.actorID)
				code, body := f.do(p.client, a.method, a.path(target), a.body(target))
				if code != a.okStatus {
					t.Fatalf("status = %d, want %d; body %s", code, a.okStatus, body)
				}
				after := f.state(target.id, p.actorID)
				if a.readOnly {
					if after != before {
						t.Errorf("read-only action changed state:\nbefore %+v\nafter  %+v", before, after)
					}
					return
				}
				if after == before {
					t.Error("action reported success but changed nothing")
				}
				if after.auditAsTarget != before.auditAsTarget+1 {
					t.Errorf("audit rows for target %d -> %d, want +1", before.auditAsTarget, after.auditAsTarget)
				}
			})
		}
	}
}

// A revoke whose id belongs to a superadmin but is addressed under another
// user's path is indistinguishable from an unknown id — same status, same
// body — and changes neither user.
func TestRevokeWithAnotherUsersIDBehavesLikeUnknownID(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	super := f.target(true, false)
	plain := f.target(false, false)

	cases := []struct {
		name, segment, superID string
	}{
		{"role grant", "grants", super.superGrant},
		{"bundle assignment", "role-access", super.assignment},
		{"page grant", "page-access", super.pageGrant},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			superBefore := f.state(super.id, adminID)
			plainBefore := f.state(plain.id, adminID)

			mCode, mBody := f.do(admin, http.MethodDelete, "/users/"+plain.id+"/"+c.segment+"/"+c.superID, "")
			uCode, uBody := f.do(admin, http.MethodDelete, "/users/"+plain.id+"/"+c.segment+"/no-such-"+id.New(), "")
			if mCode != uCode || mBody != uBody {
				t.Errorf("mismatched id = %d %q, unknown id = %d %q; want identical", mCode, mBody, uCode, uBody)
			}
			if mCode != http.StatusNoContent {
				t.Errorf("status = %d, want 204 (the unknown-id no-op)", mCode)
			}
			if after := f.state(super.id, adminID); after != superBefore {
				t.Errorf("superadmin changed through another user's path:\nbefore %+v\nafter  %+v", superBefore, after)
			}
			if after := f.state(plain.id, adminID); after != plainBefore {
				t.Errorf("path user changed:\nbefore %+v\nafter  %+v", plainBefore, after)
			}
		})
	}
}

// The API never revokes the superadmin grant or disables the superadmin's
// account — not even at the superadmin's own request, which protectTarget
// would otherwise let through as a self-action. Nothing changes.
func TestTheAPINeverRevokesOrDisablesTheSuperadmin(t *testing.T) {
	f := newProtectionFixture(t)
	super, superID := f.actor("superadmin", roleGrant{role: coreuser.RoleSuperadmin})
	var grantID string
	if err := f.pool.QueryRow(context.Background(), `
		SELECT id FROM user_node_roles WHERE user_id = $1 AND role_name = 'superadmin' AND revoked_at IS NULL`,
		superID).Scan(&grantID); err != nil {
		t.Fatalf("lookup superadmin grant: %v", err)
	}

	for _, c := range []struct {
		name, method, path, message string
	}{
		{"revoke own superadmin grant", http.MethodDelete, "/users/" + superID + "/grants/" + grantID,
			"the superadmin role cannot be revoked through the API"},
		{"disable own account", http.MethodPost, "/users/" + superID + "/disable",
			"you cannot disable your own account"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := f.state(superID, superID)
			code, body := f.do(super, c.method, c.path, "")
			if code != http.StatusBadRequest || !strings.Contains(body, c.message) {
				t.Errorf("status = %d, body %s; want 400 containing %q", code, body, c.message)
			}
			if after := f.state(superID, superID); after != before {
				t.Errorf("superadmin changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}
