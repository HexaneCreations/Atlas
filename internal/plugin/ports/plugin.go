package ports

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Settings is the plugin's configuration section (`plugins.ports`).
type Settings struct {
	// Watch names ports that always get their own up/down series, whether or
	// not they are currently listening — the same trade the service plugin
	// makes for units. Use it for the ports an operator cares about
	// individually: a database, a load balancer's health port, an internal
	// API.
	//
	// Without this list, "is port 5432 up" can only be answered from the live
	// inventory, not charted over time.
	Watch []int `yaml:"watch"`
	// MaxTLSProbes bounds how many ports get a TLS handshake attempt per
	// collection. Probing is comparatively expensive — a real network round
	// trip per port — so it is capped rather than attempted against every
	// listening socket on a host that happens to have hundreds.
	MaxTLSProbes int `yaml:"max_tls_probes"`
}

func (s Settings) maxTLSProbes() int {
	if s.MaxTLSProbes > 0 {
		return s.MaxTLSProbes
	}
	return defaultMaxTLSProbes
}

// defaultWatch are ports worth tracking individually wherever they are
// found. Chosen because a service disappearing from one of them is usually
// an incident on any host that had it.
var defaultWatch = []int{22, 80, 443, 3306, 5432, 6379, 9200, 27017, 5672}

const (
	defaultMaxTLSProbes = 20
	tlsProbeTimeout     = 1500 * time.Millisecond
	// tlsProbeConcurrency bounds simultaneous handshake attempts, so a run of
	// twenty probes takes as long as the slowest few rather than the sum of
	// all of them.
	tlsProbeConcurrency = 8
)

// Plugin observes listening ports and the TLS certificates behind them.
type Plugin struct {
	provider Provider

	mu         sync.RWMutex
	watch      []int
	settings   Settings
	collectors []collect.Collector

	certs *certCache
}

// Options configures the plugin.
type Options struct {
	// Provider supplies the connection table. Defaults to gopsutil.
	Provider Provider
}

// New builds the ports plugin.
func New(opts Options) *Plugin {
	if opts.Provider == nil {
		opts.Provider = NewProvider()
	}
	return &Plugin{provider: opts.Provider, certs: newCertCache()}
}

// Descriptor implements [plugin.Plugin].
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:          "ports",
		Name:        "Ports",
		Description: "Listening TCP and UDP ports, and the TLS certificates behind them.",
		Subject:     "network",
	}
}

// Detect reports whether the connection table can be read here.
//
// Every host with a network stack has one to read, so unlike Docker or
// systemd this is not conditional on optional software — only on whether
// Atlas has enough access to read it at all, which a sufficiently locked-down
// sandbox can still refuse.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	if _, err := p.provider.ListenPorts(ctx); err != nil {
		return false, err
	}
	return true, nil
}

// Init implements [plugin.Plugin].
func (p *Plugin) Init(_ context.Context, env plugin.Env) error {
	const op = "ports.Plugin.Init"

	var settings Settings
	if err := env.Config(&settings); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "invalid ports plugin configuration").WithOp(op)
	}

	logger := env.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	watch := slices.Clone(defaultWatch)
	watch = append(watch, settings.Watch...)

	p.mu.Lock()
	p.watch = watch
	p.settings = settings
	p.collectors = []collect.Collector{
		newPortCollector(p.provider, watch, logger),
		newTLSCollector(p.provider, watch, settings.maxTLSProbes(), p.certs, logger),
	}
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

// Listener is one listening port as served to the API: the live socket, plus
// whatever certificate the most recent TLS probe found there.
type Listener struct {
	Socket Port
	// TLSProbed reports whether the last probe cycle attempted a handshake on
	// this port. Without it, a nil TLS field is ambiguous between "plaintext"
	// and "not looked at" — see [certCache].
	TLSProbed bool
	// TLS is nil for a plaintext service, or a TCP port not yet reached by a
	// probe cycle — probing runs on its own, slower interval, so a port that
	// just started listening shows no certificate until the next sweep
	// rather than blocking the inventory request to find out.
	TLS *Certificate
}

