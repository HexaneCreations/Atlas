package v1_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

// fakeNodeAlerts is an in-memory [v1.NodeAlertStore] knowing node-1 only.
type fakeNodeAlerts struct {
	state notification.NodeAlerts
	calls int
}

func (f *fakeNodeAlerts) known(nodeID string) error {
	f.calls++
	if nodeID != "node-1" {
		return errs.New(errs.CodeNotFound, "node not found")
	}
	return nil
}

func (f *fakeNodeAlerts) MuteNode(_ context.Context, nodeID string, until time.Time) (notification.NodeAlerts, error) {
	if err := f.known(nodeID); err != nil {
		return notification.NodeAlerts{}, err
	}
	// A driver may hand the time back in any zone.
	local := until.In(time.FixedZone("z", 14*3600))
	f.state.MutedUntil = &local
	return f.state, nil
}
func (f *fakeNodeAlerts) EndMuteExplicitly(_ context.Context, nodeID string) (bool, error) {
	if f.known(nodeID) != nil {
		return false, nil
	}
	f.state.MutedUntil = nil
	return true, nil
}
func (f *fakeNodeAlerts) ArchiveNode(_ context.Context, nodeID string, at time.Time) (notification.NodeAlerts, error) {
	if err := f.known(nodeID); err != nil {
		return notification.NodeAlerts{}, err
	}
	f.state.ArchivedAt = &at
	return f.state, nil
}
func (f *fakeNodeAlerts) UnarchiveNode(_ context.Context, nodeID string) error {
	if err := f.known(nodeID); err != nil {
		return err
	}
	f.state.ArchivedAt = nil
	return nil
}

var _ v1.NodeAlertStore = (*fakeNodeAlerts)(nil)

type nodeAction struct {
	guardAction
	ok int
}

func nodeAlertActions(nodeID string) []nodeAction {
	p := "/nodes/" + nodeID
	return []nodeAction{
		{guardAction{"mute", http.MethodPut, p + "/alerts-mute", `{"duration":"1h"}`}, http.StatusOK},
		{guardAction{"unmute", http.MethodDelete, p + "/alerts-mute", ""}, http.StatusNoContent},
		{guardAction{"archive", http.MethodPost, p + "/archive", ""}, http.StatusOK},
		{guardAction{"unarchive", http.MethodDelete, p + "/archive", ""}, http.StatusNoContent},
	}
}

// nodeAlertServer runs without a notification store: muting and archiving
// need no encryption key.
func nodeAlertServer(t *testing.T, authz v1.Authorizer, store *fakeNodeAlerts) (string, *http.Client) {
	t.Helper()
	u := testUser(t, "node-alerts", "correct horse battery staple")
	srv := newWriteEndpointTestServer(t, u, authz, nil, func(d *api.Deps) { d.NodeAlerts = store })
	client := mustClientWithCookies(t)
	login(t, client, srv.URL, u.Username, "correct horse battery staple").Body.Close()
	return srv.URL, client
}

func TestNodeAlertEndpointsRequireNotificationsManageFleetWide(t *testing.T) {
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
		for _, a := range nodeAlertActions("node-1") {
			t.Run(c.name+"/"+a.name, func(t *testing.T) {
				t.Parallel()
				store := &fakeNodeAlerts{}
				base, client := nodeAlertServer(t, c.authz, store)
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

func TestNodeAlertEndpointsRequireAuthentication(t *testing.T) {
	t.Parallel()
	store := &fakeNodeAlerts{}
	base, _ := nodeAlertServer(t, roleAuthorizer{role: user.RoleSuperadmin}, store)
	for _, a := range nodeAlertActions("node-1") {
		if code := do(t, http.DefaultClient, base, a.guardAction); code != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", a.name, code)
		}
	}
}

func TestNodeAlertEndpointsUnknownNodeIs404(t *testing.T) {
	t.Parallel()
	base, client := nodeAlertServer(t, roleAuthorizer{role: user.RoleSuperadmin}, &fakeNodeAlerts{})
	for _, a := range nodeAlertActions("nope") {
		if code := do(t, client, base, a.guardAction); code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", a.name, code)
		}
	}
}

func mute(t *testing.T, client *http.Client, base, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPut, base+"/api/v1/nodes/node-1/alerts-mute", strings.NewReader(body))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func TestMuteDurations(t *testing.T) {
	t.Parallel()
	store := &fakeNodeAlerts{}
	base, client := nodeAlertServer(t, roleAuthorizer{role: user.RoleSuperadmin}, store)

	// In this order each call replaces a longer or a shorter window.
	for _, c := range []struct {
		name string
		want time.Duration
	}{{"4h", 4 * time.Hour}, {"1h", time.Hour}, {"7d", 7 * 24 * time.Hour}, {"24h", 24 * time.Hour}} {
		code, out := mute(t, client, base, `{"duration":"`+c.name+`"}`)
		until, err := time.Parse(time.RFC3339Nano, out["alerts_muted_until"].(string))
		if code != http.StatusOK || err != nil || (time.Until(until)-c.want).Abs() > time.Minute {
			t.Errorf("%s: %d %v, want ~%v ahead", c.name, code, out, c.want)
		}
		if out["node_id"] != "node-1" || out["archived_at"] != nil {
			t.Errorf("%s: %v", c.name, out)
		}
	}
	if code, out := mute(t, client, base, `{"duration":"until_unmuted"}`); code != http.StatusOK || out["alerts_muted_until"] != "9999-01-01T00:00:00Z" {
		t.Errorf("until_unmuted: %d %v, want exactly 9999-01-01T00:00:00Z", code, out)
	}

	calls := store.calls
	for _, body := range []string{
		``, `{}`, `{"duration":""}`, `{"duration":"0"}`, `{"duration":"999d"}`, `{"duration":"-1h"}`,
		`{"duration":3600}`, `{"duration":"1h","until":"2030-01-01T00:00:00Z"}`,
	} {
		if code, _ := mute(t, client, base, body); code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", body, code)
		}
	}
	if store.calls != calls {
		t.Error("a rejected mute reached the store")
	}
}

// The node DTO gains two always-present nullable fields and loses nothing.
func TestNodeResponseCarriesMuteAndArchive(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(v1.NodeResponse{})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	for _, name := range []string{"alerts_muted_until", "archived_at"} {
		if v, present := fields[name]; !present || v != nil {
			t.Errorf("%s = %v (present %v), want null", name, v, present)
		}
	}
}
