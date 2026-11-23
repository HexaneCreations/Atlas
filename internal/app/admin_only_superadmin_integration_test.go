//go:build integration

// "Admin-level changes belong only to the superadmin", against real Postgres
// through the real HTTP chain: an admin cannot grant or revoke the admin
// role (per-grant endpoints, the access editor, POST /users), nor disable,
// enable or reset the password of a user who holds it; everything else an
// admin does is unchanged, and the superadmin can do all of it. Every
// refusal is checked for "no change" with protectionFixture.state.
package app_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	coreuser "github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/id"
)

const adminOnlySuperadminMessage = "admin accounts can only be changed by a superadmin"

// plainTarget is a user holding exactly one role grant, optionally disabled.
type plainTarget struct {
	id, grant, version string
}

func (f *protectionFixture) plainTarget(role, nodeID string, disabled bool) plainTarget {
	f.t.Helper()
	ctx := context.Background()
	userID, _ := f.createUser("plain-"+role, []roleGrant{{role: role, nodeID: nodeID}}, nil)
	if disabled {
		if _, err := f.pool.Exec(ctx, `UPDATE users SET disabled_at = now() WHERE id = $1`, userID); err != nil {
			f.t.Fatalf("disable target: %v", err)
		}
	}
	pt := plainTarget{id: userID}
	if err := f.pool.QueryRow(ctx, `SELECT id FROM user_node_roles WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&pt.grant); err != nil {
		f.t.Fatalf("lookup grant: %v", err)
	}
	st, err := f.users.UserAccess(ctx, userID)
	if err != nil {
		f.t.Fatalf("UserAccess: %v", err)
	}
	pt.version = st.Version()
	return pt
}

// adminChange is one request touching the admin role or an admin account.
// setup builds a fresh target and returns its id and the request.
type adminChange struct {
	name  string
	setup func(f *protectionFixture, nodeID string) (targetID, method, path, body string)
}

func adminChanges() []adminChange {
	return []adminChange{
		{"grant fleet-wide admin", func(f *protectionFixture, _ string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleViewer, "", false)
			return t.id, http.MethodPost, "/users/" + t.id + "/grants", `{"role":"admin","fleet_wide":true}`
		}},
		{"grant node-scoped admin", func(f *protectionFixture, nodeID string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleViewer, "", false)
			return t.id, http.MethodPost, "/users/" + t.id + "/grants", `{"role":"admin","node_id":"` + nodeID + `"}`
		}},
		{"revoke fleet-wide admin", func(f *protectionFixture, _ string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleAdmin, "", false)
			return t.id, http.MethodDelete, "/users/" + t.id + "/grants/" + t.grant, ""
		}},
		{"revoke node-scoped admin", func(f *protectionFixture, nodeID string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleAdmin, nodeID, false)
			return t.id, http.MethodDelete, "/users/" + t.id + "/grants/" + t.grant, ""
		}},
		{"disable admin", func(f *protectionFixture, _ string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleAdmin, "", false)
			return t.id, http.MethodPost, "/users/" + t.id + "/disable", ""
		}},
		{"disable node-scoped admin", func(f *protectionFixture, nodeID string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleAdmin, nodeID, false)
			return t.id, http.MethodPost, "/users/" + t.id + "/disable", ""
		}},
		{"enable admin", func(f *protectionFixture, _ string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleAdmin, "", true)
			return t.id, http.MethodPost, "/users/" + t.id + "/enable", ""
		}},
		{"reset admin password", func(f *protectionFixture, _ string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleAdmin, "", false)
			return t.id, http.MethodPost, "/users/" + t.id + "/reset-password", ""
		}},
		{"editor adds fleet-wide admin", func(f *protectionFixture, _ string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleViewer, "", false)
			return t.id, http.MethodPut, "/users/" + t.id + "/access",
				`{"version":"` + t.version + `","roles":[{"role":"admin","fleet_wide":true}],"pages":[]}`
		}},
		{"editor adds node-scoped admin", func(f *protectionFixture, nodeID string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleViewer, "", false)
			return t.id, http.MethodPut, "/users/" + t.id + "/access",
				`{"version":"` + t.version + `","roles":[{"role":"viewer","fleet_wide":true},{"role":"admin","node_id":"` + nodeID + `"}],"pages":[]}`
		}},
		{"editor removes admin", func(f *protectionFixture, _ string) (string, string, string, string) {
			t := f.plainTarget(coreuser.RoleAdmin, "", false)
			return t.id, http.MethodPut, "/users/" + t.id + "/access",
				`{"version":"` + t.version + `","roles":[{"role":"operator","fleet_wide":true}],"pages":[]}`
		}},
	}
}

func TestAdminCannotChangeAdminAccess(t *testing.T) {
	f := newProtectionFixture(t)
	nodeID := f.node()
	f.actor("spare-admin", roleGrant{role: coreuser.RoleAdmin})
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})

	for _, c := range adminChanges() {
		t.Run(c.name, func(t *testing.T) {
			targetID, method, path, body := c.setup(f, nodeID)
			before := f.state(targetID, adminID)
			code, resp := f.do(admin, method, path, body)
			if code != http.StatusForbidden || !strings.Contains(resp, adminOnlySuperadminMessage) {
				t.Errorf("status = %d, body %s; want 403 %q", code, resp, adminOnlySuperadminMessage)
			}
			if after := f.state(targetID, adminID); after != before {
				t.Errorf("state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}

	t.Run("create user with admin", func(t *testing.T) {
		username := "it-adminonly-create-" + id.New()
		before := f.state(adminID, adminID)
		code, resp := f.do(admin, http.MethodPost, "/users",
			`{"username":"`+username+`","access":{"roles":[{"role":"admin","fleet_wide":true}],"pages":[]}}`)
		if code != http.StatusForbidden || !strings.Contains(resp, adminOnlySuperadminMessage) {
			t.Errorf("status = %d, body %s; want 403 %q", code, resp, adminOnlySuperadminMessage)
		}
		var n int
		if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM users WHERE username = $1`, username).Scan(&n); err != nil {
			t.Fatalf("count users: %v", err)
		}
		if n != 0 {
			t.Errorf("a user was created despite the refusal")
		}
		if after := f.state(adminID, adminID); after.auditAsActor != before.auditAsActor {
			t.Errorf("actor audit rows %d -> %d, want unchanged", before.auditAsActor, after.auditAsActor)
		}
	})
}

