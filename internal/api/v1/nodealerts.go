package v1

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// NodeAlertStore mutes and archives nodes. Satisfied by
// [*github.com/hexane/atlas/internal/storage/notification.Repository].
type NodeAlertStore interface {
	MuteNode(ctx context.Context, nodeID string, until time.Time) (notification.NodeAlerts, error)
	// EndMuteExplicitly unmutes a node and resets its states; on a node that
	// is not muted it changes nothing. ok is false for an unknown node.
	EndMuteExplicitly(ctx context.Context, nodeID string) (ok bool, err error)
	ArchiveNode(ctx context.Context, nodeID string, at time.Time) (notification.NodeAlerts, error)
	UnarchiveNode(ctx context.Context, nodeID string) error
}

// MuteNodeRequest is the body of PUT /nodes/{nodeID}/alerts-mute.
type MuteNodeRequest struct {
	Duration string `json:"duration"`
}

// NodeAlertsResponse is a node's mute and archive state, with the same
// fields [NodeResponse] carries.
type NodeAlertsResponse struct {
	NodeID           string     `json:"node_id"`
	AlertsMutedUntil *time.Time `json:"alerts_muted_until"`
	ArchivedAt       *time.Time `json:"archived_at"`
}

// presentMute hides a mute that is already over but not yet ended by the
// evaluator, so a non-null value always means muted now.
func presentMute(until *time.Time, now time.Time) *time.Time {
	if until == nil || !until.After(now) {
		return nil
	}
	return utcPtr(until)
}

// utcPtr keeps [notification.MutedIndefinitely] recognisable whatever zone
// the database driver scanned it into.
func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

func presentNodeAlerts(nodeID string, a notification.NodeAlerts) NodeAlertsResponse {
	return NodeAlertsResponse{
		NodeID: nodeID, AlertsMutedUntil: presentMute(a.MutedUntil, time.Now()), ArchivedAt: utcPtr(a.ArchivedAt),
	}
}

// nodeAlerts authorizes the caller: muting and archiving silence a node for
// everyone, so they need notifications.manage fleet-wide, not a grant on the
// node.
func (h *Handler) nodeAlerts(r *http.Request, op string) (NodeAlertStore, error) {
	if err := h.requirePermission(r, user.PermissionNotificationsManage, pageauthz.PageNone); err != nil {
		return nil, err
	}
	if h.deps.NodeAlerts == nil {
		return nil, errs.New(errs.CodeUnavailable, "node alert settings are not available on this instance").WithOp(op)
	}
	return h.deps.NodeAlerts, nil
}

// MuteNodeAlerts mutes a node's notifications, replacing any current mute.
func (h *Handler) MuteNodeAlerts(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.MuteNodeAlerts"
	store, err := h.nodeAlerts(r, op)
	if err != nil {
		return err
	}
	var req MuteNodeRequest
	if err := decodeStrict(r, &req); err != nil {
		return err
	}
	until, err := notification.MuteUntil(req.Duration, time.Now())
	if err != nil {
		return err
	}

	nodeID := r.PathValue("nodeID")
	alerts, err := store.MuteNode(r.Context(), nodeID, until)
	if err != nil {
		return err
	}
	logAdminAction(r, "node alerts muted", slog.String("node_id", nodeID),
		slog.String("duration", req.Duration), slog.Time("until", until))
	httpx.JSON(w, r, http.StatusOK, presentNodeAlerts(nodeID, alerts))
	return nil
}

// UnmuteNodeAlerts ends a node's mute early.
func (h *Handler) UnmuteNodeAlerts(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UnmuteNodeAlerts"
	store, err := h.nodeAlerts(r, op)
	if err != nil {
		return err
	}
	nodeID := r.PathValue("nodeID")
	ok, err := store.EndMuteExplicitly(r.Context(), nodeID)
	if err != nil {
		return err
	}
	if !ok {
		return errs.New(errs.CodeNotFound, "node not found").WithOp(op).WithDetail("node_id", nodeID)
	}
	logAdminAction(r, "node alerts unmuted", slog.String("node_id", nodeID))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ArchiveNode retires a node from notifications without deleting anything.
func (h *Handler) ArchiveNode(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ArchiveNode"
	store, err := h.nodeAlerts(r, op)
	if err != nil {
		return err
	}
	nodeID := r.PathValue("nodeID")
	alerts, err := store.ArchiveNode(r.Context(), nodeID, time.Now())
	if err != nil {
		return err
	}
	logAdminAction(r, "node archived", slog.String("node_id", nodeID))
	httpx.JSON(w, r, http.StatusOK, presentNodeAlerts(nodeID, alerts))
	return nil
}

// UnarchiveNode returns a node to notifications.
func (h *Handler) UnarchiveNode(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UnarchiveNode"
	store, err := h.nodeAlerts(r, op)
	if err != nil {
		return err
	}
	nodeID := r.PathValue("nodeID")
	if err := store.UnarchiveNode(r.Context(), nodeID); err != nil {
		return err
	}
	logAdminAction(r, "node unarchived", slog.String("node_id", nodeID))
	w.WriteHeader(http.StatusNoContent)
	return nil
}
