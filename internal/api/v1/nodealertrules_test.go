package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

// fakeNodeAlertRules is an in-memory [v1.NodeAlertRuleStore] knowing node-1.
type fakeNodeAlertRules struct {
	mu        sync.Mutex
	overrides map[notification.Trigger]bool
	calls     int
	actors    []string
}

func (f *fakeNodeAlertRules) rules(nodeID string) ([]notification.NodeRule, error) {
	f.calls++
	if nodeID != "node-1" {
		return nil, errs.New(errs.CodeNotFound, "node not found")
	}
	yes := true
	var specs []notification.RuleSpec
	for _, e := range notification.Catalog {
		specs = append(specs, notification.RuleSpec{Trigger: e.Key, Enabled: &yes})
	}
	for k, v := range f.overrides {
		specs = append(specs, notification.RuleSpec{Trigger: k, NodeID: nodeID, Enabled: &v})
	}
	return notification.NodeRules(specs, nodeID), nil
}

func (f *fakeNodeAlertRules) one(nodeID string, key notification.Trigger) (notification.NodeRule, error) {
	all, err := f.rules(nodeID)
	if err != nil {
		return notification.NodeRule{}, err
	}
	for _, r := range all {
		if r.Key == key {
			return r, nil
		}
	}
	return notification.NodeRule{}, errs.New(errs.CodeNotFound, "unknown rule")
}

func (f *fakeNodeAlertRules) ListNodeRules(_ context.Context, nodeID string) ([]notification.NodeRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules(nodeID)
}

func (f *fakeNodeAlertRules) SetNodeRuleEnabled(_ context.Context, nodeID string, key notification.Trigger, enabled bool, by string) (notification.NodeRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if nodeID == "node-1" {
		f.overrides[key] = enabled
		f.actors = append(f.actors, by)
	}
	return f.one(nodeID, key)
}

func (f *fakeNodeAlertRules) ClearNodeRuleEnabled(_ context.Context, nodeID string, key notification.Trigger, by string) (notification.NodeRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if nodeID == "node-1" {
		delete(f.overrides, key)
		f.actors = append(f.actors, by)
	}
	return f.one(nodeID, key)
}

var _ v1.NodeAlertRuleStore = (*fakeNodeAlertRules)(nil)

func nodeAlertRuleServer(t *testing.T, authz v1.Authorizer, store *fakeNodeAlertRules) (string, *http.Client) {
	t.Helper()
	u := testUser(t, "node-rules", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, authz, nil, func(d *api.Deps) { d.NodeAlertRules = store })
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, u.Username, "correct horse battery staple").Body.Close()
	return srv.URL, client
}

func nodeAlertRuleActions(nodeID string) []nodeAction {
	p := "/nodes/" + nodeID + "/alert-rules"
	return []nodeAction{
		{guardAction{"list", http.MethodGet, p, ""}, http.StatusOK},
		{guardAction{"set", http.MethodPut, p + "/disk_pct", `{"enabled":false}`}, http.StatusOK},
		{guardAction{"reset", http.MethodDelete, p + "/disk_pct", ""}, http.StatusNoContent},
	}
}

func TestNodeAlertRuleEndpointsRequireNotificationsManageFleetWide(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		authz roleAuthorizer
		allow bool
	}{
		{"superadmin", roleAuthorizer{role: user.RoleSuperadmin}, true},
		{"superadmin on that node only", roleAuthorizer{role: user.RoleSuperadmin, nodeID: "node-1"}, false},
		{"admin", roleAuthorizer{role: user.RoleAdmin}, false},
		{"operator", roleAuthorizer{role: user.RoleOperator}, false},
		{"viewer", roleAuthorizer{role: user.RoleViewer}, false},
	} {
		for _, a := range nodeAlertRuleActions("node-1") {
			t.Run(c.name+"/"+a.name, func(t *testing.T) {
				t.Parallel()
				store := &fakeNodeAlertRules{overrides: map[notification.Trigger]bool{}}
				base, client := nodeAlertRuleServer(t, c.authz, store)
				want := http.StatusForbidden
				if c.allow {
					want = a.ok
				}
				if code := do(t, client, base, a.guardAction); code != want {
					t.Fatalf("status = %d, want %d", code, want)
				}
				if !c.allow && store.calls != 0 {
					t.Error("a refused call reached the store")
				}
			})
		}
	}
}

func TestNodeAlertRuleEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()
	base, _ := nodeAlertRuleServer(t, roleAuthorizer{role: user.RoleSuperadmin}, &fakeNodeAlertRules{overrides: map[notification.Trigger]bool{}})
	for _, a := range nodeAlertRuleActions("node-1") {
		if code := do(t, http.DefaultClient, base, a.guardAction); code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", a.name, code)
		}
	}
}

func TestNodeAlertRuleValidation(t *testing.T) {
	t.Parallel()
	store := &fakeNodeAlertRules{overrides: map[notification.Trigger]bool{}}
	base, client := nodeAlertRuleServer(t, roleAuthorizer{role: user.RoleSuperadmin}, store)
	for _, c := range []struct {
		guardAction
		want int
	}{
		{guardAction{"unknown key", http.MethodPut, "/nodes/node-1/alert-rules/cpu_pct", `{"enabled":true}`}, http.StatusNotFound},
		{guardAction{"unknown key reset", http.MethodDelete, "/nodes/node-1/alert-rules/cpu_pct", ""}, http.StatusNotFound},
		{guardAction{"no body", http.MethodPut, "/nodes/node-1/alert-rules/disk_pct", ""}, http.StatusBadRequest},
		{guardAction{"missing enabled", http.MethodPut, "/nodes/node-1/alert-rules/disk_pct", `{}`}, http.StatusBadRequest},
		{guardAction{"wrong type", http.MethodPut, "/nodes/node-1/alert-rules/disk_pct", `{"enabled":"false"}`}, http.StatusBadRequest},
		{guardAction{"extra field", http.MethodPut, "/nodes/node-1/alert-rules/disk_pct", `{"enabled":false,"x":1}`}, http.StatusBadRequest},
		{guardAction{"unknown node", http.MethodGet, "/nodes/nope/alert-rules", ""}, http.StatusNotFound},
		{guardAction{"unknown node set", http.MethodPut, "/nodes/nope/alert-rules/disk_pct", `{"enabled":false}`}, http.StatusNotFound},
	} {
		if code := do(t, client, base, c.guardAction); code != c.want {
			t.Errorf("%s: status = %d, want %d", c.name, code, c.want)
		}
	}
	if len(store.overrides) != 0 {
		t.Errorf("a rejected call changed overrides: %v", store.overrides)
	}
}

func TestNodeAlertRuleListShowsEffectiveValueAndSource(t *testing.T) {
	t.Parallel()
	store := &fakeNodeAlertRules{overrides: map[notification.Trigger]bool{}}
	base, client := nodeAlertRuleServer(t, roleAuthorizer{role: user.RoleSuperadmin}, store)
	if code := do(t, client, base, guardAction{"set", http.MethodPut, "/nodes/node-1/alert-rules/ram_pct", `{"enabled":false}`}); code != http.StatusOK {
		t.Fatalf("set: %d", code)
	}
	if len(store.actors) != 1 || store.actors[0] != "node-rules" {
		t.Errorf("updated_by = %v, want the signed-in username", store.actors)
	}

	resp, err := client.Get(base + "/api/v1/nodes/node-1/alert-rules")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		NodeID string           `json:"node_id"`
		Rules  []map[string]any `json:"rules"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.NodeID != "node-1" || len(out.Rules) != len(notification.Catalog) {
		t.Fatalf("response = %+v", out)
	}
	for _, r := range out.Rules {
		for _, field := range []string{"key", "area", "title", "description", "default_severity", "default_enabled",
			"global_enabled", "override", "enabled", "source"} {
			if _, ok := r[field]; !ok {
				t.Errorf("rule %v lacks %q", r["key"], field)
			}
		}
		switch r["key"] {
		case "ram_pct":
			if r["enabled"] != false || r["override"] != false || r["source"] != "node" {
				t.Errorf("ram_pct = %v", r)
			}
		default:
			if r["enabled"] != true || r["override"] != nil || r["source"] != "global" {
				t.Errorf("%v = %v", r["key"], r)
			}
		}
	}

	for range 2 {
		if code := do(t, client, base, guardAction{"reset", http.MethodDelete, "/nodes/node-1/alert-rules/ram_pct", ""}); code != http.StatusNoContent {
			t.Errorf("reset: %d, want 204 whether or not an override exists", code)
		}
	}
	if len(store.overrides) != 0 {
		t.Errorf("overrides = %v after reset", store.overrides)
	}
}
