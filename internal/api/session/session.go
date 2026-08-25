// Package session is the HTTP-layer half of human-user authentication: the
// cookie an authenticated browser carries, and the middleware that resolves
// it into a principal on the request context.
//
// It mirrors internal/api/agent's PeerAuthMiddleware/PeerIdentityFrom shape
// exactly, for the human-user identity domain instead of the machine one:
// this package depends on [user.SessionStore], never on Postgres, and never
// on [user.Store] or [user.AuthzStore] — resolving a cookie to a principal
// needs only the session table.
package session

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// CookieName is the session cookie every browser client carries.
//
// One cookie serves both the REST chain and the streaming chain — see
// docs/adr/0011-deferred-rbac.md sec 3: a WebSocket upgrade has no
// Authorization header available to it, and a long-lived token in the URL
// would be recorded verbatim by AccessLog in the streaming chain. A cookie
// is sent automatically on both a fetch and a WebSocket handshake to the same
// origin, with neither problem.
const CookieName = "atlas_session"

// SetCookie writes a fresh session cookie for token, valid until expiresAt.
//
// secure controls the Secure attribute: it must be true whenever Atlas is
// reachable over anything but plain-HTTP loopback, and the caller decides
// that from its own TLS configuration — this package has no view of it.
func SetCookie(w http.ResponseWriter, token string, expiresAt time.Time, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   secure,
		// Lax rather than Strict: Strict drops the cookie on a top-level
		// navigation arriving from off-site (a bookmarked link, a shared
		// URL), which would present as a silent, confusing logout. Lax still
		// withholds the cookie from cross-site fetch/XHR, which is the
		// request shape a JSON API actually needs to be protected from.
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie expires the session cookie immediately, for logout.
func ClearCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

type principalKey struct{}

// WithPrincipal returns ctx carrying an authenticated principal. Exported for
// tests and for any composition root that authenticates a request some other
// way; ordinary requests get it from [AuthMiddleware].
func WithPrincipal(ctx context.Context, p user.Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal [AuthMiddleware] resolved for this
// request, if the request carried a live session cookie.
//
// The second return is false for an anonymous request — an absent, expired,
// revoked, or otherwise invalid cookie are all indistinguishable here by
// design, the same way [user.ErrSessionInvalid] does not distinguish them at
// the store layer. A handler that requires authentication treats false as
// "not logged in"; it never needs to know which of those it was.
func PrincipalFrom(ctx context.Context) (user.Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(user.Principal)
	return p, ok
}

// AuthMiddleware resolves the session cookie, when present and live, into a
// principal on the request context.
//
// Per docs/adr/0011-deferred-rbac.md sec 1, this is the one shared
// definition both [httpx.BaseMiddleware] and [httpx.StreamMiddleware] splice
// into their chain — never installed on one and not the other, which is the
// exact failure mode that ADR names as the highest risk of adding
// authentication after the fact.
//
// It never rejects a request. A missing or invalid cookie leaves the request
// anonymous rather than producing a 401 here: this middleware runs ahead of
// routing, before it is known whether the route even requires
// authentication, so per docs/adr/0011-deferred-rbac.md sec 2 the coarse
// authenticated/anonymous fact is made available on the context and the
// handler-level policy layer — see [user.Authorizer.Require] — is what acts
// on it. A malformed or expired cookie is deliberately not distinguished
// from an absent one, for the same reason [user.ErrSessionInvalid] collapses
// those cases at the store layer: telling an unauthenticated caller which
// applies would confirm whether a session ever existed.
func AuthMiddleware(store user.SessionStore, logger *slog.Logger) httpx.Middleware {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(CookieName)
			if err != nil || cookie.Value == "" {
				next.ServeHTTP(w, r)
				return
			}

			principal, err := store.Resolve(r.Context(), user.HashSessionToken(cookie.Value), time.Now())
			if err != nil {
				logger.DebugContext(r.Context(), "session cookie did not resolve",
					slog.String("error", err.Error()))
				next.ServeHTTP(w, r)
				return
			}

			next.ServeHTTP(w, r.WithContext(WithPrincipal(r.Context(), principal)))
		})
	}
}
