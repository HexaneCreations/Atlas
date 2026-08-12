package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/transport"
)

// fakeTransport is an in-memory transport.Transport for exercising
// fanoutTransport without a real network, spool, or control plane.
type fakeTransport struct {
	sendErr  error
	closeErr error
	sent     atomic.Int32
	closed   atomic.Bool
}

func (f *fakeTransport) Send(context.Context, transport.Envelope) error {
	f.sent.Add(1)
	return f.sendErr
}

func (f *fakeTransport) Close() error {
	f.closed.Store(true)
	return f.closeErr
}

func TestFanoutSendReachesEveryTarget(t *testing.T) {
	t.Parallel()
	a, b := &fakeTransport{}, &fakeTransport{}
	f := newFanoutTransport([]transport.Transport{a, b})

	if err := f.Send(context.Background(), transport.Envelope{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if a.sent.Load() != 1 || b.sent.Load() != 1 {
		t.Errorf("sent counts = %d, %d, want 1, 1", a.sent.Load(), b.sent.Load())
	}
}

// The core independence requirement: one relationship's transport failing
// must not fail delivery to another, and must not stop the other from being
// attempted.
func TestFanoutSendOneTargetFailingDoesNotAffectOthers(t *testing.T) {
	t.Parallel()
	failing := &fakeTransport{sendErr: errors.New("control plane unreachable")}
	ok := &fakeTransport{}
	f := newFanoutTransport([]transport.Transport{failing, ok})

	if err := f.Send(context.Background(), transport.Envelope{}); err != nil {
		t.Fatalf("Send returned an error even though one target succeeded: %v", err)
	}
	if ok.sent.Load() != 1 {
		t.Error("the healthy target was not sent to")
	}
}

// An error must be returned only when every target failed — callers
// (scheduler, inventoryPusher, eventForwarder) must never see a spurious
// failure while any relationship is still accepting envelopes.
func TestFanoutSendReturnsErrorOnlyWhenAllTargetsFail(t *testing.T) {
	t.Parallel()
	a := &fakeTransport{sendErr: errors.New("a down")}
	b := &fakeTransport{sendErr: errors.New("b down")}
	f := newFanoutTransport([]transport.Transport{a, b})

	err := f.Send(context.Background(), transport.Envelope{})
	if err == nil {
		t.Fatal("expected an aggregated error when every target failed")
	}
	if !errors.Is(err, a.sendErr) || !errors.Is(err, b.sendErr) {
		t.Errorf("error = %v, want it to wrap both underlying failures", err)
	}
}

func TestFanoutSendIsParallelNotSequential(t *testing.T) {
	t.Parallel()
	const delay = 100 * time.Millisecond
	slow1 := &slowTransport{delay: delay}
	slow2 := &slowTransport{delay: delay}
	f := newFanoutTransport([]transport.Transport{slow1, slow2})

	start := time.Now()
	if err := f.Send(context.Background(), transport.Envelope{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed >= 2*delay {
		t.Errorf("Send took %s for two %s-delayed targets, want them run in parallel (~%s, not ~%s)", elapsed, delay, delay, 2*delay)
	}
}

type slowTransport struct{ delay time.Duration }

func (s *slowTransport) Send(context.Context, transport.Envelope) error {
	time.Sleep(s.delay)
	return nil
}
func (s *slowTransport) Close() error { return nil }

func TestFanoutCloseClosesEveryTargetAndAggregatesErrors(t *testing.T) {
	t.Parallel()
	a := &fakeTransport{closeErr: errors.New("a close failed")}
	b := &fakeTransport{}
	f := newFanoutTransport([]transport.Transport{a, b})

	err := f.Close()
	if !a.closed.Load() || !b.closed.Load() {
		t.Errorf("closed = %v, %v, want both targets closed regardless of the other's error", a.closed.Load(), b.closed.Load())
	}
	if !errors.Is(err, a.closeErr) {
		t.Errorf("Close error = %v, want it to wrap %v", err, a.closeErr)
	}
}

func TestFanoutWithZeroTargetsIsANoop(t *testing.T) {
	t.Parallel()
	f := newFanoutTransport(nil)
	if err := f.Send(context.Background(), transport.Envelope{}); err != nil {
		t.Errorf("Send with zero targets = %v, want nil", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("Close with zero targets = %v, want nil", err)
	}
}

var _ transport.Transport = (*fakeTransport)(nil)
var _ transport.Transport = (*slowTransport)(nil)
