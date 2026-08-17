package libp2ptransport

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
)

// recordingHandler captures slog records so tests can assert on the exact
// observability lines NewHost's connection-path/hole-punch logging produces,
// without depending on any particular text/JSON formatting.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *recordingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordingHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(a.Key)
		b.WriteString("=")
		b.WriteString(a.Value.String())
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, b.String())
	return nil
}
func (h *recordingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordingHandler) WithGroup(string) slog.Handler      { return h }

func (h *recordingHandler) contains(substr string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, substr) {
			return true
		}
	}
	return false
}

func (h *recordingHandler) waitFor(t *testing.T, substr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if h.contains(substr) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	t.Fatalf("log never contained %q; captured: %v", substr, h.msgs)
}

// TestHolePunchTracerLogsEachEventType proves the tracer maps every DCUtR
// event go-libp2p's holepunch package can emit — hole punch attempted,
// succeeded, and failed — to a distinct, greppable log line. No network
// involved: these are the library's own event types, fed in directly, since
// forcing a real hole-punch failure requires an actual asymmetric NAT that a
// loopback test environment cannot simulate (see requirement 9's NAT
// limitations, documented in DIRECT_P2P_NAT_TRAVERSAL_IMPLEMENTATION.md).
func TestHolePunchTracerLogsEachEventType(t *testing.T) {
	h := &recordingHandler{}
	tracer := newHolePunchTracer(slog.New(h))
	remote := peer.ID("test-remote-peer")

	tracer.Trace(&holepunch.Event{Remote: remote, Type: holepunch.StartHolePunchEvtT, Evt: &holepunch.StartHolePunchEvt{}})
	if !h.contains("hole punch attempted") {
		t.Fatal("expected a hole punch attempted log line")
	}

	tracer.Trace(&holepunch.Event{Remote: remote, Type: holepunch.EndHolePunchEvtT, Evt: &holepunch.EndHolePunchEvt{Success: true, EllapsedTime: time.Second}})
	if !h.contains("hole punch succeeded") {
		t.Fatal("expected a hole punch succeeded log line")
	}

	tracer.Trace(&holepunch.Event{Remote: remote, Type: holepunch.EndHolePunchEvtT, Evt: &holepunch.EndHolePunchEvt{Success: false, Error: "simulated symmetric NAT"}})
	if !h.contains("hole punch failed") {
		t.Fatal("expected a hole punch failed log line")
	}

	tracer.Trace(&holepunch.Event{Remote: remote, Type: holepunch.ProtocolErrorEvtT, Evt: &holepunch.ProtocolErrorEvt{Error: "bad message"}})
	if !h.contains("hole punch protocol error") {
		t.Fatal("expected a hole punch protocol error log line")
	}
}

// TestConnPathNotifieeLogsDirectConnectionAndPeerID proves a direct dial
// between two real loopback hosts is logged as "direct connection
// established" and is attributed to the correct Peer ID — not any peer, a
// specific one, which is what requirement 10's "Peer ID verification" test
// asks for.
func TestConnPathNotifieeLogsDirectConnectionAndPeerID(t *testing.T) {
	h := &recordingHandler{}
	logger := slog.New(h)

	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}, Logger: logger})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err := NewHost(HostOptions{DataDir: t.TempDir(), Logger: logger})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	listener, err := Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	target, err := ParseTarget(Addrs(serverHost)[0])
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, agentHost, target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	h.waitFor(t, "direct connection established", 5*time.Second)
	h.waitFor(t, serverHost.ID().String(), 5*time.Second)
	if h.contains("relay fallback used") {
		t.Fatal("a direct connection must never be logged as relay fallback")
	}
}

// TestConnPathNotifieeLogsRelayFallback proves a circuit-relay connection —
// the only path available to a NAT'd host with no direct listen address — is
// logged as relay fallback, never misreported as direct. Three real hosts, a
// real circuitv2 relay hop, loopback network only.
func TestConnPathNotifieeLogsRelayFallback(t *testing.T) {
	h := &recordingHandler{}
	logger := slog.New(h)

	relayHost, err := NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("relay host: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })

	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir(), Logger: logger}) // no listen addrs: simulates NAT
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err := NewHost(HostOptions{DataDir: t.TempDir(), Logger: logger})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	listener, err := Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	relayInfo, err := ParseTarget(Addrs(relayHost)[0])
	if err != nil {
		t.Fatalf("parse relay target: %v", err)
	}
	circuitAddr, _, err := ReserveRelay(ctx, serverHost, relayInfo)
	if err != nil {
		t.Fatalf("reserve relay: %v", err)
	}
	circuit, err := ParseTarget(circuitAddr)
	if err != nil {
		t.Fatalf("parse circuit target: %v", err)
	}

	conn, err := Dial(ctx, agentHost, circuit)
	if err != nil {
		t.Fatalf("dial through relay: %v", err)
	}
	defer conn.Close()

	h.waitFor(t, "relay fallback used", 5*time.Second)
}

