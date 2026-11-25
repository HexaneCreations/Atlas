//go:build integration

package notification_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	corenotification "github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
)

// fireContainer leaves workloadNode with a firing container_exit and
// container_restart, one delivery each.
func (f *workloadFixture) fireContainer() {
	f.t.Helper()
	f.container("web", 1, 0, 0)
	f.evaluate()
	f.container("web", 0, 1, 1)
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateFiring)
	f.wantState(corenotification.TriggerContainerRestart, "web", corenotification.StateFiring)
}

func (f *workloadFixture) nodeColumns() (mutedUntil, archivedAt *time.Time) {
	f.t.Helper()
	if err := f.pool.QueryRow(context.Background(), `SELECT alerts_muted_until, archived_at FROM nodes WHERE node_id = $1`,
		workloadNode).Scan(&mutedUntil, &archivedAt); err != nil {
		f.t.Fatal(err)
	}
	return mutedUntil, archivedAt
}

// Muted while firing, then fixed, then unmuted early: the column is cleared,
// every state is ok, and no recovered message is ever sent.
func TestNotificationUnmuteResetsSilently(t *testing.T) {
	f := newWorkloadFixture(t)
	ctx := context.Background()
	f.fireContainer()
	if _, err := f.repo.MuteNode(ctx, workloadNode, f.clock.now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.container("web", 1, 1, 0)
	f.clock.advance(30 * time.Minute)
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateFiring) // frozen

	if ok, err := f.repo.EndMuteExplicitly(ctx, workloadNode); err != nil || !ok {
		t.Fatalf("EndMuteExplicitly: %v %v", ok, err)
	}
	if muted, _ := f.nodeColumns(); muted != nil {
		t.Errorf("alerts_muted_until = %v after unmute", muted)
	}
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateOK)
	f.wantState(corenotification.TriggerContainerRestart, "web", corenotification.StateOK)
	for range 6 {
		f.evaluate()
		f.clock.advance(time.Minute)
	}
	f.wantDeliveries(corenotification.TriggerContainerExit, corenotification.KindFiring)
	f.wantDeliveries(corenotification.TriggerContainerRestart, corenotification.KindFiring)
}

// Unmuting a node that is not muted must not silence what is firing now.
func TestNotificationUnmuteOfAnUnmutedNodeChangesNothing(t *testing.T) {
	f := newWorkloadFixture(t)
	f.fireContainer()
	before, _ := f.state(corenotification.TriggerContainerExit, "web")
	if ok, err := f.repo.EndMuteExplicitly(context.Background(), workloadNode); err != nil || !ok {
		t.Fatalf("EndMuteExplicitly: %v %v", ok, err)
	}
	if after, _ := f.state(corenotification.TriggerContainerExit, "web"); !reflect.DeepEqual(before, after) {
		t.Errorf("state changed: %+v -> %+v", before, after)
	}
	if ok, err := f.repo.EndMuteExplicitly(context.Background(), "no-such-node"); err != nil || ok {
		t.Errorf("unknown node: ok=%v err=%v", ok, err)
	}
}

func TestNotificationMuteReplacesTheWindow(t *testing.T) {
	f := newWorkloadFixture(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, until := range []time.Time{now.Add(time.Hour), now.Add(24 * time.Hour), now.Add(time.Minute), corenotification.MutedIndefinitely} {
		got, err := f.repo.MuteNode(ctx, workloadNode, until)
		if err != nil || got.MutedUntil == nil || !got.MutedUntil.Equal(until) || got.ArchivedAt != nil {
			t.Fatalf("MuteNode(%v) = %+v, %v", until, got, err)
		}
		if muted, _ := f.nodeColumns(); muted == nil || !muted.Equal(until) {
			t.Fatalf("column = %v, want %v", muted, until)
		}
	}
	if _, err := f.repo.MuteNode(ctx, "no-such-node", now); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("unknown node: %v, want not_found", err)
	}
}

// The sentinel through the real column: the evaluator leaves the node alone
// and EndMuteAfterExpiry's guard never matches it.
func TestNotificationIndefiniteMuteNeverExpires(t *testing.T) {
	f := newWorkloadFixture(t)
	ctx := context.Background()
	f.fireContainer()
	if _, err := f.repo.MuteNode(ctx, workloadNode, corenotification.MutedIndefinitely); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		f.clock.advance(50 * 365 * 24 * time.Hour)
		f.evaluate()
	}
	if did, err := f.repo.EndMuteAfterExpiry(ctx, workloadNode, f.clock.now()); err != nil || did {
		t.Errorf("EndMuteAfterExpiry = %v %v, want it to refuse", did, err)
	}
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateFiring)
	if muted, _ := f.nodeColumns(); muted == nil || !muted.Equal(corenotification.MutedIndefinitely) {
		t.Errorf("column = %v", muted)
	}
}

func TestNotificationArchiveAndUnarchive(t *testing.T) {
	f := newWorkloadFixture(t)
	ctx := context.Background()
	f.fireContainer()

	at := time.Now().UTC().Truncate(time.Microsecond)
	got, err := f.repo.ArchiveNode(ctx, workloadNode, at)
	if err != nil || got.ArchivedAt == nil || !got.ArchivedAt.Equal(at) {
		t.Fatalf("ArchiveNode = %+v, %v", got, err)
	}
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateOK)
	f.wantState(corenotification.TriggerContainerRestart, "web", corenotification.StateOK)
	if got, err := f.repo.ArchiveNode(ctx, workloadNode, at.Add(time.Hour)); err != nil || !got.ArchivedAt.Equal(at) {
		t.Errorf("archiving again = %+v, %v; want the original time", got, err)
	}

	// Archived: whatever happens, nothing is evaluated.
	f.container("web", 0, 9, 1)
	for range 4 {
		f.evaluate()
		f.clock.advance(time.Minute)
	}
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateOK)
	f.wantDeliveries(corenotification.TriggerContainerExit, corenotification.KindFiring)

	if err := f.repo.UnarchiveNode(ctx, workloadNode); err != nil {
		t.Fatal(err)
	}
	if _, archived := f.nodeColumns(); archived != nil {
		t.Errorf("archived_at = %v after unarchive", archived)
	}
	f.clock.advance(time.Hour) // past the cooldown
	f.evaluate()
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateFiring) // still down: a current condition
	f.wantDeliveries(corenotification.TriggerContainerRestart, corenotification.KindFiring) // not the restarts it missed

	if _, err := f.repo.ArchiveNode(ctx, "no-such-node", at); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("archive unknown: %v", err)
	}
	if err := f.repo.UnarchiveNode(ctx, "no-such-node"); errs.CodeOf(err) != errs.CodeNotFound {
		t.Errorf("unarchive unknown: %v", err)
	}
}

// Archive is one transaction: if the reset fails, archived_at is not set.
func TestNotificationArchiveRollsBackWhenTheResetFails(t *testing.T) {
	f := newWorkloadFixture(t)
	ctx := context.Background()
	f.fireContainer()
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT 1 FROM notification_state WHERE node_id = $1 FOR UPDATE`, workloadNode); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	if _, err := f.repo.ArchiveNode(short, workloadNode, time.Now()); err == nil {
		t.Fatal("ArchiveNode succeeded although the reset was blocked")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, archived := f.nodeColumns(); archived != nil {
		t.Errorf("archived_at = %v although the reset failed", archived)
	}
	f.wantState(corenotification.TriggerContainerExit, "web", corenotification.StateFiring)
}
