package v1

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/log"
)

// InstallTargetSource supplies the values an install command is built from.
// Resolved per request: the server's libp2p Peer ID exists only once the
// fleet listener has started.
type InstallTargetSource interface {
	InstallTarget() (fleet.InstallTarget, error)
}

// RedeemLimiter bounds unauthenticated redeem attempts. Satisfied by
// [*fleet.RedeemLimiter].
type RedeemLimiter interface {
	Allow(sourceIP string) bool
}

// CreateInstallCommandRequest is the body of POST /fleet/install-commands.
type CreateInstallCommandRequest struct {
	Environment string `json:"environment"`
	TTLMinutes  *int   `json:"ttl_minutes,omitempty"`
}

// CreateInstallCommandResponse carries the install command, the only place
// the plaintext code ever appears.
type CreateInstallCommandResponse struct {
	ID        string    `json:"id"`
	NodeID    string    `json:"node_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Command   string    `json:"command"`
}

// InstallCommandResponse is one install code as listed. It has no code
// field by design.
type InstallCommandResponse struct {
	ID             string     `json:"id"`
	NodeID         string     `json:"node_id"`
	Environment    string     `json:"environment"`
	CreatedBy      string     `json:"created_by"`
	Status         string     `json:"status"`
	CreatedAt      time.Time  `json:"created_at"`
	ExpiresAt      time.Time  `json:"expires_at"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
	RevokedBy      string     `json:"revoked_by,omitempty"`
	RedeemedAt     *time.Time `json:"redeemed_at,omitempty"`
	RedeemedPeerID string     `json:"redeemed_peer_id,omitempty"`
	RedeemedIP     string     `json:"redeemed_ip,omitempty"`
}

// ListInstallCommandsResponse is every install code, newest first.
type ListInstallCommandsResponse struct {
	InstallCommands []InstallCommandResponse `json:"install_commands"`
	Total           int                      `json:"total"`
}

// RedeemInstallCodeRequest is the body the installer posts.
type RedeemInstallCodeRequest struct {
	Code   string `json:"code"`
	PeerID string `json:"peer_id"`
}

// RedeemInstallCodeResponse is what a successful redeem tells the installer.
type RedeemInstallCodeResponse struct {
	NodeID      string `json:"node_id"`
	Environment string `json:"environment"`
}

func presentInstallCode(c fleet.InstallCode, now time.Time) InstallCommandResponse {
	return InstallCommandResponse{
		ID: c.ID, NodeID: c.NodeID, Environment: c.Environment, CreatedBy: c.CreatedBy,
		Status: c.Status(now), CreatedAt: c.CreatedAt, ExpiresAt: c.ExpiresAt,
		RevokedAt: c.RevokedAt, RevokedBy: c.RevokedBy,
		RedeemedAt: c.RedeemedAt, RedeemedPeerID: c.RedeemedPeerID, RedeemedIP: c.RedeemedIP,
	}
}

// requireFleetEnroll gates the install-command endpoints. Unlike
// [Handler.requirePermission] it fails closed when authorization is not
// wired in: these endpoints admit machines to the fleet.
func (h *Handler) requireFleetEnroll(r *http.Request, op string) (user.Principal, fleet.InstallCodeStore, error) {
	if h.deps.Authz == nil {
		return user.Principal{}, nil, errs.New(errs.CodeUnavailable,
			"install commands need user authentication, which is not configured on this instance").WithOp(op)
	}
	if err := h.requirePermission(r, user.PermissionFleetEnroll, pageauthz.PageNone); err != nil {
		return user.Principal{}, nil, err
	}
	principal, _ := session.PrincipalFrom(r.Context())
	if h.deps.InstallCodes == nil {
		return user.Principal{}, nil, errs.New(errs.CodeUnavailable, "install commands are not available on this instance").WithOp(op)
	}
	return principal, h.deps.InstallCodes, nil
}

