package cron

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/redact"
)

// Plugin observes scheduled jobs.
type Plugin struct {
	provider Provider
	redactor redact.Redactor

	mu         sync.RWMutex
	collectors []collect.Collector
}

// Options configures the plugin.
type Options struct {
	// Provider supplies jobs. Defaults to reading the standard locations.
	Provider Provider
	// DisableSecretRedaction transmits cron commands exactly as written,
	// credentials included. Off by default — see process.Options for the
	// same trade-off.
	DisableSecretRedaction bool
}

// New builds the cron plugin.
func New(opts Options) *Plugin {
	if opts.Provider == nil {
		opts.Provider = NewProvider()
	}
	return &Plugin{provider: opts.Provider, redactor: redact.New(!opts.DisableSecretRedaction)}
}

// Descriptor implements [plugin.Plugin].
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:          "cron",
		Name:        "Scheduled jobs",
		Description: "System, user, and packaged cron jobs with their schedules.",
		Subject:     "cron",
	}
}

// Detect reports whether any cron source is readable.
//
// Returning false where nothing is readable matters: on a host where Atlas
// runs unprivileged and every crontab is root-only, reporting the plugin as
// active would show "0 jobs" — which reads as "nothing is scheduled" rather
// than the truth, "I cannot see what is scheduled".
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	return p.provider.Available(ctx), nil
}

// Init implements [plugin.Plugin].
func (p *Plugin) Init(_ context.Context, env plugin.Env) error {
	logger := env.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	p.mu.Lock()
	p.collectors = []collect.Collector{newCronCollector(p.provider, logger)}
	p.mu.Unlock()
	return nil
}

// Collectors implements [plugin.Plugin].
func (p *Plugin) Collectors() []collect.Collector {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.collectors
}

// Close implements [plugin.Plugin].
func (p *Plugin) Close(context.Context) error { return nil }

// Inventory returns the live job list.
func (p *Plugin) Inventory(ctx context.Context) ([]Job, error) {
	jobs, err := p.provider.Jobs(ctx)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeOf(err), "could not read scheduled jobs").
			WithOp("cron.Plugin.Inventory")
	}
	for i := range jobs {
		jobs[i].Command = p.redactor.String(jobs[i].Command)
	}
	return jobs, nil
}

var _ plugin.Plugin = (*Plugin)(nil)

// cronCollector counts scheduled jobs.
//
// Only counts. A per-job series would be labelled by command — unbounded, and
// worse, commands routinely contain credentials passed as arguments, which
// would then live in metric labels forever. The job list is served live from
// the API instead, where it is inventory.
type cronCollector struct {
	provider Provider
	logger   *slog.Logger
}

func newCronCollector(p Provider, logger *slog.Logger) *cronCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &cronCollector{provider: p, logger: logger}
}

func (c *cronCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "cron.jobs",
		Name:        "Scheduled jobs",
		Description: "Counts of cron jobs by source and privilege.",
		// Crontabs change when someone edits them, which is rare.
		Interval: 5 * time.Minute,
		Timeout:  30 * time.Second,
	}
}

func (c *cronCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	jobs, err := c.provider.Jobs(ctx)
	if err != nil {
		return nil, err
	}

	bySource := map[Source]int{}
	root := 0
	for _, j := range jobs {
		bySource[j.Source]++
		if j.Root() {
			root++
		}
	}

	samples := make([]collect.Sample, 0, 8)
	for _, source := range []Source{SourceSystem, SourceCronDir, SourceUser, SourcePeriodic} {
		samples = append(samples, collect.Sample{
			Metric: "cron.jobs.count", Value: float64(bySource[source]),
			Unit: collect.UnitCount, Kind: collect.KindGauge, Time: now,
			Labels: map[string]string{"source": string(source)},
		})
	}

	samples = append(samples,
		collect.Sample{
			Metric: "cron.jobs.total", Value: float64(len(jobs)),
			Unit: collect.UnitCount, Kind: collect.KindGauge, Time: now,
		},
		// Root jobs are called out separately: a new one appearing is worth
		// noticing, and it is the number a change-detection rule watches.
		collect.Sample{
			Metric: "cron.jobs.root", Value: float64(root),
			Unit: collect.UnitCount, Kind: collect.KindGauge, Time: now,
		},
	)

	return samples, nil
}
