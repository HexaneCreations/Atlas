package app

import (
	"context"
	"log/slog"
	"sync"

	"github.com/hexane/atlas/internal/core/collect"
	coreeventstore "github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/core/scheduler"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/hostid"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/cron"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/hexane/atlas/internal/plugin/ports"
	"github.com/hexane/atlas/internal/plugin/process"
	"github.com/hexane/atlas/internal/plugin/service"
	"github.com/hexane/atlas/internal/plugin/system"
	storageeventstore "github.com/hexane/atlas/internal/storage/eventstore"
	"github.com/hexane/atlas/internal/storage/metric"
)

// collectionPipeline wires observation from collectors through to storage.
//
// It is a lifecycle component rather than something built in [New] because
// almost everything it needs — the repository, the sink — requires a live
// connection pool, and the pool has no connections until it starts. Building
// the pipeline in its own Start, ordered after the pool and before the HTTP
// server, keeps that dependency expressed in the startup order instead of
// hidden behind a lazily-initialised field.
//
// Its accessors return nil before Start. Nothing observes that: the HTTP
// server is registered after this component, so no request can arrive until
// the pipeline is up.
type collectionPipeline struct {
	cfg      *config.Config
	logger   *slog.Logger
	bus      *eventbus.Bus
	pool     *postgres.Pool
	identity hostid.Identity

	// mu guards the fields below, which Start populates and API handlers read
	// concurrently.
	// onEvent is called after every event is durably persisted, local or
	// fleet-wide — see [coreeventstore.Tap]. Wires the alert rule engine in
	// without this package depending on it.
	onEvent func(context.Context, coreeventstore.Record)

	mu          sync.RWMutex
	repo        *metric.Repository
	sink        *metric.Sink
	transport   *transport.InProcess
	eventWriter *coreeventstore.Writer
	plugins     *plugin.Registry
	collectors  *collect.Registry
	scheduler   *scheduler.Scheduler
	// docker is held so the API can serve the container list and log tails
	// live, rather than reconstructing them from stored samples.
	docker  *docker.Plugin
	process *process.Plugin
	service *service.Plugin
	cron    *cron.Plugin
	ports   *ports.Plugin
	// system is held for Mounts(), the same live-inventory treatment its
	// siblings get — the metrics it also produces go through the scheduler
	// like everyone else's.
	system *system.Plugin
}

func newCollectionPipeline(cfg *config.Config, logger *slog.Logger, bus *eventbus.Bus, pool *postgres.Pool,
	identity hostid.Identity, onEvent func(context.Context, coreeventstore.Record)) *collectionPipeline {
	return &collectionPipeline{
		cfg: cfg, logger: logger, bus: bus, pool: pool, identity: identity, onEvent: onEvent,
	}
}

// Name implements [lifecycle.Component].
func (c *collectionPipeline) Name() string { return "collect.pipeline" }

