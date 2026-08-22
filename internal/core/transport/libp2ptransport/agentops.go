package libp2ptransport

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/hexane/atlas/internal/platform/errs"
)

// AgentOpsProtocolID is the libp2p protocol the control plane uses to ask a
// specific, already-connected Agent to perform one of a small, explicit set
// of operations. It rides over a connection the Agent itself established by
// dialing the control plane — the Agent never listens for new connections
// (see its dial-only [HostOptions] and docs/architecture/agent-design.md
// §3), it only accepts a new stream on a connection it chose to make, for a
// protocol it explicitly registered a handler for. Nothing about this widens
// the Agent's network exposure: no new port, no new listener.
const AgentOpsProtocolID = "/atlas/agentops/1.0.0"

// AgentOpContainerLogs is, deliberately, the only operation this protocol
// implements. There is no generic command field and no dispatch-by-string-
// eval — [RegisterAgentOpsHandler] rejects anything else outright. Adding a
// future operation means adding a new, explicitly-coded case here, reviewed
// like any other endpoint, not flipping on a capability at runtime.
const AgentOpContainerLogs = "container_logs"

// AgentOpsProtocolVersion is sent as every request's ProtocolVersion field,
// checked by [RegisterAgentOpsHandler], so a future incompatible change has
// somewhere to be detected rather than silently misinterpreted.
const AgentOpsProtocolVersion = 1

// AgentOpRequest is the control-plane-to-Agent request, sent once as JSON as
// the first message on the stream. There is no handshake of its own: the
// libp2p Noise handshake already authenticated both Peer IDs and encrypted
// the stream (see [RegisterAgentOpsHandler]).
type AgentOpRequest struct {
	ProtocolVersion int    `json:"protocol_version"`
	Op              string `json:"op"`
	ContainerID     string `json:"container_id"`
	Tail            int    `json:"tail,omitempty"`
	// Since is RFC 3339, matching the convention the plain HTTP
	// /containers/{id}/logs endpoint already uses for its own ?since= query
	// parameter.
	Since      string `json:"since,omitempty"`
	Follow     bool   `json:"follow"`
	Timestamps bool   `json:"timestamps,omitempty"`
}

// AgentOpFrame is one Agent-to-control-plane response frame. For
// container_logs, one frame is sent per log line as it is produced — no
// buffering — until a terminal frame ("error" or "end") closes the session.
type AgentOpFrame struct {
	Type    string    `json:"type"` // "line" | "error" | "end"
	Time    time.Time `json:"time,omitempty"`
	Stream  string    `json:"stream,omitempty"`
	Message string    `json:"message,omitempty"`
	// Reason is set on "error" and, optionally, "end".
	Reason string `json:"reason,omitempty"`
}

// LogLine mirrors internal/plugin/docker's LogLine field-for-field, without
// this package importing that one: internal/core sits below internal/plugin
// in Atlas's dependency direction (see docs/roadmap/phases.md, Phase 0), and
// this package is the client/server transport for the operation, not a
// consumer of a specific plugin's types. The composition root (fleet.go,
// agent.go) does the two-line adaptation on either side.
type LogLine struct {
	Time    time.Time
	Stream  string
	Message string
}

// ContainerLogsFunc is what [RegisterAgentOpsHandler] calls to actually read
// logs on the Agent's side — supplied by the caller (agent.go) as a thin
// adapter over the Agent's existing docker.Client.Logs, so this package
// never depends on internal/plugin/docker either. Its shape mirrors
// docker.Client.Logs exactly: two channels, no buffering, ctx-cancellable.
type ContainerLogsFunc func(ctx context.Context, containerID string, tail int, since time.Time, follow, timestamps bool) (<-chan LogLine, <-chan error, error)

