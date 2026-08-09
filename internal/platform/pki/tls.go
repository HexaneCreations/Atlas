package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
)

// ServerLeafLifetime is how long the control plane's own TLS server
// certificate is valid. Much longer than an agent's — see [LeafLifetime] —
// because there is no equivalent of automatic renewal for the listener
// without either an outage or a hot-reload mechanism (deferred; see
// docs/architecture/agent-readiness-review.md M2). An operator restarting the
// control plane periodically is enough to stay ahead of this.
const ServerLeafLifetime = 397 * 24 * time.Hour // just under the CA/Browser Forum's public-cert ceiling, out of habit

// NewServerLeaf mints the control plane's own TLS server certificate, signed
// by ca, valid for the given hosts (DNS names or IP addresses).
//
// Agents verify this certificate against the CA bundle they received at
// enrollment time (see docs/architecture/agent-design.md §4) rather than
// against the public Web PKI, so hosts only need to be accurate for this
// fleet's deployment — there is no external CA to satisfy.
func NewServerLeaf(ca *CA, hosts []string) (tls.Certificate, error) {
	const op = "pki.NewServerLeaf"

	if len(hosts) == 0 {
		hosts = []string{"localhost", "127.0.0.1"}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, errs.Wrap(err, errs.CodeInternal, "could not generate the server key").WithOp(op)
	}

	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, errs.Wrap(err, errs.CodeInternal, "could not generate a certificate serial").WithOp(op)
	}

	var dnsNames []string
	var ips []net.IP
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			ips = append(ips, ip)
		} else {
			dnsNames = append(dnsNames, h)
		}
	}

	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "atlas-control-plane", Organization: []string{"Atlas"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(ServerLeafLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert(), &key.PublicKey, ca.Key())
	if err != nil {
		return tls.Certificate{}, errs.Wrap(err, errs.CodeInternal, "could not sign the server certificate").WithOp(op)
	}

	// The chain includes the CA certificate alongside the leaf so a client
	// that only pins the CA (rather than trusting it as a root) can still
	// build a full path during verification.
	return tls.Certificate{
		Certificate: [][]byte{der, ca.Cert().Raw},
		PrivateKey:  key,
	}, nil
}

// ServerTLSConfig builds the TLS configuration for the agent-facing listener.
//
// ClientAuth is deliberately VerifyClientCertIfGiven rather than
// RequireAndVerifyClientCert: the enrollment endpoint is reached by agents
// that do not yet have a certificate, and TLS itself cannot distinguish "no
// cert because this is enroll" from "no cert because something is wrong" —
// only the handler, which knows the route, can. Every other agent handler
// therefore checks for a verified peer certificate itself (see
// internal/api/agent) rather than relying on the listener to have rejected
// the connection already.
func ServerTLSConfig(serverCert tls.Certificate, ca *CA) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert())

	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
	}
}

// LeafTLSCertificate adapts a parsed certificate and key — as returned by
// [LoadLeaf] or produced fresh by enrollment — into the form [tls.Config]
// wants.
func LeafTLSCertificate(cert *x509.Certificate, key *ecdsa.PrivateKey) *tls.Certificate {
	return &tls.Certificate{
		Certificate: [][]byte{cert.Raw},
		PrivateKey:  key,
		Leaf:        cert,
	}
}

// ClientTLSConfig builds the TLS configuration an agent uses to reach the
// control plane.
//
// leaf is nil before enrollment — the request carries no client certificate,
// and the enrollment endpoint does not require one. After enrollment every
// call presents leaf, which is how C1 (identity bound to the credential) is
// enforced on the server side.
func ClientTLSConfig(caCert *x509.Certificate, leaf *tls.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
	}
	if leaf != nil {
		cfg.Certificates = []tls.Certificate{*leaf}
	}
	return cfg
}
