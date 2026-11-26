package monitor

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/safehttp"
)

// Limits on what a check reads.
const (
	// MaxKeywordBodyBytes is how much of a response a keyword is searched
	// in. Nothing beyond it is read, and nothing is ever stored.
	MaxKeywordBodyBytes = 1 << 20
	maxResponseHeader   = 64 << 10
	userAgent           = "Atlas-Monitor/1.0"
)

// Result is the outcome of one check.
type Result struct {
	Up            bool
	StatusCode    int // 0 when no response arrived
	ResponseMS    int // 0 when no response arrived
	Failure       Failure
	CertExpiresAt *time.Time
}

// ProberOptions configures a [Prober].
type ProberOptions struct {
	// AllowPrivate is monitoring.allow_private_targets.
	AllowPrivate bool
	// Resolver overrides the system resolver; nil uses it.
	Resolver *net.Resolver
	// RootCAs overrides the system roots; nil uses them.
	RootCAs *x509.CertPool
}

// Prober runs HTTP(S) checks. Every connection is fresh (no keep-alive), so
// each check measures and validates DNS, connect and TLS again, and every
// dial — the first and each redirect hop — passes the address guard.
// Proxy environment variables are ignored.
type Prober struct {
	transport *http.Transport
}

// NewProber builds a prober.
func NewProber(o ProberOptions) *Prober {
	dialer := &net.Dialer{Control: safehttp.Control(o.AllowPrivate), Resolver: o.Resolver, Timeout: 30 * time.Second}
	return &Prober{transport: &http.Transport{
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		DisableKeepAlives:      true,
		ForceAttemptHTTP2:      true,
		TLSHandshakeTimeout:    30 * time.Second,
		MaxResponseHeaderBytes: maxResponseHeader,
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: o.RootCAs},
	}}
}

// errRedirect is a redirect the check refuses to follow.
type errRedirect struct{ reason string }

func (e *errRedirect) Error() string { return "redirect refused: " + e.reason }

// Check runs one check of m. It never returns an error: every failure is a
// [Result] with its [Failure] category, and nothing in it comes from the
// response body or the error text.
func (p *Prober) Check(ctx context.Context, m Monitor, s Secret) Result {
	codes, err := ParseStatusCodes(m.AcceptedStatus)
	if err != nil {
		return Result{Failure: FailureHTTPStatus}
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.TimeoutSeconds)*time.Second)
	defer cancel()

	var body io.Reader
	if m.Method == MethodPost && s.Body != "" {
		body = strings.NewReader(s.Body)
	}
	req, err := http.NewRequestWithContext(ctx, string(m.Method), m.URL, body)
	if err != nil {
		return Result{Failure: FailureConnect}
	}
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", contentType(m.BodyEncoding))
	}
	for name, value := range s.Headers {
		req.Header.Set(name, value)
	}
	switch m.AuthType {
	case AuthBasic:
		req.SetBasicAuth(s.BasicUsername, s.BasicPassword)
	case AuthBearer:
		req.Header.Set("Authorization", "Bearer "+s.BearerToken)
	}

	client := &http.Client{Transport: p.transport, CheckRedirect: redirectPolicy(m, s)}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return Result{Failure: categorize(err)}
	}
	defer func() { _ = resp.Body.Close() }()

	r := Result{StatusCode: resp.StatusCode}
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		exp := resp.TLS.PeerCertificates[0].NotAfter
		r.CertExpiresAt = &exp
	}
	var found bool
	if m.Keyword != "" {
		found, err = containsLimited(resp.Body, []byte(m.Keyword))
		if err != nil {
			r.ResponseMS = elapsedMS(start)
			r.Failure = categorize(err)
			return r
		}
	}
	r.ResponseMS = elapsedMS(start)
	switch {
	case !codes.Accepts(resp.StatusCode):
		r.Failure = FailureHTTPStatus
	case m.Keyword != "" && !found:
		r.Failure = FailureKeyword
	default:
		r.Up = true
	}
	return r
}

func elapsedMS(start time.Time) int {
	return int(time.Since(start).Milliseconds())
}

func contentType(e BodyEncoding) string {
	switch e {
	case BodyForm:
		return "application/x-www-form-urlencoded"
	case BodyText:
		return "text/plain; charset=utf-8"
	}
	return "application/json"
}

// containsLimited searches at most MaxKeywordBodyBytes of r for kw.
func containsLimited(r io.Reader, kw []byte) (bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxKeywordBodyBytes))
	if err != nil {
		return false, err
	}
	return bytes.Contains(b, kw), nil
}

// redirectPolicy follows at most m.MaxRedirects hops (none: the 3xx itself
// is judged against the accepted codes), only to http(s) URLs without
// userinfo. On any hop that leaves the original host and port (an
// http-to-https upgrade on the default ports stays), every credential the
// check carries is removed — Go itself keeps custom headers
// across hosts — and a request that would re-send its body there (307/308)
// is refused.
func redirectPolicy(m Monitor, s Secret) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if m.MaxRedirects == 0 {
			return http.ErrUseLastResponse
		}
		if len(via) > m.MaxRedirects {
			return &errRedirect{"too many redirects"}
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return &errRedirect{"not http or https"}
		}
		if req.URL.User != nil {
			return &errRedirect{"userinfo in the location"}
		}
		if keepsCredentials(req.URL, via[0].URL) {
			return nil
		}
		if req.Body != nil && req.Body != http.NoBody {
			return &errRedirect{"would send the request body to another host"}
		}
		for name := range s.Headers {
			req.Header.Del(name)
		}
		req.Header.Del("Authorization")
		req.Header.Del("Cookie")
		return nil
	}
}

func keepsCredentials(next, orig *url.URL) bool {
	if !strings.EqualFold(next.Hostname(), orig.Hostname()) {
		return false
	}
	if next.Scheme == orig.Scheme {
		return port(next) == port(orig)
	}
	return orig.Scheme == "http" && next.Scheme == "https" && port(orig) == "80" && port(next) == "443"
}

func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// categorize maps a transport error to a [Failure]. The error itself is
// dropped: it can carry the URL, query string included.
func categorize(err error) Failure {
	var redir *errRedirect
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameErr x509.HostnameError
	var invalidCert x509.CertificateInvalidError
	var recordErr tls.RecordHeaderError
	var alertErr tls.AlertError
	var netErr net.Error
	switch {
	case errors.Is(err, safehttp.ErrUnsafeAddress):
		return FailureBlocked
	case errors.As(err, &redir):
		return FailureRedirect
	case errors.As(err, &dnsErr):
		return FailureDNS
	case errors.As(err, &certErr), errors.As(err, &unknownAuthority), errors.As(err, &hostnameErr),
		errors.As(err, &invalidCert), errors.As(err, &recordErr), errors.As(err, &alertErr):
		return FailureTLS
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded),
		errors.As(err, &netErr) && netErr.Timeout():
		return FailureTimeout
	case strings.Contains(err.Error(), "tls: "):
		return FailureTLS
	}
	return FailureConnect
}
