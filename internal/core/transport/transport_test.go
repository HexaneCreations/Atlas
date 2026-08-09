package transport_test

import (
	"context"

	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
)

func testOrigin() transport.Origin {
	return transport.Origin{NodeID: "node-1", Hostname: "atlas-01", AgentVersion: "0.1.0"}
}

func testBatch() collect.Batch {
	now := time.Now()
	return collect.Batch{
		CollectorID: "system.cpu",
		StartedAt:   now,
		CompletedAt: now.Add(time.Millisecond),
		Samples: []collect.Sample{{
			Metric: "system.cpu.usage", Value: 12.5,
			Unit: collect.UnitPercent, Kind: collect.KindGauge, Time: now,
		}},
	}
}

// recordingSink captures what a transport delivers.
type recordingSink struct {
	mu       sync.Mutex
	received []transport.Envelope
	err      error
	blockFor time.Duration
}

func (s *recordingSink) Receive(ctx context.Context, env transport.Envelope) error {
	if s.blockFor > 0 {
		select {
		case <-time.After(s.blockFor):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.received = append(s.received, env)
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.received)
}

func TestInProcessDeliversToSink(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	tr := transport.NewInProcess(sink)
	defer tr.Close()

	env := transport.NewEnvelope(testOrigin(), testBatch())
	if err := tr.Send(context.Background(), env); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if sink.count() != 1 {
		t.Fatalf("sink received %d envelopes, want 1", sink.count())
	}
	got := sink.received[0]
	gotBatch, ok := transport.MetricsOf(got)
	if !ok {
		t.Fatal("MetricsOf() = false, want a metrics payload")
	}
	if gotBatch.CollectorID != "system.cpu" {
		t.Errorf("CollectorID = %q", gotBatch.CollectorID)
	}
	if got.Origin.NodeID != "node-1" {
		t.Errorf("Origin.NodeID = %q, want node-1", got.Origin.NodeID)
	}
	if got.ID == "" {
		t.Error("envelope has no ID")
	}
	if stats := tr.Stats(); stats.Sent != 1 || stats.Failed != 0 {
		t.Errorf("Stats() = %+v, want 1 sent 0 failed", stats)
	}
}

func TestNewEnvelopeAssignsIdentity(t *testing.T) {
	t.Parallel()

	a := transport.NewEnvelope(testOrigin(), testBatch())
	b := transport.NewEnvelope(testOrigin(), testBatch())

	if a.ID == "" || b.ID == "" {
		t.Fatal("NewEnvelope did not assign an ID")
	}
	if a.ID == b.ID {
		t.Error("NewEnvelope produced duplicate IDs")
	}
	if a.SentAt.IsZero() {
		t.Error("NewEnvelope did not set SentAt")
	}
}

// A malformed observation is far cheaper to refuse here than to find and
// remove from a time-series store later.
func TestInProcessRejectsInvalidEnvelopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  transport.Envelope
	}{
		{
			name: "no node ID",
			env:  transport.Envelope{Origin: transport.Origin{}, Payload: transport.Metrics{Batch: testBatch()}},
		},
		{
			name: "no collector ID",
			env:  transport.Envelope{Origin: testOrigin(), Payload: transport.Metrics{}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sink := &recordingSink{}
			tr := transport.NewInProcess(sink)
			defer tr.Close()

			err := tr.Send(context.Background(), tt.env)
			if err == nil {
				t.Fatal("Send() accepted an invalid envelope")
			}
			if got := errs.CodeOf(err); got != errs.CodeInvalidArgument {
				t.Errorf("code = %q, want invalid_argument", got)
			}
			if sink.count() != 0 {
				t.Error("an invalid envelope reached the sink")
			}
			if tr.Stats().Rejected != 1 {
				t.Errorf("Rejected = %d, want 1", tr.Stats().Rejected)
			}
		})
	}
}

func TestInProcessAssignsMissingEnvelopeID(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	tr := transport.NewInProcess(sink)
	defer tr.Close()

	err := tr.Send(context.Background(), transport.Envelope{
		Origin:  testOrigin(),
		Payload: transport.Metrics{Batch: testBatch()},
	})
	if err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if sink.received[0].ID == "" {
		t.Error("Send did not assign an ID to an envelope that lacked one")
	}
}

