package health_test

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/health"
)

func check(name string, critical bool, err error) health.Checker {
	return health.Func{CheckName: name, IsCritical: critical, Probe: func(context.Context) error { return err }}
}

func TestReportStatusAggregation(t *testing.T) {
	t.Parallel()

	failure := stderrors.New("unreachable")

	tests := []struct {
		name     string
		checkers []health.Checker
		want     health.Status
		serving  bool
	}{
		{
			name:     "no checks",
			checkers: nil,
			want:     health.StatusHealthy,
			serving:  true,
		},
		{
			name:     "all healthy",
			checkers: []health.Checker{check("db", true, nil), check("docker", false, nil)},
			want:     health.StatusHealthy,
			serving:  true,
		},
		{
			// Reduced visibility is not a reason to drain the last working
			// instance.
			name:     "non-critical failure degrades but keeps serving",
			checkers: []health.Checker{check("db", true, nil), check("docker", false, failure)},
			want:     health.StatusDegraded,
			serving:  true,
		},
		{
			name:     "critical failure stops serving",
			checkers: []health.Checker{check("db", true, failure), check("docker", false, nil)},
			want:     health.StatusUnhealthy,
			serving:  false,
		},
		{
			name:     "critical failure outranks degraded",
			checkers: []health.Checker{check("docker", false, failure), check("db", true, failure)},
			want:     health.StatusUnhealthy,
			serving:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := health.NewRegistry(nil)
			r.Register(tt.checkers...)

			report := r.Run(context.Background())
			if report.Status != tt.want {
				t.Errorf("Status = %q, want %q", report.Status, tt.want)
			}
			if report.Serving() != tt.serving {
				t.Errorf("Serving() = %v, want %v", report.Serving(), tt.serving)
			}
			if len(report.Checks) != len(tt.checkers) {
				t.Errorf("Checks = %d, want %d", len(report.Checks), len(tt.checkers))
			}
			if report.CheckedAt.IsZero() {
				t.Error("CheckedAt was not set")
			}
		})
	}
}

// This report is served over HTTP; a driver's error text carries hosts and
// credentials.
func TestCheckErrorsAreRedacted(t *testing.T) {
	t.Parallel()

	const secret = "dial tcp 10.0.0.5:5432: password authentication failed for user atlas"

	r := health.NewRegistry(nil)
	r.Register(
		check("raw-error", true, stderrors.New(secret)),
		check("internal-error", true, errs.Wrap(stderrors.New(secret), errs.CodeInternal, "connect failed")),
		check("client-safe", false, errs.New(errs.CodeUnavailable, "database is unreachable")),
	)

	report := r.Run(context.Background())
	for _, c := range report.Checks {
		if strings.Contains(c.Error, "10.0.0.5") || strings.Contains(c.Error, "password") {
			t.Errorf("check %q leaked internal detail: %q", c.Name, c.Error)
		}
		if c.Error == "" {
			t.Errorf("check %q failed without an explanation", c.Name)
		}
	}

	// A deliberately client-safe message must survive intact.
	for _, c := range report.Checks {
		if c.Name == "client-safe" && c.Error != "database is unreachable" {
			t.Errorf("client-safe message = %q, want it preserved", c.Error)
		}
	}
}

// Sequential checks would make the readiness endpoint slower with every
// dependency added.
func TestChecksRunConcurrently(t *testing.T) {
	t.Parallel()

	const each, count = 100 * time.Millisecond, 5

	r := health.NewRegistry(nil)
	for i := range count {
		r.Register(health.Func{
			CheckName: string(rune('a' + i)),
			Probe: func(ctx context.Context) error {
				select {
				case <-time.After(each):
				case <-ctx.Done():
				}
				return nil
			},
		})
	}

	start := time.Now()
	r.Run(context.Background())
	elapsed := time.Since(start)

	if elapsed > each*3 {
		t.Errorf("Run took %v for %d checks of %v each; they are running sequentially", elapsed, count, each)
	}
}

func TestPanickingCheckIsContained(t *testing.T) {
	t.Parallel()

	r := health.NewRegistry(nil)
	r.Register(
		health.Func{CheckName: "panics", IsCritical: false, Probe: func(context.Context) error {
			panic("probe dereferenced nil")
		}},
		check("healthy", true, nil),
	)

	report := r.Run(context.Background())

	if report.Status != health.StatusDegraded {
		t.Errorf("Status = %q, want degraded", report.Status)
	}
	for _, c := range report.Checks {
		if c.Name == "panics" {
			if c.Status != health.StatusUnhealthy {
				t.Errorf("panicking check status = %q, want unhealthy", c.Status)
			}
			if !strings.Contains(c.Error, "panic") {
				t.Errorf("panicking check error = %q, want it to mention a panic", c.Error)
			}
		}
		if c.Name == "healthy" && c.Status != health.StatusHealthy {
			t.Error("a panic in one check affected another")
		}
	}
}

func TestCheckRecordsDuration(t *testing.T) {
	t.Parallel()

	r := health.NewRegistry(nil)
	r.Register(health.Func{CheckName: "slow", Probe: func(context.Context) error {
		time.Sleep(20 * time.Millisecond)
		return nil
	}})

	report := r.Run(context.Background())
	if report.Checks[0].DurationMS < 10 {
		t.Errorf("DurationMS = %d, want at least 10", report.Checks[0].DurationMS)
	}
}

func TestFuncWithNoProbeIsHealthy(t *testing.T) {
	t.Parallel()

	r := health.NewRegistry(nil)
	r.Register(health.Func{CheckName: "noop"})

	if got := r.Run(context.Background()); !got.Healthy() {
		t.Errorf("Status = %q, want healthy", got.Status)
	}
}
