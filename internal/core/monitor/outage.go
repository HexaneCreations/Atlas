package monitor

import (
	"context"
	"net"
	"sync"
	"time"
)

// Outage detection thresholds: the probe is treated as offline when, among
// the distinct hosts checked within outageWindow, at least outageMinHosts
// and at least outageRatio of them failed at the network level.
const (
	outageWindow      = 3 * time.Minute
	outageMinHosts    = 3
	outageRatio       = 0.5
	canaryTimeout     = 3 * time.Second
	canaryCacheWindow = 30 * time.Second
)

// ProbeState is whether the prober currently believes it has lost
// outbound connectivity.
type ProbeState struct {
	Offline bool
	Since   *time.Time
}

// OutageDetector tells the prober's own loss of connectivity from many
// sites failing at once, so an outage on atlas-server's side does not mark
// every monitor down.
//
// With no canaries it correlates: the latest result per host is kept, and
// the probe is offline while enough of those are network failures. With
// canaries it dials them instead whenever a network failure is seen
// (cached for canaryCacheWindow): offline exactly when none answers.
type OutageDetector struct {
	canaries []string
	dial     func(ctx context.Context, network, address string) (net.Conn, error)

	mu          sync.Mutex
	latest      map[string]observation
	offline     bool
	since       time.Time
	canaryAt    time.Time
	canaryAlive bool
}

type observation struct {
	at          time.Time
	networkFail bool
}

// NewOutageDetector builds a detector; canaries are host:port.
func NewOutageDetector(canaries []string) *OutageDetector {
	d := &net.Dialer{Timeout: canaryTimeout}
	return &OutageDetector{canaries: canaries, dial: d.DialContext, latest: map[string]observation{}}
}

// Observe records r for host and returns whether the probe is now offline
// and whether that changed.
func (d *OutageDetector) Observe(ctx context.Context, host string, r Result, now time.Time) (offline, changed bool) {
	networkFail := !r.Up && r.Failure.network()

	d.mu.Lock()
	d.latest[host] = observation{at: now, networkFail: networkFail}
	for h, o := range d.latest {
		if now.Sub(o.at) > outageWindow {
			delete(d.latest, h)
		}
	}
	var next bool
	if len(d.canaries) == 0 {
		next = d.correlatedLocked()
	} else {
		next = d.offline
		if networkFail || d.offline {
			if now.Sub(d.canaryAt) >= canaryCacheWindow {
				d.mu.Unlock()
				alive := d.anyCanaryAnswers(ctx)
				d.mu.Lock()
				d.canaryAt, d.canaryAlive = now, alive
			}
			next = !d.canaryAlive
		}
	}
	changed = next != d.offline
	if changed && next {
		d.since = now
	}
	d.offline = next
	d.mu.Unlock()
	return next, changed
}

func (d *OutageDetector) correlatedLocked() bool {
	fails := 0
	for _, o := range d.latest {
		if o.networkFail {
			fails++
		}
	}
	return fails >= outageMinHosts && float64(fails) >= outageRatio*float64(len(d.latest))
}

func (d *OutageDetector) anyCanaryAnswers(ctx context.Context) bool {
	for _, c := range d.canaries {
		cctx, cancel := context.WithTimeout(ctx, canaryTimeout)
		conn, err := d.dial(cctx, "tcp", c)
		cancel()
		if err == nil {
			_ = conn.Close()
			return true
		}
	}
	return false
}

// State is the detector's current verdict.
func (d *OutageDetector) State() ProbeState {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.offline {
		return ProbeState{}
	}
	since := d.since
	return ProbeState{Offline: true, Since: &since}
}
