// Package collect defines what an observation is and what it means to
// collect one.
//
// This package is the narrow waist of Atlas. Everything that produces data —
// the Docker plugin, the systemd plugin, the /proc readers — produces the
// types defined here, and everything that consumes data consumes them. A new
// technology is added by writing a [Collector], not by touching the pipeline.
//
// It deliberately depends on nothing but the platform packages. It knows
// nothing about Postgres, HTTP, or transports, which is what allows a
// collector to be unit-tested with no infrastructure at all.
package collect

import (
	"context"
	"maps"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// Unit describes what a [Sample]'s value measures.
//
// Units travel with the sample rather than being inferred from the metric
// name, because the presentation layer must be able to format a value it has
// never seen before — the whole point of a plugin architecture is that new
// metric names appear without the UI being rebuilt.
type Unit string

const (
	// UnitCount is a dimensionless quantity: processes, restarts, containers.
	UnitCount Unit = "count"
	// UnitPercent is a proportion expressed from 0 to 100.
	UnitPercent Unit = "percent"
	// UnitBytes is a quantity of data.
	UnitBytes Unit = "bytes"
	// UnitBytesPerSecond is a throughput.
	UnitBytesPerSecond Unit = "bytes_per_second"
	// UnitSeconds is a duration, always in seconds. Collectors convert;
	// storage and display never have to guess a scale.
	UnitSeconds Unit = "seconds"
	// UnitOperationsPerSecond is a rate of discrete operations, such as disk
	// IOPS.
	UnitOperationsPerSecond Unit = "operations_per_second"
	// UnitRatio is a proportion expressed from 0 to 1, used for load averages
	// and saturation figures that have no natural percentage form.
	UnitRatio Unit = "ratio"
)

// Kind describes how a [Sample]'s value behaves over time, which determines
// how it may legitimately be aggregated.
//
// Averaging a counter is meaningless; summing a gauge across hosts is usually
// wrong. Recording the kind at collection time is what lets the rollup and
// query layers refuse those mistakes instead of silently producing a
// plausible wrong number.
type Kind string

const (
	// KindGauge is a value that rises and falls: memory in use, temperature.
	KindGauge Kind = "gauge"
	// KindCounter is a value that only increases, resetting to zero when its
	// source restarts: bytes received, total context switches.
	KindCounter Kind = "counter"
)

// Sample is one observation of one metric at one instant.
type Sample struct {
	// Metric is the dotted metric name, such as "system.cpu.usage".
	Metric string `json:"metric"`
	// Value is the observed number.
	Value float64 `json:"value"`
	// Unit gives the value meaning. Required.
	Unit Unit `json:"unit"`
	// Kind determines valid aggregation. Required.
	Kind Kind `json:"kind"`
	// Time is when the observation was made.
	Time time.Time `json:"time"`
	// Labels are the dimensions of the sample: which disk, which container.
	// Keep cardinality low — every distinct label combination is a distinct
	// series, and unbounded labels such as a PID or a request id will grow
	// the time-series store without bound.
	Labels map[string]string `json:"labels,omitempty"`
}

// Validate reports whether the sample is well formed.
//
// The scheduler validates every sample before it enters the pipeline, so a
// buggy plugin corrupts nothing downstream and its mistake is reported
// against the collector that made it.
func (s Sample) Validate() error {
	switch {
	case s.Metric == "":
		return errs.New(errs.CodeInvalidArgument, "sample has no metric name")
	case s.Unit == "":
		return errs.New(errs.CodeInvalidArgument, "sample %q has no unit", s.Metric)
	case s.Kind == "":
		return errs.New(errs.CodeInvalidArgument, "sample %q has no kind", s.Metric)
	case s.Time.IsZero():
		return errs.New(errs.CodeInvalidArgument, "sample %q has no timestamp", s.Metric)
	}
	// NaN and infinity survive a float64 round-trip but break aggregation and
	// most JSON encoders. Reject them at the boundary.
	if s.Value != s.Value { // NaN
		return errs.New(errs.CodeInvalidArgument, "sample %q has a NaN value", s.Metric)
	}
	if s.Value > 1e308 || s.Value < -1e308 {
		return errs.New(errs.CodeInvalidArgument, "sample %q has an infinite value", s.Metric)
	}
	return nil
}

// Batch is the complete result of one collection run.
//
// Collectors return a batch rather than a stream of samples so that the
// samples from one run share a run identity and can be written together. Bulk
// insertion is the difference between one round trip per run and one per
// metric, which at a fifteen-second interval across a fleet is the difference
// between a workable system and an unusable one.
type Batch struct {
	// CollectorID identifies the collector that produced this batch.
	CollectorID string `json:"collector_id"`
	// StartedAt and CompletedAt bound the run, so collection cost is itself
	// measurable.
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	// Samples are the observations made.
	Samples []Sample `json:"samples"`
}

// Duration returns how long the run took.
func (b Batch) Duration() time.Duration { return b.CompletedAt.Sub(b.StartedAt) }

// Descriptor is a collector's static identity and scheduling preference.
type Descriptor struct {
	// ID uniquely identifies the collector, in <domain>.<subject> form:
	// "system.cpu", "docker.containers". Stable across releases — it appears
	// in metrics, logs, and configuration.
	ID string
	// Name is a short human-readable label for the UI.
	Name string
	// Description explains what the collector observes.
	Description string
	// Interval is how often to run. Zero means use the configured default.
	// Set it only where the metric's nature demands it: disk capacity moves
	// slowly and does not need a fifteen-second poll.
	Interval time.Duration
	// Timeout bounds a single run. Zero means use the configured default.
	Timeout time.Duration
}

// Validate reports whether the descriptor is usable.
func (d Descriptor) Validate() error {
	if d.ID == "" {
		return errs.New(errs.CodeInvalidArgument, "collector descriptor has no ID")
	}
	if d.Interval < 0 {
		return errs.New(errs.CodeInvalidArgument, "collector %q has a negative interval", d.ID)
	}
	if d.Timeout < 0 {
		return errs.New(errs.CodeInvalidArgument, "collector %q has a negative timeout", d.ID)
	}
	if d.Interval > 0 && d.Timeout > d.Interval {
		return errs.New(errs.CodeInvalidArgument,
			"collector %q has timeout %s longer than its interval %s; runs would overlap indefinitely",
			d.ID, d.Timeout, d.Interval)
	}
	return nil
}

// Collector observes one aspect of a system and reports numeric samples.
//
// Implementations must be safe for concurrent use — the scheduler may run a
// collector while a previous run is still finishing — and must return
// promptly when their context is cancelled. A collector that ignores
// cancellation can pin a scheduler slot on a host with a wedged filesystem or
// an unresponsive Docker daemon, which is precisely the situation an operator
// most needs monitoring to keep working.
//
// Collect must not panic, but the scheduler isolates panics anyway; a bug in
// one collector never stops the others.
type Collector interface {
	// Descriptor returns the collector's identity. It must be constant.
	Descriptor() Descriptor
	// Collect performs one observation run.
	Collect(ctx context.Context) ([]Sample, error)
}

// CloneLabels returns a copy of a label map.
//
// Collectors that keep a label map between runs must clone it before
// attaching it to a sample: samples travel asynchronously through the
// pipeline, and a mutation on the next run would rewrite history that has
// already been handed off.
func CloneLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	return maps.Clone(labels)
}

