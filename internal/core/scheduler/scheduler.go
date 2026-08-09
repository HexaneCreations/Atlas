// Package scheduler runs collectors on their intervals and delivers what
// they produce into a transport.
//
// The scheduler is where Atlas's promise not to harm the host it observes is
// actually enforced. Its rules:
//
//   - Every run is bounded by a timeout. A collector blocked on a wedged NFS
//     mount or an unresponsive Docker socket is cancelled, not waited on.
//   - Runs of the same collector never overlap. A slow collector falls behind
//     and reports that it did; it does not accumulate goroutines.
//   - A concurrency ceiling bounds how many collectors run at once, so a host
//     with fifty collectors does not produce fifty simultaneous readers.
//   - Start times are jittered. Without it, every collector with the same
//     interval fires on the same tick and Atlas becomes a periodic load spike.
//   - Panics are contained per collector. A bug in one never stops the others.
//
// Each rule exists because the opposite behaviour has, in some monitoring
// agent somewhere, taken down the machine it was watching.
package scheduler

import (
	"context"
	stderrors "errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/lifecycle"
)

// Event topics published by the scheduler. Subscribers use these to surface
// collector health without polling the scheduler for it.
const (
	// TopicRunFailed is published when a collection run returns an error.
	TopicRunFailed eventbus.Topic = "collector.run.failed"
	// TopicRunPanicked is published when a collector panics. Distinct from a
	// failure because it always indicates a bug, never an environment
	// condition, and should be escalated differently.
	TopicRunPanicked eventbus.Topic = "collector.run.panicked"
	// TopicRunRecovered is published on the first success after a failure,
	// so a timeline can close out an incident rather than leaving it open.
	TopicRunRecovered eventbus.Topic = "collector.run.recovered"
	// TopicCardinalityExceeded is published the first time a collector
	// exceeds its series budget. It signals a bug in that collector — almost
	// always a label whose value is unbounded — and it is escalated
	// separately because the damage it does is to the datastore rather than
	// to one metric.
	TopicCardinalityExceeded eventbus.Topic = "collector.cardinality.exceeded"
)

// CardinalityBreach is the payload of [TopicCardinalityExceeded].
type CardinalityBreach struct {
	CollectorID string `json:"collector_id"`
	// Limit is the per-collector budget that was reached.
	Limit int `json:"limit"`
	// Series is the collector's distinct series count.
	Series int `json:"series"`
	// RefusedThisRun counts samples dropped in the run that tripped the limit.
	RefusedThisRun int `json:"refused_this_run"`
}

// RunFailure is the payload of [TopicRunFailed] and [TopicRunPanicked].
type RunFailure struct {
	CollectorID string        `json:"collector_id"`
	Err         string        `json:"error"`
	Duration    time.Duration `json:"duration"`
	Consecutive int           `json:"consecutive_failures"`
	IsPanic     bool          `json:"is_panic"`
}

// Scheduler runs registered collectors and forwards their samples.
//
// It satisfies [lifecycle.Component].
type Scheduler struct {
	registry  *collect.Registry
	transport transport.Transport
	origin    transport.Origin
	bus       *eventbus.Bus
	logger    *slog.Logger
	cfg       config.Collection

	// slots bounds how many collectors run concurrently.
	slots chan struct{}

	// cardinality refuses new series once a collector exceeds its budget,
	// which is what stops one buggy collector from destroying the datastore.
	cardinality *cardinalityGuard

	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu    sync.RWMutex
	state map[string]*collectorState

	runs          atomic.Uint64
	failures      atomic.Uint64
	samples       atomic.Uint64
	refusedSeries atomic.Uint64

	// now and jitter are injected so tests are deterministic rather than
	// timing-dependent.
	now    func() time.Time
	jitter func(time.Duration) time.Duration
}

// collectorState tracks the health of one collector across runs.
type collectorState struct {
	mu                  sync.Mutex
	lastRun             time.Time
	lastSuccess         time.Time
	lastError           string
	consecutiveFailures int
	totalRuns           uint64
	totalFailures       uint64
}

