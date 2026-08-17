package agent_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/api/agent"
	"github.com/hexane/atlas/internal/core/collect"
	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/platform/pki"
)

type fakeTokens struct{ ok bool }

func (f *fakeTokens) Redeem(context.Context, string, net.IP, time.Time) (fleet.TokenGrant, error) {
	if !f.ok {
		return fleet.TokenGrant{}, fleet.ErrTokenInvalid
	}
	return fleet.TokenGrant{Environment: "production"}, nil
}

type fakeCredentials struct {
	byNode map[string]*fleet.Credential
}

func newFakeCredentials() *fakeCredentials {
	return &fakeCredentials{byNode: map[string]*fleet.Credential{}}
}

func (f *fakeCredentials) ActiveCredential(_ context.Context, nodeID string, now time.Time) (*fleet.Credential, error) {
	c, ok := f.byNode[nodeID]
	if !ok || now.After(c.ExpiresAt) {
		return nil, nil
	}
	return c, nil
}
func (f *fakeCredentials) RecordIssuance(_ context.Context, cred fleet.Credential) error {
	c := cred
	f.byNode[cred.NodeID] = &c
	return nil
}
func (f *fakeCredentials) Revoke(_ context.Context, fingerprint, _ string, _ time.Time) error {
	for k, c := range f.byNode {
		if c.Fingerprint == fingerprint {
			delete(f.byNode, k)
		}
	}
	return nil
}

type fakeDenylist struct{ denied map[string]bool }

func (f *fakeDenylist) IsDenied(_ context.Context, nodeID string) (bool, error) {
	return f.denied[nodeID], nil
}

type fakeGrants struct{}

func (fakeGrants) IsGranted(context.Context, string, string) (bool, error)              { return true, nil }
func (fakeGrants) Grant(context.Context, string, string, string, time.Time) error       { return nil }
func (fakeGrants) RevokeGrant(context.Context, string, string, string, time.Time) error { return nil }

type fakeClockSkew struct {
	nodeID string
	skew   float64
	calls  int
}

func (f *fakeClockSkew) UpdateClockSkew(_ context.Context, nodeID string, skew float64) error {
	f.nodeID, f.skew = nodeID, skew
	f.calls++
	return nil
}

type recordingReceiver struct {
	kind     transport.Kind
	received []transport.Envelope
	err      error
}

func (r *recordingReceiver) Kind() transport.Kind { return r.kind }
func (r *recordingReceiver) Receive(_ context.Context, env transport.Envelope) error {
	if r.err != nil {
		return r.err
	}
	r.received = append(r.received, env)
	return nil
}

