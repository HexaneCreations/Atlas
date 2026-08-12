package libp2ptransport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/pki"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// staticLookup adapts a single caCert/getCert/allowContainerLogs — the
// pre-Phase-3 single-relationship shape — into an AgentOpsRelationshipLookup
// that accepts any peer, for tests exercising one relationship in isolation.
func staticLookup(caCert *x509.Certificate, getCert func(*tls.ClientHelloInfo) (*tls.Certificate, error), allowContainerLogs bool) AgentOpsRelationshipLookup {
	return func(peer.ID) (*x509.Certificate, func(*tls.ClientHelloInfo) (*tls.Certificate, error), bool, bool) {
		return caCert, getCert, allowContainerLogs, true
	}
}

// --- test fixtures -----------------------------------------------------

func testCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.New("test-fleet-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	return ca
}

func testControlPlaneLeaf(t *testing.T, ca *pki.CA) tls.Certificate {
	t.Helper()
	leaf, err := pki.NewServerLeaf(ca, nil)
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	return leaf
}

func testAgentLeaf(t *testing.T, ca *pki.CA, nodeID string) tls.Certificate {
	t.Helper()
	csrDER, key, err := pki.NewCSR(nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	csr, err := pki.ParseCSR(csrDER)
	if err != nil {
		t.Fatalf("ParseCSR: %v", err)
	}
	leaf, err := ca.IssueLeaf(csr, nodeID)
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	return *pki.LeafTLSCertificate(leaf, key)
}

func staticCert(cert tls.Certificate) func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }
}

// setupAgentOpsHosts builds two real hosts in the real topology: agentHost
// is dial-only (like a real Agent) and connects out to serverHost once — the
// connection AgentOps later reuses in the reverse direction, proving a
// dial-only host still accepts a Server-initiated stream over a connection
// it established itself.
func setupAgentOpsHosts(t *testing.T) (serverHost, agentHost host.Host) {
	t.Helper()

	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err = NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	target, err := ParseTarget(Addrs(serverHost)[0])
	if err != nil {
		t.Fatalf("parse server target: %v", err)
	}
	if err := agentHost.Connect(context.Background(), target); err != nil {
		t.Fatalf("agent dial server: %v", err)
	}
	return serverHost, agentHost
}

func fixedLinesFunc(lines []LogLine) ContainerLogsFunc {
	return func(context.Context, string, int, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
		out := make(chan LogLine, len(lines))
		for _, l := range lines {
			out <- l
		}
		close(out)
		errCh := make(chan error, 1)
		close(errCh)
		return out, errCh, nil
	}
}

func erroringLogsFunc(msg string) ContainerLogsFunc {
	return func(context.Context, string, int, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
		return nil, nil, fmt.Errorf("%s", msg)
	}
}

// --- #1 / #2: request/frame JSON round trip -----------------------------

