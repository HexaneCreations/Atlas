// Package agent is the composition root for atlas-agent: it collects the
// local host and pushes observations to one or more control planes over
// mTLS. See docs/architecture/agent-design.md.
package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
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

// Agent collects the local host and pushes to one or more control planes —
// one physical node identity and one shared libp2p host, fanned out across
// every relationship it successfully bootstraps.
type Agent struct {
	cfg      Config
	logger   *slog.Logger
	identity hostid.Identity

	relationships map[string]*relationshipRuntime
	p2pHost       p2phost.Host

	fanout    *fanoutTransport
	bus       *eventbus.Bus
	scheduler *scheduler.Scheduler
	plugins   *plugin.Registry
	pusher    *inventoryPusher
	forwarder *eventForwarder
}

// relationshipRuntime is one fully bootstrapped Control-Plane relationship:
// its own trust (caCert), its own credential (holder), and its own delivery
// transport (with its own spool). Independent of every other relationship's
// runtime — nothing here is shared except the Agent's single node identity
// and single libp2p host, both passed in, never owned here. A failure,
// renewal, or shutdown of one relationship touches only its own fields.
type relationshipRuntime struct {
	id     string
	relCfg relationshipConfig

	caCert    *x509.Certificate
	holder    *credentialHolder
	transport *remote.Transport
	// peerID is the zero value if this relationship isn't on the libp2p
	// transport — it never participates in AgentOps or the shared host.
	peerID peer.ID

	cancelRenewal context.CancelFunc
}

