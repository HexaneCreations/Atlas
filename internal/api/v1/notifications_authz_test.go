package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/user"
)

// rolePermissions mirrors the role_permissions seed (migrations 0012, 0013,
// 0014, 0019, 0021, 0023). The real table is exercised by the integration tests in
// internal/storage/user and internal/app; this copy lets the handler tests
// speak in roles.
var rolePermissions = map[string][]user.Permission{
	user.RoleViewer:   {user.PermissionNodeRead},
	user.RoleOperator: {user.PermissionNodeRead, user.PermissionNodeLogsRead, user.PermissionFleetWrite},
	user.RoleAdmin: {user.PermissionNodeRead, user.PermissionNodeLogsRead, user.PermissionFleetWrite,
		user.PermissionUserManage, user.PermissionFleetEnroll},
	user.RoleSuperadmin: {user.PermissionNodeRead, user.PermissionNodeLogsRead, user.PermissionFleetWrite,
		user.PermissionUserManage, user.PermissionNotificationsManage, user.PermissionFleetEnroll},
}

// roleAuthorizer answers Require the way storage/user.Repository.HasPermission
// does for a single grant of role: a fleet-wide grant matches any node id,
// including the empty one; a node-scoped grant matches only its own node.
type roleAuthorizer struct {
	role   string
	nodeID string // "" means the grant is fleet-wide
}

func (a roleAuthorizer) Require(_ context.Context, _ user.Principal, permission user.Permission, nodeID string) error {
	if a.nodeID != "" && a.nodeID != nodeID {
		return user.ErrPermissionDenied
	}
	for _, p := range rolePermissions[a.role] {
		if p == permission {
			return nil
		}
	}
	return user.ErrPermissionDenied
}

func (a roleAuthorizer) AuthorizedNodes(context.Context, user.Principal, user.Permission) (bool, map[string]bool, error) {
	return a.nodeID == "", map[string]bool{a.nodeID: true}, nil
}

func notificationActions() []struct {
	guardAction
	ok int
} {
	const body = `{"name":"ops","type":"teams","enabled":true}`
	return []struct {
		guardAction
		ok int
	}{
		{guardAction{"list channels", http.MethodGet, "/notifications/channels", ""}, http.StatusOK},
		{guardAction{"create channel", http.MethodPost, "/notifications/channels", body}, http.StatusCreated},
		{guardAction{"get channel", http.MethodGet, "/notifications/channels/chan-1", ""}, http.StatusOK},
		{guardAction{"update channel", http.MethodPut, "/notifications/channels/chan-1", body}, http.StatusOK},
		{guardAction{"delete channel", http.MethodDelete, "/notifications/channels/chan-1", ""}, http.StatusNoContent},
		{guardAction{"list deliveries", http.MethodGet, "/notifications/deliveries", ""}, http.StatusOK},
	}
}

func seededNotificationStore() *fakeNotificationStore {
	store := newFakeNotificationStore()
	store.channels["chan-1"] = notification.Channel{
		ID: "chan-1", Name: "ops", Type: notification.ChannelTeams, Enabled: true, MinSeverity: notification.SeverityWarning,
	}
	return store
}

