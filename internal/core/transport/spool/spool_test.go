package spool_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/core/transport/spool"
)

func testEnvelope(nodeID, collectorID string) transport.Envelope {
	return transport.NewEnvelope(
		transport.Origin{NodeID: nodeID, Hostname: "h"},
		collect.Batch{CollectorID: collectorID, Samples: []collect.Sample{
			{Metric: "m", Value: 1, Unit: collect.UnitCount, Kind: collect.KindGauge, Time: time.Now()},
		}},
	)
}

func TestEnqueuePeekDequeueRoundTrips(t *testing.T) {
	t.Parallel()

	s, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	env := testEnvelope("node-1", "system.cpu")
	if err := s.Enqueue(env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("Len = %d, want 1", s.Len())
	}

	got, ok, err := s.Peek()
	if err != nil || !ok {
		t.Fatalf("Peek: ok=%v err=%v", ok, err)
	}
	if got.Origin.NodeID != "node-1" {
		t.Errorf("peeked envelope node = %q", got.Origin.NodeID)
	}
	if s.Len() != 1 {
		t.Error("Peek must not remove the entry")
	}

	if err := s.Dequeue(); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if s.Len() != 0 {
		t.Errorf("Len after Dequeue = %d, want 0", s.Len())
	}
}

func TestPeekOnEmptySpoolReturnsFalse(t *testing.T) {
	t.Parallel()

	s, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	_, ok, err := s.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if ok {
		t.Error("Peek on an empty spool returned ok=true")
	}
}

func TestOrderIsFIFO(t *testing.T) {
	t.Parallel()

	s, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for i := range 5 {
		if err := s.Enqueue(testEnvelope("node-1", collectorName(i))); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	for i := range 5 {
		got, ok, err := s.Peek()
		if err != nil || !ok {
			t.Fatalf("Peek %d: ok=%v err=%v", i, ok, err)
		}
		if batchOf(t, got).CollectorID != collectorName(i) {
			t.Errorf("Peek %d = %q, want %q (FIFO order)", i, batchOf(t, got).CollectorID, collectorName(i))
		}
		if err := s.Dequeue(); err != nil {
			t.Fatalf("Dequeue %d: %v", i, err)
		}
	}
}

func collectorName(i int) string { return "collector-" + string(rune('a'+i)) }

func batchOf(t *testing.T, env transport.Envelope) collect.Batch {
	t.Helper()
	b, ok := transport.MetricsOf(env)
	if !ok {
		t.Fatal("envelope carries no metrics payload")
	}
	return b
}

// The property this whole package exists for: an outage must not lose
// observations. Overflow drops the *oldest* entries, not the newest — recent
// data is more valuable during a recovery.
func TestOverflowDropsOldestNotNewest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	env := testEnvelope("node-1", "system.cpu")
	data, _ := jsonSize(t, env)

	s, err := spool.Open(spool.Options{Dir: dir, MaxBytes: data * 3})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for i := range 5 {
		if err := s.Enqueue(testEnvelope("node-1", collectorName(i))); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	if s.Len() > 3 {
		t.Fatalf("Len = %d, want at most 3 given the byte budget", s.Len())
	}
	if s.Dropped() == 0 {
		t.Error("Dropped() = 0, want at least one eviction")
	}

	got, ok, err := s.Peek()
	if err != nil || !ok {
		t.Fatalf("Peek: ok=%v err=%v", ok, err)
	}
	if batchOf(t, got).CollectorID == collectorName(0) {
		t.Error("the oldest entry survived an overflow; it should have been dropped first")
	}
}

func TestReopenResumesQueuedEntries(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := spool.Open(spool.Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Enqueue(testEnvelope("node-1", "a")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := s.Enqueue(testEnvelope("node-1", "b")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	reopened, err := spool.Open(spool.Options{Dir: dir})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Len() != 2 {
		t.Fatalf("Len after reopen = %d, want 2 (a process restart must not lose spooled data)", reopened.Len())
	}

	got, _, err := reopened.Peek()
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if batchOf(t, got).CollectorID != "a" {
		t.Errorf("order not preserved across reopen: got %q, want a", batchOf(t, got).CollectorID)
	}
}

// An envelope older than MaxAge cannot be safely deduplicated by the control
// plane's idempotency window (migrations/0004_fleet.sql) if replayed, so it
// must be discarded on reopen rather than risked as a duplicate.
func TestReopenDiscardsEntriesOlderThanMaxAge(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s, err := spool.Open(spool.Options{Dir: dir})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Enqueue(testEnvelope("node-1", "old")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour)
	for _, e := range entries {
		if err := os.Chtimes(filepath.Join(dir, e.Name()), old, old); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}

	reopened, err := spool.Open(spool.Options{Dir: dir, MaxAge: time.Hour})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened.Len() != 0 {
		t.Errorf("Len after reopen past MaxAge = %d, want 0", reopened.Len())
	}
}

func TestPeekNAndDequeueNBatch(t *testing.T) {
	t.Parallel()

	s, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	for i := range 5 {
		if err := s.Enqueue(testEnvelope("node-1", collectorName(i))); err != nil {
			t.Fatalf("Enqueue %d: %v", i, err)
		}
	}

	batch, err := s.PeekN(3)
	if err != nil {
		t.Fatalf("PeekN: %v", err)
	}
	if len(batch) != 3 {
		t.Fatalf("PeekN returned %d, want 3", len(batch))
	}
	for i, env := range batch {
		if batchOf(t, env).CollectorID != collectorName(i) {
			t.Errorf("batch[%d] = %q, want %q", i, batchOf(t, env).CollectorID, collectorName(i))
		}
	}
	if s.Len() != 5 {
		t.Error("PeekN must not remove entries")
	}

	if err := s.DequeueN(3); err != nil {
		t.Fatalf("DequeueN: %v", err)
	}
	if s.Len() != 2 {
		t.Errorf("Len after DequeueN(3) = %d, want 2", s.Len())
	}

	remaining, err := s.PeekN(10)
	if err != nil {
		t.Fatalf("PeekN: %v", err)
	}
	if len(remaining) != 2 || batchOf(t, remaining[0]).CollectorID != collectorName(3) {
		t.Errorf("remaining entries not as expected: %+v", remaining)
	}
}

func TestPeekNBeyondLengthReturnsAvailable(t *testing.T) {
	t.Parallel()

	s, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Enqueue(testEnvelope("node-1", "a")); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	batch, err := s.PeekN(100)
	if err != nil {
		t.Fatalf("PeekN: %v", err)
	}
	if len(batch) != 1 {
		t.Errorf("PeekN(100) on a 1-entry spool = %d entries, want 1", len(batch))
	}
}

func TestDequeueOnEmptySpoolFails(t *testing.T) {
	t.Parallel()

	s, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Dequeue(); err == nil {
		t.Fatal("Dequeue on an empty spool succeeded")
	}
}

func jsonSize(t *testing.T, env transport.Envelope) (int64, error) {
	t.Helper()
	s, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Enqueue(env); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return s.Bytes(), nil
}
