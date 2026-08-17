// Package libp2ptransport is the connect-by-identity POC transport described
// in docs/adr/0012-connect-by-identity.md: a libp2p Peer ID carries an Atlas
// [github.com/hexane/atlas/internal/core/transport.Transport] payload over an
// encrypted stream instead of a plain TCP connection.
//
// It deliberately does nothing beyond dialing and listening on one libp2p
// protocol. Everything else — the HTTP telemetry protocol, X.509 mTLS
// authorization, enrollment, spooling, retries — is unchanged; this package
// only supplies the [net.Conn] / [net.Listener] that carries it, so the
// existing http.Client and http.Server plumb straight through, per ADR-0012's
// "libp2p answers how to reach this peer, X.509 answers whether to trust it."
package libp2ptransport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	gostream "github.com/libp2p/go-libp2p-gostream"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	relayclient "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/client"
	relayv2 "github.com/libp2p/go-libp2p/p2p/protocol/circuitv2/relay"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
	"github.com/multiformats/go-multiaddr"
	"net"

	"github.com/hexane/atlas/internal/platform/errs"
)

// ProtocolID is the libp2p stream protocol Atlas telemetry travels on. One
// protocol carries the entire existing agent-facing HTTP surface (enroll,
// renew, telemetry) — a libp2p stream replaces the TCP connection underneath
// http.Server/http.Client, it does not replace the HTTP semantics riding on
// top.
const ProtocolID = protocolID

const protocolID = "/atlas/transport/1.0.0"

// identityFileName is where the Peer ID keypair is persisted, alongside but
// separate from the X.509 identity in the same data directory — see
// ADR-0012: routing identity and authorization identity are deliberately two
// keypairs, not one.
const identityFileName = "p2p-identity.key"

// LoadOrCreateIdentity returns this node's persistent libp2p identity,
// generating and saving one on first use so the Peer ID is stable across
// restarts — the entire point of connecting by identity is defeated if the
// identity changes every time the process restarts.
func LoadOrCreateIdentity(dataDir string) (crypto.PrivKey, error) {
	const op = "libp2ptransport.LoadOrCreateIdentity"
	path := filepath.Join(dataDir, identityFileName)

	if raw, err := os.ReadFile(path); err == nil {
		priv, err := crypto.UnmarshalPrivateKey(raw)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeInternal, "could not parse stored libp2p identity").WithOp(op)
		}
		return priv, nil
	} else if !os.IsNotExist(err) {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not read libp2p identity").WithOp(op)
	}

	priv, _, err := crypto.GenerateEd25519Key(nil)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not generate a libp2p identity").WithOp(op)
	}
	raw, err := crypto.MarshalPrivateKey(priv)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not marshal the libp2p identity").WithOp(op)
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not create data directory").WithOp(op)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not persist the libp2p identity").WithOp(op)
	}
	return priv, nil
}

// HostOptions configures [NewHost].
type HostOptions struct {
	// DataDir holds the persisted Peer ID keypair. Required.
	DataDir string
	// ListenAddrs are the multiaddrs this host accepts inbound streams on
	// (e.g. "/ip4/0.0.0.0/tcp/4102"). Empty means dial-only: the host can
	// still reach any peer outbound, it just never listens, which is exactly
	// what an agent behind NAT needs and what makes it require no forwarded
	// port. Only the side meant to be reachable (the POC's bootstrap target)
	// sets this.
	ListenAddrs []string
	// Logger receives connection-path and hole-punch observability events
	// (direct established/lost/re-established, relay fallback used, hole
	// punch attempted/succeeded/failed). Nil discards them.
	Logger *slog.Logger
}

