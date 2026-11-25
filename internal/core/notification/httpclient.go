package notification

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"syscall"
	"time"
)

const (
	// DefaultSendTimeout bounds the whole request: connect, write, and read
	// of the response body, not just the initial connection.
	DefaultSendTimeout = 10 * time.Second
	// DefaultResponseBodyLimit is how much of a destination's response is
	// read before the rest is discarded unread.
	DefaultResponseBodyLimit = 64 * 1024
	// DefaultRequestBodyLimit is the largest request body a sender will
	// post. It is checked before the request is sent, not enforced by
	// truncating a POST body mid-stream, which would corrupt it.
	DefaultRequestBodyLimit = 64 * 1024
)

// ErrRedirectRefused means a channel destination replied with a redirect.
// The guarded client's CheckRedirect stops at the first hop (so the
// redirect is never followed, including somewhere the dial guard would
// otherwise refuse), and the sender treats that response as this error
// rather than as a successful delivery.
var ErrRedirectRefused = errors.New("the destination returned a redirect, which notification delivery does not follow")

// ErrRequestTooLarge means the rendered request body exceeds the sender's
// configured limit.
var ErrRequestTooLarge = errors.New("the request body exceeds the configured size limit")

// ErrUnsafeAddress means a destination's hostname resolved to an address
// this guard refuses to dial: loopback, private, link-local (including the
// 169.254.169.254 cloud metadata address), CGNAT, unspecified, multicast,
// or an IPv6 form of any of those.
var ErrUnsafeAddress = errors.New("refusing to dial a non-public address")

// newGuardedClient builds an http.Client safe to point at a URL that came
// from configuration rather than from source code: it resolves the
// destination and checks the IP actually being dialed (not just the
// hostname, so a DNS name cannot be used to reach an address this guard
// would otherwise refuse), follows no redirect, ignores HTTP_PROXY /
// HTTPS_PROXY / NO_PROXY, and bounds the whole exchange by timeout.
func newGuardedClient(timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = DefaultSendTimeout
	}
	dialer := &net.Dialer{Control: controlRefusingUnsafeAddr}
	return &http.Client{
		Timeout: timeout,
		// ErrUseLastResponse stops the client from following the redirect;
		// the sender itself still has to treat the resulting 3xx response
		// as a failure (http.Client does not error on its own here).
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: &http.Transport{
			Proxy:                 nil, // never honour proxy environment variables for this client
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConnsPerHost:   2,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// controlRefusingUnsafeAddr is a [net.Dialer.Control] callback: it runs
// after DNS resolution, once per candidate address, with the literal IP
// about to be dialed — so it sees through a DNS name to what is actually
// being connected to.
func controlRefusingUnsafeAddr(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrUnsafeAddress, network)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: %q did not resolve to a literal IP", ErrUnsafeAddress, host)
	}
	if !isSafePublicAddr(ip) {
		return ErrUnsafeAddress
	}
	return nil
}

// Reserved and special-purpose ranges with no net.IP helper, checked
// alongside loopback/private/link-local/unspecified/multicast, which the
// standard library already recognises directly on a net.IP.
var (
	zeroBlock      = mustParseCIDR("0.0.0.0/8")     // RFC 791 §3.2, "this network"
	cgnatBlock     = mustParseCIDR("100.64.0.0/10") // RFC 6598, shared address space
	ietfProtocol   = mustParseCIDR("192.0.0.0/24")  // RFC 6890, IETF protocol assignments
	benchmarking   = mustParseCIDR("198.18.0.0/15") // RFC 2544, network interconnect testing
	reservedFuture = mustParseCIDR("240.0.0.0/4")   // RFC 1112, reserved; subsumes 255.255.255.255

	nat64     = mustParseCIDR("64:ff9b::/96") // RFC 6052, the NAT64 well-known prefix
	sixToFour = mustParseCIDR("2002::/16")    // RFC 3056, 6to4
)

func mustParseCIDR(s string) *net.IPNet {
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return n
}

