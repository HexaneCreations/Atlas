package ports

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"time"
)

// Certificate is what a TLS handshake reveals about a listening service's
// certificate — the same information a browser's padlock icon is built from,
// read directly rather than through a browser.
type Certificate struct {
	Subject string
	Issuer  string
	// SANs are the DNS names the certificate is valid for. A certificate
	// presented on a port that does not match any of them is misconfigured,
	// even while it remains technically unexpired.
	SANs []string

	NotBefore time.Time
	NotAfter  time.Time
	// SelfSigned means the certificate's own key signed it — there is no
	// separate issuing authority to have revoked or expired independently.
	// Extremely common for internal services, and worth knowing rather than
	// hiding: it changes what "trusted" means for this endpoint, not whether
	// expiry tracking is useful.
	SelfSigned bool
}

// DaysUntilExpiry returns how many days remain until NotAfter. Negative once
// the certificate has expired.
func (c Certificate) DaysUntilExpiry() int {
	return int(time.Until(c.NotAfter).Hours() / 24)
}

// Expired reports whether the certificate's validity window has passed.
func (c Certificate) Expired() bool {
	return !c.NotAfter.IsZero() && time.Now().After(c.NotAfter)
}

// probeCertificate attempts a TLS handshake against address and reports the
// leaf certificate presented, if the peer speaks TLS at all.
//
// A plaintext service simply fails the handshake, which is treated as "not
// TLS" rather than an error — probing is inherently a guess about what is
// listening on a port Atlas did not configure and does not otherwise know.
//
// Chain verification is deliberately skipped. Atlas is reporting what a
// service presents, not vouching for it: an internal certificate signed by a
// private CA, or self-signed entirely, is the common case for
// service-to-service traffic and exactly where expiry tracking matters most,
// since nothing else is watching it. A monitoring platform that refused to
// look at untrusted certificates would be blind precisely where it is needed.
func probeCertificate(ctx context.Context, address string, timeout time.Duration) (Certificate, bool) {
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	dialer := tls.Dialer{
		Config: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // reporting presented certs, not authenticating them — see doc comment
	}
	conn, err := dialer.DialContext(dialCtx, "tcp", address)
	if err != nil {
		return Certificate{}, false
	}
	defer conn.Close()

	tlsConn, ok := conn.(*tls.Conn)
	if !ok {
		return Certificate{}, false
	}

	certs := tlsConn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return Certificate{}, false
	}
	leaf := certs[0]

	return Certificate{
		Subject:    leaf.Subject.CommonName,
		Issuer:     leaf.Issuer.CommonName,
		SANs:       leaf.DNSNames,
		NotBefore:  leaf.NotBefore,
		NotAfter:   leaf.NotAfter,
		SelfSigned: isSelfSigned(leaf),
	}, true
}

// isSelfSigned reports whether cert's own public key validates its signature
// — there is no separate issuer to check against.
func isSelfSigned(cert *x509.Certificate) bool {
	return cert.CheckSignatureFrom(cert) == nil
}
