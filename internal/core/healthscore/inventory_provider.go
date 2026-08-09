package healthscore

import (
	"context"
	"fmt"
	"time"
)

// InventoryFreshness resolves how recently a node last pushed any inventory
// snapshot. Satisfied via an adapter over
// [github.com/hexane/atlas/internal/storage/inventory.Repository.LastReceivedAt];
// see internal/app.
type InventoryFreshness interface {
	// LastReceivedAt returns the most recent receipt time across every
	// subject a node has pushed, or found=false if it has never reported.
	LastReceivedAt(ctx context.Context, nodeID string) (time.Time, bool, error)
}

const (
	inventoryFreshWithin = 5 * time.Minute
	inventoryStaleWithin = 30 * time.Minute
)

// InventoryProvider scores a node on how recently its inventory was pushed.
//
// Inventory is agent-only, so a node with no inventory store configured, or
// that has never reported inventory, is unavailable rather than penalised —
// the same convention the inventory API endpoints already use for a node
// with no agent support wired in.
type InventoryProvider struct {
	Inventory InventoryFreshness
}

func (p InventoryProvider) Name() string { return "inventory" }

func (p InventoryProvider) Score(ctx context.Context, nodeID string) (Signal, error) {
	if p.Inventory == nil {
		return Signal{Available: false, Detail: "inventory not configured"}, nil
	}

	last, found, err := p.Inventory.LastReceivedAt(ctx, nodeID)
	if err != nil {
		return Signal{}, err
	}
	if !found {
		return Signal{Available: false, Detail: "no inventory reported"}, nil
	}

	age := time.Since(last)
	var score float64
	switch {
	case age <= inventoryFreshWithin:
		score = 100
	case age >= inventoryStaleWithin:
		score = 0
	default:
		frac := 1 - float64(age-inventoryFreshWithin)/float64(inventoryStaleWithin-inventoryFreshWithin)
		score = 100 * frac
	}
	return Signal{Score: score, Available: true, Detail: fmt.Sprintf("last inventory %s ago", age.Round(time.Second))}, nil
}
