package app

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	agentapi "github.com/hexane/atlas/internal/api/agent"
	coreeventstore "github.com/hexane/atlas/internal/core/eventstore"
	corefleet "github.com/hexane/atlas/internal/core/fleet"
	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/hexane/atlas/internal/platform/config"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/pki"
	"github.com/hexane/atlas/internal/platform/postgres"
	"github.com/hexane/atlas/internal/plugin/docker"
	storageeventstore "github.com/hexane/atlas/internal/storage/eventstore"
	storagefleet "github.com/hexane/atlas/internal/storage/fleet"
	storageinventory "github.com/hexane/atlas/internal/storage/inventory"
	"github.com/hexane/atlas/internal/storage/metric"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
)

const envelopeRetention = 48 * time.Hour

// relayReservationRenewInterval is well under the relay's default
// reservation TTL (1 hour — see circuitv2/relay.DefaultResources), so a
// missed renewal or two before the next tick still leaves margin before the
// reservation actually lapses.
const relayReservationRenewInterval = 20 * time.Minute

// fleetPipeline is the agent-facing mTLS listener: enrollment, renewal, and
// telemetry ingest. Inert when cfg.Fleet.Enabled is false.
type fleetPipeline struct {
	cfg        *config.Config
	logger     *slog.Logger
	pool       *postgres.Pool
	collection *collectionPipeline
	onEvent    func(context.Context, coreeventstore.Record)

	mu           sync.RWMutex
	server       *httpx.TLSServer
	libp2pServer *httpx.TLSServer
	libp2pHost   host.Host
	relayAddr    string // circuit multiaddr, set only when Fleet.LibP2PRelayAddr is configured
	// caCert and serverLeaf back the reversed mTLS handshake AgentOps uses
	// (see libp2ptransport.RequestContainerLogs) — the same CA and
	// certificate already minted for the agent-facing listener, not a
	// separate trust root.
	caCert     *x509.Certificate
	serverLeaf tls.Certificate
	// peerByNode maps a NodeID to the libp2p Peer ID it was last seen
	// dialing in from — in-memory only, populated as a side effect of the
	// Agent's own existing enroll/renew/telemetry traffic (see
	// recordAgentPeer), the same "no new state, no persistence" posture as
	// the Relay's rendezvous registry. A stale or missing entry surfaces
	// naturally as a failed stream open, not as a separate liveness check.
	peerByNode map[string]peer.ID
	// grants is the CP-side AgentOps authorization check — see ContainerLogs.
	// Set once at Start alongside caCert/serverLeaf.
	grants corefleet.GrantStore
	stopCh chan struct{}
	wg     sync.WaitGroup
}

func newFleetPipeline(cfg *config.Config, logger *slog.Logger, pool *postgres.Pool, collection *collectionPipeline,
	onEvent func(context.Context, coreeventstore.Record)) *fleetPipeline {
	return &fleetPipeline{cfg: cfg, logger: logger, pool: pool, collection: collection, onEvent: onEvent}
}

func (f *fleetPipeline) Name() string { return "fleet.pipeline" }

