package v1_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/core/user"
)

// The superadmin tier: a protected fourth role that ordinary fleet-wide
// admins cannot act on, and that no API caller can grant. These tests pin
// the handler-level guard (internal/api/v1.Handler.guardSuperadminTarget)
// and the grant-role rejection; the storage-layer round-trip and the
// last-admin-guard interaction are proven directly against Postgres in
// internal/storage/user/repository_integration_test.go.

const (
	superadminUserID = "super-1"
	secondAdminID    = "admin-2"
	secondAdminGrant = "g-admin2"
	superadminGrant  = "g-super"
)

// newSuperadminGuardServer wires an admin Users stack where "super-1" holds
// a fleet-wide superadmin grant and "admin-2" a fleet-wide admin grant. The
// logged-in actor is a plain fleet-wide admin unless actorIsSuperadmin, in
// which case they hold a superadmin grant too.
func newSuperadminGuardServer(t *testing.T, actorIsSuperadmin bool) (string, *fakeUserAdmin, *http.Client) {
	t.Helper()

	actor := testUser(t, "guarded-actor", "correct horse battery staple")
	admin := newFakeUserAdmin(actor,
		user.User{ID: superadminUserID, Username: "the-superadmin"},
		user.User{ID: secondAdminID, Username: "second-admin"},
	)
	admin.grants[superadminGrant] = user.NodeRole{ID: superadminGrant, UserID: superadminUserID, Role: user.RoleSuperadmin}
	admin.grants[secondAdminGrant] = user.NodeRole{ID: secondAdminGrant, UserID: secondAdminID, Role: user.RoleAdmin}
	if actorIsSuperadmin {
		admin.grants["g-actor"] = user.NodeRole{ID: "g-actor", UserID: actor.ID, Role: user.RoleSuperadmin}
	}

	srv := newUserAdminTestServer(t, actor, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "guarded-actor", "correct horse battery staple").Body.Close()
	return srv.URL, admin, client
}

type guardAction struct {
	name   string
	method string
	path   string // relative to <base>/api/v1
	body   string
}

// targetActions is the complete set of per-user actions the guard covers —
// every target-specific handler in users.go. ListUsers is deliberately not
// here: the row stays visible, only actions on it are refused.
func targetActions(targetID, grantID string) []guardAction {
	return []guardAction{
		{"grant role", http.MethodPost, "/users/" + targetID + "/grants", `{"role":"viewer","fleet_wide":true}`},
		{"revoke role", http.MethodDelete, "/users/" + targetID + "/grants/" + grantID, ""},
		{"disable", http.MethodPost, "/users/" + targetID + "/disable", ""},
		{"enable", http.MethodPost, "/users/" + targetID + "/enable", ""},
		{"reset password", http.MethodPost, "/users/" + targetID + "/reset-password", ""},
		{"force logout", http.MethodPost, "/users/" + targetID + "/force-logout", ""},
		{"view audit", http.MethodGet, "/users/" + targetID + "/audit", ""},
	}
}

func do(t *testing.T, client *http.Client, baseURL string, a guardAction) int {
	t.Helper()
	req, err := http.NewRequest(a.method, baseURL+"/api/v1"+a.path, strings.NewReader(a.body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if a.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// The superadmin role is never grantable through POST /users/{id}/grants —
// from a plain admin or from the superadmin itself. GrantSpec.Validate would
// accept it as a known role; the handler rejects it first with a 400 and
// never calls Grant.
func TestGrantRoleRejectsTheSuperadminRoleFromEveryCaller(t *testing.T) {
	t.Parallel()

	for _, actorIsSuperadmin := range []bool{false, true} {
		name := "plain admin caller"
		if actorIsSuperadmin {
			name = "superadmin caller"
		}
		t.Run(name, func(t *testing.T) {
			base, admin, client := newSuperadminGuardServer(t, actorIsSuperadmin)
			before := len(admin.grants)

			code := do(t, client, base, guardAction{
				method: http.MethodPost,
				path:   "/users/" + secondAdminID + "/grants",
				body:   `{"role":"superadmin","fleet_wide":true}`,
			})
			if code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (user.ErrSuperadminNotGrantable)", code)
			}
			if len(admin.grants) != before {
				t.Errorf("grants %d -> %d: the role must be refused before Grant is called", before, len(admin.grants))
			}
		})
	}
}

// A plain fleet-wide admin gets 403 on every per-user action against the
// superadmin's account, and nothing is mutated.
func TestNonSuperadminCannotActOnTheSuperadmin(t *testing.T) {
	t.Parallel()

	base, admin, client := newSuperadminGuardServer(t, false)
	for _, a := range targetActions(superadminUserID, superadminGrant) {
		t.Run(a.name, func(t *testing.T) {
			if code := do(t, client, base, a); code != http.StatusForbidden {
				t.Errorf("status = %d, want 403 (user.ErrProtectedSuperadmin)", code)
			}
		})
	}
	if admin.users[superadminUserID].Disabled() {
		t.Error("the superadmin was disabled despite the guard")
	}
	if _, revoked := admin.grants[superadminGrant]; !revoked {
		t.Error("the superadmin's grant map entry vanished")
	} else if admin.grants[superadminGrant].RevokedAt != nil {
		t.Error("the superadmin's grant was revoked despite the guard")
	}
}

// A superadmin actor performs every one of those actions against an admin
// exactly as an admin can today — the guard never fires for a superadmin
// caller. Fresh server per action so an earlier mutation cannot mask a
// later failure.
func TestSuperadminCanActOnAdmins(t *testing.T) {
	t.Parallel()

	for _, a := range targetActions(secondAdminID, secondAdminGrant) {
		t.Run(a.name, func(t *testing.T) {
			base, _, client := newSuperadminGuardServer(t, true)
			if code := do(t, client, base, a); code >= 400 {
				t.Errorf("status = %d, want success — a superadmin acts on an admin as admin does today", code)
			}
		})
	}
}

// Point 6: admin-vs-admin behaviour is unchanged. A plain fleet-wide admin
// acting on a different (non-superadmin) fleet-wide admin still succeeds on
// every action — the guard's precondition is specifically "target holds
// superadmin", which is not met here.
func TestAdminVsAdminActionsAreUnchanged(t *testing.T) {
	t.Parallel()

	for _, a := range targetActions(secondAdminID, secondAdminGrant) {
		t.Run(a.name, func(t *testing.T) {
			base, _, client := newSuperadminGuardServer(t, false)
			if code := do(t, client, base, a); code >= 400 {
				t.Errorf("status = %d, want success — admin-vs-admin actions must not change", code)
			}
		})
	}
}

// /auth/me reports whether the caller is the superadmin, so the frontend can
// disable per-row actions ahead of a 403. A display hint only — the guard
// re-checks on every action endpoint.
func TestAuthMeReportsSuperadminStatus(t *testing.T) {
	t.Parallel()

	for _, actorIsSuperadmin := range []bool{false, true} {
		name := "plain admin"
		if actorIsSuperadmin {
			name = "superadmin"
		}
		t.Run(name, func(t *testing.T) {
			base, _, client := newSuperadminGuardServer(t, actorIsSuperadmin)
			resp, err := client.Get(base + "/api/v1/auth/me")
			if err != nil {
				t.Fatalf("get /auth/me: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var body struct {
				IsSuperadmin bool `json:"is_superadmin"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.IsSuperadmin != actorIsSuperadmin {
				t.Errorf("is_superadmin = %v, want %v", body.IsSuperadmin, actorIsSuperadmin)
			}
		})
	}
}