// New resolves identity, bootstraps every configured relationship, and wires
// the (single, shared) collection pipeline to fan out across all of them. It
// does not start collecting; call Run for that.
//
// A relationship that fails to bootstrap is logged and dropped — it does not
// prevent any other relationship from starting. New fails only when zero
// relationships end up usable, which is exactly today's single-relationship
// behavior when that one relationship is the failure.
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

	bootstraps, err := discoverRelationships(cfg)
	if err != nil {
		return nil, fmt.Errorf("discover relationships: %w", err)
	}

	// Resolve each relationship's connection config (disk-or-env — no
	// network I/O yet) before deciding whether a shared libp2p host is
	// needed at all. A relationship whose config cannot be resolved — most
	// notably a corrupted relationship.json — must not prevent any other
	// relationship from starting: it is dropped here, exactly like a
	// bootstrap-time failure below, never treated as fatal to New as a
	// whole. It is also never allowed to fall back to bootstrap's
	// environment-sourced values — loadOrAdoptRelationshipConfig already
	// returns an error rather than doing that (see its own doc); this loop
	// only decides that error is per-relationship, not process-fatal.
	relConfigs := make(map[string]relationshipConfig, len(bootstraps))
	var resolveErrs []error
	for id, boot := range bootstraps {
		dataDir := dataDirFor(cfg.DataDir, id)
		relCfg, err := loadOrAdoptRelationshipConfig(id, dataDir, boot)
		if err != nil {
			logger.ErrorContext(ctx, "relationship configuration is unusable; it will not be available this run",
				slog.String("relationship", id), slog.String("error", err.Error()))
			resolveErrs = append(resolveErrs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		relConfigs[id] = relCfg
	}

	peerIDToRelationship, needsP2PHost, err := resolvePeerIDConflicts(relConfigs)
	if err != nil {
		return nil, err
	}

	var p2pHost p2phost.Host
	if needsP2PHost {
		h, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{DataDir: cfg.DataDir})
		if err != nil {
			return nil, fmt.Errorf("start libp2p host: %w", err)
		}
		p2pHost = h
	}

	relationships, bootErr := bootstrapAllRelationships(ctx, relConfigs, identity.NodeID, p2pHost, logger)
	if len(relationships) == 0 {
		if p2pHost != nil {
			_ = p2pHost.Close()
		}
		return nil, fmt.Errorf("no control-plane relationship could be bootstrapped: %w",
			errors.Join(append(resolveErrs, bootErr)...))
	}

	targets := make([]transport.Transport, 0, len(relationships))
	for _, rt := range relationships {
		targets = append(targets, rt.transport)
	}
	fanout := newFanoutTransport(targets)

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

	// Origin.Environment stays global across every relationship — a single
	// operator-assigned tag for this machine, not per-relationship (Phase 3
	// scope decision; see the approved plan).
	sched, err := scheduler.New(scheduler.Options{
		Registry:  collectors,
		Transport: fanout,
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
	pusher := newInventoryPusher(fanout, origin, cfg.InventoryInterval, logger,
		inventorySources(active, processPlugin, servicePlugin, cronPlugin, portsPlugin, systemPlugin, dockerPlugin))
	forwarder := newEventForwarder(bus, fanout, origin, logger)

	// AgentOps (remote container log streaming): one handler on the shared
	// host, demultiplexed per stream by the remote libp2p peer identity — see
	// libp2ptransport.AgentOpsRelationshipLookup's doc. Only relationships
	// that both resolved to a known peer id above AND survived bootstrap can
	// ever serve a session; every other peer is rejected outright, the same
	// posture as today's single-relationship silent drop.
	if p2pHost != nil {
		limiter := libp2ptransport.NewSessionLimiter(libp2ptransport.DefaultMaxConcurrentSessions)
		lookup := buildAgentOpsLookup(relationships, peerIDToRelationship)
		libp2ptransport.RegisterAgentOpsHandler(p2pHost, containerLogsFunc(dockerPlugin), limiter, lookup)
	}

	return &Agent{
		cfg: cfg, logger: logger, identity: identity,
		relationships: relationships, p2pHost: p2pHost,
		fanout: fanout, bus: bus,
		scheduler: sched, plugins: plugins, pusher: pusher, forwarder: forwarder,
	}, nil
}

// bootstrapAllRelationships bootstraps every relationship concurrently — up
// to bootstrapMaxElapsed per relationship, not multiplied across
// relationships — and returns the ones that succeeded. bootErr aggregates
// every failure, for the caller to report if zero relationships survive.
func bootstrapAllRelationships(ctx context.Context, relConfigs map[string]relationshipConfig, nodeID string, p2pHost p2phost.Host, logger *slog.Logger) (map[string]*relationshipRuntime, error) {
	type result struct {
		id  string
		rt  *relationshipRuntime
		err error
	}

	results := make(chan result, len(relConfigs))
	var wg sync.WaitGroup
	for id, relCfg := range relConfigs {
		wg.Add(1)
		go func(id string, relCfg relationshipConfig) {
			defer wg.Done()
			rt, err := bootstrapRelationship(ctx, relCfg, nodeID, p2pHost, logger)
			results <- result{id: id, rt: rt, err: err}
		}(id, relCfg)
	}
	wg.Wait()
	close(results)

	relationships := map[string]*relationshipRuntime{}
	var bootErrs []error
	for r := range results {
		if r.err != nil {
			logger.ErrorContext(ctx, "relationship failed to bootstrap; it will not be available this run",
				slog.String("relationship", r.id), slog.String("error", r.err.Error()))
			bootErrs = append(bootErrs, fmt.Errorf("%s: %w", r.id, r.err))
			continue
		}
		relationships[r.id] = r.rt
	}
	return relationships, errors.Join(bootErrs...)
}

// bootstrapRelationship wires one relationship end-to-end: dial path (if
// libp2p), credential bootstrap (enroll-or-load, with the existing bounded
// retry), its renewal loop, and its own delivery transport with its own
// spool. Fully independent of every other relationship.
func bootstrapRelationship(ctx context.Context, relCfg relationshipConfig, nodeID string, p2pHost p2phost.Host, logger *slog.Logger) (*relationshipRuntime, error) {
	if err := os.MkdirAll(relCfg.dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}

	var dial dialContextFunc
	var peerID peer.ID

	if relCfg.Transport == "libp2p" {
		if p2pHost == nil {
			return nil, fmt.Errorf("libp2p transport requested but no libp2p host is available")
		}
		switch {
		case relCfg.LibP2PRelayAddr != "" && relCfg.LibP2PServerPeerID != "":
			relayInfo, err := libp2ptransport.ParseTarget(relCfg.LibP2PRelayAddr)
			if err != nil {
				return nil, fmt.Errorf("parse libp2p relay address: %w", err)
			}
			serverID, err := peer.Decode(relCfg.LibP2PServerPeerID)
			if err != nil {
				return nil, fmt.Errorf("parse libp2p server peer id: %w", err)
			}
			dial = newDiscoveryDial(p2pHost, relayInfo, serverID, relCfg.dataDir, logger)
			peerID = serverID
			logger.InfoContext(ctx, "dialing control plane via rendezvous discovery",
				slog.String("relationship", relCfg.id),
				slog.String("peer_id", serverID.String()), slog.String("relay", relayInfo.ID.String()))
		case relCfg.LibP2PServerAddr != "":
			target, err := libp2ptransport.ParseTarget(relCfg.LibP2PServerAddr)
			if err != nil {
				return nil, fmt.Errorf("parse libp2p server address: %w", err)
			}
			dial = func(ctx context.Context, _, _ string) (net.Conn, error) {
				return libp2ptransport.Dial(ctx, p2pHost, target)
			}
			peerID = target.ID
			logger.WarnContext(ctx, "dialing control plane by static multiaddr; "+
				"set a relay address and server peer id instead",
				slog.String("relationship", relCfg.id), slog.String("peer_id", target.ID.String()))
		default:
			return nil, fmt.Errorf("libp2p transport requires either (relay address and server peer id) or the deprecated server address")
		}
	}

	caCert, holder, err := bootstrapWithRetry(ctx, relCfg, nodeID, logger, dial)
	if err != nil {
		return nil, fmt.Errorf("bootstrap credentials: %w", err)
	}

	renewCtx, cancel := context.WithCancel(ctx)
	go renewalLoop(renewCtx, relCfg, nodeID, caCert, holder, logger, dial)

	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, GetClientCertificate: holder.GetClientCertificate}

	sp, err := spool.Open(spool.Options{Dir: filepath.Join(relCfg.dataDir, "spool")})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("open spool: %w", err)
	}

	var httpClient *http.Client
	if dial != nil {
		httpClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, DialContext: dial}}
	}
	tr, err := remote.New(remote.Options{
		BaseURL: relCfg.ControlPlaneURL, TLSConfig: tlsCfg, HTTPClient: httpClient, Spool: sp, Logger: logger,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("build remote transport: %w", err)
	}

	return &relationshipRuntime{
		id: relCfg.id, relCfg: relCfg,
		caCert: caCert, holder: holder, transport: tr, peerID: peerID,
		cancelRenewal: cancel,
	}, nil
}