// Inventory returns the live port list, enriched with cached certificate
// data where a probe has found one.
//
// The port list itself is read fresh on every call — it is a cheap syscall
// or /proc read, the same as the process and service inventories. Only the
// TLS data is cached, because establishing a TLS connection per port on
// every request would make this endpoint as slow as the slowest listening
// service, on every page load.
func (p *Plugin) Inventory(ctx context.Context) ([]Listener, error) {
	live, err := p.provider.ListenPorts(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Listener, 0, len(live))
	for _, port := range live {
		l := Listener{Socket: port}
		if port.Protocol == ProtocolTCP {
			l.TLSProbed = p.certs.wasProbed(port.Port)
			if cert, ok := p.certs.get(port.Port); ok {
				l.TLS = &cert
			}
		}
		out = append(out, l)
	}

	slices.SortFunc(out, func(a, b Listener) int {
		if a.Socket.Port != b.Socket.Port {
			return int(a.Socket.Port) - int(b.Socket.Port)
		}
		return strings.Compare(string(a.Socket.Protocol), string(b.Socket.Protocol))
	})
	return out, nil
}

var _ plugin.Plugin = (*Plugin)(nil)

// certCache holds the most recent probe cycle: which ports were attempted,
// and which of those presented a certificate.
//
// Recording the attempts matters as much as the results. Probing is bounded
// by a budget and covers TCP only, so a port with no certificate has two very
// different possible meanings — "this service is plaintext" and "Atlas has not
// looked at this port" — and a security view that renders both as "no TLS"
// tells the reader something it does not know.
//
// Replaced wholesale after each cycle rather than merged incrementally: a port
// that stopped answering TLS (redeployed as plaintext, port reassigned) must
// have its stale entry disappear, which an incremental update would never do.
type certCache struct {
	mu     sync.RWMutex
	byPort map[uint32]Certificate
	probed map[uint32]struct{}
}

func newCertCache() *certCache {
	return &certCache{byPort: map[uint32]Certificate{}, probed: map[uint32]struct{}{}}
}

func (c *certCache) get(port uint32) (Certificate, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	cert, ok := c.byPort[port]
	return cert, ok
}

// wasProbed reports whether the last cycle attempted a handshake on this port.
func (c *certCache) wasProbed(port uint32) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, ok := c.probed[port]
	return ok
}

func (c *certCache) replace(found map[uint32]Certificate, attempted map[uint32]struct{}) {
	c.mu.Lock()
	c.byPort = found
	c.probed = attempted
	c.mu.Unlock()
}

// portCollector reports listening-port counts and per-watched-port liveness.
type portCollector struct {
	provider Provider
	watch    []int
	logger   *slog.Logger
}

func newPortCollector(p Provider, watch []int, logger *slog.Logger) *portCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &portCollector{provider: p, watch: watch, logger: logger}
}

func (c *portCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "ports.listening",
		Name:        "Listening ports",
		Description: "Counts of listening ports by protocol, plus up/down for watched ports.",
		Interval:    30 * time.Second,
		Timeout:     15 * time.Second,
	}
}

func (c *portCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	ports, err := c.provider.ListenPorts(ctx)
	if err != nil {
		return nil, err
	}

	byPort := map[uint32]bool{}
	tcp, udp := 0, 0
	for _, p := range ports {
		byPort[p.Port] = true
		if p.Protocol == ProtocolTCP {
			tcp++
		} else {
			udp++
		}
	}

	samples := make([]collect.Sample, 0, len(c.watch)+4)
	samples = append(samples,
		gauge("port.listening.total", float64(len(ports)), collect.UnitCount, now),
		labelled("port.listening.count", float64(tcp), collect.UnitCount, now, map[string]string{"protocol": "tcp"}),
		labelled("port.listening.count", float64(udp), collect.UnitCount, now, map[string]string{"protocol": "udp"}),
	)

	for _, port := range c.watch {
		up := 0.0
		if byPort[uint32(port)] {
			up = 1
		}
		samples = append(samples, labelled("port.watched.up", up, collect.UnitCount, now,
			map[string]string{"port": strconv.Itoa(port)}))
	}

	return samples, nil
}

// tlsCollector probes for certificates on watched and discovered ports, and
// publishes expiry as a metric for the ports it finds.
//
// Only watched ports and a bounded set of the rest get a metric, but the full
// probe results — including ports that were not watched — are cached for
// [Plugin.Inventory] to serve, which is a wider view than the cardinality
// budget allows onto a time series.
type tlsCollector struct {
	provider Provider
	watch    []int
	maxProbe int
	cache    *certCache
	logger   *slog.Logger
}

func newTLSCollector(p Provider, watch []int, maxProbe int, cache *certCache, logger *slog.Logger) *tlsCollector {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	return &tlsCollector{provider: p, watch: watch, maxProbe: maxProbe, cache: cache, logger: logger}
}

