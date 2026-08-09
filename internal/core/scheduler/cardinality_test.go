package scheduler

import (
	"strconv"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

func sampleWith(metric string, labels map[string]string) collect.Sample {
	return collect.Sample{
		Metric: metric, Value: 1,
		Unit: collect.UnitCount, Kind: collect.KindGauge,
		Time: time.Now(), Labels: labels,
	}
}

// Series identity must not depend on Go's map iteration order, or the same
// series would consume budget repeatedly and trip the limit spuriously.
func TestSeriesKeyIsOrderIndependent(t *testing.T) {
	t.Parallel()

	a := sampleWith("system.disk.usage", map[string]string{
		"device": "sda1", "mountpoint": "/", "fstype": "ext4",
	})
	b := sampleWith("system.disk.usage", map[string]string{
		"fstype": "ext4", "mountpoint": "/", "device": "sda1",
	})

	if seriesKey(a) != seriesKey(b) {
		t.Errorf("same labels in a different order produced different keys:\n  %q\n  %q",
			seriesKey(a), seriesKey(b))
	}
}

// Distinct series must never collide, or the budget would under-count and a
// runaway collector would slip past it.
func TestSeriesKeyDistinguishesDifferentSeries(t *testing.T) {
	t.Parallel()

	samples := []collect.Sample{
		sampleWith("m", nil),
		sampleWith("m", map[string]string{"a": "1"}),
		sampleWith("m", map[string]string{"a": "2"}),
		sampleWith("m", map[string]string{"b": "1"}),
		sampleWith("m", map[string]string{"a": "1", "b": "2"}),
		sampleWith("other", map[string]string{"a": "1"}),
		// Values chosen so a naive concatenation would collide.
		sampleWith("m", map[string]string{"a": "1b", "": "2"}),
		sampleWith("m", map[string]string{"a": "1", "b": "2x"}),
	}

	seen := make(map[string]int, len(samples))
	for i, s := range samples {
		key := seriesKey(s)
		if prev, dup := seen[key]; dup {
			t.Errorf("samples %d and %d collided on key %q", prev, i, key)
		}
		seen[key] = i
	}
}

// The core guarantee: a collector that labels by an unbounded value must not
// be able to write unlimited series.
func TestGuardStopsRunawayCardinality(t *testing.T) {
	t.Parallel()

	const limit = 100
	g := newCardinalityGuard(limit, time.Hour, time.Now)

	// A collector labelling by process id, the classic mistake.
	runaway := make([]collect.Sample, 0, 5000)
	for i := range 5000 {
		runaway = append(runaway, sampleWith("process.cpu", map[string]string{"pid": strconv.Itoa(i)}))
	}

	got := g.admit("bad.collector", runaway)

	if len(got.Samples) != limit {
		t.Errorf("admitted %d samples, want the limit of %d", len(got.Samples), limit)
	}
	if got.Refused != 5000-limit {
		t.Errorf("Refused = %d, want %d", got.Refused, 5000-limit)
	}
	if !got.NewlyExceeded {
		t.Error("NewlyExceeded = false; the breach would never be reported")
	}
	if got.Series != limit {
		t.Errorf("Series = %d, want %d", got.Series, limit)
	}
}

// Dropping the newest preserves the series an operator already has on a
// dashboard; evicting the oldest would silently swap them out.
func TestGuardKeepsEstablishedSeriesAndDropsNewOnes(t *testing.T) {
	t.Parallel()

	g := newCardinalityGuard(3, time.Hour, time.Now)

	established := []collect.Sample{
		sampleWith("m", map[string]string{"d": "sda"}),
		sampleWith("m", map[string]string{"d": "sdb"}),
		sampleWith("m", map[string]string{"d": "sdc"}),
	}
	if got := g.admit("c", established); len(got.Samples) != 3 {
		t.Fatalf("admitted %d of 3 established series", len(got.Samples))
	}

	// The established three must keep flowing; the new one must not.
	mixed := append(append([]collect.Sample{}, established...),
		sampleWith("m", map[string]string{"d": "sdd"}))

	got := g.admit("c", mixed)
	if len(got.Samples) != 3 {
		t.Errorf("admitted %d samples, want the 3 established ones", len(got.Samples))
	}
	if got.Refused != 1 {
		t.Errorf("Refused = %d, want 1", got.Refused)
	}
	for _, s := range got.Samples {
		if s.Labels["d"] == "sdd" {
			t.Error("the new series was admitted over an established one")
		}
	}
}

// A collector within its budget must be entirely unaffected.
func TestGuardIsTransparentBelowTheLimit(t *testing.T) {
	t.Parallel()

	g := newCardinalityGuard(1000, time.Hour, time.Now)

	samples := []collect.Sample{
		sampleWith("system.cpu.usage", nil),
		sampleWith("system.memory.usage", nil),
		sampleWith("system.disk.usage", map[string]string{"mountpoint": "/"}),
	}

	for range 100 { // repeated runs must not accumulate budget
		got := g.admit("system", samples)
		if len(got.Samples) != len(samples) {
			t.Fatalf("admitted %d of %d samples", len(got.Samples), len(samples))
		}
		if got.Refused != 0 || got.NewlyExceeded {
			t.Fatalf("a well-behaved collector was penalised: %+v", got)
		}
	}

	if series, dropped := g.stats("system"); series != 3 || dropped != 0 {
		t.Errorf("stats = %d series, %d dropped; want 3 and 0", series, dropped)
	}
}

// Legitimate churn — an unmounted filesystem, a renamed interface — must
// release budget rather than consuming it permanently.
func TestGuardForgetsSeriesThatStopReporting(t *testing.T) {
	t.Parallel()

	now := time.Now()
	clock := func() time.Time { return now }
	g := newCardinalityGuard(2, 30*time.Minute, clock)

	first := []collect.Sample{
		sampleWith("m", map[string]string{"mount": "/old-a"}),
		sampleWith("m", map[string]string{"mount": "/old-b"}),
	}
	if got := g.admit("disk", first); got.Refused != 0 {
		t.Fatalf("initial series were refused: %+v", got)
	}

	// While they are still recent, a new mount cannot fit.
	newMount := []collect.Sample{sampleWith("m", map[string]string{"mount": "/new"})}
	if got := g.admit("disk", newMount); got.Refused != 1 {
		t.Errorf("a new series was admitted while the budget was full: %+v", got)
	}

	// After the window passes with no reports, the old series are forgotten.
	now = now.Add(time.Hour)
	got := g.admit("disk", newMount)
	if got.Refused != 0 {
		t.Errorf("Refused = %d after the old series aged out, want 0", got.Refused)
	}
	if got.Series != 1 {
		t.Errorf("Series = %d, want 1 after pruning", got.Series)
	}
}

// The breach must be reported once, not on every run — otherwise a runaway
// collector floods the event bus with the news that it is flooding.
func TestGuardReportsBreachOnceThenRecovers(t *testing.T) {
	t.Parallel()

	now := time.Now()
	clock := func() time.Time { return now }
	g := newCardinalityGuard(2, 30*time.Minute, clock)

	over := []collect.Sample{
		sampleWith("m", map[string]string{"x": "1"}),
		sampleWith("m", map[string]string{"x": "2"}),
		sampleWith("m", map[string]string{"x": "3"}),
	}

	if got := g.admit("c", over); !got.NewlyExceeded {
		t.Fatal("the first breach was not reported")
	}
	for range 5 {
		if got := g.admit("c", over); got.NewlyExceeded {
			t.Error("the breach was reported more than once")
		}
	}

	// Once the collector is back within budget, a future breach is reportable
	// again rather than silently suppressed forever.
	now = now.Add(time.Hour)
	within := []collect.Sample{sampleWith("m", map[string]string{"x": "1"})}
	if got := g.admit("c", within); got.Refused != 0 {
		t.Fatalf("still refusing after pruning: %+v", got)
	}
	now = now.Add(time.Minute)
	if got := g.admit("c", over); !got.NewlyExceeded {
		t.Error("a recurrence after recovery was not reported")
	}
}

// A negative limit disables enforcement, for deployments that have accepted
// the consequences deliberately.
func TestGuardCanBeDisabled(t *testing.T) {
	t.Parallel()

	g := newCardinalityGuard(-1, time.Hour, time.Now)

	many := make([]collect.Sample, 0, 10000)
	for i := range 10000 {
		many = append(many, sampleWith("m", map[string]string{"i": strconv.Itoa(i)}))
	}

	got := g.admit("c", many)
	if len(got.Samples) != 10000 || got.Refused != 0 {
		t.Errorf("a disabled guard refused samples: admitted %d, refused %d", len(got.Samples), got.Refused)
	}
}

// Budgets are per collector: one runaway must not starve a well-behaved one.
func TestGuardIsolatesCollectors(t *testing.T) {
	t.Parallel()

	g := newCardinalityGuard(10, time.Hour, time.Now)

	runaway := make([]collect.Sample, 0, 100)
	for i := range 100 {
		runaway = append(runaway, sampleWith("m", map[string]string{"i": strconv.Itoa(i)}))
	}
	if got := g.admit("bad", runaway); got.Refused == 0 {
		t.Fatal("the runaway collector was not limited")
	}

	good := []collect.Sample{sampleWith("system.cpu.usage", nil)}
	got := g.admit("good", good)
	if len(got.Samples) != 1 || got.Refused != 0 {
		t.Errorf("a well-behaved collector was affected by another's breach: %+v", got)
	}
}

func TestGuardForget(t *testing.T) {
	t.Parallel()

	g := newCardinalityGuard(5, time.Hour, time.Now)
	g.admit("c", []collect.Sample{sampleWith("m", nil)})

	if series, _ := g.stats("c"); series != 1 {
		t.Fatalf("series = %d, want 1", series)
	}
	g.forget("c")
	if series, dropped := g.stats("c"); series != 0 || dropped != 0 {
		t.Errorf("after forget: %d series, %d dropped; want 0 and 0", series, dropped)
	}
}

func TestGuardConcurrentAdmit(t *testing.T) {
	t.Parallel()

	g := newCardinalityGuard(500, time.Hour, time.Now)
	done := make(chan struct{})

	for w := range 4 {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range 200 {
				g.admit("shared", []collect.Sample{
					sampleWith("m", map[string]string{"w": strconv.Itoa(w), "i": strconv.Itoa(i)}),
				})
				_, _ = g.stats("shared")
			}
		}()
	}
	for range 4 {
		<-done
	}

	if series, _ := g.stats("shared"); series > 500 {
		t.Errorf("series = %d, exceeding the limit under concurrency", series)
	}
}
