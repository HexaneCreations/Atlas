// Package goldensignals measures the four Golden Signals — Latency,
// Traffic, Errors, Saturation — for one node, from data this package does
// not own.
//
// Atlas monitors infrastructure, not application requests: there is no
// tracing, no per-request instrumentation, nothing latency-shaped for the
// hosts and containers it observes. Rather than assume every Golden Signal
// exists because the concept calls for it, each is its own [Provider] and
// reports Signal.Available == false when the underlying telemetry does not
// exist — see [LatencyProvider], which always does, honestly. A future
// telemetry source that does carry latency data slots in as a replacement
// provider, not a redesign of this package.
package goldensignals

import (
	"context"
	"time"

	"github.com/hexane/atlas/internal/core/capacityplanning"
)

// SignalName identifies one of the four Golden Signals.
type SignalName string

const (
	SignalLatency    SignalName = "latency"
	SignalTraffic    SignalName = "traffic"
	SignalErrors     SignalName = "errors"
	SignalSaturation SignalName = "saturation"
)

// Signal is one provider's measurement.
type Signal struct {
	Name SignalName
	// Available is false when the telemetry this signal needs does not
	// exist for this node — including, always, [LatencyProvider].
	Available bool
	Value     float64
	Unit      string
	Detail    string
}

// Snapshot is the metric-derived input for Traffic and Errors. Gathered
// from the existing metric read path — see the adapter in internal/app.
type Snapshot struct {
	NodeID string

	NetworkRxBytesPerSec float64
	NetworkTxBytesPerSec float64
	HasNetworkTraffic    bool

	// NetworkErrorsPerSec sums receive and transmit errors and drops — the
	// only genuine error-rate telemetry Atlas collects for infrastructure.
	// Alert firings and notable events are a distinct, complementary signal
	// of "something went wrong," already served by the existing alert
	// history and incident timeline endpoints; they are not blended into
	// this number, since a rate and a count are not the same unit and
	// combining them would manufacture a figure neither source actually
	// produced.
	NetworkErrorsPerSec float64
	HasNetworkErrors    bool
}

// SnapshotSource gathers a node's metric-derived snapshot. Satisfied via an
// adapter over the existing metric repository; see internal/app.
type SnapshotSource interface {
	Snapshot(ctx context.Context, nodeID string) (Snapshot, error)
}

// CapacitySource supplies the Saturation input, reusing the existing
// capacity planning engine's per-resource utilization directly rather than
// recomputing it.
type CapacitySource interface {
	Assess(ctx context.Context, nodeID string) (capacityplanning.Summary, error)
}

// Inputs is everything a [Provider] may read. Gathered once per
// [Engine.Measure] call rather than once per provider.
type Inputs struct {
	Snapshot Snapshot
	Capacity capacityplanning.Summary
}

// Provider measures one Golden Signal from Inputs. Pure and synchronous.
type Provider interface {
	Name() SignalName
	Measure(in Inputs) Signal
}

// Result is one node's measurement across every registered signal.
type Result struct {
	NodeID     string
	Signals    []Signal
	ComputedAt time.Time
}

// Engine measures Golden Signals by combining a metric snapshot and a
// capacity assessment with a set of independent signal providers. It holds
// no state and does no background work.
type Engine struct {
	snapshots SnapshotSource
	capacity  CapacitySource
	providers []Provider
}

// Options configures an [Engine].
type Options struct {
	Snapshots SnapshotSource
	Capacity  CapacitySource
	Providers []Provider
}

// NewEngine builds an Engine.
func NewEngine(opts Options) *Engine {
	return &Engine{snapshots: opts.Snapshots, capacity: opts.Capacity, providers: opts.Providers}
}

// Measure computes nodeID's Golden Signals.
func (e *Engine) Measure(ctx context.Context, nodeID string) (Result, error) {
	snap, err := e.snapshots.Snapshot(ctx, nodeID)
	if err != nil {
		return Result{}, err
	}
	capacity, err := e.capacity.Assess(ctx, nodeID)
	if err != nil {
		return Result{}, err
	}
	in := Inputs{Snapshot: snap, Capacity: capacity}

	signals := make([]Signal, 0, len(e.providers))
	for _, p := range e.providers {
		sig := p.Measure(in)
		sig.Name = p.Name()
		signals = append(signals, sig)
	}
	return Result{NodeID: nodeID, Signals: signals, ComputedAt: time.Now()}, nil
}
