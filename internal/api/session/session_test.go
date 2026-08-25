package session_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

type fakeSessionStore struct {
	principal user.Principal
	err       error
	gotHash   string
}

func (f *fakeSessionStore) CreateSession(context.Context, user.Session) error { return nil }
func (f *fakeSessionStore) RevokeSession(context.Context, string, time.Time) error {
	return nil
}
func (f *fakeSessionStore) Resolve(_ context.Context, tokenHash string, _ time.Time) (user.Principal, error) {
	f.gotHash = tokenHash
	return f.principal, f.err
}

func newDownstream(t *testing.T, want func(*testing.T, *http.Request)) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want(t, r)
		w.WriteHeader(http.StatusOK)
	})
}

func TestAuthMiddlewareLeavesAnAnonymousRequestUntouchedWithNoCookie(t *testing.T) {
	t.Parallel()

	store := &fakeSessionStore{}
	handler := session.AuthMiddleware(store, nil)(newDownstream(t, func(t *testing.T, r *http.Request) {
		if _, ok := session.PrincipalFrom(r.Context()); ok {
			t.Error("a principal is present on the context with no cookie at all")
		}
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — the middleware must never reject on its own", rec.Code)
	}
	if store.gotHash != "" {
		t.Error("Resolve was called with no cookie present")
	}
}

func TestAuthMiddlewareResolvesALiveSessionCookieIntoAPrincipal(t *testing.T) {
	t.Parallel()

	want := user.Principal{UserID: "u1", Username: "alice"}
	store := &fakeSessionStore{principal: want}
	handler := session.AuthMiddleware(store, nil)(newDownstream(t, func(t *testing.T, r *http.Request) {
		got, ok := session.PrincipalFrom(r.Context())
		if !ok {
			t.Fatal("no principal on context for a live session")
		}
		if got != want {
			t.Errorf("principal = %+v, want %+v", got, want)
		}
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "some-raw-token"})
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	// Resolve must never see the raw cookie value — only its hash reaches
	// storage, the same rule enrollment tokens follow.
	if store.gotHash == "" || store.gotHash == "some-raw-token" {
		t.Errorf("Resolve was called with %q, want a hash of the raw token", store.gotHash)
	}
}

// An invalid cookie (unknown, expired, revoked — [user.ErrSessionInvalid]
// collapses all three) must leave the request anonymous, not reject it:
// per docs/adr/0011-deferred-rbac.md sec 2, rejecting is a handler-level
// decision, and many routes this middleware wraps are public.
func TestAuthMiddlewareTreatsAnInvalidCookieAsAnonymousNotRejected(t *testing.T) {
	t.Parallel()

	store := &fakeSessionStore{err: user.ErrSessionInvalid}
	handler := session.AuthMiddleware(store, nil)(newDownstream(t, func(t *testing.T, r *http.Request) {
		if _, ok := session.PrincipalFrom(r.Context()); ok {
			t.Error("a principal is present on the context for an invalid session")
		}
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "stale-token"})
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 — an invalid cookie must not itself produce a 401", rec.Code)
	}
}

// A genuine backend failure (DB down, timeout, pool exhausted — anything
// that is NOT the business-logic "no such session" case) must fail exactly
// the same way as an invalid cookie: the request proceeds anonymous, never
// with a forged or partially-populated principal. This is the fail-closed
// property at the identity-resolution layer — see
// [user_test.TestAuthorizerRequirePropagatesStoreFailureRatherThanTreatingItAsDenial]
// for the same property at the permission-check layer, and
// TestDBErrorDuringAuthzNeverProducesA200 in internal/api/v1 for the same
// property proven end-to-end over real HTTP.
func TestAuthMiddlewareTreatsADatabaseErrorAsAnonymousNeverAuthenticated(t *testing.T) {
	t.Parallel()

	dbErr := errs.New(errs.CodeUnavailable, "connection pool exhausted")
	store := &fakeSessionStore{err: dbErr}
	var sawPrincipal user.Principal
	var sawOK bool
	handler := session.AuthMiddleware(store, nil)(newDownstream(t, func(t *testing.T, r *http.Request) {
		sawPrincipal, sawOK = session.PrincipalFrom(r.Context())
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: session.CookieName, Value: "some-token"})
	handler.ServeHTTP(rec, req)

	if sawOK {
		t.Fatalf("a database error resolving the session still produced a principal: %+v", sawPrincipal)
	}
	if sawPrincipal != (user.Principal{}) {
		t.Fatalf("a database error resolving the session left a non-zero principal on context: %+v", sawPrincipal)
	}
	// The middleware itself still answers 200 here only because the fake
	// downstream handler always does — this is not the authorization
	// decision. What matters, and what requireScope depends on, is sawOK
	// being false: see requireScope in internal/api/v1/auth.go, which turns
	// "no principal" into 401 for any endpoint that calls it.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 from the fake downstream handler", rec.Code)
	}
}

func TestSetCookieThenClearCookieExpiresIt(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	session.SetCookie(rec, "a-token", time.Now().Add(time.Hour), true)
	set := rec.Result().Cookies()
	if len(set) != 1 || set[0].Value != "a-token" {
		t.Fatalf("SetCookie did not set the expected cookie: %+v", set)
	}
	if !set[0].HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if !set[0].Secure {
		t.Error("Secure=true was requested but the cookie is not Secure")
	}

	rec2 := httptest.NewRecorder()
	session.ClearCookie(rec2, true)
	cleared := rec2.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 {
		t.Fatalf("ClearCookie did not expire the cookie: %+v", cleared)
	}
}
