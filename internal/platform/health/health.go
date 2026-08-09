// Package health aggregates the liveness and readiness of Atlas's own
// dependencies.
//
// The distinction it draws — critical versus non-critical checks — is what
// makes the result actionable. A load balancer needs one bit: should traffic
// come here. An operator needs to know that Atlas is up but its Docker socket
// is unreachable, which is a real problem that is nevertheless no reason to
// take the instance out of rotation.
//
// So a failing critical check makes the whole report unhealthy and drains the
// instance; a failing non-critical check makes it degraded, which is visible
// everywhere but removes nothing from service.
package health

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Status is the outcome of a check or of an entire report.
type Status string

const (
	// StatusHealthy means the subject is fully operational.
	StatusHealthy Status = "healthy"
	// StatusDegraded means something non-critical is failing. Atlas still
	// serves traffic, with reduced visibility.
	StatusDegraded Status = "degraded"
	// StatusUnhealthy means a critical dependency is unavailable. The
	// instance should not receive traffic.
	StatusUnhealthy Status = "unhealthy"
)

// Checker probes one dependency.
//
// Check must respect its context deadline and must be cheap enough to run on
// every readiness probe — an orchestrator calls it every few seconds, so an
// expensive check becomes load of its own.
type Checker interface {
	// Name identifies the dependency, such as "database".
	Name() string
	// Critical reports whether a failure should take the instance out of
	// service. Reserve it for dependencies without which Atlas genuinely
	// cannot function.
	Critical() bool
	// Check probes the dependency. A nil error means healthy.
	Check(ctx context.Context) error
}

// Check is the result of probing one dependency.
type Check struct {
	Name     string `json:"name"`
	Status   Status `json:"status"`
	Critical bool   `json:"critical"`
	// Error is a client-safe explanation of a failure. Empty when healthy.
	Error string `json:"error,omitempty"`
	// DurationMS is how long the probe took. A check that is healthy but
	// slowing down is the earliest warning of a dependency in trouble.
	DurationMS int64 `json:"duration_ms"`
}

// Report is the aggregate result of running every check.
type Report struct {
	Status Status  `json:"status"`
	Checks []Check `json:"checks"`
	// CheckedAt is when the report was produced.
	CheckedAt time.Time `json:"checked_at"`
}

// Healthy reports whether every check passed.
func (r Report) Healthy() bool { return r.Status == StatusHealthy }

// Serving reports whether the instance should receive traffic. Degraded
// instances still serve.
func (r Report) Serving() bool { return r.Status != StatusUnhealthy }

// Registry holds the checks to run.
type Registry struct {
	logger *slog.Logger

	mu       sync.RWMutex
	checkers []Checker
}

// NewRegistry builds an empty registry.
func NewRegistry(logger *slog.Logger) *Registry {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &Registry{logger: logger}
}

// Register adds a checker.
func (r *Registry) Register(checkers ...Checker) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkers = append(r.checkers, checkers...)
}

// Run probes every dependency concurrently and aggregates the result.
//
// Checks run in parallel so the report takes as long as the slowest probe
// rather than the sum of all of them — the difference between a readiness
// endpoint that responds in milliseconds and one that times out as
// dependencies are added.
//
// A panicking check is treated as a failure of that check alone.
func (r *Registry) Run(ctx context.Context) Report {
	r.mu.RLock()
	checkers := make([]Checker, len(r.checkers))
	copy(checkers, r.checkers)
	r.mu.RUnlock()

	results := make([]Check, len(checkers))
	var wg sync.WaitGroup

	for i, c := range checkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runOne(ctx, c)
		}()
	}
	wg.Wait()

	report := Report{Status: StatusHealthy, Checks: results, CheckedAt: time.Now()}
	for _, check := range results {
		if check.Status == StatusHealthy {
			continue
		}
		if check.Critical {
			report.Status = StatusUnhealthy
			break // nothing worse than unhealthy; stop looking
		}
		report.Status = StatusDegraded
	}
	return report
}

func runOne(ctx context.Context, c Checker) (result Check) {
	result = Check{Name: c.Name(), Critical: c.Critical(), Status: StatusHealthy}
	start := time.Now()

	defer func() {
		result.DurationMS = time.Since(start).Milliseconds()
		if rec := recover(); rec != nil {
			result.Status = StatusUnhealthy
			result.Error = "health check panicked"
		}
	}()

	if err := c.Check(ctx); err != nil {
		result.Status = StatusUnhealthy
		// errs.Message, not err.Error: this report is served over HTTP, and
		// a database driver's error text carries host names and credentials.
		result.Error = errs.Message(err)
	}
	return result
}

// Func adapts a function to [Checker].
type Func struct {
	CheckName  string
	IsCritical bool
	Probe      func(ctx context.Context) error
}

// Name implements [Checker].
func (f Func) Name() string { return f.CheckName }

// Critical implements [Checker].
func (f Func) Critical() bool { return f.IsCritical }

// Check implements [Checker].
func (f Func) Check(ctx context.Context) error {
	if f.Probe == nil {
		return nil
	}
	return f.Probe(ctx)
}