func TestAgentOpRequestJSONRoundTrip(t *testing.T) {
	want := AgentOpRequest{
		ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs,
		ContainerID: "c1", Tail: 50, Since: "2026-01-01T00:00:00Z", Follow: true, Timestamps: true,
	}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AgentOpRequest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestAgentOpFrameJSONRoundTrip(t *testing.T) {
	want := AgentOpFrame{Type: "line", Time: time.Now().UTC().Truncate(time.Second), Stream: "stdout", Message: "hello"}
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got AgentOpFrame
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Time.Equal(want.Time) || got.Type != want.Type || got.Stream != want.Stream || got.Message != want.Message {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

// --- #11 / #12: non-follow and follow log delivery ----------------------

func TestRequestContainerLogsNonFollow(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	want := []LogLine{{Message: "one"}, {Message: "two"}, {Message: "three"}}
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(want), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}

	var got []string
	for f := range frames {
		if f.Type == "error" {
			t.Fatalf("unexpected error frame: %s", f.Reason)
		}
		if f.Type == "line" {
			got = append(got, f.Message)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("got %v lines, want %d", got, len(want))
	}
	for i, l := range want {
		if got[i] != l.Message {
			t.Errorf("line %d = %q, want %q", i, got[i], l.Message)
		}
	}
}

func TestRequestContainerLogsFollow(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	produced := make(chan LogLine)
	logsFunc := func(ctx context.Context, _ string, _ int, _ time.Time, follow, _ bool) (<-chan LogLine, <-chan error, error) {
		if !follow {
			t.Errorf("expected Follow=true to reach the agent-side func")
		}
		errCh := make(chan error, 1)
		out := make(chan LogLine)
		go func() {
			defer close(out)
			for {
				select {
				case l, ok := <-produced:
					if !ok {
						return
					}
					out <- l
				case <-ctx.Done():
					return
				}
			}
		}()
		return out, errCh, nil
	}
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1", Follow: true})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}

	produced <- LogLine{Message: "live-1"}
	select {
	case f := <-frames:
		if f.Message != "live-1" {
			t.Fatalf("message = %q, want %q", f.Message, "live-1")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a live line")
	}

	cancel()
	// The frames channel must close once ctx is cancelled — proves
	// cancellation reaches the stream, not just that the test gave up
	// reading it.
	select {
	case _, open := <-frames:
		if open {
			// draining until closed is fine; a stray trailing frame is not a
			// failure by itself.
			for range frames {
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frames channel never closed after cancellation")
	}
}

// --- #14: cancellation propagates to the agent-side (Docker-facing) ctx --

func TestRequestContainerLogsCancellationReachesAgentContext(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	started := make(chan struct{})
	agentCtxDone := make(chan struct{})
	logsFunc := func(ctx context.Context, _ string, _ int, _ time.Time, _, _ bool) (<-chan LogLine, <-chan error, error) {
		out := make(chan LogLine)
		errCh := make(chan error, 1)
		close(started)
		go func() {
			<-ctx.Done()
			close(agentCtxDone)
		}()
		return out, errCh, nil
	}
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithCancel(context.Background())
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1", Follow: true})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}
	_ = frames

	// Wait for the session to actually be established on the Agent side
	// before cancelling — otherwise cancellation can race the request
	// itself across the wire, which is a real but different scenario (the
	// request never arrives at all) from the one under test here
	// (cancelling an active session).
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("agent-side session never started")
	}

	cancel() // Browser -> Server ctx cancellation, simulated at the source

	select {
	case <-agentCtxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation on the Server side never reached the Agent's docker-facing context")
	}
}

// --- Phase 3: multi-relationship demux — one Agent must never authenticate
// one relationship's control plane using another relationship's CA ---------

// setupTwoControlPlaneHosts is setupAgentOpsHosts extended to a second,
// independent control plane host — the shape a multi-relationship Agent
// actually has: one shared, dial-only agentHost with live connections to two
// distinct remote peers.
func setupTwoControlPlaneHosts(t *testing.T) (serverA, serverB, agentHost host.Host) {
	t.Helper()

	serverA, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server A host: %v", err)
	}
	t.Cleanup(func() { _ = serverA.Close() })

	serverB, err = NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server B host: %v", err)
	}
	t.Cleanup(func() { _ = serverB.Close() })

	agentHost, err = NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	for _, srv := range []host.Host{serverA, serverB} {
		target, err := ParseTarget(Addrs(srv)[0])
		if err != nil {
			t.Fatalf("parse server target: %v", err)
		}
		if err := agentHost.Connect(context.Background(), target); err != nil {
			t.Fatalf("agent dial server: %v", err)
		}
	}
	return serverA, serverB, agentHost
}

// The core Phase 3 cross-package guarantee: a single shared Agent host,
// talking to two relationships with two entirely independent CAs, must route
// each inbound AgentOps stream to that relationship's own trust context —
// never mixing them up regardless of which order they connect or which
// order streams arrive in.
func TestHandleAgentOpsStreamRoutesEachRelationshipToItsOwnCA(t *testing.T) {
	caA, caB := testCA(t), testCA(t)
	agentLeafA := testAgentLeaf(t, caA, "node-a")
	agentLeafB := testAgentLeaf(t, caB, "node-a")
	controlPlaneLeafA := testControlPlaneLeaf(t, caA)
	controlPlaneLeafB := testControlPlaneLeaf(t, caB)
	serverA, serverB, agentHost := setupTwoControlPlaneHosts(t)

	lookup := func(remotePeer peer.ID) (*x509.Certificate, func(*tls.ClientHelloInfo) (*tls.Certificate, error), bool, bool) {
		switch remotePeer {
		case serverA.ID():
			return caA.Cert(), staticCert(agentLeafA), true, true
		case serverB.ID():
			return caB.Cert(), staticCert(agentLeafB), true, true
		default:
			return nil, nil, false, false
		}
	}
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc([]LogLine{{Message: "ok"}}), NewSessionLimiter(4), lookup)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	framesA, err := RequestContainerLogs(ctx, serverA, agentHost.ID(), "node-a", caA.Cert(), controlPlaneLeafA,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs (A): %v", err)
	}
	if f, ok := <-framesA; !ok || f.Type != "line" || f.Message != "ok" {
		t.Fatalf("relationship A frame = %+v, ok=%v, want a genuine line", f, ok)
	}

	framesB, err := RequestContainerLogs(ctx, serverB, agentHost.ID(), "node-a", caB.Cert(), controlPlaneLeafB,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs (B): %v", err)
	}
	if f, ok := <-framesB; !ok || f.Type != "line" || f.Message != "ok" {
		t.Fatalf("relationship B frame = %+v, ok=%v, want a genuine line", f, ok)
	}
}

// Even though serverA's peer id is registered (it is relationship A's
// control plane), presenting a certificate signed by relationship B's CA
// must still be rejected — the peer-id-based routing selects *which* CA to
// check against, it is never a substitute for the chain check itself.
func TestHandleAgentOpsStreamRejectsPeerPresentingWrongRelationshipsCert(t *testing.T) {
	caA, caB := testCA(t), testCA(t)
	agentLeafA := testAgentLeaf(t, caA, "node-a")
	wrongLeaf := testControlPlaneLeaf(t, caB) // valid CP leaf, but from B's CA
	serverA, _, agentHost := setupTwoControlPlaneHosts(t)

	lookup := func(remotePeer peer.ID) (*x509.Certificate, func(*tls.ClientHelloInfo) (*tls.Certificate, error), bool, bool) {
		if remotePeer == serverA.ID() {
			return caA.Cert(), staticCert(agentLeafA), true, true
		}
		return nil, nil, false, false
	}
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), lookup)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverA, agentHost.ID(), "node-a", caA.Cert(), wrongLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	// Same TLS 1.3 handshake-asymmetry posture as the other CA-mismatch tests
	// in this file.
	if err != nil {
		return
	}
	if f, ok := <-frames; ok {
		t.Fatalf("expected no session: relationship A's registered peer presented a certificate from relationship B's CA, got frame %+v", f)
	}
}

