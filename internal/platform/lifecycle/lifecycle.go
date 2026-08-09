// Package lifecycle sequences the startup and shutdown of long-lived
// components.
//
// Atlas is a composition of independent parts — a database pool, an event bus,
// a collector scheduler, an HTTP server — with real ordering constraints
// between them. The pool must be up before the scheduler writes samples; the
// HTTP server must stop accepting before the pool closes, or in-flight
// requests fail against a dead connection.
//
// A [Supervisor] makes that ordering explicit and enforces three rules that
// are easy to state and tedious to get right by hand:
//
//   - Start in registration order, stop in exactly the reverse. Dependencies
//     therefore outlive their dependents at both ends.
//   - A failure partway through startup rolls back whatever already started.
//     The process never lingers half-initialised.
//   - Shutdown runs under one shared deadline, not a deadline per component,
//     so total shutdown time is bounded no matter how many components exist.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Component is a long-lived part of the process.
//
// Start must not block. A component whose work is ongoing launches its own
// goroutine and returns; the [Supervisor] is what blocks. This keeps startup
// ordering meaningful — "started" has to mean "ready for the next component
// to depend on", which a blocking Start could never signal.
//
// Stop must be idempotent and must respect its context's deadline. A Stop
// that ignores cancellation turns a bounded shutdown into an unbounded one.
type Component interface {
	// Name identifies the component in logs. Use the package-ish name of the
	// thing, such as "postgres.pool" or "http.server".
	Name() string
	// Start brings the component up. It must return promptly.
	Start(ctx context.Context) error
	// Stop shuts the component down, draining if that is meaningful.
	Stop(ctx context.Context) error
}

// FaultReporter is implemented by components that can fail after a successful
// start — an HTTP listener whose socket dies, a pool that loses its server.
//
// A fault is fatal to the process: the supervisor logs it and begins an
// orderly shutdown. Components that can recover on their own should do so
// and not report, since reporting means "I cannot continue".
type FaultReporter interface {
	// Faults delivers at most one error per component lifetime. Returning a
	// nil channel means the component never reports faults.
	Faults() <-chan error
}

// Supervisor starts, supervises, and stops a set of components.
type Supervisor struct {
	logger          *slog.Logger
	shutdownTimeout time.Duration

	mu         sync.Mutex
	components []Component
	started    []Component
	running    bool
}

// Options configures a [Supervisor].
type Options struct {
	// Logger records lifecycle transitions. Defaults to a discarding logger.
	Logger *slog.Logger
	// ShutdownTimeout bounds the whole shutdown sequence. Defaults to 30s.
	ShutdownTimeout time.Duration
}

// New builds a Supervisor.
func New(opts Options) *Supervisor {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 30 * time.Second
	}
	return &Supervisor{logger: opts.Logger, shutdownTimeout: opts.ShutdownTimeout}
}

// Register adds a component. Order is significant: components start in the
// order registered and stop in the reverse, so register a dependency before
// whatever depends on it.
//
// Register panics if called after [Supervisor.Run] has begun. Composition is
// a startup-time activity; a component appearing mid-run would have no
// defined position in the shutdown order, and a silent misordering is far
// worse to debug than an immediate panic in main.
func (s *Supervisor) Register(components ...Component) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running {
		panic("lifecycle: Register called after Run; components must be registered during composition")
	}
	s.components = append(s.components, components...)
}

