package agent

import (
	"context"
	"errors"
	"sync"
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
	f := newFanoutTransport([]fanoutTarget{{id: "a", tr: a}, {id: "b", tr: b}})

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
	f := newFanoutTransport([]fanoutTarget{{id: "failing", tr: failing}, {id: "ok", tr: ok}})

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
	f := newFanoutTransport([]fanoutTarget{{id: "a", tr: a}, {id: "b", tr: b}})

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
	f := newFanoutTransport([]fanoutTarget{{id: "slow1", tr: slow1}, {id: "slow2", tr: slow2}})

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
	f := newFanoutTransport([]fanoutTarget{{id: "a", tr: a}, {id: "b", tr: b}})

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

// The same host is legitimately "development" to one control plane and
// "production" to another; each must see its own tag, not a shared one.
func TestFanoutStampsEachRelationshipsOwnEnvironment(t *testing.T) {
	t.Parallel()
	dev, prod := &recordingTransport{}, &recordingTransport{}
	f := newFanoutTransport([]fanoutTarget{
		{id: "development", environment: "development", tr: dev},
		{id: "production", environment: "production", tr: prod},
	})

	env := transport.Envelope{Origin: transport.Origin{NodeID: "node-1", Environment: "unset"}}
	if err := f.Send(context.Background(), env); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := dev.last().Origin.Environment; got != "development" {
		t.Errorf("development relationship saw environment %q, want development", got)
	}
	if got := prod.last().Origin.Environment; got != "production" {
		t.Errorf("production relationship saw environment %q, want production", got)
	}
	if env.Origin.Environment != "unset" {
		t.Errorf("caller's envelope was mutated: environment = %q", env.Origin.Environment)
	}
}

func TestSendToDeliversOnlyToNamedRelationshipsAndReportsEachOutcome(t *testing.T) {
	t.Parallel()
	a := &fakeTransport{}
	b := &fakeTransport{sendErr: errors.New("b down")}
	c := &fakeTransport{}
	f := newFanoutTransport([]fanoutTarget{{id: "a", tr: a}, {id: "b", tr: b}, {id: "c", tr: c}})

	results := f.SendTo(context.Background(), []string{"a", "b"}, transport.Envelope{})

	if len(results) != 2 {
		t.Fatalf("results = %v, want one entry each for a and b", results)
	}
	if results["a"] != nil {
		t.Errorf("results[a] = %v, want nil", results["a"])
	}
	if !errors.Is(results["b"], b.sendErr) {
		t.Errorf("results[b] = %v, want %v", results["b"], b.sendErr)
	}
	if c.sent.Load() != 0 {
		t.Error("an unnamed relationship was sent to")
	}
}

type recordingTransport struct {
	mu   sync.Mutex
	envs []transport.Envelope
}

func (r *recordingTransport) Send(_ context.Context, env transport.Envelope) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.envs = append(r.envs, env)
	return nil
}

func (r *recordingTransport) Close() error { return nil }

func (r *recordingTransport) last() transport.Envelope {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.envs) == 0 {
		return transport.Envelope{}
	}
	return r.envs[len(r.envs)-1]
}

var _ transport.Transport = (*fakeTransport)(nil)
var _ transport.Transport = (*slowTransport)(nil)
var _ transport.Transport = (*recordingTransport)(nil)
