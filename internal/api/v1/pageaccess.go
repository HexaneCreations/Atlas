package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// PageAdmin is the RoleAccess/UserAccess management surface the admin Users
// page's page-access endpoints need. Satisfied by
// [*github.com/hexane/atlas/internal/storage/pageauthz.Repository].
//
// Bundle *definitions* (creating "container-related" and choosing its
// pages) are deliberately not exposed here — CLI/fixture-only for now, the
// same "CLI for bootstrap, UI for routine work" split the original
// human-auth CLI already draws, just landing on the CLI side of it this
// round rather than the UI side. *Assigning* an existing bundle to a user,
// and granting a page directly, are routine admin actions and so are —
// see AssignRoleAccess/GrantPageAccess below.
type PageAdmin interface {
	ListRoleAccessDefinitions(ctx context.Context) ([]pageauthz.RoleAccess, error)
	AssignRoleAccess(ctx context.Context, spec pageauthz.RoleAccessAssignmentSpec, now time.Time) error
	RevokeRoleAccessAssignment(ctx context.Context, assignmentID, revokedBy string, now time.Time) error
	ListRoleAccessAssignments(ctx context.Context, userID string) ([]pageauthz.RoleAccessAssignment, error)
	GrantPageAccess(ctx context.Context, spec pageauthz.PageGrantSpec, now time.Time) error
	RevokePageAccess(ctx context.Context, grantID, revokedBy string, now time.Time) error
	ListPageAccessGrants(ctx context.Context, userID string) ([]pageauthz.PageGrant, error)
}

func (h *Handler) pageAdmin(op string) (PageAdmin, error) {
	if h.deps.PageAdmin == nil {
		return nil, errs.New(errs.CodeNotImplemented, "page access management is not enabled").WithOp(op)
	}
	return h.deps.PageAdmin, nil
}

// RoleAccessResponse is one reusable page bundle.
type RoleAccessResponse struct {
	Name  string           `json:"name"`
	Pages []pageauthz.Page `json:"pages"`
}

// ListRoleAccessResponse is every defined bundle, for an assignment UI's
// picker.
type ListRoleAccessResponse struct {
	RoleAccess []RoleAccessResponse `json:"role_access"`
	Total      int                  `json:"total"`
}

// ListRoleAccessDefinitions returns every RoleAccess bundle and its pages.
func (h *Handler) ListRoleAccessDefinitions(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListRoleAccessDefinitions"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
		return err
	}
	store, err := h.pageAdmin(op)
	if err != nil {
		return err
	}

	defs, err := store.ListRoleAccessDefinitions(r.Context())
	if err != nil {
		return err
	}
	out := make([]RoleAccessResponse, 0, len(defs))
	for _, d := range defs {
		out = append(out, RoleAccessResponse{Name: d.Name, Pages: d.Pages})
	}
	httpx.JSON(w, r, http.StatusOK, ListRoleAccessResponse{RoleAccess: out, Total: len(out)})
	return nil
}

