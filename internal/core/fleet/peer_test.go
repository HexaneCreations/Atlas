package fleet

import (
	"crypto/rand"
	"testing"

	"github.com/hexane/atlas/internal/platform/errs"
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

func TestPeerSpecValidateAcceptsAWellFormedSpec(t *testing.T) {
	t.Parallel()
	spec := PeerSpec{PeerID: testPeerID(t), NodeID: "node-a", Environment: "production", Role: PeerRoleAgent}
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	// Role is optional; the store defaults it.
	spec.Role = ""
	if err := spec.Validate(); err != nil {
		t.Fatalf("Validate with empty role: %v", err)
	}
}

// The failure that actually happens in the field: a Peer ID copied one
// character short. It must be refused at registration rather than stored as
// an authorization that can never match a real handshake.
func TestPeerSpecValidateRejectsTruncatedPeerID(t *testing.T) {
	t.Parallel()
	full := testPeerID(t)
	spec := PeerSpec{PeerID: full[:len(full)-1], NodeID: "node-a", Environment: "production"}

	err := spec.Validate()
	if err == nil {
		t.Fatal("Validate accepted a truncated peer id")
	}
	if errs.CodeOf(err) != errs.CodeInvalidArgument {
		t.Errorf("code = %q, want %q", errs.CodeOf(err), errs.CodeInvalidArgument)
	}
}

func TestPeerSpecValidateRequiresEveryBinding(t *testing.T) {
	t.Parallel()
	pid := testPeerID(t)
	cases := map[string]PeerSpec{
		"no peer id":     {NodeID: "node-a", Environment: "production"},
		"no node id":     {PeerID: pid, Environment: "production"},
		"no environment": {PeerID: pid, NodeID: "node-a"},
		"unknown role":   {PeerID: pid, NodeID: "node-a", Environment: "production", Role: "relay"},
	}
	for name, spec := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := spec.Validate(); err == nil {
				t.Fatalf("Validate accepted a spec with %s", name)
			}
		})
	}
}

// The error a caller sees for an unregistered peer must be PermissionDenied,
// not Unauthenticated: by then libp2p has already proven who the peer is.
// The distinction is what tells an operator to go add an authorization
// rather than hunt for a broken credential.
func TestErrPeerNotAuthorizedIsPermissionDenied(t *testing.T) {
	t.Parallel()
	if got := errs.CodeOf(ErrPeerNotAuthorized); got != errs.CodePermissionDenied {
		t.Errorf("code = %q, want %q", got, errs.CodePermissionDenied)
	}
}