// Options configures a [Scheduler].
type Options struct {
	// Registry supplies the collectors to run.
	Registry *collect.Registry
	// Transport receives every successful batch.
	Transport transport.Transport
	// Origin identifies the node these observations describe.
	Origin transport.Origin
	// Bus receives collector health events. Optional.
	Bus *eventbus.Bus
	// Logger records run outcomes. Defaults to a discarding logger.
	Logger *slog.Logger
	// Config supplies default interval, timeout, and concurrency.
	Config config.Collection

	// Now and Jitter are injected by tests.
	Now    func() time.Time
	Jitter func(time.Duration) time.Duration
}

// New builds a scheduler.
func New(opts Options) (*Scheduler, error) {
	const op = "scheduler.New"

	if opts.Registry == nil {
		return nil, errs.New(errs.CodeInvalidArgument, "scheduler requires a collector registry").WithOp(op)
	}
	if opts.Transport == nil {
		return nil, errs.New(errs.CodeInvalidArgument, "scheduler requires a transport").WithOp(op)
	}
	if err := opts.Origin.Validate(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInvalidArgument, "scheduler requires a valid origin").WithOp(op)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Config.DefaultInterval <= 0 {
		opts.Config.DefaultInterval = 15 * time.Second
	}
	if opts.Config.Timeout <= 0 {
		opts.Config.Timeout = 10 * time.Second
	}
	if opts.Config.MaxConcurrent <= 0 {
		opts.Config.MaxConcurrent = 8
	}
	if opts.Config.MaxSeriesPerCollector == 0 {
		opts.Config.MaxSeriesPerCollector = config.DefaultMaxSeriesPerCollector
	}
	if opts.Config.SeriesWindow <= 0 {
		opts.Config.SeriesWindow = config.DefaultSeriesWindow
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Jitter == nil {
		opts.Jitter = defaultJitter
	}

	return &Scheduler{
		registry:  opts.Registry,
		transport: opts.Transport,
		origin:    opts.Origin,
		bus:       opts.Bus,
		logger:    opts.Logger,
		cfg:       opts.Config,
		slots:     make(chan struct{}, opts.Config.MaxConcurrent),
		cardinality: newCardinalityGuard(
			opts.Config.MaxSeriesPerCollector, opts.Config.SeriesWindow, opts.Now),
		state:  make(map[string]*collectorState),
		now:    opts.Now,
		jitter: opts.Jitter,
	}, nil
}

// defaultJitter spreads the first run of each collector across its interval.
//
// Without it, every collector sharing an interval fires on the same tick
// forever, turning steady observation into a periodic spike — and the spike
// lands on exactly the metric the spike distorts.
func defaultJitter(interval time.Duration) time.Duration {
	if interval <= 0 {
		return 0
	}
	return time.Duration(rand.Int64N(int64(interval)))
}

// Name implements [lifecycle.Component].
func (s *Scheduler) Name() string { return "collect.scheduler" }

// Start launches one goroutine per registered collector.
//
// Starting with an empty registry is valid and does nothing: a deployment
// where no plugin detected its subject has nothing to collect, and that is a
// legitimate state rather than an error.
func (s *Scheduler) Start(ctx context.Context) error {
	// The run context is detached from the start context, which callers are
	// free to cancel once startup completes; the scheduler's own lifetime is
	// governed by Stop.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	s.cancel = cancel

	collectors := s.registry.All()
	for _, c := range collectors {
		desc := c.Descriptor()

		s.mu.Lock()
		s.state[desc.ID] = &collectorState{}
		s.mu.Unlock()

		s.wg.Add(1)
		go s.runLoop(runCtx, c, desc)
	}

	streamers := s.registry.Streamers()
	for _, st := range streamers {
		desc := st.Descriptor()

		s.mu.Lock()
		s.state[desc.ID] = &collectorState{}
		s.mu.Unlock()

		s.wg.Add(1)
		go s.streamLoop(runCtx, st, desc)
	}

	s.logger.InfoContext(ctx, "collector scheduler started",
		slog.Int("collectors", len(collectors)),
		slog.Int("streamers", len(streamers)),
		slog.Int("max_concurrent", s.cfg.MaxConcurrent),
		slog.Duration("default_interval", s.cfg.DefaultInterval),
	)
	return nil
}

// Stop cancels every collector loop and waits for in-flight runs to finish.
func (s *Scheduler) Stop(ctx context.Context) error {
	if s.cancel == nil {
		return nil // never started
	}
	s.cancel()

	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		s.logger.InfoContext(ctx, "collector scheduler stopped")
		return nil
	case <-ctx.Done():
		// Collectors are supposed to honour cancellation. One that does not
		// is a bug worth naming, and the process is exiting regardless.
		return errs.Wrap(ctx.Err(), errs.CodeDeadlineExceeded,
			"collectors did not stop before the shutdown deadline").
			WithOp("scheduler.Scheduler.Stop")
	}
}

