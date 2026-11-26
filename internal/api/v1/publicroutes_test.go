package v1_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/hexane/atlas/internal/api/session"
	v1 "github.com/hexane/atlas/internal/api/v1"
	"github.com/hexane/atlas/internal/core/user"
)

// signedIn serves h as an already-authenticated caller, for tests of what a
// handler does once the session check has passed.
func signedIn(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := user.Principal{UserID: "test-user", Username: "test-user"}
		h.ServeHTTP(w, r.WithContext(session.WithPrincipal(r.Context(), p)))
	})
}

// publicRoutes freezes the routes reachable without a session. Adding one
// here is a security decision, not a test fix.
var publicRoutes = []string{
	"GET /api/v1/auth/me",
	"GET /api/v1/system/health",
	"POST /api/v1/agent/install/redeem",
	"POST /api/v1/auth/login",
	"POST /api/v1/auth/logout",
}

type recordedRoutes map[string]http.Handler

func (r recordedRoutes) Handle(pattern string, h http.Handler) { r[pattern] = h }

// anonymouslyRefused reports whether h answers an anonymous request with the
// session check's own 401. A handler that panics was reached.
func anonymouslyRefused(h http.Handler) (refused bool) {
	defer func() {
		if recover() != nil {
			refused = false
		}
	}()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	var body struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code == http.StatusUnauthorized && body.Error.Message == "authentication required"
}

// Every route Mount registers is refused to an anonymous caller unless it is
// in the frozen list above. The handler has no dependencies here, not even a
// session store or an authorizer, so nothing but the session check can refuse
// — in particular a handler's own permission check cannot stand in for it.
func TestEveryRouteRequiresASessionUnlessAllowlisted(t *testing.T) {
	t.Parallel()

	routes := recordedRoutes{}
	v1.NewHandler(v1.Deps{}).Mount(routes)
	if len(routes) < 70 {
		t.Fatalf("recorded only %d routes", len(routes))
	}

	var public []string
	for pattern, h := range routes {
		if !anonymouslyRefused(h) {
			public = append(public, pattern)
		}
	}
	slices.Sort(public)
	if !slices.Equal(public, publicRoutes) {
		t.Fatalf("routes reachable without a session:\n got %v\nwant %v", public, publicRoutes)
	}
}
