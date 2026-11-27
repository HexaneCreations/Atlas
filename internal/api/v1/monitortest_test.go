package v1_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

// countingTester records what it was asked to check.
type countingTester struct {
	mu    sync.Mutex
	calls []monitor.Monitor
	err   error
}

func (c *countingTester) Test(_ context.Context, m monitor.Monitor, _ monitor.Secret) (monitor.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, m)
	if c.err != nil {
		return monitor.Result{}, c.err
	}
	return monitor.Result{Up: true, StatusCode: 200, ResponseMS: 12}, nil
}

func (c *countingTester) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

// steppingClock moves a minute per reading, so the test limiter never
// throttles a test that is not about throttling.
func steppingClock() func() time.Time {
	var mu sync.Mutex
	t := time.Now()
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		t = t.Add(time.Minute)
		return t
	}
}

func withTester(tester v1.MonitorTester, allowPrivate bool, limiter *notification.TestLimiter) func(*api.Deps) {
	return func(d *api.Deps) {
		d.MonitorTester, d.MonitorAllowPrivateTargets, d.MonitorTestLimiter = tester, allowPrivate, limiter
	}
}

func realTester() *monitor.Tester {
	return monitor.NewTester(monitor.NewProber(monitor.ProberOptions{AllowPrivate: true}), monitor.MaxConcurrentTests)
}

func TestMonitorTestPermissions(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		authz   roleAuthorizer
		allowed bool
	}{
		{"superadmin", roleAuthorizer{role: user.RoleSuperadmin}, true},
		{"admin", roleAuthorizer{role: user.RoleAdmin}, false},
		{"operator", roleAuthorizer{role: user.RoleOperator}, false},
		{"viewer", roleAuthorizer{role: user.RoleViewer}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			tester := &countingTester{}
			store := newFakeMonitors()
			f := newMonitorFixture(t, c.authz, store, withTester(tester, false, notification.NewTestLimiter(steppingClock())))
			code, body := f.call(http.MethodPost, "/monitors/test", `{"url":"https://a.example.com/"}`)
			if c.allowed && code != http.StatusOK || !c.allowed && code != http.StatusForbidden {
				t.Fatalf("status %d %s", code, body)
			}
			if !c.allowed && (tester.count() != 0 || store.calls != 0) {
				t.Error("a refused test reached the tester or the store")
			}
		})
	}
}

// A draft test through the real prober: the response has exactly the four
// fields, the secrets reach the site and nothing else, and the audit line
// names only the host.
func TestMonitorTestDraftThroughTheProber(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotAuth, gotKey string
	status := http.StatusOK
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotAuth, gotKey = r.Header.Get("Authorization"), r.Header.Get("X-Api-Key")
		w.WriteHeader(status)
		_, _ = fmt.Fprintf(w, "body %s", secretMarker)
	}))
	t.Cleanup(site.Close)

	store := newFakeMonitors()
	f := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, store,
		withTester(realTester(), true, notification.NewTestLimiter(steppingClock())))
	draft := `{"url":"` + site.URL + `/private/path?token=` + secretMarker + `q","auth_type":"bearer","bearer_token":"` +
		secretMarker + `-tok","headers":[{"name":"X-Api-Key","value":"` + secretMarker + `-hdr"}],"keyword":"body"}`

	code, body := f.call(http.MethodPost, "/monitors/test", draft)
	if code != http.StatusOK {
		t.Fatalf("test: %d %s", code, body)
	}
	var out map[string]any
	_ = json.Unmarshal([]byte(body), &out)
	keys := make([]string, 0, len(out))
	for k := range out {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if strings.Join(keys, ",") != "failure,response_ms,status,status_code" {
		t.Errorf("response fields = %v", keys)
	}
	if out["status"] != "up" || out["status_code"] != 200.0 || out["failure"] != "" || out["response_ms"] == nil {
		t.Errorf("up result = %v", out)
	}
	mu.Lock()
	if gotAuth != "Bearer "+secretMarker+"-tok" || gotKey != secretMarker+"-hdr" {
		t.Errorf("the site got %q and %q", gotAuth, gotKey)
	}
	status = http.StatusServiceUnavailable
	mu.Unlock()

	_, body = f.call(http.MethodPost, "/monitors/test", draft)
	if !strings.Contains(body, `"status":"down"`) || !strings.Contains(body, `"failure":"http_status"`) || !strings.Contains(body, `"status_code":503`) {
		t.Errorf("down result = %s", body)
	}
	if strings.Contains(body, secretMarker) {
		t.Errorf("the response carries the site's body or a secret: %s", body)
	}
	if store.calls != 0 || len(store.views) != 0 {
		t.Error("a draft test touched the store")
	}

	logs := f.logs.String()
	if strings.Contains(logs, secretMarker) || strings.Contains(logs, "/private/path") || strings.Contains(logs, "token=") {
		t.Errorf("logs carry the path, query or a secret: %s", logs)
	}
	if strings.Count(logs, `"msg":"monitor tested"`) != 2 || !strings.Contains(logs, `"host":"127.0.0.1"`) {
		t.Errorf("no audit entry per test with the host: %s", logs)
	}
}

