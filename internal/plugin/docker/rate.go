package docker

import (
	"sync"
	"time"
)

// rateTracker converts cumulative counters into per-second rates.
//
// A copy of the system plugin's tracker rather than a shared package: the two
// are eight lines of arithmetic each, and a shared "utils" package that both
// plugins depend on is the first step toward a core that knows about Docker.
// Plugins staying independent is worth this much duplication.
//
// Most interesting system metrics are counters: bytes received, sectors
// written, pages swapped. A counter's absolute value is nearly meaningless on
// its own — "this interface has received 4 TB since boot" answers no question
// an operator has. The rate of change is the signal.
//
// Rates could be computed at query time in SQL, and for historical charts they
// will be. They are also computed here, at collection time, because the
// overview page and the alerting path both need a current rate without running
// a windowed query first, and because the collector is the only place that
// knows a counter reset means a reboot rather than a negative rate.
//
// A tracker is safe for concurrent use.
type rateTracker struct {
	mu   sync.Mutex
	prev map[string]reading
}

type reading struct {
	value float64
	at    time.Time
}

func newRateTracker() *rateTracker {
	return &rateTracker{prev: make(map[string]reading)}
}

// rate records a counter reading and returns its per-second rate since the
// previous reading for the same key.
//
// ok is false when no rate can be computed, which happens in three cases that
// must not be reported as zero:
//
//   - The first reading for a key. There is nothing to compare against.
//   - A counter that went backwards. The source restarted, the interface was
//     recreated, or the device was re-enumerated. Reporting the negative delta
//     would draw a spike downward; reporting zero would claim the device went
//     idle. Neither happened, so nothing is reported.
//   - Two readings at the same instant, which would divide by zero.
//
// Returning ok=false rather than a zero rate is what keeps a reboot from
// appearing as a traffic outage on the chart.
func (t *rateTracker) rate(key string, value float64, now time.Time) (float64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	last, seen := t.prev[key]
	t.prev[key] = reading{value: value, at: now}

	if !seen {
		return 0, false
	}
	if value < last.value {
		return 0, false // counter reset
	}

	elapsed := now.Sub(last.at).Seconds()
	if elapsed <= 0 {
		return 0, false
	}
	return (value - last.value) / elapsed, true
}

// forget drops a key's history.
//
// Called when a device or interface disappears, so the tracker does not grow
// without bound on a host that churns through network namespaces or loop
// devices — a container host can create and destroy thousands over a week.
func (t *rateTracker) forget(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.prev, key)
}

// retain drops every key not present in the supplied set.
//
// Collectors call this after each run with the devices they observed, which
// bounds memory to the number of devices currently present rather than the
// number ever seen.
func (t *rateTracker) retain(keys map[string]struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()

	for key := range t.prev {
		if _, ok := keys[key]; !ok {
			delete(t.prev, key)
		}
	}
}

// len reports how many counters are being tracked. For tests.
func (t *rateTracker) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.prev)
}
