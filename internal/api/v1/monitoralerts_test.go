package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	"github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/user"
)

// fakeMonitorAlerts is an in-memory [v1.MonitorAlertStore].
type fakeMonitorAlerts struct {
	mu        sync.Mutex
	overrides map[notification.Trigger]bool
	muted     *time.Time
	calls     int
}

func (f *fakeMonitorAlerts) rules(monitorID string) []notification.SubjectRule {
	yes := true
	var specs []notification.RuleSpec
	for _, e := range notification.Catalog {
		specs = append(specs, notification.RuleSpec{Trigger: e.Key, Enabled: &yes})
	}
	for k, v := range f.overrides {
		specs = append(specs, notification.RuleSpec{Trigger: k, MonitorID: monitorID, Enabled: &v})
	}
	return notification.MonitorRules(specs, monitorID)
}

func (f *fakeMonitorAlerts) one(monitorID string, key notification.Trigger) notification.SubjectRule {
	for _, r := range f.rules(monitorID) {
		if r.Key == key {
			return r
		}
	}
	return notification.SubjectRule{}
}

func (f *fakeMonitorAlerts) ListMonitorRules(_ context.Context, monitorID string) ([]notification.SubjectRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rules(monitorID), nil
}

func (f *fakeMonitorAlerts) SetMonitorRuleEnabled(_ context.Context, monitorID string, key notification.Trigger, enabled bool, _ string) (notification.SubjectRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.overrides[key] = enabled
	return f.one(monitorID, key), nil
}

func (f *fakeMonitorAlerts) ClearMonitorRuleEnabled(_ context.Context, monitorID string, key notification.Trigger, _ string) (notification.SubjectRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	delete(f.overrides, key)
	return f.one(monitorID, key), nil
}

func (f *fakeMonitorAlerts) MuteMonitor(_ context.Context, _ string, until time.Time) (*time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.muted = &until
	return f.muted, nil
}

func (f *fakeMonitorAlerts) EndMonitorMuteExplicitly(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.muted = nil
	return true, nil
}

const alertMonitorURL = "https://shop.example.com/health?token=" + secretMarker

func newMonitorAlertFixture(t *testing.T, authz roleAuthorizer) (*monitorFixture, *fakeMonitorAlerts) {
	t.Helper()
	store := newFakeMonitors()
	store.views["mon-1"] = monitor.View{Monitor: monitor.Monitor{ID: "mon-1", Name: "Shop", URL: alertMonitorURL}}
	alerts := &fakeMonitorAlerts{overrides: map[notification.Trigger]bool{}}
	f := newMonitorFixture(t, authz, store, func(d *api.Deps) { d.MonitorAlerts = alerts })
	return f, alerts
}

var monitorAlertActions = []guardAction{
	{"list rules", http.MethodGet, "/monitors/mon-1/alert-rules", ""},
	{"set rule", http.MethodPut, "/monitors/mon-1/alert-rules/website_down", `{"enabled":false}`},
	{"reset rule", http.MethodDelete, "/monitors/mon-1/alert-rules/website_down", ""},
	{"mute", http.MethodPut, "/monitors/mon-1/mute", `{"duration":"4h"}`},
	{"unmute", http.MethodDelete, "/monitors/mon-1/mute", ""},
	{"get channels", http.MethodGet, "/monitors/mon-1/channels", ""},
	{"set channels", http.MethodPut, "/monitors/mon-1/channels", `{"channel_ids":["c1"]}`},
}

func TestMonitorAlertEndpointsNeedNotificationsManage(t *testing.T) {
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
			for _, a := range monitorAlertActions {
				f, alerts := newMonitorAlertFixture(t, c.authz)
				code := do(t, f.client, f.url, a)
				if c.allowed && code >= 300 || !c.allowed && code != http.StatusForbidden {
					t.Errorf("%s: status %d, allowed=%v", a.name, code, c.allowed)
				}
				if !c.allowed && (alerts.calls != 0 || f.store.calls != 0) {
					t.Errorf("%s: a refused call reached a store", a.name)
				}
			}
		})
	}
	f, _ := newMonitorAlertFixture(t, roleAuthorizer{role: user.RoleSuperadmin})
	for _, a := range monitorAlertActions {
		if code := do(t, http.DefaultClient, f.url, a); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s: %d", a.name, code)
		}
	}
	none := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, newFakeMonitors())
	if code, _ := none.call(http.MethodGet, "/monitors/mon-1/alert-rules", ""); code != http.StatusServiceUnavailable {
		t.Errorf("no alert store: %d", code)
	}
}

