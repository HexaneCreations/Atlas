package v1_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hexane/atlas/internal/core/user"
)

// unprotectedUserTargetRoutes are the only {userID} routes allowed to skip
// protectTarget: read-only views the superadmin rule does not cover.
var unprotectedUserTargetRoutes = []string{
	"GET /users/{userID}/access",
	"GET /users/{userID}/page-access",
}

// Every route registered in system.go whose pattern names a {userID} target
// must be registered as h.protectTarget(h.X), so a future route cannot
// forget the superadmin guard. Parsed from source rather than probed at
// runtime because ServeMux cannot enumerate its patterns.
func TestEveryUserTargetRouteIsProtected(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "system.go", nil, 0)
	if err != nil {
		t.Fatalf("parse system.go: %v", err)
	}

	var protected, exempt []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 || !isSelector(call.Fun, "mux", "Handle") {
			return true
		}
		pattern, ok := routePattern(call.Args[0])
		if !ok {
			t.Errorf("%s: mux.Handle pattern is not a literal/Prefix concatenation; extend this test", fset.Position(call.Pos()))
			return true
		}
		if !strings.Contains(pattern, "{userID}") {
			return true
		}
		if slices.Contains(unprotectedUserTargetRoutes, pattern) {
			exempt = append(exempt, pattern)
			return true
		}
		wrap, ok := call.Args[1].(*ast.CallExpr)
		if !ok || !isSelector(wrap.Fun, "h", "protectTarget") {
			t.Errorf("%s: %q is a {userID} route not registered through h.protectTarget", fset.Position(call.Pos()), pattern)
			return true
		}
		protected = append(protected, pattern)
		return true
	})

	// Guards against the walk silently matching nothing (e.g. a rename of
	// mux or Prefix): the 12 target routes that exist today must all be seen.
	if len(protected) != 12 {
		t.Errorf("protected {userID} routes = %d, want 12: %v", len(protected), protected)
	}
	if !slices.Equal(exempt, unprotectedUserTargetRoutes) {
		t.Errorf("exempt routes seen = %v, want %v", exempt, unprotectedUserTargetRoutes)
	}
}

func isSelector(e ast.Expr, recv, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == recv
}

// routePattern flattens `"GET "+Prefix+"/users/{userID}"` to
// "GET /users/{userID}", dropping the Prefix identifier.
func routePattern(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		return "", v.Name == "Prefix"
	case *ast.BinaryExpr:
		l, ok := routePattern(v.X)
		if !ok || v.Op != token.ADD {
			return "", false
		}
		r, ok := routePattern(v.Y)
		return strings.Replace(l+r, "  ", " ", 1), ok
	}
	return "", false
}

// The guard itself still lets a superadmin act on another superadmin — it
// returns early when the actor is a superadmin (guardSuperadminTarget's
// actorSuper branch). Since migrations/0022_single_superadmin.sql a second
// superadmin cannot exist in Postgres, so this pins the guard logic only:
// every action passes the guard, and only the two superadmin-specific
// refusals (revoke the superadmin grant, disable the superadmin) answer 400,
// from the guard-passing caller too.
func TestSuperadminCanActOnAnotherSuperadmin(t *testing.T) {
	t.Parallel()

	refused := map[string]bool{"revoke role": true, "disable": true}
	for _, a := range targetActions(superadminUserID, superadminGrant) {
		t.Run(a.name, func(t *testing.T) {
			base, admin, client := newSuperadminGuardServer(t, true)
			code := do(t, client, base, a)
			if refused[a.name] {
				if code != http.StatusBadRequest {
					t.Errorf("status = %d, want 400 — the API never revokes or disables a superadmin", code)
				}
				if admin.grants[superadminGrant].RevokedAt != nil || admin.users[superadminUserID].Disabled() {
					t.Error("the superadmin changed despite the refusal")
				}
				return
			}
			if code >= 400 {
				t.Errorf("status = %d, want success — the guard lets a superadmin through", code)
			}
		})
	}
}

// The superadmin cannot revoke their own superadmin grant through the API —
// protectTarget lets an actor act on themselves, so this is the refusal that
// stops it.
func TestSuperadminCannotRevokeTheirOwnSuperadminGrant(t *testing.T) {
	t.Parallel()

	base, admin, client := newSuperadminGuardServer(t, true)
	actorID := admin.grants["g-actor"].UserID
	code := do(t, client, base, guardAction{method: http.MethodDelete, path: "/users/" + actorID + "/grants/g-actor"})
	if code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (user.ErrSuperadminNotRevocable)", code)
	}
	if admin.grants["g-actor"].RevokedAt != nil {
		t.Error("the superadmin's own grant was revoked")
	}
}

// A grant id belonging to someone other than the path user is a no-op 204,
// exactly like an unknown id, and leaves the superadmin's grant untouched.
func TestRevokeRoleWithAnotherUsersGrantIDBehavesLikeUnknown(t *testing.T) {
	t.Parallel()

	base, admin, client := newSuperadminGuardServer(t, false)
	mismatched := do(t, client, base, guardAction{method: http.MethodDelete, path: "/users/" + secondAdminID + "/grants/" + superadminGrant})
	unknown := do(t, client, base, guardAction{method: http.MethodDelete, path: "/users/" + secondAdminID + "/grants/no-such-grant"})
	if mismatched != unknown || mismatched != http.StatusNoContent {
		t.Errorf("mismatched id status = %d, unknown id status = %d; want both 204", mismatched, unknown)
	}
	if admin.grants[superadminGrant].RevokedAt != nil {
		t.Error("the superadmin's grant was revoked through another user's path")
	}
}

// The 403 for a protected target carries the sentinel's message.
func TestProtectedSuperadminMessage(t *testing.T) {
	t.Parallel()
	if got, want := user.ErrProtectedSuperadmin.Error(), "superadmin accounts can only be changed by a superadmin"; !strings.Contains(got, want) {
		t.Errorf("ErrProtectedSuperadmin = %q, want it to contain %q", got, want)
	}
}

// Disable, enable and reset-password must be registered as
// h.protectTarget(h.adminTarget(h.X)): the admin-only-superadmin rule for
// targets who hold admin.
func TestAdminTargetRoutesAreGuarded(t *testing.T) {
	t.Parallel()

	want := []string{
		"POST /users/{userID}/disable",
		"POST /users/{userID}/enable",
		"POST /users/{userID}/reset-password",
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "system.go", nil, 0)
	if err != nil {
		t.Fatalf("parse system.go: %v", err)
	}
	var guarded []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 2 || !isSelector(call.Fun, "mux", "Handle") {
			return true
		}
		pattern, ok := routePattern(call.Args[0])
		if !ok {
			return true
		}
		outer, ok := call.Args[1].(*ast.CallExpr)
		if !ok || !isSelector(outer.Fun, "h", "protectTarget") || len(outer.Args) != 1 {
			return true
		}
		if inner, ok := outer.Args[0].(*ast.CallExpr); ok && isSelector(inner.Fun, "h", "adminTarget") {
			guarded = append(guarded, pattern)
		}
		return true
	})
	if !slices.Equal(guarded, want) {
		t.Errorf("adminTarget routes = %v, want %v", guarded, want)
	}
}
