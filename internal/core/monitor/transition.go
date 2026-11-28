package monitor

import "time"

// Counters is a monitor's persisted probe state.
type Counters struct {
	Status               Status
	ConsecutiveFailures  int
	ConsecutiveSuccesses int
	StatusSince          time.Time
	DownSince            *time.Time
	// LastCheckAt and CertExpiresAt are what the previous check saw; they
	// decide whether this one crossed a certificate threshold.
	LastCheckAt   *time.Time
	CertExpiresAt *time.Time
}

// EventKind is a recorded up/down transition.
type EventKind string

const (
	EventDown EventKind = "down"
	EventUp   EventKind = "up"
	// EventCertWarn and EventCertCrit: the certificate entered the warning
	// or critical window. EventCertRenewed: it left them, renewed.
	EventCertWarn    EventKind = "cert_warn"
	EventCertCrit    EventKind = "cert_crit"
	EventCertRenewed EventKind = "cert_renewed"
)

// Event is one up/down transition.
type Event struct {
	ID              string
	MonitorID       string
	Time            time.Time
	Kind            EventKind
	StatusCode      *int
	Failure         Failure
	DurationSeconds *int // for EventUp: how long the monitor was down
	// CertExpiresAt is the certificate's expiry, for the cert events.
	CertExpiresAt *time.Time
}

// certBand is where a certificate expiring at expires stands at now: 0
// outside the warning window, 1 inside it, 2 inside the critical one.
func certBand(m Monitor, expires *time.Time, now time.Time) int {
	if expires == nil {
		return 0
	}
	left := expires.Sub(now)
	switch {
	case left <= time.Duration(m.CertCritDays)*24*time.Hour:
		return 2
	case left <= time.Duration(m.CertWarnDays)*24*time.Hour:
		return 1
	}
	return 0
}

// CertEvent is the certificate transition, if any, between the previous
// check (c) and this one, which saw expires at now. A check that saw no
// certificate (plain http, or a failed handshake) changes nothing, and
// neither does a monitor whose certificate is not watched.
func CertEvent(m Monitor, c Counters, expires *time.Time, now time.Time) *Event {
	if !m.CertNotify || expires == nil {
		return nil
	}
	prevAt := now
	if c.LastCheckAt != nil {
		prevAt = *c.LastCheckAt
	}
	before, after := certBand(m, c.CertExpiresAt, prevAt), certBand(m, expires, now)
	if c.CertExpiresAt == nil {
		before = 0
	}
	at := expires.UTC()
	switch {
	case after == before:
		return nil
	case after == 2:
		return &Event{Kind: EventCertCrit, Time: now, CertExpiresAt: &at}
	case after == 1 && before == 0:
		return &Event{Kind: EventCertWarn, Time: now, CertExpiresAt: &at}
	case after == 0:
		return &Event{Kind: EventCertRenewed, Time: now, CertExpiresAt: &at}
	}
	return nil // critical back to warning: renewed, but not out of the window
}

// Apply advances c by one check result r at now.
//
// A failure makes the monitor pending, and down once more than m.Retries
// consecutive checks have failed (Retries 0: the first failure is down).
// A down monitor comes back up after m.RecoverySuccesses consecutive
// successes; a pending one on the first success. While probeOffline, a
// network-level failure (dns, connect, timeout) says nothing about the
// site: the counters do not move and the heartbeat is unknown.
func Apply(m Monitor, c Counters, r Result, probeOffline bool, now time.Time) (Counters, Status, *Event) {
	if !r.Up && probeOffline && r.Failure.network() {
		return c, StatusUnknown, nil
	}
	next := c
	var event *Event
	if r.Up {
		next.ConsecutiveFailures = 0
		next.ConsecutiveSuccesses++
		switch {
		case c.Status != StatusDown:
			next.Status = StatusUp
		case next.ConsecutiveSuccesses >= m.RecoverySuccesses:
			next.Status = StatusUp
			event = &Event{Kind: EventUp, Time: now, StatusCode: code(r)}
			if c.DownSince != nil {
				d := int(now.Sub(*c.DownSince).Seconds())
				event.DurationSeconds = &d
			}
			next.DownSince = nil
		}
	} else {
		next.ConsecutiveSuccesses = 0
		next.ConsecutiveFailures++
		switch {
		case c.Status == StatusDown:
		case next.ConsecutiveFailures > m.Retries:
			next.Status = StatusDown
			next.DownSince = &now
			event = &Event{Kind: EventDown, Time: now, StatusCode: code(r), Failure: r.Failure}
		default:
			next.Status = StatusPending
		}
	}
	if next.Status != c.Status {
		next.StatusSince = now
	}
	beat := next.Status
	if r.Up {
		beat = StatusUp
	}
	return next, beat, event
}

func code(r Result) *int {
	if r.StatusCode == 0 {
		return nil
	}
	c := r.StatusCode
	return &c
}

// NextInterval is how long after a check the next one is due: the retry
// interval while confirming a failure, else the normal interval.
func NextInterval(m Monitor, c Counters) time.Duration {
	if c.Status == StatusPending {
		return time.Duration(m.RetryIntervalSeconds) * time.Second
	}
	return time.Duration(m.IntervalSeconds) * time.Second
}
