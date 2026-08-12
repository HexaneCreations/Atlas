package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/platform/pki"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/libp2p/go-libp2p/core/peer"
)

// newTestFleetPipeline builds a fleetPipeline with just enough state set
// directly (bypassing Start, which needs a real Postgres pool) to exercise
// the NodeID-to-PeerID registry and RemoteLogSource behavior in isolation.
func newTestFleetPipeline(t *testing.T) (*fleetPipeline, *pki.CA) {
	t.Helper()
	ca, err := pki.New("test-fleet-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	serverLeaf, err := pki.NewServerLeaf(ca, nil)
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	f := &fleetPipeline{}
	f.caCert = ca.Cert()
	f.serverLeaf = serverLeaf
	f.peerByNode = make(map[string]peer.ID)
	return f, ca
}

func testAgentCert(t *testing.T, ca *pki.CA, nodeID string) *x509.Certificate {
	t.Helper()
	csrDER, _, err := pki.NewCSR(nodeID)
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
	return leaf
}

// --- Phase 1: NodeID -> PeerID registry -------------------------------

func TestRecordAgentPeerPopulatesRegistryFromAuthenticatedRequest(t *testing.T) {
	f, ca := newTestFleetPipeline(t)

	agentHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("agent host: %v", err)
	}
	t.Cleanup(func() { _ = agentHost.Close() })

	cert := testAgentCert(t, ca, "node-a")

	handlerCalled := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handlerCalled = true })

	req := httptest.NewRequest(http.MethodGet, "/api/v1/agent/renew", nil)
	req.RemoteAddr = agentHost.ID().String()
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}

	f.recordAgentPeer(next).ServeHTTP(httptest.NewRecorder(), req)

	if !handlerCalled {
		t.Fatal("recordAgentPeer must always call the wrapped handler")
	}

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

// A node id that reconnects under a new Peer ID (e.g. a re-image, or a fresh
// identity file) must overwrite the old mapping, not accumulate stale ones —
// this is the "connect/disconnect maintained correctly" requirement.
func TestRecordAgentPeerUpdatesOnReconnectWithNewPeerID(t *testing.T) {
	f, ca := newTestFleetPipeline(t)
	cert := testAgentCert(t, ca, "node-a")
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

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

	req1 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/renew", nil)
	req1.RemoteAddr = first.ID().String()
	req1.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	f.recordAgentPeer(next).ServeHTTP(httptest.NewRecorder(), req1)

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/agent/renew", nil)
	req2.RemoteAddr = second.ID().String()
	req2.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	f.recordAgentPeer(next).ServeHTTP(httptest.NewRecorder(), req2)

	f.mu.RLock()
	pid := f.peerByNode["node-a"]
	f.mu.RUnlock()
	if pid != second.ID() {
		t.Errorf("registry = %s after reconnect, want the new peer id %s", pid, second.ID())
	}
}

// A request with no TLS client certificate (e.g. the enrollment endpoint,
// which allows an unauthenticated call) must not panic and must not record
// anything.
func TestRecordAgentPeerIgnoresUnauthenticatedRequests(t *testing.T) {
	f, _ := newTestFleetPipeline(t)
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/enroll", nil)
	f.recordAgentPeer(next).ServeHTTP(httptest.NewRecorder(), req)

	f.mu.RLock()
	n := len(f.peerByNode)
	f.mu.RUnlock()
	if n != 0 {
		t.Errorf("registry has %d entries, want 0 for an unauthenticated request", n)
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
