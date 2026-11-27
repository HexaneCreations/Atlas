package v1_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
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
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/log"
)

// fakeMonitors is an in-memory [v1.MonitorStore].
type fakeMonitors struct {
	mu      sync.Mutex
	views   map[string]monitor.View
	secrets map[string]monitor.Secret
	groups  []monitor.Group
	calls   int
	next    int
}

func newFakeMonitors() *fakeMonitors {
	return &fakeMonitors{views: map[string]monitor.View{}, secrets: map[string]monitor.Secret{}}
}

func (f *fakeMonitors) touch() { f.calls++ }

func (f *fakeMonitors) get(id string) (monitor.View, error) {
	v, ok := f.views[id]
	if !ok {
		return monitor.View{}, errs.New(errs.CodeNotFound, "monitor not found")
	}
	return v, nil
}

func (f *fakeMonitors) ListMonitors(context.Context) ([]monitor.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	out := []monitor.View{}
	for _, v := range f.views {
		out = append(out, v)
	}
	return out, nil
}
func (f *fakeMonitors) GetMonitor(_ context.Context, id string) (monitor.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	return f.get(id)
}
func (f *fakeMonitors) StoredMonitor(_ context.Context, id string) (monitor.Monitor, monitor.Secret, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	v, err := f.get(id)
	return v.Monitor, f.secrets[id], err
}
func (f *fakeMonitors) CreateMonitor(_ context.Context, m monitor.Monitor, s monitor.Secret, by string) (monitor.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	f.next++
	m.ID = "mon-" + string(rune('0'+f.next))
	m.SecretConfigured, m.CreatedBy, m.UpdatedBy = !s.Empty(), by, by
	v := monitor.View{Monitor: m, Live: monitor.Live{Status: monitor.StatusUnknown}}
	f.views[m.ID], f.secrets[m.ID] = v, s
	return v, nil
}
func (f *fakeMonitors) UpdateMonitor(_ context.Context, id string, apply monitor.UpdateFunc, by string) (monitor.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	v, err := f.get(id)
	if err != nil {
		return monitor.View{}, err
	}
	m, s, err := apply(v.Monitor, f.secrets[id])
	if err != nil {
		return monitor.View{}, err
	}
	m.ID, m.SecretConfigured, m.UpdatedBy = id, !s.Empty(), by
	v.Monitor = m
	f.views[id], f.secrets[id] = v, s
	return v, nil
}
func (f *fakeMonitors) SetMonitorActive(_ context.Context, id string, active bool, _ string) (monitor.View, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	v, err := f.get(id)
	if err != nil {
		return monitor.View{}, err
	}
	v.Active = active
	f.views[id] = v
	return v, nil
}
func (f *fakeMonitors) DeleteMonitor(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	if _, err := f.get(id); err != nil {
		return err
	}
	delete(f.views, id)
	return nil
}
func (f *fakeMonitors) RecentHeartbeats(context.Context, []string, int) (map[string][]monitor.Heartbeat, error) {
	return map[string][]monitor.Heartbeat{}, nil
}
func (f *fakeMonitors) UptimeSince(context.Context, []string, time.Time) (map[string]monitor.UptimeCount, error) {
	return map[string]monitor.UptimeCount{}, nil
}
func (f *fakeMonitors) Uptime(context.Context, string, time.Time) (monitor.Uptime, error) {
	return monitor.Uptime{Day: monitor.UptimeCount{Up: 3, Counted: 4}}, nil
}
func (f *fakeMonitors) Heartbeats(context.Context, string, time.Time, int) ([]monitor.Heartbeat, error) {
	return []monitor.Heartbeat{}, nil
}
func (f *fakeMonitors) HourlyHistory(context.Context, string, time.Time) ([]monitor.HourlyBucket, error) {
	return []monitor.HourlyBucket{}, nil
}
func (f *fakeMonitors) Events(context.Context, string, time.Time, int) ([]monitor.Event, error) {
	return []monitor.Event{}, nil
}
func (f *fakeMonitors) ListGroups(context.Context) ([]monitor.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	return append([]monitor.Group{}, f.groups...), nil
}
func (f *fakeMonitors) CreateGroup(_ context.Context, g monitor.Group) (monitor.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	g.ID = "grp-1"
	f.groups = append(f.groups, g)
	return g, nil
}
func (f *fakeMonitors) UpdateGroup(_ context.Context, g monitor.Group) (monitor.Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	return g, nil
}
func (f *fakeMonitors) DeleteGroup(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.touch()
	return nil
}

