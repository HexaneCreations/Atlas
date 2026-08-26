package v1_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
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
	"github.com/hexane/atlas/internal/plugin/docker"
)

// closedLogLineChan and closedErrChan back a [fakeDockerClient] whose Logs()
// result is read to completion but never needs real content — ContainerLogs
// ranges over lines and then reads errCh unconditionally, and a nil channel
// (the zero value) blocks forever on both.
var (
	closedLogLineChan = closedChan[docker.LogLine]()
	closedErrChan     = closedChan[error]()
)

func closedChan[T any]() chan T {
	c := make(chan T)
	close(c)
	return c
}

// memUserStore is an in-memory [v1.UserStore] for the login flow.
type memUserStore struct {
	byUsername map[string]user.User
}

func (m *memUserStore) ByUsername(_ context.Context, username string) (user.User, error) {
	u, ok := m.byUsername[username]
	if !ok {
		return user.User{}, errs.New(errs.CodeNotFound, "no such user")
	}
	return u, nil
}

// memSessionStore is an in-memory [v1.SessionStore]/[session.SessionStore]:
// it behaves like [storageuser.Repository] closely enough to exercise the
// real login -> cookie -> Resolve -> protected-endpoint path end to end,
// without a database.
type memSessionStore struct {
	mu       sync.Mutex
	byHash   map[string]user.Session
	byUserID map[string]user.User
}

func newMemSessionStore(users *memUserStore) *memSessionStore {
	byUserID := make(map[string]user.User, len(users.byUsername))
	for _, u := range users.byUsername {
		byUserID[u.ID] = u
	}
	return &memSessionStore{byHash: map[string]user.Session{}, byUserID: byUserID}
}

func (m *memSessionStore) CreateSession(_ context.Context, s user.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.byHash[s.TokenHash] = s
	return nil
}

func (m *memSessionStore) Resolve(_ context.Context, tokenHash string, now time.Time) (user.Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.byHash[tokenHash]
	if !ok || !s.Live(now) {
		return user.Principal{}, user.ErrSessionInvalid
	}
	u := m.byUserID[s.UserID]
	return user.Principal{UserID: u.ID, Username: u.Username}, nil
}

func (m *memSessionStore) RevokeSession(_ context.Context, tokenHash string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.byHash[tokenHash]; ok {
		s.RevokedAt = &now
		m.byHash[tokenHash] = s
	}
	return nil
}

func (m *memSessionStore) RevokeAllSessions(_ context.Context, userID, _ string, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for hash, s := range m.byHash {
		if s.UserID == userID {
			s.RevokedAt = &now
			m.byHash[hash] = s
		}
	}
	return nil
}

// fakeAuthorizer grants, denies, or fails uniformly, recording the last
// permission and node it was asked about so a test can assert the endpoint
// under test actually asked the right question.
type fakeAuthorizer struct {
	mu    sync.Mutex
	allow bool
	// failWith, when set, simulates a backend failure in the permission
	// check itself (DB down, timeout, pool exhausted) — distinct from allow
	// being false, which simulates a real, successfully-answered denial.
	failWith      error
	gotPermission user.Permission
	gotNodeID     string
}

func (f *fakeAuthorizer) Require(_ context.Context, _ user.Principal, permission user.Permission, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gotPermission, f.gotNodeID = permission, nodeID
	if f.failWith != nil {
		return f.failWith
	}
	if f.allow {
		return nil
	}
	return user.ErrPermissionDenied
}

// authTestServer wires a real user + session + authz stack into the router,
// mirroring the composition [internal/app.App] does — see internal/app/user.go.
type authTestServer struct {
	*httptest.Server
	authz *fakeAuthorizer
}

func newAuthTestServer(t *testing.T, seedUser user.User, allow bool) authTestServer {
	t.Helper()
	return newAuthTestServerWithAuthorizer(t, seedUser, &fakeAuthorizer{allow: allow})
}

