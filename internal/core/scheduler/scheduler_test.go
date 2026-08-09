package scheduler_test

import (
	"context"
	stderrors "errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/scheduler"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// fakeCollector is a controllable Collector for scheduler tests.
type fakeCollector struct {
	desc     collect.Descriptor
	runs     atomic.Int64
	inFlight atomic.Int64
	maxSeen  atomic.Int64

	mu       sync.Mutex
	samples  []collect.Sample
	err      error
	panicNow bool
	blockFor time.Duration
}

func (f *fakeCollector) Descriptor() collect.Descriptor { return f.desc }

func (f *fakeCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	f.runs.Add(1)

	n := f.inFlight.Add(1)
	defer f.inFlight.Add(-1)
	for {
		max := f.maxSeen.Load()
		if n <= max || f.maxSeen.CompareAndSwap(max, n) {
			break
		}
	}

	f.mu.Lock()
	block, shouldPanic, err, samples := f.blockFor, f.panicNow, f.err, f.samples
	f.mu.Unlock()

	if shouldPanic {
		panic("simulated collector bug")
	}
	if block > 0 {
		select {
		case <-time.After(block):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return samples, err
}

func (f *fakeCollector) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func sample() collect.Sample {
	return collect.Sample{
		Metric: "test.metric", Value: 1,
		Unit: collect.UnitCount, Kind: collect.KindGauge, Time: time.Now(),
	}
}

type capturingSink struct {
	mu        sync.Mutex
	envelopes []transport.Envelope
}

func (s *capturingSink) Receive(_ context.Context, env transport.Envelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.envelopes = append(s.envelopes, env)
	return nil
}

func (s *capturingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.envelopes)
}

func (s *capturingSink) all() []transport.Envelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]transport.Envelope(nil), s.envelopes...)
}

// harness wires a scheduler with fast intervals and no jitter, so tests are
// quick and deterministic.
type harness struct {
	sched *scheduler.Scheduler
	sink  *capturingSink
	reg   *collect.Registry
	bus   *eventbus.Bus
}

func newHarness(t *testing.T, cfg config.Collection, collectors ...collect.Collector) *harness {
	t.Helper()

	reg := collect.NewRegistry()
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			t.Fatalf("Register() error = %v", err)
		}
	}

	sink := &capturingSink{}
	tr := transport.NewInProcess(sink)
	bus := eventbus.New(eventbus.Options{BufferSize: 64})

	s, err := scheduler.New(scheduler.Options{
		Registry:  reg,
		Transport: tr,
		Origin:    transport.Origin{NodeID: "node-1", Hostname: "test", AgentVersion: "test"},
		Bus:       bus,
		Config:    cfg,
		// No jitter: tests assert on run counts within tight windows.
		Jitter: func(time.Duration) time.Duration { return 0 },
	})
	if err != nil {
		t.Fatalf("scheduler.New() error = %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.Stop(ctx)
		_ = tr.Close()
		_ = bus.Close()
	})

	return &harness{sched: s, sink: sink, reg: reg, bus: bus}
}

func fastConfig() config.Collection {
	return config.Collection{
		DefaultInterval: 20 * time.Millisecond,
		Timeout:         15 * time.Millisecond,
		MaxConcurrent:   8,
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestSchedulerRunsCollectorsAndDelivers(t *testing.T) {
	t.Parallel()

	c := &fakeCollector{
		desc:    collect.Descriptor{ID: "test.metric", Name: "Test"},
		samples: []collect.Sample{sample(), sample()},
	}
	h := newHarness(t, fastConfig(), c)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	waitFor(t, "three deliveries", func() bool { return h.sink.count() >= 3 })

	env := h.sink.all()[0]
	batch, ok := transport.MetricsOf(env)
	if !ok {
		t.Fatal("delivered envelope carries no metrics payload")
	}
	if batch.CollectorID != "test.metric" {
		t.Errorf("CollectorID = %q", batch.CollectorID)
	}
	if len(batch.Samples) != 2 {
		t.Errorf("delivered %d samples, want 2", len(batch.Samples))
	}
	if env.Origin.NodeID != "node-1" {
		t.Errorf("Origin.NodeID = %q", env.Origin.NodeID)
	}
	if batch.StartedAt.IsZero() || batch.CompletedAt.IsZero() {
		t.Error("batch is missing run timestamps")
	}
	if stats := h.sched.Stats(); stats.Samples < 6 {
		t.Errorf("Stats().Samples = %d, want at least 6", stats.Samples)
	}
}

// An empty registry is a legitimate state — no plugin detected its subject.
func TestSchedulerStartsWithNoCollectors(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fastConfig())
	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatalf("Start() with an empty registry error = %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.sched.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}
}

// A collector blocked on a wedged mount must be cancelled, not waited on.
func TestSlowCollectorIsCancelledAtItsTimeout(t *testing.T) {
	t.Parallel()

	c := &fakeCollector{
		desc:     collect.Descriptor{ID: "slow.collector"},
		blockFor: time.Hour,
	}
	h := newHarness(t, fastConfig(), c)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "two attempted runs", func() bool { return c.runs.Load() >= 2 })

	// It keeps being retried rather than wedging the loop forever, and every
	// run is recorded as a failure.
	waitFor(t, "failures recorded", func() bool { return h.sched.Stats().Failures >= 2 })

	health := h.sched.Health()
	if len(health) != 1 {
		t.Fatalf("Health() returned %d entries, want 1", len(health))
	}
	if health[0].Healthy {
		t.Error("a collector that times out every run should not be healthy")
	}
	if health[0].ConsecutiveFailures < 2 {
		t.Errorf("ConsecutiveFailures = %d, want at least 2", health[0].ConsecutiveFailures)
	}
}

// Overlapping runs would let a slow collector accumulate goroutines without
// bound.
func TestRunsOfOneCollectorNeverOverlap(t *testing.T) {
	t.Parallel()

	c := &fakeCollector{
		desc:     collect.Descriptor{ID: "overlap.test"},
		blockFor: 30 * time.Millisecond, // longer than the 20ms interval
	}
	cfg := fastConfig()
	cfg.Timeout = 100 * time.Millisecond // allow the run to complete
	h := newHarness(t, cfg, c)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "several runs", func() bool { return c.runs.Load() >= 4 })

	if got := c.maxSeen.Load(); got > 1 {
		t.Errorf("observed %d concurrent runs of one collector, want at most 1", got)
	}
}

