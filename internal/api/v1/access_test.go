package v1_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/core/access"
	"github.com/hexane/atlas/internal/core/pageauthz"
)

func doBody(t *testing.T, client *http.Client, baseURL string, a guardAction) (int, string) {
	t.Helper()
	req, err := http.NewRequest(a.method, baseURL+"/api/v1"+a.path, strings.NewReader(a.body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

var emptyVersion = access.State{}.Version()

// Nobody edits their own access through the editor, the superadmin included.
func TestPutUserAccessRefusesTheCallersOwnAccount(t *testing.T) {
	t.Parallel()
	for _, super := range []bool{false, true} {
		base, admin, client := newSuperadminGuardServer(t, super)
		var actorID string
		for id, u := range admin.users {
			if u.Username == "guarded-actor" {
				actorID = id
			}
		}
		code, body := doBody(t, client, base, guardAction{method: http.MethodPut, path: "/users/" + actorID + "/access",
			body: `{"version":"` + emptyVersion + `","roles":[],"pages":[]}`})
		if code != http.StatusForbidden || !strings.Contains(body, "you cannot change your own access") {
			t.Errorf("superadmin=%v: status = %d, body %s; want 403 own-access", super, code, body)
		}
		if admin.replaced[actorID] != 0 {
			t.Errorf("superadmin=%v: ReplaceUserAccess was reached", super)
		}
	}
}

func TestPutUserAccessRequestValidation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, body string
		want       int
		contains   string
	}{
		{"stale version", `{"version":"stale","roles":[],"pages":[]}`, http.StatusConflict, "reload"},
		{"missing version", `{"roles":[],"pages":[]}`, http.StatusBadRequest, "version is required"},
		{"missing roles", `{"version":"` + emptyVersion + `","pages":[]}`, http.StatusBadRequest, "send [] to clear"},
		{"null pages", `{"version":"` + emptyVersion + `","roles":[],"pages":null}`, http.StatusBadRequest, "send [] to clear"},
		{"unknown field", `{"version":"` + emptyVersion + `","roles":[],"pages":[],"bundles":[]}`, http.StatusBadRequest, "unknown field"},
		{"superadmin role", `{"version":"` + emptyVersion + `","roles":[{"role":"superadmin","fleet_wide":true}],"pages":[]}`,
			http.StatusBadRequest, "superadmin"},
		{"page without a role", `{"version":"` + emptyVersion + `","roles":[],"pages":[{"page":"overview","fleet_wide":true}]}`,
			http.StatusBadRequest, "need a role"},
		{"valid", `{"version":"` + emptyVersion + `","roles":[{"role":"viewer","node_id":"n1"}],"pages":[{"page":"ports","node_id":"n1"}]}`,
			http.StatusOK, `"granted":[{"kind":"role","role":"viewer","node_id":"n1","fleet_wide":false},{"kind":"page","page":"ports","node_id":"n1","fleet_wide":false}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			base, _, client := newSuperadminGuardServer(t, false)
			code, body := doBody(t, client, base, guardAction{method: http.MethodPut, path: "/users/" + secondAdminID + "/access", body: c.body})
			if code != c.want || !strings.Contains(body, c.contains) {
				t.Errorf("status = %d, body %s; want %d containing %q", code, body, c.want, c.contains)
			}
		})
	}
}

// GET is exempt from protectTarget: an admin may read the superadmin's
// access, never change it.
func TestGetUserAccessIsReadableForTheSuperadmin(t *testing.T) {
	t.Parallel()
	base, _, client := newSuperadminGuardServer(t, false)
	code, body := doBody(t, client, base, guardAction{method: http.MethodGet, path: "/users/" + superadminUserID + "/access"})
	want := `{"version":"` + emptyVersion + `","roles":[],"pages":[],"bundles":[]}`
	if code != http.StatusOK || strings.TrimSpace(body) != want {
		t.Errorf("status = %d, body %s; want 200 %s", code, body, want)
	}
}

func TestCreateUserPassesInitialAccessThrough(t *testing.T) {
	t.Parallel()
	base, admin, client := newSuperadminGuardServer(t, false)

	code, body := doBody(t, client, base, guardAction{method: http.MethodPost, path: "/users", body: `{"username":"plain"}`})
	if code != http.StatusCreated {
		t.Fatalf("create without access: status = %d, body %s", code, body)
	}
	if admin.createdWith["new-plain"] != nil {
		t.Error("an omitted access body reached storage as non-nil")
	}

	code, body = doBody(t, client, base, guardAction{method: http.MethodPost, path: "/users",
		body: `{"username":"with","access":{"roles":[{"role":"viewer","fleet_wide":true}],"pages":[{"page":"ports","node_id":"n1"}]}}`})
	if code != http.StatusCreated {
		t.Fatalf("create with access: status = %d, body %s", code, body)
	}
	got := admin.createdWith["new-with"]
	if got == nil || len(got.Roles) != 1 || got.Roles[0].Role != "viewer" || !got.Roles[0].Scope.FleetWide ||
		len(got.Pages) != 1 || got.Pages[0].Page != pageauthz.PagePorts || got.Pages[0].Scope.NodeID != "n1" {
		t.Errorf("initial access = %+v", got)
	}
}