func TestMonitorAlertValidation(t *testing.T) {
	t.Parallel()
	f, alerts := newMonitorAlertFixture(t, roleAuthorizer{role: user.RoleSuperadmin})
	for _, c := range []struct {
		guardAction
		want int
	}{
		{guardAction{"node rule key", http.MethodPut, "/monitors/mon-1/alert-rules/disk_pct", `{"enabled":false}`}, http.StatusNotFound},
		{guardAction{"unknown key", http.MethodDelete, "/monitors/mon-1/alert-rules/cpu_pct", ""}, http.StatusNotFound},
		{guardAction{"missing enabled", http.MethodPut, "/monitors/mon-1/alert-rules/website_down", `{}`}, http.StatusBadRequest},
		{guardAction{"extra field", http.MethodPut, "/monitors/mon-1/alert-rules/website_down", `{"enabled":true,"x":1}`}, http.StatusBadRequest},
		{guardAction{"unknown monitor", http.MethodGet, "/monitors/nope/alert-rules", ""}, http.StatusNotFound},
		{guardAction{"unknown monitor mute", http.MethodPut, "/monitors/nope/mute", `{"duration":"1h"}`}, http.StatusNotFound},
		{guardAction{"bad duration", http.MethodPut, "/monitors/mon-1/mute", `{"duration":"2h"}`}, http.StatusBadRequest},
		{guardAction{"missing channel_ids", http.MethodPut, "/monitors/mon-1/channels", `{}`}, http.StatusBadRequest},
		{guardAction{"empty channel id", http.MethodPut, "/monitors/mon-1/channels", `{"channel_ids":[""]}`}, http.StatusBadRequest},
		{guardAction{"bad channel_ids on create", http.MethodPost, "/monitors", `{"name":"A","url":"https://a.example.com/","channel_ids":[""]}`}, http.StatusBadRequest},
	} {
		if code := do(t, f.client, f.url, c.guardAction); code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, code, c.want)
		}
	}
	if len(alerts.overrides) != 0 || alerts.muted != nil {
		t.Errorf("a rejected call changed state: %v %v", alerts.overrides, alerts.muted)
	}
}

func TestMonitorAlertRulesListOnlyMonitorRulesAndAuditTheHostOnly(t *testing.T) {
	t.Parallel()
	f, _ := newMonitorAlertFixture(t, roleAuthorizer{role: user.RoleSuperadmin})
	code, body := f.call(http.MethodPut, "/monitors/mon-1/alert-rules/website_cert_expiry", `{"enabled":false}`)
	if code != http.StatusOK || !strings.Contains(body, `"source":"monitor"`) {
		t.Fatalf("set = %d %s", code, body)
	}
	code, body = f.call(http.MethodGet, "/monitors/mon-1/alert-rules", "")
	var out struct {
		MonitorID string           `json:"monitor_id"`
		Rules     []map[string]any `json:"rules"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &out) != nil || out.MonitorID != "mon-1" || len(out.Rules) != 2 {
		t.Fatalf("list = %d %s", code, body)
	}
	for _, r := range out.Rules {
		if key, _ := r["key"].(string); notification.Trigger(key).SubjectKind() != notification.SubjectMonitor {
			t.Errorf("monitor endpoint lists node rule %q", key)
		}
	}
	if code, _ := f.call(http.MethodPut, "/monitors/mon-1/mute", `{"duration":"until_unmuted"}`); code != http.StatusOK {
		t.Errorf("mute = %d", code)
	}
	logs := f.logs.String()
	for _, want := range []string{"monitor alert rule set", "monitor alerts muted", `"host":"shop.example.com"`} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs lack %s", want)
		}
	}
	if strings.Contains(logs, secretMarker) || strings.Contains(logs, "/health") {
		t.Error("an audit line carries more of the URL than its host")
	}
}

func TestMonitorChannelSelection(t *testing.T) {
	t.Parallel()
	store := newFakeMonitors()
	store.defaultChannels = []string{"c-default"}
	admin := newMonitorFixture(t, roleAuthorizer{role: user.RoleSuperadmin}, store)

	_, body := admin.call(http.MethodPost, "/monitors", `{"name":"A","url":"https://a.example.com/"}`)
	var created struct {
		ID                      string   `json:"id"`
		ChannelIDs              []string `json:"channel_ids"`
		NotificationsConfigured bool     `json:"notifications_configured"`
	}
	if json.Unmarshal([]byte(body), &created) != nil || len(created.ChannelIDs) != 1 || created.ChannelIDs[0] != "c-default" ||
		!created.NotificationsConfigured {
		t.Fatalf("a new monitor did not get the default channels: %s", body)
	}
	_, body = admin.call(http.MethodPost, "/monitors", `{"name":"B","url":"https://b.example.com/","channel_ids":[]}`)
	if !strings.Contains(body, `"notifications_configured":false`) || !strings.Contains(body, `"channel_ids":[]`) {
		t.Errorf("an explicit empty selection was not kept: %s", body)
	}

	if code, body := admin.call(http.MethodPut, "/monitors/"+created.ID, `{"name":"A2"}`); code != http.StatusOK || !strings.Contains(body, `"channel_ids":["c-default"]`) {
		t.Errorf("an update without channel_ids changed the selection: %d %s", code, body)
	}
	if code, body := admin.call(http.MethodPut, "/monitors/"+created.ID+"/channels", `{"channel_ids":[]}`); code != http.StatusOK || !strings.Contains(body, `"channel_ids":[]`) {
		t.Errorf("clear = %d %s", code, body)
	}

	viewer := newMonitorFixture(t, roleAuthorizer{role: user.RoleViewer}, store)
	_, list := viewer.call(http.MethodGet, "/monitors", "")
	if strings.Contains(list, "channel_ids") || !strings.Contains(list, "notifications_configured") {
		t.Errorf("viewer list = %s", list)
	}
}