// The superadmin performs every one of those changes.
func TestSuperadminChangesAdminAccess(t *testing.T) {
	f := newProtectionFixture(t)
	nodeID := f.node()
	f.actor("spare-admin", roleGrant{role: coreuser.RoleAdmin})
	super, superID := f.actor("superadmin", roleGrant{role: coreuser.RoleSuperadmin})

	for _, c := range adminChanges() {
		t.Run(c.name, func(t *testing.T) {
			targetID, method, path, body := c.setup(f, nodeID)
			before := f.state(targetID, superID)
			code, resp := f.do(super, method, path, body)
			if code >= 400 {
				t.Fatalf("status = %d, body %s; want success", code, resp)
			}
			if after := f.state(targetID, superID); after == before {
				t.Error("reported success but changed nothing")
			}
		})
	}

	t.Run("create user with admin", func(t *testing.T) {
		code, resp := f.do(super, http.MethodPost, "/users",
			`{"username":"it-adminonly-create-`+id.New()+`","access":{"roles":[{"role":"admin","fleet_wide":true}],"pages":[]}}`)
		if code != http.StatusCreated {
			t.Errorf("status = %d, body %s; want 201", code, resp)
		}
	})
}

// An admin's other user management is unchanged: the same operations on a
// viewer or operator succeed and change the target.
func TestAdminStillManagesNonAdmins(t *testing.T) {
	f := newProtectionFixture(t)
	f.actor("spare-admin", roleGrant{role: coreuser.RoleAdmin})
	admin, adminID := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})

	cases := []struct {
		name, role string
		disabled   bool
		request    func(t plainTarget) (method, path, body string)
	}{
		{"grant operator", coreuser.RoleViewer, false, func(t plainTarget) (string, string, string) {
			return http.MethodPost, "/users/" + t.id + "/grants", `{"role":"operator","fleet_wide":true}`
		}},
		{"revoke viewer", coreuser.RoleViewer, false, func(t plainTarget) (string, string, string) {
			return http.MethodDelete, "/users/" + t.id + "/grants/" + t.grant, ""
		}},
		{"disable operator", coreuser.RoleOperator, false, func(t plainTarget) (string, string, string) {
			return http.MethodPost, "/users/" + t.id + "/disable", ""
		}},
		{"enable viewer", coreuser.RoleViewer, true, func(t plainTarget) (string, string, string) {
			return http.MethodPost, "/users/" + t.id + "/enable", ""
		}},
		{"reset operator password", coreuser.RoleOperator, false, func(t plainTarget) (string, string, string) {
			return http.MethodPost, "/users/" + t.id + "/reset-password", ""
		}},
		{"editor viewer to operator", coreuser.RoleViewer, false, func(t plainTarget) (string, string, string) {
			return http.MethodPut, "/users/" + t.id + "/access",
				`{"version":"` + t.version + `","roles":[{"role":"operator","fleet_wide":true}],"pages":[]}`
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			target := f.plainTarget(c.role, "", c.disabled)
			method, path, body := c.request(target)
			before := f.state(target.id, adminID)
			code, resp := f.do(admin, method, path, body)
			if code >= 400 {
				t.Fatalf("status = %d, body %s; want success", code, resp)
			}
			if after := f.state(target.id, adminID); after == before {
				t.Error("reported success but changed nothing")
			}
		})
	}

	t.Run("create user with viewer", func(t *testing.T) {
		code, resp := f.do(admin, http.MethodPost, "/users",
			`{"username":"it-adminonly-create-`+id.New()+`","access":{"roles":[{"role":"viewer","fleet_wide":true}],"pages":[]}}`)
		if code != http.StatusCreated {
			t.Errorf("status = %d, body %s; want 201", code, resp)
		}
	})
}

