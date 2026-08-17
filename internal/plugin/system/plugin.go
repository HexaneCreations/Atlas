package system

import (
	"context"
	"log/slog"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Plugin observes the host operating system.
//
// Unlike most plugins, this one is always present: every machine Atlas runs on
// has a CPU, memory, and filesystems. Detection therefore verifies that the
// facts are actually *readable* rather than that the subject exists — a
// container with a restricted `/proc`, or a sandbox that blocks `sysctl`, is a
// real deployment where this plugin cannot work and must say so.
type Plugin struct {
	provider Provider
	recorder FactsRecorder
	nodeID   string

	env        plugin.Env
	collectors []collect.Collector
	// cores is captured during detection so the load collector can normalise
	// without every run re-reading a number that cannot change.
	cores int
}

// Options configures the system plugin.
type Options struct {
	// Provider supplies host facts. Defaults to the gopsutil implementation.
	Provider Provider
	// Recorder persists descriptive node facts. Optional; without it the
	// plugin still reports metrics but the node's OS and kernel stay unknown.
	Recorder FactsRecorder
	// NodeID identifies the machine these observations describe.
	NodeID string
}

// New builds the system plugin.
func New(opts Options) *Plugin {
	if opts.Provider == nil {
		opts.Provider = NewProvider()
	}
	return &Plugin{
		provider: opts.Provider,
		recorder: opts.Recorder,
		nodeID:   opts.NodeID,
	}
}

// Descriptor implements [plugin.Plugin].
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:          "system",
		Name:        "System",
		Description: "CPU, memory, swap, disk, network, load, and host facts.",
		Subject:     "operating system",
	}
}

// Detect reports whether host facts can be read here.
//
// It reads the host info rather than checking for a file, because the failure
// this needs to catch is a restricted environment where the paths exist but
// the reads are refused. A path check would pass and every collection would
// then fail — reporting a broken integration where the honest answer is that
// this environment cannot be observed.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	info, err := p.provider.Host(ctx)
	if err != nil {
		return false, err
	}
	if info.Hostname == "" && info.OS == "" {
		// Reads succeeded but returned nothing usable, which happens in some
		// restricted sandboxes.
		return false, nil
	}
	p.cores = info.LogicalCores
	return true, nil
}

// Init implements [plugin.Plugin].
func (p *Plugin) Init(ctx context.Context, env plugin.Env) error {
	const op = "system.Plugin.Init"

	if p.nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "the system plugin requires a node id").WithOp(op)
	}
	p.env = env

	logger := env.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	p.collectors = []collect.Collector{
		newHostCollector(p.provider, p.recorder, env.Bus, p.nodeID, logger),
		newCPUCollector(p.provider),
		newMemoryCollector(p.provider),
		newLoadCollector(p.provider, p.cores),
		newDiskCollector(p.provider, logger),
		newDiskIOCollector(p.provider),
		newNetworkCollector(p.provider),
	}

	// The rate-based collectors have no previous reading yet, so their first
	// scheduled run establishes a baseline and emits nothing for those series.
	// Priming here means the first *reported* run already has rates, which is
	// the difference between a dashboard that is useful immediately and one
	// that is blank for an interval.
	p.prime(ctx)

	logger.InfoContext(ctx, "system plugin ready",
		slog.Int("collectors", len(p.collectors)),
		slog.Int("logical_cores", p.cores))
	return nil
}

// prime takes a first reading from the counter-based collectors so their next
// run can compute a rate. Failures are ignored: this is an optimisation, and
// the scheduler will report any genuine problem on the first real run.
func (p *Plugin) prime(ctx context.Context) {
	for _, c := range p.collectors {
		switch c.(type) {
		case *cpuCollector, *diskIOCollector, *networkCollector, *memoryCollector:
			_, _ = c.Collect(ctx)
		}
	}
}

// Collectors implements [plugin.Plugin].
func (p *Plugin) Collectors() []collect.Collector { return p.collectors }

// MountInfo is one mounted filesystem, combining what it is with its current
// capacity.
type MountInfo struct {
	Device     string
	Mountpoint string
	Fstype     string
	Opts       []string

	Total             uint64
	Used              uint64
	Free              uint64
	UsedPercent       float64
	InodesTotal       uint64
	InodesUsedPercent float64
}

// Mounts returns the live filesystem list, matching what [diskCollector]
// charts — the same filtering, read fresh rather than from stored samples.
//
// This is inventory rather than a time series for the same reason the
// container and process lists are: "what is mounted right now, and how full
// is it" is a question about current state, and a mount that appeared or a
// filesystem that just crossed 100% should not wait for the next scheduled
// collection to be visible.
func (p *Plugin) Mounts(ctx context.Context) ([]MountInfo, error) {
	partitions, err := p.provider.Partitions(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]MountInfo, 0, len(partitions))
	for _, part := range partitions {
		if ctx.Err() != nil {
			break
		}

		usage, err := p.provider.DiskUsage(ctx, part.Mountpoint)
		if err != nil {
			// One unreadable mount — removed between listing and reading,
			// permission-restricted — must not hide every other filesystem.
			continue
		}

		out = append(out, MountInfo{
			Device: part.Device, Mountpoint: part.Mountpoint,
			Fstype: part.Fstype, Opts: part.Opts,
			Total: usage.Total, Used: usage.Used, Free: usage.Free,
			UsedPercent:       clampPercent(usage.UsedPercent),
			InodesTotal:       usage.InodesTotal,
			InodesUsedPercent: clampPercent(usage.InodesUsedPercent),
		})
	}
	return out, nil
}

// Host returns the machine's descriptive facts.
func (p *Plugin) Host(ctx context.Context) (HostInfo, error) {
	return p.provider.Host(ctx)
}

// NetworkIdentity returns the host's addressing: interfaces with their
// addresses and state, default routes, and resolver configuration.
//
// Inventory rather than a time series: an address or a default route is
// current state, and it is what an alert has to be correlated against to
// mean anything ("which machine is 10.0.4.12").
func (p *Plugin) NetworkIdentity(ctx context.Context) (NetworkIdentity, error) {
	return p.provider.NetworkIdentity(ctx)
}

// Close implements [plugin.Plugin]. The plugin holds no sockets or handles;
// gopsutil reads are stateless per call.
func (p *Plugin) Close(context.Context) error { return nil }

var _ plugin.Plugin = (*Plugin)(nil)
