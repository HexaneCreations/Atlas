package pki_test

import (
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/pki"
)

func testCA(t *testing.T) *pki.CA {
	t.Helper()
	ca, err := pki.New("test-ca")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ca
}

func TestCAIssuesLeafWithNodeURISAN(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	csrDER, _, err := pki.NewCSR("node-abc")
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	csr, err := pki.ParseCSR(csrDER)
	if err != nil {
		t.Fatalf("ParseCSR: %v", err)
	}

	leaf, err := ca.IssueLeaf(csr, "node-abc")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	if len(leaf.URIs) != 1 {
		t.Fatalf("leaf has %d URI SANs, want 1", len(leaf.URIs))
	}
	if got := leaf.URIs[0].String(); got != "atlas://node/node-abc" {
		t.Errorf("URI SAN = %q, want atlas://node/node-abc", got)
	}
}

// The certificate that comes back must name the identity the server decided
// to grant, never one smuggled in through the CSR's own subject or SAN list.
// Otherwise a held token would let an agent request a certificate for any
// node id it likes.
func TestIssueLeafIgnoresCSRSubject(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	csrDER, _, err := pki.NewCSR("node-claims-to-be-someone-else")
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	csr, err := pki.ParseCSR(csrDER)
	if err != nil {
		t.Fatalf("ParseCSR: %v", err)
	}

	leaf, err := ca.IssueLeaf(csr, "node-real-identity")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	id, ok := pki.NodeIDFromURI(leaf.URIs[0])
	if !ok || id != "node-real-identity" {
		t.Errorf("issued identity = %q, want node-real-identity (the server's decision, not the CSR's claim)", id)
	}
}

func TestPeerNodeIDRoundTrips(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	csrDER, _, _ := pki.NewCSR("node-xyz")
	csr, _ := pki.ParseCSR(csrDER)
	leaf, err := ca.IssueLeaf(csr, "node-xyz")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	id, err := pki.PeerNodeID(leaf)
	if err != nil {
		t.Fatalf("PeerNodeID: %v", err)
	}
	if id != "node-xyz" {
		t.Errorf("PeerNodeID = %q, want node-xyz", id)
	}
}

func TestPeerNodeIDRejectsNilCertificate(t *testing.T) {
	t.Parallel()

	_, err := pki.PeerNodeID(nil)
	if err == nil {
		t.Fatal("PeerNodeID(nil) succeeded")
	}
	if errs.CodeOf(err) != errs.CodeUnauthenticated {
		t.Errorf("code = %q, want unauthenticated", errs.CodeOf(err))
	}
}

// A certificate with no Atlas URI SAN at all — say, a certificate from a
// wholly different system that happened to chain to the same pool — must not
// resolve to any node identity.
func TestPeerNodeIDRejectsCertificateWithoutNodeURI(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	_, err := pki.PeerNodeID(ca.Cert()) // the CA's own cert carries no node URI
	if err == nil {
		t.Fatal("PeerNodeID accepted a certificate with no node identity")
	}
	if errs.CodeOf(err) != errs.CodeUnauthenticated {
		t.Errorf("code = %q, want unauthenticated", errs.CodeOf(err))
	}
}

func TestNodeIDFromURIRejectsForeignSchemes(t *testing.T) {
	t.Parallel()

	cases := []*url.URL{
		{Scheme: "https", Host: "node", Path: "/x"},
		{Scheme: "atlas", Host: "tenant", Path: "/x"},
		{Scheme: "atlas", Host: "node", Path: "/"},
		nil,
	}
	for _, u := range cases {
		if _, ok := pki.NodeIDFromURI(u); ok {
			t.Errorf("NodeIDFromURI(%v) = ok, want rejected", u)
		}
	}
}

