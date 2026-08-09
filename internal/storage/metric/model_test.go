package metric_test

import (
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/storage/metric"
)

// The storage layer is the floor every feature stands on: alerting, SLOs and
// incident history will all read through it. These tests pin the parts that
// decide what a caller sees — liveness classification, query normalisation and
// resolution selection — none of which need a database to exercise.

// Liveness is expressed in multiples of the collection interval, not fixed
// durations. A deployment collecting once a minute must not be permanently
// reported as stale, which a hard-coded threshold would do.
func TestNodeStatusScalesWithCollectionInterval(t *testing.T) {
	t.Parallel()

	now := time.Now()

	cases := []struct {
		name     string
		interval time.Duration
		silence  time.Duration
		want     metric.NodeStatus
	}{
		{"just reported", 15 * time.Second, time.Second, metric.StatusUp},
		{"within three intervals", 15 * time.Second, 40 * time.Second, metric.StatusUp},
		{"past three intervals", 15 * time.Second, 50 * time.Second, metric.StatusStale},
		{"past ten intervals", 15 * time.Second, 3 * time.Minute, metric.StatusDown},

		// The same silence against a slower cadence is healthy. This is the
		// case a fixed threshold gets wrong.
		{"slow cadence, same silence", 2 * time.Minute, 3 * time.Minute, metric.StatusUp},
		{"slow cadence, stale", 2 * time.Minute, 7 * time.Minute, metric.StatusStale},
		{"slow cadence, down", 2 * time.Minute, 25 * time.Minute, metric.StatusDown},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			n := metric.Node{LastSeenAt: now.Add(-c.silence)}
			if got := n.Status(c.interval, now); got != c.want {
				t.Errorf("silence %v at interval %v = %s, want %s",
					c.silence, c.interval, got, c.want)
			}
		})
	}
}

// A zero or negative interval must not divide the thresholds into nonsense —
// it comes from configuration and can legitimately be unset.
func TestNodeStatusToleratesUnsetInterval(t *testing.T) {
	t.Parallel()

	now := time.Now()
	n := metric.Node{LastSeenAt: now.Add(-time.Second)}

	for _, interval := range []time.Duration{0, -time.Minute} {
		if got := n.Status(interval, now); got != metric.StatusUp {
			t.Errorf("interval %v = %s, want up via the default", interval, got)
		}
	}
}

func TestNodeUptimeSeconds(t *testing.T) {
	t.Parallel()

	// A node that never reported a boot time has no uptime to report, and
	// zero is the honest answer rather than "up since the epoch".
	if got := (metric.Node{}).UptimeSeconds(); got != 0 {
		t.Errorf("uptime with no boot time = %v, want 0", got)
	}

	n := metric.Node{BootTime: time.Now().Add(-2 * time.Hour)}
	if got := n.UptimeSeconds(); got < 7000 || got > 7400 {
		t.Errorf("uptime = %v, want about 7200", got)
	}
}

func TestQueryNormalizeRequiresNode(t *testing.T) {
	t.Parallel()

	q := metric.Query{}
	err := q.Normalize(time.Now())
	assertInvalidArgument(t, err, "node")
}

func TestQueryNormalizeFillsDefaults(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	q := metric.Query{NodeID: "n1"}

	if err := q.Normalize(now); err != nil {
		t.Fatalf("Normalize: %v", err)
	}

	if !q.To.Equal(now) {
		t.Errorf("To = %v, want now", q.To)
	}
	// An unset range defaults to the last hour, which is the window every
	// dashboard opens on.
	if got := q.To.Sub(q.From); got != time.Hour {
		t.Errorf("default span = %v, want 1h", got)
	}
	if q.MaxPoints != metric.DefaultMaxPoints {
		t.Errorf("MaxPoints = %d, want %d", q.MaxPoints, metric.DefaultMaxPoints)
	}
	if q.Resolution != metric.ResolutionRaw {
		t.Errorf("Resolution = %s, want raw for a one-hour span", q.Resolution)
	}
}

