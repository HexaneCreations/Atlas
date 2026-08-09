package scheduler

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
)

// cardinalityGuard bounds how many distinct series each collector may produce.
//
// # Why this exists
//
// Unbounded label cardinality is the most common way a metrics platform is
// destroyed, and it is almost always a one-line bug in a collector: labelling
// by process id, container id, request path, or a timestamp. Every distinct
// label combination is a distinct series, so a single such label turns a
// handful of series into millions. The hypertable, its indexes, and every
// continuous aggregate built on it grow without limit until queries time out
// and the database becomes unusable.
//
// By the time the symptom is visible, the damage is already stored. The only
// effective defence is refusing the writes, and the only place that sees every
// sample from every collector is here.
//
// # The policy
//
// Each collector gets a budget of distinct series. Once it is spent, series
// already being reported keep flowing and *new* ones are dropped. That
// direction matters: dropping the newest preserves the continuity of the
// series an operator is already watching, whereas evicting the oldest would
// silently swap out the metrics they built a dashboard on.
//
// Series not observed for a while are forgotten, so legitimate churn — a
// filesystem unmounted, a network interface renamed — does not permanently
// consume budget that no longer corresponds to anything.
type cardinalityGuard struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	seen    map[string]map[string]time.Time // collector -> series key -> last seen
	dropped map[string]uint64               // collector -> total series refused
	// exceeded records collectors currently over budget, so the transition
	// into that state can be reported once rather than on every run.
	exceeded map[string]bool
}

func newCardinalityGuard(limit int, window time.Duration, now func() time.Time) *cardinalityGuard {
	if now == nil {
		now = time.Now
	}
	return &cardinalityGuard{
		limit:    limit,
		window:   window,
		now:      now,
		seen:     make(map[string]map[string]time.Time),
		dropped:  make(map[string]uint64),
		exceeded: make(map[string]bool),
	}
}

// admitted is the outcome of applying the budget to one collection run.
type admitted struct {
	// Samples are those permitted through.
	Samples []collect.Sample
	// Refused counts samples dropped in this run for exceeding the budget.
	Refused int
	// NewlyExceeded is true only on the run where a collector first goes over
	// budget, so the condition is reported once instead of every interval.
	NewlyExceeded bool
	// Series is the collector's current distinct-series count.
	Series int
}

// admit applies the budget to one collector's samples.
//
// A limit of zero or less disables enforcement, which exists so the guard can
// be turned off in a deployment that genuinely needs unbounded series and has
// accepted the consequences.
func (g *cardinalityGuard) admit(collectorID string, samples []collect.Sample) admitted {
	if g.limit <= 0 {
		return admitted{Samples: samples, Series: len(samples)}
	}

	now := g.now()

	g.mu.Lock()
	defer g.mu.Unlock()

	series, ok := g.seen[collectorID]
	if !ok {
		series = make(map[string]time.Time, len(samples))
		g.seen[collectorID] = series
	}

	// Forget series that have stopped reporting, so a host that churns through
	// devices does not permanently exhaust the budget with entries that no
	// longer exist.
	g.pruneLocked(series, now)

	kept := make([]collect.Sample, 0, len(samples))
	refused := 0

	for _, s := range samples {
		key := seriesKey(s)

		if _, known := series[key]; known {
			series[key] = now
			kept = append(kept, s)
			continue
		}
		if len(series) < g.limit {
			series[key] = now
			kept = append(kept, s)
			continue
		}
		refused++
	}

	result := admitted{Samples: kept, Refused: refused, Series: len(series)}

	if refused > 0 {
		g.dropped[collectorID] += uint64(refused)
		if !g.exceeded[collectorID] {
			g.exceeded[collectorID] = true
			result.NewlyExceeded = true
		}
	} else if len(series) < g.limit {
		// Back within budget: allow the condition to be reported again if it
		// recurs, rather than staying silent forever after the first breach.
		g.exceeded[collectorID] = false
	}

	return result
}

func (g *cardinalityGuard) pruneLocked(series map[string]time.Time, now time.Time) {
	if g.window <= 0 {
		return
	}
	cutoff := now.Add(-g.window)
	for key, lastSeen := range series {
		if lastSeen.Before(cutoff) {
			delete(series, key)
		}
	}
}

// stats reports a collector's current series count and lifetime refusals.
func (g *cardinalityGuard) stats(collectorID string) (series int, dropped uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.seen[collectorID]), g.dropped[collectorID]
}

// forget drops a collector's tracking entirely.
func (g *cardinalityGuard) forget(collectorID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.seen, collectorID)
	delete(g.dropped, collectorID)
	delete(g.exceeded, collectorID)
}

// seriesKey renders a sample's identity: its metric name plus its labels.
//
// Labels are sorted so that the same combination always produces the same key
// regardless of map iteration order. The separators are characters that cannot
// appear in a metric name or a well-formed label, so no combination of values
// can be crafted to collide with a different one.
func seriesKey(s collect.Sample) string {
	if len(s.Labels) == 0 {
		return s.Metric
	}

	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(s.Labels))
	for k, v := range s.Labels {
		pairs = append(pairs, kv{k, v})
	}
	slices.SortFunc(pairs, func(a, b kv) int { return cmp.Compare(a.k, b.k) })

	var b strings.Builder
	b.Grow(len(s.Metric) + len(pairs)*24)
	b.WriteString(s.Metric)
	for _, p := range pairs {
		b.WriteByte(0x1f) // unit separator
		b.WriteString(p.k)
		b.WriteByte(0x1e) // record separator
		b.WriteString(p.v)
	}
	return b.String()
}
