package v1

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/access"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// AccessRoleRow is one requested role. Exactly one of NodeID and FleetWide
// must be set; there is no default between them.
type AccessRoleRow struct {
	Role      string `json:"role"`
	NodeID    string `json:"node_id,omitempty"`
	FleetWide bool   `json:"fleet_wide"`
}

// AccessPageRow is one requested direct page grant, scoped like
// [AccessRoleRow].
type AccessPageRow struct {
	Page      pageauthz.Page `json:"page"`
	NodeID    string         `json:"node_id,omitempty"`
	FleetWide bool           `json:"fleet_wide"`
}

// AccessBody is the editable part of a user's access: every role and direct
// page grant they should hold. Bundles and the superadmin role are not part
// of it.
type AccessBody struct {
	Roles []AccessRoleRow `json:"roles"`
	Pages []AccessPageRow `json:"pages"`
}

func (b AccessBody) desired() access.Desired {
	d := access.Desired{Roles: make([]access.Role, 0, len(b.Roles)), Pages: make([]access.Page, 0, len(b.Pages))}
	for _, r := range b.Roles {
		d.Roles = append(d.Roles, access.Role{Scope: pageauthz.Scope{FleetWide: r.FleetWide, NodeID: r.NodeID}, Role: r.Role})
	}
	for _, p := range b.Pages {
		d.Pages = append(d.Pages, access.Page{Scope: pageauthz.Scope{FleetWide: p.FleetWide, NodeID: p.NodeID}, Page: p.Page})
	}
	return d
}

// PutUserAccessRequest is the body of PUT /users/{userID}/access. It
// replaces the user's editable access in full, so both lists are required
// (send [] to clear one) and Version must be the one GET returned.
type PutUserAccessRequest struct {
	Version string `json:"version"`
	AccessBody
}

// AccessRoleResponse is one active role grant.
type AccessRoleResponse struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	NodeID    string    `json:"node_id,omitempty"`
	FleetWide bool      `json:"fleet_wide"`
	GrantedAt time.Time `json:"granted_at"`
	GrantedBy string    `json:"granted_by,omitempty"`
}

// AccessPageResponse is one active direct page grant.
type AccessPageResponse struct {
	ID        string         `json:"id"`
	Page      pageauthz.Page `json:"page"`
	NodeID    string         `json:"node_id,omitempty"`
	FleetWide bool           `json:"fleet_wide"`
	GrantedAt time.Time      `json:"granted_at"`
	GrantedBy string         `json:"granted_by,omitempty"`
}

// AccessBundleResponse is one active bundle assignment and the pages it
// covers. Read-only to the editor.
type AccessBundleResponse struct {
	ID         string           `json:"id"`
	RoleAccess string           `json:"role_access"`
	NodeID     string           `json:"node_id,omitempty"`
	FleetWide  bool             `json:"fleet_wide"`
	Pages      []pageauthz.Page `json:"pages"`
	GrantedAt  time.Time        `json:"granted_at"`
	GrantedBy  string           `json:"granted_by,omitempty"`
}

// UserAccessResponse is GET /users/{userID}/access: the user's role grants
// (never superadmin), direct page grants, and bundle assignments, plus the
// version a PUT must send back.
type UserAccessResponse struct {
	Version string                 `json:"version"`
	Roles   []AccessRoleResponse   `json:"roles"`
	Pages   []AccessPageResponse   `json:"pages"`
	Bundles []AccessBundleResponse `json:"bundles"`
}

// AccessChangeResponse is one grant or revoke a PUT performed. ID is set on
// revokes (the row that was revoked).
type AccessChangeResponse struct {
	Kind      string         `json:"kind"` // "role" or "page"
	ID        string         `json:"id,omitempty"`
	Role      string         `json:"role,omitempty"`
	Page      pageauthz.Page `json:"page,omitempty"`
	NodeID    string         `json:"node_id,omitempty"`
	FleetWide bool           `json:"fleet_wide"`
}

// AccessChangesResponse lists what a PUT changed; both lists are empty for
// a no-op.
type AccessChangesResponse struct {
	Granted []AccessChangeResponse `json:"granted"`
	Revoked []AccessChangeResponse `json:"revoked"`
}

// PutUserAccessResponse is the user's access after a PUT, and what changed.
type PutUserAccessResponse struct {
	Access  UserAccessResponse    `json:"access"`
	Changes AccessChangesResponse `json:"changes"`
}