var _ v1.MonitorStore = (*fakeMonitors)(nil)

type monitorFixture struct {
	t      *testing.T
	url    string
	client *http.Client
	store  *fakeMonitors
	logs   *lockedBuffer
}

func newMonitorFixture(t *testing.T, authz v1.Authorizer, store *fakeMonitors, opts ...func(*api.Deps)) *monitorFixture {
	t.Helper()
	u := testUser(t, "web-admin", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, authz, nil, append([]func(*api.Deps){func(d *api.Deps) {
		if store != nil {
			d.Monitors = store
		}
	}}, opts...)...)
	f := &monitorFixture{t: t, store: store, logs: &lockedBuffer{}}
	logger := slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		srv.Config.Handler.ServeHTTP(w, r.WithContext(log.Into(r.Context(), logger)))
	}))
	t.Cleanup(front.Close)
	f.url = front.URL
	f.client = mustClientWithCookies(t)
	login(t, f.client, f.url, u.Username, "correct horse battery staple").Body.Close()
	return f
}

func (f *monitorFixture) call(method, path, body string) (int, string) {
	f.t.Helper()
	req, err := http.NewRequest(method, f.url+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := f.client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

const secretMarker = "s3ntinel-c2d9"

var fullMonitorBody = `{"name":"Shop","url":"https://shop.example.com/health?token=` + secretMarker + `q","method":"POST",
	"auth_type":"bearer","bearer_token":"` + secretMarker + `-tok","body":"{\"k\":\"` + secretMarker + `\"}",
	"headers":[{"name":"x-api-key","value":"` + secretMarker + `-hdr"}],"tags":["prod"]}`

func TestMonitorEndpointPermissions(t *testing.T) {
	t.Parallel()
	reads := []guardAction{
		{"list", http.MethodGet, "/monitors", ""},
		{"get", http.MethodGet, "/monitors/mon-1", ""},
		{"heartbeats", http.MethodGet, "/monitors/mon-1/heartbeats", ""},
		{"history", http.MethodGet, "/monitors/mon-1/history", ""},
		{"events", http.MethodGet, "/monitors/mon-1/events", ""},
		{"groups", http.MethodGet, "/monitor-groups", ""},
	}
	writes := []guardAction{
		{"create", http.MethodPost, "/monitors", `{"name":"A","url":"https://a.example.com/"}`},
		{"update", http.MethodPut, "/monitors/mon-1", `{"name":"B"}`},
		{"active", http.MethodPut, "/monitors/mon-1/active", `{"active":false}`},
		{"delete", http.MethodDelete, "/monitors/mon-1", ""},
		{"create group", http.MethodPost, "/monitor-groups", `{"name":"G"}`},
		{"update group", http.MethodPut, "/monitor-groups/grp-1", `{"name":"H"}`},
		{"delete group", http.MethodDelete, "/monitor-groups/grp-1", ""},
	}
	for _, c := range []struct {
		name        string
		authz       roleAuthorizer
		read, write bool
	}{
		{"superadmin", roleAuthorizer{role: user.RoleSuperadmin}, true, true},
		{"admin", roleAuthorizer{role: user.RoleAdmin}, true, false},
		{"operator", roleAuthorizer{role: user.RoleOperator}, true, false},
		{"viewer", roleAuthorizer{role: user.RoleViewer}, true, false},
		{"viewer of one node", roleAuthorizer{role: user.RoleViewer, nodeID: "node-1"}, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			for _, a := range append(reads, writes...) {
				store := newFakeMonitors()
				store.views["mon-1"] = monitor.View{Monitor: monitor.Monitor{ID: "mon-1", Name: "One", URL: "https://one.example.com/"}}
				store.groups = []monitor.Group{{ID: "grp-1", Name: "G"}}
				f := newMonitorFixture(t, c.authz, store)
				allowed := c.read
				if strings.HasPrefix(a.method, "P") || a.method == http.MethodDelete {
					allowed = c.write
				}
				code := do(t, f.client, f.url, a)
				if allowed && (code == http.StatusForbidden || code >= 500) || !allowed && code != http.StatusForbidden {
					t.Errorf("%s: status %d, allowed=%v", a.name, code, allowed)
				}
				if !allowed && store.calls != 0 {
					t.Errorf("%s: a refused call reached the store", a.name)
				}
			}
		})
	}
	f := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, newFakeMonitors())
	for _, a := range append(reads, writes...) {
		if code := do(t, http.DefaultClient, f.url, a); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", a.name, code)
		}
	}
	// No store wired: 503.
	none := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, nil)
	if code, _ := none.call(http.MethodGet, "/monitors", ""); code != http.StatusServiceUnavailable {
		t.Errorf("no store: %d", code)
	}
}