// --- Phase 2: AgentOps authorization is local, explicit, and independent of
// authentication -----------------------------------------------------------

// A control plane can authenticate perfectly (genuine cert, correct CA,
// correct node) and still be refused: allowContainerLogs is a separate,
// local gate. No Docker call must ever be attempted when it is false.
func TestHandleAgentOpsStreamRefusesUnauthorizedContainerLogs(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	called := false
	logs := func(context.Context, string, int, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
		called = true
		return nil, nil, nil
	}
	RegisterAgentOpsHandler(agentHost, logs, NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), false))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}

	f, ok := <-frames
	if !ok || f.Type != "error" {
		t.Fatalf("frame = %+v, ok=%v, want an error frame", f, ok)
	}
	if called {
		t.Error("the Docker-facing logs func must not run when the operation is not locally authorized")
	}
}

// --- #3: unsupported operation --------------------------------------------

func TestRegisterAgentOpsHandlerRejectsUnknownOp(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: "container_restart", ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}

	f, ok := <-frames
	if !ok {
		t.Fatal("expected an error frame, got a closed channel with nothing")
	}
	if f.Type != "error" {
		t.Fatalf("frame type = %q, want %q", f.Type, "error")
	}
}

// --- #7 / #8: agent-side error surfaced as an error frame -----------------

