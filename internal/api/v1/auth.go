package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// sessionTTL bounds how long an issued session cookie is valid before an
// operator must log in again. There is no sliding-window refresh in this
// phase — see the implementation report's remaining decisions.
const sessionTTL = 24 * time.Hour

// UserStore is the human-user identity surface the login endpoint needs.
// Satisfied by [*github.com/hexane/atlas/internal/storage/user.Repository].
type UserStore interface {
	ByUsername(ctx context.Context, username string) (user.User, error)
}

// SessionStore is the session surface the auth endpoints and
// [session.AuthMiddleware] need. Satisfied by
// [*github.com/hexane/atlas/internal/storage/user.Repository].
type SessionStore interface {
	CreateSession(ctx context.Context, s user.Session) error
	Resolve(ctx context.Context, tokenHash string, now time.Time) (user.Principal, error)
	RevokeSession(ctx context.Context, tokenHash string, now time.Time) error
	RevokeAllSessions(ctx context.Context, userID, actorUserID string, now time.Time) error
}

// Authorizer is the authorization policy layer node-scoped handlers call.
// Satisfied by [user.Authorizer].
//
// Per docs/adr/0011-deferred-rbac.md sec 2, this is invoked from handlers —
// see [Handler.requireScope] — never from middleware: middleware runs before
// a handler has resolved which node a request names.
type Authorizer interface {
	Require(ctx context.Context, principal user.Principal, permission user.Permission, nodeID string) error
	// AuthorizedNodes reports which nodes principal may see for permission —
	// see [Handler.ListNodes], which filters a result set rather than
	// gating one pass/fail check.
	AuthorizedNodes(ctx context.Context, principal user.Principal, permission user.Permission) (fleetWide bool, nodeIDs map[string]bool, err error)
}

// PageAuthorizer is the page-visibility policy layer — a second,
// independent axis from Authorizer above (node.read/node.logs.read/
// fleet.write/user.manage). Checked alongside, never instead of, the
// existing per-operation Authorizer check: see internal/core/pageauthz's
// doc. Satisfied by [pageauthz.Authorizer].
type PageAuthorizer interface {
	Require(ctx context.Context, userID string, page pageauthz.Page, nodeID string) error
}

// LoginLimiter bounds POST /auth/login attempts. Satisfied by
// [*user.LoginLimiter].
type LoginLimiter interface {
	Allow(username, sourceIP string) bool
	ResetUsername(username string)
}

// LoginRequest is the login payload.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// CurrentUserResponse describes the authenticated caller.
type CurrentUserResponse struct {
	UserID   string `json:"user_id"`
	Username string `json:"username"`
	// CanManageUsers tells the frontend whether to show the admin Users
	// page's nav entry — a display hint only, never the enforcement point:
	// every /users endpoint independently checks PermissionUserManage
	// itself regardless of what this says.
	CanManageUsers bool `json:"can_manage_users"`
}

// currentUserResponse builds [CurrentUserResponse] for principal, checking
// PermissionUserManage the same way [Handler.requirePermission] does.
func (h *Handler) currentUserResponse(ctx context.Context, principal user.Principal) CurrentUserResponse {
	resp := CurrentUserResponse{UserID: principal.UserID, Username: principal.Username}
	if h.deps.Authz != nil {
		resp.CanManageUsers = h.deps.Authz.Require(ctx, principal, user.PermissionUserManage, "") == nil
	}
	return resp
}

// Login authenticates a username and password and, on success, issues a
// session cookie.
//
// This endpoint, /auth/logout, and /auth/me are the only ones deliberately
// left reachable without a session: a login form cannot present a
// credential it does not have yet. Every other endpoint's protection is
// unaffected by that — see [Handler.requireScope].
func (h *Handler) Login(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.Login"

	if h.deps.Users == nil || h.deps.Sessions == nil {
		return errs.New(errs.CodeUnavailable, "authentication is not configured on this instance").WithOp(op)
	}

	var req LoginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	if strings.TrimSpace(req.Username) == "" || req.Password == "" {
		return errs.New(errs.CodeInvalidArgument, "username and password are required").WithOp(op)
	}

	// Checked, and a token consumed, before any password verification: the
	// budget bounds the cost of guessing, not just its eventual success. See
	// [user.LoginLimiter] for why username and source IP are independent
	// budgets rather than one.
	if h.deps.LoginLimiter != nil && !h.deps.LoginLimiter.Allow(req.Username, httpx.ClientIP(r)) {
		return errs.New(errs.CodeRateLimited, "too many login attempts; try again later").WithOp(op)
	}

	u, err := h.deps.Users.ByUsername(r.Context(), req.Username)
	if err != nil {
		if errs.CodeOf(err) == errs.CodeNotFound {
			return user.ErrInvalidCredentials
		}
		return err
	}
	if u.Disabled() || !user.VerifyPassword(u.PasswordHash, req.Password) {
		return user.ErrInvalidCredentials
	}
	if h.deps.LoginLimiter != nil {
		h.deps.LoginLimiter.ResetUsername(req.Username)
	}

	generated, err := user.NewSession()
	if err != nil {
		return err
	}
	now := time.Now()
	sess := user.Session{
		TokenHash: generated.Hash,
		UserID:    u.ID,
		CreatedAt: now,
		ExpiresAt: now.Add(sessionTTL),
	}
	if err := h.deps.Sessions.CreateSession(r.Context(), sess); err != nil {
		return err
	}

	session.SetCookie(w, generated.Plaintext, sess.ExpiresAt, h.deps.SessionSecure)
	principal := user.Principal{UserID: u.ID, Username: u.Username}
	httpx.JSON(w, r, http.StatusOK, h.currentUserResponse(r.Context(), principal))
	return nil
}

