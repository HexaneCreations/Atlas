package libp2ptransport

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

func TestLoadOrCreateIdentityIsStableAcrossCalls(t *testing.T) {
	dir := t.TempDir()

	first, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	second, err := LoadOrCreateIdentity(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if !first.Equals(second) {
		t.Fatal("identity changed across restarts; Peer ID would not be stable")
	}
}

// TestDialListenCarriesBytes proves a real libp2p connection (Noise-encrypted
// stream, real Peer ID handshake, real TCP transport under it — loopback
// only, but nothing about the libp2p stack is faked) can carry an arbitrary
// byte stream, which is what remote.Transport's http.Client/http.Server pair
// will be handed in place of a plain net.Dial.
func TestDialListenCarriesBytes(t *testing.T) {
	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	clientHost, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("client host: %v", err)
	}
	t.Cleanup(func() { _ = clientHost.Close() })

	listener, err := Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	const message = "atlas telemetry envelope"
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, len(message))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		if string(buf) == message {
			close(accepted)
		}
	}()

	addrs := Addrs(serverHost)
	if len(addrs) == 0 {
		t.Fatal("server host advertised no listen addresses")
	}
	target, err := ParseTarget(addrs[0])
	if err != nil {
		t.Fatalf("parse target %q: %v", addrs[0], err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := Dial(ctx, clientHost, target)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(message)); err != nil {
		t.Fatalf("write: %v", err)
	}

	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the message")
	}
}

// TestReserveRelayThenDialThroughCircuit proves the Atlas Relay v1 path: a
// server host with NO direct listen address (simulating NAT — nothing an
// outside peer could ever dial directly) reserves a slot on a real
// circuit-relay-v2 relay host, and an agent host dials the resulting circuit
// multiaddr and successfully carries bytes to it. Three real libp2p hosts,
// a real relay hop, real Noise-encrypted end-to-end stream — loopback
// network only, nothing about the libp2p/relay stack is faked.
func TestReserveRelayThenDialThroughCircuit(t *testing.T) {
	relayHost, err := NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("relay host: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })

	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir()}) // no listen addrs: simulates NAT
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

	const message = "atlas telemetry envelope via relay"
	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, len(message))
		if _, err := io.ReadFull(conn, buf); err != nil {
			return
		}
		if string(buf) == message {
			close(accepted)
		}
	}()

	relayAddrs := Addrs(relayHost)
	if len(relayAddrs) == 0 {
		t.Fatal("relay host advertised no listen addresses")
	}
	relayInfo, err := ParseTarget(relayAddrs[0])
	if err != nil {
		t.Fatalf("parse relay target: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	circuitAddr, expiration, err := ReserveRelay(ctx, serverHost, relayInfo)
	if err != nil {
		t.Fatalf("reserve relay: %v", err)
	}
	if expiration.Before(time.Now()) {
		t.Fatalf("reservation already expired: %v", expiration)
	}

	target, err := ParseTarget(circuitAddr)
	if err != nil {
		t.Fatalf("parse circuit addr %q: %v", circuitAddr, err)
	}

	conn, err := Dial(ctx, agentHost, target)
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
		t.Fatal("server never received the message through the relay")
	}
}