// runLoop drives one collector on its own schedule.
func (s *Scheduler) runLoop(ctx context.Context, c collect.Collector, desc collect.Descriptor) {
	defer s.wg.Done()

	interval := desc.Interval
	if interval <= 0 {
		interval = s.cfg.DefaultInterval
	}

	// Wait a jittered fraction of the interval before the first run.
	select {
	case <-ctx.Done():
		return
	case <-time.After(s.jitter(interval)):
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run immediately after the jitter so a freshly started Atlas has data
	// within a jitter window rather than a full interval.
	s.runOnce(ctx, c, desc)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Ticks that arrive while a run is in progress are dropped by
			// the ticker itself (its channel has capacity one), which is
			// exactly the non-overlapping behaviour wanted: a slow collector
			// falls behind rather than piling up.
			s.runOnce(ctx, c, desc)
		}
	}
}

// runOnce performs a single bounded collection run.
func (s *Scheduler) runOnce(ctx context.Context, c collect.Collector, desc collect.Descriptor) {
	// Acquire a concurrency slot, or give up if shutting down.
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return
	}

	timeout := desc.Timeout
	if timeout <= 0 {
		timeout = s.cfg.Timeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	started := s.now()
	s.runs.Add(1)

	samples, err := s.safeCollect(runCtx, c)
	completed := s.now()

	if err != nil {
		s.recordFailure(ctx, desc, err, completed.Sub(started))
		return
	}

	valid, invalid := partitionValid(samples)
	if len(invalid) > 0 {
		// A malformed sample is a plugin bug. Drop only the bad samples and
		// keep the good ones: partial data from a partly broken collector is
		// still better than none, and the log names what to fix.
		s.logger.WarnContext(ctx, "collector produced invalid samples",
			slog.String("collector_id", desc.ID),
			slog.Int("invalid", len(invalid)),
			slog.Int("valid", len(valid)),
			slog.String("first_error", invalid[0].Error()),
		)
	}

	// Cardinality is enforced after validation and before storage, because
	// this is the last point at which a runaway collector can be stopped
	// without the damage already being durable.
	admission := s.cardinality.admit(desc.ID, valid)
	if admission.Refused > 0 {
		s.logger.WarnContext(ctx, "collector exceeded its series budget; new series are being dropped",
			slog.String("collector_id", desc.ID),
			slog.Int("limit", s.cfg.MaxSeriesPerCollector),
			slog.Int("series", admission.Series),
			slog.Int("refused_this_run", admission.Refused),
			slog.String("likely_cause", "a label with an unbounded value, such as a process id or request path"),
		)
	}
	if admission.NewlyExceeded {
		s.publish(ctx, TopicCardinalityExceeded, desc.ID, CardinalityBreach{
			CollectorID:    desc.ID,
			Limit:          s.cfg.MaxSeriesPerCollector,
			Series:         admission.Series,
			RefusedThisRun: admission.Refused,
		})
	}

	batch := collect.Batch{
		CollectorID: desc.ID,
		StartedAt:   started,
		CompletedAt: completed,
		Samples:     admission.Samples,
	}

	if err := s.transport.Send(runCtx, transport.NewEnvelope(s.origin, batch)); err != nil {
		s.recordFailure(ctx, desc, err, completed.Sub(started))
		return
	}

	s.samples.Add(uint64(len(admission.Samples)))
	s.refusedSeries.Add(uint64(admission.Refused))
	s.recordSuccess(ctx, desc, completed)
}

