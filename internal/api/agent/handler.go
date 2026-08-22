// Package agent serves the API agents use to enroll, renew their
// certificate, and push telemetry. See docs/architecture/agent-design.md.
//
// The same handlers are mounted on two listeners that authenticate
// differently, and every handler takes its node identity from whichever
// proof the caller actually supplied:
//
//   - HTTPS: a verified client certificate. The listener's [tls.Config] is
//     VerifyClientCertIfGiven rather than RequireAndVerifyClientCert because
//     Enroll is reached by agents that do not have one yet — see
//     [httpx.TLSServer] and [pki.ServerTLSConfig].
//   - libp2p: no TLS and no certificate. The Noise handshake proves the
//     caller's Peer ID, [PeerAuthMiddleware] authorizes that Peer ID against
//     the agent_peers table, and the node id and environment come from that
//     authorization record. Renew has nothing to renew on this path and
//     refuses; see docs/adr/0012-connect-by-identity.md.
package agent

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math"
	"net"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/pki"
)

// DefaultMaxClockSkew is the tolerance beyond which a sample's timestamp is
// rejected rather than trusted. See docs/architecture/agent-design.md §3, H6.
const DefaultMaxClockSkew = 5 * time.Minute

// ProtocolVersion is the current agent protocol version, returned at
// enrollment and enforced on every telemetry push. See
// docs/architecture/agent-design.md §2, M1.
//
// It is defined once in internal/core/transport, below both this handler and
// the agent's own client, so the two ends cannot drift.
const ProtocolVersion = transport.ProtocolVersion

// ClockSkewRecorder records a node's most recently observed clock skew.
// Satisfied by [*metric.Repository].
type ClockSkewRecorder interface {
	UpdateClockSkew(ctx context.Context, nodeID string, skewSeconds float64) error
}

// Deps are the collaborators the agent handlers need.
type Deps struct {
	CA           *pki.CA
	Enroller     *fleet.Enroller
	Denylist     fleet.DenylistStore
	Router       *transport.Router
	ClockSkew    ClockSkewRecorder
	Logger       *slog.Logger
	MaxClockSkew time.Duration
}

// Handler serves the agent API.
type Handler struct{ deps Deps }

// NewHandler builds the handler, applying defaults.
func NewHandler(deps Deps) *Handler {
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	if deps.MaxClockSkew <= 0 {
		deps.MaxClockSkew = DefaultMaxClockSkew
	}
	return &Handler{deps: deps}
}

// Mount registers every agent route on mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.Handle("POST /api/v1/agent/enroll", httpx.Handler(h.Enroll))
	mux.Handle("POST /api/v1/agent/renew", httpx.Handler(h.Renew))
	mux.Handle("POST /api/v1/agent/telemetry", httpx.Handler(h.Telemetry))
	mux.Handle("POST /api/v1/agent/heartbeat", httpx.Handler(h.Heartbeat))
}

// EnrollRequest is a first-time enrollment.
type EnrollRequest struct {
	Token  string `json:"token"`
	NodeID string `json:"node_id"`
	// CSR is a base64-encoded DER certificate signing request.
	CSR string `json:"csr"`
}

// CertResponse is an issued certificate.
type CertResponse struct {
	// Certificate is base64-encoded DER.
	Certificate string `json:"certificate"`
	// CACertificate is base64-encoded DER, present only on enrollment — an
	// already-enrolled agent already has it.
	CACertificate   string    `json:"ca_certificate,omitempty"`
	NotAfter        time.Time `json:"not_after"`
	ProtocolVersion int       `json:"protocol_version"`
}

// Enroll exchanges a bounded enrollment token and a CSR for a node
// certificate. No client certificate is required or checked.
func (h *Handler) Enroll(w http.ResponseWriter, r *http.Request) error {
	const op = "agent.Handler.Enroll"

	var req EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "could not decode the enrollment request").WithOp(op)
	}
	csrDER, err := base64.StdEncoding.DecodeString(req.CSR)
	if err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "csr is not valid base64").WithOp(op)
	}

	leaf, err := h.deps.Enroller.Enroll(r.Context(), fleet.EnrollRequest{
		TokenPlaintext: req.Token, NodeID: req.NodeID, CSRDER: csrDER, SourceIP: sourceIP(r),
	})
	if err != nil {
		h.deps.Logger.WarnContext(r.Context(), "enrollment refused",
			slog.String("node_id", req.NodeID), slog.String("remote_addr", r.RemoteAddr), slog.String("error", err.Error()))
		return err
	}

	h.deps.Logger.InfoContext(r.Context(), "node enrolled", slog.String("node_id", req.NodeID))
	httpx.JSON(w, r, http.StatusOK, CertResponse{
		Certificate:     base64.StdEncoding.EncodeToString(leaf.Raw),
		CACertificate:   base64.StdEncoding.EncodeToString(h.deps.CA.Cert().Raw),
		NotAfter:        leaf.NotAfter,
		ProtocolVersion: ProtocolVersion,
	})
	return nil
}