// Start builds the pipeline and begins collecting.
//
// The order here is the data path in reverse: storage first, then the sink
// that writes to it, then the transport that feeds the sink, then the plugins
// that produce observations, and finally the scheduler that drives them. Each
// stage exists before anything can hand work to it.
func (c *collectionPipeline) Start(ctx context.Context) error {
	const op = "app.collectionPipeline.Start"

	repo := metric.NewRepository(c.pool.DB())
	sink := metric.NewSink(repo, c.logger)
	tr := transport.NewInProcess(sink)

	eventStore := coreeventstore.Store(storageeventstore.NewRepository(c.pool.DB()))
	if c.onEvent != nil {
		eventStore = coreeventstore.Tap(eventStore, c.onEvent)
	}
	eventWriter := coreeventstore.New(coreeventstore.Options{
		Bus: c.bus, Store: eventStore,
		NodeID: c.identity.NodeID, Logger: c.logger,
	})
	if err := eventWriter.Start(ctx); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not start the event store writer").WithOp(op)
	}

	// Plugins are registered here, in the composition root. Adding a
	// technology means one line in this list plus a new package — the core
	// never learns about Docker or Redis.
	plugins := plugin.NewRegistry(c.logger, nil).WithConfig(c.pluginSection)
	systemPlugin := system.New(system.Options{
		NodeID:   c.identity.NodeID,
		Recorder: factsRecorder{repo: repo},
	})
	dockerPlugin := docker.New(docker.Options{})
	processPlugin := process.New(process.Options{})
	servicePlugin := service.New(service.Options{})
	cronPlugin := cron.New(cron.Options{})
	portsPlugin := ports.New(ports.Options{})

	for _, p := range []plugin.Plugin{
		systemPlugin,
		dockerPlugin,
		processPlugin,
		servicePlugin,
		cronPlugin,
		portsPlugin,
	} {
		if err := plugins.Register(p); err != nil {
			return errs.Wrap(err, errs.CodeInternal, "could not register a plugin").WithOp(op)
		}
	}

	env := plugin.Env{
		Logger: c.logger,
		Bus:    c.bus,
		NodeID: c.identity.NodeID,
		// The decoder is bound per plugin inside the registry, which knows
		// which plugin it is activating. Here it is only the lookup.
		Config: plugin.NoConfig,
	}
	if err := plugins.Activate(ctx, env); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not activate plugins").WithOp(op)
	}

	collectors := collect.NewRegistry()
	if err := plugins.RegisterCollectors(collectors); err != nil {
		// A collector id collision between plugins is a programming error, not
		// a runtime condition to route around.
		return errs.Wrap(err, errs.CodeInternal, "could not register collectors").WithOp(op)
	}

	sched, err := scheduler.New(scheduler.Options{
		Registry:  collectors,
		Transport: tr,
		Origin: transport.Origin{
			NodeID:       c.identity.NodeID,
			Hostname:     c.identity.Hostname,
			AgentVersion: agentVersion(),
			Environment:  c.cfg.Node.Environment,
		},
		Bus:    c.bus,
		Logger: c.logger,
		Config: c.cfg.Collection,
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not build the collector scheduler").WithOp(op)
	}

	c.mu.Lock()
	c.repo, c.sink, c.transport = repo, sink, tr
	c.eventWriter = eventWriter
	c.plugins, c.collectors, c.scheduler = plugins, collectors, sched
	c.docker = dockerPlugin
	c.process, c.service, c.cron = processPlugin, servicePlugin, cronPlugin
	c.ports = portsPlugin
	c.system = systemPlugin
	c.mu.Unlock()

	if err := sched.Start(ctx); err != nil {
		return err
	}

	c.logger.InfoContext(ctx, "collection pipeline ready",
		slog.String("node_id", c.identity.NodeID),
		slog.String("hostname", c.identity.Hostname),
		slog.String("identity_source", string(c.identity.Source)),
		slog.Int("plugins_active", len(plugins.ActiveStates())),
		slog.Int("collectors", collectors.Len()),
	)

	// An ephemeral identity means every restart appears as a new machine, and
	// this host's history fragments. It is recoverable — an operator can pin
	// node.id — but only if they are told.
	if c.identity.Ephemeral() {
		c.logger.WarnContext(ctx, "node identity is not persisted; this host will appear as a new node after a restart",
			slog.String("node_id", c.identity.NodeID),
			slog.String("remedy", "set node.id, or make node.id_file writable"))
	}
	return nil
}