// Every notification endpoint — reads included — is reachable only through a
// fleet-wide superadmin grant. admin, operator, viewer, and a node-scoped
// superadmin are all refused, and a refused write changes nothing.
func TestNotificationEndpointsRequireNotificationsManage(t *testing.T) {
	t.Parallel()

	callers := []struct {
		name  string
		authz roleAuthorizer
		allow bool
	}{
		{"fleet-wide superadmin", roleAuthorizer{role: user.RoleSuperadmin}, true},
		{"node-scoped superadmin", roleAuthorizer{role: user.RoleSuperadmin, nodeID: "node-1"}, false},
		{"fleet-wide admin", roleAuthorizer{role: user.RoleAdmin}, false},
		{"fleet-wide operator", roleAuthorizer{role: user.RoleOperator}, false},
		{"fleet-wide viewer", roleAuthorizer{role: user.RoleViewer}, false},
	}
	for _, c := range callers {
		for _, a := range notificationActions() {
			t.Run(c.name+"/"+a.name, func(t *testing.T) {
				t.Parallel()
				u := testUser(t, "notif-caller", "correct horse battery staple")
				store := seededNotificationStore()
				srv := newWriteEndpointTestServer(t, u, c.authz, store)
				client := mustClientWithCookies(t)
				login(t, client, srv.URL, u.Username, "correct horse battery staple").Body.Close()

				want := http.StatusForbidden
				if c.allow {
					want = a.ok
				}
				if code := do(t, client, srv.URL, a.guardAction); code != want {
					t.Fatalf("status = %d, want %d", code, want)
				}
				if !c.allow {
					if ch, ok := store.channels["chan-1"]; !ok || len(store.channels) != 1 || ch.Name != "ops" {
						t.Errorf("a refused call changed the store: %+v", store.channels)
					}
				}
			})
		}
	}
}

// Without a session every notification endpoint is 401, not a data response.
func TestNotificationEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()

	u := testUser(t, "notif-anon", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, roleAuthorizer{role: user.RoleSuperadmin}, seededNotificationStore())
	for _, a := range notificationActions() {
		t.Run(a.name, func(t *testing.T) {
			if code := do(t, http.DefaultClient, srv.URL, a.guardAction); code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", code)
			}
		})
	}
}

// /auth/me reports can_manage_notifications for a fleet-wide superadmin and
// for nobody else.
func TestCurrentUserReportsCanManageNotificationsForSuperadminOnly(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name  string
		authz roleAuthorizer
		want  bool
	}{
		{"fleet-wide superadmin", roleAuthorizer{role: user.RoleSuperadmin}, true},
		{"node-scoped superadmin", roleAuthorizer{role: user.RoleSuperadmin, nodeID: "node-1"}, false},
		{"fleet-wide admin", roleAuthorizer{role: user.RoleAdmin}, false},
		{"fleet-wide operator", roleAuthorizer{role: user.RoleOperator}, false},
		{"fleet-wide viewer", roleAuthorizer{role: user.RoleViewer}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			u := testUser(t, "notif-me", "correct horse battery staple")
			srv := newWriteEndpointTestServer(t, u, c.authz, newFakeNotificationStore())
			client := mustClientWithCookies(t)
			login(t, client, srv.URL, u.Username, "correct horse battery staple").Body.Close()

			resp, err := client.Get(srv.URL + "/api/v1/auth/me")
			if err != nil {
				t.Fatalf("GET /auth/me: %v", err)
			}
			defer resp.Body.Close()
			var me map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got, present := me["can_manage_notifications"].(bool)
			if !present {
				t.Fatalf("can_manage_notifications missing or not a bool: %v", me)
			}
			if got != c.want {
				t.Errorf("can_manage_notifications = %v, want %v", got, c.want)
			}
		})
	}
}

// With no encryption key the app wires no notification store: every
// endpoint answers 503 to a superadmin, and the permission check still runs
// first, so nobody else learns whether notifications are configured.
func TestNotificationEndpointsAre503WithoutAStore(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name  string
		authz roleAuthorizer
		want  int
	}{
		{"fleet-wide superadmin", roleAuthorizer{role: user.RoleSuperadmin}, http.StatusServiceUnavailable},
		{"fleet-wide admin", roleAuthorizer{role: user.RoleAdmin}, http.StatusForbidden},
	} {
		for _, a := range notificationActions() {
			t.Run(c.name+"/"+a.name, func(t *testing.T) {
				t.Parallel()
				u := testUser(t, "notif-nokey", "correct horse battery staple")
				srv := newWriteEndpointTestServer(t, u, c.authz, nil)
				client := mustClientWithCookies(t)
				login(t, client, srv.URL, u.Username, "correct horse battery staple").Body.Close()
				if code := do(t, client, srv.URL, a.guardAction); code != c.want {
					t.Fatalf("status = %d, want %d", code, c.want)
				}
			})
		}
	}
}