// NewHost builds a libp2p host identified by this node's persistent Peer ID.
// DCUtR hole punching (github.com/libp2p/go-libp2p/p2p/protocol/holepunch)
// is enabled on every host, listener or dial-only: whichever side receives
// the inbound relayed connection is the one that initiates the punch, so
// both roles need it, not just the reachable side. No custom NAT-traversal
// protocol is implemented here — this only turns on and observes go-libp2p's
// own DCUtR.
func NewHost(opts HostOptions) (host.Host, error) {
	const op = "libp2ptransport.NewHost"
	if opts.DataDir == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "libp2p host requires a data directory").WithOp(op)
	}

	priv, err := LoadOrCreateIdentity(opts.DataDir)
	if err != nil {
		return nil, err
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	// EnableRelay is documented as on by default, but that default does not
	// reliably apply alongside our other options — verified by inspecting
	// the resulting host's registered stream protocols, which are missing
	// the relay STOP handler unless this is passed explicitly. Needed on
	// both ends: the dialing side to use a circuit address, the reachable
	// side to accept an inbound relayed connection at all. EnableHolePunching
	// requires it for the same reason DCUtR needs a relayed connection to
	// punch through. NATPortMap is a best-effort UPnP/NAT-PMP mapping for the
	// listening side; a no-op when there is nothing to map (dial-only hosts).
	libp2pOpts := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.EnableRelay(),
		libp2p.EnableHolePunching(holepunch.WithTracer(newHolePunchTracer(logger))),
		libp2p.NATPortMap(),
	}
	if len(opts.ListenAddrs) == 0 {
		libp2pOpts = append(libp2pOpts, libp2p.NoListenAddrs)
	} else {
		libp2pOpts = append(libp2pOpts, libp2p.ListenAddrStrings(opts.ListenAddrs...))
	}

	h, err := libp2p.New(libp2pOpts...)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not start libp2p host").WithOp(op)
	}
	h.Network().Notify(newConnPathNotifiee(logger))
	return h, nil
}

// ParseTarget parses a full peer multiaddr, e.g.
// "/ip4/203.0.113.5/tcp/4102/p2p/12D3Koo...", into the address/identity pair
// [Dial] connects to.
func ParseTarget(addr string) (peer.AddrInfo, error) {
	const op = "libp2ptransport.ParseTarget"
	ma, err := multiaddr.NewMultiaddr(addr)
	if err != nil {
		return peer.AddrInfo{}, errs.Wrap(err, errs.CodeInvalidArgument, "invalid libp2p multiaddr").WithOp(op)
	}
	info, err := peer.AddrInfoFromP2pAddr(ma)
	if err != nil {
		return peer.AddrInfo{}, errs.Wrap(err, errs.CodeInvalidArgument, "multiaddr has no peer id (missing /p2p/...)").WithOp(op)
	}
	return *info, nil
}

// Dial opens a stream to target on [ProtocolID] and returns it as a
// [net.Conn], so it can be handed to an unmodified crypto/tls client — the
// same Atlas mTLS handshake and certificate logic that runs over a plain TCP
// dial runs unchanged over this stream.
func Dial(ctx context.Context, h host.Host, target peer.AddrInfo) (net.Conn, error) {
	const op = "libp2ptransport.Dial"
	if len(target.Addrs) > 0 {
		if err := h.Connect(ctx, target); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not connect to peer").WithOp(op)
		}
	}
	conn, err := gostream.Dial(ctx, h, target.ID, protocolID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not open libp2p stream").WithOp(op)
	}
	return conn, nil
}

// Listen accepts inbound [ProtocolID] streams as a [net.Listener], so an
// unmodified http.Server (already TLS-configured for agent mTLS) can Serve
// on it exactly as it does on a TCP listener.
func Listen(h host.Host) (net.Listener, error) {
	const op = "libp2ptransport.Listen"
	l, err := gostream.Listen(h, protocolID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not listen on libp2p protocol").WithOp(op)
	}
	return l, nil
}

// PeerID returns h's Peer ID as a string, for logging and for building the
// multiaddr an operator hands to the other side.
func PeerID(h host.Host) string { return h.ID().String() }

