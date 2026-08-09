package process

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Settings is the plugin's configuration section (`plugins.process`).
type Settings struct {
	// TopN is how many of the heaviest processes get their own series.
	//
	// This is the cardinality dial. Each distinct process *name* that ever
	// appears in the top N becomes a series, so a large N on a host with
	// varied workloads grows the series count. Ten covers the processes an
	// operator actually looks at.
	TopN int `yaml:"top_n"`
	// InventoryLimit caps how many processes the live API returns.
	InventoryLimit int `yaml:"inventory_limit"`
}

func (s Settings) topN() int {
	if s.TopN > 0 {
		return s.TopN
	}
	return 10
}

func (s Settings) inventoryLimit() int {
	if s.InventoryLimit > 0 {
		return s.InventoryLimit
	}
	return 500
}

// Plugin observes processes and binaries.
type Plugin struct {
	provider Provider

	mu         sync.RWMutex
	settings   Settings
	collectors []collect.Collector
}

// Options configures the plugin.
type Options struct {
	// Provider supplies process facts. Defaults to the gopsutil implementation.
	Provider Provider
}

// New builds the process plugin.
func New(opts Options) *Plugin {
	if opts.Provider == nil {
		opts.Provider = NewProvider()
	}
	return &Plugin{provider: opts.Provider}
}

// Descriptor implements [plugin.Plugin].
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:          "process",
		Name:        "Processes",
		Description: "Process counts by state, the heaviest consumers, and a live inventory.",
		Subject:     "processes",
	}
}

// Detect reports whether processes can be enumerated.
//
// Every host has processes, so this checks readability rather than presence: a
// hardened container or a restricted sandbox may refuse the enumeration
// entirely, and that is a real deployment where this plugin cannot work.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	procs, err := p.provider.Processes(ctx)
	if err != nil {
		return false, err
	}
	// A host with no visible processes at all means the enumeration is being
	// refused rather than that nothing is running.
	return len(procs) > 0, nil
}

// Init implements [plugin.Plugin].
func (p *Plugin) Init(_ context.Context, env plugin.Env) error {
	const op = "process.Plugin.Init"

	var settings Settings
	if err := env.Config(&settings); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "invalid process plugin configuration").WithOp(op)
	}

	logger := env.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	p.mu.Lock()
	p.settings = settings
	p.collectors = []collect.Collector{newProcessCollector(p.provider, settings.topN(), logger)}
	p.mu.Unlock()
	return nil
}

// Collectors implements [plugin.Plugin].
func (p *Plugin) Collectors() []collect.Collector {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.collectors
}

// Close implements [plugin.Plugin]. Nothing is held open.
func (p *Plugin) Close(context.Context) error { return nil }

// Inventory returns the live process list, sorted by CPU then memory.
//
// Served on demand rather than stored, for the same reason the container list
// is: "what is running right now" is an inventory question, and a stored
// answer would be up to an interval stale while costing a series per process.
func (p *Plugin) Inventory(ctx context.Context) ([]Process, error) {
	procs, err := p.provider.Processes(ctx)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(procs, func(a, b Process) int {
		if a.CPUPercent != b.CPUPercent {
			if a.CPUPercent > b.CPUPercent {
				return -1
			}
			return 1
		}
		if a.MemoryRSS > b.MemoryRSS {
			return -1
		}
		if a.MemoryRSS < b.MemoryRSS {
			return 1
		}
		return 0
	})

	p.mu.RLock()
	limit := p.settings.inventoryLimit()
	p.mu.RUnlock()

	if len(procs) > limit {
		procs = procs[:limit]
	}
	return procs, nil
}

var _ plugin.Plugin = (*Plugin)(nil)

// processCollector reports aggregates and the heaviest consumers.
type processCollector struct {
	provider Provider
	topN     int
	logger   *slog.Logger
}

func newProcessCollector(p Provider, topN int, logger *slog.Logger) *processCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &processCollector{provider: p, topN: topN, logger: logger}
}

