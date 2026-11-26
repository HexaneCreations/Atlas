package monitor

import (
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder captures what a test server received.
type recorder struct {
	mu      sync.Mutex
	headers []http.Header
	bodies  []string
}

func (r *recorder) record(req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.headers = append(r.headers, req.Header.Clone())
	r.bodies = append(r.bodies, string(b))
}

func (r *recorder) last() (http.Header, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.headers) == 0 {
		return nil, ""
	}
	return r.headers[len(r.headers)-1], r.bodies[len(r.bodies)-1]
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.headers)
}

func server(t *testing.T, rec *recorder, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rec != nil {
			rec.record(r)
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func monitorFor(rawURL string) Monitor {
	m := Defaults()
	m.ID, m.Name, m.URL, m.TimeoutSeconds = "m1", "test", rawURL, 5
	return m
}

// local is a prober allowed to reach the loopback test servers.
var local = NewProber(ProberOptions{AllowPrivate: true})

func TestCheckStatusCodesAndKeyword(t *testing.T) {
	t.Parallel()
	srv := server(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, "all systems operational")
		case "/fail":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/big":
			_, _ = io.WriteString(w, strings.Repeat("x", MaxKeywordBodyBytes)+"needle")
		}
	})
	ctx := context.Background()

	r := local.Check(ctx, monitorFor(srv.URL+"/ok"), Secret{})
	if !r.Up || r.StatusCode != 200 || r.Failure != FailureNone || r.ResponseMS < 0 {
		t.Errorf("ok: %+v", r)
	}
	if r := local.Check(ctx, monitorFor(srv.URL+"/fail"), Secret{}); r.Up || r.Failure != FailureHTTPStatus || r.StatusCode != 503 {
		t.Errorf("fail: %+v", r)
	}
	m := monitorFor(srv.URL + "/fail")
	m.AcceptedStatus = []string{"500-599"}
	if r := local.Check(ctx, m, Secret{}); !r.Up {
		t.Errorf("accepted 5xx: %+v", r)
	}
	m = monitorFor(srv.URL + "/ok")
	m.Keyword = "operational"
	if r := local.Check(ctx, m, Secret{}); !r.Up {
		t.Errorf("keyword present: %+v", r)
	}
	m.Keyword = "degraded"
	if r := local.Check(ctx, m, Secret{}); r.Up || r.Failure != FailureKeyword || r.StatusCode != 200 {
		t.Errorf("keyword missing: %+v", r)
	}
	// A keyword past the read limit is not found: the body is never read in full.
	m = monitorFor(srv.URL + "/big")
	m.Keyword = "needle"
	if r := local.Check(ctx, m, Secret{}); r.Failure != FailureKeyword {
		t.Errorf("keyword past the limit: %+v", r)
	}
}

func TestCheckSendsCredentialsHeadersAndBody(t *testing.T) {
	t.Parallel()
	rec := &recorder{}
	srv := server(t, rec, func(http.ResponseWriter, *http.Request) {})
	ctx := context.Background()

	m := monitorFor(srv.URL)
	m.Method, m.AuthType, m.BodyEncoding = MethodPost, AuthBasic, BodyForm
	local.Check(ctx, m, Secret{Headers: map[string]string{"X-Api-Key": "k1"}, BasicUsername: "u", BasicPassword: "p", Body: "a=1"})
	h, body := rec.last()
	if u, p, _ := (&http.Request{Header: h}).BasicAuth(); u != "u" || p != "p" {
		t.Errorf("basic auth = %q/%q", u, p)
	}
	if h.Get("X-Api-Key") != "k1" || h.Get("Content-Type") != "application/x-www-form-urlencoded" || body != "a=1" {
		t.Errorf("headers %v body %q", h, body)
	}
	if h.Get("User-Agent") != userAgent {
		t.Errorf("user agent = %q", h.Get("User-Agent"))
	}

	m = monitorFor(srv.URL)
	m.AuthType = AuthBearer
	local.Check(ctx, m, Secret{BearerToken: "t0k", Body: "ignored on GET"})
	h, body = rec.last()
	if h.Get("Authorization") != "Bearer t0k" || body != "" {
		t.Errorf("bearer: %v %q", h, body)
	}
}

