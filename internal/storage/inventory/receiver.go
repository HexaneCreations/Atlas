package inventory

import (
	"context"
	"encoding/json"
	"time"

	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Promoter promotes the subjects whose data also has a dedicated home in the
// nodes table out of the latest-only snapshot and into that home, so a remote
// agent's host facts and interface addressing land in `nodes` /
// `node_addresses` and not only inside inventory_snapshots.
//
// The local self-monitoring path already does this via the system plugin's
// host collector; this interface is its missing sibling for snapshots that
// arrive from a remote agent over the transport. Optional on [NewReceiver]:
// the in-process path has no remote agents and passes nil.
type Promoter interface {
	// PromoteHost is called with a stored "host" snapshot's raw payload.
	PromoteHost(ctx context.Context, env transport.Envelope, data json.RawMessage) error
	// PromoteNetwork is called with a stored "network" snapshot's raw payload
	// and the host observation time it carried.
	PromoteNetwork(ctx context.Context, env transport.Envelope, observedAt time.Time, data json.RawMessage) error
}

// Receiver adapts a [Repository] into a [transport.Receiver] for
// [transport.KindInventory] envelopes, so it can be registered on a
// [transport.Router] alongside the metrics receiver.
type Receiver struct {
	repo     *Repository
	promoter Promoter
}

// NewReceiver builds a Receiver over repo. promoter may be nil, which
// disables promotion of the host and network subjects.
func NewReceiver(repo *Repository, promoter Promoter) *Receiver {
	return &Receiver{repo: repo, promoter: promoter}
}

// Kind implements [transport.Receiver].
func (*Receiver) Kind() transport.Kind { return transport.KindInventory }

// Receive implements [transport.Receiver]. Idempotent by construction: [Put]
// upserts the (node, subject) row, so replaying the same snapshot twice
// leaves the same row rather than duplicating anything.
//
// After the snapshot is stored, the host and network subjects are promoted
// into the nodes table. Promotion runs the same replace-on-arrival way the
// snapshot store does, so a retry of the same envelope is harmless; a
// promotion failure is returned so the envelope is retried rather than the
// promotion silently lost.
func (r *Receiver) Receive(ctx context.Context, env transport.Envelope) error {
	const op = "inventory.Receiver.Receive"

	payload, ok := env.Payload.(coreinventory.Payload)
	if !ok {
		return errs.New(errs.CodeInternal, "inventory receiver was given a %s envelope", env.Kind()).WithOp(op)
	}

	if err := r.repo.Put(ctx, coreinventory.StoredSnapshot{
		NodeID:      env.Origin.NodeID,
		Subject:     payload.Subject,
		ObservedAt:  payload.ObservedAt,
		ContentHash: payload.ContentHash,
		Data:        payload.Data,
	}); err != nil {
		return err
	}

	if r.promoter == nil {
		return nil
	}
	switch payload.Subject {
	case coreinventory.SubjectHost:
		return r.promoter.PromoteHost(ctx, env, payload.Data)
	case coreinventory.SubjectNetwork:
		return r.promoter.PromoteNetwork(ctx, env, payload.ObservedAt, payload.Data)
	}
	return nil
}

var _ transport.Receiver = (*Receiver)(nil)