// Concurrency and timeout tuning. Small, fixed constants for this milestone
// — see the SessionLimiter doc for why a limit exists at all, and
// docs/roadmap/phases.md for the "constant now, configurable later if
// needed" convention already used elsewhere (e.g. maxFollowDuration in
// internal/api/v1/containers.go, which maxSessionDuration mirrors).
const (
	// DefaultMaxConcurrentSessions bounds how many AgentOps sessions one
	// Agent will run at once. Without this, a control-plane bug — or an
	// authenticated peer behaving badly — could open unbounded concurrent
	// Docker log streams against a single host.
	DefaultMaxConcurrentSessions = 4
	// streamOpenTimeout bounds NewStream, so a request against an Agent that
	// looks connected but is actually wedged fails fast rather than hanging
	// indefinitely — required behavior, not just a nicety.
	streamOpenTimeout = 10 * time.Second
	// maxSessionDuration mirrors containers.go's maxFollowDuration: an
	// operator still watching after this long reconnects, which the
	// frontend already does without asking.
	maxSessionDuration = 6 * time.Hour
)

// SessionLimiter bounds concurrent AgentOps sessions. Safe for concurrent
// use; a zero value is not usable, use [NewSessionLimiter].
type SessionLimiter struct {
	sem chan struct{}
}

// NewSessionLimiter returns a limiter allowing at most max concurrent
// sessions.
func NewSessionLimiter(max int) *SessionLimiter {
	if max <= 0 {
		max = DefaultMaxConcurrentSessions
	}
	return &SessionLimiter{sem: make(chan struct{}, max)}
}

func (l *SessionLimiter) tryAcquire() bool {
	select {
	case l.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *SessionLimiter) release() { <-l.sem }

// RequestContainerLogs opens a container_logs AgentOps session with the
// Agent at agentPeerID, over h's existing connection to it, and returns the
// response as the same two-channel shape docker.Client.Logs uses locally —
// so internal/api/v1/containers.go can forward a local and a remote session
// through the identical loop (see RemoteLogSource/followLogs there).
//
// Authentication is the libp2p Noise handshake and nothing else. agentPeerID
// is not an address hint that some later handshake re-checks: go-libp2p
// refuses to hand back a stream on a connection whose remote peer is not
// that exact Peer ID, so by the time this function returns a stream, the
// identity of the far end is already proven. Which node that peer speaks for
// is the caller's authorization question, answered from the agent_peers
// table before this is ever called (see app.fleetPipeline.ContainerLogs) —
// authentication here, authorization there, deliberately not conflated.
//
// This replaces an earlier reversed-mTLS handshake over the same stream. It
// proved a second identity, derived from enrollment, on a channel whose
// identity was already proven — which is exactly the enrollment dependency
// ADR-0012 removes.
func RequestContainerLogs(ctx context.Context, h host.Host, agentPeerID peer.ID, req AgentOpRequest) (<-chan AgentOpFrame, error) {
	const op = "libp2ptransport.RequestContainerLogs"

	openCtx, openCancel := context.WithTimeout(ctx, streamOpenTimeout)
	stream, err := h.NewStream(openCtx, agentPeerID, AgentOpsProtocolID)
	openCancel()
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not open an agentops stream to the agent").WithOp(op)
	}

	if err := json.NewEncoder(stream).Encode(req); err != nil {
		_ = stream.Reset()
		return nil, errs.Wrap(err, errs.CodeInternal, "could not send the log request").WithOp(op)
	}

	frames := make(chan AgentOpFrame)
	done := make(chan struct{})

	// One goroutine owns Reset(), fired exactly once whichever happens
	// first: ctx cancellation (propagates the reset to the Agent, which is
	// what unblocks its own Read-based close-detection — see
	// handleAgentOpsStream) or the decode loop below ending on its own.
	// network.Stream has no native context awareness; a blocked Read
	// reliably returns once the stream is reset, the same idiom
	// containers.go's WebSocket side uses via conn.CloseRead(ctx).
	go func() {
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = stream.Reset()
	}()

	go func() {
		defer close(frames)
		defer close(done)
		dec := json.NewDecoder(stream)
		for {
			var frame AgentOpFrame
			if err := dec.Decode(&frame); err != nil {
				return
			}
			select {
			case frames <- frame:
			case <-ctx.Done():
				return
			}
			if frame.Type != "line" {
				return
			}
		}
	}()

	return frames, nil
}

// AgentOpsRelationshipLookup decides whether an inbound AgentOps stream may
// be served, keyed by the libp2p peer it arrived from.
//
// remotePeer is not a claim: the Noise handshake proved it before this
// stream existed. The Agent answers with the only question left — is this
// Peer ID the control plane of one of my configured relationships, and is
// the operation allowed on this host. ok is false for any peer that is not
// one of the Agent's own control planes, and the stream is dropped outright.
// That check is what stops one relationship's control plane, or any other
// peer on the network, from driving an operation on behalf of another.
type AgentOpsRelationshipLookup func(remotePeer peer.ID) (allowContainerLogs bool, ok bool)