// An admin editing an admin target's pages and non-admin roles through the
// editor is allowed, including when the save cleans up a node-scoped admin
// row the fleet-wide admin role already covers.
func TestAdminEditsAnAdminsNonAdminAccess(t *testing.T) {
	f := newProtectionFixture(t)
	nodeID := f.node()
	f.actor("spare-admin", roleGrant{role: coreuser.RoleAdmin})
	admin, _ := f.actor("fleet-admin", roleGrant{role: coreuser.RoleAdmin})
	targetID, _ := f.createUser("admin-target", []roleGrant{{role: coreuser.RoleAdmin}, {role: coreuser.RoleAdmin, nodeID: nodeID}}, nil)

	version := f.getAccess(admin, targetID).Version
	code, resp := f.putAccess(admin, targetID, version, `[{"role":"admin","fleet_wide":true}]`,
		`[{"page":"overview","fleet_wide":true},{"page":"ports","fleet_wide":true}]`)
	if code != http.StatusOK {
		t.Fatalf("status = %d, body %s; want 200", code, resp)
	}
	want := []string{"page * overview", "page * ports", "role * admin"}
	if got := f.activeRows(targetID); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// The last-admin checks still refuse the superadmin.
func TestLastAdminChecksStillHoldForTheSuperadmin(t *testing.T) {
	f := newProtectionFixture(t)
	super, superID := f.actor("superadmin", roleGrant{role: coreuser.RoleSuperadmin})

	for _, c := range []struct {
		name string
		path func(t plainTarget) string
	}{
		{"revoke", func(t plainTarget) string { return "/users/" + t.id + "/grants/" + t.grant }},
		{"disable", func(t plainTarget) string { return "/users/" + t.id + "/disable" }},
	} {
		t.Run(c.name, func(t *testing.T) {
			target := f.plainTarget(coreuser.RoleAdmin, "", false)
			revokeOtherFleetAdmins(t, f, target.id)
			method := http.MethodPost
			if c.name == "revoke" {
				method = http.MethodDelete
			}
			before := f.state(target.id, superID)
			code, resp := f.do(super, method, c.path(target), "")
			if code != http.StatusPreconditionFailed || !strings.Contains(resp, "last user with fleet-wide admin access") {
				t.Errorf("status = %d, body %s; want 412 last-admin", code, resp)
			}
			if after := f.state(target.id, superID); after != before {
				t.Errorf("state changed despite the refusal:\nbefore %+v\nafter  %+v", before, after)
			}
		})
	}
}
