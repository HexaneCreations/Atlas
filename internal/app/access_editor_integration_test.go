//go:build integration

// The access editor (GET/PUT /users/{userID}/access, and POST /users with an
// access body) against real Postgres through the real HTTP chain. The
// superadmin-target refusals are part of the shared matrix in
// superadmin_protection_integration_test.go ("edit access"); this file
// covers the editor's own rules. Every refusal is checked for "no change"
// with protectionFixture.state, which includes audit row counts.
package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/hexane/atlas/internal/api/v1"
	corepageauthz "github.com/hexane/atlas/internal/core/pageauthz"
	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/id"
)

// node inserts a nodes row for the duration of the test; the editor only
// adds grants on nodes that exist.
func (f *protectionFixture) node() string {
	f.t.Helper()
	nodeID := "it-access-node-" + id.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO nodes (node_id, hostname) VALUES ($1, $1)`, nodeID); err != nil {
		f.t.Fatalf("insert node: %v", err)
	}
	f.t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DELETE FROM nodes WHERE node_id = $1`, nodeID)
	})
	return nodeID
}

func (f *protectionFixture) getAccess(client *http.Client, userID string) v1.UserAccessResponse {
	f.t.Helper()
	code, body := f.do(client, http.MethodGet, "/users/"+userID+"/access", "")
	if code != http.StatusOK {
		f.t.Fatalf("GET access: status = %d, body %s", code, body)
	}
	var out v1.UserAccessResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		f.t.Fatalf("decode access: %v", err)
	}
	return out
}

func (f *protectionFixture) putAccess(client *http.Client, userID, version, roles, pages string) (int, string) {
	f.t.Helper()
	return f.do(client, http.MethodPut, "/users/"+userID+"/access",
		`{"version":"`+version+`","roles":`+roles+`,"pages":`+pages+`}`)
}

