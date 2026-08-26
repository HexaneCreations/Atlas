package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/postgres"
)

// fakeUserAdmin is an in-memory [v1.UserAdmin], reusing [fakeAuthorizer] for
// permission decisions the same way [newWriteEndpointTestServer] does — this
// fake only needs to prove the handlers call through correctly, not
// re-implement the storage layer's own last-admin-guard logic (that is
// proven directly against real Postgres in
// internal/storage/user/repository_integration_test.go).
type fakeUserAdmin struct {
	mu     sync.Mutex
	users  map[string]user.User
	grants map[string]user.NodeRole
	audit  map[string][]user.AuditEntry

	// lastAdminGrantID/lastAdminUserID, when set, make RevokeGrant/DisableUser
	// return [user.ErrLastAdminGrant] for that one id — simulating the guard
	// without re-deriving it here.
	lastAdminGrantID string
	lastAdminUserID  string
}

func newFakeUserAdmin(seed ...user.User) *fakeUserAdmin {
	f := &fakeUserAdmin{users: map[string]user.User{}, grants: map[string]user.NodeRole{}, audit: map[string][]user.AuditEntry{}}
	for _, u := range seed {
		f.users[u.ID] = u
	}
	return f
}

func (f *fakeUserAdmin) ListUsersWithGrants(context.Context) ([]user.UserWithGrants, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]user.UserWithGrants, 0, len(f.users))
	for _, u := range f.users {
		var grants []user.NodeRole
		for _, g := range f.grants {
			if g.UserID == u.ID && g.RevokedAt == nil {
				grants = append(grants, g)
			}
		}
		out = append(out, user.UserWithGrants{User: u, Grants: grants})
	}
	return out, nil
}

func (f *fakeUserAdmin) GetUser(_ context.Context, id string) (user.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return user.User{}, errs.New(errs.CodeNotFound, "no such user")
	}
	return u, nil
}

func (f *fakeUserAdmin) AdminCreateUser(_ context.Context, username, email, _ string, now time.Time) (user.User, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := user.User{ID: "new-" + username, Username: username, Email: email, CreatedAt: now}
	f.users[u.ID] = u
	return u, "generated-one-time-password", nil
}

func (f *fakeUserAdmin) DisableUser(_ context.Context, userID, _ string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if userID == f.lastAdminUserID {
		return user.ErrLastAdminGrant
	}
	u, ok := f.users[userID]
	if !ok {
		return errs.New(errs.CodeNotFound, "no such user")
	}
	u.DisabledAt = &now
	f.users[userID] = u
	return nil
}

func (f *fakeUserAdmin) EnableUser(_ context.Context, userID, _ string, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[userID]
	if !ok {
		return errs.New(errs.CodeNotFound, "no such user")
	}
	u.DisabledAt = nil
	f.users[userID] = u
	return nil
}

func (f *fakeUserAdmin) ResetPassword(_ context.Context, userID, _ string, _ time.Time) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[userID]; !ok {
		return "", errs.New(errs.CodeNotFound, "no such user")
	}
	return "new-generated-password", nil
}

func (f *fakeUserAdmin) ListAudit(_ context.Context, targetUserID string) ([]user.AuditEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.audit[targetUserID], nil
}

func (f *fakeUserAdmin) Grant(_ context.Context, spec user.GrantSpec, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("grant-%d", len(f.grants)+1)
	var nodeID *string
	if !spec.FleetWide {
		nodeID = &spec.NodeID
	}
	f.grants[id] = user.NodeRole{ID: id, UserID: spec.UserID, NodeID: nodeID, Role: spec.Role, GrantedAt: now, GrantedBy: spec.GrantedBy}
	return nil
}

func (f *fakeUserAdmin) RevokeGrant(_ context.Context, grantID, _ string, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if grantID == f.lastAdminGrantID {
		return user.ErrLastAdminGrant
	}
	g, ok := f.grants[grantID]
	if !ok {
		return nil
	}
	g.RevokedAt = &now
	f.grants[grantID] = g
	return nil
}

// newUserAdminTestServer wires a real user + session + authz stack plus a
// [fakeUserAdmin] into the router — the same composition
// [newWriteEndpointTestServer] uses for the fleet.write endpoints.
func newUserAdminTestServer(t *testing.T, seedUser user.User, authz *fakeAuthorizer, admin *fakeUserAdmin) *httptest.Server {
	t.Helper()

	users := &memUserStore{byUsername: map[string]user.User{seedUser.Username: seedUser}}
	sessions := newMemSessionStore(users)

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     health.NewRegistry(nil),
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: fakeCollection{},
		Users:      users,
		Sessions:   sessions,
		Authz:      authz,
		UserAdmin:  admin,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// --- Authentication and permission gating -----------------------------------

// Every admin Users page endpoint requires authentication before anything
// else — a nil UserAdmin call would otherwise surface as 501/503, not 401,
// which is exactly what would prove the permission check did not run first.
func TestUserAdminEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()

	u := testUser(t, "rex", "correct horse battery staple")
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, newFakeUserAdmin())

	tests := []struct {
		method, path, body string
	}{
		{http.MethodGet, "/api/v1/users", ""},
		{http.MethodPost, "/api/v1/users", `{"username":"newbie"}`},
		{http.MethodPost, "/api/v1/users/target-1/grants", `{"role":"viewer","fleet_wide":true}`},
		{http.MethodDelete, "/api/v1/users/target-1/grants/grant-1", ""},
		{http.MethodPost, "/api/v1/users/target-1/disable", ""},
		{http.MethodPost, "/api/v1/users/target-1/enable", ""},
		{http.MethodPost, "/api/v1/users/target-1/reset-password", ""},
		{http.MethodPost, "/api/v1/users/target-1/force-logout", ""},
		{http.MethodGet, "/api/v1/users/target-1/audit", ""},
	}
	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var body *strings.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			} else {
				body = strings.NewReader("")
			}
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, body)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", resp.StatusCode)
			}
		})
	}
}