func TestMonitorSecretsNeverLeave(t *testing.T) {
	t.Parallel()
	store := newFakeMonitors()
	admin := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, store)

	code, body := admin.call(http.MethodPost, "/monitors", fullMonitorBody)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var created v1.MonitorResponse
	_ = json.Unmarshal([]byte(body), &created)
	if created.SecretConfigured == nil || !*created.SecretConfigured || strings.Join(created.HeaderNames, ",") != "X-Api-Key" ||
		created.AuthType != "bearer" || created.Host != "shop.example.com" {
		t.Errorf("created = %+v", created)
	}
	if s := store.secrets[created.ID]; s.BearerToken != secretMarker+"-tok" || s.Headers["X-Api-Key"] != secretMarker+"-hdr" {
		t.Error("the secret did not reach the store")
	}

	// Update without secret fields keeps them; nothing echoes them.
	code, updated := admin.call(http.MethodPut, "/monitors/"+created.ID, `{"name":"Shop 2","headers":[{"name":"X-Api-Key","value":null}]}`)
	if code != http.StatusOK || store.secrets[created.ID].Headers["X-Api-Key"] != secretMarker+"-hdr" || store.secrets[created.ID].BearerToken == "" {
		t.Fatalf("update: %d %s", code, updated)
	}
	_, list := admin.call(http.MethodGet, "/monitors", "")
	_, detail := admin.call(http.MethodGet, "/monitors/"+created.ID, "")
	for name, out := range map[string]string{"create": body, "update": updated, "list": list, "detail": detail} {
		for _, leak := range []string{secretMarker + "-tok", secretMarker + "-hdr", `\"k\"`} {
			if strings.Contains(out, leak) {
				t.Errorf("%s response carries a secret: %s", name, out)
			}
		}
	}
	// Logs and audit entries carry the host, never the URL query or a secret.
	logs := admin.logs.String()
	if strings.Contains(logs, secretMarker) {
		t.Errorf("logs carry secret material: %s", logs)
	}
	if !strings.Contains(logs, `"msg":"monitor created"`) || !strings.Contains(logs, `"host":"shop.example.com"`) {
		t.Errorf("no audit entry for the create: %s", logs)
	}

	// A viewer sees status, not the query string, headers or auth.
	viewer := newMonitorFixture(t, roleAuthorizer{role: user.RoleViewer}, store)
	_, vlist := viewer.call(http.MethodGet, "/monitors", "")
	_, vdetail := viewer.call(http.MethodGet, "/monitors/"+created.ID, "")
	for name, out := range map[string]string{"list": vlist, "detail": vdetail} {
		if strings.Contains(out, secretMarker) || strings.Contains(out, "X-Api-Key") || strings.Contains(out, "bearer") ||
			strings.Contains(out, "secret_configured") || strings.Contains(out, "web-admin") {
			t.Errorf("viewer %s shows configuration it must not: %s", name, out)
		}
		if !strings.Contains(out, `"url":"https://shop.example.com/health"`) || !strings.Contains(out, `"redacted":true`) {
			t.Errorf("viewer %s: %s", name, out)
		}
	}
}