// Run starts every component, blocks until ctx is cancelled or a component
// reports a fault, then shuts everything down in reverse order.
//
// The returned error joins the fault that triggered shutdown, if any, with
// any errors from stopping components. A nil return means a clean, fully
// drained shutdown.
func (s *Supervisor) Run(ctx context.Context) error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return errors.New("lifecycle: Run called twice")
	}
	s.running = true
	components := make([]Component, len(s.components))
	copy(components, s.components)
	s.mu.Unlock()

	faults := make(chan error, len(components))

	if err := s.startAll(ctx, components, faults); err != nil {
		// Roll back whatever came up before the failure. The shutdown budget
		// applies here too: a failed start must not hang the process either.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
		defer cancel()
		return errors.Join(err, s.stopAll(stopCtx))
	}

	s.logger.InfoContext(ctx, "all components started", slog.Int("count", len(components)))

	var trigger error
	select {
	case <-ctx.Done():
		s.logger.InfoContext(ctx, "shutdown signal received, draining")
	case err := <-faults:
		trigger = err
		s.logger.ErrorContext(ctx, "component fault, shutting down", slog.Any("error", err))
	}

	// context.WithoutCancel is essential: ctx is already cancelled on the
	// normal path, and deriving the shutdown context from it would give every
	// Stop an already-expired deadline — turning graceful drain into an
	// immediate kill.
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.shutdownTimeout)
	defer cancel()

	return errors.Join(trigger, s.stopAll(stopCtx))
}

func (s *Supervisor) startAll(ctx context.Context, components []Component, faults chan<- error) error {
	for _, c := range components {
		s.logger.InfoContext(ctx, "starting component", slog.String("component", c.Name()))

		if err := c.Start(ctx); err != nil {
			return fmt.Errorf("lifecycle: start %s: %w", c.Name(), err)
		}

		s.mu.Lock()
		s.started = append(s.started, c)
		s.mu.Unlock()

		s.watch(c, faults)
	}
	return nil
}

// watch forwards a component's first fault onto the supervisor's channel.
func (s *Supervisor) watch(c Component, faults chan<- error) {
	reporter, ok := c.(FaultReporter)
	if !ok {
		return
	}
	ch := reporter.Faults()
	if ch == nil {
		return
	}
	go func() {
		// A closed channel yields the zero value; only a real error is a
		// fault, so a component may close its channel to say "nothing to
		// report" without triggering a shutdown.
		if err, ok := <-ch; ok && err != nil {
			faults <- fmt.Errorf("lifecycle: %s failed: %w", c.Name(), err)
		}
	}()
}

// stopAll stops started components in reverse order under a shared deadline.
func (s *Supervisor) stopAll(ctx context.Context) error {
	s.mu.Lock()
	started := s.started
	s.started = nil
	s.mu.Unlock()

	var errs []error
	for i := len(started) - 1; i >= 0; i-- {
		c := started[i]
		s.logger.InfoContext(ctx, "stopping component", slog.String("component", c.Name()))

		if err := c.Stop(ctx); err != nil {
			s.logger.ErrorContext(ctx, "component did not stop cleanly",
				slog.String("component", c.Name()), slog.Any("error", err))
			errs = append(errs, fmt.Errorf("lifecycle: stop %s: %w", c.Name(), err))
		}

		// Keep going after a failure — every remaining component still gets
		// its chance to release resources — but abandon the sequence once the
		// shared budget is spent, which is the bound we promised.
		if ctx.Err() != nil {
			remaining := i
			if remaining > 0 {
				err := fmt.Errorf("lifecycle: shutdown deadline exceeded with %d component(s) unstopped", remaining)
				s.logger.ErrorContext(ctx, "shutdown deadline exceeded", slog.Int("unstopped", remaining))
				errs = append(errs, err)
			}
			break
		}
	}
	return errors.Join(errs...)
}

// Func adapts plain functions to [Component], for parts that need no state of
// their own.
type Func struct {
	// ComponentName is returned by Name.
	ComponentName string
	// OnStart is called at startup. Optional.
	OnStart func(ctx context.Context) error
	// OnStop is called at shutdown. Optional.
	OnStop func(ctx context.Context) error
}

// Name implements [Component].
func (f Func) Name() string { return f.ComponentName }

// Start implements [Component].
func (f Func) Start(ctx context.Context) error {
	if f.OnStart == nil {
		return nil
	}
	return f.OnStart(ctx)
}

// Stop implements [Component].
func (f Func) Stop(ctx context.Context) error {
	if f.OnStop == nil {
		return nil
	}
	return f.OnStop(ctx)
}
