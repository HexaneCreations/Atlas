// Package inventory is the PostgreSQL-backed implementation of
// [github.com/hexane/atlas/internal/core/inventory.Store]: latest-only
// snapshots, one row per (node, subject), replaced on arrival.
//
// See docs/architecture/agent-design.md §1 for why this is deliberately not
// a time series — inventory history is a real, different future feature, and
// storing every push as a row would be both enormous (roughly 140 GB/day at
// 1,000 nodes, per that section's sizing) and pointless, since only the
// newest snapshot is ever read.
package inventory

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the inventory snapshot store.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// Put implements [inventory.Store].
//
// A plain upsert, not an insert-only append: see the package doc. The
// content hash and observed_at both update even when the payload itself is
// unchanged — see docs/architecture/agent-design.md §5 on hash-only pushes —
// so a stable host's freshness indicator still advances instead of appearing
// to stall.
func (r *Repository) Put(ctx context.Context, s inventory.StoredSnapshot) error {
	const op = "inventory.Repository.Put"

	const q = `
		INSERT INTO inventory_snapshots (node_id, subject, observed_at, received_at, content_hash, data)
		VALUES ($1, $2, $3, now(), $4, $5)
		ON CONFLICT (node_id, subject) DO UPDATE SET
			observed_at  = EXCLUDED.observed_at,
			received_at  = now(),
			content_hash = EXCLUDED.content_hash,
			data         = EXCLUDED.data`

	if _, err := r.pool.Exec(ctx, q, s.NodeID, string(s.Subject), s.ObservedAt, s.ContentHash, s.Data); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not store the inventory snapshot").
			WithOp(op).WithDetail("node_id", s.NodeID).WithDetail("subject", string(s.Subject))
	}
	return nil
}

// Get implements [inventory.Store].
func (r *Repository) Get(ctx context.Context, nodeID string, subject inventory.Subject) (inventory.StoredSnapshot, error) {
	const op = "inventory.Repository.Get"

	const q = `
		SELECT observed_at, received_at, content_hash, data
		FROM inventory_snapshots
		WHERE node_id = $1 AND subject = $2`

	var s inventory.StoredSnapshot
	s.NodeID, s.Subject = nodeID, subject
	err := r.pool.QueryRow(ctx, q, nodeID, string(subject)).
		Scan(&s.ObservedAt, &s.ReceivedAt, &s.ContentHash, &s.Data)
	if err != nil {
		if err == pgx.ErrNoRows {
			return inventory.StoredSnapshot{}, errs.New(errs.CodeNotFound,
				"no %s inventory has been pushed for this node", subject).
				WithOp(op).WithDetail("node_id", nodeID).WithDetail("subject", string(subject))
		}
		return inventory.StoredSnapshot{}, errs.Wrap(err, errs.CodeUnavailable,
			"could not read the inventory snapshot").WithOp(op)
	}
	return s, nil
}

// HasReported implements [inventory.Store].
func (r *Repository) HasReported(ctx context.Context, nodeID string) (bool, error) {
	const op = "inventory.Repository.HasReported"

	const q = `SELECT 1 FROM inventory_snapshots WHERE node_id = $1 LIMIT 1`
	var one int
	err := r.pool.QueryRow(ctx, q, nodeID).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case err == pgx.ErrNoRows:
		return false, nil
	default:
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not check inventory history").WithOp(op)
	}
}

// LastReceivedAt returns the most recent received_at across every subject
// nodeID has pushed, the freshness signal the health score engine reads —
// see [github.com/hexane/atlas/internal/core/healthscore.InventoryFreshness].
func (r *Repository) LastReceivedAt(ctx context.Context, nodeID string) (time.Time, bool, error) {
	const op = "inventory.Repository.LastReceivedAt"

	const q = `SELECT MAX(received_at) FROM inventory_snapshots WHERE node_id = $1`
	var last *time.Time
	if err := r.pool.QueryRow(ctx, q, nodeID).Scan(&last); err != nil {
		return time.Time{}, false, errs.Wrap(err, errs.CodeUnavailable, "could not read inventory freshness").WithOp(op)
	}
	if last == nil {
		return time.Time{}, false, nil
	}
	return *last, true, nil
}

// There is no PruneOlderThan here: unlike ingested_envelopes, this table's
// size is bounded by the number of (node, subject) pairs, not by time — see
// the package doc. There is nothing for a retention policy to remove.

var _ inventory.Store = (*Repository)(nil)
