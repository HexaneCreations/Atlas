package libp2ptransport

import (
	"log/slog"
	"sync"

	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/protocol/holepunch"
	ma "github.com/multiformats/go-multiaddr"
)

// isRelayedConn reports whether c is a circuit-v2 connection. network.ConnStats.Limited
// is set by the relay only when it grants a resource-limited reservation; Atlas's own
// relay (see NewRelayHost's relayv2.WithInfiniteLimits) deliberately grants unlimited
// ones, so Limited alone under-reports — checked and confirmed by test against a real
// relay. The multiaddr /p2p-circuit component (go-libp2p's own swarm_dial.go isRelayAddr
// uses the same check) is unaffected by that config and is authoritative either way.
func isRelayedConn(c network.Conn) bool {
	if c.Stat().Limited {
		return true
	}
	_, err := c.RemoteMultiaddr().ValueForProtocol(ma.P_CIRCUIT)
	return err == nil
}

// newConnPathNotifiee logs each connection to a peer as direct or
// relay-fallback as it opens and closes, so an operator can see from the log
// alone whether Atlas traffic is actually on a direct path, never inferring it.
func newConnPathNotifiee(logger *slog.Logger) network.Notifiee {
	var mu sync.Mutex
	seenDirect := map[peer.ID]bool{}

	return &network.NotifyBundle{
		ConnectedF: func(_ network.Network, c network.Conn) {
			p := c.RemotePeer()
			if isRelayedConn(c) {
				logger.Info("libp2p relay fallback used", "peer", p.String())
				return
			}
			mu.Lock()
			again := seenDirect[p]
			seenDirect[p] = true
			mu.Unlock()
			if again {
				logger.Info("libp2p direct connection re-established", "peer", p.String())
			} else {
				logger.Info("libp2p direct connection established", "peer", p.String())
			}
		},
		DisconnectedF: func(_ network.Network, c network.Conn) {
			if !isRelayedConn(c) {
				logger.Warn("libp2p direct connection lost", "peer", c.RemotePeer().String())
			}
		},
	}
}

// holePunchTracer relays go-libp2p's own DCUtR events to the agent/server
// log. No custom hole-punch signaling exists in Atlas — this only observes
// github.com/libp2p/go-libp2p/p2p/protocol/holepunch's existing EventTracer hook.
type holePunchTracer struct{ logger *slog.Logger }

func newHolePunchTracer(logger *slog.Logger) holepunch.EventTracer {
	return &holePunchTracer{logger: logger}
}

func (t *holePunchTracer) Trace(evt *holepunch.Event) {
	p := evt.Remote.String()
	switch evt.Type {
	case holepunch.StartHolePunchEvtT:
		t.logger.Info("libp2p hole punch attempted", "peer", p)
	case holepunch.EndHolePunchEvtT:
		if e, ok := evt.Evt.(*holepunch.EndHolePunchEvt); ok {
			if e.Success {
				t.logger.Info("libp2p hole punch succeeded", "peer", p, "elapsed", e.EllapsedTime)
			} else {
				t.logger.Warn("libp2p hole punch failed", "peer", p, "error", e.Error)
			}
		}
	case holepunch.ProtocolErrorEvtT:
		if e, ok := evt.Evt.(*holepunch.ProtocolErrorEvt); ok {
			t.logger.Warn("libp2p hole punch protocol error", "peer", p, "error", e.Error)
		}
	}
}

// PreferDirectConnection runs onDirect whenever h establishes a non-relayed
// connection to target. DCUtR does not migrate already-open streams onto a
// newly hole-punched connection (see EnableHolePunching's doc) — a caller
// pooling connections, e.g. net/http's keep-alive, must react to this and
// drop its idle ones itself; only then does the next request's dial see the
// direct connection that Swarm.bestConnToPeer already prefers over a relayed
// one.
func PreferDirectConnection(h host.Host, target peer.ID, onDirect func()) {
	h.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, c network.Conn) {
			if c.RemotePeer() == target && !isRelayedConn(c) {
				onDirect()
			}
		},
	})
}