func TestMonitorValidation(t *testing.T) {
	t.Parallel()
	store := newFakeMonitors()
	f := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, store)
	for _, c := range []struct{ name, body, field string }{
		{"no name", `{"url":"https://a.example.com/"}`, "name"},
		{"userinfo", `{"name":"a","url":"https://u:p@a.example.com/"}`, "url"},
		{"loopback", `{"name":"a","url":"http://127.0.0.1:8080/"}`, "url"},
		{"metadata", `{"name":"a","url":"http://169.254.169.254/"}`, "url"},
		{"scheme", `{"name":"a","url":"file:///etc/passwd"}`, "url"},
		{"interval", `{"name":"a","url":"https://a.example.com/","interval_seconds":10}`, "interval_seconds"},
		{"status codes", `{"name":"a","url":"https://a.example.com/","accepted_status":["2xx"]}`, "accepted_status"},
		{"host header", `{"name":"a","url":"https://a.example.com/","headers":[{"name":"Host","value":"x"}]}`, "headers"},
		{"bearer without token", `{"name":"a","url":"https://a.example.com/","auth_type":"bearer"}`, "bearer_token"},
		{"body on GET", `{"name":"a","url":"https://a.example.com/","body":"x"}`, "body"},
	} {
		code, body := f.call(http.MethodPost, "/monitors", c.body)
		if code != http.StatusBadRequest || !strings.Contains(body, `"field":"`+c.field+`"`) {
			t.Errorf("%s: %d %s", c.name, code, body)
		}
	}
	if code, _ := f.call(http.MethodPost, "/monitors", `{"name":"a","url":"https://a.example.com/","nope":1}`); code != http.StatusBadRequest {
		t.Errorf("unknown field: %d", code)
	}
	if len(store.views) != 0 {
		t.Error("an invalid monitor was stored")
	}

	code, body := f.call(http.MethodPost, "/monitors", `{"name":"a","url":"https://a.example.com/"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, body)
	}
	var m v1.MonitorResponse
	_ = json.Unmarshal([]byte(body), &m)
	if m.IntervalSeconds != 60 || m.Retries != 3 || strings.Join(m.AcceptedStatus, ",") != "200-299" || !m.Active {
		t.Errorf("defaults = %+v", m)
	}
	if code, body := f.call(http.MethodPut, "/monitors/"+m.ID, `{"active":false}`); code != http.StatusBadRequest {
		t.Errorf("active through PUT: %d %s", code, body)
	}
	if code, _ := f.call(http.MethodPut, "/monitors/"+m.ID+"/active", `{}`); code != http.StatusBadRequest {
		t.Errorf("active without a value: %d", code)
	}
	if code, body := f.call(http.MethodPut, "/monitors/"+m.ID, `{"retries":5}`); code != http.StatusOK || !strings.Contains(body, `"retries":5`) ||
		!strings.Contains(body, `"interval_seconds":60`) {
		t.Errorf("partial update: %d %s", code, body)
	}
	if code, _ := f.call(http.MethodGet, "/monitors?status=sideways", ""); code != http.StatusBadRequest {
		t.Errorf("bad status filter: %d", code)
	}
	if code, _ := f.call(http.MethodGet, "/monitors/"+m.ID+"/history?range=2d", ""); code != http.StatusBadRequest {
		t.Errorf("bad range: %d", code)
	}
	if code, _ := f.call(http.MethodGet, "/monitors/missing", ""); code != http.StatusNotFound {
		t.Errorf("missing: %d", code)
	}
	if code, _ := f.call(http.MethodPost, "/monitor-groups", `{"name":" "}`); code != http.StatusBadRequest {
		t.Errorf("blank group: %d", code)
	}
}

func TestMonitorListFilters(t *testing.T) {
	t.Parallel()
	store := newFakeMonitors()
	for id, v := range map[string]monitor.View{
		"a": {Monitor: monitor.Monitor{ID: "a", Name: "Shop", URL: "https://shop.example.com/", Active: true, Tags: []string{"prod"}, GroupID: "g1"},
			Live: monitor.Live{Status: monitor.StatusDown}},
		"b": {Monitor: monitor.Monitor{ID: "b", Name: "Blog", URL: "https://blog.example.org/", Active: false, Tags: []string{"staging"}},
			Live: monitor.Live{Status: monitor.StatusUp}},
	} {
		store.views[id] = v
	}
	f := newMonitorFixture(t, roleAuthorizer{role: user.RoleViewer}, store)
	for query, want := range map[string]string{
		"": "a,b", "?status=down": "a", "?active=false": "b", "?group_id=g1": "a", "?tag=staging": "b",
		"?q=example.org": "b", "?q=PROD": "a", "?q=nothing": "",
	} {
		_, body := f.call(http.MethodGet, "/monitors"+query, "")
		var out v1.ListMonitorsResponse
		_ = json.Unmarshal([]byte(body), &out)
		var ids []string
		for _, m := range out.Monitors {
			ids = append(ids, m.ID)
		}
		slices.Sort(ids)
		if strings.Join(ids, ",") != want {
			t.Errorf("filter %q = %v, want %s", query, ids, want)
		}
	}
}
