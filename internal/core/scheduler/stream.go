package scheduler

import (
	"context"
	stderrors "errors"
	"log/slog"
	"math/rand/v2"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Streaming supervision parameters.
//
// A stream produces samples at whatever rate its source generates them, which
// for a Docker event feed is bursty and for a log tail can be relentless. The
// scheduler therefore batches on both size and time: whichever comes first.
// Batching only on size would delay a quiet stream's samples indefinitely;
// batching only on time would send single-sample batches during a flood.
const (
	// streamFlushInterval bounds how long a sample waits before being sent.
	streamFlushInterval = 5 * time.Second
	// streamMaxBatch caps a batch, so a flood produces several bounded writes
	// rather than one unbounded one.
	streamMaxBatch = 500
	// streamBufferSize is the channel depth between a streamer and the
	// scheduler. It absorbs a burst without making the streamer block.
	streamBufferSize = 1024

	// Restart backoff. A source that is down stays down for a while, and
	// hammering a dead Docker socket helps nobody.
	streamRestartMin = 1 * time.Second
	streamRestartMax = 2 * time.Minute
)

// ErrStreamPanic marks an error produced by recovering a streamer panic.
var ErrStreamPanic = stderrors.New("streamer panicked")

// streamLoop supervises one streaming collector for the life of the scheduler.
//
// The supervision is the point. A plugin that spawned its own goroutine would
// get none of this: no restart when the daemon it watches is restarted, no
// panic isolation, no health reporting, no cardinality enforcement, and no
// bounded shutdown. Every one of those is a property the pull path already
// has, and a Docker event stream needs them more, not less — it runs
// continuously against a daemon that is upgraded underneath it.
func (s *Scheduler) streamLoop(ctx context.Context, st collect.Streamer, desc collect.Descriptor) {
	defer s.wg.Done()

	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}

		start := s.now()
		err := s.runStream(ctx, st, desc)
		ran := s.now().Sub(start)

		if ctx.Err() != nil {
			return // shutting down; a stream ending here is expected
		}

		// A stream that ran for a meaningful period before ending was working;
		// treat the next start as a fresh attempt rather than escalating the
		// backoff of a source that is merely restarted occasionally.
		if ran > streamRestartMax {
			attempt = 0
		}

		switch {
		case err != nil:
			s.recordFailure(ctx, desc, err, ran)
		default:
			// Ending without error is still an ended stream. It is not a
			// failure, but the source has stopped producing and must be
			// reconnected.
			s.logger.InfoContext(ctx, "stream ended, reconnecting",
				slog.String("collector_id", desc.ID), slog.Duration("ran_for", ran))
		}

		delay := streamBackoff(attempt)
		attempt++

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

// runStream runs one attempt, draining samples until the stream ends.
func (s *Scheduler) runStream(ctx context.Context, st collect.Streamer, desc collect.Descriptor) error {
	// The stream's own context is cancelled as soon as this attempt finishes,
	// so a streamer that leaks a goroutine internally still has its context
	// closed rather than being left running against a dead scheduler.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	out := make(chan collect.Sample, streamBufferSize)
	done := make(chan error, 1)

	go func() { done <- s.safeStream(streamCtx, st, out) }()

	ticker := time.NewTicker(streamFlushInterval)
	defer ticker.Stop()

	buffer := make([]collect.Sample, 0, streamMaxBatch)
	batchStart := s.now()

	flush := func() {
		if len(buffer) == 0 {
			return
		}
		s.deliver(ctx, desc, buffer, batchStart, s.now())
		buffer = buffer[:0]
		batchStart = s.now()
	}

	for {
		select {
		case <-ctx.Done():
			// Flush what was already collected before exiting. Discarding it
			// would lose observations that were successfully made.
			flush()
			return nil

		case err := <-done:
			flush()
			return err

		case sample := <-out:
			buffer = append(buffer, sample)
			if len(buffer) >= streamMaxBatch {
				flush()
			}

		case <-ticker.C:
			flush()
		}
	}
}

// safeStream runs a streamer, converting a panic into an error.
func (s *Scheduler) safeStream(ctx context.Context, st collect.Streamer, out chan<- collect.Sample) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = errs.Wrap(ErrStreamPanic, errs.CodeInternal, "streamer panicked: %v", rec).
				WithOp("scheduler.Scheduler.safeStream")
		}
	}()
	return st.Stream(ctx, out)
}

// deliver validates, budgets, and sends a batch of streamed samples.
//
// It shares the validation, cardinality, and transport path with polled
// collection, so a streaming source cannot bypass the guard that stops a
// runaway collector from destroying the datastore. A log tail labelling by
// container id is exactly the shape of bug that guard exists for.
func (s *Scheduler) deliver(ctx context.Context, desc collect.Descriptor, samples []collect.Sample, started, completed time.Time) {
	valid, invalid := partitionValid(samples)
	if len(invalid) > 0 {
		s.logger.WarnContext(ctx, "streamer produced invalid samples",
			slog.String("collector_id", desc.ID),
			slog.Int("invalid", len(invalid)),
			slog.String("first_error", invalid[0].Error()))
	}

	admission := s.cardinality.admit(desc.ID, valid)
	if admission.NewlyExceeded {
		s.logger.WarnContext(ctx, "streamer exceeded its series budget; new series are being dropped",
			slog.String("collector_id", desc.ID),
			slog.Int("limit", s.cfg.MaxSeriesPerCollector),
			slog.Int("series", admission.Series))
		s.publish(ctx, TopicCardinalityExceeded, desc.ID, CardinalityBreach{
			CollectorID: desc.ID, Limit: s.cfg.MaxSeriesPerCollector,
			Series: admission.Series, RefusedThisRun: admission.Refused,
		})
	}
	if len(admission.Samples) == 0 {
		return
	}

	batch := collect.Batch{
		CollectorID: desc.ID,
		StartedAt:   started,
		CompletedAt: completed,
		Samples:     admission.Samples,
	}

	// A send during shutdown must still have a live deadline, or a flush of
	// already-collected samples would be cancelled the moment it started.
	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeout)
	defer cancel()

	if err := s.transport.Send(sendCtx, transport.NewEnvelope(s.origin, batch)); err != nil {
		s.recordFailure(ctx, desc, err, completed.Sub(started))
		return
	}

	s.samples.Add(uint64(len(admission.Samples)))
	s.refusedSeries.Add(uint64(admission.Refused))
	s.recordSuccess(ctx, desc, completed)
}

// streamBackoff returns the delay before the next reconnection attempt.
//
// Full jitter, not plain exponential: when a Docker daemon or a control plane
// restarts, every stream watching it fails at the same instant, and a
// deterministic backoff would reconnect them all at the same instant too —
// repeatedly. Randomising the whole interval spreads the retries.
func streamBackoff(attempt int) time.Duration {
	backoff := streamRestartMin << min(attempt, 16)
	if backoff > streamRestartMax || backoff <= 0 {
		backoff = streamRestartMax
	}
	return time.Duration(rand.Int64N(int64(backoff)) + int64(streamRestartMin))
}

// streamerIDs indexes streaming descriptors for a membership test.
func streamerIDs(descs []collect.Descriptor) map[string]struct{} {
	out := make(map[string]struct{}, len(descs))
	for _, d := range descs {
		out[d.ID] = struct{}{}
	}
	return out
}

// intervalLabel renders a collector's cadence. Streamers have none: they run
// continuously, and reporting a default interval would imply a poll that never
// happens.
func intervalLabel(d time.Duration) string {
	if d == 0 {
		return "continuous"
	}
	return d.String()
}
