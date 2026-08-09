package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"

	"github.com/hexane/atlas/internal/platform/errs"
)

// NewCSR generates a fresh keypair and a certificate signing request for it.
//
// This runs on the agent side, before it has ever spoken to the control
// plane. The private key it generates is returned alongside the request and
// must be kept by the caller — see docs/architecture/agent-design.md §4: "the
// private key never leaves the host and is never transmitted." Only the
// request (a public key plus a signature over it) crosses the network.
func NewCSR(nodeID string) (der []byte, key *ecdsa.PrivateKey, err error) {
	const op = "pki.NewCSR"

	key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "could not generate an agent key").WithOp(op)
	}

	template := &x509.CertificateRequest{
		Subject:            pkix.Name{CommonName: nodeID},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
	}
	der, err = x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "could not create the CSR").WithOp(op)
	}
	return der, key, nil
}

// ParseCSR decodes a DER-encoded certificate signing request, as received by
// the enrollment endpoint.
func ParseCSR(der []byte) (*x509.CertificateRequest, error) {
	const op = "pki.ParseCSR"
	csr, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInvalidArgument, "could not parse the certificate request").WithOp(op)
	}
	return csr, nil
}
