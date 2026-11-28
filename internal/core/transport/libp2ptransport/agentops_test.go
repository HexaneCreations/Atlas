package libp2ptransport

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/pki"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
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
	return func(context.Context, string, int, time.Time, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
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
	return func(context.Context, string, int, time.Time, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
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
	var last AgentOpFrame
	for f := range frames {
		if f.Type == "error" {
			t.Fatalf("unexpected error frame: %s", f.Reason)
		}
		if f.Type == "line" {
			got = append(got, f.Message)
		}
		last = f
	}
	if last.Type != "end" {
		t.Fatalf("last frame = %+v, want the terminal %q frame", last, "end")
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
	logsFunc := func(ctx context.Context, _ string, _ int, _, _ time.Time, follow, _ bool) (<-chan LogLine, <-chan error, error) {
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
	logsFunc := func(ctx context.Context, _ string, _ int, _, _ time.Time, _, _ bool) (<-chan LogLine, <-chan error, error) {
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
	logs := func(context.Context, string, int, time.Time, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
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
	logs := func(context.Context, string, int, time.Time, time.Time, bool, bool) (<-chan LogLine, <-chan error, error) {
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

// The terminal frame is the one that explains a session's outcome, so it must
// arrive on every request, not most: the handler's own return used to reset
// the stream, racing the graceful close and discarding the frame unread.
func TestRequestContainerLogsAlwaysDeliversTheTerminalFrame(t *testing.T) {
	const runs = 200

	cases := []struct {
		name       string
		logs       ContainerLogsFunc
		wantType   string
		wantReason string
	}{
		{"error", erroringLogsFunc("docker is not available on this host"), "error", "docker is not available on this host"},
		{"end", fixedLinesFunc([]LogLine{{Message: "one"}}), "end", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			serverHost, agentHost := setupAgentOpsHosts(t)
			// Sized past runs so a session still releasing its slot can never
			// turn the next request into a limiter refusal.
			RegisterAgentOpsHandler(agentHost, tc.logs, NewSessionLimiter(runs), acceptAnyPeer(true))

			for i := range runs {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
					AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"})
				if err != nil {
					cancel()
					t.Fatalf("run %d: RequestContainerLogs: %v", i, err)
				}
				var last AgentOpFrame
				var got bool
				for f := range frames {
					last, got = f, true
				}
				cancel()
				if !got || last.Type != tc.wantType || last.Reason != tc.wantReason {
					t.Fatalf("run %d: last frame = %+v (received any: %v), want type %q reason %q",
						i, last, got, tc.wantType, tc.wantReason)
				}
			}
		})
	}
}

// The handler's close-detection Read must not outlive a normal return. Here
// the control plane reads its terminal frame and then never closes or resets
// its side, so the only thing that can end that Read is the handler's own
// s.Close().
func TestHandleAgentOpsStreamReadGoroutineExitsWhenClientNeverCloses(t *testing.T) {
	const sessions = 50

	serverHost, agentHost := setupAgentOpsHosts(t)
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc([]LogLine{{Message: "one"}}), NewSessionLimiter(sessions+1), acceptAnyPeer(true))

	var held []network.Stream
	t.Cleanup(func() {
		for _, s := range held {
			_ = s.Reset()
		}
	})
	runSession := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := serverHost.NewStream(ctx, agentHost.ID(), AgentOpsProtocolID)
		if err != nil {
			t.Fatalf("NewStream: %v", err)
		}
		held = append(held, s)
		if err := json.NewEncoder(s).Encode(AgentOpRequest{ProtocolVersion: AgentOpsProtocolVersion, Op: AgentOpContainerLogs, ContainerID: "c1"}); err != nil {
			t.Fatalf("send request: %v", err)
		}
		dec := json.NewDecoder(s)
		for {
			var f AgentOpFrame
			if err := dec.Decode(&f); err != nil {
				t.Fatalf("decode before terminal frame: %v", err)
			}
			if f.Type != "line" {
				return
			}
		}
	}

	// settledCount polls until the count is at most atMost or has held
	// steady for several samples, whichever comes first.
	settledCount := func(atMost int) int {
		n, steady := runtime.NumGoroutine(), 0
		for deadline := time.Now().Add(3 * time.Second); n > atMost && steady < 5 && time.Now().Before(deadline); {
			time.Sleep(20 * time.Millisecond)
			next := runtime.NumGoroutine()
			if next == n {
				steady++
			} else {
				steady = 0
			}
			n = next
		}
		return n
	}

	// One warm-up session starts whatever per-connection goroutines the
	// stack creates lazily, so the baseline counts only steady state.
	runSession()
	baseline := settledCount(0)

	for range sessions {
		runSession()
	}
	// A leaked Read goroutine per session would put the count at least
	// `sessions` above baseline; the slack absorbs unrelated churn.
	const slack = 5
	if n := settledCount(baseline + slack); n > baseline+slack {
		t.Fatalf("goroutines = %d after %d sessions, baseline %d: handler goroutines did not exit", n, sessions, baseline)
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
	logsFunc := func(ctx context.Context, _ string, _ int, _, _ time.Time, _, _ bool) (<-chan LogLine, <-chan error, error) {
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
	logsFunc := func(ctx context.Context, _ string, _ int, _, _ time.Time, _, _ bool) (<-chan LogLine, <-chan error, error) {
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

// --- protocol version 2: until, negotiated per request ---------------------

type recordedCall struct {
	since, until time.Time
	tail         int
}

func recordingLogsFunc(calls chan<- recordedCall) ContainerLogsFunc {
	return func(_ context.Context, _ string, tail int, since, until time.Time, _, _ bool) (<-chan LogLine, <-chan error, error) {
		calls <- recordedCall{since: since, until: until, tail: tail}
		out := make(chan LogLine)
		close(out)
		errCh := make(chan error, 1)
		close(errCh)
		return out, errCh, nil
	}
}

func lastFrame(t *testing.T, frames <-chan AgentOpFrame) AgentOpFrame {
	t.Helper()
	var last AgentOpFrame
	for f := range frames {
		last = f
	}
	return last
}

// A version 2 Agent serves version 1 requests (a current production control
// plane) and passes Until through only on version 2.
func TestAgentServesVersionOneAndTwoRequests(t *testing.T) {
	serverHost, agentHost := setupAgentOpsHosts(t)
	calls := make(chan recordedCall, 4)
	RegisterAgentOpsHandler(agentHost, recordingLogsFunc(calls), NewSessionLimiter(4), acceptAnyPeer(true))
	since := time.Date(2026, 1, 1, 10, 0, 0, 123_000_000, time.UTC)
	until := since.Add(time.Hour)

	for _, c := range []struct {
		version   int
		wantUntil time.Time
	}{
		{1, time.Time{}}, // a version 1 request never carries until; an extra field is ignored
		{2, until},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(), AgentOpRequest{ProtocolVersion: c.version,
			Op: AgentOpContainerLogs, ContainerID: "c1", Since: since.Format(time.RFC3339Nano), Until: until.Format(time.RFC3339Nano)})
		if err != nil {
			t.Fatal(err)
		}
		if f := lastFrame(t, frames); f.Type != "end" {
			t.Fatalf("version %d: last frame %+v", c.version, f)
		}
		cancel()
		got := <-calls
		if !got.since.Equal(since) || !got.until.Equal(c.wantUntil) {
			t.Errorf("version %d: since %v until %v, want %v %v", c.version, got.since, got.until, since, c.wantUntil)
		}
	}
}

// An unknown version gets the error frame version 1 Agents already send, the
// signal a control plane falls back on.
func TestAgentRefusesAnUnknownVersionWithTheVersionOneFrame(t *testing.T) {
	serverHost, agentHost := setupAgentOpsHosts(t)
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), acceptAnyPeer(true))
	for _, v := range []int{0, AgentOpsProtocolVersion + 1} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
			AgentOpRequest{ProtocolVersion: v, Op: AgentOpContainerLogs, ContainerID: "c1"})
		if err != nil {
			t.Fatal(err)
		}
		f := lastFrame(t, frames)
		cancel()
		if f.Type != "error" || !IsUnsupportedVersion(f.Reason) || f.Reason != fmt.Sprintf("unsupported protocol version %d", v) {
			t.Errorf("version %d: frame %+v", v, f)
		}
	}
	if IsUnsupportedVersion("docker is not available on this host") {
		t.Error("an unrelated error read as a version refusal")
	}
}

func TestAgentRejectsAMalformedUntil(t *testing.T) {
	serverHost, agentHost := setupAgentOpsHosts(t)
	RegisterAgentOpsHandler(agentHost, fixedLinesFunc(nil), NewSessionLimiter(4), acceptAnyPeer(true))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	frames, err := RequestContainerLogs(ctx, serverHost, agentHost.ID(),
		AgentOpRequest{ProtocolVersion: 2, Op: AgentOpContainerLogs, ContainerID: "c1", Until: "yesterday"})
	if err != nil {
		t.Fatal(err)
	}
	if f := lastFrame(t, frames); f.Type != "error" {
		t.Errorf("frame = %+v, want an error", f)
	}
}
