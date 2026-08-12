// Package agent is the composition root for atlas-agent: it collects the
// local host and pushes observations to a control plane over mTLS. See
// docs/architecture/agent-design.md.
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"time"

	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/plugin"
	"github.com/hexane/atlas/internal/core/scheduler"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/core/transport/remote"
	"github.com/hexane/atlas/internal/core/transport/spool"
	"github.com/hexane/atlas/internal/platform/build"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/eventbus"
	"github.com/hexane/atlas/internal/platform/hostid"
	"github.com/hexane/atlas/internal/plugin/cron"
	"github.com/hexane/atlas/internal/plugin/docker"
	"github.com/hexane/atlas/internal/plugin/ports"
	"github.com/hexane/atlas/internal/plugin/process"
	"github.com/hexane/atlas/internal/plugin/service"
	"github.com/hexane/atlas/internal/plugin/system"
	p2phost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

// Agent collects the local host and pushes to a control plane.
type Agent struct {
	cfg      Config
	logger   *slog.Logger
	identity hostid.Identity

	caCert    *x509.Certificate
	holder    *credentialHolder
	spool     *spool.Spool
	transport *remote.Transport
	p2pHost   p2phost.Host
	bus       *eventbus.Bus
	scheduler *scheduler.Scheduler
	plugins   *plugin.Registry
	pusher    *inventoryPusher
	forwarder *eventForwarder
}