// resolvePeerIDConflicts determines, without dialing anything, which
// relationships target the libp2p transport and whether any two of them
// resolve to the same server peer id. Duplicate libp2p Peer IDs across
// relationships are a configuration/security error, not a warning: allowing
// it would mean one relationship's AgentOps demux entry silently wins over
// another's — exactly the "relationship X authenticates as relationship Y"
// failure mode this phase exists to prevent, so this fails outright rather
// than silently dropping one side. A relationship whose target simply
// doesn't parse is left out of the returned map and fails later,
// per-relationship, in bootstrapRelationship — an ordinary bootstrap
// failure, not a cross-relationship conflict.
func resolvePeerIDConflicts(relConfigs map[string]relationshipConfig) (peerIDToRelationship map[peer.ID]string, needsP2PHost bool, err error) {
	peerIDToRelationship = map[peer.ID]string{}
	for id, relCfg := range relConfigs {
		if relCfg.Transport != "libp2p" {
			continue
		}
		needsP2PHost = true
		pid, ok := resolveLibP2PServerPeerID(relCfg)
		if !ok {
			continue
		}
		if existing, dup := peerIDToRelationship[pid]; dup {
			return nil, false, fmt.Errorf(
				"relationships %q and %q both resolve to libp2p peer id %s; "+
					"each relationship must target a distinct control plane", existing, id, pid)
		}
		peerIDToRelationship[pid] = id
	}
	return peerIDToRelationship, needsP2PHost, nil
}