// ErrCollectorPanic marks an error produced by recovering a collector panic.
//
// A panic is categorically different from a returned error: an error usually
// means the environment is uncooperative, while a panic always means the
// collector has a bug. They are routed to different topics so an operator can
// alert on one and file a ticket for the other.
var ErrCollectorPanic = stderrors.New("collector panicked")

// safeCollect runs a collector, converting a panic into an error.
//
// Atlas reads /proc entries that vanish mid-read and daemon payloads that
// change shape between versions. A nil dereference in one collector must not
// take the process down and end monitoring for everything else.
func (s *Scheduler) safeCollect(ctx context.Context, c collect.Collector) (samples []collect.Sample, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = errs.Wrap(ErrCollectorPanic, errs.CodeInternal, "collector panicked: %v", rec).
				WithOp("scheduler.Scheduler.safeCollect")
			samples = nil
		}
	}()
	return c.Collect(ctx)
}

func partitionValid(samples []collect.Sample) (valid []collect.Sample, invalid []error) {
	valid = make([]collect.Sample, 0, len(samples))
	for _, sample := range samples {
		if err := sample.Validate(); err != nil {
			invalid = append(invalid, err)
			continue
		}
		valid = append(valid, sample)
	}
	return valid, invalid
}

func (s *Scheduler) recordSuccess(ctx context.Context, desc collect.Descriptor, at time.Time) {
	st := s.stateFor(desc.ID)

	st.mu.Lock()
	recovered := st.consecutiveFailures > 0
	failures := st.consecutiveFailures
	st.consecutiveFailures = 0
	st.lastError = ""
	st.lastRun = at
	st.lastSuccess = at
	st.totalRuns++
	st.mu.Unlock()

	if recovered {
		s.logger.InfoContext(ctx, "collector recovered",
			slog.String("collector_id", desc.ID),
			slog.Int("after_failures", failures))
		s.publish(ctx, TopicRunRecovered, desc.ID, RunFailure{
			CollectorID: desc.ID,
			Consecutive: failures,
		})
	}
}

func (s *Scheduler) recordFailure(ctx context.Context, desc collect.Descriptor, err error, took time.Duration) {
	st := s.stateFor(desc.ID)

	st.mu.Lock()
	st.consecutiveFailures++
	consecutive := st.consecutiveFailures
	st.lastError = err.Error()
	st.lastRun = s.now()
	st.totalRuns++
	st.totalFailures++
	st.mu.Unlock()

	s.failures.Add(1)
	isPanic := errs.Is(err, ErrCollectorPanic)

	s.logger.ErrorContext(ctx, "collection run failed",
		slog.String("collector_id", desc.ID),
		slog.Int("consecutive_failures", consecutive),
		slog.Duration("duration", took),
		slog.Bool("panic", isPanic),
		slog.Any("error", err),
	)

	topic := TopicRunFailed
	if isPanic {
		topic = TopicRunPanicked
	}
	s.publish(ctx, topic, desc.ID, RunFailure{
		CollectorID: desc.ID,
		// errs.Message, not err.Error: this payload reaches the API and the
		// UI through the incident timeline.
		Err:         errs.Message(err),
		Duration:    took,
		Consecutive: consecutive,
		IsPanic:     isPanic,
	})
}

func (s *Scheduler) publish(ctx context.Context, topic eventbus.Topic, subject string, payload any) {
	if s.bus == nil {
		return
	}
	s.bus.Publish(ctx, eventbus.Event{
		Topic:   topic,
		Source:  s.Name(),
		Subject: subject,
		Payload: payload,
	})
}