func (f *fleetPipeline) Start(ctx context.Context) error {
	if !f.cfg.Fleet.Enabled {
		return nil
	}

	ca, err := pki.LoadOrCreateCA(f.cfg.Fleet.DataDir, "atlas-control-plane")
	if err != nil {
		return err
	}
	serverLeaf, err := pki.NewServerLeaf(ca, f.cfg.Fleet.AdvertisedHosts)
	if err != nil {
		return err
	}

	fleetRepo := storagefleet.NewRepository(f.pool.DB())
	invRepo := storageinventory.NewRepository(f.pool.DB())
	metricRepo := f.collection.Repository()
	enroller := corefleet.NewEnroller(ca, fleetRepo, fleetRepo, fleetRepo, fleetRepo)

	eventStore := coreeventstore.Store(storageeventstore.NewRepository(f.pool.DB()))
	if f.onEvent != nil {
		eventStore = coreeventstore.Tap(eventStore, f.onEvent)
	}

	router := transport.NewRouter()
	if err := router.Register(metric.NewSink(metricRepo, f.logger)); err != nil {
		return err
	}
	if err := router.Register(storageinventory.NewReceiver(invRepo)); err != nil {
		return err
	}
	if err := router.Register(storageeventstore.NewReceiver(eventStore)); err != nil {
		return err
	}

	handler := agentapi.NewHandler(agentapi.Deps{
		CA: ca, Enroller: enroller, Denylist: fleetRepo, Router: router,
		ClockSkew: metricRepo, Logger: f.logger,
	})
	mux := http.NewServeMux()
	handler.Mount(mux)

	agentHandler := httpx.AgentMiddleware(f.cfg.Fleet)(mux)

	tlsConfig := pki.ServerTLSConfig(serverLeaf, ca)
	server := httpx.NewTLSServer("fleet.server", f.cfg.Fleet.Addr(), agentHandler, tlsConfig, f.logger)
	if err := server.Start(ctx); err != nil {
		return err
	}

	f.mu.Lock()
	f.server = server
	f.stopCh = make(chan struct{})
	f.caCert = ca.Cert()
	f.serverLeaf = serverLeaf
	f.peerByNode = make(map[string]peer.ID)
	f.grants = fleetRepo
	f.mu.Unlock()

	// libp2p is a second, POC listener for the same mux — see
	// docs/adr/0012-connect-by-identity.md. It carries the identical
	// enrollment/renewal/telemetry HTTP surface, just reached by Peer ID
	// instead of a dialable address, so an operator behind NAT needs no
	// forwarded port for this listener to exist.
	if f.cfg.Fleet.LibP2PEnabled {
		p2pHost, err := libp2ptransport.NewHost(libp2ptransport.HostOptions{
			DataDir: f.cfg.Fleet.DataDir, ListenAddrs: f.cfg.Fleet.LibP2PListenAddrs, Logger: f.logger,
		})
		if err != nil {
			return err
		}
		listener, err := libp2ptransport.Listen(p2pHost)
		if err != nil {
			_ = p2pHost.Close()
			return err
		}
		// recordAgentPeer wraps only this listener's mux, not the shared one
		// the HTTPS listener also serves — an r.RemoteAddr from a plain TCP
		// connection is an IP, not a Peer ID, so recording it would be
		// meaningless (and recordAgentPeer's own peer.Decode guards against
		// treating it as one regardless).
		libp2pServer := httpx.NewTLSServerFromListener("fleet.server.libp2p", listener, httpx.AgentMiddleware(f.cfg.Fleet)(f.recordAgentPeer(mux)), tlsConfig, f.logger)
		if err := libp2pServer.Start(ctx); err != nil {
			_ = p2pHost.Close()
			return err
		}

		f.mu.Lock()
		f.libp2pHost = p2pHost
		f.libp2pServer = libp2pServer
		f.mu.Unlock()

		f.logger.InfoContext(ctx, "fleet libp2p listener ready",
			slog.String("peer_id", libp2ptransport.PeerID(p2pHost)),
			slog.Any("addrs", libp2ptransport.Addrs(p2pHost)))

		// Atlas Relay (docs/adr/0012-connect-by-identity.md): when the
		// control plane itself is behind NAT, LibP2PListenAddrs is not
		// reachable from outside. Reserving a slot on a relay gives it a
		// dialable circuit address instead — the relay only ever forwards
		// already-encrypted bytes, it never terminates this stream.
		if f.cfg.Fleet.LibP2PRelayAddr != "" {
			relayInfo, err := libp2ptransport.ParseTarget(f.cfg.Fleet.LibP2PRelayAddr)
			if err != nil {
				return err
			}
			if err := f.reserveRelay(ctx, p2pHost, relayInfo); err != nil {
				return err
			}
			f.wg.Add(1)
			go f.relayRenewalLoop(p2pHost, relayInfo)
		}
	}

	f.wg.Add(1)
	go f.pruneEnvelopesLoop(fleetRepo)

	f.logger.InfoContext(ctx, "fleet listener ready", slog.String("addr", server.Addr()))
	return nil
}

// LibP2PPeerAddrs returns the fleet listener's dialable libp2p multiaddrs
// (each including its Peer ID), or nil when libp2p is disabled or not yet
// started. When a relay is configured this is the circuit address through
// it, not the (likely unreachable) direct one. Operators read the
// equivalent from the startup log; tests use this to point a real agent at
// a real, freshly bound listener.
func (f *fleetPipeline) LibP2PPeerAddrs() []string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.relayAddr != "" {
		return []string{f.relayAddr}
	}
	if f.libp2pHost == nil {
		return nil
	}
	return libp2ptransport.Addrs(f.libp2pHost)
}

