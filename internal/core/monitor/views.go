package monitor

import "time"

// Live is a monitor's current probe state as the API shows it.
type Live struct {
	Status              Status
	ConsecutiveFailures int
	StatusSince         time.Time
	DownSince           *time.Time
	LastCheckAt         *time.Time
	LastStatusCode      *int
	LastResponseMS      *int
	LastFailure         Failure
	NextCheckAt         time.Time
	CertExpiresAt       *time.Time
}

// View is a monitor with its live state.
type View struct {
	Monitor
	Live Live
}

// Heartbeat is one stored check.
type Heartbeat struct {
	Time       time.Time
	Status     Status
	StatusCode *int
	ResponseMS *int
	Failure    Failure
}

// HourlyBucket is one hour of heartbeats, rolled up.
type HourlyBucket struct {
	Bucket        time.Time
	Up            int
	Pending       int
	Down          int
	Unknown       int
	AvgResponseMS *float64
	MaxResponseMS *int
}

// UptimeCount is the raw material of an uptime percentage: checks that
// counted as up (up or pending) out of checks that counted at all (every
// one except unknown, when the probe itself was offline).
type UptimeCount struct {
	Up      int
	Counted int
}

// Percent is the uptime, or nil when nothing counted.
func (u UptimeCount) Percent() *float64 {
	if u.Counted == 0 {
		return nil
	}
	p := 100 * float64(u.Up) / float64(u.Counted)
	return &p
}

// Uptime is a monitor's uptime over the standard windows.
type Uptime struct {
	Day   UptimeCount
	Week  UptimeCount
	Month UptimeCount
}

// UpdateFunc computes a monitor's new configuration and secret from the
// stored ones, inside the store's update transaction.
type UpdateFunc = func(stored Monitor, secret Secret) (Monitor, Secret, error)
