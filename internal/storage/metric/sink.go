package metric

import (
	"context"
	"log/slog"
	"sync/atomic"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Sink persists envelopes arriving from a transport.
//
// This is the receiving end of the seam described in ADR-0005. It has no idea
// whether an envelope crossed a network or a function call, which is exactly
// what allows Tier 4 to add remote agents without changing storage.
type Sink struct {
	repo   BatchWriter
	logger *slog.Logger

	received atomic.Uint64
	samples  atomic.Uint64
	failed   atomic.Uint64
}

// BatchWriter is the storage surface a sink needs.
//
// Narrowed from the full repository so the ingest path can be tested without a
// database. This is the path where collected observations are silently lost if
// anything goes wrong, and it was previously untestable — the sink held a
// concrete *Repository, so exercising it meant standing up Postgres.
type BatchWriter interface {
	InsertBatch(ctx context.Context, env transport.Envelope, batch collect.Batch) error
}

// NewSink builds a sink writing through repo.
func NewSink(repo BatchWriter, logger *slog.Logger) *Sink {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Sink{repo: repo, logger: logger}
}

// Receive persists an envelope.
//
// Errors are returned rather than swallowed, so the scheduler records the
// collection run as failed and publishes it. A collector that is reading its
// source perfectly but whose data never lands must not be reported as healthy:
// that is the failure mode where a dashboard looks fine and the underlying
// series has been empty for a day.
func (s *Sink) Receive(ctx context.Context, env transport.Envelope) error {
	const op = "metric.Sink.Receive"

	s.received.Add(1)

	batch, ok := transport.MetricsOf(env)
	if !ok {
		// The router dispatches by kind, so this sink cannot legitimately be
		// handed anything else. Reaching here means a wiring mistake, which
		// is an internal fault rather than bad input.
		s.failed.Add(1)
		return errs.New(errs.CodeInternal,
			"the metric sink was given a %s envelope", env.Kind()).
			WithOp(op).WithDetail("kind", string(env.Kind()))
	}

	if err := s.repo.InsertBatch(ctx, env, batch); err != nil {
		s.failed.Add(1)
		return errs.Wrap(err, errs.CodeOf(err), "could not persist observations").
			WithOp(op).
			WithDetail("collector_id", batch.CollectorID)
	}

	n := len(batch.Samples)
	s.samples.Add(uint64(n))

	s.logger.DebugContext(ctx, "observations stored",
		slog.String("collector_id", batch.CollectorID),
		slog.String("node_id", env.Origin.NodeID),
		slog.Int("samples", n),
	)
	return nil
}

// Kind implements [transport.Receiver]: this sink handles metric batches.
func (s *Sink) Kind() transport.Kind { return transport.KindMetrics }

// Stats reports ingest activity.
type Stats struct {
	// Envelopes counts batches offered to the sink.
	Envelopes uint64 `json:"envelopes"`
	// Samples counts individual observations written.
	Samples uint64 `json:"samples"`
	// Failed counts envelopes that could not be persisted. Non-zero means
	// observations were collected and then lost.
	Failed uint64 `json:"failed"`
}

// Stats returns a snapshot of ingest activity.
func (s *Sink) Stats() Stats {
	return Stats{
		Envelopes: s.received.Load(),
		Samples:   s.samples.Load(),
		Failed:    s.failed.Load(),
	}
}

var (
	_ transport.Sink     = (*Sink)(nil)
	_ transport.Receiver = (*Sink)(nil)
)