func (s *Scheduler) stateFor(id string) *collectorState {
	s.mu.RLock()
	st, ok := s.state[id]
	s.mu.RUnlock()
	if ok {
		return st
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.state[id]; ok {
		return st
	}
	st = &collectorState{}
	s.state[id] = st
	return st
}

// CollectorHealth reports one collector's observed reliability.
type CollectorHealth struct {
	CollectorID string `json:"collector_id"`
	Name        string `json:"name"`
	Interval    string `json:"interval"`
	// Streaming reports a continuously supervised source rather than a polled
	// one. Streaming collectors have no interval.
	Streaming           bool      `json:"streaming"`
	LastRun             time.Time `json:"last_run,omitzero"`
	LastSuccess         time.Time `json:"last_success,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	TotalRuns           uint64    `json:"total_runs"`
	TotalFailures       uint64    `json:"total_failures"`
	// Series is the collector's current distinct series count, and
	// SeriesLimit its budget. A count at the limit means new series are being
	// refused.
	Series        int    `json:"series"`
	SeriesLimit   int    `json:"series_limit"`
	RefusedSeries uint64 `json:"refused_series"`
	Healthy       bool   `json:"healthy"`
}

// Health returns per-collector health, in registration order.
//
// A monitoring platform that cannot say which of its own collectors are
// failing will eventually report a host as healthy because nothing has
// observed it for an hour. This is the endpoint that prevents that.
func (s *Scheduler) Health() []CollectorHealth {
	descriptors := s.registry.Descriptors()
	streaming := s.registry.StreamerDescriptors()
	out := make([]CollectorHealth, 0, len(descriptors)+len(streaming))

	for _, desc := range append(descriptors, streaming...) {
		st := s.stateFor(desc.ID)

		interval := desc.Interval
		if interval <= 0 {
			interval = s.cfg.DefaultInterval
		}
		// A streamer has no interval: it runs continuously. Reporting the
		// default would imply a poll that never happens.
		if _, isStream := streamerIDs(streaming)[desc.ID]; isStream {
			interval = 0
		}

		series, refused := s.cardinality.stats(desc.ID)

		st.mu.Lock()
		out = append(out, CollectorHealth{
			CollectorID:         desc.ID,
			Name:                desc.Name,
			Interval:            intervalLabel(interval),
			Streaming:           interval == 0,
			LastRun:             st.lastRun,
			LastSuccess:         st.lastSuccess,
			LastError:           st.lastError,
			ConsecutiveFailures: st.consecutiveFailures,
			TotalRuns:           st.totalRuns,
			TotalFailures:       st.totalFailures,
			Series:              series,
			SeriesLimit:         s.cfg.MaxSeriesPerCollector,
			RefusedSeries:       refused,
			// A collector refusing series is not failing its runs, but it is
			// not healthy either: it is silently losing data.
			Healthy: st.consecutiveFailures == 0 && refused == 0,
		})
		st.mu.Unlock()
	}
	return out
}

// Stats summarises scheduler activity.
type Stats struct {
	Collectors int    `json:"collectors"`
	Runs       uint64 `json:"runs"`
	Failures   uint64 `json:"failures"`
	Samples    uint64 `json:"samples"`
	// RefusedSeries counts samples dropped for exceeding a series budget.
	// Non-zero means a collector is producing unbounded labels.
	RefusedSeries uint64 `json:"refused_series"`
}

// Stats returns a snapshot of scheduler activity.
func (s *Scheduler) Stats() Stats {
	return Stats{
		// Streamers count too: a fleet operator asking "how many collectors
		// are running here" means both kinds.
		Collectors:    s.registry.Len() + len(s.registry.Streamers()),
		Runs:          s.runs.Load(),
		Failures:      s.failures.Load(),
		Samples:       s.samples.Load(),
		RefusedSeries: s.refusedSeries.Load(),
	}
}

func (s *Scheduler) String() string {
	return fmt.Sprintf("scheduler(%d collectors)", s.registry.Len())
}

var _ lifecycle.Component = (*Scheduler)(nil)