func testHandler(t *testing.T, tokens *fakeTokens, creds *fakeCredentials, deny *fakeDenylist, recv *recordingReceiver, skew *fakeClockSkew) (*agent.Handler, *pki.CA) {
	t.Helper()
	ca, err := pki.New("test-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	router := transport.NewRouter()
	if recv != nil {
		if err := router.Register(recv); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	deps := agent.Deps{
		CA:       ca,
		Enroller: fleet.NewEnroller(ca, tokens, creds, deny, fakeGrants{}),
		Denylist: deny,
		Router:   router,
	}
	// A typed nil *fakeClockSkew assigned into the ClockSkewRecorder
	// interface field would be a non-nil interface holding a nil pointer —
	// the handler's nil check would pass and then panic on the call. Only
	// assign it when a real fake was supplied.
	if skew != nil {
		deps.ClockSkew = skew
	}
	h := agent.NewHandler(deps)
	return h, ca
}

func csrFor(t *testing.T, nodeID string) string {
	t.Helper()
	der, _, err := pki.NewCSR(nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func withPeerCert(r *http.Request, cert *x509.Certificate) *http.Request {
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	return r
}

func issueCert(t *testing.T, ca *pki.CA, nodeID string) *x509.Certificate {
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
	return leaf
}

func doJSON(h http.HandlerFunc, method, path string, body any) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestEnrollIssuesCertificate(t *testing.T) {
	t.Parallel()
	h, _ := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, nil, nil)

	rec := doJSON(handlerFunc(h.Enroll), http.MethodPost, "/api/v1/agent/enroll", agent.EnrollRequest{
		Token: "atlas_enroll_x", NodeID: "node-1", CSR: csrFor(t, "node-1"),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	var resp agent.CertResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Certificate == "" || resp.CACertificate == "" {
		t.Error("missing certificate or CA certificate in response")
	}
}

func TestEnrollWithBadTokenIsRefused(t *testing.T) {
	t.Parallel()
	h, _ := testHandler(t, &fakeTokens{ok: false}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, nil, nil)

	rec := doJSON(handlerFunc(h.Enroll), http.MethodPost, "/api/v1/agent/enroll", agent.EnrollRequest{
		Token: "bad", NodeID: "node-1", CSR: csrFor(t, "node-1"),
	})
	if rec.Code == http.StatusOK {
		t.Fatal("enrollment succeeded with an invalid token")
	}
}

func TestEnrollWithMalformedCSRIsRejected(t *testing.T) {
	t.Parallel()
	h, _ := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, nil, nil)

	rec := doJSON(handlerFunc(h.Enroll), http.MethodPost, "/api/v1/agent/enroll", agent.EnrollRequest{
		Token: "atlas_enroll_x", NodeID: "node-1", CSR: "not-valid-base64!!!",
	})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

func TestRenewRequiresClientCertificate(t *testing.T) {
	t.Parallel()
	h, _ := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, nil, nil)

	rec := doJSON(handlerFunc(h.Renew), http.MethodPost, "/api/v1/agent/renew", agent.RenewRequest{CSR: csrFor(t, "node-1")})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestRenewIssuesFreshCertificateOverExistingIdentity(t *testing.T) {
	t.Parallel()
	tokens, creds, deny := &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}
	h, ca := testHandler(t, tokens, creds, deny, nil, nil)

	enrollRec := doJSON(handlerFunc(h.Enroll), http.MethodPost, "/api/v1/agent/enroll", agent.EnrollRequest{
		Token: "x", NodeID: "node-1", CSR: csrFor(t, "node-1"),
	})
	var enrolled agent.CertResponse
	if err := json.Unmarshal(enrollRec.Body.Bytes(), &enrolled); err != nil {
		t.Fatalf("decode: %v", err)
	}
	certDER, _ := base64.StdEncoding.DecodeString(enrolled.Certificate)
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse issued cert: %v", err)
	}

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(agent.RenewRequest{CSR: csrFor(t, "node-1")})
	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/renew", &buf), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Renew)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var renewed agent.CertResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &renewed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if renewed.Certificate == enrolled.Certificate {
		t.Error("Renew returned the same certificate")
	}
	_ = ca
}

func TestTelemetryRequiresClientCertificate(t *testing.T) {
	t.Parallel()
	h, _ := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, nil, nil)

	rec := doJSON(handlerFunc(h.Telemetry), http.MethodPost, "/api/v1/agent/telemetry", agent.TelemetryRequest{ProtocolVersion: agent.ProtocolVersion})
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestTelemetryRefusesDenylistedNode(t *testing.T) {
	t.Parallel()
	deny := &fakeDenylist{denied: map[string]bool{"node-1": true}}
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), deny, nil, nil)
	cert := issueCert(t, ca, "node-1")

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(agent.TelemetryRequest{ProtocolVersion: agent.ProtocolVersion})
	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", &buf), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Telemetry)(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func testEnvelope(nodeID string, sentAt time.Time) transport.Envelope {
	env := transport.NewEnvelope(
		transport.Origin{NodeID: nodeID, Hostname: "h"},
		collect.Batch{CollectorID: "system.cpu", Samples: []collect.Sample{
			{Metric: "m", Value: 1, Unit: collect.UnitCount, Kind: collect.KindGauge, Time: sentAt},
		}},
	)
	env.SentAt = sentAt
	return env
}

