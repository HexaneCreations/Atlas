package notification

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/hexane/atlas/internal/platform/safehttp"
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
// the guard refuses to dial; it is [safehttp.ErrUnsafeAddress].
var ErrUnsafeAddress = safehttp.ErrUnsafeAddress

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

// controlRefusingUnsafeAddr refuses every non-public address; see
// [safehttp.Control].
var controlRefusingUnsafeAddr = safehttp.Control(false)

func isSafePublicAddr(ip net.IP) bool { return safehttp.IsPublicAddr(ip) }

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