// New resolves identity, bootstraps or loads a certificate, and wires the
// collection pipeline. It does not start collecting; call Run for that.
func New(ctx context.Context, cfg Config, logger *slog.Logger) (*Agent, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	identity, err := hostid.Resolve(hostid.Options{
		ConfiguredID: cfg.NodeID,
		StateFile:    filepath.Join(cfg.DataDir, "node-id"),
	})
	if err != nil {
		return nil, fmt.Errorf("resolve identity: %w", err)
	}
	logger.InfoContext(ctx, "identity resolved",
		slog.String("node_id", identity.NodeID), slog.String("source", string(identity.Source)))

	// dial overrides how the control plane is reached. nil keeps the default
	// TCP dial; the libp2p POC transport (docs/adr/0012-connect-by-identity.md)
	// sets it to route every enroll/renew/telemetry call over a libp2p stream
	// addressed by the control plane's Peer ID instead of its network address.
	// Nothing downstream of the dial — the HTTP protocol, TLS handshake, or
	// mTLS credential logic — changes either way.
	var dial dialContextFunc
	var p2pHost p2phost.Host
	if cfg.Transport == "libp2p" {
		h, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: cfg.DataDir})
		if err != nil {
			return nil, fmt.Errorf("start libp2p host: %w", err)
		}
		p2pHost = h

		switch {
		case cfg.LibP2PRelayAddr != "" && cfg.LibP2PServerPeerID != "":
			// Rendezvous discovery (docs/adr/0012-connect-by-identity.md):
			// the Agent knows only the Relay's address and the Server's
			// Peer ID, and looks the Server's current direct/circuit
			// addresses up on every dial — no manually assembled circuit
			// multiaddr required.
			relayInfo, err := libp2ptransport.ParseTarget(cfg.LibP2PRelayAddr)
			if err != nil {
				return nil, fmt.Errorf("parse libp2p relay address: %w", err)
			}
			serverID, err := peer.Decode(cfg.LibP2PServerPeerID)
			if err != nil {
				return nil, fmt.Errorf("parse libp2p server peer id: %w", err)
			}
			dial = newDiscoveryDial(h, relayInfo, serverID, cfg.DataDir, logger)
			logger.InfoContext(ctx, "dialing control plane via rendezvous discovery",
				slog.String("peer_id", serverID.String()), slog.String("relay", relayInfo.ID.String()))
		case cfg.LibP2PServerAddr != "":
			// Deprecated static-target path: kept for backward compatibility
			// with configs predating rendezvous discovery.
			target, err := libp2ptransport.ParseTarget(cfg.LibP2PServerAddr)
			if err != nil {
				return nil, fmt.Errorf("parse libp2p server address: %w", err)
			}
			dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return libp2ptransport.Dial(ctx, h, target)
			}
			logger.WarnContext(ctx, "dialing control plane by static multiaddr; "+
				"set ATLAS_AGENT_LIBP2P_RELAY_ADDR and ATLAS_AGENT_LIBP2P_SERVER_PEER_ID instead",
				slog.String("peer_id", target.ID.String()), slog.String("transport", "libp2p"))
		default:
			return nil, fmt.Errorf("ATLAS_AGENT_TRANSPORT=libp2p requires either " +
				"(ATLAS_AGENT_LIBP2P_RELAY_ADDR and ATLAS_AGENT_LIBP2P_SERVER_PEER_ID) " +
				"or the deprecated ATLAS_AGENT_LIBP2P_SERVER_ADDR")
		}
	}

	caCert, holder, err := bootstrapWithRetry(ctx, cfg, identity.NodeID, logger, dial)
	if err != nil {
		return nil, fmt.Errorf("bootstrap credentials: %w", err)
	}
	go renewalLoop(ctx, cfg, identity.NodeID, caCert, holder, logger, dial)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, GetClientCertificate: holder.GetClientCertificate}

	sp, err := spool.Open(spool.Options{Dir: filepath.Join(cfg.DataDir, "spool")})
	if err != nil {
		return nil, fmt.Errorf("open spool: %w", err)
	}

	var httpClient *http.Client
	if dial != nil {
		httpClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, DialContext: dial}}
	}
	tr, err := remote.New(remote.Options{
		BaseURL: cfg.ControlPlaneURL, TLSConfig: tlsCfg, HTTPClient: httpClient, Spool: sp, Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("build remote transport: %w", err)
	}

	bus := eventbus.New(eventbus.Options{Logger: logger})

	plugins := plugin.NewRegistry(logger, nil)
	dockerPlugin := docker.New(docker.Options{})
	processPlugin := process.New(process.Options{})
	servicePlugin := service.New(service.Options{})
	cronPlugin := cron.New(cron.Options{})
	portsPlugin := ports.New(ports.Options{})
	systemPlugin := system.New(system.Options{NodeID: identity.NodeID})

	for _, p := range []plugin.Plugin{systemPlugin, dockerPlugin, processPlugin, servicePlugin, cronPlugin, portsPlugin} {
		if err := plugins.Register(p); err != nil {
			return nil, fmt.Errorf("register plugin: %w", err)
		}
	}

	env := plugin.Env{Logger: logger, Bus: bus, NodeID: identity.NodeID, Config: plugin.NoConfig}
	if err := plugins.Activate(ctx, env); err != nil {
		return nil, fmt.Errorf("activate plugins: %w", err)
	}

	collectors := collect.NewRegistry()
	if err := plugins.RegisterCollectors(collectors); err != nil {
		return nil, fmt.Errorf("register collectors: %w", err)
	}

	sched, err := scheduler.New(scheduler.Options{
		Registry:  collectors,
		Transport: tr,
		Origin: transport.Origin{
			NodeID: identity.NodeID, Hostname: identity.Hostname,
			AgentVersion: build.Current().Version, Environment: cfg.Environment,
		},
		Logger: logger,
		Config: config.Collection{
			DefaultInterval: cfg.CollectionInterval, Timeout: cfg.CollectionTimeout,
			MaxConcurrent: 4, MaxSeriesPerCollector: 1000,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build scheduler: %w", err)
	}

	active := map[string]bool{}
	for _, st := range plugins.States() {
		active[st.ID] = st.Active()
	}

	origin := transport.Origin{
		NodeID: identity.NodeID, Hostname: identity.Hostname,
		AgentVersion: build.Current().Version, Environment: cfg.Environment,
	}
	pusher := newInventoryPusher(tr, origin, cfg.InventoryInterval, logger,
		inventorySources(active, processPlugin, servicePlugin, cronPlugin, portsPlugin, systemPlugin, dockerPlugin))
	forwarder := newEventForwarder(bus, tr, origin, logger)

	// AgentOps (remote container log streaming): registers a handler for one
	// more libp2p protocol on the same dial-only host — the control plane
	// opens the stream over the connection this Agent already established by
	// dialing out, never the reverse; see
	// internal/core/transport/libp2ptransport/agentops.go and ADR-0012's "the
	// agent has no inbound surface", which this preserves exactly: no new
	// listen address, no new port. Only registered when libp2p is the
	// active transport — an HTTPS-only agent has no such connection for the
	// control plane to reuse, so remote logs are simply unavailable for it in
	// this phase, by design (see the docs on RemoteLogSource in
	// internal/api/v1/containers.go).
	if p2pHost != nil {
		limiter := libp2ptransport.NewSessionLimiter(libp2ptransport.DefaultMaxConcurrentSessions)
		libp2ptransport.RegisterAgentOpsHandler(p2pHost, caCert,
			func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return holder.GetClientCertificate(nil) },
			containerLogsFunc(dockerPlugin), limiter)
	}

	return &Agent{
		cfg: cfg, logger: logger, identity: identity,
		caCert: caCert, holder: holder, spool: sp, transport: tr, p2pHost: p2pHost, bus: bus,
		scheduler: sched, plugins: plugins, pusher: pusher, forwarder: forwarder,
	}, nil
}

// Run starts collection and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.scheduler.Start(ctx); err != nil {
		return fmt.Errorf("start scheduler: %w", err)
	}
	go a.pusher.run(ctx)
	go a.forwarder.run(ctx)

	a.logger.InfoContext(ctx, "agent running",
		slog.String("node_id", a.identity.NodeID), slog.String("control_plane", a.cfg.ControlPlaneURL))

	<-ctx.Done()
	return a.Close(context.Background())
}

// Close shuts the pipeline down.
func (a *Agent) Close(ctx context.Context) error {
	if err := a.scheduler.Stop(ctx); err != nil {
		a.logger.ErrorContext(ctx, "scheduler stop failed", slog.String("error", err.Error()))
	}
	if err := a.plugins.Close(ctx); err != nil {
		a.logger.ErrorContext(ctx, "plugin close failed", slog.String("error", err.Error()))
	}
	_ = a.bus.Close()
	err := a.transport.Close()
	if a.p2pHost != nil {
		if closeErr := a.p2pHost.Close(); closeErr != nil {
			a.logger.ErrorContext(ctx, "libp2p host close failed", slog.String("error", closeErr.Error()))
		}
	}
	return err
}