// TestAnnounceThenLookupReturnsCandidates proves the rendezvous discovery
// path: a Server announces its addresses to a real Relay's registry, and an
// Agent that only knows the Server's Peer ID looks them back up — no
// manually assembled circuit multiaddr involved.
func TestAnnounceThenLookupReturnsCandidates(t *testing.T) {
	relayHost, err := NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("relay host: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })
	RegisterRendezvousHandlers(relayHost, NewRegistry())

	relayAddrs := Addrs(relayHost)
	if len(relayAddrs) == 0 {
		t.Fatal("relay host advertised no listen addresses")
	}
	relayInfo, err := ParseTarget(relayAddrs[0])
	if err != nil {
		t.Fatalf("parse relay target: %v", err)
	}

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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	directAddrs := Addrs(serverHost)
	if len(directAddrs) == 0 {
		t.Fatal("server host advertised no direct addresses")
	}
	if err := Announce(ctx, serverHost, relayInfo, directAddrs, ""); err != nil {
		t.Fatalf("announce: %v", err)
	}

	res, err := Lookup(ctx, agentHost, relayInfo, serverHost.ID())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if !res.Found {
		t.Fatal("expected lookup to find the announced record")
	}
	if len(res.DirectAddrs) != len(directAddrs) || res.DirectAddrs[0] != directAddrs[0] {
		t.Fatalf("looked-up direct addrs = %v, want %v", res.DirectAddrs, directAddrs)
	}
}

// TestLookupUnknownPeerNotFound proves a Relay with no announced record for
// a Peer ID reports that plainly rather than erroring or fabricating one.
func TestLookupUnknownPeerNotFound(t *testing.T) {
	relayHost, err := NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("relay host: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })
	RegisterRendezvousHandlers(relayHost, NewRegistry())

	relayInfo, err := ParseTarget(Addrs(relayHost)[0])
	if err != nil {
		t.Fatalf("parse relay target: %v", err)
	}

	agentHost, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	unknownHost, err := NewHost(HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("unknown host: %v", err)
	}
	t.Cleanup(func() { _ = unknownHost.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	res, err := Lookup(ctx, agentHost, relayInfo, unknownHost.ID())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if res.Found {
		t.Fatal("expected lookup for an unannounced peer to report not found")
	}
}

// TestDialWithFallbackUsesDirectWhenReachable proves direct-first
// preference: given a reachable direct candidate and an unusable one after
// it, DialWithFallback succeeds without needing the fallback candidate at
// all — the required behavior for a publicly-reachable Server (relay
// should not be needed once direct works).
func TestDialWithFallbackUsesDirectWhenReachable(t *testing.T) {
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

	listener, err := Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	const message = "direct preferred"
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

	direct, err := ParseTarget(Addrs(serverHost)[0])
	if err != nil {
		t.Fatalf("parse direct target: %v", err)
	}
	// A second candidate that would fail if ever dialed — proves the first,
	// reachable candidate is what gets used, and the loop stops there.
	unreachableCircuit := peer.AddrInfo{ID: serverHost.ID()}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := DialWithFallback(ctx, agentHost, []peer.AddrInfo{direct, unreachableCircuit})
	if err != nil {
		t.Fatalf("DialWithFallback: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(message)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the message via the direct candidate")
	}
}

// TestDialWithFallbackFallsBackWhenDirectUnreachable proves the NAT'd-Server
// case: an unreachable direct candidate is skipped and a working relay
// circuit candidate after it is used instead.
func TestDialWithFallbackFallsBackWhenDirectUnreachable(t *testing.T) {
	relayHost, err := NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("relay host: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })

	serverHost, err := NewHost(HostOptions{DataDir: t.TempDir()}) // no listen addrs: simulates NAT
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

	const message = "fell back to relay"
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

	relayInfo, err := ParseTarget(Addrs(relayHost)[0])
	if err != nil {
		t.Fatalf("parse relay target: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	circuitAddr, _, err := ReserveRelay(ctx, serverHost, relayInfo)
	if err != nil {
		t.Fatalf("reserve relay: %v", err)
	}
	circuit, err := ParseTarget(circuitAddr)
	if err != nil {
		t.Fatalf("parse circuit target: %v", err)
	}

	badAddr, err := multiaddr.NewMultiaddr("/ip4/127.0.0.1/tcp/1")
	if err != nil {
		t.Fatalf("build bad multiaddr: %v", err)
	}
	unreachableDirect := peer.AddrInfo{ID: serverHost.ID(), Addrs: []multiaddr.Multiaddr{badAddr}}

	conn, err := DialWithFallback(ctx, agentHost, []peer.AddrInfo{unreachableDirect, circuit})
	if err != nil {
		t.Fatalf("DialWithFallback: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(message)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the message via the relay fallback candidate")
	}
}

// TestParseTargetRejectsAddrWithoutPeerID guards the one input this POC
// trusts from configuration: a multiaddr missing /p2p/<id> names a location,
// not an identity, which is exactly what ADR-0012 says Atlas must not dial by.
func TestParseTargetRejectsAddrWithoutPeerID(t *testing.T) {
	if _, err := ParseTarget("/ip4/127.0.0.1/tcp/4102"); err == nil {
		t.Fatal("expected an error for a multiaddr with no peer id")
	}
}
