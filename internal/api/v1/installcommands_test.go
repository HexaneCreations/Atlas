package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/health"
	"github.com/hexane/atlas/internal/platform/postgres"
)

const testPeerID = "12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5SA"

type fakeInstallStore struct {
	mu          sync.Mutex
	redeemCalls int
	redeemErr   error
	lastIP      net.IP
}

func (f *fakeInstallStore) setErr(err error) { f.mu.Lock(); f.redeemErr = err; f.mu.Unlock() }
func (f *fakeInstallStore) calls() (int, net.IP) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.redeemCalls, f.lastIP
}

func (f *fakeInstallStore) CreateInstallCode(context.Context, string, fleet.InstallCodeSpec, time.Time) (fleet.InstallCode, error) {
	return fleet.InstallCode{}, errs.New(errs.CodeInternal, "not used")
}
func (f *fakeInstallStore) ListInstallCodes(context.Context) ([]fleet.InstallCode, error) {
	return nil, nil
}
func (f *fakeInstallStore) RevokeInstallCode(context.Context, string, string, time.Time) (fleet.InstallCode, error) {
	return fleet.InstallCode{}, errs.New(errs.CodeNotFound, "no such install code")
}
func (f *fakeInstallStore) RedeemInstallCode(_ context.Context, _, _ string, ip net.IP, _ time.Time) (fleet.Redemption, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redeemCalls++
	f.lastIP = ip
	if f.redeemErr != nil {
		return fleet.Redemption{}, f.redeemErr
	}
	return fleet.Redemption{CodeID: "c1", NodeID: "0123456789abcdef0123456789abcdef", Environment: "production"}, nil
}

func newInstallTestServer(t *testing.T, store *fakeInstallStore, limiter v1.RedeemLimiter, authz v1.Authorizer) *httptest.Server {
	t.Helper()
	cfg := config.Default()
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })
	users := &memUserStore{byUsername: map[string]user.User{}}
	deps := api.Deps{
		Config: &cfg, Health: health.NewRegistry(nil), Pool: postgres.NewPool(cfg.Database, nil), Bus: bus,
		Collection: fakeCollection{}, Users: users, Sessions: newMemSessionStore(users), Authz: authz,
		InstallCodes: store, RedeemLimiter: limiter,
	}
	srv := httptest.NewServer(api.New(deps))
	t.Cleanup(srv.Close)
	return srv
}

func redeem(t *testing.T, base, body string, header map[string]string) (int, map[string]any, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/agent/install/redeem", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	_ = json.Unmarshal(raw, &decoded)
	return resp.StatusCode, decoded, string(raw)
}

// withoutRequestID drops the per-request correlation id, the only field
// that legitimately differs between two refusals.
func withoutRequestID(t *testing.T, body map[string]any) string {
	t.Helper()
	e, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("not an error envelope: %v", body)
	}
	delete(e, "request_id")
	out, _ := json.Marshal(body)
	return string(out)
}

func validCode(t *testing.T) string {
	t.Helper()
	c, err := fleet.NewInstallCode()
	if err != nil {
		t.Fatal(err)
	}
	return c.Plaintext
}

func redeemBody(code, peerID string) string {
	b, _ := json.Marshal(map[string]string{"code": code, "peer_id": peerID})
	return string(b)
}

// Malformed input is refused before the store is touched, and every refusal
// — malformed or a store-level rejection of any kind — is the same status
// and body.
func TestRedeemRefusalsAreUniformAndFormatChecksPrecedeTheStore(t *testing.T) {
	t.Parallel()
	store := &fakeInstallStore{}
	srv := newInstallTestServer(t, store, fleet.NewRedeemLimiter(1000, 1000), nil)
	code := validCode(t)

	malformed := map[string]string{
		"enrollment token prefix": redeemBody(fleet.TokenPrefix+strings.Repeat("a", 64), testPeerID),
		"short code":              redeemBody(code[:len(code)-1], testPeerID),
		"bad peer id":             redeemBody(code, "12D3KooWnot-a-peer"),
		"truncated peer id":       redeemBody(code, "12D3KooWnope"),
		"empty peer id":           redeemBody(code, ""),
		"not json":                "code=" + code,
		"unknown field":           `{"code":"` + code + `","peer_id":"` + testPeerID + `","node_id":"x"}`,
		"empty body":              "",
	}
	var want string
	for name, body := range malformed {
		status, decoded, raw := redeem(t, srv.URL, body, nil)
		if status != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401", name, status)
		}
		got := withoutRequestID(t, decoded)
		if want == "" {
			want = got
		} else if got != want {
			t.Errorf("%s: body %s differs from %s", name, got, want)
		}
		if strings.Contains(raw, code) {
			t.Errorf("%s: the response echoes the code", name)
		}
	}
	if n, _ := store.calls(); n != 0 {
		t.Fatalf("store called %d times for malformed input, want 0", n)
	}

	for _, storeErr := range []error{
		fleet.ErrInstallCodeRejected,
		errs.Wrap(fleet.ErrInstallCodeRejected, errs.CodeUnauthenticated, "the peer id is already registered"),
		errs.Wrap(fleet.ErrInstallCodeRejected, errs.CodeUnauthenticated, "the pre-assigned node already exists"),
	} {
		store.setErr(storeErr)
		status, decoded, _ := redeem(t, srv.URL, redeemBody(code, testPeerID), nil)
		if status != http.StatusUnauthorized || withoutRequestID(t, decoded) != want {
			t.Errorf("store error %v: got %d %v, want 401 %s", storeErr, status, decoded, want)
		}
	}
}

