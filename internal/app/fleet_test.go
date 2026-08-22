package app

import (
	"context"
	"testing"
	"time"

	corefleet "github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/pki"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/libp2p/go-libp2p/core/peer"
)

// fakeGrants is an in-memory GrantStore, always-granted unless overridden.
type fakeGrants struct{ granted bool }

func (g *fakeGrants) IsGranted(context.Context, string, string) (bool, error) { return g.granted, nil }
func (g *fakeGrants) Grant(context.Context, string, string, string, time.Time) error {
	g.granted = true
	return nil
}
func (g *fakeGrants) RevokeGrant(context.Context, string, string, string, time.Time) error {
	g.granted = false
	return nil
}

// newTestFleetPipeline builds a fleetPipeline with just enough state set
// directly (bypassing Start, which needs a real Postgres pool) to exercise
// the NodeID-to-PeerID registry and RemoteLogSource behavior in isolation.
func newTestFleetPipeline(t *testing.T) (*fleetPipeline, *pki.CA) {
	t.Helper()
	ca, err := pki.New("test-fleet-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	f := &fleetPipeline{}
	f.peerByNode = make(map[string]peer.ID)
	f.grants = &fakeGrants{granted: true}
	return f, ca
}

// --- NodeID -> PeerID registry ----------------------------------------

// recordAgentPeer is now fed by PeerAuthMiddleware rather than by a
// certificate: both halves it stores are already-established facts (the Peer
// ID from the Noise handshake, the node id from the operator's agent_peers
// registration), so the test supplies them the same way the middleware does.
func TestRecordAgentPeerPopulatesRegistryFromAuthorizedPeer(t *testing.T) {
	f, _ := newTestFleetPipeline(t)

	agentHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	f.recordAgentPeer(corefleet.PeerIdentity{
		PeerID: agentHost.ID().String(), NodeID: "node-a", Environment: "production", Role: corefleet.PeerRoleAgent,
	}, agentHost.ID())

	f.mu.RLock()
	pid, ok := f.peerByNode["node-a"]
	f.mu.RUnlock()
	if !ok {
		t.Fatal("expected node-a to be recorded in the registry")
	}
	if pid != agentHost.ID() {
		t.Errorf("recorded peer id = %s, want %s", pid, agentHost.ID())
	}
}

// A node that reconnects under a new Peer ID (a re-image, or a fresh identity
// file plus a fresh authorization) must overwrite the old mapping, not
// accumulate stale ones.
func TestRecordAgentPeerUpdatesOnReconnectWithNewPeerID(t *testing.T) {
	f, _ := newTestFleetPipeline(t)

	first, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("first host: %v", err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("second host: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })

	f.recordAgentPeer(corefleet.PeerIdentity{PeerID: first.ID().String(), NodeID: "node-a"}, first.ID())
	f.recordAgentPeer(corefleet.PeerIdentity{PeerID: second.ID().String(), NodeID: "node-a"}, second.ID())

	f.mu.RLock()
	pid := f.peerByNode["node-a"]
	f.mu.RUnlock()
	if pid != second.ID() {
		t.Errorf("registry = %s after reconnect, want the new peer id %s", pid, second.ID())
	}
}

// --- Phase 2: AgentOps authorization, independent of authentication --------

// A node with no active libp2p session would normally fail with "unavailable"
// (see TestContainerLogsUnavailableWhenNodeNeverConnected) — but authorization
// is checked first, so a node that additionally lacks a grant must fail with
// a permission error, not "unavailable", and must never reach the peer
// lookup at all.
func TestContainerLogsDeniedWhenNotGranted(t *testing.T) {
	f, _ := newTestFleetPipeline(t)
	f.grants = &fakeGrants{granted: false}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, errCh := f.ContainerLogs(ctx, "node-a", "c1", docker.LogOptions{})

	if _, open := <-lines; open {
		t.Fatal("expected lines to be closed immediately")
	}
	err := <-errCh
	if err == nil {
		t.Fatal("expected a permission error")
	}
	if errs.CodeOf(err) != errs.CodePermissionDenied {
		t.Errorf("code = %q, want %q", errs.CodeOf(err), errs.CodePermissionDenied)
	}
}

func TestContainerLogsAllowedWhenGranted(t *testing.T) {
	f, _ := newTestFleetPipeline(t)
	f.grants = &fakeGrants{granted: true}
	// No libp2p session recorded: authorization passes, then the existing
	// "no active session" check (below) takes over — proves authorization
	// doesn't short-circuit the rest of the function when it succeeds.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, errCh := f.ContainerLogs(ctx, "node-a", "c1", docker.LogOptions{})

	err := <-errCh
	if err == nil || errs.CodeOf(err) != errs.CodeUnavailable {
		t.Fatalf("code = %q, want %q (authorization passed, reachability is the remaining gap)", errs.CodeOf(err), errs.CodeUnavailable)
	}
}

// --- RemoteLogSource: agent offline / unreachable -----------------------

// #9: a node never seen over libp2p has no registry entry at all.
func TestContainerLogsUnavailableWhenNodeNeverConnected(t *testing.T) {
	f, _ := newTestFleetPipeline(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, errCh := f.ContainerLogs(ctx, "never-seen-node", "c1", docker.LogOptions{})

	if _, open := <-lines; open {
		t.Fatal("expected lines to be closed immediately")
	}
	err := <-errCh
	if err == nil {
		t.Fatal("expected an unavailable error")
	}
}

// #10: the registry has an entry, but the peer is not actually reachable
// (e.g. the connection dropped since the last recorded request) — the
// stream open itself must fail, promptly, not hang.
func TestContainerLogsUnavailableWhenPeerUnreachable(t *testing.T) {
	f, _ := newTestFleetPipeline(t)

	serverHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir(), ListenAddrs: []string{"/ip4/127.0.0.1/tcp/0"}})
	if err != nil {
		t.Fatalf("server host: %v", err)
	}
	t.Cleanup(func() { _ = serverHost.Close() })
	f.libp2pHost = serverHost

	// A syntactically valid Peer ID that this host has never connected to
	// and has no address for — exactly what a stale registry entry looks
	// like once the underlying connection is gone.
	ghost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("ghost host: %v", err)
	}
	_ = ghost.Close() // closed immediately: nothing is listening, nothing to reach

	f.peerByNode = map[string]peer.ID{"node-a": ghost.ID()}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	start := time.Now()
	lines, errCh := f.ContainerLogs(ctx, "node-a", "c1", docker.LogOptions{})

	if _, open := <-lines; open {
		t.Fatal("expected lines to be closed")
	}
	err = <-errCh
	if err == nil {
		t.Fatal("expected an error reaching an unreachable peer")
	}
	if elapsed := time.Since(start); elapsed > 12*time.Second {
		t.Errorf("took %s to fail, want it bounded (no indefinite hang)", elapsed)
	}
}