// Logout revokes the caller's session, if any, and clears the cookie.
//
// Revoking an absent or already-invalid session is not an error: the
// caller's intent — that they are no longer logged in — holds either way,
// the same idempotency [user.SessionStore.RevokeSession] documents.
func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) error {
	if h.deps.Sessions != nil {
		if cookie, err := r.Cookie(session.CookieName); err == nil && cookie.Value != "" {
			_ = h.deps.Sessions.RevokeSession(r.Context(), user.HashSessionToken(cookie.Value), time.Now())
		}
	}
	session.ClearCookie(w, h.deps.SessionSecure)
	httpx.NoContent(w)
	return nil
}

// CurrentUser reports the authenticated caller, for the frontend to learn
// its own login state on load without guessing from the presence of a
// cookie it cannot itself read (the cookie is HttpOnly).
func (h *Handler) CurrentUser(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CurrentUser"

	principal, ok := session.PrincipalFrom(r.Context())
	if !ok {
		return errs.New(errs.CodeUnauthenticated, "not logged in").WithOp(op)
	}
	httpx.JSON(w, r, http.StatusOK, h.currentUserResponse(r.Context(), principal))
	return nil
}

// requireScope resolves the inventory scope from r and checks that the
// authenticated caller holds permission for the node it names.
//
// This is the single point node-scoped handlers call through — the same
// chokepoint [Handler.scopeFrom] already was for scope resolution — rather
// than each handler implementing its own authentication and authorization
// check. See docs/adr/0011-deferred-rbac.md sec 2 and 4: authorization is a
// policy layer called from handlers, and permissions bind per node.
// page names which frontend page r's route belongs to, for the second,
// independent page-visibility check layered on top of the operation-level
// one — see [pageauthz]. Pass [pageauthz.PageNone] for a route that maps to
// no dedicated page yet (capacity/summary, cost/estimate, signals,
// health/score): it skips the page-access check entirely, the same way a
// nil h.deps.PageAuthz does, rather than a page a caller forgot to name.
func (h *Handler) requireScope(r *http.Request, permission user.Permission, page pageauthz.Page) (inventory.Scope, error) {
	const op = "v1.Handler.requireScope"

	scope := h.scopeFrom(r)

	if h.deps.Authz == nil {
		// No authorizer configured: authentication is not wired in on this
		// instance (see [Handler.Login]'s same nil check), so every
		// node-scoped read behaves as it did before this milestone rather
		// than refusing every request on an instance that never opted in.
		return scope, nil
	}

	principal, ok := session.PrincipalFrom(r.Context())
	if !ok {
		return inventory.Scope{}, errs.New(errs.CodeUnauthenticated, "authentication required").WithOp(op)
	}
	nodeID := h.resolvedNode(scope)
	if err := h.deps.Authz.Require(r.Context(), principal, permission, nodeID); err != nil {
		return inventory.Scope{}, err
	}
	if page != pageauthz.PageNone && h.deps.PageAuthz != nil {
		if err := h.deps.PageAuthz.Require(r.Context(), principal.UserID, page, nodeID); err != nil {
			return inventory.Scope{}, err
		}
	}
	return scope, nil
}

// requireNode is [Handler.requireScope] for the handlers that only need the
// resolved node id, not the full scope.
func (h *Handler) requireNode(r *http.Request, permission user.Permission, page pageauthz.Page) (string, error) {
	scope, err := h.requireScope(r, permission, page)
	if err != nil {
		return "", err
	}
	return h.resolvedNode(scope), nil
}

// requirePermission is [Handler.requireScope] for the handlers that gate a
// privileged write with no node dimension at all — alert rules, SLO
// definitions, notification channels, user management. See
// [user.PermissionFleetWrite]: an empty node id means only a fleet-wide
// grant can satisfy the check, never a grant scoped to one node.
func (h *Handler) requirePermission(r *http.Request, permission user.Permission, page pageauthz.Page) error {
	const op = "v1.Handler.requirePermission"

	if h.deps.Authz == nil {
		// Same "not wired in on this instance" convention as
		// [Handler.requireScope] — see its comment.
		return nil
	}

	principal, ok := session.PrincipalFrom(r.Context())
	if !ok {
		return errs.New(errs.CodeUnauthenticated, "authentication required").WithOp(op)
	}
	if err := h.deps.Authz.Require(r.Context(), principal, permission, ""); err != nil {
		return err
	}
	if page != pageauthz.PageNone && h.deps.PageAuthz != nil {
		if err := h.deps.PageAuthz.Require(r.Context(), principal.UserID, page, ""); err != nil {
			return err
		}
	}
	return nil
}
