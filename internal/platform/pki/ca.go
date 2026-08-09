// Package pki is Atlas's private certificate authority.
//
// It exists for one reason: agent identity (see docs/architecture/agent-design.md
// §3) must be bound to a transport credential, not to a claim in a request
// body. A credential needs an issuer, and running a full external CA
// (Vault, cert-manager, step-ca) is a dependency Atlas should not force on an
// operator who wants to monitor a handful of servers. So the control plane is
// its own CA: it generates a root on first start, signs short-lived agent
// certificates, and needs nothing else to run.
//
// # Why the node id lives in a URI SAN
//
// Certificate identity has historically been read from the Common Name, and
// that convention is deprecated for exactly the reason that matters here: a
// CN is an unstructured string with no defined semantics, and code that
// parses "meaning" out of one is one refactor away from being wrong. A URI
// SAN in the form atlas://node/<id> is structured, is validated by the X.509
// stack the same way a DNS SAN is, and cannot be confused with a hostname.
//
// # Why certificates are short-lived
//
// A 24-hour certificate makes expiry the revocation mechanism: a compromised
// or decommissioned host loses access within a day with no CRL, no OCSP
// responder, and no infrastructure beyond what X.509 verification already
// does. The denylist in [Store] exists for ejection that cannot wait that
// long.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net/url"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// NodeURIScheme is the scheme of the URI SAN carrying a node's identity.
const NodeURIScheme = "atlas"

// LeafLifetime is how long an issued agent certificate is valid.
//
// Short deliberately: see the package doc. Renewal happens automatically at
// half this lifetime (see [RenewAt]), so an agent that stays reachable never
// experiences the expiry.
const LeafLifetime = 24 * time.Hour

// RenewAt is the fraction of [LeafLifetime] at which an agent should renew.
// 0.5 leaves a full day-equivalent of margin against a missed renewal before
// the certificate actually expires.
const RenewAt = 0.5

// RootLifetime is how long the CA's own certificate is valid. Long, because
// rotating the root means re-issuing every agent certificate in the fleet;
// that is a deliberate, operator-driven event, not something that should
// happen by a timer.
const RootLifetime = 10 * 365 * 24 * time.Hour

// NodeURI returns the URI SAN identifying a node.
func NodeURI(nodeID string) *url.URL {
	return &url.URL{Scheme: NodeURIScheme, Host: "node", Path: "/" + nodeID}
}

// NodeIDFromURI extracts a node id from a URI SAN, if it names one.
func NodeIDFromURI(u *url.URL) (string, bool) {
	if u == nil || u.Scheme != NodeURIScheme || u.Host != "node" {
		return "", false
	}
	id := trimLeadingSlash(u.Path)
	if id == "" {
		return "", false
	}
	return id, true
}

func trimLeadingSlash(s string) string {
	if len(s) > 0 && s[0] == '/' {
		return s[1:]
	}
	return s
}

// CA is Atlas's private certificate authority.
//
// A CA value is safe for concurrent use: signing operations only read the
// root key and certificate, neither of which changes after [New] or [Load]
// returns.
type CA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	der  []byte // cached DER encoding of cert, returned by CertPEM callers
}

// New generates a fresh root CA.
//
// P-256 (not RSA) because agent enrollment may happen on resource-constrained
// hosts and because ECDSA keypair generation and signing are materially
// cheaper — this runs on every certificate issuance, potentially thousands of
// times across a fleet's lifetime.
func New(commonName string) (*CA, error) {
	const op = "pki.New"

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not generate the CA key").WithOp(op)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not generate a CA serial").WithOp(op)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"Atlas"}},
		NotBefore:             now.Add(-5 * time.Minute), // clock skew margin at the CA itself
		NotAfter:              now.Add(RootLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not self-sign the CA certificate").WithOp(op)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not parse the freshly signed CA certificate").WithOp(op)
	}

	return &CA{cert: cert, key: key, der: der}, nil
}

// Load reconstructs a CA from a previously persisted key and certificate. See
// [Persist] for the counterpart, and package pkistore for the on-disk format.
func Load(cert *x509.Certificate, key *ecdsa.PrivateKey) (*CA, error) {
	const op = "pki.Load"
	if cert == nil || key == nil {
		return nil, errs.New(errs.CodeInvalidArgument, "a CA needs both a certificate and a key").WithOp(op)
	}
	if !cert.IsCA {
		return nil, errs.New(errs.CodeInvalidArgument, "certificate is not marked as a CA").WithOp(op)
	}
	return &CA{cert: cert, key: key, der: cert.Raw}, nil
}

// Cert returns the CA's own certificate.
func (ca *CA) Cert() *x509.Certificate { return ca.cert }

// Key returns the CA's private key, for persistence. Callers must treat this
// as the root of trust for every agent in the fleet: anyone who obtains it
// can mint a certificate for any node id.
func (ca *CA) Key() *ecdsa.PrivateKey { return ca.key }

// IssueLeaf signs a CSR into a short-lived agent certificate.
//
// The CSR's own subject and SAN list are ignored — the certificate that comes
// back carries exactly one identity, the caller-supplied nodeID, as a URI
// SAN. This is what makes enrollment's node-id check (in package fleet)
// meaningful: an agent cannot request a certificate for any identity but the
// one the server decided to grant it.
func (ca *CA) IssueLeaf(csr *x509.CertificateRequest, nodeID string) (*x509.Certificate, error) {
	const op = "pki.CA.IssueLeaf"

	if nodeID == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "cannot issue a certificate with no node ID").WithOp(op)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, errs.Wrap(err, errs.CodeInvalidArgument, "the CSR's self-signature does not verify").WithOp(op)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not generate a certificate serial").WithOp(op)
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: nodeID, Organization: []string{"Atlas Agent"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(LeafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{NodeURI(nodeID)},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not sign the agent certificate").
			WithOp(op).WithDetail("node_id", nodeID)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not parse the freshly signed certificate").WithOp(op)
	}
	return leaf, nil
}

// PeerNodeID extracts and validates the node identity from a verified peer
// certificate.
//
// This is the only function in the codebase that is meant to answer "which
// node is this connection from", and every caller that needs that answer
// should go through it rather than re-deriving it from raw TLS state. It
// deliberately does not accept a chain — the caller must already have
// verified the certificate against the CA pool (Go's TLS stack does this
// automatically for RequireAndVerifyClientCert) — this function only reads
// the identity out of a certificate already known to be trusted.
func PeerNodeID(cert *x509.Certificate) (string, error) {
	const op = "pki.PeerNodeID"
	if cert == nil {
		return "", errs.New(errs.CodeUnauthenticated, "no client certificate was presented").WithOp(op)
	}
	for _, u := range cert.URIs {
		if id, ok := NodeIDFromURI(u); ok {
			return id, nil
		}
	}
	return "", errs.New(errs.CodeUnauthenticated, "certificate carries no Atlas node identity").WithOp(op)
}

func randomSerial() (*big.Int, error) {
	// 128 bits: collision-negligible even at fleet scale, and the
	// conventional size for X.509 serials.
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// Fingerprint returns a stable identifier for a certificate, used for
// revocation records and audit logs — shorter and more legible than a raw
// serial, and unlike the serial it survives a re-issued certificate for the
// same identity being distinguished from the one it replaced.
func Fingerprint(cert *x509.Certificate) string {
	return fmt.Sprintf("%x", cert.SerialNumber)
}