// Addrs returns h's advertised listen multiaddrs including its Peer ID
// suffix, ready to hand to [ParseTarget] on the dialing side.
func Addrs(h host.Host) []string {
	out := make([]string, 0, len(h.Addrs()))
	for _, a := range h.Addrs() {
		out = append(out, fmt.Sprintf("%s/p2p/%s", a, h.ID()))
	}
	return out
}

// NewRelayHost builds a host running only the circuit-relay-v2 service: it
// forwards encrypted streams between peers it never authenticates or
// decrypts, and carries none of Atlas's own logic — see ADR-0012, "relay
// must sit below the encrypted Atlas stream, never terminate it."
//
// Per-connection relay limits are disabled. The default (2 minutes / 128KB)
// is sized for short-lived NAT-punch bootstrapping, not a kept-alive HTTP
// connection carrying telemetry; a self-hosted, single-tenant relay has no
// third party to protect from unbounded relayed traffic the way a public
// relay would.
func NewRelayHost(dataDir string, listenAddrs []string) (host.Host, error) {
	const op = "libp2ptransport.NewRelayHost"
	if len(listenAddrs) == 0 {
		return nil, errs.New(errs.CodeInvalidArgument, "relay host requires a reachable listen address").WithOp(op)
	}

	priv, err := LoadOrCreateIdentity(dataDir)
	if err != nil {
		return nil, err
	}

	h, err := libp2p.New(
		libp2p.Identity(priv),
		libp2p.ListenAddrStrings(listenAddrs...),
		// EnableRelayService only activates once AutoNAT decides the host is
		// publicly reachable, which needs other peers to cross-confirm it —
		// slow or silent-never in a small POC topology. The operator running
		// this as a relay already knows it is reachable; ForceReachabilityPublic
		// skips the probe instead of waiting on it.
		libp2p.ForceReachabilityPublic(),
		libp2p.EnableRelayService(relayv2.WithInfiniteLimits()),
	)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not start relay host").WithOp(op)
	}
	return h, nil
}

// rendezvousAnnounceProtocolID and rendezvousLookupProtocolID are the
// discovery protocols an Atlas Relay speaks in addition to circuit-relay-v2
// itself: a Server announces its current reachable addresses, and an Agent
// looks a Server's Peer ID up to get them, instead of an operator manually
// copy-pasting a circuit multiaddr between processes.
const (
	rendezvousAnnounceProtocolID = "/atlas/rendezvous/announce/1.0.0"
	rendezvousLookupProtocolID   = "/atlas/rendezvous/lookup/1.0.0"
)

// dialCandidateTimeout bounds how long [DialWithFallback] waits on each
// candidate before trying the next one, so an unreachable direct address
// fails fast into the relay fallback instead of hanging on the OS-level TCP
// connect timeout.
const dialCandidateTimeout = 8 * time.Second

type announceRequest struct {
	DirectAddrs []string `json:"direct_addrs"`
	CircuitAddr string   `json:"circuit_addr,omitempty"`
}

type announceResponse struct {
	OK bool `json:"ok"`
}

type lookupRequest struct {
	PeerID string `json:"peer_id"`
}

type lookupResponse struct {
	DirectAddrs []string `json:"direct_addrs,omitempty"`
	CircuitAddr string   `json:"circuit_addr,omitempty"`
	Found       bool     `json:"found"`
}

// LookupResult is what an Agent gets back from [Lookup]: the Server's
// currently-announced direct addresses and, if it has one, its relay circuit
// address — [DialWithFallback] tries the former first and the latter last.
type LookupResult struct {
	DirectAddrs []string
	CircuitAddr string
	Found       bool
}

