// Package metric persists and queries time-series observations.
//
// It is the storage side of the transport seam: envelopes arrive from any
// transport, in-process today and from remote agents at Tier 4, and this
// package is what makes them durable and queryable.
//
// The package owns SQL. Nothing above it writes a query, and nothing below it
// knows about HTTP — which is what lets the query layer be reshaped for the
// SLO work in Tier 3 without touching a collector or a handler.
package metric

import (
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Node is a machine Atlas observes.
type Node struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname"`

	OS           string `json:"os,omitempty"`
	Platform     string `json:"platform,omitempty"`
	Kernel       string `json:"kernel,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	CPUCores     int    `json:"cpu_cores,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	// Environment is the operator-assigned tag, empty when untagged.
	Environment string `json:"environment,omitempty"`

	// BootTime lets uptime be computed at query time. Storing uptime as a
	// sample would make it stale the moment it was written.
	BootTime    time.Time `json:"boot_time,omitzero"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`

	// ClockSkewSeconds is agent send time minus control-plane receive time
	// from the most recent push. Nil until an agent has pushed at least once.
	ClockSkewSeconds *float64 `json:"clock_skew_seconds,omitempty"`
}

// UptimeSeconds returns how long the node has been up, or zero if boot time is
// unknown.
func (n Node) UptimeSeconds() float64 {
	if n.BootTime.IsZero() {
		return 0
	}
	return time.Since(n.BootTime).Seconds()
}

// NodeStatus is a node's liveness, derived from how recently it was observed
// rather than stored.
//
// Deriving it means a node that stops reporting becomes stale without anything
// having to notice and write a status — which is important, because the most
// likely reason a node stops reporting is that the thing which would have
// written the status is the thing that died.
type NodeStatus string

const (
	// StatusUp means the node reported within the expected window.
	StatusUp NodeStatus = "up"
	// StatusStale means the node has missed several collection intervals. It
	// may be under load, partitioned, or gone.
	StatusStale NodeStatus = "stale"
	// StatusDown means nothing has been heard for long enough that the node
	// should be treated as unavailable.
	StatusDown NodeStatus = "down"
)

// Status classifies a node's liveness against the collection interval.
//
// The thresholds are multiples of the interval rather than fixed durations, so
// a deployment that collects every minute is not permanently reported as stale.
func (n Node) Status(interval time.Duration, now time.Time) NodeStatus {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	silence := now.Sub(n.LastSeenAt)

	switch {
	case silence > 10*interval:
		return StatusDown
	case silence > 3*interval:
		return StatusStale
	default:
		return StatusUp
	}
}

// Point is one value in a time series returned by a query.
type Point struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
	// Min and Max are populated when the query read a rollup rather than raw
	// samples. An average alone hides the spike an operator is looking for.
	Min *float64 `json:"min,omitempty"`
	Max *float64 `json:"max,omitempty"`
}

// Series is one metric on one node, with one label combination.
type Series struct {
	NodeID      string            `json:"node_id"`
	CollectorID string            `json:"collector_id"`
	Metric      string            `json:"metric"`
	Unit        collect.Unit      `json:"unit"`
	Kind        collect.Kind      `json:"kind"`
	Labels      map[string]string `json:"labels,omitempty"`
	Points      []Point           `json:"points"`
}

// Resolution names the storage a query read from.
//
// It is returned to the caller because a chart drawn from hourly rollups is
// telling a different story from one drawn at raw resolution, and a client
// that cannot tell the difference will eventually present one as the other.
type Resolution string

const (
	// ResolutionRaw reads individual samples.
	ResolutionRaw Resolution = "raw"
	// ResolutionMinute reads the one-minute continuous aggregate.
	ResolutionMinute Resolution = "1m"
	// ResolutionHour reads the one-hour continuous aggregate.
	ResolutionHour Resolution = "1h"
)

// Interval returns the bucket width of a resolution. Raw has none.
func (r Resolution) Interval() time.Duration {
	switch r {
	case ResolutionMinute:
		return time.Minute
	case ResolutionHour:
		return time.Hour
	default:
		return 0
	}
}

// Query selects a set of series over a time range.
type Query struct {
	// NodeID restricts to one node. Required: an unfiltered query across a
	// fleet is almost never what the caller meant, and is expensive enough
	// that it should be asked for deliberately.
	NodeID string
	// Metrics restricts to these metric names. Empty means every metric,
	// which is only sensible over a short range.
	Metrics []string
	// From and To bound the range. To defaults to now.
	From time.Time
	To   time.Time
	// Resolution selects raw samples or a rollup. Empty auto-selects based on
	// the range width.
	Resolution Resolution
	// MaxPoints caps points per series, so a wide range cannot return a
	// response too large to render or transfer.
	MaxPoints int
}

// DefaultMaxPoints bounds a series when the caller does not choose. Roughly
// the horizontal pixel budget of a wide chart; more points than pixels is
// data the viewer cannot see and the browser must still parse.
const DefaultMaxPoints = 1500

// MaxAllowedPoints is the ceiling a caller may request.
const MaxAllowedPoints = 10000

// Normalize fills defaults and validates the query.
func (q *Query) Normalize(now time.Time) error {
	const op = "metric.Query.Normalize"

	if q.NodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "node is required").
			WithOp(op).WithDetail("field", "node")
	}
	if q.To.IsZero() {
		q.To = now
	}
	if q.From.IsZero() {
		q.From = q.To.Add(-time.Hour)
	}
	if !q.From.Before(q.To) {
		return errs.New(errs.CodeInvalidArgument, "from must be earlier than to").
			WithOp(op).WithDetail("field", "from")
	}
	if q.MaxPoints <= 0 {
		q.MaxPoints = DefaultMaxPoints
	}
	if q.MaxPoints > MaxAllowedPoints {
		return errs.New(errs.CodeInvalidArgument,
			"max_points must not exceed %d", MaxAllowedPoints).
			WithOp(op).WithDetail("field", "max_points")
	}
	if q.Resolution == "" {
		q.Resolution = autoResolution(q.To.Sub(q.From))
	}
	switch q.Resolution {
	case ResolutionRaw, ResolutionMinute, ResolutionHour:
	default:
		return errs.New(errs.CodeInvalidArgument,
			"unknown resolution %q (want raw, 1m, or 1h)", q.Resolution).
			WithOp(op).WithDetail("field", "resolution")
	}
	return nil
}

// autoResolution picks storage appropriate to the range width.
//
// The thresholds come from the point budget: at a fifteen-second cadence, six
// hours of raw samples is already about 1440 points, which is the most a chart
// can usefully show. Beyond that, reading a rollup returns the same picture
// for a fraction of the work.
func autoResolution(span time.Duration) Resolution {
	switch {
	case span <= 6*time.Hour:
		return ResolutionRaw
	case span <= 14*24*time.Hour:
		return ResolutionMinute
	default:
		return ResolutionHour
	}
}

// Result is the outcome of a query.
type Result struct {
	Series []Series `json:"series"`
	// Resolution reports which storage answered, including when it was
	// auto-selected.
	Resolution Resolution `json:"resolution"`
	From       time.Time  `json:"from"`
	To         time.Time  `json:"to"`
	// Truncated reports that MaxPoints capped at least one series, so the
	// caller knows the chart is not showing everything in the range.
	Truncated bool `json:"truncated"`
}
