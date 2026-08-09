package docker

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/platform/errs"
)

// Settings is the plugin's configuration section.
//
// Decoded from `plugins.docker` in the Atlas configuration file. The plugin
// owns this type; the core stores the section opaquely and never interprets
// it.
type Settings struct {
	// Host overrides the daemon endpoint. Empty uses DOCKER_HOST, then the
	// platform default socket.
	Host string `yaml:"host"`
	// StatsConcurrency bounds simultaneous stats reads. A host with hundreds
	// of containers would otherwise open hundreds of connections to the daemon
	// at once — a denial of service against the thing being monitored.
	StatsConcurrency int `yaml:"stats_concurrency"`
	// CollectEvents follows the daemon's event stream. On by default: it is
	// the only way to observe a container that started and died between two
	// polls.
	CollectEvents *bool `yaml:"collect_events"`
}

func (s Settings) statsConcurrency() int {
	if s.StatsConcurrency > 0 {
		return s.StatsConcurrency
	}
	return 8
}

func (s Settings) collectEvents() bool {
	return s.CollectEvents == nil || *s.CollectEvents
}

// Plugin observes the Docker Engine.
type Plugin struct {
	// newClient is injected so tests exercise the plugin against a fake daemon.
	newClient func(host string) (Client, error)

	mu         sync.Mutex
	client     Client
	settings   Settings
	collectors []collect.Collector
	streamers  []collect.Streamer
	version    Version
}

// Options configures the plugin.
type Options struct {
	// NewClient builds a Docker client. Defaults to the real engine client.
	NewClient func(host string) (Client, error)
}

// New builds the Docker plugin.
func New(opts Options) *Plugin {
	if opts.NewClient == nil {
		opts.NewClient = NewEngineClient
	}
	return &Plugin{newClient: opts.NewClient}
}

// Descriptor implements [plugin.Plugin].
func (p *Plugin) Descriptor() plugin.Descriptor {
	return plugin.Descriptor{
		ID:          "docker",
		Name:        "Docker",
		Description: "Containers, resource use, health, images, networks, volumes, events, and logs.",
		Subject:     "Docker Engine",
	}
}

// Detect reports whether a Docker daemon is present and answering.
//
// It pings rather than checking for the socket file. A socket that exists but
// does not answer — a daemon mid-restart, or one the current user cannot reach
// — would pass a file check and then fail every collection, reporting a broken
// integration where the honest answer is that Docker is not usable here.
//
// A daemon that is genuinely absent returns (false, nil): that is the expected
// state on most hosts and is not a fault.
func (p *Plugin) Detect(ctx context.Context) (bool, error) {
	client, err := p.newClient(p.hostFromSettings())
	if err != nil {
		return false, nil // no client is constructible: Docker is not here
	}

	version, err := client.Ping(ctx)
	if err != nil {
		_ = client.Close()
		return false, nil
	}

	p.mu.Lock()
	p.client, p.version = client, version
	p.mu.Unlock()
	return true, nil
}

func (p *Plugin) hostFromSettings() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.settings.Host
}

// Init implements [plugin.Plugin].
func (p *Plugin) Init(ctx context.Context, env plugin.Env) error {
	const op = "docker.Plugin.Init"

	var settings Settings
	if err := env.Config(&settings); err != nil {
		return errs.Wrap(err, errs.CodeInvalidArgument, "invalid Docker plugin configuration").WithOp(op)
	}

	logger := env.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	p.mu.Lock()
	p.settings = settings
	client := p.client
	version := p.version
	p.mu.Unlock()

	// Detect ran first and left a live client. A configured host that differs
	// from the one detection used means the operator changed it, so reconnect.
	if settings.Host != "" && client != nil {
		reconnected, err := p.newClient(settings.Host)
		if err != nil {
			return errs.Wrap(err, errs.CodeUnavailable,
				"could not connect to the configured Docker host").WithOp(op)
		}
		if v, err := reconnected.Ping(ctx); err == nil {
			_ = client.Close()
			client, version = reconnected, v
		} else {
			_ = reconnected.Close()
		}
	}
	if client == nil {
		return errs.New(errs.CodeUnavailable, "the Docker client was not initialised").WithOp(op)
	}

	collectors := []collect.Collector{
		newContainerCollector(client, logger),
		newStatsCollector(client, logger, settings.statsConcurrency()),
		newInventoryCollector(client, logger),
	}

	var streamers []collect.Streamer
	if settings.collectEvents() {
		streamers = append(streamers, newEventStreamer(client, env.Bus, env.NodeID, logger))
	}

	p.mu.Lock()
	p.client, p.version = client, version
	p.collectors, p.streamers = collectors, streamers
	p.mu.Unlock()

	logger.InfoContext(ctx, "docker plugin ready",
		slog.String("daemon_version", version.Version),
		slog.String("api_version", version.APIVersion),
		slog.Int("collectors", len(collectors)),
		slog.Int("streamers", len(streamers)))
	return nil
}

// Collectors implements [plugin.Plugin].
func (p *Plugin) Collectors() []collect.Collector {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.collectors
}

// Streamers implements [plugin.StreamingPlugin].
func (p *Plugin) Streamers() []collect.Streamer {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streamers
}

// Version returns the daemon's identity, for display.
func (p *Plugin) Version() Version {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.version
}

// DockerClient exposes the live client for the API layer, which serves the
// container list and log tails on demand rather than from stored samples.
//
// Returns nil before Init.
func (p *Plugin) DockerClient() Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.client
}

// Close implements [plugin.Plugin]. Idempotent.
func (p *Plugin) Close(context.Context) error {
	p.mu.Lock()
	client := p.client
	p.client = nil
	p.mu.Unlock()

	if client == nil {
		return nil
	}
	return client.Close()
}

var (
	_ plugin.Plugin          = (*Plugin)(nil)
	_ plugin.StreamingPlugin = (*Plugin)(nil)
)