// isSafePublicAddr reports whether ip is safe for this guard to dial.
//
// For an IPv4 address (including one carried as IPv4-mapped IPv6,
// ::ffff:a.b.c.d, which ip.To4() resolves the same as a literal v4
// address): not loopback, private (RFC1918), link-local (including
// 169.254.169.254's /16), unspecified, multicast, "this network"
// (0.0.0.0/8), IETF protocol assignments (192.0.0.0/24), benchmarking
// (198.18.0.0/15), reserved (240.0.0.0/4, which already subsumes the
// 255.255.255.255 broadcast address), or CGNAT (100.64.0.0/10) — the last
// two with no net.IP helper, checked against the CIDRs above.
//
// For a genuine IPv6 address: not loopback, private (the IPv6 ULA block,
// fc00::/7), link-local, unspecified, or multicast. NAT64 (64:ff9b::/96)
// and 6to4 (2002::/16) are translation mechanisms that carry an IPv4
// address in fixed bits rather than addressing anything of their own, so
// rather than trust the wrapper, [embeddedIPv4] extracts that address and
// this function is applied to it recursively — an address such as
// 64:ff9b::a9fe:a9fe (NAT64 for 169.254.169.254) is refused because the
// cloud metadata address it carries is, not merely because it falls in a
// generically-blocked IPv6 range.
func isSafePublicAddr(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4.IsLoopback(), ip4.IsPrivate(), ip4.IsLinkLocalUnicast(), ip4.IsLinkLocalMulticast(),
			ip4.IsUnspecified(), ip4.IsMulticast():
			return false
		case zeroBlock.Contains(ip4), ietfProtocol.Contains(ip4), benchmarking.Contains(ip4),
			reservedFuture.Contains(ip4), cgnatBlock.Contains(ip4):
			return false
		}
		return true
	}

	switch {
	case ip.IsLoopback(), ip.IsPrivate(), ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast(),
		ip.IsUnspecified(), ip.IsMulticast():
		return false
	}
	if embedded := embeddedIPv4(ip); embedded != nil {
		return isSafePublicAddr(embedded)
	}
	return true
}

// embeddedIPv4 extracts the IPv4 address a NAT64 (RFC 6052) or 6to4
// (RFC 3056) IPv6 address carries, or nil if ip is neither. NAT64 embeds
// it in the low 32 bits of the 64:ff9b::/96 prefix; 6to4 embeds it in the
// 32 bits immediately after the fixed 2002: prefix.
func embeddedIPv4(ip net.IP) net.IP {
	ip16 := ip.To16()
	if ip16 == nil {
		return nil
	}
	switch {
	case nat64.Contains(ip16):
		return net.IPv4(ip16[12], ip16[13], ip16[14], ip16[15])
	case sixToFour.Contains(ip16):
		return net.IPv4(ip16[2], ip16[3], ip16[4], ip16[5])
	}
	return nil
}

// wrapSendError turns err from building or performing a request into an
// error safe to return from a [Sender]: never the destination URL itself.
// When err is (or wraps) a *url.Error — which is what both
// http.NewRequestWithContext (a malformed URL) and http.Client.Do (any
// transport failure) return — only the operation and the host are taken
// from it, and the host is parsed back out of url.Error.URL rather than
// used as a raw string, so a URL whose path or query carries the
// credential (a Teams sig=, a Slack or Discord path-embedded token) never
// appears. The inner cause passes through [Redact] as a second line of
// defense regardless.
//
// The result is a [*SendError]: besides that text it carries the fixed
// category [SendErrorCategory] reports, decided here while the cause is
// still typed.
func wrapSendError(op string, err error) error {
	return &SendError{msg: sendErrorText(op, err), category: transportCategory(err)}
}

func sendErrorText(op string, err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		host := ""
		if u, parseErr := url.Parse(urlErr.URL); parseErr == nil {
			host = u.Host
		}
		if host != "" {
			return fmt.Sprintf("%s: %s %s: %s", op, urlErr.Op, host, Redact(urlErr.Err.Error()))
		}
		return fmt.Sprintf("%s: %s: %s", op, urlErr.Op, Redact(urlErr.Err.Error()))
	}
	return fmt.Sprintf("%s: %s", op, Redact(err.Error()))
}

// readLimitedResponse reads up to limit bytes of resp's body and discards
// the rest unread (so the connection can still be reused), then closes it.
func readLimitedResponse(resp *http.Response, limit int) []byte {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, int64(limit)))
	_, _ = io.Copy(io.Discard, resp.Body)
	return body
}
