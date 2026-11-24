package v1

import (
	"net/http"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// publicRoutes is every route reachable without a session, keyed by the exact
// pattern it is registered under. Every other route is refused before its
// handler runs; see [sessionRequired].
var publicRoutes = map[string]bool{
	// A caller logging in has no session yet.
	"POST " + Prefix + "/auth/login": true,
	// Idempotent: must clear the cookie even when the session is already gone.
	"POST " + Prefix + "/auth/logout": true,
	// How the UI learns it is logged out; answers its own 401 "not logged in".
	"GET " + Prefix + "/auth/me": true,
	// Liveness for monitors that hold no session.
	"GET " + Prefix + "/system/health": true,
	// The installer holds a one-time code, not a session; see [Handler.RedeemInstallCode].
	"POST " + Prefix + "/agent/install/redeem": true,
}

// sessionRequired is the [Registrar] [Handler.Mount] registers through: every
// route not in [publicRoutes] requires a session, so a new route is
// authenticated unless it is deliberately listed. It decides only whether a
// caller is logged in; what they may see stays with the handlers.
//
// There is no bypass for an instance with no session store: no principal,
// no access.
type sessionRequired struct{ mux Registrar }

func (s sessionRequired) Handle(pattern string, handler http.Handler) {
	if !publicRoutes[pattern] {
		handler = requireSession(handler)
	}
	s.mux.Handle(pattern, handler)
}

func requireSession(next http.Handler) http.Handler {
	return httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		if _, ok := session.PrincipalFrom(r.Context()); !ok {
			return errs.New(errs.CodeUnauthenticated, "authentication required").WithOp("v1.requireSession")
		}
		next.ServeHTTP(w, r)
		return nil
	})
}
