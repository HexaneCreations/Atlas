package monitor

import (
	"context"

	"github.com/hexane/atlas/internal/platform/errs"
)

// MaxConcurrentTests bounds how many on-demand tests run at once across the
// server. Tests have their own slots, so they can never take one from the
// scheduler's checks.
const MaxConcurrentTests = 4

// Tester runs on-demand checks of a draft or saved configuration. It records
// nothing: no heartbeat, no live state, no event.
type Tester struct {
	checker Checker
	slots   chan struct{}
}

// NewTester builds a tester running at most maxConcurrent checks at once
// through checker, which should be the scheduler's own [Prober] so a test
// behaves exactly like a real check.
func NewTester(checker Checker, maxConcurrent int) *Tester {
	return &Tester{checker: checker, slots: make(chan struct{}, max(maxConcurrent, 1))}
}

// Test runs one check of m. It refuses at once, without waiting, when every
// slot is taken. The timeout and redirect limit are clamped to what a saved
// monitor may have, whatever m says.
func (t *Tester) Test(ctx context.Context, m Monitor, s Secret) (Result, error) {
	select {
	case t.slots <- struct{}{}:
	default:
		return Result{}, errs.New(errs.CodeRateLimited, "too many website tests are running; try again shortly").WithOp("monitor.Tester.Test")
	}
	defer func() { <-t.slots }()
	m.TimeoutSeconds = min(max(m.TimeoutSeconds, 1), MaxTimeoutSeconds)
	m.MaxRedirects = min(max(m.MaxRedirects, 0), MaxRedirects)
	return t.checker.Check(ctx, m, s), nil
}