// C1: identity is bound to the verified peer certificate, never to the
// envelope's own claim. An envelope claiming a different node must be
// rejected and never reach the receiver.
func TestTelemetryRejectsIdentityMismatch(t *testing.T) {
	t.Parallel()
	recv := &recordingReceiver{kind: transport.KindMetrics}
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, recv, nil)
	cert := issueCert(t, ca, "node-1")

	env := testEnvelope("node-2", time.Now()) // claims a different node
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(agent.TelemetryRequest{ProtocolVersion: agent.ProtocolVersion, Envelopes: []transport.Envelope{env}})
	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", &buf), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Telemetry)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp agent.TelemetryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 0 || len(resp.Rejected) != 1 || resp.Rejected[0].Reason != "identity_mismatch" {
		t.Errorf("response = %+v, want one rejection reason identity_mismatch", resp)
	}
	if len(recv.received) != 0 {
		t.Error("a forged envelope reached the receiver")
	}
}

// C1, the positive case: the receiver must see the verified peer's node id
// even when the envelope's own Origin.NodeID already matched (defense in
// depth against a future bug that skips the check).
func TestTelemetryBindsOriginToVerifiedIdentity(t *testing.T) {
	t.Parallel()
	recv := &recordingReceiver{kind: transport.KindMetrics}
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, recv, nil)
	cert := issueCert(t, ca, "node-1")

	env := testEnvelope("", time.Now())
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(agent.TelemetryRequest{ProtocolVersion: agent.ProtocolVersion, Envelopes: []transport.Envelope{env}})
	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", &buf), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Telemetry)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(recv.received) != 1 {
		t.Fatalf("receiver got %d envelopes, want 1", len(recv.received))
	}
	if recv.received[0].Origin.NodeID != "node-1" {
		t.Errorf("Origin.NodeID = %q, want node-1 (from the verified certificate)", recv.received[0].Origin.NodeID)
	}
}

// H6: a sample timestamped far outside tolerance is rejected rather than
// trusted, and must not reach the receiver.
func TestTelemetryRejectsClockSkewBeyondTolerance(t *testing.T) {
	t.Parallel()
	recv := &recordingReceiver{kind: transport.KindMetrics}
	skew := &fakeClockSkew{}
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, recv, skew)
	cert := issueCert(t, ca, "node-1")

	env := testEnvelope("node-1", time.Now().Add(-2*time.Hour))
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(agent.TelemetryRequest{ProtocolVersion: agent.ProtocolVersion, Envelopes: []transport.Envelope{env}})
	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", &buf), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Telemetry)(rec, req)

	var resp agent.TelemetryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 0 || len(resp.Rejected) != 1 || resp.Rejected[0].Reason != "clock_skew" {
		t.Errorf("response = %+v, want one rejection reason clock_skew", resp)
	}
	if len(recv.received) != 0 {
		t.Error("a skewed envelope reached the receiver")
	}
	if skew.calls == 0 {
		t.Error("clock skew was not recorded even though it was measured")
	}
}

func TestTelemetryAcceptsWithinToleranceAndRecordsSkew(t *testing.T) {
	t.Parallel()
	recv := &recordingReceiver{kind: transport.KindMetrics}
	skew := &fakeClockSkew{}
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, recv, skew)
	cert := issueCert(t, ca, "node-1")

	env := testEnvelope("node-1", time.Now().Add(-2*time.Second))
	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(agent.TelemetryRequest{ProtocolVersion: agent.ProtocolVersion, Envelopes: []transport.Envelope{env}})
	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", &buf), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Telemetry)(rec, req)

	var resp agent.TelemetryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 || len(resp.Rejected) != 0 {
		t.Errorf("response = %+v, want 1 accepted, 0 rejected", resp)
	}
	if skew.nodeID != "node-1" {
		t.Errorf("skew recorded for node %q, want node-1", skew.nodeID)
	}
}

