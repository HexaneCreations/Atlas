package access_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/core/access"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

var fleet = pageauthz.Scope{FleetWide: true}

func node(id string) pageauthz.Scope { return pageauthz.Scope{NodeID: id} }

func role(s pageauthz.Scope, r string) access.Role { return access.Role{Scope: s, Role: r} }

func page(s pageauthz.Scope, p pageauthz.Page) access.Page { return access.Page{Scope: s, Page: p} }

func heldRole(id string, s pageauthz.Scope, r string) access.RoleGrant {
	return access.RoleGrant{ID: id, Role: role(s, r)}
}

func heldPage(id string, s pageauthz.Scope, p pageauthz.Page) access.PageGrant {
	return access.PageGrant{ID: id, Page: page(s, p)}
}

var nodes = map[string]bool{"n1": true, "n2": true}

// summary renders Changes as sorted "+role ..." / "-page ..." lines, so a
// table row states the expected result in one readable list.
func summary(c access.Changes) []string {
	var out []string
	for _, r := range c.GrantRoles {
		out = append(out, "+role "+label(r.Scope)+" "+r.Role)
	}
	for _, g := range c.RevokeRoles {
		out = append(out, "-role "+label(g.Scope)+" "+g.Role.Role+" "+g.ID)
	}
	for _, p := range c.GrantPages {
		out = append(out, "+page "+label(p.Scope)+" "+string(p.Page))
	}
	for _, g := range c.RevokePages {
		out = append(out, "-page "+label(g.Scope)+" "+string(g.Page.Page)+" "+g.ID)
	}
	slices.Sort(out)
	return out
}

func label(s pageauthz.Scope) string {
	if s.FleetWide {
		return "*"
	}
	return s.NodeID
}

