package v1_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/access"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
)

const adminOnlyMessage = "admin accounts can only be changed by a superadmin"

// seedAdminAccess gives secondAdmin an editor state holding the fleet-wide
// admin role, and returns its version.
func seedAdminAccess(admin *fakeUserAdmin) string {
	st := access.State{Roles: []access.RoleGrant{{
		ID:   secondAdminGrant,
		Role: access.Role{Role: user.RoleAdmin, Scope: pageauthz.Scope{FleetWide: true}}, GrantedAt: time.Now(),
	}}}
	admin.access[secondAdminID] = st
	return st.Version()
}

func adminOnlyActions(version string) []guardAction {
	return []guardAction{
		{"grant fleet-wide admin", http.MethodPost, "/users/" + secondAdminID + "/grants", `{"role":"admin","fleet_wide":true}`},
		{"grant node-scoped admin", http.MethodPost, "/users/" + secondAdminID + "/grants", `{"role":"admin","node_id":"n1"}`},
		{"revoke admin", http.MethodDelete, "/users/" + secondAdminID + "/grants/" + secondAdminGrant, ""},
		{"disable admin", http.MethodPost, "/users/" + secondAdminID + "/disable", ""},
		{"enable admin", http.MethodPost, "/users/" + secondAdminID + "/enable", ""},
		{"reset admin password", http.MethodPost, "/users/" + secondAdminID + "/reset-password", ""},
		{"editor removes admin", http.MethodPut, "/users/" + secondAdminID + "/access",
			`{"version":"` + version + `","roles":[{"role":"operator","fleet_wide":true}],"pages":[]}`},
		{"editor swaps fleet-wide admin for node admin", http.MethodPut, "/users/" + secondAdminID + "/access",
			`{"version":"` + version + `","roles":[{"role":"operator","fleet_wide":true},{"role":"admin","node_id":"n1"}],"pages":[]}`},
		{"create user with admin", http.MethodPost, "/users",
			`{"username":"new","access":{"roles":[{"role":"admin","node_id":"n1"}],"pages":[]}}`},
	}
}

func TestAdminCannotTakeAdminOnlyActions(t *testing.T) {
	t.Parallel()
	for _, a := range adminOnlyActions("") {
		t.Run(a.name, func(t *testing.T) {
			base, admin, client := newSuperadminGuardServer(t, false)
			if strings.Contains(a.body, `"version"`) {
				a.body = strings.Replace(a.body, `"version":""`, `"version":"`+seedAdminAccess(admin)+`"`, 1)
			}
			grants := len(admin.grants)
			code, body := doBody(t, client, base, a)
			if code != http.StatusForbidden || !strings.Contains(body, adminOnlyMessage) {
				t.Errorf("status = %d, body %s; want 403 %q", code, body, adminOnlyMessage)
			}
			if len(admin.grants) != grants || admin.grants[secondAdminGrant].RevokedAt != nil ||
				admin.users[secondAdminID].Disabled() || admin.replaced[secondAdminID] != 0 || len(admin.createdWith) != 0 {
				t.Error("the store was changed despite the refusal")
			}
		})
	}
}

// A superadmin is never refused, and every request reaches storage.
func TestSuperadminTakesAdminOnlyActions(t *testing.T) {
	t.Parallel()
	for _, a := range adminOnlyActions("") {
		t.Run(a.name, func(t *testing.T) {
			base, admin, client := newSuperadminGuardServer(t, true)
			if strings.Contains(a.body, `"version"`) {
				a.body = strings.Replace(a.body, `"version":""`, `"version":"`+seedAdminAccess(admin)+`"`, 1)
			}
			if code, body := doBody(t, client, base, a); code >= 400 {
				t.Errorf("status = %d, body %s; want success", code, body)
			}
		})
	}
}

// Editing an admin's non-admin access stays allowed for an admin, and a
// stale version is still a 409, not a 403.
func TestAdminEditsAnAdminsOtherAccess(t *testing.T) {
	t.Parallel()
	base, admin, client := newSuperadminGuardServer(t, false)
	version := seedAdminAccess(admin)

	code, body := doBody(t, client, base, guardAction{method: http.MethodPut, path: "/users/" + secondAdminID + "/access",
		body: `{"version":"` + version + `","roles":[{"role":"admin","fleet_wide":true}],"pages":[{"page":"ports","fleet_wide":true}]}`})
	if code != http.StatusOK {
		t.Errorf("status = %d, body %s; want 200", code, body)
	}
	code, body = doBody(t, client, base, guardAction{method: http.MethodPut, path: "/users/" + secondAdminID + "/access",
		body: `{"version":"stale","roles":[],"pages":[]}`})
	if code != http.StatusConflict {
		t.Errorf("stale: status = %d, body %s; want 409", code, body)
	}
}

// The self-action refusal answers before the admin guard.
func TestSelfRefusalFiresBeforeTheAdminGuard(t *testing.T) {
	t.Parallel()
	base, admin, client := newSuperadminGuardServer(t, false)
	var actorID string
	for id, u := range admin.users {
		if u.Username == "guarded-actor" {
			actorID = id
		}
	}
	admin.grants["g-actor-admin"] = user.NodeRole{ID: "g-actor-admin", UserID: actorID, Role: user.RoleAdmin}

	code, body := doBody(t, client, base, guardAction{method: http.MethodPost, path: "/users/" + actorID + "/disable"})
	if code != http.StatusBadRequest || !strings.Contains(body, "you cannot disable your own account") {
		t.Errorf("status = %d, body %s; want 400 self-disable", code, body)
	}
}