func TestTelemetryRejectsProtocolVersionMismatch(t *testing.T) {
	t.Parallel()
	recv := &recordingReceiver{kind: transport.KindMetrics}
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, recv, nil)
	cert := issueCert(t, ca, "node-1")

	for _, version := range []int{0, agent.ProtocolVersion + 1} {
		env := testEnvelope("node-1", time.Now())
		var buf bytes.Buffer
		_ = json.NewEncoder(&buf).Encode(agent.TelemetryRequest{ProtocolVersion: version, Envelopes: []transport.Envelope{env}})
		req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", &buf), cert)
		rec := httptest.NewRecorder()
		handlerFunc(h.Telemetry)(rec, req)

		if rec.Code != http.StatusBadRequest {
			t.Errorf("protocol version %d: status = %d, want 400", version, rec.Code)
		}
	}
	if len(recv.received) != 0 {
		t.Errorf("receiver got %d envelopes from version-mismatched batches, want 0", len(recv.received))
	}
}

func TestTelemetryRejectsInvalidEnvelope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		env  transport.Envelope
	}{
		{
			name: "payload missing required field",
			env: transport.NewEnvelope(
				transport.Origin{Hostname: "h"},
				collect.Batch{Samples: []collect.Sample{
					{Metric: "m", Value: 1, Unit: collect.UnitCount, Kind: collect.KindGauge, Time: time.Now()},
				}},
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recv := &recordingReceiver{kind: transport.KindMetrics}
			h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, recv, nil)
			cert := issueCert(t, ca, "node-1")

			env := tt.env
			env.SentAt = time.Now()
			var buf bytes.Buffer
			_ = json.NewEncoder(&buf).Encode(agent.TelemetryRequest{ProtocolVersion: agent.ProtocolVersion, Envelopes: []transport.Envelope{env}})
			req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", &buf), cert)
			rec := httptest.NewRecorder()
			handlerFunc(h.Telemetry)(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			var resp agent.TelemetryResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Accepted != 0 || len(resp.Rejected) != 1 || resp.Rejected[0].Reason != "invalid_envelope" {
				t.Errorf("response = %+v, want one rejection reason invalid_envelope", resp)
			}
			if len(recv.received) != 0 {
				t.Error("an invalid envelope reached the receiver")
			}
		})
	}
}

func TestTelemetryRejectsMalformedEnvelopeBatch(t *testing.T) {
	t.Parallel()
	recv := &recordingReceiver{kind: transport.KindMetrics}
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, recv, nil)
	cert := issueCert(t, ca, "node-1")

	body := `{"protocol_version":1,"envelopes":[{"id":"env-1","origin":{"node_id":"node-1"},"payload":null}]}`
	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/telemetry", bytes.NewBufferString(body)), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Telemetry)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a malformed envelope", rec.Code)
	}
	if len(recv.received) != 0 {
		t.Error("a malformed envelope reached the receiver")
	}
}

func TestHeartbeatRequiresClientCertificate(t *testing.T) {
	t.Parallel()
	h, _ := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, nil, nil)

	rec := doJSON(handlerFunc(h.Heartbeat), http.MethodPost, "/api/v1/agent/heartbeat", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

func TestHeartbeatAcknowledgesAuthenticatedAgent(t *testing.T) {
	t.Parallel()
	h, ca := testHandler(t, &fakeTokens{ok: true}, newFakeCredentials(), &fakeDenylist{denied: map[string]bool{}}, nil, nil)
	cert := issueCert(t, ca, "node-1")

	req := withPeerCert(httptest.NewRequest(http.MethodPost, "/api/v1/agent/heartbeat", nil), cert)
	rec := httptest.NewRecorder()
	handlerFunc(h.Heartbeat)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp agent.HeartbeatResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Directive != "ok" {
		t.Errorf("Directive = %q, want ok", resp.Directive)
	}
}

func handlerFunc(fn func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return httpx.Handler(fn).ServeHTTP
}