// Stop shuts the pipeline down in reverse: stop producing, then close what
// consumes, so nothing is collected after the path to storage is gone.
func (c *collectionPipeline) Stop(ctx context.Context) error {
	c.mu.RLock()
	sched, plugins, tr, eventWriter := c.scheduler, c.plugins, c.transport, c.eventWriter
	c.mu.RUnlock()

	var failures []error
	if sched != nil {
		if err := sched.Stop(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	if plugins != nil {
		if err := plugins.Close(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	if eventWriter != nil {
		if err := eventWriter.Stop(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	if tr != nil {
		if err := tr.Close(); err != nil {
			failures = append(failures, err)
		}
	}
	return errs.Join(failures...)
}

// Repository returns the metric repository, or nil before Start.
func (c *collectionPipeline) Repository() *metric.Repository {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.repo
}

// CollectorHealth reports per-collector reliability, or nil before Start.
func (c *collectionPipeline) CollectorHealth() []scheduler.CollectorHealth {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.scheduler == nil {
		return nil
	}
	return c.scheduler.Health()
}

// PluginStates reports plugin activation outcomes, or nil before Start.
func (c *collectionPipeline) PluginStates() []plugin.State {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.plugins == nil {
		return nil
	}
	return c.plugins.States()
}

// SchedulerStats reports collection activity.
func (c *collectionPipeline) SchedulerStats() scheduler.Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.scheduler == nil {
		return scheduler.Stats{}
	}
	return c.scheduler.Stats()
}

// IngestStats reports how much has been persisted.
func (c *collectionPipeline) IngestStats() metric.Stats {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.sink == nil {
		return metric.Stats{}
	}
	return c.sink.Stats()
}

// DockerClient returns the live Docker client, or nil when Docker is absent
// or the pipeline has not started.
func (c *collectionPipeline) DockerClient() docker.Client {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.docker == nil {
		return nil
	}
	return c.docker.DockerClient()
}

// Processes returns the live process inventory, or nil if the plugin is
// inactive on this host.
func (c *collectionPipeline) Processes(ctx context.Context, scope inventory.Scope) ([]process.Process, error) {
	const op = "app.collectionPipeline.Processes"

	if !scope.IsLocal(c.identity.NodeID) {
		return nil, inventory.ErrRemoteUnavailable(op, scope.NodeID, "process inventory")
	}

	c.mu.RLock()
	p := c.process
	c.mu.RUnlock()
	if p == nil {
		return nil, nil
	}
	return p.Inventory(ctx)
}

// Services returns the live systemd unit inventory, or nil where there is no
// service manager.
func (c *collectionPipeline) Services(ctx context.Context, scope inventory.Scope) ([]service.Unit, error) {
	const op = "app.collectionPipeline.Services"

	if !scope.IsLocal(c.identity.NodeID) {
		return nil, inventory.ErrRemoteUnavailable(op, scope.NodeID, "service inventory")
	}

	c.mu.RLock()
	p := c.service
	c.mu.RUnlock()
	if p == nil {
		return nil, nil
	}
	return p.Inventory(ctx)
}

// ServiceGraph returns the systemd dependency graph with live state overlaid,
// or nil where there is no service manager.
func (c *collectionPipeline) ServiceGraph(ctx context.Context, scope inventory.Scope) (*service.Graph, error) {
	const op = "app.collectionPipeline.ServiceGraph"

	if !scope.IsLocal(c.identity.NodeID) {
		return nil, inventory.ErrRemoteUnavailable(op, scope.NodeID, "service dependency graph")
	}

	c.mu.RLock()
	p := c.service
	c.mu.RUnlock()
	if p == nil {
		return nil, nil
	}
	return p.Graph(ctx)
}

// CronJobs returns the live scheduled-job inventory.
func (c *collectionPipeline) CronJobs(ctx context.Context, scope inventory.Scope) ([]cron.Job, error) {
	const op = "app.collectionPipeline.CronJobs"

	if !scope.IsLocal(c.identity.NodeID) {
		return nil, inventory.ErrRemoteUnavailable(op, scope.NodeID, "scheduled jobs")
	}

	c.mu.RLock()
	p := c.cron
	c.mu.RUnlock()
	if p == nil {
		return nil, nil
	}
	return p.Inventory(ctx)
}

// Ports returns the live listening-port inventory, or nil if the plugin is
// inactive on this host.
func (c *collectionPipeline) Ports(ctx context.Context, scope inventory.Scope) ([]ports.Listener, error) {
	const op = "app.collectionPipeline.Ports"

	if !scope.IsLocal(c.identity.NodeID) {
		return nil, inventory.ErrRemoteUnavailable(op, scope.NodeID, "listening ports")
	}

	c.mu.RLock()
	p := c.ports
	c.mu.RUnlock()
	if p == nil {
		return nil, nil
	}
	return p.Inventory(ctx)
}

// Mounts returns the live filesystem inventory. Unlike the optional plugins,
// this is always available once the pipeline has started — every host this
// runs on has a root filesystem.
func (c *collectionPipeline) Mounts(ctx context.Context, scope inventory.Scope) ([]system.MountInfo, error) {
	const op = "app.collectionPipeline.Mounts"

	if !scope.IsLocal(c.identity.NodeID) {
		return nil, inventory.ErrRemoteUnavailable(op, scope.NodeID, "filesystem inventory")
	}

	c.mu.RLock()
	p := c.system
	c.mu.RUnlock()
	if p == nil {
		return nil, nil
	}
	return p.Mounts(ctx)
}

// pluginActive reports whether a plugin came up on this host, so the API can
// distinguish "nothing found" from "not available here".
func (c *collectionPipeline) PluginActive(id string) bool {
	for _, st := range c.PluginStates() {
		if st.ID == id {
			return st.Active()
		}
	}
	return false
}

// Identity returns this node's resolved identity.
func (c *collectionPipeline) Identity() hostid.Identity { return c.identity }

// pluginSection returns a decoder for one plugin's configuration section.
//
// Each plugin decodes its own section into its own type. The core stores the
// section as an opaque YAML node and never interprets it, which is what lets a
// new plugin bring new settings without this package changing.
func (c *collectionPipeline) pluginSection(pluginID string) func(any) error {
	node, ok := c.cfg.Plugins[pluginID]
	if !ok {
		return nil
	}
	return func(target any) error {
		if err := node.Decode(target); err != nil {
			return errs.Wrap(err, errs.CodeInvalidArgument,
				"the configuration section for plugin %q is not valid", pluginID).
				WithOp("app.collectionPipeline.pluginSection").
				WithDetail("plugin_id", pluginID)
		}
		return nil
	}
}

// factsRecorder adapts the metric repository to the system plugin's recorder
// contract.
//
// The two NodeFacts types are deliberately separate: the plugin owns the
// contract it depends on, and storage owns its own shape. This adapter is the
// three lines that keeps a plugin from importing the storage package.
type factsRecorder struct{ repo *metric.Repository }

func (r factsRecorder) UpdateNodeFacts(ctx context.Context, nodeID string, facts system.NodeFacts) error {
	return r.repo.UpdateNodeFacts(ctx, nodeID, metric.NodeFacts{
		OS:           facts.OS,
		Platform:     facts.Platform,
		Kernel:       facts.Kernel,
		Architecture: facts.Architecture,
		CPUCores:     facts.CPUCores,
		BootTime:     facts.BootTime,
	})
}
