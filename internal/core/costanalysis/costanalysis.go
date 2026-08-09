// Package costanalysis estimates a node's infrastructure cost, per hour,
// from resource usage this package does not own — CPU, memory, disk,
// network, container and process counts, uptime.
//
// The engine only combines: [UsageSource] gathers what a node is using,
// [PricingModel] says what that usage costs, and the engine sums the
// per-category costs into a breakdown and a total. Swapping the pricing
// model — a flat reference rate, a real cloud provider's rates, an on-prem
// or custom rate card — changes what a resource costs, never how the engine
// combines categories.
package costanalysis

import (
	"context"
	"time"
)

// Usage is one node's resource consumption, the engine's only input.
// Gathered from existing metric read paths — see the adapter in
// internal/app.
type Usage struct {
	NodeID string

	CPUCores              int
	CPUUtilizationPercent float64 // 0-100

	MemoryTotalBytes float64
	MemoryUsedBytes  float64

	DiskTotalBytes float64
	DiskUsedBytes  float64

	// NetworkRxBytesPerSec and NetworkTxBytesPerSec are the most recently
	// observed throughput, not a cumulative counter.
	NetworkRxBytesPerSec float64
	NetworkTxBytesPerSec float64

	RunningContainers int
	RunningProcesses  int

	UptimeSeconds float64
}

// UsageSource gathers a node's resource usage. Satisfied via an adapter over
// the existing metric repository; see internal/app.
type UsageSource interface {
	Usage(ctx context.Context, nodeID string) (Usage, error)
}

// CPUPricing prices CPU usage.
type CPUPricing interface{ Cost(u Usage) float64 }

// MemoryPricing prices memory usage.
type MemoryPricing interface{ Cost(u Usage) float64 }

// DiskPricing prices disk usage.
type DiskPricing interface{ Cost(u Usage) float64 }

// NetworkPricing prices network throughput.
type NetworkPricing interface{ Cost(u Usage) float64 }

// ContainerPricing prices running containers.
type ContainerPricing interface{ Cost(u Usage) float64 }

// PricingModel turns resource usage into a cost per category, each priced
// independently — a model that does not meter a category returns a
// zero-cost pricer for it rather than the engine special-casing absence.
//
// Implementations: [ReferencePricing] today; AWS, Azure, GCP, on-prem, or a
// custom enterprise rate card later, each a new PricingModel with no change
// to [Engine].
type PricingModel interface {
	Name() string
	CPU() CPUPricing
	Memory() MemoryPricing
	Disk() DiskPricing
	Network() NetworkPricing
	Container() ContainerPricing
}

// Breakdown is estimated cost per hour, by resource category.
type Breakdown struct {
	CPU       float64 `json:"cpu"`
	Memory    float64 `json:"memory"`
	Disk      float64 `json:"disk"`
	Network   float64 `json:"network"`
	Container float64 `json:"container"`
}

// Total sums every category into the hourly total.
func (b Breakdown) Total() float64 {
	return b.CPU + b.Memory + b.Disk + b.Network + b.Container
}

// Result is one node's estimated cost.
type Result struct {
	NodeID       string
	PricingModel string
	Usage        Usage
	Breakdown    Breakdown
	// HourlyTotal is Breakdown.Total(), named for what a caller reads it as.
	HourlyTotal float64
	// EstimatedSinceBoot extrapolates HourlyTotal across the node's uptime.
	// It is informational — an estimate of accrued cost, not a billing
	// reconciliation against what a provider would actually invoice.
	EstimatedSinceBoot float64
	ComputedAt         time.Time
}

// Engine estimates cost by combining resource usage with a pricing model. It
// holds no state and does no background work: every call reads current
// usage fresh.
type Engine struct {
	usage   UsageSource
	pricing PricingModel
}

// Options configures an [Engine].
type Options struct {
	Usage   UsageSource
	Pricing PricingModel
}

// NewEngine builds an Engine over a usage source and a pricing model.
func NewEngine(opts Options) *Engine {
	return &Engine{usage: opts.Usage, pricing: opts.Pricing}
}

// Estimate computes nodeID's cost from its current usage.
func (e *Engine) Estimate(ctx context.Context, nodeID string) (Result, error) {
	u, err := e.usage.Usage(ctx, nodeID)
	if err != nil {
		return Result{}, err
	}
	return e.estimate(u), nil
}

func (e *Engine) estimate(u Usage) Result {
	b := Breakdown{
		CPU:       e.pricing.CPU().Cost(u),
		Memory:    e.pricing.Memory().Cost(u),
		Disk:      e.pricing.Disk().Cost(u),
		Network:   e.pricing.Network().Cost(u),
		Container: e.pricing.Container().Cost(u),
	}
	hourly := b.Total()
	uptimeHours := u.UptimeSeconds / 3600

	return Result{
		NodeID: u.NodeID, PricingModel: e.pricing.Name(), Usage: u, Breakdown: b,
		HourlyTotal: hourly, EstimatedSinceBoot: hourly * uptimeHours, ComputedAt: time.Now(),
	}
}