// A test of a saved monitor uses its stored secret where the draft leaves it
// out, its stored configuration where the draft says nothing, and writes
// nothing back.
func TestMonitorTestSavedMonitorMergesStoredSecret(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var got []string
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("X-Api-Key"))
	}))
	t.Cleanup(site.Close)

	store := newFakeMonitors()
	m := monitor.Defaults()
	m.ID, m.Name, m.URL, m.AuthType, m.HeaderNames = "mon-1", "Shop", site.URL+"/saved", monitor.AuthBearer, []string{"X-Api-Key"}
	store.views["mon-1"] = monitor.View{Monitor: m, Live: monitor.Live{Status: monitor.StatusDown, ConsecutiveFailures: 4}}
	stored := monitor.Secret{BearerToken: "stored-tok", Headers: map[string]string{"X-Api-Key": "stored-hdr"}}
	store.secrets["mon-1"] = stored
	f := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, store,
		withTester(realTester(), true, notification.NewTestLimiter(steppingClock())))

	for _, body := range []string{
		`{"monitor_id":"mon-1"}`,
		`{"monitor_id":"mon-1","url":"` + site.URL + `/edited","headers":[{"name":"X-Api-Key","value":null}]}`,
		`{"monitor_id":"mon-1","bearer_token":"draft-tok"}`,
	} {
		if code, out := f.call(http.MethodPost, "/monitors/test", body); code != http.StatusOK || !strings.Contains(out, `"status":"up"`) {
			t.Fatalf("%s: %d %s", body, code, out)
		}
	}
	want := []string{
		"GET /saved Bearer stored-tok stored-hdr",
		"GET /edited Bearer stored-tok stored-hdr",
		"GET /saved Bearer draft-tok stored-hdr",
	}
	mu.Lock()
	if !slices.Equal(got, want) {
		t.Errorf("the site saw\n%v\nwant\n%v", got, want)
	}
	mu.Unlock()
	v := store.views["mon-1"]
	if v.URL != site.URL+"/saved" || v.Live.Status != monitor.StatusDown || v.Live.ConsecutiveFailures != 4 ||
		store.secrets["mon-1"].BearerToken != "stored-tok" {
		t.Errorf("a test changed the saved monitor: %+v", v)
	}

	if code, _ := f.call(http.MethodPost, "/monitors/test", `{"monitor_id":"mon-404"}`); code != http.StatusNotFound {
		t.Errorf("unknown monitor_id: %d", code)
	}
}