func TestConcurrencyCeilingIsEnforced(t *testing.T) {
	t.Parallel()

	const collectors, ceiling = 10, 3

	shared := &struct {
		inFlight atomic.Int64
		maxSeen  atomic.Int64
	}{}

	cfg := fastConfig()
	cfg.MaxConcurrent = ceiling
	cfg.Timeout = 100 * time.Millisecond

	list := make([]collect.Collector, 0, collectors)
	for i := range collectors {
		list = append(list, &countingConcurrency{
			id:     "c" + string(rune('a'+i)),
			shared: shared,
			block:  20 * time.Millisecond,
		})
	}
	h := newHarness(t, cfg, list...)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "runs to accumulate", func() bool { return h.sched.Stats().Runs >= 20 })

	if got := shared.maxSeen.Load(); got > ceiling {
		t.Errorf("observed %d concurrent collections, want at most %d", got, ceiling)
	}
}

type countingConcurrency struct {
	id     string
	block  time.Duration
	shared *struct {
		inFlight atomic.Int64
		maxSeen  atomic.Int64
	}
}

func (c *countingConcurrency) Descriptor() collect.Descriptor {
	return collect.Descriptor{ID: c.id}
}

func (c *countingConcurrency) Collect(ctx context.Context) ([]collect.Sample, error) {
	n := c.shared.inFlight.Add(1)
	defer c.shared.inFlight.Add(-1)
	for {
		max := c.shared.maxSeen.Load()
		if n <= max || c.shared.maxSeen.CompareAndSwap(max, n) {
			break
		}
	}
	select {
	case <-time.After(c.block):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return []collect.Sample{sample()}, nil
}

// A bug in one collector must not stop the others or the process.
func TestPanickingCollectorIsIsolated(t *testing.T) {
	t.Parallel()

	bad := &fakeCollector{desc: collect.Descriptor{ID: "bad.collector"}, panicNow: true}
	good := &fakeCollector{
		desc:    collect.Descriptor{ID: "good.collector"},
		samples: []collect.Sample{sample()},
	}
	h := newHarness(t, fastConfig(), bad, good)

	sub, err := h.bus.Subscribe("test", "collector.run.**")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}

	// The healthy collector keeps delivering.
	waitFor(t, "good collector deliveries", func() bool { return h.sink.count() >= 3 })
	// The panicking one keeps being retried.
	waitFor(t, "bad collector retries", func() bool { return bad.runs.Load() >= 2 })

	var sawPanic bool
	for range 10 {
		select {
		case e := <-sub.C:
			if e.Topic == scheduler.TopicRunPanicked {
				sawPanic = true
				if e.Subject != "bad.collector" {
					t.Errorf("panic event subject = %q, want bad.collector", e.Subject)
				}
			}
		case <-time.After(200 * time.Millisecond):
		}
		if sawPanic {
			break
		}
	}
	if !sawPanic {
		t.Error("a collector panic did not publish collector.run.panicked")
	}

	for _, hc := range h.sched.Health() {
		if hc.CollectorID == "good.collector" && !hc.Healthy {
			t.Error("the healthy collector was marked unhealthy")
		}
		if hc.CollectorID == "bad.collector" && hc.Healthy {
			t.Error("the panicking collector was marked healthy")
		}
	}
}

