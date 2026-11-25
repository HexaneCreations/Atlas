package notification

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeExecer is a minimal execer for testing endMute's own sequencing
// without a database: it counts calls, can fail a chosen call, and
// reports a configurable RowsAffected per call so a test can drive the
// guarded-clear-matches-nothing path without any SQL at all.
type fakeExecer struct {
	calls      int
	fail       int   // which call (1-indexed) fails; 0 = never
	rowsByCall []int // RowsAffected to report per call; missing entries default to 1
}

func (f *fakeExecer) Exec(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
	f.calls++
	if f.fail != 0 && f.calls == f.fail {
		return pgconn.CommandTag{}, errors.New("simulated failure")
	}
	rows := 1
	if idx := f.calls - 1; idx < len(f.rowsByCall) {
		rows = f.rowsByCall[idx]
	}
	return pgconn.NewCommandTag(fmt.Sprintf("UPDATE %d", rows)), nil
}

// TestEndMuteSkipsTheResetWhenTheGuardedClearMatchesNothing covers the
// "did nothing" half of endMute's contract directly: if the clear's own
// WHERE clause (the guard) matches no row, the reset must never run —
// exactly what stops a concurrent re-mute's later alerts_muted_until
// from being clobbered along with a reset it should never have gotten.
func TestEndMuteSkipsTheResetWhenTheGuardedClearMatchesNothing(t *testing.T) {
	exec := &fakeExecer{rowsByCall: []int{0}}
	did, err := endMute(context.Background(), exec, "node-1", "alerts_muted_until <= $2", 0)
	if err != nil {
		t.Fatalf("endMute: %v", err)
	}
	if did {
		t.Errorf("did = true, want false: the guarded clear matched nothing")
	}
	if exec.calls != 1 {
		t.Errorf("exec was called %d times, want exactly 1 (the clear only — no reset when nothing was cleared)", exec.calls)
	}
}

// TestEndMutePropagatesTheResetStatementsFailure covers the other half:
// once the clear has matched a row, a failure in the reset that follows
// it must come back as an error (and did=false) rather than being
// swallowed — so the caller's own transaction rolls back the clear too,
// instead of committing a cleared column with frozen state left behind.
func TestEndMutePropagatesTheResetStatementsFailure(t *testing.T) {
	exec := &fakeExecer{fail: 2}
	did, err := endMute(context.Background(), exec, "node-1", "")
	if err == nil {
		t.Fatal("endMute returned no error; want the reset statement's failure propagated")
	}
	if did {
		t.Errorf("did = true alongside an error; want false")
	}
	if exec.calls != 2 {
		t.Errorf("exec was called %d times, want exactly 2 (the clear, then the reset that failed)", exec.calls)
	}
}