func TestInProcessPropagatesSinkFailure(t *testing.T) {
	t.Parallel()

	sinkErr := errs.New(errs.CodeUnavailable, "storage is down")
	tr := transport.NewInProcess(&recordingSink{err: sinkErr})
	defer tr.Close()

	err := tr.Send(context.Background(), transport.NewEnvelope(testOrigin(), testBatch()))
	if err == nil {
		t.Fatal("Send() hid a sink failure")
	}
	// The code must survive the wrap, or the scheduler cannot tell a
	// transient outage from a permanent rejection.
	if got := errs.CodeOf(err); got != errs.CodeUnavailable {
		t.Errorf("code = %q, want unavailable", got)
	}
	if tr.Stats().Failed != 1 {
		t.Errorf("Failed = %d, want 1", tr.Stats().Failed)
	}
}

func TestInProcessHonoursCancelledContext(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	tr := transport.NewInProcess(sink)
	defer tr.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := tr.Send(ctx, transport.NewEnvelope(testOrigin(), testBatch())); err == nil {
		t.Fatal("Send() ignored a cancelled context")
	}
	if sink.count() != 0 {
		t.Error("a cancelled send still reached the sink")
	}
}

func TestInProcessRefusesSendAfterClose(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	tr := transport.NewInProcess(sink)

	if err := tr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}

	err := tr.Send(context.Background(), transport.NewEnvelope(testOrigin(), testBatch()))
	if err == nil {
		t.Fatal("Send() succeeded after Close")
	}
	if got := errs.CodeOf(err); got != errs.CodeUnavailable {
		t.Errorf("code = %q, want unavailable", got)
	}
	if sink.count() != 0 {
		t.Error("an envelope reached the sink after Close")
	}
}

func TestSinkFuncAdapter(t *testing.T) {
	t.Parallel()

	var got transport.Envelope
	sink := transport.SinkFunc(func(_ context.Context, env transport.Envelope) error {
		got = env
		return nil
	})

	tr := transport.NewInProcess(sink)
	defer tr.Close()

	if err := tr.Send(context.Background(), transport.NewEnvelope(testOrigin(), testBatch())); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	gotBatch, ok := transport.MetricsOf(got)
	if !ok || gotBatch.CollectorID != "system.cpu" {
		t.Errorf("SinkFunc received %+v", got)
	}
}

func TestInProcessConcurrentSends(t *testing.T) {
	t.Parallel()

	sink := &recordingSink{}
	tr := transport.NewInProcess(sink)
	defer tr.Close()

	const senders, each = 8, 50
	var wg sync.WaitGroup
	for range senders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if err := tr.Send(context.Background(), transport.NewEnvelope(testOrigin(), testBatch())); err != nil {
					t.Errorf("Send() error = %v", err)
				}
			}
		}()
	}
	wg.Wait()

	if got, want := sink.count(), senders*each; got != want {
		t.Errorf("sink received %d envelopes, want %d", got, want)
	}
	if got := tr.Stats().Sent; got != uint64(senders*each) {
		t.Errorf("Stats().Sent = %d, want %d", got, senders*each)
	}
}

func TestOriginAndEnvelopeValidation(t *testing.T) {
	t.Parallel()

	if err := (transport.Origin{}).Validate(); err == nil {
		t.Error("an origin with no node ID should be invalid")
	}
	if err := testOrigin().Validate(); err != nil {
		t.Errorf("a complete origin should be valid, got %v", err)
	}

	valid := transport.Envelope{Origin: testOrigin(), Payload: transport.Metrics{Batch: testBatch()}}
	if err := valid.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

// The Transport interface is the whole point of the package: an alternative
// implementation must be substitutable without touching callers.
func TestTransportInterfaceIsSubstitutable(t *testing.T) {
	t.Parallel()

	var tr transport.Transport = &countingTransport{}
	if err := tr.Send(context.Background(), transport.NewEnvelope(testOrigin(), testBatch())); err != nil {
		t.Fatalf("Send() error = %v", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if tr.(*countingTransport).sends != 1 {
		t.Error("substituted transport did not receive the send")
	}
}

type countingTransport struct{ sends int }

func (c *countingTransport) Send(context.Context, transport.Envelope) error {
	c.sends++
	return nil
}

func (c *countingTransport) Close() error { return nil }