func newAuthTestServerWithAuthorizer(t *testing.T, seedUser user.User, authz *fakeAuthorizer) authTestServer {
	t.Helper()

	users := &memUserStore{byUsername: map[string]user.User{seedUser.Username: seedUser}}
	sessions := newMemSessionStore(users)

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config: &cfg,
		Health: health.NewRegistry(nil),
		Pool:   postgres.NewPool(cfg.Database, nil),
		Bus:    bus,
		Collection: fakeCollection{
			docker: &fakeDockerClient{
				containers: []docker.Container{{ID: testContainerID, Name: "web"}},
				// Closed rather than nil: ContainerLogs ranges over lines and
				// then reads errCh unconditionally, and a nil channel blocks
				// forever on both — these tests only need the request to
				// complete, not real log content.
				lines:   closedLogLineChan,
				logsErr: closedErrChan,
			},
		},
		Users:    users,
		Sessions: sessions,
		Authz:    authz,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return authTestServer{Server: srv, authz: authz}
}

func mustClientWithCookies(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{Jar: jar}
}

func testUser(t *testing.T, username, password string) user.User {
	t.Helper()
	hash, err := user.HashPassword(password)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	return user.User{ID: "uid-" + username, Username: username, PasswordHash: hash}
}

func login(t *testing.T, client *http.Client, base, username, password string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := client.Post(base+"/api/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("login POST: %v", err)
	}
	return resp
}

func TestLoginWithCorrectCredentialsSetsASessionCookie(t *testing.T) {
	t.Parallel()

	u := testUser(t, "alice", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)
	client := mustClientWithCookies(t)

	resp := login(t, client, srv.URL, "alice", "correct horse battery staple")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == "atlas_session" {
			found = true
			if !c.HttpOnly {
				t.Error("session cookie is not HttpOnly")
			}
		}
	}
	if !found {
		t.Error("no atlas_session cookie in the login response")
	}
}

func TestLoginWithWrongPasswordIsUnauthorized(t *testing.T) {
	t.Parallel()

	u := testUser(t, "bob", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)
	client := mustClientWithCookies(t)

	resp := login(t, client, srv.URL, "bob", "wrong password")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// An unknown username and a wrong password must be indistinguishable to the
// caller — see [user.ErrInvalidCredentials]'s doc.
func TestLoginWithUnknownUsernameIsUnauthorizedNotNotFound(t *testing.T) {
	t.Parallel()

	u := testUser(t, "carol", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)
	client := mustClientWithCookies(t)

	resp := login(t, client, srv.URL, "nobody", "whatever")
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — must read the same as a wrong password, not 404", resp.StatusCode)
	}
}