// recordAgentPeer wraps next with a side effect: for every request that
// arrives with a verified mTLS client certificate, it records which libp2p
// Peer ID it came in on, keyed by the certificate's NodeID.
//
// Nothing about this needs a new mechanism. [pki.PeerNodeID] is the same
// authenticated-identity extraction every agent-facing handler already
// performs; r.RemoteAddr, for a request that arrived over the libp2p
// listener, is already the peer's libp2p Peer ID as a string — verified
// directly against the go-libp2p-gostream source: gostream's net.Conn wraps
// a network.Stream and its RemoteAddr().String() returns
// stream.Conn().RemotePeer().String(), and Go's net/http copies
// conn.RemoteAddr() into every Request.RemoteAddr automatically. No wire
// format change, no Agent-side change, populated as a side effect of traffic
// the Agent already sends (enroll, renew, telemetry).
//
// The map is never proactively pruned on disconnect: a stale entry simply
// fails the next AgentOps stream open (no live connection to reuse, and the
// Agent has no independently dialable address — see ADR-0012), which is
// exactly the "no active session" signal RemoteLogSource's caller needs.
// Actively tracking connect/disconnect events would only make that failure
// arrive slightly sooner, at the cost of a second bookkeeping mechanism.
func (f *fleetPipeline) recordAgentPeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
			if nodeID, err := pki.PeerNodeID(r.TLS.PeerCertificates[0]); err == nil {
				if pid, err := peer.Decode(r.RemoteAddr); err == nil {
					f.mu.Lock()
					f.peerByNode[nodeID] = pid
					f.mu.Unlock()
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ContainerLogs implements v1.RemoteLogSource: it proxies a live log session
// to nodeID's Agent over AgentOps, using whichever Peer ID recordAgentPeer
// last saw that node dial in from. Returns the same two-channel shape
// docker.Client.Logs uses locally, so internal/api/v1/containers.go forwards
// a local and a remote session through the identical loop.
func (f *fleetPipeline) ContainerLogs(ctx context.Context, nodeID, containerID string, opts docker.LogOptions) (<-chan docker.LogLine, <-chan error) {
	const op = "app.fleetPipeline.ContainerLogs"

	lines := make(chan docker.LogLine)
	errCh := make(chan error, 1)

	f.mu.RLock()
	grants := f.grants
	pid, known := f.peerByNode[nodeID]
	p2pHost := f.libp2pHost
	caCert := f.caCert
	serverLeaf := f.serverLeaf
	f.mu.RUnlock()

	// Authorization is explicit and independent of authentication: a live,
	// non-denylisted certificate is never by itself sufficient to invoke a
	// privileged operation (see docs/architecture/security.md).
	granted, err := grants.IsGranted(ctx, nodeID, corefleet.OperationContainerLogs)
	if err != nil {
		close(lines)
		errCh <- errs.Wrap(err, errs.CodeUnavailable, "could not check agent operation authorization").WithOp(op).WithDetail("node", nodeID)
		close(errCh)
		return lines, errCh
	}
	if !granted {
		close(lines)
		errCh <- errs.New(errs.CodePermissionDenied, "this node is not authorized for container log streaming").
			WithOp(op).WithDetail("node", nodeID)
		close(errCh)
		return lines, errCh
	}

	if !known || p2pHost == nil {
		close(lines)
		errCh <- errs.New(errs.CodeUnavailable,
			"no active libp2p session for this agent; remote logs are unavailable").
			WithOp(op).WithDetail("node", nodeID)
		close(errCh)
		return lines, errCh
	}

	req := libp2ptransport.AgentOpRequest{
		ProtocolVersion: libp2ptransport.AgentOpsProtocolVersion,
		Op:              libp2ptransport.AgentOpContainerLogs,
		ContainerID:     containerID,
		Tail:            opts.Tail,
		Follow:          opts.Follow,
		Timestamps:      opts.Timestamps,
	}
	if !opts.Since.IsZero() {
		req.Since = opts.Since.Format(time.RFC3339)
	}

	frames, err := libp2ptransport.RequestContainerLogs(ctx, p2pHost, pid, nodeID, caCert, serverLeaf, req)
	if err != nil {
		close(lines)
		errCh <- err
		close(errCh)
		return lines, errCh
	}

	go func() {
		defer close(lines)
		defer close(errCh)
		for frame := range frames {
			switch frame.Type {
			case "line":
				select {
				case lines <- docker.LogLine{Time: frame.Time, Stream: docker.Stream(frame.Stream), Message: frame.Message}:
				case <-ctx.Done():
					return
				}
			case "error":
				errCh <- fmt.Errorf("%s", frame.Reason)
				return
			default: // "end"
				return
			}
		}
	}()

	return lines, errCh
}

// reserveRelay reserves a slot on relay for p2pHost, records the resulting
// circuit address, and announces both it and p2pHost's direct addresses to
// relay's rendezvous registry — the discovery path an Agent uses instead of
// being handed a manually-assembled circuit multiaddr (ADR-0012).
func (f *fleetPipeline) reserveRelay(ctx context.Context, p2pHost host.Host, relayInfo peer.AddrInfo) error {
	circuit, expiration, err := libp2ptransport.ReserveRelay(ctx, p2pHost, relayInfo)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.relayAddr = circuit
	f.mu.Unlock()
	f.logger.InfoContext(ctx, "reserved a relay slot",
		slog.String("circuit_addr", circuit), slog.Time("expires", expiration))

	if err := libp2ptransport.Announce(ctx, p2pHost, relayInfo, libp2ptransport.Addrs(p2pHost), circuit); err != nil {
		// Non-fatal: the circuit reservation above still works for any Agent
		// dialing it directly; only rendezvous-based discovery is degraded
		// until the next renewal tick retries the announce.
		f.logger.ErrorContext(ctx, "rendezvous announce failed", slog.String("error", err.Error()))
	}
	return nil
}

// relayRenewalLoop re-reserves the relay slot before it expires. A failed
// renewal is logged and retried next tick; the previous reservation (and
// f.relayAddr) is left in place until it actually lapses on the relay side.
func (f *fleetPipeline) relayRenewalLoop(p2pHost host.Host, relayInfo peer.AddrInfo) {
	defer f.wg.Done()
	ticker := time.NewTicker(relayReservationRenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := f.reserveRelay(ctx, p2pHost, relayInfo); err != nil {
				f.logger.ErrorContext(ctx, "relay reservation renewal failed", slog.String("error", err.Error()))
			}
			cancel()
		}
	}
}

func (f *fleetPipeline) Stop(ctx context.Context) error {
	f.mu.RLock()
	server, stopCh := f.server, f.stopCh
	libp2pServer, p2pHost := f.libp2pServer, f.libp2pHost
	f.mu.RUnlock()
	if server == nil {
		return nil
	}
	close(stopCh)
	f.wg.Wait()

	if libp2pServer != nil {
		if err := libp2pServer.Stop(ctx); err != nil {
			f.logger.ErrorContext(ctx, "libp2p listener stop failed", slog.String("error", err.Error()))
		}
	}
	if p2pHost != nil {
		if err := p2pHost.Close(); err != nil {
			f.logger.ErrorContext(ctx, "libp2p host close failed", slog.String("error", err.Error()))
		}
	}
	return server.Stop(ctx)
}

func (f *fleetPipeline) pruneEnvelopesLoop(repo *storagefleet.Repository) {
	defer f.wg.Done()
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := repo.PruneIngestedEnvelopes(ctx, time.Now().Add(-envelopeRetention)); err != nil {
				f.logger.ErrorContext(ctx, "could not prune ingested_envelopes", slog.String("error", err.Error()))
			}
			cancel()
		}
	}
}

