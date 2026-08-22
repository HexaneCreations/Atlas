package agent

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/libp2p/go-libp2p/core/peer"
)

type peerIdentityKey struct{}

// WithPeerIdentity returns ctx carrying an authenticated, authorized libp2p
// peer's identity. Exported for tests and for a composition root that
// authenticates a stream some other way; ordinary requests get it from
// [PeerAuthMiddleware].
func WithPeerIdentity(ctx context.Context, id fleet.PeerIdentity) context.Context {
	return context.WithValue(ctx, peerIdentityKey{}, id)
}

// PeerIdentityFrom returns the identity [PeerAuthMiddleware] resolved for
// this request, if it arrived over an authenticated libp2p stream.
func PeerIdentityFrom(ctx context.Context) (fleet.PeerIdentity, bool) {
	id, ok := ctx.Value(peerIdentityKey{}).(fleet.PeerIdentity)
	return id, ok
}

// PeerAuthMiddleware authenticates agent requests by libp2p Peer ID instead
// of by client certificate, and must be mounted only on a libp2p listener.
//
// There is no credential to check here and deliberately none to introduce.
// By the time a request reaches this middleware the libp2p Noise handshake
// has already proven, cryptographically, which Peer ID is on the other end
// of the stream — r.RemoteAddr on a gostream listener is that verified Peer
// ID, not anything the caller can assert. What remains is authorization:
// whether that peer has been registered by an operator, and which node it
// speaks for. That answer comes from [fleet.PeerStore], never from the
// request body.
//
// A peer with no active authorization is refused with 403 and no further
// detail. onAuthorized, when set, is called with each resolved identity and
// the peer it arrived from — the composition root uses it to record which
// Peer ID a node is currently reachable at, for reverse (AgentOps) streams.
func PeerAuthMiddleware(store fleet.PeerStore, logger *slog.Logger, onAuthorized func(fleet.PeerIdentity, peer.ID)) httpx.Middleware {
	const op = "agent.PeerAuthMiddleware"
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// A plain TCP RemoteAddr is "ip:port" and never decodes as a Peer
			// ID, so this middleware mounted on the wrong listener refuses
			// every request rather than silently authenticating nobody.
			pid, err := peer.Decode(r.RemoteAddr)
			if err != nil {
				httpx.Error(w, r, errs.New(errs.CodePermissionDenied,
					"this endpoint is reachable only over an authenticated libp2p stream").WithOp(op))
				return
			}

			id, err := store.AuthorizedPeer(r.Context(), pid.String())
			if err != nil {
				logger.WarnContext(r.Context(), "libp2p peer refused",
					slog.String("peer_id", pid.String()), slog.String("path", r.URL.Path),
					slog.String("error", err.Error()))
				httpx.Error(w, r, err)
				return
			}

			if onAuthorized != nil {
				onAuthorized(id, pid)
			}
			next.ServeHTTP(w, r.WithContext(WithPeerIdentity(r.Context(), id)))
		})
	}
}
