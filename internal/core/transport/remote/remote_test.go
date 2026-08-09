package remote_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/core/transport/remote"
	"github.com/hexane/atlas/internal/core/transport/spool"
)

func metricEnvelope(nodeID, collectorID string) transport.Envelope {
	return transport.NewEnvelope(
		transport.Origin{NodeID: nodeID, Hostname: "h"},
		collect.Batch{CollectorID: collectorID, Samples: []collect.Sample{
			{Metric: "m", Value: 1, Unit: collect.UnitCount, Kind: collect.KindGauge, Time: time.Now()},
		}},
	)
}

func snapshotEnvelope(nodeID string) transport.Envelope {
	return transport.NewEnvelopeOf(
		transport.Origin{NodeID: nodeID, Hostname: "h"},
		coreinventory.Payload{Subject: coreinventory.SubjectContainers, ObservedAt: time.Now(), ContentHash: "h", Data: []byte(`[]`)},
	)
}

type recording struct {
	mu       sync.Mutex
	requests [][]transport.Envelope
	fail     atomic.Int32 // fail N more requests, then succeed
	response func([]transport.Envelope) (int, string, int)
}

func newTestServer(t *testing.T, rec *recording) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Envelopes []transport.Envelope `json:"envelopes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		if rec.fail.Load() > 0 {
			rec.fail.Add(-1)
			http.Error(w, "simulated failure", http.StatusServiceUnavailable)
			return
		}

		rec.mu.Lock()
		rec.requests = append(rec.requests, req.Envelopes)
		rec.mu.Unlock()

		accepted, directive, retryMs := len(req.Envelopes), "ok", 0
		if rec.response != nil {
			accepted, directive, retryMs = rec.response(req.Envelopes)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accepted": accepted, "rejected": []any{},
			"directive": directive, "retry_after_ms": retryMs,
		})
	}))
}

func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

func TestStreamEnvelopeIsSpooledAndDelivered(t *testing.T) {
	t.Parallel()

	rec := &recording{}
	srv := newTestServer(t, rec)
	defer srv.Close()

	sp, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	tr, err := remote.New(remote.Options{BaseURL: srv.URL, Spool: sp})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer tr.Close()

	if err := tr.Send(context.Background(), metricEnvelope("node-1", "system.cpu")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	waitFor(t, "delivery", func() bool { return tr.Stats().Sent >= 1 })
	if sp.Len() != 0 {
		t.Errorf("spool still has %d entries after successful delivery", sp.Len())
	}
}

func TestSnapshotEnvelopeIsNeverSpooled(t *testing.T) {
	t.Parallel()

	rec := &recording{}
	srv := newTestServer(t, rec)
	defer srv.Close()

	sp, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	tr, err := remote.New(remote.Options{BaseURL: srv.URL, Spool: sp})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer tr.Close()

	if err := tr.Send(context.Background(), snapshotEnvelope("node-1")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sp.Len() != 0 {
		t.Error("a snapshot envelope was spooled")
	}
}

func TestSnapshotEnvelopeFailureIsDroppedNotRetried(t *testing.T) {
	t.Parallel()

	rec := &recording{}
	rec.fail.Store(100) // every request fails
	srv := newTestServer(t, rec)
	defer srv.Close()

	sp, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	tr, err := remote.New(remote.Options{BaseURL: srv.URL, Spool: sp})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer tr.Close()

	err = tr.Send(context.Background(), snapshotEnvelope("node-1"))
	if err == nil {
		t.Fatal("Send succeeded despite server failure")
	}
	if sp.Len() != 0 {
		t.Error("a failed snapshot was spooled for retry; it should have been dropped")
	}
}

func TestOutageThenRecoveryReplaysSpooledEnvelopes(t *testing.T) {
	t.Parallel()

	rec := &recording{}
	rec.fail.Store(2)
	srv := newTestServer(t, rec)
	defer srv.Close()

	sp, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	tr, err := remote.New(remote.Options{BaseURL: srv.URL, Spool: sp})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer tr.Close()

	for i := range 3 {
		if err := tr.Send(context.Background(), metricEnvelope("node-1", collectorName(i))); err != nil {
			t.Fatalf("Send %d: %v", i, err)
		}
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && tr.Stats().Sent < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	if tr.Stats().Sent < 3 {
		t.Fatalf("Sent = %d after outage recovery, want 3", tr.Stats().Sent)
	}
	if sp.Len() != 0 {
		t.Errorf("spool has %d entries after recovery, want 0", sp.Len())
	}
}

func TestMultipleSendsBatchIntoOneRequest(t *testing.T) {
	t.Parallel()

	rec := &recording{}
	srv := newTestServer(t, rec)
	defer srv.Close()

	sp, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	for i := range 5 {
		if err := sp.Enqueue(metricEnvelope("node-1", collectorName(i))); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	tr, err := remote.New(remote.Options{BaseURL: srv.URL, Spool: sp})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer tr.Close()

	waitFor(t, "delivery", func() bool { return tr.Stats().Sent >= 5 })

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.requests) != 1 {
		t.Errorf("requests = %d, want 1 batched request for 5 pre-spooled envelopes", len(rec.requests))
	}
}

func TestBackpressureDirectivePausesReplay(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	rec := &recording{}
	rec.response = func(envs []transport.Envelope) (int, string, int) {
		n := calls.Add(1)
		if n == 1 {
			return len(envs), "slow_down", 300
		}
		return len(envs), "ok", 0
	}
	srv := newTestServer(t, rec)
	defer srv.Close()

	sp, err := spool.Open(spool.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("spool.Open: %v", err)
	}
	for i := range 2 {
		if err := sp.Enqueue(metricEnvelope("node-1", collectorName(i))); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}

	tr, err := remote.New(remote.Options{BaseURL: srv.URL, Spool: sp})
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	defer tr.Close()

	start := time.Now()
	waitFor(t, "delivery after backpressure pause", func() bool { return sp.Len() == 0 })
	if time.Since(start) < 250*time.Millisecond {
		t.Error("replay did not honour the slow_down directive's retry_after_ms")
	}
}

func collectorName(i int) string { return string(rune('a' + i)) }