// Announce tells relay the current set of addresses h can be reached at:
// its direct listen multiaddrs (may be empty, e.g. behind NAT) and its
// relay circuit address (may be empty, if h has not reserved one). relay
// authenticates the caller for free via the libp2p Noise handshake — the
// registry key is the stream's real remote Peer ID, not anything the
// request body claims, so one peer cannot overwrite another's record.
func Announce(ctx context.Context, h host.Host, relay peer.AddrInfo, directAddrs []string, circuitAddr string) error {
	const op = "libp2ptransport.Announce"
	if len(relay.Addrs) > 0 {
		if err := h.Connect(ctx, relay); err != nil {
			return errs.Wrap(err, errs.CodeUnavailable, "could not connect to relay").WithOp(op)
		}
	}
	s, err := h.NewStream(ctx, relay.ID, rendezvousAnnounceProtocolID)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not open rendezvous announce stream").WithOp(op)
	}
	defer s.Close()

	if err := json.NewEncoder(s).Encode(announceRequest{DirectAddrs: directAddrs, CircuitAddr: circuitAddr}); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not send announce request").WithOp(op)
	}
	if err := s.CloseWrite(); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not close announce write side").WithOp(op)
	}
	var resp announceResponse
	if err := json.NewDecoder(s).Decode(&resp); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not read announce response").WithOp(op)
	}
	if !resp.OK {
		return errs.New(errs.CodeInternal, "relay rejected announce").WithOp(op)
	}
	return nil
}

// Lookup asks relay for target's currently-announced addresses.
func Lookup(ctx context.Context, h host.Host, relay peer.AddrInfo, target peer.ID) (LookupResult, error) {
	const op = "libp2ptransport.Lookup"
	if len(relay.Addrs) > 0 {
		if err := h.Connect(ctx, relay); err != nil {
			return LookupResult{}, errs.Wrap(err, errs.CodeUnavailable, "could not connect to relay").WithOp(op)
		}
	}
	s, err := h.NewStream(ctx, relay.ID, rendezvousLookupProtocolID)
	if err != nil {
		return LookupResult{}, errs.Wrap(err, errs.CodeUnavailable, "could not open rendezvous lookup stream").WithOp(op)
	}
	defer s.Close()

	if err := json.NewEncoder(s).Encode(lookupRequest{PeerID: target.String()}); err != nil {
		return LookupResult{}, errs.Wrap(err, errs.CodeInternal, "could not send lookup request").WithOp(op)
	}
	if err := s.CloseWrite(); err != nil {
		return LookupResult{}, errs.Wrap(err, errs.CodeInternal, "could not close lookup write side").WithOp(op)
	}
	var resp lookupResponse
	if err := json.NewDecoder(s).Decode(&resp); err != nil {
		return LookupResult{}, errs.Wrap(err, errs.CodeUnavailable, "could not read lookup response").WithOp(op)
	}
	return LookupResult{DirectAddrs: resp.DirectAddrs, CircuitAddr: resp.CircuitAddr, Found: resp.Found}, nil
}

// Registry is an Atlas Relay's in-memory directory of announced Server
// addresses. It is deliberately not persisted: a relay restart loses it,
// and every Server re-populates it on its next reservation-renewal tick
// (well within minutes) — see ADR-0012, relay must hold none of Atlas's own
// state.
type Registry struct {
	mu      sync.RWMutex
	records map[peer.ID]registryRecord
}

type registryRecord struct {
	DirectAddrs []string
	CircuitAddr string
	UpdatedAt   time.Time
}

// NewRegistry returns an empty rendezvous registry.
func NewRegistry() *Registry {
	return &Registry{records: make(map[peer.ID]registryRecord)}
}

func (r *Registry) set(id peer.ID, directAddrs []string, circuitAddr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records[id] = registryRecord{DirectAddrs: directAddrs, CircuitAddr: circuitAddr, UpdatedAt: time.Now()}
}

func (r *Registry) get(id peer.ID) (registryRecord, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	rec, ok := r.records[id]
	return rec, ok
}