// The address guard of a real check: refused at validation without
// reaching the tester, or refused at the dial on a redirect.
func TestMonitorTestRefusesUnsafeTargets(t *testing.T) {
	t.Parallel()
	tester := &countingTester{}
	f := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, newFakeMonitors(),
		withTester(tester, false, notification.NewTestLimiter(steppingClock())))
	for _, u := range []string{
		"http://10.0.0.5/", "http://127.0.0.1:8080/", "http://localhost/", "http://169.254.169.254/latest/meta-data/",
		"http://[fd00::1]/", "http://2130706433/", "https://user:pass@example.com/", "ftp://example.com/",
	} {
		code, body := f.call(http.MethodPost, "/monitors/test", `{"url":"`+u+`"}`)
		if code != http.StatusBadRequest || !strings.Contains(body, "url") {
			t.Errorf("%s: %d %s", u, code, body)
		}
		if strings.Contains(body, "pass@") {
			t.Errorf("%s: the error echoes the URL: %s", u, body)
		}
	}
	if tester.count() != 0 {
		t.Errorf("%d unsafe targets reached the tester", tester.count())
	}

	// Even with private targets allowed, a redirect to the metadata address
	// is refused at the dial.
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	t.Cleanup(site.Close)
	g := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, newFakeMonitors(),
		withTester(realTester(), true, notification.NewTestLimiter(steppingClock())))
	_, body := g.call(http.MethodPost, "/monitors/test", `{"url":"`+site.URL+`/"}`)
	if !strings.Contains(body, `"status":"down"`) || !strings.Contains(body, `"failure":"blocked"`) {
		t.Errorf("redirect to the metadata address: %s", body)
	}
}

func TestMonitorTestLimits(t *testing.T) {
	t.Parallel()
	tester := &countingTester{}
	f := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, newFakeMonitors(),
		withTester(tester, false, notification.NewTestLimiter(time.Now)))

	// The same rules as a saved check: no timeout above the maximum, none
	// at or above the interval.
	for body, field := range map[string]string{
		`{"url":"https://a.example.com/","timeout_seconds":3600,"interval_seconds":86400}`: "timeout_seconds",
		`{"url":"https://a.example.com/","timeout_seconds":60}`:                            "timeout_seconds",
		`{"url":"https://a.example.com/","max_redirects":50}`:                              "max_redirects",
		`{"url":"https://a.example.com/","bogus":1}`:                                       "",
		`{}`: "url",
	} {
		code, out := f.call(http.MethodPost, "/monitors/test", body)
		if code != http.StatusBadRequest || field != "" && !strings.Contains(out, field) {
			t.Errorf("%s: %d %s", body, code, out)
		}
	}

	// Per target: a second test of the same host at once is refused.
	if code, out := f.call(http.MethodPost, "/monitors/test", `{"url":"https://b.example.com/one"}`); code != http.StatusOK {
		t.Fatalf("first test: %d %s", code, out)
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, f.url+"/api/v1/monitors/test", strings.NewReader(`{"url":"https://B.example.com/two"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Errorf("second test of the host: %d, Retry-After %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	if tester.count() != 1 {
		t.Errorf("tester ran %d times", tester.count())
	}
	if !strings.Contains(f.logs.String(), `"outcome":"rate_limited"`) {
		t.Error("a refused test is not audited")
	}

	// Every slot busy: refused, not queued.
	busy := &countingTester{err: errs.New(errs.CodeRateLimited, "too many website tests are running; try again shortly")}
	g := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, newFakeMonitors(),
		withTester(busy, false, notification.NewTestLimiter(steppingClock())))
	if code, _ := g.call(http.MethodPost, "/monitors/test", `{"url":"https://c.example.com/"}`); code != http.StatusTooManyRequests {
		t.Errorf("busy tester: %d", code)
	}

	// No tester wired: 503.
	h := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, newFakeMonitors())
	if code, _ := h.call(http.MethodPost, "/monitors/test", `{"url":"https://c.example.com/"}`); code != http.StatusServiceUnavailable {
		t.Errorf("no tester: %d", code)
	}
}
