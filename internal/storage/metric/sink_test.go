package metric_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/storage/metric"
)

// The ingest path is where collected observations are lost if anything goes
// wrong, and the failure mode is silent: a collector reads its source
// perfectly, the write fails, and a dashboard shows a series that has been
// empty for a day while every health indicator stays green. These tests pin
// the behaviour that prevents it — errors surface, and the counters record
// what was lost.

type fakeWriter struct {
	mu       sync.Mutex
	envelope []transport.Envelope
	err      error
}

func (f *fakeWriter) InsertBatch(_ context.Context, env transport.Envelope, _ collect.Batch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.envelope = append(f.envelope, env)
	return nil
}

func (f *fakeWriter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.envelope)
}

func testEnvelope(collectorID string, samples int) transport.Envelope {
	s := make([]collect.Sample, samples)
	for i := range s {
		s[i] = collect.Sample{
			Metric: "system.cpu.usage",
			Value:  float64(i),
			Unit:   collect.UnitPercent,
			Kind:   collect.KindGauge,
			Time:   time.Now(),
		}
	}
	return transport.Envelope{
		Origin:  transport.Origin{NodeID: "node-1", Hostname: "host-1"},
		Payload: transport.Metrics{Batch: collect.Batch{CollectorID: collectorID, Samples: s}},
	}
}

func TestSinkCountsEnvelopesAndSamples(t *testing.T) {
	t.Parallel()

	w := &fakeWriter{}
	sink := metric.NewSink(w, nil)

	if err := sink.Receive(context.Background(), testEnvelope("system.cpu", 3)); err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if err := sink.Receive(context.Background(), testEnvelope("system.memory", 2)); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	got := sink.Stats()
	if got.Envelopes != 2 {
		t.Errorf("envelopes = %d, want 2", got.Envelopes)
	}
	if got.Samples != 5 {
		t.Errorf("samples = %d, want 5", got.Samples)
	}
	if got.Failed != 0 {
		t.Errorf("failed = %d, want 0", got.Failed)
	}
	if w.count() != 2 {
		t.Errorf("writer saw %d envelopes, want 2", w.count())
	}
}

// A failed write must return an error so the scheduler marks the run failed
// and publishes it. Swallowing it is what produces a healthy-looking collector
// with no data behind it.
func TestSinkSurfacesWriteFailures(t *testing.T) {
	t.Parallel()

	w := &fakeWriter{err: errors.New("connection refused")}
	sink := metric.NewSink(w, nil)

	err := sink.Receive(context.Background(), testEnvelope("system.cpu", 4))
	if err == nil {
		t.Fatal("a failed write returned nil; the collector would be reported healthy")
	}

	var apiErr *errs.Error
	if !errs.As(err, &apiErr) {
		t.Fatalf("error is not typed: %T", err)
	}
	// The collector id is what turns "storage failed" into "this collector's
	// data is missing", which is the actionable form.
	if apiErr.Details["collector_id"] != "system.cpu" {
		t.Errorf("details.collector_id = %v, want system.cpu", apiErr.Details["collector_id"])
	}
}

// Samples from a failed envelope must not be counted as written. The sample
// counter is the evidence for "observations were collected and stored", and
// inflating it hides exactly the loss this path is meant to expose.
func TestSinkDoesNotCountSamplesFromFailedWrites(t *testing.T) {
	t.Parallel()

	w := &fakeWriter{err: errors.New("disk full")}
	sink := metric.NewSink(w, nil)

	_ = sink.Receive(context.Background(), testEnvelope("system.disk", 7))

	got := sink.Stats()
	if got.Envelopes != 1 {
		t.Errorf("envelopes = %d, want 1 — the attempt still happened", got.Envelopes)
	}
	if got.Samples != 0 {
		t.Errorf("samples = %d, want 0 — nothing was written", got.Samples)
	}
	if got.Failed != 1 {
		t.Errorf("failed = %d, want 1", got.Failed)
	}
}

// The scheduler runs collectors concurrently, so the sink is written to from
// several goroutines at once. Counter corruption here would silently
// misreport how much data landed.
func TestSinkCountersAreConcurrencySafe(t *testing.T) {
	t.Parallel()

	w := &fakeWriter{}
	sink := metric.NewSink(w, nil)

	const workers, each = 8, 50
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				_ = sink.Receive(context.Background(), testEnvelope("system.cpu", 2))
			}
		}()
	}
	wg.Wait()

	got := sink.Stats()
	if got.Envelopes != workers*each {
		t.Errorf("envelopes = %d, want %d", got.Envelopes, workers*each)
	}
	if got.Samples != workers*each*2 {
		t.Errorf("samples = %d, want %d", got.Samples, workers*each*2)
	}
}

// The sink is the receiving end of the transport seam (ADR-0005). It must
// satisfy transport.Sink so an agent pushing over a network is the same code
// path as an in-process collector.
func TestSinkSatisfiesTransportSink(t *testing.T) {
	t.Parallel()

	var _ transport.Sink = metric.NewSink(&fakeWriter{}, nil)
}
