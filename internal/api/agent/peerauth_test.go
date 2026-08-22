package agent

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexane/atlas/internal/core/fleet"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

// fakePeerStore authorizes exactly the peers it was built with.
type fakePeerStore struct {
	peers map[string]fleet.PeerIdentity
	calls []string
}

func (s *fakePeerStore) AuthorizedPeer(_ context.Context, peerID string) (fleet.PeerIdentity, error) {
	s.calls = append(s.calls, peerID)
	id, ok := s.peers[peerID]
	if !ok {
		return fleet.PeerIdentity{}, fleet.ErrPeerNotAuthorized
	}
	return id, nil
}

func testPeer(t *testing.T) peer.ID {
	t.Helper()
	priv, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("derive peer id: %v", err)
	}
	return pid
}

func TestPeerAuthMiddlewareInjectsIdentityForAnAuthorizedPeer(t *testing.T) {
	t.Parallel()
	pid := testPeer(t)
	want := fleet.PeerIdentity{PeerID: pid.String(), NodeID: "node-a", Environment: "production", Role: fleet.PeerRoleAgent}
	store := &fakePeerStore{peers: map[string]fleet.PeerIdentity{pid.String(): want}}

	var recorded fleet.PeerIdentity
	var recordedPeer peer.ID
	var got fleet.PeerIdentity
	var served bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		served = true
		got, _ = PeerIdentityFrom(r.Context())
	})

	h := PeerAuthMiddleware(store, nil, func(id fleet.PeerIdentity, p peer.ID) {
		recorded, recordedPeer = id, p
	})(next)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", nil)
	req.RemoteAddr = pid.String()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !served {
		t.Fatal("an authorized peer's request never reached the handler")
	}
	if got != want {
		t.Errorf("identity in context = %+v, want %+v", got, want)
	}
	if recorded != want || recordedPeer != pid {
		t.Errorf("onAuthorized got (%+v, %s), want (%+v, %s)", recorded, recordedPeer, want, pid)
	}
	// The lookup key must be the peer that authenticated, nothing else.
	if len(store.calls) != 1 || store.calls[0] != pid.String() {
		t.Errorf("store consulted with %v, want exactly [%s]", store.calls, pid)
	}
}

// Authentication is not authorization: a real, cryptographically proven peer
// that no operator has registered is refused, and never reaches a handler.
func TestPeerAuthMiddlewareRefusesUnregisteredPeer(t *testing.T) {
	t.Parallel()
	store := &fakePeerStore{peers: map[string]fleet.PeerIdentity{}}

	served := false
	h := PeerAuthMiddleware(store, nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", nil)
	req.RemoteAddr = testPeer(t).String()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if served {
		t.Fatal("an unregistered peer reached the handler")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// Mounted on anything but a libp2p listener, RemoteAddr is "ip:port" and
// decodes as no peer at all. That must refuse the request rather than fall
// through as an unauthenticated one.
func TestPeerAuthMiddlewareRefusesNonLibP2PRemoteAddr(t *testing.T) {
	t.Parallel()
	store := &fakePeerStore{peers: map[string]fleet.PeerIdentity{}}

	served := false
	h := PeerAuthMiddleware(store, nil, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { served = true }))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", nil)
	req.RemoteAddr = "192.0.2.10:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if served {
		t.Fatal("a plain TCP request reached the handler through the libp2p middleware")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
	}
	if len(store.calls) != 0 {
		t.Errorf("store was consulted %d times for a non-peer address, want 0", len(store.calls))
	}
}

// A node id an agent puts in its own request body must never become the
// authenticated identity: the middleware's answer is the only source.
func TestPeerIdentityFromIsAbsentWithoutTheMiddleware(t *testing.T) {
	t.Parallel()
	if _, ok := PeerIdentityFrom(context.Background()); ok {
		t.Fatal("a bare context reported an authenticated peer identity")
	}
}