func (c *processCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "process.summary",
		Name:        "Processes",
		Description: "Process counts by state, thread totals, and the heaviest consumers.",
		// Enumerating processes is thousands of small reads. Every fifteen
		// seconds would make Atlas a measurable load source on a busy host,
		// which is the one thing it must never be.
		Interval: 30 * time.Second,
		Timeout:  25 * time.Second,
	}
}

func (c *processCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	procs, err := c.provider.Processes(ctx)
	if err != nil {
		return nil, err
	}

	byState := map[State]int{}
	var totalThreads int64
	for _, p := range procs {
		byState[p.State]++
		totalThreads += int64(p.Threads)
	}

	samples := make([]collect.Sample, 0, len(byState)+c.topN*2+4)

	// Every state is emitted, including zeros, so a chart shows a count
	// falling to zero rather than the series simply disappearing.
	for _, state := range []State{
		StateRunning, StateSleeping, StateStopped, StateZombie, StateIdle, StateOther,
	} {
		samples = append(samples, labelled("process.count", float64(byState[state]),
			collect.UnitCount, now, map[string]string{"state": string(state)}))
	}

	samples = append(samples,
		gauge("process.total", float64(len(procs)), collect.UnitCount, now),
		gauge("process.threads", float64(totalThreads), collect.UnitCount, now),
		// Zombies get their own metric as well as a state count. A rising
		// zombie count means a parent is not reaping children, which ends in
		// PID exhaustion, and it deserves an alert of its own rather than
		// being one label value among six.
		gauge("process.zombies", float64(byState[StateZombie]), collect.UnitCount, now),
	)

	samples = append(samples, c.topConsumers(procs, now)...)
	return samples, nil
}

// topConsumers emits a series for the heaviest processes, labelled by name.
//
// Aggregated by name rather than PID, and summed across instances: a host
// running forty worker processes of one program should show one line for the
// program, not forty that individually look idle. That is also what keeps the
// label bounded.
func (c *processCollector) topConsumers(procs []Process, now time.Time) []collect.Sample {
	type usage struct {
		cpu       float64
		memory    uint64
		instances int
	}

	byName := make(map[string]*usage, len(procs))
	for _, p := range procs {
		if p.Name == "" {
			continue
		}
		u, ok := byName[p.Name]
		if !ok {
			u = &usage{}
			byName[p.Name] = u
		}
		u.cpu += p.CPUPercent
		u.memory += p.MemoryRSS
		u.instances++
	}

	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}

	samples := make([]collect.Sample, 0, c.topN*3)

	// Top by CPU.
	slices.SortFunc(names, func(a, b string) int {
		switch {
		case byName[a].cpu > byName[b].cpu:
			return -1
		case byName[a].cpu < byName[b].cpu:
			return 1
		default:
			return 0
		}
	})
	for _, name := range names[:min(c.topN, len(names))] {
		u := byName[name]
		if u.cpu <= 0 {
			break // nothing below this is worth a series
		}
		labels := map[string]string{"process": name}
		samples = append(samples,
			labelled("process.top.cpu", u.cpu, collect.UnitPercent, now, labels),
			labelled("process.instances", float64(u.instances), collect.UnitCount, now, labels),
		)
	}

	// Top by memory, which is frequently a different set of processes.
	slices.SortFunc(names, func(a, b string) int {
		switch {
		case byName[a].memory > byName[b].memory:
			return -1
		case byName[a].memory < byName[b].memory:
			return 1
		default:
			return 0
		}
	})
	for _, name := range names[:min(c.topN, len(names))] {
		u := byName[name]
		if u.memory == 0 {
			break
		}
		samples = append(samples, labelled("process.top.memory", float64(u.memory),
			collect.UnitBytes, now, map[string]string{"process": name}))
	}

	return samples
}

func gauge(metric string, value float64, unit collect.Unit, at time.Time) collect.Sample {
	return collect.Sample{Metric: metric, Value: value, Unit: unit, Kind: collect.KindGauge, Time: at}
}

func labelled(metric string, value float64, unit collect.Unit, at time.Time, labels map[string]string) collect.Sample {
	return collect.Sample{
		Metric: metric, Value: value, Unit: unit, Kind: collect.KindGauge, Time: at,
		Labels: collect.CloneLabels(labels),
	}
}
