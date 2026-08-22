package libp2ptransport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/pki"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// acceptAnyPeer accepts every peer, for tests exercising one relationship in
// isolation. Peer-id-based rejection has its own tests below.
func acceptAnyPeer(allowContainerLogs bool) AgentOpsRelationshipLookup {
	return func(peer.ID) (bool, bool) { return allowContainerLogs, true }
}

// onlyPeers accepts exactly the listed peers — the shape a real Agent builds
// from its own configured control-plane Peer IDs (see agent.buildAgentOpsLookup).
func onlyPeers(allowContainerLogs bool, allowed ...peer.ID) AgentOpsRelationshipLookup {
	set := make(map[peer.ID]bool, len(allowed))
	for _, p := range allowed {
		set[p] = true
	}
	return func(remote peer.ID) (bool, bool) {
		if !set[remote] {
			return false, false
		}
		return allowContainerLogs, true
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
	serverHost, agentHost := setupAgentOpsHosts(t)

	want := []LogLine{{Message: "one"}, {Message: "two"}, {Message: "three"}}
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(want), NewSessionLimiter(4), acceptAnyPeer(true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
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
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(4), acceptAnyPeer(true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
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
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(4), acceptAnyPeer(true))

	ctx, cancel := context.WithCancel(context.Background())
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
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

// The core multi-relationship guarantee: a single shared Agent host talking
// to two control planes must decide each inbound AgentOps stream on the Peer
// ID that stream actually arrived from — the identity Noise proved — and
// never mix the two up regardless of connection or stream order.
func TestHandleAgentOpsStreamRoutesEachRelationshipByPeerID(t *testing.T) {
	serverA, serverB, agentHost := setupTwoControlPlaneHosts(t)

	var mu sync.Mutex
	var seen []peer.ID
	lookup := func(remote peer.ID) (bool, bool) {
		mu.Lock()
		seen = append(seen, remote)
		mu.Unlock()
		switch remote {
		case serverA.ID(), serverB.ID():
			return true, true
		default:
			return false, false
		}
	}
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc([]LogLine{{Message: "ok"}}), NewSessionLimiter(4), lookup)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	framesA, err := RequestContainerLogs(ctx, serverA, agentHost.ID(),
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs (A): %v", err)
	}
	if f, ok := <-framesA; !ok || f.Type != "line" || f.Message != "ok" {
		t.Fatalf("relationship A frame = %+v, ok=%v, want a genuine line", f, ok)
	}

	framesB, err := RequestContainerLogs(ctx, serverB, agentHost.ID(),
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		t.Fatalf("RequestContainerLogs (B): %v", err)
	}
	if f, ok := <-framesB; !ok || f.Type != "line" || f.Message != "ok" {
		t.Fatalf("relationship B frame = %+v, ok=%v, want a genuine line", f, ok)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != serverA.ID() || seen[1] != serverB.ID() {
		t.Fatalf("lookup saw %v, want exactly [serverA, serverB] — the peer id each stream really arrived from", seen)
	}
}

// A peer that is not one of this Agent's control planes gets no session, even
// though the libp2p connection itself is perfectly valid. Authentication (the
// Noise handshake) is never by itself authorization.
func TestHandleAgentOpsStreamRejectsUnregisteredPeer(t *testing.T) {
	serverA, serverB, agentHost := setupTwoControlPlaneHosts(t)

	called := false
	logs := func(context.Context, string, int, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
		called = true
		return nil, nil, nil
	}
	// Only serverA is one of the Agent's relationships; serverB is a stranger.
	RegisterAgentOpsHandler(agentHost, logs, NewSessionLimiter(4), onlyPeers(true, serverA.ID()))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverB, agentHost.ID(),
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
	if err != nil {
		return // stream dropped before the request was even written
	}
	if f, ok := <-frames; ok {
		t.Fatalf("expected no session for an unregistered peer, got frame %+v", f)
	}
	if called {
		t.Error("the Docker-facing logs func must never run for an unregistered peer")
	}
}

// --- Phase 2: AgentOps authorization is local, explicit, and independent of
// authentication -----------------------------------------------------------

// A control plane can authenticate perfectly (genuine cert, correct CA,
// correct node) and still be refused: allowContainerLogs is a separate,
// local gate. No Docker call must ever be attempted when it is false.
func TestHandleAgentOpsStreamRefusesUnauthorizedContainerLogs(t *testing.T) {
	serverHost, agentHost := setupAgentOpsHosts(t)

	called := false
	logs := func(context.Context, string, int, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
		called = true
		return nil, nil, nil
	}
	RegisterAgentOpsHandler(agentHost, logs, NewSessionLimiter(4), acceptAnyPeer(false))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
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
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), acceptAnyPeer(true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
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
	serverHost, agentHost := setupAgentOpsHosts(t)

	RegisterAgentOpsHandler(agentHost, erroringLogsFunc("docker is not available on this host"), NewSessionLimiter(4), acceptAnyPeer(true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "does-not-exist"})
	if err != nil {
		t.Fatalf("RequestContainerLogs: %v", err)
	}

	f, ok := <-frames
	if !ok || f.Type != "error" || f.Reason == "" {
		t.Fatalf("frame = %+v, ok=%v, want a populated error frame", f, ok)
	}
}

// --- authentication is the Noise handshake -------------------------------
//
// The reversed-mTLS handshake these tests used to exercise (untrusted CA,
// wrong node identity, impostor control-plane certificate) is gone: it proved
// a second, enrollment-derived identity on a channel whose identity libp2p
// had already proven, which is the enrollment dependency ADR-0012 removes.
// What replaced it is covered above — the Peer ID a stream arrived from is
// the identity, and TestHandleAgentOpsStreamRejectsUnregisteredPeer is the
// rejection case. go-libp2p itself guarantees the dial half: a stream is
// never returned on a connection whose remote peer is not the requested one.

func TestRequestContainerLogsRefusesUnreachablePeer(t *testing.T) {
	serverHost, agentHost := setupAgentOpsHosts(t)
	stranger, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("stranger host: %v", err)
	}
	t.Cleanup(func() { _ = stranger.Close() })

	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), acceptAnyPeer(true))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// stranger has never been dialed and has no listen address: there is no
	// connection to open a stream over, so no session can be established.
	if _, err := RequestContainerLogs(ctx, serverHost, stranger.ID(),
		AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"}); err == nil {
		t.Fatal("expected no agentops session to a peer this host has no connection to")
	}
}

// --- #15: large stream / backpressure, no full buffering -------------------

func TestRequestContainerLogsLargeStreamNoDeadlock(t *testing.T) {
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
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(4), acceptAnyPeer(true))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
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
	RegisterAgentOpsHandler(agentHost, logsFunc, NewSessionLimiter(1), acceptAnyPeer(true))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req := AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1", Follow: true}

	frames1, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), req)
	if err != nil {
		t.Fatalf("first session: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("first session never reached the agent-side logs func")
	}

	// A second concurrent session must be turned away — the limiter is 1.
	frames2, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), req)
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

	frames3, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), req)
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
