package transport

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
)

// InProcess is the [Transport] used when collection and storage share a
// process — the single-node deployment Atlas ships as today.
//
// It hands the envelope straight to the sink with no serialisation, no
// queueing, and no copying. That makes the seam free in the deployment where
// it is not needed, which is what makes it affordable to have the seam at all:
// an abstraction that taxes the common case gets removed, and then the fleet
// rewrite happens anyway.
//
// Because delivery is synchronous, the sink's latency is the scheduler's
// latency. That is intended — it is real back-pressure onto collection, and
// unlike the event bus, observations here are the durable record and must not
// be silently dropped. The scheduler bounds it with a per-run timeout.
type InProcess struct {
	sink Sink

	mu     sync.RWMutex
	closed bool

	sent     atomic.Uint64
	failed   atomic.Uint64
	rejected atomic.Uint64
}

// NewInProcess builds an in-process transport delivering to sink.
func NewInProcess(sink Sink) *InProcess {
	return &InProcess{sink: sink}
}

// Send validates the envelope and delivers it to the sink.
//
// Invalid envelopes are rejected rather than passed on: a malformed
// observation that reaches storage is far more expensive to find and remove
// than one refused at the boundary, and the error names the collector that
// produced it.
func (t *InProcess) Send(ctx context.Context, env Envelope) error {
	const op = "transport.InProcess.Send"

	if env.ID == "" {
		env.ID = id.New()
	}
	if err := env.Validate(); err != nil {
		t.rejected.Add(1)
		return errs.Wrap(err, errs.CodeInvalidArgument, "envelope rejected").WithOp(op)
	}

	t.mu.RLock()
	closed := t.closed
	t.mu.RUnlock()
	if closed {
		t.rejected.Add(1)
		return errs.New(errs.CodeUnavailable, "transport is closed").WithOp(op)
	}

	if err := ctx.Err(); err != nil {
		t.failed.Add(1)
		return errs.Wrap(err, errs.CodeDeadlineExceeded, "delivery cancelled").WithOp(op)
	}

	if err := t.sink.Receive(ctx, env); err != nil {
		t.failed.Add(1)
		return errs.Wrap(err, errs.CodeOf(err), "sink rejected envelope").
			WithOp(op).WithDetail("kind", string(env.Kind()))
	}

	t.sent.Add(1)
	return nil
}

// Close marks the transport closed. There is nothing buffered to flush.
// It is idempotent.
func (t *InProcess) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return nil
}

// Stats reports delivery counts.
type Stats struct {
	// Sent counts envelopes the sink accepted.
	Sent uint64 `json:"sent"`
	// Failed counts envelopes the sink refused or that were cancelled.
	Failed uint64 `json:"failed"`
	// Rejected counts envelopes refused before delivery as malformed, or
	// because the transport was closed.
	Rejected uint64 `json:"rejected"`
}

// Stats returns a snapshot of delivery counts.
func (t *InProcess) Stats() Stats {
	return Stats{
		Sent:     t.sent.Load(),
		Failed:   t.failed.Load(),
		Rejected: t.rejected.Load(),
	}
}

var _ Transport = (*InProcess)(nil)