func TestCheckRedirects(t *testing.T) {
	t.Parallel()
	other := &recorder{}
	elsewhere := server(t, other, func(http.ResponseWriter, *http.Request) {})
	origin := &recorder{}
	var originURL string
	srv := server(t, origin, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/same":
			http.Redirect(w, r, "/landed", http.StatusFound)
		case "/cross":
			http.Redirect(w, r, elsewhere.URL+"/landed", http.StatusFound)
		case "/cross-307":
			http.Redirect(w, r, elsewhere.URL+"/landed", http.StatusTemporaryRedirect)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/userinfo":
			w.Header().Set("Location", "http://u:p@"+strings.TrimPrefix(originURL, "http://")+"/landed")
			w.WriteHeader(http.StatusFound)
		case "/ftp":
			w.Header().Set("Location", "ftp://example.com/")
			w.WriteHeader(http.StatusFound)
		}
	})
	originURL = srv.URL
	ctx := context.Background()
	secret := Secret{Headers: map[string]string{"X-Api-Key": "k1"}, BearerToken: "t0k"}
	bearer := func(path string) Monitor {
		m := monitorFor(srv.URL + path)
		m.AuthType = AuthBearer
		return m
	}

	// Same host: credentials follow.
	if r := local.Check(ctx, bearer("/same"), secret); !r.Up {
		t.Fatalf("same-host redirect: %+v", r)
	}
	if h, _ := origin.last(); h.Get("X-Api-Key") != "k1" || h.Get("Authorization") != "Bearer t0k" {
		t.Errorf("same-host hop lost credentials: %v", h)
	}

	// Another host (same IP, other port): every credential is removed.
	if r := local.Check(ctx, bearer("/cross"), secret); !r.Up {
		t.Fatalf("cross-host redirect: %+v", r)
	}
	if h, _ := other.last(); h.Get("X-Api-Key") != "" || h.Get("Authorization") != "" {
		t.Errorf("cross-host hop carried credentials: %v", h)
	}

	// A 307 would re-send the POST body to the other host: refused.
	m := bearer("/cross-307")
	m.Method = MethodPost
	n := other.count()
	if r := local.Check(ctx, m, Secret{Body: "secret-body"}); r.Failure != FailureRedirect {
		t.Errorf("cross-host 307 with body: %+v", r)
	}
	if other.count() != n {
		t.Error("the body reached the other host")
	}

	for path, want := range map[string]Failure{
		"/loop": FailureRedirect, "/metadata": FailureBlocked, "/userinfo": FailureRedirect, "/ftp": FailureRedirect,
	} {
		if r := local.Check(ctx, monitorFor(srv.URL+path), Secret{}); r.Failure != want {
			t.Errorf("%s: %+v, want %s", path, r, want)
		}
	}

	// max_redirects 0: the 302 itself is judged.
	m = monitorFor(srv.URL + "/same")
	m.MaxRedirects = 0
	if r := local.Check(ctx, m, Secret{}); r.Failure != FailureHTTPStatus || r.StatusCode != 302 {
		t.Errorf("no redirects: %+v", r)
	}
	m.AcceptedStatus = []string{"300-399"}
	if r := local.Check(ctx, m, Secret{}); !r.Up {
		t.Errorf("accepted 3xx: %+v", r)
	}
	m = monitorFor(srv.URL + "/same")
	m.MaxRedirects = 1
	if r := local.Check(ctx, m, Secret{}); !r.Up {
		t.Errorf("one redirect allowed: %+v", r)
	}
}