func presentAccess(s access.State) UserAccessResponse {
	out := UserAccessResponse{
		Version: s.Version(),
		Roles:   make([]AccessRoleResponse, 0, len(s.Roles)),
		Pages:   make([]AccessPageResponse, 0, len(s.Pages)),
		Bundles: make([]AccessBundleResponse, 0, len(s.Bundles)),
	}
	for _, g := range s.Roles {
		out.Roles = append(out.Roles, AccessRoleResponse{
			ID: g.ID, Role: g.Role.Role, NodeID: g.Scope.NodeID, FleetWide: g.Scope.FleetWide, GrantedAt: g.GrantedAt, GrantedBy: g.GrantedBy,
		})
	}
	for _, g := range s.Pages {
		out.Pages = append(out.Pages, AccessPageResponse{
			ID: g.ID, Page: g.Page.Page, NodeID: g.Scope.NodeID, FleetWide: g.Scope.FleetWide, GrantedAt: g.GrantedAt, GrantedBy: g.GrantedBy,
		})
	}
	for _, b := range s.Bundles {
		pages := b.Pages
		if pages == nil {
			pages = []pageauthz.Page{}
		}
		out.Bundles = append(out.Bundles, AccessBundleResponse{
			ID: b.ID, RoleAccess: b.Name, NodeID: b.Scope.NodeID, FleetWide: b.Scope.FleetWide, Pages: pages, GrantedAt: b.GrantedAt, GrantedBy: b.GrantedBy,
		})
	}
	return out
}

func presentChanges(c access.Changes) AccessChangesResponse {
	out := AccessChangesResponse{Granted: []AccessChangeResponse{}, Revoked: []AccessChangeResponse{}}
	for _, r := range c.GrantRoles {
		out.Granted = append(out.Granted, AccessChangeResponse{Kind: "role", Role: r.Role, NodeID: r.Scope.NodeID, FleetWide: r.Scope.FleetWide})
	}
	for _, p := range c.GrantPages {
		out.Granted = append(out.Granted, AccessChangeResponse{Kind: "page", Page: p.Page, NodeID: p.Scope.NodeID, FleetWide: p.Scope.FleetWide})
	}
	for _, g := range c.RevokeRoles {
		out.Revoked = append(out.Revoked, AccessChangeResponse{Kind: "role", ID: g.ID, Role: g.Role.Role, NodeID: g.Scope.NodeID, FleetWide: g.Scope.FleetWide})
	}
	for _, g := range c.RevokePages {
		out.Revoked = append(out.Revoked, AccessChangeResponse{Kind: "page", ID: g.ID, Page: g.Page.Page, NodeID: g.Scope.NodeID, FleetWide: g.Scope.FleetWide})
	}
	return out
}

// GetUserAccess returns one user's access for the access editor. Read-only
// and, like GET /users/{userID}/page-access, not wrapped in protectTarget:
// an admin may see the superadmin's access, just not change it.
func (h *Handler) GetUserAccess(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetUserAccess"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
		return err
	}
	store, err := h.userAdmin(op)
	if err != nil {
		return err
	}
	state, err := store.UserAccess(r.Context(), r.PathValue("userID"))
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentAccess(state))
	return nil
}

// PutUserAccess replaces one user's editable access in a single
// transaction. Registered through protectTarget; refuses the caller's own
// account ([access.ErrOwnAccess]) for every caller.
func (h *Handler) PutUserAccess(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.PutUserAccess"
	if err := h.requirePermission(r, user.PermissionUserManage, pageauthz.PageUsers); err != nil {
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
		return access.ErrOwnAccess
	}

	var req PutUserAccessRequest
	if err := decodeStrict(r, &req); err != nil {
		return err
	}
	switch {
	case req.Version == "":
		return errs.New(errs.CodeInvalidArgument, "version is required; send the version GET returned").WithOp(op)
	case req.Roles == nil || req.Pages == nil:
		return errs.New(errs.CodeInvalidArgument, "roles and pages are both required; send [] to clear one").WithOp(op)
	}

	desired := req.desired()
	if err = h.guardAdminChange(r, store, actorID, targetID, req.Version, desired); err != nil {
		return err
	}
	state, changes, err := store.ReplaceUserAccess(r.Context(), targetID, req.Version, desired, actorID, time.Now())
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, PutUserAccessResponse{Access: presentAccess(state), Changes: presentChanges(changes)})
	return nil
}

// guardAdminChange refuses, for a non-superadmin caller, a save that would
// grant or remove the admin role ([access.ChangesAdmin]). It decides against
// the state at the client's version; ReplaceUserAccess re-checks that version
// inside its transaction, so the save cannot apply to any other state.
func (h *Handler) guardAdminChange(r *http.Request, store UserAdmin, actorID, targetID, version string, desired access.Desired) error {
	super, err := store.IsSuperadmin(r.Context(), actorID)
	if err != nil || super {
		return err
	}
	current, err := store.UserAccess(r.Context(), targetID)
	if err != nil {
		return err
	}
	if current.Version() != version {
		return access.ErrStaleVersion
	}
	if access.ChangesAdmin(current, desired) {
		return user.ErrAdminOnlySuperadmin
	}
	return nil
}

// decodeStrict decodes a JSON body, refusing unknown fields — a misspelled
// key in a full-replacement body would otherwise read as "revoke this".
func decodeStrict(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body: %v", err)
	}
	return nil
}
