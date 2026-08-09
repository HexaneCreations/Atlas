package inventory

import (
	"context"

	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Receiver adapts a [Repository] into a [transport.Receiver] for
// [transport.KindInventory] envelopes, so it can be registered on a
// [transport.Router] alongside the metrics receiver.
type Receiver struct {
	repo *Repository
}

// NewReceiver builds a Receiver over repo.
func NewReceiver(repo *Repository) *Receiver { return &Receiver{repo: repo} }

// Kind implements [transport.Receiver].
func (*Receiver) Kind() transport.Kind { return transport.KindInventory }

// Receive implements [transport.Receiver]. Idempotent by construction: [Put]
// upserts the (node, subject) row, so replaying the same snapshot twice
// leaves the same row rather than duplicating anything.
func (r *Receiver) Receive(ctx context.Context, env transport.Envelope) error {
	const op = "inventory.Receiver.Receive"

	payload, ok := env.Payload.(coreinventory.Payload)
	if !ok {
		return errs.New(errs.CodeInternal, "inventory receiver was given a %s envelope", env.Kind()).WithOp(op)
	}

	return r.repo.Put(ctx, coreinventory.StoredSnapshot{
		NodeID:      env.Origin.NodeID,
		Subject:     payload.Subject,
		ObservedAt:  payload.ObservedAt,
		ContentHash: payload.ContentHash,
		Data:        payload.Data,
	})
}

var _ transport.Receiver = (*Receiver)(nil)