// TestHolePunchFailureLeavesRelayPathUsable is requirement 10's "hole punch
// failure -> relay fallback" case. A real asymmetric NAT cannot be
// constructed in a loopback test environment (both hosts are always mutually
// reachable, so a real DCUtR attempt here would succeed, not fail — that
// path is exercised manually against real NAT, per the implementation
// report). What is deterministically provable here: (a) a failed hole punch
// is logged distinctly (via the tracer, event-level, no network needed), and
// (b) the pre-existing relay connection a failed hole punch leaves behind
// keeps carrying application traffic — the actual fallback guarantee.
func TestHolePunchFailureLeavesRelayPathUsable(t *testing.T) {
	h := &recordingHandler{}
	tracer := newHolePunchTracer(slog.New(h))
	remote := peer.ID("nat-blocked-peer")
	tracer.Trace(&holepunch.Event{Remote: remote, Type: holepunch.EndHolePunchEvtT,
		Evt: &holepunch.EndHolePunchEvt{Success: false, Error: "simulated symmetric NAT: no matching candidate"}})
	if !h.contains("hole punch failed") {
		t.Fatal("expected the failed hole punch to be logged")
	}

	relayHost, err := NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("relay host: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })

	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	listener, err := Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	const message = "relay still carries traffic after a failed hole punch"
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, len(message))
		if _, err := io.ReadFull(conn, buf); err == nil && string(buf) == message {
			close(accepted)
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	relayInfo, err := ParseTarget(Addrs(relayHost)[0])
	if err != nil {
		t.Fatalf("parse relay target: %v", err)
	}
	circuitAddr, _, err := ReserveRelay(ctx, serverHost, relayInfo)
	if err != nil {
		t.Fatalf("reserve relay: %v", err)
	}
	circuit, err := ParseTarget(circuitAddr)
	if err != nil {
		t.Fatalf("parse circuit target: %v", err)
	}

	conn, err := Dial(ctx, agentHost, circuit)
	if err != nil {
		t.Fatalf("dial through relay: %v", err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(message)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("relay path did not carry traffic")
	}
}

// TestPreferDirectConnectionFiresOnlyForMatchingPeerID proves onDirect fires
// for a real direct connection to the exact target Peer ID given, and not
// for a connection to a different peer — requirement 10's "Peer ID
// verification" case applied to the actual production hook agent.go wires in.
func TestPreferDirectConnectionFiresOnlyForMatchingPeerID(t *testing.T) {
	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	otherHost, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("other host: %v", err)
	}
	t.Cleanup(func() { _ = otherHost.Close() })

	acceptAll := func(h host.Host) {
		l, err := Listen(h)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		t.Cleanup(func() { _ = l.Close() })
		go func() {
			for {
				c, err := l.Accept()
				if err != nil {
					return
				}
				_ = c
			}
		}()
	}
	acceptAll(serverHost)
	acceptAll(otherHost)

	fired := make(chan peer.ID, 4)
	PreferDirectConnection(agentHost, serverHost.ID(), func() { fired <- serverHost.ID() })

	target, err := ParseTarget(Addrs(serverHost)[0])
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, agentHost, target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case got := <-fired:
		if got != serverHost.ID() {
			t.Fatalf("onDirect fired for %s, want %s", got, serverHost.ID())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onDirect never fired for a direct connection to the watched peer")
	}

	otherTarget, err := ParseTarget(Addrs(otherHost)[0])
	if err != nil {
		t.Fatalf("parse other target: %v", err)
	}
	otherConn, err := Dial(ctx, agentHost, otherTarget)
	if err != nil {
		t.Fatalf("dial other: %v", err)
	}
	defer otherConn.Close()

	select {
	case <-fired:
		t.Fatal("onDirect fired for a connection to an unwatched peer")
	case <-time.After(300 * time.Millisecond):
	}
}

// TestConnPathNotifieeLogsReconnectAfterDirectLoss proves the "direct
// connection lost" / "direct connection re-established" pair — an agent
// reconnecting after its direct connection drops (e.g. a NAT mapping
// expiring) is observable, not silent.
func TestConnPathNotifieeLogsReconnectAfterDirectLoss(t *testing.T) {
	h := &recordingHandler{}
	logger := slog.New(h)

	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}, Logger: logger})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err := NewHost(HostOptions{DataDir: t.TempDir(), Logger: logger})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	listener, err := Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_ = c
		}
	}()

	target, err := ParseTarget(Addrs(serverHost)[0])
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	conn, err := Dial(ctx, agentHost, target)
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	h.waitFor(t, "direct connection established", 5*time.Second)

	conn.Close()
	if err := agentHost.Network().ClosePeer(serverHost.ID()); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	h.waitFor(t, "direct connection lost", 5*time.Second)

	conn2, err := Dial(ctx, agentHost, target)
	if err != nil {
		t.Fatalf("second dial: %v", err)
	}
	defer conn2.Close()
	h.waitFor(t, "direct connection re-established", 5*time.Second)
}

// TestEnrollmentAndTelemetryStyleHTTPOverDirectConnection proves ordinary
// application traffic — an HTTP request/response, standing in for Atlas's
// real enroll/renew/telemetry calls, all of which are plain net/http over
// this exact Dial/Listen pair (see remote.Transport and credentials.go) —
// works unchanged over a direct libp2p connection built with hole punching
// and NAT-port-mapping enabled, i.e. these additions are transparent to the
// application layer above them.
func TestEnrollmentAndTelemetryStyleHTTPOverDirectConnection(t *testing.T) {
	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/agent/enroll", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "enrolled"})
	})
	mux.HandleFunc("/api/v1/agent/telemetry", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	listener, err := Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	srv := &httptest.Server{Listener: listener, Config: &http.Server{Handler: mux}}
	srv.Start()
	t.Cleanup(srv.Close)

	target, err := ParseTarget(Addrs(serverHost)[0])
	if err != nil {
		t.Fatalf("parse target: %v", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return Dial(ctx, agentHost, target)
		}},
	}

	resp, err := httpClient.Post("http://libp2p/api/v1/agent/enroll", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("enroll over direct libp2p: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("enroll status = %d, want 200", resp.StatusCode)
	}
	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out["status"] != "enrolled" {
		t.Fatalf("unexpected enroll response: %v %v", out, err)
	}

	resp2, err := httpClient.Post("http://libp2p/api/v1/agent/telemetry", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatalf("telemetry over direct libp2p: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusAccepted {
		t.Fatalf("telemetry status = %d, want 202", resp2.StatusCode)
	}
}
