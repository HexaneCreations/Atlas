package fleet

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/libp2p/go-libp2p/core/peer"
)

// PeerRoleAgent is the only role Atlas authorizes today. The column exists
// so that adding a second kind of peer (a relay, a peering control plane) is
// a value rather than a migration; nothing dispatches on it yet.
const PeerRoleAgent = "agent"

// Peer statuses. Revocation is a status change rather than a delete: the row
// is the record that this keypair was once trusted.
const (
	PeerStatusActive  = "active"
	PeerStatusRevoked = "revoked"
)

// PeerIdentity is what an authorized libp2p Peer ID resolves to: the machine
// it speaks for and the environment that authorization is scoped to.
//
// The two identities are deliberately distinct and neither replaces the
// other. PeerID is the transport identity — proven cryptographically by the
// libp2p Noise handshake, never asserted by the agent in a payload. NodeID is
// the stable machine identity every other table in Atlas is keyed by. This
// struct is the only place the two are joined, and the join is established
// by an operator, not by traffic.
type PeerIdentity struct {
	PeerID      string
	NodeID      string
	Environment string
	Role        string
}

// PeerSpec describes a Peer ID authorization to create.
type PeerSpec struct {
	PeerID      string
	NodeID      string
	Environment string
	// Role defaults to [PeerRoleAgent] when empty.
	Role string
}

// Validate reports whether the spec is usable.
//
// PeerID is checked by actually decoding it, not by measuring it: a Peer ID
// that is one character short of a real one is a plausible copy/paste result
// and would otherwise sit in the table authorizing nothing, presenting as an
// agent that mysteriously cannot connect.
func (s PeerSpec) Validate() error {
	const op = "fleet.PeerSpec.Validate"
	switch {
	case s.PeerID == "":
		return errs.New(errs.CodeInvalidArgument, "a peer authorization must name a libp2p peer id").WithOp(op)
	case s.NodeID == "":
		return errs.New(errs.CodeInvalidArgument, "a peer authorization must name the node id it speaks for").WithOp(op)
	case s.Environment == "":
		return errs.New(errs.CodeInvalidArgument,
			"a peer authorization must name the environment it is scoped to").WithOp(op)
	case s.Role != "" && s.Role != PeerRoleAgent:
		return errs.New(errs.CodeInvalidArgument, "unknown peer role %q", s.Role).WithOp(op)
	}
	if _, err := peer.Decode(s.PeerID); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument,
			"that is not a valid libp2p peer id; copy it from the agent's own startup log or `atlas-agent peer-id`").
			WithOp(op)
	}
	return nil
}

// ErrPeerNotAuthorized is returned by [PeerStore.AuthorizedPeer] for a peer
// with no active authorization — unknown, revoked, or registered for a
// different environment.
//
// PermissionDenied rather than Unauthenticated, and the distinction is not
// cosmetic: by the time this error can be produced, the Noise handshake has
// already proven who the peer is. What it lacks is permission, and an
// operator reading a 403 here should go look for a missing `peer authorize`,
// not for a broken credential.
var ErrPeerNotAuthorized = errs.New(errs.CodePermissionDenied, "this libp2p peer is not authorized to act as an agent")

// PeerStore is the storage surface for Peer ID authorization. Implemented by
// internal/storage/fleet.Repository.
type PeerStore interface {
	// AuthorizedPeer resolves an authenticated Peer ID to the node and
	// environment it may act as. It returns [ErrPeerNotAuthorized] when no
	// active authorization exists — never a zero identity with a nil error,
	// so a caller cannot accidentally treat "unknown peer" as "some node".
	AuthorizedPeer(ctx context.Context, peerID string) (PeerIdentity, error)
}

// PeerRegistry is the operator-facing half of the same table: registering,
// listing and revoking authorizations. Separate from [PeerStore] because the
// request path only ever needs to read, and should not be handed an
// interface that can grant.
type PeerRegistry interface {
	RegisterPeer(ctx context.Context, spec PeerSpec) error
	RevokePeer(ctx context.Context, peerID string) error
	ListPeers(ctx context.Context) ([]PeerAuthorization, error)
}

// PeerAuthorization is a row as an operator sees it, including the
// bookkeeping fields the request path has no use for.
type PeerAuthorization struct {
	PeerIdentity
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}
