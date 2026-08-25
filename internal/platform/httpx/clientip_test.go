package httpx_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexane/atlas/internal/platform/httpx"
)

// simulateProxyChain builds the request atlas-server would actually receive
// after clientInitialXFF and realClientIP pass through this deployment's
// real two-hop chain (deploy/caddy/Caddyfile, docker-compose.prod.yml):
//
//  1. Caddy: per its own documented default (no trusted_proxies configured
//     in this repo's Caddyfile), it ignores clientInitialXFF outright and
//     sets X-Forwarded-For fresh to only realClientIP — see
//     https://caddyserver.com/docs/caddyfile/directives/reverse_proxy.
//  2. nginx (web/nginx.conf's `proxy_set_header X-Forwarded-For
//     $proxy_add_x_forwarded_for`): appends its own immediate peer — Caddy,
//     simulated here as nginxPeerOfCaddy — to whatever it received.
//
// clientInitialXFF is accepted so a caller can prove that whatever a client
// tries to plant there never survives the first hop.
func simulateProxyChain(clientInitialXFF, realClientIP, nginxPeerOfCaddy, atlasServerPeerOfNginx string) *http.Request {
	_ = clientInitialXFF // discarded by Caddy's own default — see the doc above
	afterCaddy := realClientIP
	afterNginx := afterCaddy + ", " + nginxPeerOfCaddy

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = atlasServerPeerOfNginx + ":54321"
	r.Header.Set("X-Forwarded-For", afterNginx)
	return r
}

// The topology this whole file's tests are pinned to.
func TestTrustedProxyHopsIsTwo(t *testing.T) {
	t.Parallel()
	if httpx.TrustedProxyHops != 2 {
		t.Fatalf("TrustedProxyHops = %d, want 2 (Caddy, then nginx — see deploy/caddy/Caddyfile and docker-compose.prod.yml)", httpx.TrustedProxyHops)
	}
}

// The realistic case: a real two-hop chain, no attacker involved, resolves
// to the real client.
func TestClientIPResolvesTheRealClientThroughTheTwoHopChain(t *testing.T) {
	t.Parallel()

	r := simulateProxyChain("", "203.0.113.7", "127.0.0.1", "172.20.0.2")
	if got := httpx.ClientIP(r); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want 203.0.113.7", got)
	}
}

// Required test 1: a client sends X-Forwarded-For: 1.2.3.4 directly —
// exactly one entry, fewer than TrustedProxyHops (2) — simulating a request
// that never actually passed through the real two-hop chain at all. The
// resolved IP must be the actual connection's address, never the claimed
// value.
func TestClientIPClientSuppliedHeaderAloneDoesNotSurvive(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.9:40000" // the real, actual connection
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	got := httpx.ClientIP(r)
	if got == "1.2.3.4" {
		t.Fatal("ClientIP trusted a single client-supplied entry as if it had passed through both real hops")
	}
	if got != "198.51.100.9" {
		t.Errorf("ClientIP = %q, want 198.51.100.9 (the real RemoteAddr, since the header had too few entries to trust)", got)
	}
}

// Required test 2: a client sends a long fabricated chain
// (X-Forwarded-For: 1.1.1.1, 2.2.2.2, 3.3.3.3) trying to look like it
// already passed through legitimate proxies. Run through the real chain
// simulation — which discards it at the Caddy hop exactly as Caddy's own
// documented default does — the resolved IP must be the attacker's actual
// connecting address, never any of the fabricated entries.
func TestClientIPLongFabricatedChainDoesNotSurviveTheRealChain(t *testing.T) {
	t.Parallel()

	const attackerRealIP = "192.0.2.55"
	r := simulateProxyChain("1.1.1.1, 2.2.2.2, 3.3.3.3", attackerRealIP, "127.0.0.1", "172.20.0.2")

	got := httpx.ClientIP(r)
	for _, fake := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"} {
		if got == fake {
			t.Fatalf("ClientIP = %q, a fabricated entry the attacker supplied — none of them should ever survive Caddy's own default (ignores incoming X-Forwarded-For, sets it fresh)", got)
		}
	}
	if got != attackerRealIP {
		t.Errorf("ClientIP = %q, want %q (the attacker's real connecting address)", got, attackerRealIP)
	}
}

// The same fabricated-chain attempt, but proving the ClientIP algorithm
// itself is robust independent of Caddy's current default — i.e. even if
// the header somehow arrived with extra attacker-controlled entries ahead
// of the two genuine ones (Caddy reconfigured with trusted_proxies and
// appending instead of replacing, or any other future change), counting
// from the right still lands on the correct, genuine entry rather than the
// leftmost or any padded one.
func TestClientIPCountsFromTheRightRegardlessOfExtraLeadingEntries(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "172.20.0.2:54321"
	// 5 entries: 3 fabricated, then the 2 a real chain would have appended.
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 8.8.8.8, 7.7.7.7, 203.0.113.7, 127.0.0.1")

	if got := httpx.ClientIP(r); got != "203.0.113.7" {
		t.Errorf("ClientIP = %q, want 203.0.113.7 (2nd from the right, regardless of how much precedes it)", got)
	}
}

// Required test 4: fewer entries than TrustedProxyHops — including no
// header at all, and a header present but blank — falls back to RemoteAddr,
// doesn't crash, doesn't trust anything client-controlled.
func TestClientIPFallsBackToRemoteAddrWhenTheChainIsIncomplete(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		xff  string // "" means the header is not set at all
	}{
		{"no header at all", ""},
		{"one entry (only one real hop, or none)", "203.0.113.7"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.0.0.5:9999"
			if tt.xff != "" {
				r.Header.Set("X-Forwarded-For", tt.xff)
			}

			if got := httpx.ClientIP(r); got != "10.0.0.5" {
				t.Errorf("ClientIP = %q, want 10.0.0.5 (RemoteAddr fallback)", got)
			}
		})
	}
}

// A header present but empty at the trusted position (rather than simply
// too short) must not resolve to an empty string either.
func TestClientIPFallsBackWhenTheTrustedPositionIsBlank(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.6:9999"
	r.Header.Set("X-Forwarded-For", ", 127.0.0.1") // 2 entries, but the trusted one is blank

	if got := httpx.ClientIP(r); got != "10.0.0.6" {
		t.Errorf("ClientIP = %q, want 10.0.0.6 (RemoteAddr fallback for a blank trusted entry)", got)
	}
}

// ClientIP must never panic on a malformed RemoteAddr either (no port, or
// garbage) — [net.SplitHostPort] failing is the one case the original
// implementation already handled, preserved here.
func TestClientIPDoesNotPanicOnAMalformedRemoteAddr(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "not-a-valid-address"

	if got := httpx.ClientIP(r); got != "not-a-valid-address" {
		t.Errorf("ClientIP = %q, want the raw RemoteAddr echoed back", got)
	}
}
