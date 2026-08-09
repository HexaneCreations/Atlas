package lifecycle_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/lifecycle"
)

// recorder is a Component that appends to a shared, mutex-guarded log so
// tests can assert on exact ordering.
type recorder struct {
	name     string
	log      *orderLog
	startErr error
	stopErr  error
	stopFor  time.Duration
	faults   chan error
}

type orderLog struct {
	mu      sync.Mutex
	entries []string
}

func (l *orderLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries = append(l.entries, s)
}

func (l *orderLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.entries)
}

func (r *recorder) Name() string { return r.name }

func (r *recorder) Start(context.Context) error {
	r.log.add("start:" + r.name)
	return r.startErr
}

func (r *recorder) Stop(ctx context.Context) error {
	if r.stopFor > 0 {
		select {
		case <-time.After(r.stopFor):
		case <-ctx.Done():
			r.log.add("stop-cancelled:" + r.name)
			return ctx.Err()
		}
	}
	r.log.add("stop:" + r.name)
	return r.stopErr
}

func (r *recorder) Faults() <-chan error { return r.faults }

func TestStartsInOrderAndStopsInReverse(t *testing.T) {
	t.Parallel()

	log := &orderLog{}
	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: time.Second})
	sup.Register(
		&recorder{name: "db", log: log},
		&recorder{name: "bus", log: log},
		&recorder{name: "http", log: log},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	// Wait until everything is up before signalling shutdown.
	waitFor(t, func() bool { return len(log.snapshot()) == 3 })
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := []string{
		"start:db", "start:bus", "start:http",
		"stop:http", "stop:bus", "stop:db",
	}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Errorf("lifecycle order =\n  %v\nwant\n  %v", got, want)
	}
}

// A half-initialised process is worse than one that refused to start.
func TestFailedStartRollsBackStartedComponents(t *testing.T) {
	t.Parallel()

	log := &orderLog{}
	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: time.Second})
	sup.Register(
		&recorder{name: "db", log: log},
		&recorder{name: "bus", log: log},
		&recorder{name: "scheduler", log: log, startErr: errors.New("no collectors registered")},
		&recorder{name: "http", log: log},
	)

	err := sup.Run(context.Background())
	if err == nil {
		t.Fatal("Run() returned nil for a failed start")
	}
	if !strings.Contains(err.Error(), "scheduler") {
		t.Errorf("error should name the failing component, got: %v", err)
	}

	want := []string{
		"start:db", "start:bus", "start:scheduler",
		// scheduler failed, so it is never stopped; http never started.
		"stop:bus", "stop:db",
	}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Errorf("rollback order =\n  %v\nwant\n  %v", got, want)
	}
}

func TestRuntimeFaultTriggersOrderlyShutdown(t *testing.T) {
	t.Parallel()

	log := &orderLog{}
	faults := make(chan error, 1)
	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: time.Second})
	sup.Register(
		&recorder{name: "db", log: log},
		&recorder{name: "http", log: log, faults: faults},
	)

	done := make(chan error, 1)
	go func() { done <- sup.Run(context.Background()) }()

	waitFor(t, func() bool { return len(log.snapshot()) == 2 })
	listenErr := errors.New("listen tcp :8080: address already in use")
	faults <- listenErr

	err := <-done
	if !errors.Is(err, listenErr) {
		t.Errorf("Run() error = %v, want it to wrap the reported fault", err)
	}
	want := []string{"start:db", "start:http", "stop:http", "stop:db"}
	if got := log.snapshot(); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// Deriving the shutdown context from the cancelled run context would give
// every Stop an expired deadline. This asserts components really do get their
// full drain budget.
func TestStopReceivesLiveDeadlineAfterCancellation(t *testing.T) {
	t.Parallel()

	log := &orderLog{}
	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: 2 * time.Second})
	sup.Register(&recorder{name: "draining", log: log, stopFor: 100 * time.Millisecond})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitFor(t, func() bool { return len(log.snapshot()) == 1 })
	cancel()

	if err := <-done; err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if got := log.snapshot(); !slices.Contains(got, "stop:draining") {
		t.Errorf("component was cut off instead of draining: %v", got)
	}
}

func TestShutdownDeadlineIsBounded(t *testing.T) {
	t.Parallel()

	log := &orderLog{}
	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: 100 * time.Millisecond})
	sup.Register(
		&recorder{name: "db", log: log},
		&recorder{name: "wedged", log: log, stopFor: time.Hour},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitFor(t, func() bool { return len(log.snapshot()) == 2 })

	start := time.Now()
	cancel()
	err := <-done
	elapsed := time.Since(start)

	if elapsed > 3*time.Second {
		t.Fatalf("shutdown took %v; the timeout should have bounded it", elapsed)
	}
	if err == nil {
		t.Fatal("Run() returned nil despite a component that would not stop")
	}
	if !strings.Contains(err.Error(), "deadline exceeded") {
		t.Errorf("error should report the exceeded deadline, got: %v", err)
	}
	if got := log.snapshot(); slices.Contains(got, "stop:db") {
		t.Error("supervisor kept stopping after the budget was spent")
	}
}

// Stop errors must not abort the sequence: every component still gets a
// chance to release its resources.
func TestStopContinuesAfterAnError(t *testing.T) {
	t.Parallel()

	log := &orderLog{}
	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: time.Second})
	sup.Register(
		&recorder{name: "db", log: log},
		&recorder{name: "flaky", log: log, stopErr: errors.New("close failed")},
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	waitFor(t, func() bool { return len(log.snapshot()) == 2 })
	cancel()

	err := <-done
	if err == nil {
		t.Fatal("Run() should report the stop failure")
	}
	if got := log.snapshot(); !slices.Contains(got, "stop:db") {
		t.Errorf("db was not stopped after flaky failed: %v", got)
	}
}

func TestRegisterAfterRunPanics(t *testing.T) {
	t.Parallel()

	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sup.Run(ctx) }()

	defer func() {
		if recover() == nil {
			t.Error("Register after Run should panic")
		}
		cancel()
		<-done
	}()

	waitFor(t, func() bool { return true })
	time.Sleep(20 * time.Millisecond) // let Run mark itself running
	sup.Register(lifecycle.Func{ComponentName: "late"})
}

func TestFuncAdapter(t *testing.T) {
	t.Parallel()

	var started, stopped bool
	sup := lifecycle.New(lifecycle.Options{ShutdownTimeout: time.Second})
	sup.Register(lifecycle.Func{
		ComponentName: "adapter",
		OnStart:       func(context.Context) error { started = true; return nil },
		OnStop:        func(context.Context) error { stopped = true; return nil },
	})
	// A Func with neither hook must be valid.
	sup.Register(lifecycle.Func{ComponentName: "noop"})

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := sup.Run(ctx); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !started || !stopped {
		t.Errorf("started = %v, stopped = %v; want both true", started, stopped)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}
