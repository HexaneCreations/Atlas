// Package relay is Atlas Relay: the minimal circuit-relay-v2 bootstrap
// component from docs/adr/0012-connect-by-identity.md that lets two peers
// behind NAT reach each other by Peer ID. It forwards encrypted streams
// between peers it never authenticates or decrypts, and holds none of
// Atlas's own logic — no Postgres, no HTTP API, no mTLS termination.
package relay

import (
	"context"
	"log/slog"

	"github.com/hexane/atlas/internal/core/transport/libp2ptransport"
	"github.com/libp2p/go-libp2p/core/host"
)

// Relay is a running Atlas Relay instance.
type Relay struct {
	host   host.Host
	logger *slog.Logger
}

// New starts the relay's libp2p host. It does not block; call Run for that.
func New(cfg Config, logger *slog.Logger) (*Relay, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	h, err := libp2ptransport.NewRelayHost(cfg.DataDir, cfg.ListenAddrs)
	if err != nil {
		return nil, err
	}

	// Rendezvous (docs/adr/0012-connect-by-identity.md): a Server announces
	// its current direct + circuit addresses here, so an Agent that only
	// knows the Server's Peer ID can look them up instead of an operator
	// manually assembling a circuit multiaddr. In-memory only — see
	// [libp2ptransport.Registry].
	libp2ptransport.RegisterRendezvousHandlers(h, libp2ptransport.NewRegistry())

	return &Relay{host: h, logger: logger}, nil
}

// Run logs this relay's Peer ID and dialable addresses — what an operator
// hands to Atlas Server as ATLAS_FLEET_LIBP2P_RELAY_ADDR — then blocks until
// ctx is cancelled.
func (r *Relay) Run(ctx context.Context) error {
	r.logger.InfoContext(ctx, "atlas relay ready",
		slog.String("peer_id", libp2ptransport.PeerID(r.host)),
		slog.Any("addrs", libp2ptransport.Addrs(r.host)))
	<-ctx.Done()
	return r.host.Close()
}