func TestRequestContainerLogsSurfacesAgentSideError(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, erroringLogsFunc("docker is not available on this host"), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "does-not-exist"})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}

	f, ok := <-frames
	if !ok || f.Type != "error" || f.Reason == "" {
		t.Fatalf("frame = %+v, ok=%v, want a populated error frame", f, ok)
	}
}

// --- #4: authentication failure (untrusted CA) -----------------------------

func TestRequestContainerLogsRejectsAgentFromUntrustedCA(t *testing.T) {
	ca := testCA(t)
	otherCA := testCA(t) // a different fleet's CA — agentLeaf below is not signed by ca
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, otherCA, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err == nil {
		t.Fatal("expected the handshake to fail against a certificate from an untrusted CA")
	}
}

// --- #5: authorization failure (wrong node identity) -----------------------

func TestRequestContainerLogsRejectsWrongNodeIdentity(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	// agentHost's certificate genuinely identifies "node-a", signed by the
	// real fleet CA — but the control plane believes it is calling "node-b"
	// (e.g. a stale registry entry). This must not be accepted.
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-b", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err == nil {
		t.Fatal("expected rejection when the presented certificate names a different node than expected")
	}
}

// verifyControlPlaneCertificate's own authorization check: an Agent must not
// accept a stream from a peer presenting another Agent's certificate, even
// though it is signed by the same trusted fleet CA. The Agent's
// VerifyPeerCertificate rejection aborts the TLS handshake, which the
// control-plane side observes as its own handshake failing.
func TestHandleAgentOpsStreamRejectsNonControlPlaneCertificate(t *testing.T) {
	ca := testCA(t)
	// The "control plane" side presents another Agent's leaf certificate
	// instead of the real control plane's — same CA, wrong identity.
	impostorLeaf := testAgentLeaf(t, ca, "some-other-node")
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), impostorLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	// TLS 1.3 mutual auth is asymmetric here: the control-plane side's own
	// Handshake call can return success before it has learned the Agent
	// rejected its certificate — that only surfaces on the next read. So the
	// real assertion is that no genuine session ever gets established: no
	// frame is ever delivered, either because the setup itself errors or
	// because the frames channel closes immediately empty.
	if err != nil {
		return
	}
	if f, ok := <-frames; ok {
		t.Fatalf("expected no session to be established for an impostor certificate, got frame %+v", f)
	}
}

// --- Phase 1: control-plane identity must be scoped to the specific CA the
// Agent pins for this relationship, not "any control plane certificate the
// Agent happens to trust" -----------------------------------------------

// A certificate that is a genuine, correctly-shaped control-plane leaf (real
// ControlPlaneURI SAN, ServerAuth EKU) but signed by a *different* CA than
// the one the Agent pinned must still be rejected. Before Phase 1 this check
// only asked "does CommonName say atlas-control-plane", which a certificate
// from any CA could satisfy identically; the chain-to-CA check existed
// separately but the identity check itself carried no CA-specific signal.
func TestHandleAgentOpsStreamRejectsControlPlaneFromWrongCA(t *testing.T) {
	ca := testCA(t)      // the CA the Agent pins for this connection
	otherCA := testCA(t) // a different control plane's CA
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	wrongControlPlaneLeaf := testControlPlaneLeaf(t, otherCA) // valid CP leaf, wrong CA
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), wrongControlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	// Same TLS 1.3 asymmetry as TestHandleAgentOpsStreamRejectsNonControlPlaneCertificate:
	// the control-plane side's own Handshake call can return success before it
	// has learned the Agent rejected its certificate. The real assertion is
	// that no genuine session is ever established.
	if err != nil {
		return
	}
	if f, ok := <-frames; ok {
		t.Fatalf("expected no session for a control-plane certificate signed by a CA the Agent does not pin, got frame %+v", f)
	}
}

