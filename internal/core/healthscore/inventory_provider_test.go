package healthscore

import (
	"context"
	"testing"
	"time"
)

type fakeInventoryFreshness struct {
	last  time.Time
	found bool
}

func (f fakeInventoryFreshness) LastReceivedAt(context.Context, string) (time.Time, bool, error) {
	return f.last, f.found, nil
}

func TestInventoryProviderUnavailableWhenNeverReported(t *testing.T) {
	p := InventoryProvider{Inventory: fakeInventoryFreshness{found: false}}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Available {
		t.Fatal("expected unavailable for a node with no inventory pushed")
	}
}

func TestInventoryProviderUnavailableWhenNotConfigured(t *testing.T) {
	p := InventoryProvider{}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Available {
		t.Fatal("expected unavailable when no inventory store is wired")
	}
}

func TestInventoryProviderScoresFreshAsHealthy(t *testing.T) {
	p := InventoryProvider{Inventory: fakeInventoryFreshness{last: time.Now(), found: true}}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !sig.Available || sig.Score != 100 {
		t.Fatalf("got %+v, want available score 100", sig)
	}
}

func TestInventoryProviderScoresStaleAsUnhealthy(t *testing.T) {
	p := InventoryProvider{Inventory: fakeInventoryFreshness{last: time.Now().Add(-time.Hour), found: true}}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Score != 0 {
		t.Fatalf("score = %v, want 0 for inventory an hour stale", sig.Score)
	}
}

func TestInventoryProviderInterpolatesBetweenFreshAndStale(t *testing.T) {
	mid := inventoryFreshWithin + (inventoryStaleWithin-inventoryFreshWithin)/2
	p := InventoryProvider{Inventory: fakeInventoryFreshness{last: time.Now().Add(-mid), found: true}}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Score < 40 || sig.Score > 60 {
		t.Fatalf("score = %v, want roughly 50 halfway through the freshness window", sig.Score)
	}
}