// resolveLibP2PServerPeerID determines a relationship's target libp2p peer
// id without dialing anything — from the rendezvous pair if set, else the
// deprecated static address. ok is false only when neither form parses;
// that is an ordinary per-relationship bootstrap failure (caught later in
// bootstrapRelationship), not the cross-relationship conflict this function
// exists to help detect in New.
func resolveLibP2PServerPeerID(relCfg relationshipConfig) (peer.ID, bool) {
	if relCfg.LibP2PRelayAddr != "" && relCfg.LibP2PServerPeerID != "" {
		pid, err := peer.Decode(relCfg.LibP2PServerPeerID)
		if err != nil {
			return "", false
		}
		return pid, true
	}
	if relCfg.LibP2PServerAddr != "" {
		target, err := libp2ptransport.ParseTarget(relCfg.LibP2PServerAddr)
		if err != nil {
			return "", false
		}
		return target.ID, true
	}
	return "", false
}

// buildAgentOpsLookup builds the Agent-side half of the AgentOps demux
// described in libp2ptransport.AgentOpsRelationshipLookup's doc. Unlike the
// control plane (which learns an Agent's peer id dynamically, from traffic
// it receives), the Agent already knows each libp2p relationship's target
// peer id statically from its own resolved config — so this map is built
// once, here, from data already validated duplicate-free in New, and never
// mutated afterward. No locking needed.
func buildAgentOpsLookup(relationships map[string]*relationshipRuntime, peerIDToRelationship map[peer.ID]string) libp2ptransport.AgentOpsRelationshipLookup {
	return func(remotePeer peer.ID) (*x509.Certificate, func(*tls.ClientHelloInfo) (*tls.Certificate, error), bool, bool) {
		id, ok := peerIDToRelationship[remotePeer]
		if !ok {
			return nil, nil, false, false
		}
		rt, ok := relationships[id]
		if !ok {
			// This relationship's peer id was known statically, but it did
			// not survive bootstrap (see New) — no live credential exists to
			// present, so this stream cannot be served.
			return nil, nil, false, false
		}
		holder := rt.holder
		getCert := func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return holder.GetClientCertificate(nil) }
		return rt.caCert, getCert, !rt.relCfg.AgentOpsContainerLogsDisabled, true
	}
}

// Run starts collection and blocks until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	if err := a.scheduler.Start(ctx); err != nil {
		return fmt.Errorf("start scheduler: %w", err)
	}
	go a.pusher.run(ctx)
	go a.forwarder.run(ctx)

	targets := make([]string, 0, len(a.relationships))
	for id, rt := range a.relationships {
		targets = append(targets, id+"="+rt.relCfg.ControlPlaneURL)
	}
	a.logger.InfoContext(ctx, "agent running",
		slog.String("node_id", a.identity.NodeID), slog.Any("relationships", targets))

	<-ctx.Done()
	return a.Close(context.Background())
}

// Close shuts the pipeline down. Every relationship's renewal loop is
// cancelled before its transport is closed, so a renewal in flight never
// races a transport shutdown; every relationship is still closed regardless
// of another's error.
func (a *Agent) Close(ctx context.Context) error {
	if err := a.scheduler.Stop(ctx); err != nil {
		a.logger.ErrorContext(ctx, "scheduler stop failed", slog.String("error", err.Error()))
	}
	if err := a.plugins.Close(ctx); err != nil {
		a.logger.ErrorContext(ctx, "plugin close failed", slog.String("error", err.Error()))
	}
	_ = a.bus.Close()

	for _, rt := range a.relationships {
		rt.cancelRenewal()
	}
	err := a.fanout.Close()

	if a.p2pHost != nil {
		if closeErr := a.p2pHost.Close(); closeErr != nil {
			a.logger.ErrorContext(ctx, "libp2p host close failed", slog.String("error", closeErr.Error()))
		}
	}
	return err
}