func TestPlan(t *testing.T) {
	t.Parallel()

	bundle := access.Bundle{ID: "b1", Name: "containers", Scope: fleet, Pages: []pageauthz.Page{pageauthz.PageContainers}}
	nodeBundle := access.Bundle{ID: "b2", Name: "disks", Scope: node("n1"), Pages: []pageauthz.Page{pageauthz.PageDisks}}

	cases := []struct {
		name     string
		current  access.State
		desired  access.Desired
		want     []string
		wantCode errs.Code // "" means success
		wantErr  string
	}{
		{name: "empty to empty is a no-op"},
		{
			name:    "grant into an empty state",
			desired: access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(fleet, "overview"), page(node("n1"), "containers")}},
			want:    []string{"+page * overview", "+page n1 containers", "+role * viewer"},
		},
		{
			name:    "identical request changes nothing",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", node("n1"), "operator")}, Pages: []access.PageGrant{heldPage("p1", node("n1"), "ports")}},
			desired: access.Desired{Roles: []access.Role{role(node("n1"), "operator")}, Pages: []access.Page{page(node("n1"), "ports")}},
		},
		{
			name:    "rows left out of the request are revoked",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", node("n1"), "viewer"), heldRole("r2", node("n2"), "viewer")}, Pages: []access.PageGrant{heldPage("p1", node("n2"), "ports")}},
			desired: access.Desired{Roles: []access.Role{role(node("n1"), "viewer")}, Pages: []access.Page{}},
			want:    []string{"-page n2 ports p1", "-role n2 viewer r2"},
		},
		{
			name:    "a role change is one revoke plus one grant",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", node("n1"), "viewer")}},
			desired: access.Desired{Roles: []access.Role{role(node("n1"), "operator")}},
			want:    []string{"+role n1 operator", "-role n1 viewer r1"},
		},
		{
			name:    "several current roles on one scope collapse to the requested one",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", node("n1"), "viewer"), heldRole("r2", node("n1"), "operator")}},
			desired: access.Desired{Roles: []access.Role{role(node("n1"), "operator")}},
			want:    []string{"-role n1 viewer r1"},
		},
		{
			name:    "a node role at or below the fleet-wide role is dropped",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", node("n1"), "viewer")}},
			desired: access.Desired{Roles: []access.Role{role(fleet, "operator"), role(node("n1"), "viewer"), role(node("n2"), "operator")}},
			want:    []string{"+role * operator", "-role n1 viewer r1"},
		},
		{
			name:    "a node role above the fleet-wide role is kept",
			desired: access.Desired{Roles: []access.Role{role(fleet, "viewer"), role(node("n1"), "admin")}},
			want:    []string{"+role * viewer", "+role n1 admin"},
		},
		{
			name:    "a node page under the same fleet-wide page is dropped",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", fleet, "viewer")}, Pages: []access.PageGrant{heldPage("p1", node("n1"), "ports")}},
			desired: access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(fleet, "ports"), page(node("n1"), "ports")}},
			want:    []string{"+page * ports", "-page n1 ports p1"},
		},
		{
			name:    "rows on a node that no longer exists can still be revoked",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", node("gone"), "viewer")}},
			desired: access.Desired{},
			want:    []string{"-role gone viewer r1"},
		},
		{
			name:    "a held row on a node that no longer exists can be kept",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", node("gone"), "viewer")}},
			desired: access.Desired{Roles: []access.Role{role(node("gone"), "viewer")}},
		},
		{
			name:     "a new grant on an unknown node is refused",
			desired:  access.Desired{Roles: []access.Role{role(node("ghost"), "viewer")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "unknown node ghost",
		},
		{
			name:     "superadmin is never requestable",
			desired:  access.Desired{Roles: []access.Role{role(fleet, "superadmin")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "superadmin",
		},
		{
			name:     "unknown role",
			desired:  access.Desired{Roles: []access.Role{role(fleet, "root")}},
			wantCode: errs.CodeInvalidArgument, wantErr: `unknown role "root"`,
		},
		{
			name:     "two roles on one scope",
			desired:  access.Desired{Roles: []access.Role{role(node("n1"), "viewer"), role(node("n1"), "operator")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "more than one role on node n1",
		},
		{
			name:     "a row with neither scope",
			desired:  access.Desired{Roles: []access.Role{role(pageauthz.Scope{}, "viewer")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "no default between them",
		},
		{
			name:     "a row with both scopes",
			desired:  access.Desired{Pages: []access.Page{page(pageauthz.Scope{FleetWide: true, NodeID: "n1"}, "ports")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "at the same time",
		},
		{
			name:     "unknown page",
			desired:  access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(fleet, "billing")}},
			wantCode: errs.CodeInvalidArgument, wantErr: `unknown page "billing"`,
		},
		{
			name:     "a fleet-only page on a node",
			desired:  access.Desired{Roles: []access.Role{role(node("n1"), "viewer")}, Pages: []access.Page{page(node("n1"), "overview")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "no per-node concept",
		},
		{
			name:     "the same page twice",
			desired:  access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(fleet, "ports"), page(fleet, "ports")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "listed twice",
		},
		{
			name:     "a node page without a role on that node",
			desired:  access.Desired{Roles: []access.Role{role(node("n1"), "viewer")}, Pages: []access.Page{page(node("n2"), "ports")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "ports on node n2",
		},
		{
			name:    "a node page with only a fleet-wide role",
			desired: access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(node("n2"), "ports")}},
			want:    []string{"+page n2 ports", "+role * viewer"},
		},
		{
			name:     "overview without any role",
			desired:  access.Desired{Pages: []access.Page{page(fleet, "overview")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "overview (fleet-wide)",
		},
		{
			name:    "overview and nodes with only a node role",
			desired: access.Desired{Roles: []access.Role{role(node("n1"), "viewer")}, Pages: []access.Page{page(fleet, "overview"), page(fleet, "nodes")}},
			want:    []string{"+page * nodes", "+page * overview", "+role n1 viewer"},
		},
		{
			name:    "users page without fleet-wide admin is allowed",
			desired: access.Desired{Roles: []access.Role{role(node("n1"), "operator")}, Pages: []access.Page{page(fleet, "users")}},
			want:    []string{"+page * users", "+role n1 operator"},
		},
		{
			name:    "users page with no role at all is allowed",
			desired: access.Desired{Pages: []access.Page{page(fleet, "users")}},
			want:    []string{"+page * users"},
		},
		{
			name:     "removing the role under a held page is refused, not left dangling",
			current:  access.State{Roles: []access.RoleGrant{heldRole("r1", node("n1"), "viewer")}, Pages: []access.PageGrant{heldPage("p1", node("n1"), "ports")}},
			desired:  access.Desired{Pages: []access.Page{page(node("n1"), "ports")}},
			wantCode: errs.CodeInvalidArgument, wantErr: "ports on node n1",
		},
		{
			name:     "a direct page a fleet-wide bundle already covers",
			current:  access.State{Roles: []access.RoleGrant{heldRole("r1", fleet, "viewer")}, Bundles: []access.Bundle{bundle}},
			desired:  access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(node("n1"), "containers")}},
			wantCode: errs.CodeAlreadyExists, wantErr: "already granted by a role bundle",
		},
		{
			name:     "a fleet-wide direct page over a node bundle",
			current:  access.State{Roles: []access.RoleGrant{heldRole("r1", fleet, "viewer")}, Bundles: []access.Bundle{nodeBundle}},
			desired:  access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(fleet, "disks")}},
			wantCode: errs.CodeAlreadyExists, wantErr: "already granted by a role bundle",
		},
		{
			name:    "a direct page on another node than the bundle is fine",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", fleet, "viewer")}, Bundles: []access.Bundle{nodeBundle}},
			desired: access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(node("n2"), "disks")}},
			want:    []string{"+page n2 disks"},
		},
		{
			name:    "an existing overlapping direct page is kept, not re-checked",
			current: access.State{Roles: []access.RoleGrant{heldRole("r1", fleet, "viewer")}, Pages: []access.PageGrant{heldPage("p1", fleet, "containers")}, Bundles: []access.Bundle{bundle}},
			desired: access.Desired{Roles: []access.Role{role(fleet, "viewer")}, Pages: []access.Page{page(fleet, "containers")}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := access.Plan(c.current, c.desired, nodes)
			if c.wantCode != "" {
				if err == nil {
					t.Fatalf("Plan() = %v, want a %s error", summary(got), c.wantCode)
				}
				if errs.CodeOf(err) != c.wantCode || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("Plan() error = %v (code %s), want code %s containing %q", err, errs.CodeOf(err), c.wantCode, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			if s := summary(got); !slices.Equal(s, c.want) {
				t.Errorf("Plan() =\n  %v\nwant\n  %v", s, c.want)
			}
			if got.Empty() != (len(c.want) == 0) {
				t.Errorf("Empty() = %v with %d changes", got.Empty(), len(c.want))
			}
		})
	}
}

func TestPlanRefusesSuperadminWithTheSharedSentinel(t *testing.T) {
	t.Parallel()
	_, err := access.Plan(access.State{}, access.Desired{Roles: []access.Role{role(fleet, user.RoleSuperadmin)}}, nodes)
	if !errors.Is(err, user.ErrSuperadminNotGrantable) {
		t.Errorf("Plan() error = %v, want user.ErrSuperadminNotGrantable", err)
	}
}

func TestRevokesFleetWideAdmin(t *testing.T) {
	t.Parallel()
	current := access.State{Roles: []access.RoleGrant{heldRole("r1", fleet, "admin"), heldRole("r2", node("n1"), "admin")}}
	for _, c := range []struct {
		name    string
		desired access.Desired
		want    bool
	}{
		{"kept", access.Desired{Roles: []access.Role{role(fleet, "admin")}}, false},
		{"only the node admin removed", access.Desired{Roles: []access.Role{role(fleet, "admin"), role(node("n2"), "viewer")}}, false},
		{"downgraded", access.Desired{Roles: []access.Role{role(fleet, "operator"), role(node("n1"), "admin")}}, true},
		{"removed", access.Desired{}, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := access.Plan(current, c.desired, nodes)
			if err != nil {
				t.Fatalf("Plan() error = %v", err)
			}
			if got.RevokesFleetWideAdmin() != c.want {
				t.Errorf("RevokesFleetWideAdmin() = %v, want %v (changes %v)", !c.want, c.want, summary(got))
			}
		})
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	a := access.State{Roles: []access.RoleGrant{heldRole("r1", fleet, "viewer")}, Pages: []access.PageGrant{heldPage("p1", fleet, "overview")}}
	reordered := access.State{Pages: a.Pages, Roles: a.Roles}
	if a.Version() != reordered.Version() {
		t.Error("Version depends on field order")
	}
	b := access.State{Roles: []access.RoleGrant{heldRole("r2", fleet, "viewer")}, Pages: a.Pages}
	if a.Version() == b.Version() {
		t.Error("a replaced row id did not change the version")
	}
	// The same id in a different table is a different state.
	c := access.State{Bundles: []access.Bundle{{ID: "r1"}}, Pages: a.Pages}
	if a.Version() == c.Version() {
		t.Error("an id moving between tables did not change the version")
	}
	if (access.State{}).Version() == "" {
		t.Error("an empty state has no version")
	}
}