// CreateInstallCommand creates a one-time install code with a pre-assigned
// node id and returns the ready install command.
func (h *Handler) CreateInstallCommand(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CreateInstallCommand"
	principal, store, err := h.requireFleetEnroll(r, op)
	if err != nil {
		return err
	}

	var req CreateInstallCommandRequest
	if err := decodeStrict(r, &req); err != nil {
		return err
	}
	spec := fleet.InstallCodeSpec{Environment: req.Environment, TTLMinutes: req.TTLMinutes, CreatedBy: principal.UserID}
	if _, err := spec.TTL(); err != nil {
		return err
	}

	// The target is resolved and validated before a code exists, so a
	// misconfigured server never leaves a pending code nobody can use.
	if h.deps.InstallTargets == nil {
		return errs.New(errs.CodeUnavailable, "install commands are not available on this instance").WithOp(op)
	}
	target, err := h.deps.InstallTargets.InstallTarget()
	if err != nil {
		return err
	}
	if err := target.Validate(); err != nil {
		return err
	}

	code, err := fleet.NewInstallCode()
	if err != nil {
		return err
	}
	command, err := fleet.InstallCommand(target, code.Plaintext)
	if err != nil {
		return err
	}
	created, err := store.CreateInstallCode(r.Context(), code.Hash, spec, time.Now())
	if err != nil {
		return err
	}

	log.FromContext(r.Context()).InfoContext(r.Context(), "install code created",
		slog.String("install_code_id", created.ID), slog.String("node_id", created.NodeID),
		slog.String("environment", created.Environment), slog.String("created_by", created.CreatedBy),
		slog.Time("expires_at", created.ExpiresAt))

	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, r, http.StatusCreated, CreateInstallCommandResponse{
		ID: created.ID, NodeID: created.NodeID, ExpiresAt: created.ExpiresAt, Command: command,
	})
	return nil
}

// ListInstallCommands lists every install code, without any code.
func (h *Handler) ListInstallCommands(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListInstallCommands"
	_, store, err := h.requireFleetEnroll(r, op)
	if err != nil {
		return err
	}
	codes, err := store.ListInstallCodes(r.Context())
	if err != nil {
		return err
	}
	now := time.Now()
	out := make([]InstallCommandResponse, 0, len(codes))
	for _, c := range codes {
		out = append(out, presentInstallCode(c, now))
	}
	httpx.JSON(w, r, http.StatusOK, ListInstallCommandsResponse{InstallCommands: out, Total: len(out)})
	return nil
}

// RevokeInstallCommand revokes a pending install code.
func (h *Handler) RevokeInstallCommand(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.RevokeInstallCommand"
	principal, store, err := h.requireFleetEnroll(r, op)
	if err != nil {
		return err
	}
	revoked, err := store.RevokeInstallCode(r.Context(), r.PathValue("installID"), principal.UserID, time.Now())
	if err != nil {
		return err
	}
	log.FromContext(r.Context()).InfoContext(r.Context(), "install code revoked",
		slog.String("install_code_id", revoked.ID), slog.String("node_id", revoked.NodeID),
		slog.String("revoked_by", revoked.RevokedBy))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// RedeemInstallCode is the one unauthenticated endpoint of the install
// flow: the installer presents the code and the agent's Peer ID, and the
// Peer ID is authorized for the code's pre-assigned node. Every refusal is
// the same [fleet.ErrInstallCodeRejected]; the reason is logged, never sent.
func (h *Handler) RedeemInstallCode(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.RedeemInstallCode"
	if h.deps.InstallCodes == nil || h.deps.RedeemLimiter == nil {
		return errs.New(errs.CodeUnavailable, "install code redemption is not available on this instance").WithOp(op)
	}

	sourceIP := httpx.ClientIP(r)
	if !h.deps.RedeemLimiter.Allow(sourceIP) {
		return errs.New(errs.CodeRateLimited, "too many install attempts; try again later").WithOp(op)
	}

	logger := log.FromContext(r.Context())
	refuse := func(reason string) error {
		logger.WarnContext(r.Context(), "install code redeem refused",
			slog.String("reason", reason), slog.String("source_ip", sourceIP))
		return fleet.ErrInstallCodeRejected
	}

	var req RedeemInstallCodeRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return refuse("malformed request body")
	}
	if !fleet.ValidInstallCodeFormat(req.Code) {
		return refuse("malformed install code")
	}
	if !fleet.ValidPeerID(req.PeerID) {
		return refuse("invalid peer id")
	}

	red, err := h.deps.InstallCodes.RedeemInstallCode(r.Context(), fleet.HashToken(req.Code), req.PeerID,
		net.ParseIP(sourceIP), time.Now())
	if errors.Is(err, fleet.ErrInstallCodeRejected) {
		return refuse(err.Error())
	}
	if err != nil {
		return err
	}

	logger.InfoContext(r.Context(), "install code redeemed",
		slog.String("install_code_id", red.CodeID), slog.String("node_id", red.NodeID),
		slog.String("environment", red.Environment), slog.String("peer_id", req.PeerID),
		slog.String("source_ip", sourceIP))

	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, r, http.StatusOK, RedeemInstallCodeResponse{NodeID: red.NodeID, Environment: red.Environment})
	return nil
}