// The server leaf's cryptographic identity must always be the CA's own
// fingerprint — never an operator-assigned label — and CommonName stays
// informational only (see ControlPlaneURI's doc).
func TestNewServerLeafHasControlPlaneURISAN(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	serverCert, err := pki.NewServerLeaf(ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(serverCert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}

	if len(leaf.URIs) != 1 {
		t.Fatalf("server leaf has %d URI SANs, want 1", len(leaf.URIs))
	}
	wantID := pki.Fingerprint(ca.Cert())
	if got := leaf.URIs[0].String(); got != "atlas://control-plane/"+wantID {
		t.Errorf("URI SAN = %q, want atlas://control-plane/%s", got, wantID)
	}
	if leaf.Subject.CommonName != pki.ControlPlaneCommonName {
		t.Errorf("CommonName = %q, want %q (informational only, unchanged)", leaf.Subject.CommonName, pki.ControlPlaneCommonName)
	}

	gotID, err := pki.ControlPlaneID(leaf)
	if err != nil {
		t.Fatalf("ControlPlaneID: %v", err)
	}
	if gotID != wantID {
		t.Errorf("ControlPlaneID = %q, want %q", gotID, wantID)
	}
}

func TestControlPlaneIDFromURIRejectsForeignSchemes(t *testing.T) {
	t.Parallel()

	cases := []*url.URL{
		{Scheme: "https", Host: "control-plane", Path: "/x"},
		{Scheme: "atlas", Host: "node", Path: "/x"}, // a node identity, not a control plane's
		{Scheme: "atlas", Host: "control-plane", Path: "/"},
		nil,
	}
	for _, u := range cases {
		if _, ok := pki.ControlPlaneIDFromURI(u); ok {
			t.Errorf("ControlPlaneIDFromURI(%v) = ok, want rejected", u)
		}
	}
}

// The control plane's identity must survive its own server leaf being
// reissued (renewal) — it is derived from the CA, not the leaf's own serial.
func TestControlPlaneIDStableAcrossServerLeafReissuance(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	leaf1, err := pki.NewServerLeaf(ca, nil)
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	leaf2, err := pki.NewServerLeaf(ca, nil)
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	cert1, _ := x509.ParseCertificate(leaf1.Certificate[0])
	cert2, _ := x509.ParseCertificate(leaf2.Certificate[0])

	id1, err := pki.ControlPlaneID(cert1)
	if err != nil {
		t.Fatalf("ControlPlaneID: %v", err)
	}
	id2, err := pki.ControlPlaneID(cert2)
	if err != nil {
		t.Fatalf("ControlPlaneID: %v", err)
	}
	if id1 != id2 {
		t.Errorf("control plane identity changed across reissuance: %q vs %q", id1, id2)
	}
}

// Two distinct control planes (distinct CAs) must never resolve to the same
// identity — this is what a multi-Control-Plane Agent relies on to tell them
// apart.
func TestControlPlaneIDDiffersAcrossCAs(t *testing.T) {
	t.Parallel()

	caA := testCA(t)
	caB := testCA(t)
	leafA, err := pki.NewServerLeaf(caA, nil)
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	leafB, err := pki.NewServerLeaf(caB, nil)
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	certA, _ := x509.ParseCertificate(leafA.Certificate[0])
	certB, _ := x509.ParseCertificate(leafB.Certificate[0])

	idA, err := pki.ControlPlaneID(certA)
	if err != nil {
		t.Fatalf("ControlPlaneID: %v", err)
	}
	idB, err := pki.ControlPlaneID(certB)
	if err != nil {
		t.Fatalf("ControlPlaneID: %v", err)
	}
	if idA == idB {
		t.Error("two distinct CAs produced the same control-plane identity")
	}
}

func TestControlPlaneIDRejectsCertificateWithoutControlPlaneURI(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	csrDER, _, _ := pki.NewCSR("node-not-a-cp")
	csr, _ := pki.ParseCSR(csrDER)
	agentLeaf, err := ca.IssueLeaf(csr, "node-not-a-cp") // a node leaf, not a control-plane leaf
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	if _, err := pki.ControlPlaneID(agentLeaf); err == nil {
		t.Fatal("ControlPlaneID accepted a node certificate")
	}
}

func TestLeafLifetimeIsShort(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	csrDER, _, _ := pki.NewCSR("node-short")
	csr, _ := pki.ParseCSR(csrDER)
	leaf, err := ca.IssueLeaf(csr, "node-short")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	lifetime := leaf.NotAfter.Sub(leaf.NotBefore)
	// Short expiry is the revocation mechanism (see the package doc and
	// docs/architecture/agent-design.md §3); a leaf lasting weeks would
	// silently defeat it.
	if lifetime > 25*time.Hour {
		t.Errorf("leaf lifetime = %s, want approximately %s", lifetime, pki.LeafLifetime)
	}
}

func TestIssueLeafRejectsUnsignedCSR(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	// A CertificateRequest built without CreateCertificateRequest has no
	// valid self-signature.
	tampered := &x509.CertificateRequest{}
	if _, err := ca.IssueLeaf(tampered, "node-x"); err == nil {
		t.Fatal("IssueLeaf accepted a CSR with no valid signature")
	}
}

func TestLoadRejectsNonCACertificate(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	csrDER, key, _ := pki.NewCSR("node-not-a-ca")
	csr, _ := pki.ParseCSR(csrDER)
	leaf, err := ca.IssueLeaf(csr, "node-not-a-ca")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	if _, err := pki.Load(leaf, key); err == nil {
		t.Fatal("Load accepted a non-CA certificate")
	}
}

// End-to-end: an agent's leaf certificate, presented over real TLS, must
// verify against the CA pool and expose the node identity as a URI SAN a
// server can read from the connection state — the exact path
// internal/api/agent uses.
func TestLeafVerifiesOverRealTLSHandshake(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	serverCert, err := pki.NewServerLeaf(ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}

	csrDER, key, _ := pki.NewCSR("node-handshake")
	csr, _ := pki.ParseCSR(csrDER)
	agentCert, err := ca.IssueLeaf(csr, "node-handshake")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}
	agentTLSCert := pki.LeafTLSCertificate(agentCert, key)

	var gotPeerID string
	serverCfg := pki.ServerTLSConfig(serverCert, ca)
	serverCfg.ClientAuth = tls.RequireAndVerifyClientCert // strict for this test
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	defer ln.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		tlsConn, ok := conn.(*tls.Conn)
		if !ok {
			return
		}
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		state := tlsConn.ConnectionState()
		if len(state.PeerCertificates) == 0 {
			return
		}
		id, err := pki.PeerNodeID(state.PeerCertificates[0])
		if err != nil {
			return
		}
		gotPeerID = id
	}()

	clientCfg := pki.ClientTLSConfig(ca.Cert(), agentTLSCert)
	conn, err := tls.Dial("tcp", ln.Addr().String(), clientCfg)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	if err := conn.Handshake(); err != nil {
		t.Fatalf("client handshake: %v", err)
	}

	<-done
	if gotPeerID != "node-handshake" {
		t.Errorf("server resolved peer identity %q, want node-handshake", gotPeerID)
	}
}

