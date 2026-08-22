//go:build integration

package fleet_test

import (
	"context"
	"crypto/rand"
	"errors"
	"testing"

	corefleet "github.com/hexane/atlas/internal/core/fleet"
	libp2pcrypto "github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

func testPeerID(t *testing.T) string {
	t.Helper()
	priv, _, err := libp2pcrypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	pid, err := peer.IDFromPrivateKey(priv)
	if err != nil {
		t.Fatalf("derive peer id: %v", err)
	}
	return pid.String()
}

func TestRegisterPeerThenAuthorize(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	pid := testPeerID(t)

	if err := repo.RegisterPeer(ctx, corefleet.PeerSpec{
		PeerID: pid, NodeID: "node-a", Environment: "production",
	}); err != nil {
		t.Fatalf("RegisterPeer: %v", err)
	}

	id, err := repo.AuthorizedPeer(ctx, pid)
	if err != nil {
		t.Fatalf("AuthorizedPeer: %v", err)
	}
	if id.NodeID != "node-a" || id.Environment != "production" {
		t.Errorf("identity = %+v, want node-a/production", id)
	}
	if id.Role != corefleet.PeerRoleAgent {
		t.Errorf("role = %q, want %q (defaulted on insert)", id.Role, corefleet.PeerRoleAgent)
	}
}

// An unknown peer must produce ErrPeerNotAuthorized, never a zero identity
// with a nil error — the difference between "no node" and "some node".
func TestAuthorizedPeerRejectsUnknownPeer(t *testing.T) {
	repo := newRepository(t)

	id, err := repo.AuthorizedPeer(context.Background(), testPeerID(t))
	if !errors.Is(err, corefleet.ErrPeerNotAuthorized) {
		t.Fatalf("err = %v, want ErrPeerNotAuthorized", err)
	}
	if id != (corefleet.PeerIdentity{}) {
		t.Errorf("identity = %+v, want the zero value alongside the error", id)
	}
}

func TestRevokePeerRemovesAuthorizationWithoutDeletingTheRecord(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	pid := testPeerID(t)

	if err := repo.RegisterPeer(ctx, corefleet.PeerSpec{PeerID: pid, NodeID: "node-a", Environment: "production"}); err != nil {
		t.Fatalf("RegisterPeer: %v", err)
	}
	if err := repo.RevokePeer(ctx, pid); err != nil {
		t.Fatalf("RevokePeer: %v", err)
	}

	if _, err := repo.AuthorizedPeer(ctx, pid); !errors.Is(err, corefleet.ErrPeerNotAuthorized) {
		t.Fatalf("err = %v, want ErrPeerNotAuthorized after revocation", err)
	}

	peers, err := repo.ListPeers(ctx)
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if len(peers) != 1 || peers[0].Status != corefleet.PeerStatusRevoked {
		t.Fatalf("peers = %+v, want one row still present with status revoked", peers)
	}
}

// Re-registering the same Peer ID rebinds and reactivates it in place —
// one keypair, one binding, revocable in one place. It must not accumulate
// a second row that could disagree with the first.
func TestRegisterPeerIsIdempotentAndReactivates(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	pid := testPeerID(t)

	if err := repo.RegisterPeer(ctx, corefleet.PeerSpec{PeerID: pid, NodeID: "node-a", Environment: "development"}); err != nil {
		t.Fatalf("RegisterPeer: %v", err)
	}
	if err := repo.RevokePeer(ctx, pid); err != nil {
		t.Fatalf("RevokePeer: %v", err)
	}
	if err := repo.RegisterPeer(ctx, corefleet.PeerSpec{PeerID: pid, NodeID: "node-a", Environment: "production"}); err != nil {
		t.Fatalf("re-RegisterPeer: %v", err)
	}

	id, err := repo.AuthorizedPeer(ctx, pid)
	if err != nil {
		t.Fatalf("AuthorizedPeer after re-registration: %v", err)
	}
	if id.Environment != "production" {
		t.Errorf("environment = %q, want production (the corrected binding)", id.Environment)
	}

	peers, err := repo.ListPeers(ctx)
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if len(peers) != 1 {
		t.Errorf("ListPeers returned %d rows, want 1 — re-registration must rebind, not accumulate", len(peers))
	}
}

// Two machines, two keypairs: authorizing one must say nothing about the
// other. This is the property that makes a Peer ID, not a node id, the
// admission key — learning node-a's id gets an attacker nothing.
func TestAuthorizationIsPerPeerNotPerNode(t *testing.T) {
	repo := newRepository(t)
	ctx := context.Background()
	authorized, stranger := testPeerID(t), testPeerID(t)

	if err := repo.RegisterPeer(ctx, corefleet.PeerSpec{PeerID: authorized, NodeID: "node-a", Environment: "production"}); err != nil {
		t.Fatalf("RegisterPeer: %v", err)
	}

	if _, err := repo.AuthorizedPeer(ctx, stranger); !errors.Is(err, corefleet.ErrPeerNotAuthorized) {
		t.Fatalf("err = %v, want ErrPeerNotAuthorized: a second keypair claiming the same node must not be admitted", err)
	}
}
