//go:build integration

package notification_test

import (
	"context"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
)

// TestEndMuteAfterExpiryDoesNothingWhenAConcurrentRemuteWinsTheRace is
// the second fix round's required race test: between the evaluator
// deciding a mute has expired and this call actually running, someone
// else re-mutes the node with a later alerts_muted_until. The guarded
// clear's WHERE clause must then match nothing, so neither the column
// nor any notification_state row is touched — the node stays muted for
// this tick, for the evaluator to pick up again on the next one.
func TestEndMuteAfterExpiryDoesNothingWhenAConcurrentRemuteWinsTheRace(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")

	evalNow := time.Now().UTC().Truncate(time.Microsecond)
	past := evalNow.Add(-time.Hour)
	future := evalNow.Add(time.Hour)

	if _, err := pool.Exec(ctx, `UPDATE nodes SET alerts_muted_until = $2 WHERE node_id = $1`, "node-1", past); err != nil {
		t.Fatalf("seed the expired mute: %v", err)
	}
	frozen := corenotification.State{
		Trigger: corenotification.TriggerNodeDown, NodeID: "node-1", State: corenotification.StateFiring,
		Severity: corenotification.SeverityCritical, StateSince: past,
	}
	if err := repo.UpsertState(ctx, frozen); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	// The race itself: a concurrent re-mute sets a later alerts_muted_until
	// before EndMuteAfterExpiry, called with the evaluator's own (now
	// stale) now, gets a chance to run.
	if _, err := pool.Exec(ctx, `UPDATE nodes SET alerts_muted_until = $2 WHERE node_id = $1`, "node-1", future); err != nil {
		t.Fatalf("simulate the concurrent re-mute: %v", err)
	}

	did, err := repo.EndMuteAfterExpiry(ctx, "node-1", evalNow)
	if err != nil {
		t.Fatalf("EndMuteAfterExpiry: %v", err)
	}
	if did {
		t.Fatalf("did = true, want false: the guard must not match a concurrently-set later alerts_muted_until")
	}

	got, found, err := repo.GetState(ctx, corenotification.TriggerNodeDown, "node-1", "")
	if err != nil || !found || got.State != corenotification.StateFiring {
		t.Fatalf("state after the lost race = %+v (found=%v, err=%v), want untouched firing", got, found, err)
	}

	var stillMuted *time.Time
	if err := pool.QueryRow(ctx, `SELECT alerts_muted_until FROM nodes WHERE node_id = $1`, "node-1").Scan(&stillMuted); err != nil {
		t.Fatalf("read alerts_muted_until: %v", err)
	}
	if stillMuted == nil || !stillMuted.Equal(future) {
		t.Errorf("alerts_muted_until = %v, want untouched at the concurrent re-mute's %v", stillMuted, future)
	}
}

// TestEndMuteExplicitlyRollsBackTheClearWhenTheResetFails covers the
// early-unmute path's own atomicity: if the notification_state reset
// fails after the nodes clear has already run (in the same, uncommitted
// transaction), the whole thing must roll back — alerts_muted_until must
// not come back nil with the node's state left exactly as it was before.
//
// The failure is forced by holding an exclusive row lock on the exact
// notification_state row resetNodeStates' UPDATE needs, from a separate,
// never-committed transaction: EndMuteExplicitly's own UPDATE nodes
// succeeds immediately (an unrelated table), but its UPDATE
// notification_state blocks on that lock and is then cancelled by the
// short context deadline — a deterministic way to fail exactly the
// second statement without touching the schema.
func TestEndMuteExplicitlyRollsBackTheClearWhenTheResetFails(t *testing.T) {
	repo, pool := newRepository(t)
	ctx := context.Background()
	seedNode(t, pool, "node-1")

	muted := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, `UPDATE nodes SET alerts_muted_until = $2 WHERE node_id = $1`, "node-1", muted); err != nil {
		t.Fatalf("seed the mute: %v", err)
	}
	frozen := corenotification.State{
		Trigger: corenotification.TriggerNodeDown, NodeID: "node-1", State: corenotification.StateFiring,
		Severity: corenotification.SeverityCritical, StateSince: muted,
	}
	if err := repo.UpsertState(ctx, frozen); err != nil {
		t.Fatalf("seed state: %v", err)
	}

	lockTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin the locking tx: %v", err)
	}
	defer func() { _ = lockTx.Rollback(ctx) }()
	if _, err := lockTx.Exec(ctx, `SELECT 1 FROM notification_state WHERE node_id = $1 FOR UPDATE`, "node-1"); err != nil {
		t.Fatalf("lock the row: %v", err)
	}

	shortCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	did, err := repo.EndMuteExplicitly(shortCtx, "node-1")
	if err == nil {
		t.Fatalf("EndMuteExplicitly returned no error; want the blocked reset statement to fail (did=%v)", did)
	}
	if did {
		t.Errorf("did = true alongside an error; want false")
	}

	// Release the lock before reading the final state back.
	if err := lockTx.Rollback(ctx); err != nil {
		t.Fatalf("release the lock: %v", err)
	}

	var stillMuted *time.Time
	if err := pool.QueryRow(ctx, `SELECT alerts_muted_until FROM nodes WHERE node_id = $1`, "node-1").Scan(&stillMuted); err != nil {
		t.Fatalf("read alerts_muted_until: %v", err)
	}
	if stillMuted == nil || !stillMuted.Equal(muted) {
		t.Errorf("alerts_muted_until = %v, want untouched at %v: the whole transaction must roll back together", stillMuted, muted)
	}

	got, found, err := repo.GetState(ctx, corenotification.TriggerNodeDown, "node-1", "")
	if err != nil || !found || got.State != corenotification.StateFiring {
		t.Fatalf("state after the rollback = %+v (found=%v, err=%v), want untouched firing", got, found, err)
	}
}
