package agent

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/libp2p/go-libp2p/core/peer"
)

func TestCachedTargetRoundTrip(t *testing.T) {
	dir := t.TempDir()

	if _, ok := loadCachedTarget(dir); ok {
		t.Fatal("expected no cached target before one is saved")
	}

	want := cachedTarget{DirectAddrs: []string{"/ip4/127.0.0.1/tcp/4102/p2p/12D3KooWtest"}, CircuitAddr: "/ip4/127.0.0.1/tcp/4103/p2p/relay/p2p-circuit/p2p/12D3KooWtest"}
	if err := saveCachedTarget(dir, want); err != nil {
		t.Fatalf("saveCachedTarget: %v", err)
	}

	got, ok := loadCachedTarget(dir)
	if !ok {
		t.Fatal("expected a cached target after saving one")
	}
	if len(got.DirectAddrs) != 1 || got.DirectAddrs[0] != want.DirectAddrs[0] || got.CircuitAddr != want.CircuitAddr {
		t.Fatalf("loadCachedTarget = %+v, want %+v", got, want)
	}
}

func TestBuildCandidatesFiltersMismatchedPeerID(t *testing.T) {
	serverHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	otherHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("other host: %v", err)
	}
	t.Cleanup(func() { _ = otherHost.Close() })

	direct := append(libp2ptransport.Addrs(serverHost), libp2ptransport.Addrs(otherHost)...)

	candidates, err := buildCandidates(direct, "", serverHost.ID())
	if err != nil {
		t.Fatalf("buildCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != serverHost.ID() {
		t.Fatalf("candidates = %+v, want exactly one entry for %s", candidates, serverHost.ID())
	}
}

func TestBuildCandidatesErrorsWhenNoneValid(t *testing.T) {
	serverHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	if _, err := buildCandidates(nil, "", serverHost.ID()); err == nil {
		t.Fatal("expected an error when no direct or circuit addresses are given")
	}
}

// TestResolveCandidatesFallsBackToCacheWhenLookupFails proves an Agent that
// has already discovered the Server once can still reconnect during a
// temporary relay outage, using its last cached result instead of failing.
func TestResolveCandidatesFallsBackToCacheWhenLookupFails(t *testing.T) {
	serverHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	agentHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	dataDir := t.TempDir()
	direct := libp2ptransport.Addrs(serverHost)
	if err := saveCachedTarget(dataDir, cachedTarget{DirectAddrs: direct}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	// An unreachable relay address (nothing listens there): Lookup must
	// fail, forcing resolveCandidates onto the cache path.
	badRelay := peer.AddrInfo{ID: serverHost.ID()}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	candidates, err := resolveCandidates(ctx, agentHost, badRelay, serverHost.ID(), dataDir, discardLogger())
	if err != nil {
		t.Fatalf("resolveCandidates: %v", err)
	}
	if len(candidates) != 1 || candidates[0].ID != serverHost.ID() {
		t.Fatalf("candidates = %+v, want the cached direct address for %s", candidates, serverHost.ID())
	}
}

// TestNewDiscoveryDialConnectsAndCaches is the full rendezvous discovery
// path used by a real Agent: Relay registry, Server announce, Agent lookup,
// direct dial, and persisting the result to disk for next time.
func TestNewDiscoveryDialConnectsAndCaches(t *testing.T) {
	relayHost, err := libp2ptransport.NewRelayHost(t.TempDir(), []string{"/ip4/127.0.0.1/tcp/0"})
	if err != nil {
		t.Fatalf("relay host: %v", err)
	}
	t.Cleanup(func() { _ = relayHost.Close() })
	libp2ptransport.RegisterRendezvousHandlers(relayHost, libp2ptransport.NewRegistry())

	relayInfo, err := libp2ptransport.ParseTarget(libp2ptransport.Addrs(relayHost)[0])
	if err != nil {
		t.Fatalf("parse relay target: %v", err)
	}

	serverHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })

	listener, err := libp2ptransport.Listen(serverHost)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	const message = "discovered and dialed"
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := libp2ptransport.Announce(ctx, serverHost, relayInfo, libp2ptransport.Addrs(serverHost), ""); err != nil {
		t.Fatalf("announce: %v", err)
	}

	agentHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	dataDir := t.TempDir()
	dial := newDiscoveryDial(agentHost, relayInfo, serverHost.ID(), dataDir, slog.New(slog.DiscardHandler))

	conn, err := dial(ctx, "", "")
	if err != nil {
		t.Fatalf("discovery dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(message)); err != nil {
		t.Fatalf("write: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("server never received the message via discovery dial")
	}

	if _, ok := loadCachedTarget(dataDir); !ok {
		t.Fatal("expected the successful lookup to be cached to disk")
	}
}