func TestCurrentUserWithoutASessionIsUnauthorized(t *testing.T) {
	t.Parallel()

	u := testUser(t, "dave", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)

	resp, err := http.Get(srv.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestCurrentUserAfterLoginReportsThePrincipal(t *testing.T) {
	t.Parallel()

	u := testUser(t, "erin", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)
	client := mustClientWithCookies(t)

	if resp := login(t, client, srv.URL, "erin", "correct horse battery staple"); resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}

	resp, err := client.Get(srv.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("get /auth/me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var got struct {
		Username string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Username != "erin" {
		t.Errorf("username = %q, want erin", got.Username)
	}
}

// can_manage_users is a display hint for the frontend's admin Users page nav
// entry — it must track the real PermissionUserManage decision, not be
// hardcoded true or false regardless of the authorizer.
func TestCurrentUserReportsCanManageUsersFromTheRealAuthzDecision(t *testing.T) {
	t.Parallel()

	for _, allow := range []bool{true, false} {
		t.Run(fmt.Sprintf("allow=%t", allow), func(t *testing.T) {
			t.Parallel()

			u := testUser(t, fmt.Sprintf("can-manage-%t", allow), "correct horse battery staple")
			authz := &fakeAuthorizer{allow: allow}
			srv := newAuthTestServerWithAuthorizer(t, u, authz)
			client := mustClientWithCookies(t)
			login(t, client, srv.URL, u.Username, "correct horse battery staple").Body.Close()

			resp, err := client.Get(srv.URL + "/api/v1/auth/me")
			if err != nil {
				t.Fatalf("get /auth/me: %v", err)
			}
			defer resp.Body.Close()

			var got struct {
				CanManageUsers bool `json:"can_manage_users"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.CanManageUsers != allow {
				t.Errorf("can_manage_users = %t, want %t", got.CanManageUsers, allow)
			}
			if authz.gotPermission != user.PermissionUserManage {
				t.Errorf("permission checked = %q, want user.manage", authz.gotPermission)
			}
		})
	}
}

func TestLogoutThenCurrentUserIsUnauthorizedAgain(t *testing.T) {
	t.Parallel()

	u := testUser(t, "frank", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)
	client := mustClientWithCookies(t)

	login(t, client, srv.URL, "frank", "correct horse battery staple").Body.Close()

	logoutResp, err := client.Post(srv.URL+"/api/v1/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	logoutResp.Body.Close()
	if logoutResp.StatusCode != http.StatusNoContent {
		t.Errorf("logout status = %d, want 204", logoutResp.StatusCode)
	}

	resp, err := client.Get(srv.URL + "/api/v1/auth/me")
	if err != nil {
		t.Fatalf("get /auth/me: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status after logout = %d, want 401", resp.StatusCode)
	}
}

// Logging out with no session at all must not error — the caller's intent
// ("I am logged out") holds either way.
func TestLogoutWithNoSessionIsStillNoContent(t *testing.T) {
	t.Parallel()

	u := testUser(t, "grace", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)

	resp, err := http.Post(srv.URL+"/api/v1/auth/logout", "application/json", nil)
	if err != nil {
		t.Fatalf("logout: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
}

// --- Node-scoped endpoint protection ------------------------------------

func TestNodeScopedEndpointRequiresAuthenticationWhenAuthzIsConfigured(t *testing.T) {
	t.Parallel()

	u := testUser(t, "heidi", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true) // would allow, but there is no session at all

	resp, err := http.Get(srv.URL + "/api/v1/containers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — no session cookie was ever sent", resp.StatusCode)
	}
}

func TestNodeScopedEndpointReturns403WhenAuthenticatedButDenied(t *testing.T) {
	t.Parallel()

	u := testUser(t, "ivan", "correct horse battery staple")
	srv := newAuthTestServer(t, u, false) // authenticated, but Authz always denies
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "ivan", "correct horse battery staple").Body.Close()

	resp, err := client.Get(srv.URL + "/api/v1/containers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if srv.authz.gotPermission != user.PermissionNodeRead {
		t.Errorf("permission checked = %q, want node.read", srv.authz.gotPermission)
	}
}

func TestNodeScopedEndpointSucceedsWhenAuthenticatedAndGranted(t *testing.T) {
	t.Parallel()

	u := testUser(t, "judy", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "judy", "correct horse battery staple").Body.Close()

	resp, err := client.Get(srv.URL + "/api/v1/containers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// The priority endpoint: container log content is gated by node.logs.read,
// a distinct and stricter permission than the node.read every other
// inventory endpoint checks — see docs/adr/0011-deferred-rbac.md.
func TestContainerLogsChecksTheLogsPermissionNotPlainNodeRead(t *testing.T) {
	t.Parallel()

	u := testUser(t, "kevin", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "kevin", "correct horse battery staple").Body.Close()

	resp, err := client.Get(srv.URL + "/api/v1/containers/web/logs")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if srv.authz.gotPermission != user.PermissionNodeLogsRead {
		t.Errorf("permission checked = %q, want node.logs.read", srv.authz.gotPermission)
	}
}

// The fail-closed property, proven end to end over real HTTP rather than at
// the unit level: when the authorization *check itself* fails — the
// permission-store equivalent of the database being down, a timeout, a
// connection pool exhausted — a fully authenticated, otherwise-legitimate
// caller must still never receive 200. [user.Authorizer.Require] returns the
// store's raw error in this case (see internal/core/user/authz.go), which
// carries CodeUnavailable; the assertion here is deliberately just "not a
// success status" rather than pinned to 503, so the test still catches a
// regression if that mapping ever changes to something else non-2xx.
func TestDBErrorDuringAuthzNeverProducesA200(t *testing.T) {
	t.Parallel()

	u := testUser(t, "mallory", "correct horse battery staple")
	authz := &fakeAuthorizer{failWith: errs.New(errs.CodeUnavailable, "connection pool exhausted")}
	srv := newAuthTestServerWithAuthorizer(t, u, authz)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "mallory", "correct horse battery staple").Body.Close()

	resp, err := client.Get(srv.URL + "/api/v1/containers")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		t.Fatalf("status = %d — a failing permission check must never produce a 2xx, regardless of how legitimate the caller is", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusOK {
		t.Fatal("status = 200 exactly — fail-closed violated")
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code == "" {
		t.Error("no error code in the response body")
	}
	if body.Error.Code == "permission_denied" {
		t.Error("a backend failure was reported as permission_denied — that misrepresents an outage as a real authorization decision")
	}
}

// The WebSocket log-follow endpoint must reject an unauthenticated caller
// before ever attempting the upgrade — a browser's WebSocket handshake has
// no Authorization header, so the session cookie checked here is the only
// mechanism available, and this is the endpoint
// docs/adr/0011-deferred-rbac.md names as highest-risk if it were missed.
func TestContainerLogsFollowRejectsAnUnauthenticatedUpgrade(t *testing.T) {
	t.Parallel()

	u := testUser(t, "laura", "correct horse battery staple")
	srv := newAuthTestServer(t, u, true) // would allow if authenticated

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/api/v1/containers/web/logs/follow", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 — an unauthenticated caller must never reach the WebSocket handshake", resp.StatusCode)
	}
}

// --- Write-endpoint protection (fleet.write) ------------------------------
//
// notifications/channels, alerts/rules, and slo writes are not node-scoped
// — see docs/adr/0013-human-user-authentication-and-authorization.md and
// migrations/0013_fleet_write_permission.sql — so they're gated by
// requirePermission(PermissionFleetWrite), not requireScope. Before this,
// any caller could register a notification channel — including an
// arbitrary webhook URL — with zero authentication.

// newWriteEndpointTestServer reuses [fakeNotificationStore] (defined in
// notifications_test.go, same package) rather than a second fake — the
// concrete risk described in review is registering a channel through
// exactly that store.
func newWriteEndpointTestServer(t *testing.T, seedUser user.User, authz *fakeAuthorizer, notifications *fakeNotificationStore) *httptest.Server {
	t.Helper()

	users := &memUserStore{byUsername: map[string]user.User{seedUser.Username: seedUser}}
	sessions := newMemSessionStore(users)

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:            &cfg,
		Health:            health.NewRegistry(nil),
		Pool:              postgres.NewPool(cfg.Database, nil),
		Bus:               bus,
		Collection:        fakeCollection{},
		Users:             users,
		Sessions:          sessions,
		Authz:             authz,
		NotificationStore: notifications,
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

const validChannelBody = `{"name":"ops-webhook","type":"webhook","webhook_url":"https://example.com/hook"}`

// The representative case: registering a notification channel — the
// concrete risk named in review — requires authentication at all.
func TestCreateNotificationChannelRequiresAuthentication(t *testing.T) {
	t.Parallel()

	u := testUser(t, "nora", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, &fakeAuthorizer{allow: true}, newFakeNotificationStore())

	resp, err := http.Post(srv.URL+"/api/v1/notifications/channels", "application/json", strings.NewReader(validChannelBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// Authenticated but not granted fleet.write (a viewer, say) must be denied.
func TestCreateNotificationChannelRequiresFleetWritePermission(t *testing.T) {
	t.Parallel()

	u := testUser(t, "oscar", "correct horse battery staple")
	authz := &fakeAuthorizer{allow: false}
	srv := newWriteEndpointTestServer(t, u, authz, newFakeNotificationStore())
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "oscar", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/notifications/channels", "application/json", strings.NewReader(validChannelBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if authz.gotPermission != user.PermissionFleetWrite {
		t.Errorf("permission checked = %q, want fleet.write", authz.gotPermission)
	}
}

// Authenticated and granted fleet.write (operator/admin) succeeds — and the
// channel actually reaches the store, proving the check is a gate, not a
// silent no-op.
func TestCreateNotificationChannelSucceedsWhenGranted(t *testing.T) {
	t.Parallel()

	u := testUser(t, "petra", "correct horse battery staple")
	notifications := newFakeNotificationStore()
	srv := newWriteEndpointTestServer(t, u, &fakeAuthorizer{allow: true}, notifications)
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, "petra", "correct horse battery staple").Body.Close()

	resp, err := client.Post(srv.URL+"/api/v1/notifications/channels", "application/json", strings.NewReader(validChannelBody))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201: %s", resp.StatusCode, body)
	}
	if len(notifications.channels) != 1 {
		t.Fatalf("store received %d channels, want 1", len(notifications.channels))
	}
	for _, c := range notifications.channels {
		if c.Webhook.URL != "https://example.com/hook" {
			t.Errorf("stored channel = %+v, want webhook url https://example.com/hook", c)
		}
	}
}

// The remaining 8 write handlers (alerts create/update/delete, SLO
// create/update/delete, notification channel update/delete) all go through
// the same requirePermission(PermissionFleetWrite) call — this proves each
// one actually does, by confirming an unauthenticated request to every one
// of them is refused with 401 before it ever reaches its store (which is
// nil in this harness and would otherwise surface as 503, not 401 — the
// distinction is exactly what proves the permission check runs first).
func TestAllRemainingWriteEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()

	u := testUser(t, "quinn", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, &fakeAuthorizer{allow: true}, newFakeNotificationStore())

	tests := []struct {
		method, path, body string
	}{
		{http.MethodPost, "/api/v1/alerts/rules", `{"name":"r","kind":"threshold","severity":"critical","metric":"m","comparison":"gt","threshold":1}`},
		{http.MethodPut, "/api/v1/alerts/rules/rule-1", `{"name":"r","kind":"threshold","severity":"critical","metric":"m","comparison":"gt","threshold":1}`},
		{http.MethodDelete, "/api/v1/alerts/rules/rule-1", ""},
		{http.MethodPost, "/api/v1/slo", `{"name":"s","metric":"m","comparison":"lt","threshold":1,"target_percentage":99,"window_seconds":3600}`},
		{http.MethodPut, "/api/v1/slo/slo-1", `{"name":"s","metric":"m","comparison":"lt","threshold":1,"target_percentage":99,"window_seconds":3600}`},
		{http.MethodDelete, "/api/v1/slo/slo-1", ""},
		{http.MethodPut, "/api/v1/notifications/channels/chan-1", validChannelBody},
		{http.MethodDelete, "/api/v1/notifications/channels/chan-1", ""},
	}

	for _, tt := range tests {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			var bodyReader *strings.Reader
			if tt.body != "" {
				bodyReader = strings.NewReader(tt.body)
			} else {
				bodyReader = strings.NewReader("")
			}
			req, err := http.NewRequest(tt.method, srv.URL+tt.path, bodyReader)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401 (a nil store surfacing as 503 would mean the permission check did not run first)", resp.StatusCode)
			}
		})
	}
}

// --- Login rate limiting ---------------------------------------------------

// newRateLimitedTestServer wires a real [user.LoginLimiter] into the auth
// stack — the other auth_test.go servers leave it nil (disabled), the same
// "nil disables" convention as every other optional store.
func newRateLimitedTestServer(t *testing.T, seedUser user.User) *httptest.Server {
	t.Helper()

	users := &memUserStore{byUsername: map[string]user.User{seedUser.Username: seedUser}}
	sessions := newMemSessionStore(users)

	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	handler := api.New(api.Deps{
		Config:       &cfg,
		Health:       health.NewRegistry(nil),
		Pool:         postgres.NewPool(cfg.Database, nil),
		Bus:          bus,
		Users:        users,
		Sessions:     sessions,
		LoginLimiter: user.NewLoginLimiter(),
	})

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// Confirms 429 after the threshold, and confirms errs.CodeRateLimited
// actually maps to 429 over real HTTP rather than by reading the map in
// httpx/response.go and trusting it applies here.
func TestLoginIsRateLimitedAfterFiveFailedAttempts(t *testing.T) {
	t.Parallel()

	u := testUser(t, "harriet", "correct horse battery staple")
	srv := newRateLimitedTestServer(t, u)

	for i := range 5 {
		resp := login(t, http.DefaultClient, srv.URL, "harriet", "wrong password")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401 (wrong password, not yet rate limited)", i+1, resp.StatusCode)
		}
	}

	resp := login(t, http.DefaultClient, srv.URL, "harriet", "wrong password")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("6th attempt status = %d, want 429", resp.StatusCode)
	}

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Error.Code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", body.Error.Code)
	}
}

// The behavior explicitly called out in review: a successful login resets
// the username's budget, so failed attempts before it don't carry over and
// lock the user out of their next visit.
func TestSuccessfulLoginResetsTheRateLimitBudget(t *testing.T) {
	t.Parallel()

	u := testUser(t, "ivan", "correct horse battery staple")
	srv := newRateLimitedTestServer(t, u)

	// 4 failed attempts — one under the threshold.
	for range 4 {
		login(t, http.DefaultClient, srv.URL, "ivan", "wrong password").Body.Close()
	}

	// The 5th, correct, attempt must still be allowed through (it is only
	// the 5th call to Allow, and the threshold is 5) and must succeed.
	resp := login(t, http.DefaultClient, srv.URL, "ivan", "correct horse battery staple")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correct-password attempt status = %d, want 200", resp.StatusCode)
	}

	// Without the reset, this next attempt would be the 6th call to Allow
	// and would be denied by the limiter (429) rather than by a wrong
	// password (401) — so seeing 401 here, not 429, is what proves the
	// reset happened.
	resp2 := login(t, http.DefaultClient, srv.URL, "ivan", "wrong password again")
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("post-success attempt status = %d, want 401 (429 would mean the successful login never reset the budget)", resp2.StatusCode)
	}
}

// loginWithForwardedFor POSTs to /auth/login carrying an X-Forwarded-For
// header shaped exactly as this deployment's real two-hop chain produces it
// (see httpx.ClientIP's doc): "<real client>, <nginx's peer, i.e. Caddy's
// address>" — two entries, so httpx.TrustedProxyHops resolves the first one
// as the client, exactly as it would for a request that had actually passed
// through Caddy then nginx.
func loginWithForwardedFor(t *testing.T, base, simulatedRealClientIP, username, password string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]string{"username": username, "password": password})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/auth/login", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", simulatedRealClientIP+", 127.0.0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	return resp
}

// Required test 3: the fix being verified end to end, not just the parsing
// function in isolation. Two different real clients, simulated through the
// full two-hop chain shape, must get independent per-IP budgets — before
// httpx.ClientIP counted from the right, every request arriving through
// nginx collapsed onto whatever single address the Go test server's own
// connection presented, sharing one IP budget regardless of who the real
// caller was.
//
// Each of the attacker's requests below names a distinct username, so the
// 5-per-username budget (unaffected by this bug either way — it never
// depended on ClientIP) cannot be what produces a 429; only the 20-per-IP
// budget can. That isolates the property this test exists to prove.
func TestLoginRateLimitGivesIndependentBudgetsPerRealClientIPThroughTheTwoHopChain(t *testing.T) {
	t.Parallel()

	srv := newRateLimitedTestServer(t, testUser(t, "seed-user", "correct horse battery staple"))

	const attackerIP = "203.0.113.50"
	const bystanderIP = "203.0.113.99"

	// 20 attempts from the attacker's IP, each against a fresh username
	// (never registered — ByUsername returns not-found, collapsed the same
	// as a wrong password) — exhausts the attacker's *IP* budget without
	// ever touching any single username's budget more than once.
	for i := range 20 {
		username := fmt.Sprintf("nonexistent-%d", i)
		resp := loginWithForwardedFor(t, srv.URL, attackerIP, username, "guess")
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attacker attempt %d status = %d, want 401", i+1, resp.StatusCode)
		}
	}
	attackerResp := loginWithForwardedFor(t, srv.URL, attackerIP, "nonexistent-20", "guess")
	defer attackerResp.Body.Close()
	if attackerResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("attacker's 21st attempt status = %d, want 429 (their IP budget should now be exhausted)", attackerResp.StatusCode)
	}

	// A different real client, resolved to a different address, attempting
	// a username the attacker only ever touched once (so its own
	// per-username budget is nowhere near exhausted) must be unaffected by
	// the attacker's exhausted IP budget.
	bystanderResp := loginWithForwardedFor(t, srv.URL, bystanderIP, "nonexistent-20", "guess")
	defer bystanderResp.Body.Close()
	if bystanderResp.StatusCode != http.StatusUnauthorized {
		t.Errorf("bystander status = %d, want 401 (wrong credentials) — 429 would mean the two IPs shared one budget", bystanderResp.StatusCode)
	}
}