// lazyInventoryStore defers repository construction to call time, since the
// underlying pool is not connected until [postgres.Pool.Start] runs, which
// happens after api.New builds the router that holds this value.
type lazyInventoryStore struct{ pool *postgres.Pool }

func (l lazyInventoryStore) Put(ctx context.Context, s coreinventory.StoredSnapshot) error {
	return storageinventory.NewRepository(l.pool.DB()).Put(ctx, s)
}

func (l lazyInventoryStore) Get(ctx context.Context, nodeID string, subject coreinventory.Subject) (coreinventory.StoredSnapshot, error) {
	return storageinventory.NewRepository(l.pool.DB()).Get(ctx, nodeID, subject)
}

func (l lazyInventoryStore) HasReported(ctx context.Context, nodeID string) (bool, error) {
	return storageinventory.NewRepository(l.pool.DB()).HasReported(ctx, nodeID)
}

// lazyEventStore defers repository construction to call time, for the same
// reason as [lazyInventoryStore].
type lazyEventStore struct{ pool *postgres.Pool }

func (l lazyEventStore) Query(ctx context.Context, filter coreeventstore.Filter) ([]coreeventstore.Record, error) {
	return storageeventstore.NewRepository(l.pool.DB()).Query(ctx, filter)
}

// nodeLookup adapts the collection pipeline's repository accessor into
// [v1.NodeExistence], deferring resolution to call time for the same reason
// as [lazyInventoryStore].
type nodeLookup struct{ pipeline *collectionPipeline }

func (n nodeLookup) GetNode(ctx context.Context, nodeID string) (metric.Node, error) {
	repo := n.pipeline.Repository()
	if repo == nil {
		return metric.Node{}, errs.New(errs.CodeUnavailable, "collection is not ready").WithOp("app.nodeLookup.GetNode")
	}
	return repo.GetNode(ctx, nodeID)
}