// Partial data from a partly broken collector beats no data, and the bad
// samples must not reach storage.
func TestInvalidSamplesAreDroppedButValidOnesDelivered(t *testing.T) {
	t.Parallel()

	c := &fakeCollector{
		desc: collect.Descriptor{ID: "mixed.collector"},
		samples: []collect.Sample{
			sample(),
			{Metric: "", Value: 1, Unit: collect.UnitCount, Kind: collect.KindGauge, Time: time.Now()},
			{Metric: "no.unit", Value: 1, Kind: collect.KindGauge, Time: time.Now()},
			sample(),
		},
	}
	h := newHarness(t, fastConfig(), c)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a delivery", func() bool { return h.sink.count() >= 1 })

	got := h.sink.all()[0]
	gotBatch, ok := transport.MetricsOf(got)
	if !ok {
		t.Fatal("delivered envelope carries no metrics payload")
	}
	if len(gotBatch.Samples) != 2 {
		t.Errorf("delivered %d samples, want the 2 valid ones", len(gotBatch.Samples))
	}
	for _, s := range gotBatch.Samples {
		if err := s.Validate(); err != nil {
			t.Errorf("an invalid sample reached the sink: %v", err)
		}
	}
}

func TestFailureAndRecoveryAreReported(t *testing.T) {
	t.Parallel()

	c := &fakeCollector{
		desc:    collect.Descriptor{ID: "flaky.collector"},
		samples: []collect.Sample{sample()},
		err:     stderrors.New("docker socket unavailable"),
	}
	h := newHarness(t, fastConfig(), c)

	sub, err := h.bus.Subscribe("test", "collector.run.**")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "failures", func() bool { return h.sched.Stats().Failures >= 2 })

	health := h.sched.Health()[0]
	if health.Healthy || health.ConsecutiveFailures < 2 {
		t.Errorf("health = %+v, want unhealthy with consecutive failures", health)
	}
	if health.LastError == "" {
		t.Error("LastError was not recorded")
	}

	// Recover the collector; the next run must clear the failure state and
	// publish a recovery so a timeline can close the incident.
	c.setErr(nil)
	waitFor(t, "recovery", func() bool { return h.sched.Health()[0].Healthy })

	var sawRecovery bool
	deadline := time.After(2 * time.Second)
	for !sawRecovery {
		select {
		case e := <-sub.C:
			if e.Topic == scheduler.TopicRunRecovered {
				sawRecovery = true
			}
		case <-deadline:
			t.Fatal("no collector.run.recovered event was published")
		}
	}

	recovered := h.sched.Health()[0]
	if recovered.LastError != "" {
		t.Errorf("LastError = %q, want it cleared after recovery", recovered.LastError)
	}
	if recovered.LastSuccess.IsZero() {
		t.Error("LastSuccess was not recorded")
	}
}

func TestPerCollectorIntervalOverridesDefault(t *testing.T) {
	t.Parallel()

	fast := &fakeCollector{
		desc:    collect.Descriptor{ID: "fast.collector", Interval: 10 * time.Millisecond, Timeout: 5 * time.Millisecond},
		samples: []collect.Sample{sample()},
	}
	slow := &fakeCollector{
		desc:    collect.Descriptor{ID: "slow.collector", Interval: 2 * time.Second, Timeout: time.Second},
		samples: []collect.Sample{sample()},
	}
	h := newHarness(t, fastConfig(), fast, slow)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the fast collector to run repeatedly", func() bool { return fast.runs.Load() >= 8 })

	if got := slow.runs.Load(); got > 2 {
		t.Errorf("the two-second collector ran %d times while the fast one ran 8", got)
	}

	for _, hc := range h.sched.Health() {
		if hc.CollectorID == "fast.collector" && hc.Interval != "10ms" {
			t.Errorf("reported interval = %q, want 10ms", hc.Interval)
		}
	}
}

func TestStopWaitsForInFlightRuns(t *testing.T) {
	t.Parallel()

	c := &fakeCollector{
		desc:     collect.Descriptor{ID: "draining.collector"},
		blockFor: 50 * time.Millisecond,
		samples:  []collect.Sample{sample()},
	}
	cfg := fastConfig()
	cfg.Timeout = 500 * time.Millisecond
	h := newHarness(t, cfg, c)

	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a run to begin", func() bool { return c.runs.Load() >= 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.sched.Stop(ctx); err != nil {
		t.Errorf("Stop() error = %v", err)
	}

	// After Stop returns, nothing more may run.
	before := c.runs.Load()
	time.Sleep(100 * time.Millisecond)
	if after := c.runs.Load(); after != before {
		t.Errorf("collector ran %d more times after Stop returned", after-before)
	}
}

func TestStopBeforeStartIsSafe(t *testing.T) {
	t.Parallel()

	h := newHarness(t, fastConfig())
	if err := h.sched.Stop(context.Background()); err != nil {
		t.Errorf("Stop() before Start error = %v", err)
	}
}

func TestNewValidatesItsDependencies(t *testing.T) {
	t.Parallel()

	reg := collect.NewRegistry()
	tr := transport.NewInProcess(&capturingSink{})
	origin := transport.Origin{NodeID: "n1"}

	tests := []struct {
		name string
		opts scheduler.Options
	}{
		{"no registry", scheduler.Options{Transport: tr, Origin: origin}},
		{"no transport", scheduler.Options{Registry: reg, Origin: origin}},
		{"invalid origin", scheduler.Options{Registry: reg, Transport: tr}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := scheduler.New(tt.opts); err == nil {
				t.Error("New() accepted incomplete options")
			}
		})
	}
}