// editorAudit returns the actions of userID's audit rows tagged
// via=access_editor, sorted.
func (f *protectionFixture) editorAudit(userID string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT action || ' ' || coalesce(detail->>'role', detail->>'page') || ' ' || coalesce(detail->>'node_id', '*')
		FROM user_audit_log WHERE target_user_id = $1 AND detail->>'via' = 'access_editor'`, userID)
	if err != nil {
		f.t.Fatalf("read audit: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatalf("scan audit: %v", err)
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

// activeRows renders userID's active editor rows as sorted "role n1 viewer"
// / "page * ports" lines.
func (f *protectionFixture) activeRows(userID string) []string {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT 'role ' || coalesce(node_id, '*') || ' ' || role_name FROM user_node_roles WHERE user_id = $1 AND revoked_at IS NULL
		UNION ALL
		SELECT 'page ' || coalesce(node_id, '*') || ' ' || page_key FROM user_page_access WHERE user_id = $1 AND revoked_at IS NULL`, userID)
	if err != nil {
		f.t.Fatalf("read rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatalf("scan rows: %v", err)
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func (f *protectionFixture) grantNodePage(userID, nodeID string, page corepageauthz.Page) {
	f.t.Helper()
	if err := f.pages.GrantPageAccess(context.Background(), corepageauthz.PageGrantSpec{
		UserID: userID, Page: page, NodeID: nodeID, GrantedBy: "integration-test",
	}, time.Now()); err != nil {
		f.t.Fatalf("GrantPageAccess: %v", err)
	}
}

func (f *protectionFixture) username(userID string) string {
	f.t.Helper()
	var u string
	if err := f.pool.QueryRow(context.Background(), `SELECT username FROM users WHERE id = $1`, userID).Scan(&u); err != nil {
		f.t.Fatalf("lookup username: %v", err)
	}
	return u
}

func replaceNode(s, nodeID string) string { return strings.ReplaceAll(s, "N1", nodeID) }

// An admin edits a viewer: one audit row per change, each tagged
// via=access_editor; the response equals a fresh GET; a repeat of the same
// request is a no-op that writes no audit rows.
func TestAccessEditorAdminOnViewer(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	n1 := f.node()
	viewerID, _ := f.createUser("viewer", []roleGrant{{role: coreuser.RoleViewer, nodeID: n1}}, nil)
	f.grantNodePage(viewerID, n1, corepageauthz.PageDisks)

	before := f.state(viewerID, adminID)
	got := f.getAccess(admin, viewerID)
	if len(got.Roles) != 1 || got.Roles[0].Role != "viewer" || got.Roles[0].NodeID != n1 || len(got.Pages) != 1 {
		t.Fatalf("GET access = %+v", got)
	}

	roles := replaceNode(`[{"role":"operator","node_id":"N1"}]`, n1)
	pages := replaceNode(`[{"page":"overview","fleet_wide":true},{"page":"ports","node_id":"N1"}]`, n1)
	code, body := f.putAccess(admin, viewerID, got.Version, roles, pages)
	if code != http.StatusOK {
		t.Fatalf("PUT: status = %d, body %s", code, body)
	}
	var resp v1.PutUserAccessResponse
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Changes.Granted) != 3 || len(resp.Changes.Revoked) != 2 {
		t.Errorf("changes = %+v, want 3 granted, 2 revoked", resp.Changes)
	}

	wantAudit := []string{
		"grant_page_access overview *", "grant_page_access ports " + n1, "grant_role operator " + n1,
		"revoke_page_access disks " + n1, "revoke_role viewer " + n1,
	}
	slices.Sort(wantAudit)
	if a := f.editorAudit(viewerID); !slices.Equal(a, wantAudit) {
		t.Errorf("audit =\n  %v\nwant\n  %v", a, wantAudit)
	}
	after := f.state(viewerID, adminID)
	if after.auditAsTarget != before.auditAsTarget+5 {
		t.Errorf("audit rows %d -> %d, want +5", before.auditAsTarget, after.auditAsTarget)
	}
	wantRows := []string{"page * overview", "page " + n1 + " ports", "role " + n1 + " operator"}
	slices.Sort(wantRows)
	if rows := f.activeRows(viewerID); !slices.Equal(rows, wantRows) {
		t.Errorf("rows = %v, want %v", rows, wantRows)
	}

	fresh := f.getAccess(admin, viewerID)
	if fresh.Version != resp.Access.Version || fresh.Version == got.Version {
		t.Errorf("versions: before %s, PUT response %s, fresh GET %s", got.Version, resp.Access.Version, fresh.Version)
	}

	t.Run("a repeat is a no-op", func(t *testing.T) {
		mid := f.state(viewerID, adminID)
		code, body := f.putAccess(admin, viewerID, fresh.Version, roles, pages)
		if code != http.StatusOK || !strings.Contains(body, `"changes":{"granted":[],"revoked":[]}`) {
			t.Fatalf("status = %d, body %s; want 200 with no changes", code, body)
		}
		if end := f.state(viewerID, adminID); end != mid {
			t.Errorf("a no-op PUT changed state:\nbefore %+v\nafter  %+v", mid, end)
		}
	})
}

// Removing the last enabled fleet-wide admin is refused and nothing in the
// request is applied, including its unrelated parts.
func TestAccessEditorLastAdminRollsBack(t *testing.T) {
	f := newProtectionFixture(t)
	super, superID := f.actor("superadmin", roleGrant{role: coreuser.RoleSuperadmin})
	targetID, _ := f.createUser("last-admin", []roleGrant{{role: coreuser.RoleAdmin}}, nil)
	revokeOtherFleetAdmins(t, f, targetID)

	before := f.state(targetID, superID)
	version := f.getAccess(super, targetID).Version
	code, body := f.putAccess(super, targetID, version, `[{"role":"viewer","fleet_wide":true}]`, `[{"page":"overview","fleet_wide":true}]`)
	if code != http.StatusPreconditionFailed || !strings.Contains(body, "last user with fleet-wide admin access") {
		t.Errorf("status = %d, body %s; want 412 last-admin", code, body)
	}
	if after := f.state(targetID, superID); after != before {
		t.Errorf("state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
	}
}

// revokeOtherFleetAdmins leaves keepUserID the only fleet-wide admin. This
// package's tests share one database and run sequentially, so the admins
// revoked are ones earlier tests left behind — see releaseSuperadminSlot.
func revokeOtherFleetAdmins(t *testing.T, f *protectionFixture, keepUserID string) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), `
		UPDATE user_node_roles SET revoked_at = now(), revoked_by = 'integration-test'
		WHERE role_name = 'admin' AND node_id IS NULL AND revoked_at IS NULL AND user_id <> $1`, keepUserID); err != nil {
		t.Fatalf("revoke other admins: %v", err)
	}
}

