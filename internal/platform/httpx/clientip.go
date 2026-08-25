package httpx

import (
	"net"
	"net/http"
	"strings"
)

// TrustedProxyHops is the number of reverse proxies this deployment's own
// topology places between a real client and atlas-server, each expected to
// append exactly one entry to X-Forwarded-For — see [ClientIP]. Fixed at 2:
//
//	internet client -> Caddy (public, terminates TLS) -> atlas-ui/nginx
//	(127.0.0.1:8081, loopback-only) -> atlas-server (atlas-internal Docker
//	network only; port 8080 is "expose"d and never "ports"-published)
//
// (deploy/caddy/Caddyfile, docker-compose.prod.yml). Named and centralised
// here, not a literal buried in the parsing logic, specifically so it stays
// visibly tied to that topology and is easy to find and revisit if a hop is
// ever added or removed.
const TrustedProxyHops = 2

// ClientIP returns the real client's address for this deployment's fixed
// reverse-proxy topology — see [TrustedProxyHops] — falling back to the
// immediate TCP peer when X-Forwarded-For is absent or has fewer entries
// than that topology guarantees, rather than guessing.
//
// # Why counting from the right, not the left
//
// X-Forwarded-For is comma-separated, and by RFC 7239's convention each
// proxy appends what it saw as its own immediate peer. A client controls
// everything it sends as the *initial* value before the header ever reaches
// the first real proxy, so entries are only trustworthy counting backward
// from the right — exactly as many positions as there are hops
// independently verified to be this deployment's own. With
// TrustedProxyHops == 2 (Caddy, then nginx), the real client's address is
// the entry two positions in from the right:
//
//	X-Forwarded-For: <anything a client could have sent>, <real client>, <Caddy's address>
//	                                                        ^^^^^^^^^^^ 2nd from the right: this one
//	                  (appended by Caddy)                                (appended by nginx)
//
// In slice terms, for parts := strings.Split(header, ","), the client is
// parts[len(parts)-TrustedProxyHops] — not "skip TrustedProxyHops entries
// from the right and take the next", which would land one position too far
// left, onto a client-controlled entry when one is present. A worked check:
// with exactly TrustedProxyHops entries (the minimal, correctly-relayed
// case), len(parts)-TrustedProxyHops == 0 — the leftmost, and only
// legitimate, candidate for "the entry the first trusted hop appended",
// which is by definition the real client.
//
// This holds regardless of how many extra entries a client prepends to the
// left trying to look like it already passed through legitimate proxies:
// padding only pushes the trustworthy boundary further left in the raw
// string, never changes which position from the *right* is correct.
//
// # Caddy's own default already discards a client-supplied header
//
// deploy/caddy/Caddyfile configures no trusted_proxies, so by Caddy's own
// documented default, incoming X-Forwarded-For values are ignored outright
// before Caddy sets the header fresh with only its own observed peer:
// "For these X-Forwarded-* headers, by default, the proxy will ignore their
// values from incoming requests, to prevent spoofing."
// (https://caddyserver.com/docs/caddyfile/directives/reverse_proxy). In
// practice this means a client's attempt to pre-seed the header is wiped
// entirely at the Caddy hop, before nginx ever appends its own entry — the
// header this deployment actually sees has at most TrustedProxyHops
// entries. This function does not depend on that holding forever: counting
// from the right is correct whether Caddy replaces (today) or is ever
// reconfigured with trusted_proxies to append instead, which is the point
// of deriving position from the right rather than trusting a fixed length.
//
// # Which peer is trusted to be nginx
//
// Every call here trusts that whatever connected directly to atlas-server
// is nginx — not a specific verified IP. Docker's bridge networking gives
// no stable per-container IP across restarts, so pinning to one would be
// fragile in exactly the way that invites a silent break. Instead this
// relies on the network topology itself: atlas-server's port is "expose"d
// and never "ports"-published (docker-compose.prod.yml), so nothing outside
// the atlas-internal Docker network can reach it at all, and atlas-ui's
// nginx is the only service on that network configured to. "Any peer at
// all" is therefore an acceptable trust boundary here, not a weak one — the
// boundary that actually matters is enforced by the compose file's network
// topology, not by application-layer IP matching. If atlas-internal ever
// gains another service capable of reaching atlas-server directly, this
// assumption needs revisiting alongside TrustedProxyHops.
func ClientIP(r *http.Request) string {
	if ip := trustedForwardedFor(r); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// trustedForwardedFor returns the client IP X-Forwarded-For names at
// exactly TrustedProxyHops positions from the right, or "" if the header is
// absent, blank at that position, or has fewer entries than that — never a
// guess at a shorter list.
func trustedForwardedFor(r *http.Request) string {
	raw := r.Header.Get("X-Forwarded-For")
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, ",")
	if len(parts) < TrustedProxyHops {
		return ""
	}
	return strings.TrimSpace(parts[len(parts)-TrustedProxyHops])
}