// The expected control-plane identity must be derived at verification time
// from the pinned CA itself, and must be stable across independent
// verification calls against the same CA — this is the property multi-
// relationship support (a future phase) depends on.
func TestHandleAgentOpsStreamAcceptsControlPlaneFromPinnedCA(t *testing.T) {
	ca := testCA(t)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc([]LogLine{{Message: "ok"}}), NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}
	f, ok := <-frames
	if !ok || f.Type != "line" || f.Message != "ok" {
		t.Fatalf("frame = %+v, ok=%v, want a genuine line from a control plane presenting a leaf from the pinned CA", f, ok)
	}
}

// --- #15: large stream / backpressure, no full buffering -------------------

func TestRequestContainerLogsLargeStreamNoDeadlock(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	const total = 2000
	logsFunc := func(ctx context.Context, _ string, _ int, _ time.Time, _, _ bool) (<-chan LogLine, <-chan error, error) {
		out := make(chan LogLine) // unbuffered: forces real backpressure
		errCh := make(chan error, 1)
		go func() {
			defer close(out)
			defer close(errCh)
			for i := 0; i < total; i++ {
				select {
				case out <- LogLine{Message: fmt.Sprintf("line-%d", i)}:
				case <-ctx.Done():
					return
				}
			}
		}()
		return out, errCh, nil
	}
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(4), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf,
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}

	n := 0
	for f := range frames {
		if f.Type == "error" {
			t.Fatalf("unexpected error frame: %s", f.Reason)
		}
		if f.Type != "line" {
			continue
		}
		want := fmt.Sprintf("line-%d", n)
		if f.Message != want {
			t.Fatalf("line %d = %q, want %q (out of order or dropped)", n, f.Message, want)
		}
		n++
	}
	if n != total {
		t.Fatalf("received %d lines, want %d", n, total)
	}
}

// --- #17 / #18: concurrency limit, and release on completion --------------

func TestSessionLimiterBoundsConcurrencyAndReleases(t *testing.T) {
	ca := testCA(t)
	controlPlaneLeaf := testControlPlaneLeaf(t, ca)
	agentLeaf := testAgentLeaf(t, ca, "node-a")
	serverHost, agentHost := setupAgentOpsHosts(t)

	release := make(chan struct{})
	started := make(chan struct{}, 2)
	logsFunc := func(ctx context.Context, _ string, _ int, _ time.Time, _, _ bool) (<-chan LogLine, <-chan error, error) {
		started <- struct{}{}
		out := make(chan LogLine)
		errCh := make(chan error, 1)
		go func() {
			select {
			case <-release:
			case <-ctx.Done():
			}
			close(out)
			close(errCh)
		}()
		return out, errCh, nil
	}
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(1), staticLookup(ca.Cert(), staticCert(agentLeaf), true))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1", Follow: true}

	frames1, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf, req)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first session never reached the agent-side logs func")
	}

	// A second concurrent session must be turned away — the limiter is 1.
	frames2, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf, req)
	if err != nil {
		t.Fatalf("second session (stream open): %v", err)
	}
	f, ok := <-frames2
	if !ok || f.Type != "error" {
		t.Fatalf("second session frame = %+v, ok=%v, want a rejection while the first is still active", f, ok)
	}

	// Release the first session and prove the limiter slot comes back: a
	// third attempt must now succeed.
	close(release)
	for range frames1 {
	}

	frames3, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), "node-a", ca.Cert(), controlPlaneLeaf, req)
	if err != nil {
		t.Fatalf("third session: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("limiter slot was not released after the first session completed")
	}
	_ = frames3
}