// RenewRequest rotates an already-enrolled agent's certificate.
type RenewRequest struct {
	CSR string `json:"csr"`
}

// Renew issues a fresh certificate over the agent's existing mTLS connection.
// No token: the verified peer certificate is the proof of identity.
func (h *Handler) Renew(w http.ResponseWriter, r *http.Request) error {
	const op = "agent.Handler.Renew"

	cert, err := peerCert(r)
	if err != nil {
		return err
	}

	var req RenewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "could not decode the renewal request").WithOp(op)
	}
	csrDER, err := base64.StdEncoding.DecodeString(req.CSR)
	if err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "csr is not valid base64").WithOp(op)
	}

	leaf, err := h.deps.Enroller.Renew(r.Context(), fleet.RenewRequest{PeerCert: cert, CSRDER: csrDER})
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, CertResponse{
		Certificate:     base64.StdEncoding.EncodeToString(leaf.Raw),
		NotAfter:        leaf.NotAfter,
		ProtocolVersion: ProtocolVersion,
	})
	return nil
}

// TelemetryRequest is a batch of envelopes pushed in one request.
type TelemetryRequest struct {
	ProtocolVersion int                  `json:"protocol_version"`
	Envelopes       []transport.Envelope `json:"envelopes"`
}

// RejectedEnvelope names one envelope in a batch that was not accepted.
type RejectedEnvelope struct {
	EnvelopeID string `json:"envelope_id"`
	Reason     string `json:"reason"`
}

// TelemetryResponse reports what happened to a batch, and carries the
// backpressure signal — see docs/architecture/agent-design.md §2.
type TelemetryResponse struct {
	Accepted int                `json:"accepted"`
	Rejected []RejectedEnvelope `json:"rejected,omitempty"`
	// Directive is "ok", "slow_down", or "pause".
	Directive    string `json:"directive"`
	RetryAfterMs int    `json:"retry_after_ms,omitempty"`
}

// Telemetry accepts a batch of envelopes over an authenticated connection.
//
// Every envelope's Origin.NodeID is overwritten from the caller's
// authenticated identity — the verified peer certificate over HTTPS, or the
// authorized Peer ID's agent_peers record over libp2p — before it reaches a
// receiver. This is C1: identity is bound to what the caller proved, never
// to a claim in the request body. A payload naming a
// different node is rejected outright and logged as a security event, since a
// mismatch is what impersonation looks like.
func (h *Handler) Telemetry(w http.ResponseWriter, r *http.Request) error {
	const op = "agent.Handler.Telemetry"

	nodeID, err := h.peerNodeID(r)
	if err != nil {
		return err
	}
	if err := h.checkDenylist(r.Context(), nodeID); err != nil {
		return err
	}

	var req TelemetryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "could not decode the telemetry request").WithOp(op)
	}

	// Whole batch, not per-envelope: if the ends disagree about the wire
	// contract, every envelope was decoded under an assumption that no
	// longer holds.
	if req.ProtocolVersion != ProtocolVersion {
		h.deps.Logger.WarnContext(r.Context(), "telemetry rejected: protocol version mismatch",
			slog.String("node_id", nodeID),
			slog.Int("agent_protocol_version", req.ProtocolVersion),
			slog.Int("server_protocol_version", ProtocolVersion))
		return errs.New(errs.CodeInvalidArgument, "unsupported agent protocol version").WithOp(op).
			WithDetail("agent_protocol_version", req.ProtocolVersion).
			WithDetail("server_protocol_version", ProtocolVersion)
	}

	// Set only for a libp2p peer, whose authorization names the environment
	// it was registered for. That registration is an operator's statement
	// about the machine, so it outranks the agent's own configuration for
	// the same reason the node id does.
	authorizedEnvironment := ""
	if id, ok := PeerIdentityFrom(r.Context()); ok {
		authorizedEnvironment = id.Environment
	}

	now := time.Now()
	resp := TelemetryResponse{Directive: "ok"}

	var latestSkew *float64
	var latestSentAt time.Time

	for _, env := range req.Envelopes {
		if env.Origin.NodeID != "" && env.Origin.NodeID != nodeID {
			h.deps.Logger.WarnContext(r.Context(), "envelope identity mismatch",
				slog.String("peer_node_id", nodeID), slog.String("claimed_node_id", env.Origin.NodeID),
				slog.String("envelope_id", env.ID))
			resp.Rejected = append(resp.Rejected, RejectedEnvelope{EnvelopeID: env.ID, Reason: "identity_mismatch"})
			continue
		}
		env.Origin.NodeID = nodeID
		if authorizedEnvironment != "" {
			env.Origin.Environment = authorizedEnvironment
		}

		// After the identity rebind: an empty claimed NodeID is only valid
		// once the authenticated identity has supplied one.
		if err := env.Validate(); err != nil {
			h.deps.Logger.WarnContext(r.Context(), "envelope failed validation",
				slog.String("node_id", nodeID), slog.String("envelope_id", env.ID),
				slog.String("error", err.Error()))
			resp.Rejected = append(resp.Rejected, RejectedEnvelope{EnvelopeID: env.ID, Reason: "invalid_envelope"})
			continue
		}

		skew := now.Sub(env.SentAt).Seconds()
		if env.SentAt.After(latestSentAt) {
			s := skew
			latestSkew = &s
			latestSentAt = env.SentAt
		}
		if math.Abs(skew) > h.deps.MaxClockSkew.Seconds() {
			resp.Rejected = append(resp.Rejected, RejectedEnvelope{EnvelopeID: env.ID, Reason: "clock_skew"})
			continue
		}

		if err := h.deps.Router.Receive(r.Context(), env); err != nil {
			h.deps.Logger.ErrorContext(r.Context(), "envelope rejected by receiver",
				slog.String("node_id", nodeID), slog.String("envelope_id", env.ID), slog.String("error", err.Error()))
			resp.Rejected = append(resp.Rejected, RejectedEnvelope{EnvelopeID: env.ID, Reason: string(errs.CodeOf(err))})
			continue
		}
		resp.Accepted++
	}

	if latestSkew != nil && h.deps.ClockSkew != nil {
		if err := h.deps.ClockSkew.UpdateClockSkew(r.Context(), nodeID, *latestSkew); err != nil {
			h.deps.Logger.WarnContext(r.Context(), "could not record clock skew",
				slog.String("node_id", nodeID), slog.String("error", err.Error()))
		}
	}

	httpx.JSON(w, r, http.StatusOK, resp)
	return nil
}