func TestKeepsCredentials(t *testing.T) {
	t.Parallel()
	u := func(s string) *url.URL { v, _ := url.Parse(s); return v }
	for _, c := range []struct {
		next, orig string
		keep       bool
	}{
		{"https://a.example/x", "https://a.example/", true},
		{"https://A.example:443/", "https://a.example/", true},
		{"https://a.example/", "http://a.example/", true},
		{"http://a.example/", "https://a.example/", false},
		{"https://a.example:8443/", "http://a.example/", false},
		{"https://b.example/", "https://a.example/", false},
		{"https://sub.a.example/", "https://a.example/", false},
		{"https://a.example:444/", "https://a.example/", false},
	} {
		if got := keepsCredentials(u(c.next), u(c.orig)); got != c.keep {
			t.Errorf("%s -> %s: keep = %v", c.orig, c.next, got)
		}
	}
}

func TestCheckRefusesNonPublicTargetsByDefault(t *testing.T) {
	t.Parallel()
	hits := &recorder{}
	srv := server(t, hits, func(http.ResponseWriter, *http.Request) {})
	public := NewProber(ProberOptions{})
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	for _, raw := range []string{
		srv.URL,                      // 127.0.0.1
		"http://localhost" + port,    // a name resolving to loopback
		"http://[::1]" + port,        // IPv6 loopback
		"http://169.254.169.254/",    // cloud metadata
		"http://[fd00:ec2::254]/",    // EC2 IPv6 metadata
		"http://0.0.0.0" + port,      // unspecified
		"http://[::ffff:127.0.0.1]/", // v4-mapped loopback
	} {
		if r := public.Check(context.Background(), monitorFor(raw), Secret{}); r.Failure != FailureBlocked {
			t.Errorf("%s: %+v, want blocked", raw, r)
		}
	}
	if n := hits.count(); n != 0 {
		t.Errorf("a refused target was reached %d times", n)
	}
	// Metadata stays refused with allow_private_targets on.
	if r := local.Check(context.Background(), monitorFor("http://169.254.169.254/"), Secret{}); r.Failure != FailureBlocked {
		t.Errorf("metadata with private allowed: %+v", r)
	}
}

func TestCheckIgnoresProxyEnvironment(t *testing.T) {
	// Not parallel: sets process environment.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	srv := server(t, nil, func(http.ResponseWriter, *http.Request) {})
	if r := NewProber(ProberOptions{AllowPrivate: true}).Check(context.Background(), monitorFor(srv.URL), Secret{}); !r.Up {
		t.Errorf("proxy env was honoured: %+v", r)
	}
}

func TestCheckTimeouts(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	srv := server(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow-body" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	for _, path := range []string{"/slow-headers", "/slow-body"} {
		m := monitorFor(srv.URL + path)
		m.TimeoutSeconds, m.Keyword = 1, "never"
		start := time.Now()
		if r := local.Check(context.Background(), m, Secret{}); r.Failure != FailureTimeout {
			t.Errorf("%s: %+v", path, r)
		}
		if time.Since(start) > 3*time.Second {
			t.Errorf("%s: the timeout did not bound the check", path)
		}
	}
}

func TestCheckFailureCategories(t *testing.T) {
	t.Parallel()
	// Nothing listens on a just-closed port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closed := "http://" + l.Addr().String()
	_ = l.Close()
	if r := local.Check(context.Background(), monitorFor(closed), Secret{}); r.Failure != FailureConnect {
		t.Errorf("refused: %+v", r)
	}
	if r := local.Check(context.Background(), monitorFor("http://no-such-host.invalid/"), Secret{}); r.Failure != FailureDNS {
		t.Errorf("nxdomain: %+v", r)
	}
	// Untrusted certificate.
	tlsSrv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(tlsSrv.Close)
	if r := local.Check(context.Background(), monitorFor(tlsSrv.URL), Secret{}); r.Failure != FailureTLS {
		t.Errorf("untrusted cert: %+v", r)
	}
	// Trusted: up, with the leaf's expiry.
	pool := x509.NewCertPool()
	pool.AddCert(tlsSrv.Certificate())
	trusting := NewProber(ProberOptions{AllowPrivate: true, RootCAs: pool})
	r := trusting.Check(context.Background(), monitorFor(tlsSrv.URL), Secret{})
	if !r.Up || r.CertExpiresAt == nil || !r.CertExpiresAt.Equal(tlsSrv.Certificate().NotAfter) {
		t.Errorf("trusted cert: %+v", r)
	}
}