func TestRedeemSuccessReturnsOnlyNodeAndEnvironment(t *testing.T) {
	t.Parallel()
	store := &fakeInstallStore{}
	srv := newInstallTestServer(t, store, fleet.NewRedeemLimiter(1000, 1000), nil)
	code := validCode(t)

	status, decoded, raw := redeem(t, srv.URL, redeemBody(code, testPeerID), nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, raw)
	}
	if len(decoded) != 2 || decoded["node_id"] != "0123456789abcdef0123456789abcdef" || decoded["environment"] != "production" {
		t.Errorf("body = %v, want exactly node_id and environment", decoded)
	}
	if strings.Contains(raw, code) {
		t.Error("the response echoes the code")
	}
}

// The per-IP budget keys on the proxy-appended X-Forwarded-For entry, so a
// client prepending its own entries cannot get a fresh bucket; the global
// cap holds across addresses.
func TestRedeemIsRateLimitedPerClientIPAndGlobally(t *testing.T) {
	t.Parallel()
	store := &fakeInstallStore{}
	srv := newInstallTestServer(t, store, fleet.NewRedeemLimiter(2, 3), nil)
	body := redeemBody(validCode(t), testPeerID)
	via := func(xff string) map[string]string { return map[string]string{"X-Forwarded-For": xff} }

	for i, xff := range []string{"198.51.100.7, 172.18.0.1", "1.2.3.4, 198.51.100.7, 172.18.0.1"} {
		if status, _, _ := redeem(t, srv.URL, body, via(xff)); status != http.StatusOK {
			t.Fatalf("attempt %d: status %d, want 200", i+1, status)
		}
	}
	if _, ip := store.calls(); ip.String() != "198.51.100.7" {
		t.Errorf("source ip recorded as %v, want the proxy-attested client 198.51.100.7", ip)
	}
	if status, _, _ := redeem(t, srv.URL, body, via("9.9.9.9, 198.51.100.7, 172.18.0.1")); status != http.StatusTooManyRequests {
		t.Fatalf("3rd attempt from one client: status %d, want 429", status)
	}
	if status, _, _ := redeem(t, srv.URL, body, via("203.0.113.9, 172.18.0.1")); status != http.StatusTooManyRequests {
		t.Fatalf("attempt past the global cap: status %d, want 429", status)
	}
	if n, _ := store.calls(); n != 2 {
		t.Errorf("store called %d times, want 2 (limited attempts never reach it)", n)
	}
}

func TestInstallCommandEndpointsNeedASessionAndWiredAuthz(t *testing.T) {
	t.Parallel()
	actions := []guardAction{
		{"create", http.MethodPost, "/fleet/install-commands", `{"environment":"production"}`},
		{"list", http.MethodGet, "/fleet/install-commands", ""},
		{"revoke", http.MethodDelete, "/fleet/install-commands/abc", ""},
	}
	withAuthz := newInstallTestServer(t, &fakeInstallStore{}, fleet.NewRedeemLimiter(10, 10), roleAuthorizer{role: user.RoleSuperadmin})
	noAuthz := newInstallTestServer(t, &fakeInstallStore{}, fleet.NewRedeemLimiter(10, 10), nil)
	for _, a := range actions {
		if code := do(t, http.DefaultClient, withAuthz.URL, a); code != http.StatusUnauthorized {
			t.Errorf("%s anonymous: status %d, want 401", a.name, code)
		}
		// Fails closed: no authorizer means no install commands, not open ones.
		if code := do(t, http.DefaultClient, noAuthz.URL, a); code != http.StatusServiceUnavailable {
			t.Errorf("%s without authz: status %d, want 503", a.name, code)
		}
	}
}

func TestInstallCommandListResponseHasNoCodeField(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(v1.ListInstallCommandsResponse{InstallCommands: []v1.InstallCommandResponse{{ID: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"code"`, `"code_hash"`, `"command"`} {
		if strings.Contains(string(raw), field) {
			t.Errorf("list response carries %s: %s", field, raw)
		}
	}
}
