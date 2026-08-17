package httpx_test

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/pki"
)

func nodeRequest(t *testing.T, ca *pki.CA, nodeID string) *http.Request {
	t.Helper()

	der, _, err := pki.NewCSR(nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	csr, err := pki.ParseCSR(der)
	if err != nil {
		t.Fatalf("ParseCSR: %v", err)
	}
	leaf, err := ca.IssueLeaf(csr, nodeID)
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", nil)
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
	return req
}

func countOK(t *testing.T, h http.Handler, req func() *http.Request, attempts int) (ok, limited int) {
	t.Helper()
	for i := 0; i < attempts; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req())
		switch rec.Code {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("unexpected status %d", rec.Code)
		}
	}
	return ok, limited
}

// One misconfigured or compromised agent must not be able to flood ingest
// for the whole fleet.
func TestPerNodeRateLimitBoundsOneNode(t *testing.T) {
	t.Parallel()

	ca, err := pki.New("test-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}

	const perMinute = 10
	h := httpx.PerNodeRateLimit(perMinute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := nodeRequest(t, ca, "node-1")
	ok, limited := countOK(t, h, func() *http.Request { return req }, perMinute*3)

	if ok > perMinute {
		t.Errorf("%d requests allowed, want no more than the %d burst", ok, perMinute)
	}
	if limited == 0 {
		t.Error("no request was rate limited")
	}
}

// The limit is per node identity: one noisy agent must not consume another
// agent's allowance, since many agents legitimately share an egress address.
func TestPerNodeRateLimitIsolatesNodes(t *testing.T) {
	t.Parallel()

	ca, err := pki.New("test-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}

	const perMinute = 5
	h := httpx.PerNodeRateLimit(perMinute)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	noisy := nodeRequest(t, ca, "node-noisy")
	if _, limited := countOK(t, h, func() *http.Request { return noisy }, perMinute*4); limited == 0 {
		t.Fatal("the noisy node was never limited, so this test proves nothing")
	}

	quiet := nodeRequest(t, ca, "node-quiet")
	ok, limited := countOK(t, h, func() *http.Request { return quiet }, perMinute)
	if ok != perMinute || limited != 0 {
		t.Errorf("quiet node: %d ok / %d limited, want all %d allowed", ok, limited, perMinute)
	}
}

// Enrollment arrives with no client certificate by design. Limiting it here
// would reject first contact for a whole fleet provisioning at once; that
// route's own admission control (single-use, expiring tokens) is what bounds
// it.
func TestPerNodeRateLimitPassesUnauthenticatedRequestsThrough(t *testing.T) {
	t.Parallel()

	h := httpx.PerNodeRateLimit(1)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/agent/enroll", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status = %d, want 200", i, rec.Code)
		}
	}
}