// HeartbeatResponse acknowledges liveness and carries the backpressure
// signal for an agent with nothing else to send.
type HeartbeatResponse struct {
	Directive    string    `json:"directive"`
	RetryAfterMs int       `json:"retry_after_ms,omitempty"`
	ServerTime   time.Time `json:"server_time"`
}

// Heartbeat acknowledges an agent that is alive but has nothing to push.
func (h *Handler) Heartbeat(w http.ResponseWriter, r *http.Request) error {
	nodeID, err := h.peerNodeID(r)
	if err != nil {
		return err
	}
	if err := h.checkDenylist(r.Context(), nodeID); err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, HeartbeatResponse{Directive: "ok", ServerTime: time.Now()})
	return nil
}

func (h *Handler) checkDenylist(ctx context.Context, nodeID string) error {
	const op = "agent.Handler.checkDenylist"
	if h.deps.Denylist == nil {
		return nil
	}
	denied, err := h.deps.Denylist.IsDenied(ctx, nodeID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not check the denylist").WithOp(op)
	}
	if denied {
		return errs.New(errs.CodeUnauthenticated, "this node has been ejected from the fleet").
			WithOp(op).WithDetail("node_id", nodeID)
	}
	return nil
}

func peerCert(r *http.Request) (*x509.Certificate, error) {
	const op = "agent.peerCert"
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil, errs.New(errs.CodeUnauthenticated, "this endpoint requires a client certificate").WithOp(op)
	}
	return r.TLS.PeerCertificates[0], nil
}

// peerNodeID returns the authenticated node identity for this request.
//
// Two transports, two proofs, one rule: the node id is always taken from
// something the caller proved, never from something it claimed. Over libp2p
// the proof is the Noise handshake plus the operator-registered peer
// authorization [PeerAuthMiddleware] resolved (see docs/adr/0012); over
// HTTPS it is the verified client certificate. A request body naming a node
// participates in neither, which is what keeps [Handler.Telemetry]'s
// identity rebinding meaningful on both paths.
func (h *Handler) peerNodeID(r *http.Request) (string, error) {
	if id, ok := PeerIdentityFrom(r.Context()); ok {
		return id.NodeID, nil
	}
	cert, err := peerCert(r)
	if err != nil {
		return "", err
	}
	return pki.PeerNodeID(cert)
}

func sourceIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return net.ParseIP(r.RemoteAddr)
	}
	return net.ParseIP(host)
}
