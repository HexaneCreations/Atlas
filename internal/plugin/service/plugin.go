package service

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

// Settings is the plugin's configuration section (`plugins.service`).
type Settings struct {
	// Watch names units that always get their own series, whether or not they
	// are failing. Use it for the services an operator cares about
	// individually — nginx, postgresql, sshd.
	//
	// Without this list, only failed units get per-unit series. That is the
	// cardinality trade: a host can have three hundred units, and giving each
	// one a permanent series to say "still fine" is a lot of storage for very
	// little signal.
	Watch []string `yaml:"watch"`
}

// defaultWatch are units worth tracking individually wherever they exist.
// Chosen because their failure is an incident on almost any host.
var defaultWatch = []string{
	"sshd.service", "ssh.service",
	"docker.service", "containerd.service",
	"nginx.service", "apache2.service", "httpd.service",
	"postgresql.service", "mysql.service", "mariadb.service",
	"redis.service", "redis-server.service",
	"cron.service", "crond.service",
}

// Plugin observes system services.
type Plugin struct {
	provider Provider

	mu         sync.RWMutex
	collectors []collect.Collector
	watch      []string

	graph *graphCache
}

// Options configures the plugin.
type Options struct {
	// Provider supplies service facts. Defaults to systemd.
	Provider Provider
}

// New builds the service plugin.
func New(opts Options) *Plugin {
	if opts.Provider == nil {
		opts.Provider = NewSystemdProvider()
	}
	return &Plugin{provider: opts.Provider, graph: newGraphCache(opts.Provider)}
}

// Descriptor implements [plugin.Plugin].
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:          "service",
		Name:        "Services",
		Description: "systemd unit states, failures, restarts, and resource use.",
		Subject:     "systemd",
	}
}

// Detect reports whether a service manager is present.
//
// A macOS laptop, an Alpine container, or a BSD host has no systemd, and must
// report no service integration rather than a broken one.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	return p.provider.Available(ctx), nil
}

// Init implements [plugin.Plugin].
func (p *Plugin) Init(_ context.Context, env plugin.Env) error {
	const op = "service.Plugin.Init"

	var settings Settings
	if err := env.Config(&settings); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "invalid service plugin configuration").WithOp(op)
	}

	logger := env.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	watch := slices.Clone(defaultWatch)
	watch = append(watch, settings.Watch...)

	p.mu.Lock()
	p.watch = watch
	p.collectors = []collect.Collector{newServiceCollector(p.provider, watch, logger)}
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

// Inventory returns the live unit list, failed units first.
func (p *Plugin) Inventory(ctx context.Context) ([]Unit, error) {
	units, err := p.provider.Units(ctx)
	if err != nil {
		return nil, err
	}

	slices.SortFunc(units, func(a, b Unit) int {
		if a.Failed() != b.Failed() {
			if a.Failed() {
				return -1
			}
			return 1
		}
		return compareStrings(a.Name, b.Name)
	})
	return units, nil
}

// Graph returns the dependency graph with current unit state overlaid.
//
// The structure is cached and refreshed on its own slow schedule — unit files
// change on deploy, not between page loads — while the states painted over it
// are read fresh on every call. A caller therefore gets a graph whose shape
// costs two process spawns per refresh interval and whose states are never
// stale.
func (p *Plugin) Graph(ctx context.Context) (*Graph, error) {
	structure, err := p.graph.get(ctx)
	if err != nil {
		return nil, err
	}

	units, err := p.provider.Units(ctx)
	if err != nil {
		// The structure is still worth returning. Its embedded states are as
		// of the last structure refresh, which is stale but not wrong, and a
		// graph with slightly old states beats no graph during a transient
		// manager failure.
		return structure, nil
	}
	return structure.WithState(units), nil
}

// graphRefreshInterval bounds how stale a cached structure may be.
//
// Five minutes because unit files change on deploy, not continuously, and the
// collection costs two systemctl spawns. Live state is never this stale — it
// is read on every request and overlaid.
const graphRefreshInterval = 5 * time.Minute

// graphCache holds the parsed dependency structure between refreshes.
//
// Refresh is single-flighted: a burst of concurrent requests after expiry
// produces one collection, not one per request. Without that, a page whose
// panels each fetch the graph would spawn systemctl once per panel.
type graphCache struct {
	provider Provider

	mu        sync.Mutex
	graph     *Graph
	fetchedAt time.Time
	// inflight is non-nil while a refresh is running; waiters block on it.
	inflight chan struct{}
	err      error
}

