package scheduler_test

import (
	"context"
	stderrors "errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/scheduler"
	"github.com/hexane/atlas/internal/core/transport"
)

// fakeStreamer is a controllable Streamer.
type fakeStreamer struct {
	id     string
	starts atomic.Int64
	sent   atomic.Int64

	// emit is how many samples to send before behaving per the fields below.
	emit int
	// endAfter ends the stream once emit samples have been sent.
	endAfter bool
	err      error
	panicNow bool
	// blockForever keeps the stream open until its context is cancelled,
	// which is what a healthy event feed does.
	blockForever bool
}

func (f *fakeStreamer) Descriptor() collect.Descriptor {
	return collect.Descriptor{ID: f.id, Name: f.id, Description: "test streamer"}
}

func (f *fakeStreamer) Stream(ctx context.Context, out chan<- collect.Sample) error {
	f.starts.Add(1)

	if f.panicNow {
		panic("simulated streamer bug")
	}

	for i := 0; i < f.emit; i++ {
		select {
		case out <- collect.Sample{
			Metric: "stream.event", Value: float64(i),
			Unit: collect.UnitCount, Kind: collect.KindCounter, Time: time.Now(),
		}:
			f.sent.Add(1)
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if f.err != nil {
		return f.err
	}
	if f.endAfter {
		return nil
	}
	if f.blockForever {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func newStreamHarness(t *testing.T, streamers ...collect.Streamer) *harness {
	t.Helper()

	h := newHarness(t, fastConfig())
	for _, s := range streamers {
		if err := h.reg.RegisterStreamer(s); err != nil {
			t.Fatalf("RegisterStreamer() error = %v", err)
		}
	}
	return h
}

func TestStreamerSamplesReachTheTransport(t *testing.T) {
	t.Parallel()

	st := &fakeStreamer{id: "docker.events", emit: 600, blockForever: true}
	h := newStreamHarness(t, st)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	// 600 samples exceeds the 500 batch cap, so delivery happens on size
	// rather than waiting for the flush timer.
	waitFor(t, "a batch to be delivered", func() bool { return h.sink.count() >= 1 })

	env := h.sink.all()[0]
	batch, ok := transport.MetricsOf(env)
	if !ok {
		t.Fatal("delivered envelope carries no metrics payload")
	}
	if batch.CollectorID != "docker.events" {
		t.Errorf("CollectorID = %q", batch.CollectorID)
	}
	if len(batch.Samples) == 0 {
		t.Error("delivered an empty batch")
	}
	if len(batch.Samples) > 500 {
		t.Errorf("batch of %d samples exceeds the cap", len(batch.Samples))
	}
	if env.Origin.NodeID != "node-1" {
		t.Errorf("Origin.NodeID = %q", env.Origin.NodeID)
	}
}

// A quiet stream must not have its samples held indefinitely waiting for a
// batch to fill.
func TestStreamerFlushesOnTimeEvenWhenQuiet(t *testing.T) {
	t.Parallel()

	st := &fakeStreamer{id: "quiet.stream", emit: 3, blockForever: true}
	h := newStreamHarness(t, st)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Well under the batch cap; only the flush timer can deliver these.
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) && h.sink.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if h.sink.count() == 0 {
		t.Fatal("a quiet stream's samples were never flushed")
	}
	flushed, ok := transport.MetricsOf(h.sink.all()[0])
	if !ok {
		t.Fatal("flushed envelope carries no metrics payload")
	}
	if got := len(flushed.Samples); got != 3 {
		t.Errorf("flushed %d samples, want 3", got)
	}
}

// A daemon restart ends the stream. The supervisor must reconnect, or Atlas
// silently stops observing after the first Docker upgrade.
func TestEndedStreamIsRestarted(t *testing.T) {
	t.Parallel()

	st := &fakeStreamer{id: "flaky.stream", emit: 1, endAfter: true}
	h := newStreamHarness(t, st)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the stream to be restarted", func() bool { return st.starts.Load() >= 2 })
}

func TestFailingStreamIsRestartedAndReportedUnhealthy(t *testing.T) {
	t.Parallel()

	st := &fakeStreamer{id: "broken.stream", err: stderrors.New("docker socket closed")}
	h := newStreamHarness(t, st)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "restarts after failure", func() bool { return st.starts.Load() >= 2 })
	waitFor(t, "the failure to be recorded", func() bool {
		for _, hc := range h.sched.Health() {
			if hc.CollectorID == "broken.stream" && hc.ConsecutiveFailures > 0 {
				return true
			}
		}
		return false
	})

	for _, hc := range h.sched.Health() {
		if hc.CollectorID == "broken.stream" {
			if hc.Healthy {
				t.Error("a continuously failing stream is reported healthy")
			}
			if hc.LastError == "" {
				t.Error("no last error was recorded")
			}
		}
	}
}

// A panic in a streamer must not take down the process or the other streams.
// This is the guarantee an unmanaged goroutine in Plugin.Init would not have.
func TestPanickingStreamerIsIsolatedAndRestarted(t *testing.T) {
	t.Parallel()

	bad := &fakeStreamer{id: "bad.stream", panicNow: true}
	good := &fakeStreamer{id: "good.stream", emit: 600, blockForever: true}
	h := newStreamHarness(t, bad, good)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	waitFor(t, "the healthy stream to deliver", func() bool { return h.sink.count() >= 1 })
	waitFor(t, "the panicking stream to be retried", func() bool { return bad.starts.Load() >= 2 })

	for _, hc := range h.sched.Health() {
		if hc.CollectorID == "good.stream" && !hc.Healthy {
			t.Error("a panic in one stream affected another")
		}
	}
}

// Streamers and collectors share one ID namespace; a collision would make it
// ambiguous which produced a series.
func TestStreamerAndCollectorIDsCannotCollide(t *testing.T) {
	t.Parallel()

	reg := collect.NewRegistry()
	if err := reg.Register(&fakeCollector{desc: collect.Descriptor{ID: "shared.id"}}); err != nil {
		t.Fatal(err)
	}
	if err := reg.RegisterStreamer(&fakeStreamer{id: "shared.id"}); err == nil {
		t.Error("a streamer took a collector's ID")
	}

	reg2 := collect.NewRegistry()
	if err := reg2.RegisterStreamer(&fakeStreamer{id: "other.id"}); err != nil {
		t.Fatal(err)
	}
	if err := reg2.Register(&fakeCollector{desc: collect.Descriptor{ID: "other.id"}}); err == nil {
		t.Error("a collector took a streamer's ID")
	}
	if err := reg2.RegisterStreamer(&fakeStreamer{id: "other.id"}); err == nil {
		t.Error("a duplicate streamer ID was accepted")
	}
}

// Streamers must appear in health, or a failing event feed is invisible.
func TestStreamersAppearInHealthAsContinuous(t *testing.T) {
	t.Parallel()

	st := &fakeStreamer{id: "docker.events", blockForever: true}
	h := newStreamHarness(t, st)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, hc := range h.sched.Health() {
		if hc.CollectorID == "docker.events" {
			found = true
			if !hc.Streaming {
				t.Error("Streaming = false for a streamer")
			}
			if hc.Interval != "continuous" {
				t.Errorf("Interval = %q, want continuous", hc.Interval)
			}
		}
	}
	if !found {
		t.Error("the streamer is missing from Health()")
	}
	if got := h.sched.Stats().Collectors; got < 1 {
		t.Errorf("Stats().Collectors = %d, want the streamer counted", got)
	}
}

// Shutdown must be bounded and must not discard already-collected samples.
func TestStopDrainsStreamersPromptly(t *testing.T) {
	t.Parallel()

	st := &fakeStreamer{id: "draining.stream", emit: 5, blockForever: true}
	h := newStreamHarness(t, st)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the stream to start", func() bool { return st.starts.Load() >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	if err := h.sched.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("Stop took %v; streamer shutdown is not bounded", elapsed)
	}

	// Samples collected before shutdown must have been flushed, not dropped.
	if h.sink.count() == 0 {
		t.Error("samples collected before shutdown were discarded")
	}
}

// Streamed samples go through the same cardinality guard as polled ones, or a
// log tail labelling by container id bypasses the datastore's only defence.
func TestStreamedSamplesAreCardinalityLimited(t *testing.T) {
	t.Parallel()

	st := &runawayStreamer{id: "runaway.stream"}
	cfg := fastConfig()
	cfg.MaxSeriesPerCollector = 20
	h := newHarness(t, cfg)
	if err := h.reg.RegisterStreamer(st); err != nil {
		t.Fatal(err)
	}

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a delivery", func() bool { return h.sink.count() >= 1 })

	if got := h.sched.Stats().RefusedSeries; got == 0 {
		t.Error("a runaway streamer was not limited")
	}
	for _, env := range h.sink.all() {
		batch, ok := transport.MetricsOf(env)
		if !ok {
			t.Fatal("delivered envelope carries no metrics payload")
		}
		if len(batch.Samples) > 20 {
			t.Errorf("a batch carried %d samples, exceeding the series budget", len(batch.Samples))
		}
	}
}

// runawayStreamer emits an unbounded number of distinct series, the classic
// plugin bug.
type runawayStreamer struct{ id string }

func (r *runawayStreamer) Descriptor() collect.Descriptor {
	return collect.Descriptor{ID: r.id, Name: r.id}
}

func (r *runawayStreamer) Stream(ctx context.Context, out chan<- collect.Sample) error {
	for i := 0; ; i++ {
		select {
		case out <- collect.Sample{
			Metric: "log.line", Value: 1,
			Unit: collect.UnitCount, Kind: collect.KindCounter, Time: time.Now(),
			Labels: map[string]string{"request_id": string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260%10))},
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func TestStreamerRegisteredButSchedulerNotStartedIsSafe(t *testing.T) {
	t.Parallel()

	h := newStreamHarness(t, &fakeStreamer{id: "unused.stream"})
	if err := h.sched.Stop(context.Background()); err != nil {
		t.Errorf("Stop() before Start error = %v", err)
	}
}

var _ = scheduler.ErrStreamPanic