// RegisterAgentOpsHandler wires h to accept AgentOps streams from any
// Control-Plane relationship the Agent is bootstrapped for. h need not, and
// for the Agent must not, accept new inbound connections for this — see the
// package doc on [AgentOpsProtocolID]; this only registers a handler for one
// more protocol on a host that already only ever dials out.
func RegisterAgentOpsHandler(h host.Host, logs ContainerLogsFunc, limiter *SessionLimiter, lookup AgentOpsRelationshipLookup) {
	if limiter == nil {
		limiter = NewSessionLimiter(DefaultMaxConcurrentSessions)
	}
	h.SetStreamHandler(AgentOpsProtocolID, func(s network.Stream) {
		handleAgentOpsStream(s, lookup, logs, limiter)
	})
}

func handleAgentOpsStream(s network.Stream, lookup AgentOpsRelationshipLookup, logs ContainerLogsFunc, limiter *SessionLimiter) {
	defer s.Close()

	// The peer identity is already established — cryptographically, by the
	// Noise handshake that brought this connection up, before any Atlas byte
	// was exchanged. A peer that is not one of this Agent's own control
	// planes is dropped silently: there is no one to report an error to, the
	// same posture as an HTTP listener refusing an unauthenticated request.
	allowContainerLogs, ok := lookup(s.Conn().RemotePeer())
	if !ok {
		return
	}

	var req AgentOpRequest
	if err := json.NewDecoder(s).Decode(&req); err != nil {
		return
	}

	enc := json.NewEncoder(s)

	if req.ProtocolVersion != AgentOpsProtocolVersion {
		_ = enc.Encode(AgentOpFrame{Type: "error", Reason: fmt.Sprintf("unsupported protocol version %d", req.ProtocolVersion)})
		return
	}

	if req.Op != AgentOpContainerLogs {
		_ = enc.Encode(AgentOpFrame{Type: "error", Reason: fmt.Sprintf("unsupported operation %q", req.Op)})
		return
	}
	if !allowContainerLogs {
		_ = enc.Encode(AgentOpFrame{Type: "error", Reason: "container log streaming is not authorized on this agent"})
		return
	}

	if !limiter.tryAcquire() {
		_ = enc.Encode(AgentOpFrame{Type: "error", Reason: "too many concurrent log sessions on this agent"})
		return
	}
	defer limiter.release()

	var since time.Time
	if req.Since != "" {
		if t, err := time.Parse(time.RFC3339, req.Since); err == nil {
			since = t
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), maxSessionDuration)
	defer cancel()
	go func() {
		<-ctx.Done()
		_ = s.Reset()
	}()
	// Mirrors the outbound direction above: this handler never expects
	// another message after the request, so a blocked Read only returns
	// when the control plane resets/closes the stream (e.g. the browser
	// disconnected) — the signal that cancels ctx and reaches the Docker
	// call below, same idiom as containers.go's conn.CloseRead(ctx).
	go func() {
		var discard [1]byte
		_, _ = s.Read(discard[:])
		cancel()
	}()

	lines, errCh, err := logs(ctx, req.ContainerID, req.Tail, since, req.Follow, req.Timestamps)
	if err != nil {
		_ = enc.Encode(AgentOpFrame{Type: "error", Reason: err.Error()})
		return
	}

	for lines != nil || errCh != nil {
		select {
		case <-ctx.Done():
			return

		case line, open := <-lines:
			if !open {
				lines = nil
				continue
			}
			if err := enc.Encode(AgentOpFrame{Type: "line", Time: line.Time, Stream: line.Stream, Message: line.Message}); err != nil {
				return
			}

		case streamErr, open := <-errCh:
			if !open {
				errCh = nil
				continue
			}
			if streamErr != nil {
				_ = enc.Encode(AgentOpFrame{Type: "error", Reason: streamErr.Error()})
				return
			}
			errCh = nil
		}
	}
	_ = enc.Encode(AgentOpFrame{Type: "end"})
}
