package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// UserAdmin is the storage surface the admin Users page's endpoints need,
// beyond authentication's own UserStore/SessionStore. Satisfied by
// [*github.com/hexane/atlas/internal/storage/user.Repository].
type UserAdmin interface {
	ListUsersWithGrants(ctx context.Context) ([]user.UserWithGrants, error)
	GetUser(ctx context.Context, id string) (user.User, error)
	AdminCreateUser(ctx context.Context, username, email, actorUserID string, now time.Time) (user.User, string, error)
	DisableUser(ctx context.Context, userID, actorUserID string, now time.Time) error
	EnableUser(ctx context.Context, userID, actorUserID string, now time.Time) error
	ResetPassword(ctx context.Context, userID, actorUserID string, now time.Time) (string, error)
	ListAudit(ctx context.Context, targetUserID string) ([]user.AuditEntry, error)
	Grant(ctx context.Context, spec user.GrantSpec, now time.Time) error
	RevokeGrant(ctx context.Context, grantID, revokedBy string, now time.Time) error
}

// GrantResponse is one role grant as the admin Users page sees it.
type GrantResponse struct {
	ID        string     `json:"id"`
	Role      string     `json:"role"`
	NodeID    string     `json:"node_id,omitempty"`
	FleetWide bool       `json:"fleet_wide"`
	GrantedAt time.Time  `json:"granted_at"`
	GrantedBy string     `json:"granted_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	RevokedBy string     `json:"revoked_by,omitempty"`
}

func presentGrant(g user.NodeRole) GrantResponse {
	out := GrantResponse{
		ID: g.ID, Role: g.Role, GrantedAt: g.GrantedAt, GrantedBy: g.GrantedBy,
		RevokedAt: g.RevokedAt, RevokedBy: g.RevokedBy,
	}
	if g.NodeID != nil {
		out.NodeID = *g.NodeID
	} else {
		out.FleetWide = true
	}
	return out
}

// UserResponse is one user as the admin Users page sees it.
type UserResponse struct {
	ID        string          `json:"id"`
	Username  string          `json:"username"`
	Email     string          `json:"email,omitempty"`
	Disabled  bool            `json:"disabled"`
	CreatedAt time.Time       `json:"created_at"`
	Grants    []GrantResponse `json:"grants,omitempty"`
}

func presentUser(u user.User) UserResponse {
	return UserResponse{ID: u.ID, Username: u.Username, Email: u.Email, Disabled: u.Disabled(), CreatedAt: u.CreatedAt}
}

// ListUsersResponse is every user Atlas knows about.
type ListUsersResponse struct {
	Users []UserResponse `json:"users"`
	Total int            `json:"total"`
}

func (h *Handler) userAdmin(op string) (UserAdmin, error) {
	if h.deps.UserAdmin == nil {
		return nil, errs.New(errs.CodeNotImplemented, "user management is not enabled").WithOp(op)
	}
	return h.deps.UserAdmin, nil
}

// actor returns the authenticated principal's user id, for recording who
// performed a user-management action. [Handler.requirePermission] has
// already established that the caller is authenticated by the time any
// handler below reaches this — the second lookup here is a cheap context
// read, not a repeated check.
func (h *Handler) actor(r *http.Request, op string) (string, error) {
	principal, ok := session.PrincipalFrom(r.Context())
	if !ok {
		return "", errs.New(errs.CodeUnauthenticated, "authentication required").WithOp(op)
	}
	return principal.UserID, nil
}

// ListUsers returns every user and their current active role grants.
func (h *Handler) ListUsers(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListUsers"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}

	users, err := store.ListUsersWithGrants(r.Context())
	if err != nil {
		return err
	}
	out := make([]UserResponse, 0, len(users))
	for _, uwg := range users {
		resp := presentUser(uwg.User)
		for _, g := range uwg.Grants {
			resp.Grants = append(resp.Grants, presentGrant(g))
		}
		out = append(out, resp)
	}
	httpx.JSON(w, r, http.StatusOK, ListUsersResponse{Users: out, Total: len(out)})
	return nil
}

// CreateUserRequest is the body of POST /users. There is no password field:
// the admin Users page always generates one, shown to the admin exactly
// once in the response — see [CreateUserResponse].
type CreateUserRequest struct {
	Username string `json:"username"`
	Email    string `json:"email,omitempty"`
}

// CreateUserResponse carries the generated password exactly once. There is
// no "reveal password" API — if it is lost, [Handler.ResetPassword] issues a
// new one.
type CreateUserResponse struct {
	User     UserResponse `json:"user"`
	Password string       `json:"password"`
}

// CreateUser creates a new user with a generated one-time password.
func (h *Handler) CreateUser(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CreateUser"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	var req CreateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	if strings.TrimSpace(req.Username) == "" {
		return errs.New(errs.CodeInvalidArgument, "a username is required").WithOp(op)
	}

	created, plaintext, err := store.AdminCreateUser(r.Context(), req.Username, req.Email, actorID, time.Now())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusCreated, CreateUserResponse{User: presentUser(created), Password: plaintext})
	return nil
}

// GrantRoleRequest is the body of POST /users/{userID}/grants.
//
// There is deliberately no default between NodeID and FleetWide — the
// caller must choose explicitly, the same requirement
// [user.GrantSpec.Validate] enforces for the CLI's `user grant`. An omitted
// or blank node_id with fleet_wide left false is rejected, not silently
// treated as fleet-wide.
type GrantRoleRequest struct {
	Role      string `json:"role"`
	NodeID    string `json:"node_id,omitempty"`
	FleetWide bool   `json:"fleet_wide"`
}

// GrantRole grants a role to a user, scoped to one node or fleet-wide.
func (h *Handler) GrantRole(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GrantRole"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	var req GrantRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	spec := user.GrantSpec{
		UserID: r.PathValue("userID"), Role: req.Role,
		NodeID: req.NodeID, FleetWide: req.FleetWide, GrantedBy: actorID,
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := store.Grant(r.Context(), spec, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// RevokeRole revokes one specific role grant, identified by its own id
// rather than by (user, role, scope) — a user may hold several grants at
// once, and the admin Users page's revoke picker names the exact one to
// remove.
func (h *Handler) RevokeRole(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.RevokeRole"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	if err := store.RevokeGrant(r.Context(), r.PathValue("grantID"), actorID, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// DisableUser prevents a user from authenticating and terminates their
// active sessions. Refused against the caller's own account — see
// [Handler.ForceLogout]'s same self-action guard — so a logged-in admin
// cannot lock themselves out by accident; [user.ErrLastAdminGrant] refuses
// it separately when it would leave no enabled fleet-wide admin.
func (h *Handler) DisableUser(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.DisableUser"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	targetID := r.PathValue("userID")
	if targetID == actorID {
		return errs.New(errs.CodeInvalidArgument, "you cannot disable your own account").WithOp(op)
	}
	if err := store.DisableUser(r.Context(), targetID, actorID, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// EnableUser reverses [Handler.DisableUser].
func (h *Handler) EnableUser(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.EnableUser"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	if err := store.EnableUser(r.Context(), r.PathValue("userID"), actorID, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ResetPasswordResponse carries the generated password exactly once.
type ResetPasswordResponse struct {
	Password string `json:"password"`
}

// ResetPassword invalidates a user's current password and issues a new,
// generated one.
func (h *Handler) ResetPassword(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ResetPassword"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	plaintext, err := store.ResetPassword(r.Context(), r.PathValue("userID"), actorID, time.Now())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, ResetPasswordResponse{Password: plaintext})
	return nil
}

// ForceLogout terminates every session a user currently holds, without
// disabling their account — they can log back in immediately. Refused
// against the caller's own account, the same self-action guard
// [Handler.DisableUser] documents: this session is that account's own
// active session, so it would otherwise let an admin instantly and
// accidentally log themselves out.
func (h *Handler) ForceLogout(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ForceLogout"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	if h.deps.Sessions == nil {
		return errs.New(errs.CodeNotImplemented, "user management is not enabled").WithOp(op)
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	targetID := r.PathValue("userID")
	if targetID == actorID {
		return errs.New(errs.CodeInvalidArgument, "you cannot force-logout your own account").WithOp(op)
	}
	if err := h.deps.Sessions.RevokeAllSessions(r.Context(), targetID, actorID, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// AuditEntryResponse is one recorded user-management action.
type AuditEntryResponse struct {
	ID            string         `json:"id"`
	ActorUserID   string         `json:"actor_user_id"`
	ActorUsername string         `json:"actor_username,omitempty"`
	TargetUserID  string         `json:"target_user_id"`
	Action        string         `json:"action"`
	Detail        map[string]any `json:"detail,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

// ListUserAuditResponse is one user's activity history, newest first.
type ListUserAuditResponse struct {
	Entries []AuditEntryResponse `json:"entries"`
	Total   int                  `json:"total"`
}

// ListUserAudit returns every recorded action taken against one user — who
// granted or revoked which role, when, and every disable/enable/reset/
// force-logout — for the admin Users page's per-user activity view.
func (h *Handler) ListUserAudit(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListUserAudit"
	if err := h.requirePermission(r, user.PermissionUserManage); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}

	entries, err := store.ListAudit(r.Context(), r.PathValue("userID"))
	if err != nil {
		return err
	}
	out := make([]AuditEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, AuditEntryResponse{
			ID: e.ID, ActorUserID: e.ActorUserID, ActorUsername: e.ActorUsername,
			TargetUserID: e.TargetUserID, Action: e.Action, Detail: e.Detail, CreatedAt: e.CreatedAt,
		})
	}
	httpx.JSON(w, r, http.StatusOK, ListUserAuditResponse{Entries: out, Total: len(out)})
	return nil
}