// AssignmentResponse is one page-access grant — a RoleAccess assignment or
// a direct page grant, presented the same shape.
type AssignmentResponse struct {
	ID        string     `json:"id"`
	NodeID    string     `json:"node_id,omitempty"`
	FleetWide bool       `json:"fleet_wide"`
	GrantedAt time.Time  `json:"granted_at"`
	GrantedBy string     `json:"granted_by,omitempty"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	RevokedBy string     `json:"revoked_by,omitempty"`
	// RoleAccess is set for a bundle assignment, empty for a direct grant.
	RoleAccess string `json:"role_access,omitempty"`
	// Page is set for a direct grant, empty for a bundle assignment.
	Page pageauthz.Page `json:"page,omitempty"`
}

func presentRoleAccessAssignment(a pageauthz.RoleAccessAssignment) AssignmentResponse {
	out := AssignmentResponse{
		ID: a.ID, RoleAccess: a.RoleAccessName, GrantedAt: a.GrantedAt, GrantedBy: a.GrantedBy,
		RevokedAt: a.RevokedAt, RevokedBy: a.RevokedBy,
	}
	if a.NodeID != nil {
		out.NodeID = *a.NodeID
	} else {
		out.FleetWide = true
	}
	return out
}

func presentPageGrant(g pageauthz.PageGrant) AssignmentResponse {
	out := AssignmentResponse{
		ID: g.ID, Page: g.Page, GrantedAt: g.GrantedAt, GrantedBy: g.GrantedBy,
		RevokedAt: g.RevokedAt, RevokedBy: g.RevokedBy,
	}
	if g.NodeID != nil {
		out.NodeID = *g.NodeID
	} else {
		out.FleetWide = true
	}
	return out
}

// ListUserPageAccessResponse is one user's full page-access picture: every
// active RoleAccess assignment and every active direct grant.
type ListUserPageAccessResponse struct {
	RoleAccess []AssignmentResponse `json:"role_access"`
	Pages      []AssignmentResponse `json:"pages"`
}

// ListUserPageAccess returns userID's active RoleAccess assignments and
// direct page grants.
func (h *Handler) ListUserPageAccess(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListUserPageAccess"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
		return err
	}
	store, err := h.pageAdmin(op)
	if err != nil {
		return err
	}

	userID := r.PathValue("userID")
	assignments, err := store.ListRoleAccessAssignments(r.Context(), userID)
	if err != nil {
		return err
	}
	grants, err := store.ListPageAccessGrants(r.Context(), userID)
	if err != nil {
		return err
	}

	resp := ListUserPageAccessResponse{
		RoleAccess: make([]AssignmentResponse, 0, len(assignments)),
		Pages:      make([]AssignmentResponse, 0, len(grants)),
	}
	for _, a := range assignments {
		resp.RoleAccess = append(resp.RoleAccess, presentRoleAccessAssignment(a))
	}
	for _, g := range grants {
		resp.Pages = append(resp.Pages, presentPageGrant(g))
	}
	httpx.JSON(w, r, http.StatusOK, resp)
	return nil
}

// AssignRoleAccessRequest is the body of POST /users/{userID}/role-access.
//
// No default between NodeID and FleetWide — the same no-default-between-
// scope-choices rule every grant surface in this codebase enforces.
type AssignRoleAccessRequest struct {
	RoleAccessName string `json:"role_access_name"`
	NodeID         string `json:"node_id,omitempty"`
	FleetWide      bool   `json:"fleet_wide"`
}

// AssignRoleAccess grants a user an existing RoleAccess bundle, scoped to
// one node or fleet-wide.
func (h *Handler) AssignRoleAccess(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.AssignRoleAccess"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
		return err
	}
	store, err := h.pageAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	var req AssignRoleAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	spec := pageauthz.RoleAccessAssignmentSpec{
		UserID: r.PathValue("userID"), RoleAccessName: req.RoleAccessName,
		NodeID: req.NodeID, FleetWide: req.FleetWide, GrantedBy: actorID,
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := store.AssignRoleAccess(r.Context(), spec, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// RevokeRoleAccessAssignment revokes one RoleAccess assignment, identified
// by its own id.
func (h *Handler) RevokeRoleAccessAssignment(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.RevokeRoleAccessAssignment"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
		return err
	}
	store, err := h.pageAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	if err := store.RevokeRoleAccessAssignment(r.Context(), r.PathValue("assignmentID"), actorID, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// GrantPageAccessRequest is the body of POST /users/{userID}/page-access.
type GrantPageAccessRequest struct {
	Page      pageauthz.Page `json:"page"`
	NodeID    string         `json:"node_id,omitempty"`
	FleetWide bool           `json:"fleet_wide"`
}

// GrantPageAccess grants a user direct access to one page, independent of
// any RoleAccess bundle. Refused — see [pageauthz.ErrPageAccessConflict] —
// when an active RoleAccess assignment already covers the same page for an
// overlapping scope.
func (h *Handler) GrantPageAccess(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GrantPageAccess"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
		return err
	}
	store, err := h.pageAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	var req GrantPageAccessRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	spec := pageauthz.PageGrantSpec{
		UserID: r.PathValue("userID"), Page: req.Page,
		NodeID: req.NodeID, FleetWide: req.FleetWide, GrantedBy: actorID,
	}
	if err := spec.Validate(); err != nil {
		return err
	}
	if err := store.GrantPageAccess(r.Context(), spec, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// RevokePageAccess revokes one direct page grant, identified by its own id.
func (h *Handler) RevokePageAccess(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.RevokePageAccess"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
		return err
	}
	store, err := h.pageAdmin(op)
	if err != nil {
		return err
	}
	actorID, err := h.actor(r, op)
	if err != nil {
		return err
	}

	if err := store.RevokePageAccess(r.Context(), r.PathValue("grantID"), actorID, time.Now()); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