func TestSaveAndLoadLeafRoundTrips(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	ca := testCA(t)
	csrDER, key, _ := pki.NewCSR("node-persist")
	csr, _ := pki.ParseCSR(csrDER)
	leaf, err := ca.IssueLeaf(csr, "node-persist")
	if err != nil {
		t.Fatalf("IssueLeaf: %v", err)
	}

	if err := pki.SaveLeaf(dir, leaf.Raw, key); err != nil {
		t.Fatalf("SaveLeaf: %v", err)
	}

	gotCert, gotKey, err := pki.LoadLeaf(dir)
	if err != nil {
		t.Fatalf("LoadLeaf: %v", err)
	}
	if gotCert.SerialNumber.Cmp(leaf.SerialNumber) != 0 {
		t.Error("loaded certificate has a different serial than the one saved")
	}
	if gotKey.D.Cmp(key.D) != 0 {
		t.Error("loaded key does not match the one saved")
	}
}

func TestLoadOrCreateCAIsStableAcrossRestarts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	first, err := pki.LoadOrCreateCA(dir, "atlas-test")
	if err != nil {
		t.Fatalf("LoadOrCreateCA (create): %v", err)
	}

	second, err := pki.LoadOrCreateCA(dir, "atlas-test")
	if err != nil {
		t.Fatalf("LoadOrCreateCA (load): %v", err)
	}

	if first.Cert().SerialNumber.Cmp(second.Cert().SerialNumber) != 0 {
		t.Error("LoadOrCreateCA generated a new root on the second call instead of loading the persisted one")
	}
}

func TestFingerprintIsStablePerCertificate(t *testing.T) {
	t.Parallel()

	ca := testCA(t)
	csrDER, _, _ := pki.NewCSR("node-fp")
	csr, _ := pki.ParseCSR(csrDER)
	a, _ := ca.IssueLeaf(csr, "node-fp")
	b, _ := ca.IssueLeaf(csr, "node-fp") // re-issued: different serial

	if pki.Fingerprint(a) == pki.Fingerprint(b) {
		t.Error("two distinct certificates produced the same fingerprint")
	}
	if pki.Fingerprint(a) != pki.Fingerprint(a) {
		t.Error("Fingerprint is not stable for the same certificate")
	}
}
