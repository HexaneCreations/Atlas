package libp2ptransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/pki"
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

// AgentOpRequest is the control-plane-to-Agent request, sent once as JSON
// immediately after the reversed mTLS handshake (see
// [RegisterAgentOpsHandler]) completes.
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
	// Agent will run at once. Without this, a control-plane bug — or a
	// credential-holding peer behaving badly — could open unbounded
	// concurrent Docker log streams against a single host.
	DefaultMaxConcurrentSessions = 4
	// streamOpenTimeout bounds NewStream, so a request against an Agent that
	// looks connected but is actually wedged fails fast rather than hanging
	// indefinitely — required behavior, not just a nicety.
	streamOpenTimeout = 10 * time.Second
	// handshakeTimeout bounds the reversed TLS handshake on both sides.
	handshakeTimeout = 10 * time.Second
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

// streamConn adapts a [network.Stream] into a [net.Conn] so crypto/tls can
// wrap it. This is the same trick go-libp2p-gostream uses internally
// (verified against its source: RemoteAddr's peer.ID, read through
// net.Conn.RemoteAddr().String(), is exactly how fleet.go's NodeID-to-PeerID
// recording gets an Agent's Peer ID off an ordinary HTTP request today) —
// duplicated here in miniature because gostream's version is unexported and
// tied to its own Dial/Listen, neither of which fits a stream opened
// directly via host.NewStream/SetStreamHandler.
type streamConn struct{ network.Stream }

func (c streamConn) LocalAddr() net.Addr  { return peerAddr{c.Conn().LocalPeer()} }
func (c streamConn) RemoteAddr() net.Addr { return peerAddr{c.Conn().RemotePeer()} }

type peerAddr struct{ id peer.ID }

func (a peerAddr) Network() string { return "libp2p-agentops" }
func (a peerAddr) String() string  { return a.id.String() }

// verifiedLeaf parses the certificate a TLS peer presented and checks it
// chains to pool for the given key usage. It is the shared half of both
// verification directions below; identity is checked by the caller once this
// returns a cert known to be signed by the fleet's own CA.
func verifiedLeaf(rawCerts [][]byte, pool *x509.CertPool, usage x509.ExtKeyUsage) (*x509.Certificate, error) {
	if len(rawCerts) == 0 {
		return nil, fmt.Errorf("no certificate was presented")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return nil, fmt.Errorf("parse presented certificate: %w", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
		return nil, fmt.Errorf("verify certificate chain: %w", err)
	}
	return cert, nil
}

// verifyControlPlaneCertificate is the Agent's check on the certificate the
// control plane presents: real cert, signed by the fleet CA, AND
// specifically the control plane's own identity — not merely any certificate
// that CA happens to have issued (agent leaf certs are signed by the same
// CA). This is what stops one Agent from posing as the control plane to
// another.
func verifyControlPlaneCertificate(pool *x509.CertPool) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		cert, err := verifiedLeaf(rawCerts, pool, x509.ExtKeyUsageServerAuth)
		if err != nil {
			return err
		}
		if cert.Subject.CommonName != pki.ControlPlaneCommonName {
			return fmt.Errorf("presented certificate identifies %q, not the Atlas control plane", cert.Subject.CommonName)
		}
		return nil
	}
}

// verifyAgentCertificate is the control plane's check on the certificate an
// Agent presents: real cert, signed by the fleet CA, AND identifies exactly
// the node the control plane meant to reach — using [pki.PeerNodeID], the
// same authoritative node-identity extraction every other authenticated
// endpoint in Atlas already uses (see internal/api/agent). This stops the
// control plane from ever acting on a response from the wrong node, even if
// the NodeID-to-PeerID mapping were ever stale.
func verifyAgentCertificate(pool *x509.CertPool, expectedNodeID string) func([][]byte, [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		cert, err := verifiedLeaf(rawCerts, pool, x509.ExtKeyUsageClientAuth)
		if err != nil {
			return err
		}
		nodeID, err := pki.PeerNodeID(cert)
		if err != nil {
			return err
		}
		if nodeID != expectedNodeID {
			return fmt.Errorf("certificate identifies node %q, expected %q", nodeID, expectedNodeID)
		}
		return nil
	}
}