func (c *tlsCollector) Descriptor() collect.Descriptor {
	return collect.Descriptor{
		ID:          "ports.tls",
		Name:        "TLS certificates",
		Description: "Certificate expiry for listening TLS services, discovered by probing.",
		// Certificates do not change minute to minute, and a probe is a real
		// network round trip per port rather than a /proc read — the same
		// reasoning that puts the cron collector on a slow interval.
		Interval: 5 * time.Minute,
		Timeout:  30 * time.Second,
	}
}

func (c *tlsCollector) Collect(ctx context.Context) ([]collect.Sample, error) {
	now := time.Now()

	live, err := c.provider.ListenPorts(ctx)
	if err != nil {
		return nil, err
	}

	targets := c.probeTargets(live)
	found := c.probeAll(ctx, targets)

	attempted := make(map[uint32]struct{}, len(targets))
	for port := range targets {
		attempted[port] = struct{}{}
	}
	c.cache.replace(found, attempted)

	watched := make(map[int]bool, len(c.watch))
	for _, port := range c.watch {
		watched[port] = true
	}

	samples := make([]collect.Sample, 0, len(found))
	for port, cert := range found {
		if !watched[int(port)] {
			// Cardinality budget: only ports an operator named get a
			// permanent series. Everything discovered is still visible live
			// through the inventory endpoint.
			continue
		}
		samples = append(samples, labelled("ssl.cert.expiry_seconds", time.Until(cert.NotAfter).Seconds(),
			collect.UnitSeconds, now, map[string]string{"port": strconv.Itoa(int(port))}))
	}
	return samples, nil
}

// probeTargets selects which currently-listening TCP ports to attempt a
// handshake against: watched ports first, so the probe budget is spent on
// what an operator named before anything merely discovered, then the
// remaining live TCP ports in ascending order until the budget is spent.
//
// A watched port not currently listening has nothing to probe — dropping it
// here is what lets [certCache.replace] correctly clear a stale certificate
// for a service that stopped answering, rather than that entry surviving
// forever on a guess.
func (c *tlsCollector) probeTargets(live []Port) map[uint32]string {
	watched := make(map[int]bool, len(c.watch))
	for _, port := range c.watch {
		watched[port] = true
	}

	var priority, rest []Port
	for _, p := range live {
		if p.Protocol != ProtocolTCP {
			continue
		}
		if watched[int(p.Port)] {
			priority = append(priority, p)
		} else {
			rest = append(rest, p)
		}
	}
	slices.SortFunc(rest, func(a, b Port) int { return int(a.Port) - int(b.Port) })

	targets := make(map[uint32]string, c.maxProbe)
	for _, p := range append(priority, rest...) {
		if len(targets) >= c.maxProbe {
			break
		}
		targets[p.Port] = dialAddress(p.Address, p.Port)
	}
	return targets
}

// dialAddress resolves a socket's bind address to something Atlas can
// actually connect to. A wildcard bind (0.0.0.0, ::) accepts connections on
// every local address including loopback, so probing there is always valid
// regardless of what the host's real routable address happens to be.
//
// Every [Port] reaching this function has already passed through
// [normaliseAddress], so the platform-specific spelling of "wildcard" —
// lsof's bare "*" on macOS and the BSDs versus Linux's literal 0.0.0.0 — is
// not this function's concern.
func dialAddress(bindAddr string, port uint32) string {
	switch bindAddr {
	case "", "0.0.0.0", "::", "::0":
		bindAddr = "127.0.0.1"
	}
	return net.JoinHostPort(bindAddr, strconv.FormatUint(uint64(port), 10))
}

// probeAll attempts every target concurrently, bounded by
// tlsProbeConcurrency, and returns only the ports that answered with a
// certificate.
func (c *tlsCollector) probeAll(ctx context.Context, targets map[uint32]string) map[uint32]Certificate {
	found := make(map[uint32]Certificate, len(targets))
	if len(targets) == 0 {
		return found
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, tlsProbeConcurrency)

	for port, addr := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(port uint32, addr string) {
			defer wg.Done()
			defer func() { <-sem }()

			cert, ok := probeCertificate(ctx, addr, tlsProbeTimeout)
			if !ok {
				return
			}
			mu.Lock()
			found[port] = cert
			mu.Unlock()
		}(port, addr)
	}
	wg.Wait()
	return found
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