func TestQueryNormalizeRejectsInvertedRange(t *testing.T) {
	t.Parallel()

	now := time.Now()
	q := metric.Query{NodeID: "n1", From: now, To: now.Add(-time.Hour)}
	assertInvalidArgument(t, q.Normalize(now), "from")

	// Equal bounds are empty rather than inverted, and equally unanswerable.
	same := metric.Query{NodeID: "n1", From: now, To: now}
	assertInvalidArgument(t, same.Normalize(now), "from")
}

// The point cap protects the browser as much as the database: a chart cannot
// usefully draw more points than it has pixels.
func TestQueryNormalizeEnforcesPointCeiling(t *testing.T) {
	t.Parallel()

	q := metric.Query{NodeID: "n1", MaxPoints: metric.MaxAllowedPoints + 1}
	assertInvalidArgument(t, q.Normalize(time.Now()), "max_points")

	atLimit := metric.Query{NodeID: "n1", MaxPoints: metric.MaxAllowedPoints}
	if err := atLimit.Normalize(time.Now()); err != nil {
		t.Errorf("the ceiling itself was rejected: %v", err)
	}
}

func TestQueryNormalizeRejectsUnknownResolution(t *testing.T) {
	t.Parallel()

	q := metric.Query{NodeID: "n1", Resolution: metric.Resolution("5s")}
	assertInvalidArgument(t, q.Normalize(time.Now()), "resolution")
}

// An explicit resolution must survive normalisation: a caller asking for
// hourly rollups over a short range is downsampling deliberately.
func TestQueryNormalizeKeepsExplicitResolution(t *testing.T) {
	t.Parallel()

	q := metric.Query{NodeID: "n1", Resolution: metric.ResolutionHour}
	if err := q.Normalize(time.Now()); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if q.Resolution != metric.ResolutionHour {
		t.Errorf("Resolution = %s, want the requested 1h", q.Resolution)
	}
}

// Resolution is chosen from the range width against the point budget. Six
// hours of fifteen-second samples is already about 1,440 points — the most a
// chart can use — so wider ranges read a rollup instead.
func TestAutoResolutionFollowsRangeWidth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		span time.Duration
		want metric.Resolution
	}{
		{time.Hour, metric.ResolutionRaw},
		{6 * time.Hour, metric.ResolutionRaw},
		{6*time.Hour + time.Minute, metric.ResolutionMinute},
		{14 * 24 * time.Hour, metric.ResolutionMinute},
		{15 * 24 * time.Hour, metric.ResolutionHour},
		{365 * 24 * time.Hour, metric.ResolutionHour},
	}

	now := time.Now()
	for _, c := range cases {
		q := metric.Query{NodeID: "n1", From: now.Add(-c.span), To: now}
		if err := q.Normalize(now); err != nil {
			t.Fatalf("span %v: %v", c.span, err)
		}
		if q.Resolution != c.want {
			t.Errorf("span %v selected %s, want %s", c.span, q.Resolution, c.want)
		}
	}
}

func TestResolutionInterval(t *testing.T) {
	t.Parallel()

	if got := metric.ResolutionMinute.Interval(); got != time.Minute {
		t.Errorf("1m interval = %v", got)
	}
	if got := metric.ResolutionHour.Interval(); got != time.Hour {
		t.Errorf("1h interval = %v", got)
	}
	// Raw has no fixed interval — samples arrive at whatever cadence the
	// collector runs — and must not claim one.
	if got := metric.ResolutionRaw.Interval(); got != 0 {
		t.Errorf("raw interval = %v, want 0", got)
	}
}

func assertInvalidArgument(t *testing.T, err error, field string) {
	t.Helper()

	if err == nil {
		t.Fatalf("expected an invalid-argument error for %q, got nil", field)
	}

	var apiErr *errs.Error
	if !errs.As(err, &apiErr) {
		t.Fatalf("error is not typed: %T", err)
	}
	if apiErr.Code != errs.CodeInvalidArgument {
		t.Errorf("code = %s, want %s", apiErr.Code, errs.CodeInvalidArgument)
	}
	// The field name is what lets a caller point at the offending parameter
	// rather than restating the whole request.
	if got := apiErr.Details["field"]; got != field {
		t.Errorf("details.field = %v, want %q", got, field)
	}
}