// RequestContainerLogs opens a container_logs AgentOps session with the
// Agent at agentPeerID, over h's existing connection to it, and returns the
// response as the same two-channel shape docker.Client.Logs uses locally —
// so internal/api/v1/containers.go can forward a local and a remote session
// through the identical loop (see RemoteLogSource/followLogs there).
//
// Authentication is mutual TLS, roles reversed from every other Atlas
// connection: the control plane is the TLS client here, the Agent the TLS
// server, because this stream is control-plane-initiated. Both sides present
// their existing enrollment-issued certificates and trust the same fleet CA
// — no new PKI. Standard hostname-based verification does not apply to a raw
// libp2p stream (there is no DNS name), so both sides skip it and verify
// explicitly instead: see verifyAgentCertificate.
func RequestContainerLogs(ctx context.Context, h host.Host, agentPeerID peer.ID, expectedNodeID string, caCert *x509.Certificate, controlPlaneLeaf tls.Certificate, req AgentOpRequest) (<-chan AgentOpFrame, error) {
	const op = "libp2ptransport.RequestContainerLogs"

	openCtx, openCancel := context.WithTimeout(ctx, streamOpenTimeout)
	stream, err := h.NewStream(openCtx, agentPeerID, AgentOpsProtocolID)
	openCancel()
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not open an agentops stream to the agent").WithOp(op)
	}

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	tlsConn := tls.Client(streamConn{stream}, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{controlPlaneLeaf},
		// InsecureSkipVerify disables Go's built-in hostname/SAN check,
		// which has no meaning on a raw libp2p stream; VerifyPeerCertificate
		// below performs a strictly more specific check (chain-to-CA plus
		// exact node identity) in its place. This is not "skip
		// verification" — it is "verify differently, and check more."
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyAgentCertificate(pool, expectedNodeID),
	})

	hsCtx, hsCancel := context.WithTimeout(ctx, handshakeTimeout)
	err = tlsConn.HandshakeContext(hsCtx)
	hsCancel()
	if err != nil {
		_ = stream.Reset()
		return nil, errs.Wrap(err, errs.CodeUnauthenticated, "could not verify the agent's identity").WithOp(op)
	}

	if err := json.NewEncoder(tlsConn).Encode(req); err != nil {
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
		dec := json.NewDecoder(tlsConn)
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

// RegisterAgentOpsHandler wires h to accept AgentOps streams from the
// control plane. h need not, and for the Agent must not, accept new inbound
// connections for this — see the package doc on [AgentOpsProtocolID]; this
// only registers a handler for one more protocol on a host that already only
// ever dials out.
//
// caCert authenticates the control plane's presented certificate against the
// same fleet CA the Agent already trusts from enrollment. getCert supplies
// the Agent's own certificate to present in return — a function rather than
// a static value so a certificate renewed mid-session (see
// credentialHolder.GetClientCertificate in internal/agent) is picked up by
// the next handshake automatically, the same as every other Atlas TLS
// config already does.
func RegisterAgentOpsHandler(h host.Host, caCert *x509.Certificate, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), logs ContainerLogsFunc, limiter *SessionLimiter) {
	if limiter == nil {
		limiter = NewSessionLimiter(DefaultMaxConcurrentSessions)
	}
	h.SetStreamHandler(AgentOpsProtocolID, func(s network.Stream) {
		handleAgentOpsStream(s, caCert, getCert, logs, limiter)
	})
}

func handleAgentOpsStream(s network.Stream, caCert *x509.Certificate, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), logs ContainerLogsFunc, limiter *SessionLimiter) {
	defer s.Close()

	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	tlsConn := tls.Server(streamConn{s}, &tls.Config{
		MinVersion:     tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return getCert(nil) },
		// ClientAuth forces the peer to present a certificate at all; which
		// certificate is acceptable is decided explicitly below, for the
		// same reason as the control-plane side — see RequestContainerLogs.
		ClientAuth:            tls.RequireAnyClientCert,
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifyControlPlaneCertificate(pool),
	})

	hsCtx, hsCancel := context.WithTimeout(context.Background(), handshakeTimeout)
	err := tlsConn.HandshakeContext(hsCtx)
	hsCancel()
	if err != nil {
		// Not a genuine control plane, or a transient failure — either way
		// there is no one yet to report an error to. Drop silently, the same
		// posture as an HTTP listener refusing an unauthenticated request.
		return
	}

	var req AgentOpRequest
	if err := json.NewDecoder(tlsConn).Decode(&req); err != nil {
		return
	}

	enc := json.NewEncoder(tlsConn)

	if req.ProtocolVersion != AgentOpsProtocolVersion {
		_ = enc.Encode(AgentOpFrame{Type: "error", Reason: fmt.Sprintf("unsupported protocol version %d", req.ProtocolVersion)})
		return
	}

	if req.Op != AgentOpContainerLogs {
		_ = enc.Encode(AgentOpFrame{Type: "error", Reason: fmt.Sprintf("unsupported operation %q", req.Op)})
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
		_, _ = tlsConn.Read(discard[:])
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