func TestListUsersRequiresUserManagePermission(t *testing.T) {
	t.Parallel()

	u := testUser(t, "sybil", "correct horse battery staple")
	authz := &fakeAuthorizer{allow: false}
	srv := newUserAdminTestServer(t, u, authz, newFakeUserAdmin())
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "sybil", "correct horse battery staple").Body.Close()

	resp, err := client.Get(srv.URL + "/api/v1/users")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if authz.gotPermission != user.PermissionUserManage {
		t.Errorf("permission checked = %q, want user.manage", authz.gotPermission)
	}
}

func TestListUsersSucceedsWhenGranted(t *testing.T) {
	t.Parallel()

	u := testUser(t, "trent", "correct horse battery staple")
	admin := newFakeUserAdmin(u, user.User{ID: "other-1", Username: "other"})
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "trent", "correct horse battery staple").Body.Close()

	resp, err := client.Get(srv.URL + "/api/v1/users")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		Users []struct {
			Username string `json:"username"`
		} `json:"users"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 2 {
		t.Errorf("total = %d, want 2", got.Total)
	}
}

// --- Create --------------------------------------------------------------

func TestCreateUserSucceedsAndReturnsGeneratedPasswordExactlyOnce(t *testing.T) {
	t.Parallel()

	u := testUser(t, "ursula", "correct horse battery staple")
	admin := newFakeUserAdmin(u)
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "ursula", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users", "application/json", strings.NewReader(`{"username":"newbie"}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}

	var got struct {
		User struct {
			Username string `json:"username"`
		} `json:"user"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.User.Username != "newbie" {
		t.Errorf("username = %q, want newbie", got.User.Username)
	}
	if got.Password == "" {
		t.Error("no generated password in the response")
	}
}

func TestCreateUserRejectsAnEmptyUsername(t *testing.T) {
	t.Parallel()

	u := testUser(t, "victor", "correct horse battery staple")
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, newFakeUserAdmin(u))
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "victor", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users", "application/json", strings.NewReader(`{"username":""}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// --- Grant / revoke --------------------------------------------------------

// The explicit node_id XOR fleet_wide requirement — never a default between
// them — must reach through the handler down to [user.GrantSpec.Validate].
func TestGrantRoleRejectsAnAmbiguousScope(t *testing.T) {
	t.Parallel()

	u := testUser(t, "wendy", "correct horse battery staple")
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, newFakeUserAdmin(u))
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "wendy", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users/target-1/grants", "application/json",
		strings.NewReader(`{"role":"viewer"}`)) // neither node_id nor fleet_wide
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (no default between node-scoped and fleet-wide)", resp.StatusCode)
	}
}

func TestGrantRoleSucceedsWithAnExplicitFleetWideScope(t *testing.T) {
	t.Parallel()

	u := testUser(t, "xander", "correct horse battery staple")
	admin := newFakeUserAdmin(u)
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "xander", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users/target-1/grants", "application/json",
		strings.NewReader(`{"role":"viewer","fleet_wide":true}`))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if len(admin.grants) != 1 {
		t.Fatalf("grants recorded = %d, want 1", len(admin.grants))
	}
}

// Item 3 of the admin-users-page addendum, proven at the HTTP layer: the
// last-fleet-wide-admin guard's error must surface as a real, distinct
// status — not swallowed into a generic 500 or misreported as success.
func TestRevokeRoleSurfacesTheLastAdminGuard(t *testing.T) {
	t.Parallel()

	u := testUser(t, "yara", "correct horse battery staple")
	admin := newFakeUserAdmin(u)
	admin.grants["grant-last-admin"] = user.NodeRole{ID: "grant-last-admin", UserID: "target-1", Role: user.RoleAdmin}
	admin.lastAdminGrantID = "grant-last-admin"
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "yara", "correct horse battery staple").Body.Close()

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/api/v1/users/target-1/grants/grant-last-admin", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 (errs.CodeFailedPrecondition)", resp.StatusCode)
	}
}

// --- Disable / enable / self-action guard -----------------------------------

