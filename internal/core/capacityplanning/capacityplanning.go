// Package capacityplanning assesses a node's current resource capacity —
// utilization, remaining headroom, and saturation status — from resource
// data this package does not own. It is deliberately not forecasting: no
// domain here projects "when," only "where things stand now." A forecasting
// algorithm is future work that plugs in as another [Provider], not a
// redesign of this package.
package capacityplanning

import (
	"context"
	"time"
)

// Domain names, as returned by each built-in [Provider.Name] — exported so
// another package (goldensignals' Saturation provider) can pick specific
// domains out of a [Summary] without string-matching by convention.
const (
	DomainCPU              = "cpu"
	DomainMemory           = "memory"
	DomainDisk             = "disk"
	DomainNetwork          = "network"
	DomainContainerDensity = "container_density"
	DomainProcessDensity   = "process_density"
)

// Status is a capacity domain's, or a node's, saturation level.
type Status string

const (
	StatusHealthy  Status = "healthy"
	StatusWarning  Status = "warning"
	StatusCritical Status = "critical"
)

func (s Status) rank() int {
	switch s {
	case StatusCritical:
		return 2
	case StatusWarning:
		return 1
	default:
		return 0
	}
}

// worseStatus returns whichever of a, b ranks worse.
func worseStatus(a, b Status) Status {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// DiskMount is one filesystem's capacity, as observed.
type DiskMount struct {
	Mountpoint         string
	TotalBytes         float64
	UsedBytes          float64
	UtilizationPercent float64
}

// Snapshot is one node's current resource picture — the engine's only
// input, gathered from the existing metric read path. A Has* flag distinct
// from a zero value: a node genuinely idle at 0% CPU and a node with no CPU
// sample at all must not be assessed the same way.
type Snapshot struct {
	NodeID string

	CPUCores              int
	CPUUtilizationPercent float64
	HasCPU                bool

	MemoryTotalBytes         float64
	MemoryUsedBytes          float64
	MemoryUtilizationPercent float64
	HasMemory                bool

	// Disks is empty when no disk sample was found — not itself a Has* flag,
	// since an empty slice already means "nothing to assess."
	Disks []DiskMount

	NetworkRxBytesPerSec float64
	NetworkTxBytesPerSec float64
	HasNetwork           bool

	RunningContainers int
	HasContainers     bool

	RunningProcesses int
	HasProcesses     bool
}

// SnapshotSource gathers a node's current resource snapshot. Satisfied via
// an adapter over the existing metric repository; see internal/app.
type SnapshotSource interface {
	Snapshot(ctx context.Context, nodeID string) (Snapshot, error)
}

// Domain is one capacity provider's assessment.
type Domain struct {
	Name string
	// Available is false when there is nothing to assess — no sample for
	// this domain, or no configured capacity ceiling to measure against.
	// An unavailable domain does not count toward a node's overall status.
	Available          bool
	UtilizationPercent float64
	RemainingCapacity  float64
	RemainingUnit      string
	Status             Status
	Detail             string
}

// Provider assesses one capacity domain from a snapshot. Pure and
// synchronous: every input it needs is already on Snapshot, gathered once
// per [Engine.Assess] call rather than once per provider.
type Provider interface {
	Name() string
	Assess(s Snapshot) Domain
}

// Summary is one node's capacity assessment across every registered
// provider.
type Summary struct {
	NodeID     string
	Status     Status
	Domains    []Domain
	ComputedAt time.Time
}

// Engine assesses capacity by combining a resource snapshot with a set of
// independent domain providers. It holds no state and does no background
// work: every call reads current usage fresh.
type Engine struct {
	snapshots SnapshotSource
	providers []Provider
}

// Options configures an [Engine].
type Options struct {
	Snapshots SnapshotSource
	Providers []Provider
}

// NewEngine builds an Engine over a snapshot source and a set of providers.
func NewEngine(opts Options) *Engine {
	return &Engine{snapshots: opts.Snapshots, providers: opts.Providers}
}

// Assess computes nodeID's capacity summary from its current snapshot.
func (e *Engine) Assess(ctx context.Context, nodeID string) (Summary, error) {
	snap, err := e.snapshots.Snapshot(ctx, nodeID)
	if err != nil {
		return Summary{}, err
	}

	domains := make([]Domain, 0, len(e.providers))
	status := StatusHealthy
	for _, p := range e.providers {
		d := p.Assess(snap)
		d.Name = p.Name()
		domains = append(domains, d)
		if d.Available {
			status = worseStatus(status, d.Status)
		}
	}

	return Summary{NodeID: nodeID, Status: status, Domains: domains, ComputedAt: time.Now()}, nil
}