// RegisterRendezvousHandlers wires registry into h as the Announce/Lookup
// stream handlers — called once, on the relay host, alongside its existing
// circuit-relay-v2 service.
func RegisterRendezvousHandlers(h host.Host, registry *Registry) {
	h.SetStreamHandler(rendezvousAnnounceProtocolID, func(s network.Stream) {
		defer s.Close()
		var req announceRequest
		if err := json.NewDecoder(s).Decode(&req); err != nil {
			return
		}
		registry.set(s.Conn().RemotePeer(), req.DirectAddrs, req.CircuitAddr)
		_ = json.NewEncoder(s).Encode(announceResponse{OK: true})
	})
	h.SetStreamHandler(rendezvousLookupProtocolID, func(s network.Stream) {
		defer s.Close()
		var req lookupRequest
		if err := json.NewDecoder(s).Decode(&req); err != nil {
			return
		}
		id, err := peer.Decode(req.PeerID)
		if err != nil {
			_ = json.NewEncoder(s).Encode(lookupResponse{Found: false})
			return
		}
		rec, ok := registry.get(id)
		if !ok {
			_ = json.NewEncoder(s).Encode(lookupResponse{Found: false})
			return
		}
		_ = json.NewEncoder(s).Encode(lookupResponse{DirectAddrs: rec.DirectAddrs, CircuitAddr: rec.CircuitAddr, Found: true})
	})
}

// DialWithFallback tries each candidate in order — direct addresses first,
// a relay circuit address last, per the caller's construction of the slice
// — and returns the first successful connection. Each attempt is bounded by
// [dialCandidateTimeout] so an unreachable direct address fails fast into
// the relay fallback instead of hanging.
func DialWithFallback(ctx context.Context, h host.Host, candidates []peer.AddrInfo) (net.Conn, error) {
	const op = "libp2ptransport.DialWithFallback"
	if len(candidates) == 0 {
		return nil, errs.New(errs.CodeInvalidArgument, "no dial candidates").WithOp(op)
	}

	var attemptErrs []error
	for _, c := range candidates {
		dialCtx, cancel := context.WithTimeout(ctx, dialCandidateTimeout)
		conn, err := Dial(dialCtx, h, c)
		cancel()
		if err == nil {
			return conn, nil
		}
		attemptErrs = append(attemptErrs, fmt.Errorf("%s: %w", c.ID, err))
	}
	return nil, errs.Wrap(errors.Join(attemptErrs...), errs.CodeUnavailable, "all dial candidates failed").WithOp(op)
}

// ReserveRelay reserves a slot on relay for h — the relay's promise to
// forward circuit dials addressed to h's Peer ID — and returns the dialable
// circuit multiaddr other peers use to reach h through it, e.g.
// ".../p2p/<relay-id>/p2p-circuit/p2p/<h-id>". Reservations expire; callers
// behind NAT (no direct listen address) must call this again before
// Expiration to stay reachable.
//
// Relay-dialing capability itself needs no opt-in on h — [libp2p.New]
// enables it by default; only the relay side (see [NewRelayHost]) is opt-in.
func ReserveRelay(ctx context.Context, h host.Host, relay peer.AddrInfo) (addr string, expiration time.Time, err error) {
	const op = "libp2ptransport.ReserveRelay"
	if len(relay.Addrs) == 0 {
		return "", time.Time{}, errs.New(errs.CodeInvalidArgument, "relay address has no dialable multiaddr").WithOp(op)
	}
	if err := h.Connect(ctx, relay); err != nil {
		return "", time.Time{}, errs.Wrap(err, errs.CodeUnavailable, "could not connect to relay").WithOp(op)
	}
	res, err := relayclient.Reserve(ctx, h, relay)
	if err != nil {
		return "", time.Time{}, errs.Wrap(err, errs.CodeUnavailable, "could not reserve a relay slot").WithOp(op)
	}
	circuit := fmt.Sprintf("%s/p2p/%s/p2p-circuit/p2p/%s", relay.Addrs[0], relay.ID, h.ID())
	return circuit, res.Expiration, nil
}