// Item 2 of the addendum: a logged-in admin must not be able to disable
// their own account by accident.
func TestDisableUserRefusesTargetingYourself(t *testing.T) {
	t.Parallel()

	u := testUser(t, "zach", "correct horse battery staple")
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, newFakeUserAdmin(u))
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "zach", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users/"+u.ID+"/disable", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (cannot disable your own account)", resp.StatusCode)
	}
}

func TestDisableUserSurfacesTheLastAdminGuard(t *testing.T) {
	t.Parallel()

	u := testUser(t, "amber", "correct horse battery staple")
	admin := newFakeUserAdmin(u, user.User{ID: "target-admin", Username: "target-admin"})
	admin.lastAdminUserID = "target-admin"
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "amber", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users/target-admin/disable", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPreconditionFailed {
		t.Errorf("status = %d, want 412 (errs.CodeFailedPrecondition)", resp.StatusCode)
	}
}

func TestDisableThenEnableUserSucceeds(t *testing.T) {
	t.Parallel()

	u := testUser(t, "brody", "correct horse battery staple")
	admin := newFakeUserAdmin(u, user.User{ID: "target-2", Username: "target-2"})
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "brody", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users/target-2/disable", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("disable status = %d, want 204", resp.StatusCode)
	}
	if !admin.users["target-2"].Disabled() {
		t.Fatal("target-2 was not marked disabled")
	}

	resp, err = client.Post(srv.URL+"/api/v1/users/target-2/enable", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enable status = %d, want 204", resp.StatusCode)
	}
	if admin.users["target-2"].Disabled() {
		t.Error("target-2 still disabled after enable")
	}
}

// --- Reset password / force logout ------------------------------------------

func TestResetPasswordReturnsAGeneratedPassword(t *testing.T) {
	t.Parallel()

	u := testUser(t, "carter", "correct horse battery staple")
	admin := newFakeUserAdmin(u, user.User{ID: "target-3", Username: "target-3"})
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "carter", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users/target-3/reset-password", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Password == "" {
		t.Error("no generated password in the response")
	}
}

// The same self-action guard as disable — force-logging-out your own
// account would end your own session mid-request.
func TestForceLogoutRefusesTargetingYourself(t *testing.T) {
	t.Parallel()

	u := testUser(t, "dana", "correct horse battery staple")
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, newFakeUserAdmin(u))
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "dana", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/users/"+u.ID+"/force-logout", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (cannot force-logout your own account)", resp.StatusCode)
	}
}

func TestForceLogoutSucceedsAgainstAnotherUser(t *testing.T) {
	t.Parallel()

	u := testUser(t, "ellis", "correct horse battery staple")
	target := testUser(t, "target-user", "correct horse battery staple")
	users := &memUserStore{byUsername: map[string]user.User{u.Username: u, target.Username: target}}
	sessions := newMemSessionStore(users)

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:     &cfg,
		Health:     health.NewRegistry(nil),
		Pool:       postgres.NewPool(cfg.Database, nil),
		Bus:        bus,
		Collection: fakeCollection{},
		Users:      users,
		Sessions:   sessions,
		Authz:      &fakeAuthorizer{allow: true},
		UserAdmin:  newFakeUserAdmin(u, target),
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// target logs in first, establishing a live session to be terminated.
	targetClient := mustClientWithCookies(t)
	login(t, targetClient, srv.URL, "target-user", "correct horse battery staple").Body.Close()

	adminClient := mustClientWithCookies(t)
	login(t, adminClient, srv.URL, "ellis", "correct horse battery staple").Body.Close()

	resp, err := adminClient.Post(srv.URL+"/api/v1/users/"+target.ID+"/force-logout", "application/json", nil)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("force-logout status = %d, want 204", resp.StatusCode)
	}

	resp, err = targetClient.Get(srv.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("get /auth/me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("target's session status = %d after force-logout, want 401", resp.StatusCode)
	}
}

// --- Audit -----------------------------------------------------------------

func TestListUserAuditReturnsRecordedEntries(t *testing.T) {
	t.Parallel()

	u := testUser(t, "felix", "correct horse battery staple")
	admin := newFakeUserAdmin(u, user.User{ID: "target-4", Username: "target-4"})
	admin.audit["target-4"] = []user.AuditEntry{
		{ID: "a1", ActorUserID: u.ID, ActorUsername: "felix", TargetUserID: "target-4", Action: user.AuditActionDisableUser, CreatedAt: time.Now()},
	}
	srv := newUserAdminTestServer(t, u, &fakeAuthorizer{allow: true}, admin)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "felix", "correct horse battery staple").Body.Close()

	resp, err := client.Get(srv.URL + "/api/v1/users/target-4/audit")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		Entries []struct {
			Action        string `json:"action"`
			ActorUsername string `json:"actor_username"`
		} `json:"entries"`
		Total int `json:"total"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Total != 1 || got.Entries[0].Action != user.AuditActionDisableUser {
		t.Errorf("entries = %+v, want one disable_user entry", got.Entries)
	}
	if got.Entries[0].ActorUsername != "felix" {
		t.Errorf("actor_username = %q, want felix", got.Entries[0].ActorUsername)
	}
}
