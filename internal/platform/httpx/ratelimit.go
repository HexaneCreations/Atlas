package httpx

import (
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/pki"
)

// limiterIdleTTL is how long an idle limiter is kept before eviction. It
// bounds memory on a control plane whose fleet churns — an agent that
// enrolled once and disappeared must not hold a limiter forever — while
// being long enough that a real agent's own interval never loses its bucket
// between requests.
const limiterIdleTTL = 30 * time.Minute

// PerNodeRateLimit bounds how often one enrolled node may call the agent
// API, at requestsPerMinute sustained with burst headroom for a spool
// draining after an outage.
//
// Identity comes from the mTLS peer certificate, not the source address:
// many agents legitimately share an egress IP, and one agent moves between
// addresses, so limiting by address would either punish a whole site or
// miss the agent it was meant to bound. A request with no verified
// certificate is not limited here, and the routes that accept it enforce
// their own admission: on the HTTPS listener that is enrollment (tokens are
// single-use, bounded and expiring), and on the libp2p listener it is the
// Noise-authenticated Peer ID checked against agent_peers (see
// internal/api/agent.PeerAuthMiddleware).
func PerNodeRateLimit(requestsPerMinute int) Middleware {
	limiters := newNodeLimiters(rate.Limit(float64(requestsPerMinute)/60), requestsPerMinute)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			nodeID, err := pki.PeerNodeID(r.TLS.PeerCertificates[0])
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}

			if !limiters.allow(nodeID) {
				Error(w, r, errs.New(errs.CodeRateLimited, "too many requests from this node").
					WithOp("httpx.PerNodeRateLimit").WithDetail("node_id", nodeID))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type nodeLimiters struct {
	limit rate.Limit
	burst int

	mu      sync.Mutex
	byNode  map[string]*nodeLimiter
	lastGC  time.Time
	nowFunc func() time.Time
}

type nodeLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newNodeLimiters(limit rate.Limit, burst int) *nodeLimiters {
	return &nodeLimiters{
		limit: limit, burst: burst,
		byNode: map[string]*nodeLimiter{}, nowFunc: time.Now,
	}
}

func (n *nodeLimiters) allow(nodeID string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()

	now := n.nowFunc()
	l, ok := n.byNode[nodeID]
	if !ok {
		l = &nodeLimiter{limiter: rate.NewLimiter(n.limit, n.burst)}
		n.byNode[nodeID] = l
	}
	l.lastSeen = now

	// Swept on use rather than by a goroutine: this map is only ever touched
	// under this lock, and a background sweeper would add a lifecycle to
	// every listener that installs the middleware.
	if now.Sub(n.lastGC) > limiterIdleTTL {
		for id, entry := range n.byNode {
			if now.Sub(entry.lastSeen) > limiterIdleTTL {
				delete(n.byNode, id)
			}
		}
		n.lastGC = now
	}

	return l.limiter.AllowN(now, 1)
}