func newGraphCache(p Provider) *graphCache { return &graphCache{provider: p} }

func (c *graphCache) get(ctx context.Context) (*Graph, error) {
	c.mu.Lock()

	if c.graph != nil && time.Since(c.fetchedAt) < graphRefreshInterval {
		g := c.graph
		c.mu.Unlock()
		return g, nil
	}

	if c.inflight != nil {
		wait := c.inflight
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		c.mu.Lock()
		g, err := c.graph, c.err
		c.mu.Unlock()
		if g == nil {
			return nil, err
		}
		return g, nil
	}

	done := make(chan struct{})
	c.inflight = done
	c.mu.Unlock()

	structure, err := c.provider.Structure(ctx)

	c.mu.Lock()
	c.inflight = nil
	c.err = err
	if err == nil {
		c.graph = NewGraph(structure.Nodes, structure.Edges, structure.CollectedAt)
		c.fetchedAt = time.Now()
	}
	g := c.graph
	c.mu.Unlock()
	close(done)

	if g == nil {
		return nil, err
	}
	// A refresh that failed while a previous graph is still held returns the
	// old one: a slightly stale shape is more useful than an error.
	return g, nil
}

func compareStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

var _ plugin.Plugin = (*Plugin)(nil)

// serviceCollector reports unit states and per-unit detail for the units that
// warrant it.
type serviceCollector struct {
	provider Provider
	watch    []string
	logger   *slog.Logger
}

func newServiceCollector(p Provider, watch []string, logger *slog.Logger) *serviceCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &serviceCollector{provider: p, watch: watch, logger: logger}
}

func (c *serviceCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "service.units",
		Name:        "Services",
		Description: "systemd unit counts by state, plus detail for failed and watched units.",
		// Unit state changes on deploys and failures, not continuously, and
		// each run spawns systemctl.
		Interval: 60 * time.Second,
		Timeout:  45 * time.Second,
	}
}

func (c *serviceCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	units, err := c.provider.Units(ctx)
	if err != nil {
		return nil, err
	}

	byState := map[ActiveState]int{}
	samples := make([]collect.Sample, 0, 16)

	for _, u := range units {
		byState[u.ActiveState]++

		// Per-unit series are given only to units that are failing or that an
		// operator asked to watch. A host can have three hundred units, and a
		// permanent series per unit saying "still fine" is a great deal of
		// storage for very little signal.
		if !u.Failed() && !c.watched(u.Name) {
			continue
		}
		samples = append(samples, c.unitSamples(u, now)...)
	}

	for _, state := range []ActiveState{
		ActiveStateActive, ActiveStateInactive, ActiveStateFailed,
		ActiveStateActivating, ActiveStateDeactivating,
	} {
		samples = append(samples, labelled("service.count", float64(byState[state]),
			collect.UnitCount, now, map[string]string{"state": string(state)}))
	}

	samples = append(samples,
		gauge("service.total", float64(len(units)), collect.UnitCount, now),
		// Failed units get their own metric as well as a state count: it is
		// the number an alert rule watches, and burying it as one label value
		// among five makes that rule harder to write and easier to get wrong.
		gauge("service.failed", float64(byState[ActiveStateFailed]), collect.UnitCount, now),
	)

	return samples, nil
}

func (c *serviceCollector) unitSamples(u Unit, now time.Time) []collect.Sample {
	labels := map[string]string{"unit": u.Name}

	up := 0.0
	if u.Running() {
		up = 1
	}

	out := []collect.Sample{
		labelled("service.up", up, collect.UnitCount, now, labels),
		labelled("service.restarts", float64(u.RestartCount), collect.UnitCount, now, labels),
	}

	// A unit that is running but not enabled will vanish on the next reboot —
	// an outage that is invisible until it happens.
	enabled := 0.0
	if u.Enabled {
		enabled = 1
	}
	out = append(out, labelled("service.enabled", enabled, collect.UnitCount, now, labels))

	if u.Running() && !u.ActiveSince.IsZero() {
		out = append(out, labelled("service.uptime", u.Uptime().Seconds(), collect.UnitSeconds, now, labels))
	}
	if u.MemoryBytes > 0 {
		out = append(out, labelled("service.memory", float64(u.MemoryBytes), collect.UnitBytes, now, labels))
	}
	return out
}

func (c *serviceCollector) watched(name string) bool {
	return slices.Contains(c.watch, name)
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