// Streamer is a collector whose source pushes rather than being polled.
//
// Docker's event stream, a container log tail, and systemd's D-Bus signals all
// produce data when something happens rather than when asked. Modelling them
// as [Collector] would mean polling a stream, which is both wasteful and
// lossy — events between two polls are simply missed.
//
// A Streamer runs continuously and is supervised by the scheduler, which gives
// it the same guarantees a polled collector gets: panic isolation, restart with
// backoff, health reporting, cardinality enforcement, and bounded shutdown. The
// alternative — a plugin spawning its own goroutine in Init — has none of
// those, and is exactly the failure the scheduler exists to prevent.
//
// Implementations must:
//
//   - Return promptly when ctx is cancelled. This is the shutdown path.
//   - Never close out. The scheduler owns that channel.
//   - Treat a blocked send as back-pressure and honour ctx while sending,
//     rather than dropping silently.
//
// Returning nil means the stream ended cleanly — a daemon restarting, say. The
// scheduler restarts it with backoff either way, because an event source that
// has stopped producing is not a source that should stay stopped.
type Streamer interface {
	// Descriptor returns the streamer's identity. It must be constant, and its
	// ID shares a namespace with collectors: no streamer may take a
	// collector's ID.
	Descriptor() Descriptor
	// Stream runs until ctx is cancelled, sending samples to out.
	Stream(ctx context.Context, out chan<- Sample) error
}