// A change through any other path (here POST /grants) makes a version read
// before it stale: 409 and nothing applied.
func TestAccessEditorStaleVersion(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	targetID, _ := f.createUser("stale", []roleGrant{{role: coreuser.RoleViewer}}, nil)

	version := f.getAccess(admin, targetID).Version
	if code, body := f.do(admin, http.MethodPost, "/users/"+targetID+"/grants", `{"role":"operator","fleet_wide":true}`); code != http.StatusNoContent {
		t.Fatalf("grant: status = %d, body %s", code, body)
	}

	before := f.state(targetID, adminID)
	code, body := f.putAccess(admin, targetID, version, `[]`, `[]`)
	if code != http.StatusConflict || !strings.Contains(body, "access changed since you opened it") {
		t.Errorf("status = %d, body %s; want 409 stale", code, body)
	}
	if after := f.state(targetID, adminID); after != before {
		t.Errorf("state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
	}
}

// A direct page a bundle already covers is refused with 409; bundle pages
// are shown in GET but are never sent.
func TestAccessEditorRefusesABundlePage(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	target := f.target(false, false) // holds a fleet-wide bundle covering containers

	got := f.getAccess(admin, target.id)
	if len(got.Bundles) != 1 || !slices.Contains(got.Bundles[0].Pages, corepageauthz.PageContainers) || !got.Bundles[0].FleetWide {
		t.Fatalf("bundles = %+v, want one fleet-wide bundle with containers", got.Bundles)
	}

	before := f.state(target.id, adminID)
	code, body := f.putAccess(admin, target.id, got.Version, `[{"role":"admin","fleet_wide":true}]`,
		`[{"page":"overview","fleet_wide":true},{"page":"containers","fleet_wide":true}]`)
	if code != http.StatusConflict || !strings.Contains(body, "already granted by a role bundle") {
		t.Errorf("status = %d, body %s; want 409 bundle conflict", code, body)
	}
	if after := f.state(target.id, adminID); after != before {
		t.Errorf("state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
	}
}

// Rows the editor's model cannot show — two roles on one scope, a node role
// or node page under the fleet-wide one — are cleaned on save.
func TestAccessEditorCleansRedundantRows(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	n1 := f.node()
	targetID, _ := f.createUser("redundant", []roleGrant{
		{role: coreuser.RoleOperator}, {role: coreuser.RoleViewer, nodeID: n1}, {role: coreuser.RoleOperator, nodeID: n1},
	}, []corepageauthz.Page{corepageauthz.PagePorts})
	f.grantNodePage(targetID, n1, corepageauthz.PagePorts)

	before := f.state(targetID, adminID)
	version := f.getAccess(admin, targetID).Version
	code, body := f.putAccess(admin, targetID, version,
		replaceNode(`[{"role":"operator","fleet_wide":true},{"role":"viewer","node_id":"N1"}]`, n1),
		replaceNode(`[{"page":"ports","fleet_wide":true},{"page":"ports","node_id":"N1"}]`, n1))
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s", code, body)
	}
	if rows := f.activeRows(targetID); !slices.Equal(rows, []string{"page * ports", "role * operator"}) {
		t.Errorf("rows = %v, want only the fleet-wide operator role and ports page", rows)
	}
	if after := f.state(targetID, adminID); after.auditAsTarget != before.auditAsTarget+3 {
		t.Errorf("audit rows %d -> %d, want +3 (two node roles, one node page)", before.auditAsTarget, after.auditAsTarget)
	}
}

// Requests the editor refuses with 400 change nothing.
func TestAccessEditorRefusesInconsistentRequests(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	n1 := f.node()
	targetID, _ := f.createUser("refused", []roleGrant{{role: coreuser.RoleViewer, nodeID: n1}}, nil)

	for _, c := range []struct {
		name, roles, pages, message string
	}{
		{"page on a node without a role there", replaceNode(`[{"role":"viewer","node_id":"N1"}]`, n1),
			`[{"page":"ports","node_id":"` + f.node() + `"}]`, "need a role"},
		{"overview with no role", `[]`, `[{"page":"overview","fleet_wide":true}]`, "need a role"},
		{"unknown node", `[{"role":"viewer","node_id":"it-no-such-node"}]`, `[]`, "unknown node"},
		{"superadmin role", `[{"role":"superadmin","fleet_wide":true}]`, `[]`, "superadmin"},
		{"fleet-only page on a node", replaceNode(`[{"role":"viewer","node_id":"N1"}]`, n1),
			replaceNode(`[{"page":"nodes","node_id":"N1"}]`, n1), "no per-node concept"},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := f.state(targetID, adminID)
			code, body := f.putAccess(admin, targetID, f.getAccess(admin, targetID).Version, c.roles, c.pages)
			if code != http.StatusBadRequest || !strings.Contains(body, c.message) {
				t.Errorf("status = %d, body %s; want 400 containing %q", code, body, c.message)
			}
			if after := f.state(targetID, adminID); after != before {
				t.Errorf("state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// Nobody edits their own access, the superadmin included; GET of one's own
// access still works.
func TestAccessEditorRefusesSelfEdit(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	super, superID := f.actor("superadmin", roleGrant{role: coreuser.RoleSuperadmin})

	for _, c := range []struct {
		name    string
		client  *http.Client
		actorID string
	}{{"admin", admin, adminID}, {"superadmin", super, superID}} {
		t.Run(c.name, func(t *testing.T) {
			before := f.state(c.actorID, c.actorID)
			version := f.getAccess(c.client, c.actorID).Version
			code, body := f.putAccess(c.client, c.actorID, version, `[]`, `[]`)
			if code != http.StatusForbidden || !strings.Contains(body, "you cannot change your own access") {
				t.Errorf("status = %d, body %s; want 403 own-access", code, body)
			}
			if after := f.state(c.actorID, c.actorID); after != before {
				t.Errorf("state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}

// GET is exempt from protectTarget: an admin reads the superadmin's access,
// and the superadmin grant itself is not part of it.
func TestAccessEditorGetOmitsTheSuperadminGrant(t *testing.T) {
	f := newProtectionFixture(t)
	admin, _ := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	super := f.target(true, false)

	got := f.getAccess(admin, super.id)
	for _, r := range got.Roles {
		if r.Role == coreuser.RoleSuperadmin {
			t.Errorf("GET access returned the superadmin grant: %+v", r)
		}
	}
	if len(got.Roles) != 1 || got.Roles[0].Role != coreuser.RoleAdmin {
		t.Errorf("roles = %+v, want only the admin grant", got.Roles)
	}
}

// POST /users with an access body creates the user and their access in one
// transaction: a refused body leaves no user and no audit row behind.
func TestCreateUserWithAccessIsAtomic(t *testing.T) {
	f := newProtectionFixture(t)
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	n1 := f.node()

	countActor := func() int {
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM user_audit_log WHERE actor_user_id = $1`, adminID).Scan(&n); err != nil {
			t.Fatalf("count audit: %v", err)
		}
		return n
	}

	t.Run("refused", func(t *testing.T) {
		username := "it-access-refused-" + id.New()
		auditBefore := countActor()
		code, body := f.do(admin, http.MethodPost, "/users", `{"username":"`+username+`","access":{"roles":[],"pages":[{"page":"overview","fleet_wide":true}]}}`)
		if code != http.StatusBadRequest {
			t.Errorf("status = %d, body %s; want 400", code, body)
		}
		var exists bool
		if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM users WHERE username = $1)`, username).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Error("the user was created despite the refused access body")
		}
		if n := countActor(); n != auditBefore {
			t.Errorf("audit rows by the actor %d -> %d, want unchanged", auditBefore, n)
		}
	})

	t.Run("applied", func(t *testing.T) {
		username := "it-access-created-" + id.New()
		code, body := f.do(admin, http.MethodPost, "/users", replaceNode(`{"username":"`+username+
			`","access":{"roles":[{"role":"viewer","node_id":"N1"}],"pages":[{"page":"overview","fleet_wide":true},{"page":"ports","node_id":"N1"}]}}`, n1))
		if code != http.StatusCreated {
			t.Fatalf("status = %d, body %s", code, body)
		}
		var resp v1.CreateUserResponse
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		want := []string{"page * overview", "page " + n1 + " ports", "role " + n1 + " viewer"}
		slices.Sort(want)
		if rows := f.activeRows(resp.User.ID); !slices.Equal(rows, want) {
			t.Errorf("rows = %v, want %v", rows, want)
		}
		if a := f.editorAudit(resp.User.ID); len(a) != 3 {
			t.Errorf("editor audit = %v, want 3 rows", a)
		}
		if f.username(resp.User.ID) != username {
			t.Error("created user has the wrong username")
		}
	})
}
