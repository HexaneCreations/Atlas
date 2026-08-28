package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/hexane/atlas/internal/core/transport"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr/net"
)

// observedIP extracts the plain IP address from a libp2p connection's remote
// multiaddr. It rejects a relayed (/p2p-circuit) address: only a direct
// connection carries an address that is actually this peer's — a circuit
// address's IP component is the relay's, not the agent's.
func observedIP(addr ma.Multiaddr) (string, bool) {
	if addr == nil {
		return "", false
	}
	if _, err := addr.ValueForProtocol(ma.P_CIRCUIT); err == nil {
		return "", false
	}
	ip, err := manet.ToIP(addr)
	if err != nil || ip == nil || ip.IsUnspecified() {
		return "", false
	}
	return ip.String(), true
}

// observePeerPublicIP records the source address a libp2p connection from an
// authorized agent peer was observed arriving at. Wired to the fleet host's
// network Connected notifiee, so it runs once per connection establishment.
//
// The Peer ID is proven by the Noise handshake; the node it maps to comes
// from the operator's agent_peers registration — neither is a claim from the
// agent. An unregistered peer (or the relay itself) resolves to nothing and
// is ignored.
func (f *fleetPipeline) observePeerPublicIP(pid peer.ID, addr ma.Multiaddr) {
	ip, ok := observedIP(addr)
	if !ok {
		return
	}

	f.mu.RLock()
	fleetRepo, metricRepo := f.fleetRepo, f.metricRepo
	f.mu.RUnlock()
	if fleetRepo == nil || metricRepo == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	id, err := fleetRepo.AuthorizedPeer(ctx, pid.String())
	if err != nil {
		return // unregistered peer, revoked binding, or the relay — nothing to attribute
	}

	// A connection from an authorized peer can precede that node's first
	// telemetry batch, so the row may not exist yet; UpdatePublicIP is a bare
	// UPDATE. Create the row here so the capture is not lost on a long-lived
	// connection that never triggers the notifiee again.
	env := transport.Envelope{Origin: transport.Origin{NodeID: id.NodeID, Environment: id.Environment}}
	if err := metricRepo.EnsureNode(ctx, env); err != nil {
		f.logger.WarnContext(ctx, "could not ensure node row for observed public ip",
			slog.String("node_id", id.NodeID), slog.String("error", err.Error()))
		return
	}
	if err := metricRepo.UpdatePublicIP(ctx, id.NodeID, ip); err != nil {
		f.logger.WarnContext(ctx, "could not record observed public ip",
			slog.String("node_id", id.NodeID), slog.String("error", err.Error()))
	}
}
