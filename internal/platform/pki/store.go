package pki

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"

	"github.com/hexane/atlas/internal/platform/errs"
)

// pemBlockCert and pemBlockKey name the PEM block types written and read
// here. EC PRIVATE KEY (SEC1) rather than PRIVATE KEY (PKCS8) is used for the
// key blocks because it is unambiguous about the curve without an embedded
// algorithm identifier, keeping the on-disk format simple to eyeball.
const (
	pemBlockCert = "CERTIFICATE"
	pemBlockKey  = "EC PRIVATE KEY"
)

// LoadOrCreateCA loads a CA persisted at dir, or generates and persists a new
// one if none exists.
//
// This is the only place a root CA is created. Called once, at control-plane
// startup, so the generation path is exercised on every fresh install and the
// load path on every restart — there is no separate "first run" mode to drift
// out of sync with normal operation.
func LoadOrCreateCA(dir, commonName string) (*CA, error) {
	const op = "pki.LoadOrCreateCA"

	certPath := filepath.Join(dir, "ca-cert.pem")
	keyPath := filepath.Join(dir, "ca-key.pem")

	cert, key, err := readCertAndKey(certPath, keyPath)
	switch {
	case err == nil:
		return Load(cert, key)
	case !os.IsNotExist(err):
		return nil, errs.Wrap(err, errs.CodeInternal, "could not read the existing CA").WithOp(op)
	}

	ca, err := New(commonName)
	if err != nil {
		return nil, err
	}
	if err := writeCertAndKey(dir, certPath, keyPath, ca.Cert().Raw, ca.Key()); err != nil {
		return nil, errs.Wrap(err, errs.CodeInternal, "could not persist the new CA").WithOp(op)
	}
	return ca, nil
}

// LoadLeaf reads a previously issued agent certificate and key from dir, for
// an agent resuming after a restart. Returns os.ErrNotExist (wrapped) if
// nothing is there yet — the caller's response is to enroll.
func LoadLeaf(dir string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	return readCertAndKey(
		filepath.Join(dir, "agent-cert.pem"),
		filepath.Join(dir, "agent-key.pem"),
	)
}

// SaveLeaf persists an issued agent certificate and key to dir, replacing
// anything already there. Called after enrollment and after every renewal.
func SaveLeaf(dir string, certDER []byte, key *ecdsa.PrivateKey) error {
	const op = "pki.SaveLeaf"
	if err := writeCertAndKey(dir,
		filepath.Join(dir, "agent-cert.pem"),
		filepath.Join(dir, "agent-key.pem"),
		certDER, key,
	); err != nil {
		return errs.Wrap(err, errs.CodeInternal, "could not persist the agent certificate").WithOp(op)
	}
	return nil
}

func readCertAndKey(certPath, keyPath string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}

	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil || certBlock.Type != pemBlockCert {
		return nil, nil, errs.New(errs.CodeInternal, "%s does not contain a valid certificate", certPath)
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "could not parse %s", certPath)
	}

	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil || keyBlock.Type != pemBlockKey {
		return nil, nil, errs.New(errs.CodeInternal, "%s does not contain a valid EC private key", keyPath)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, errs.Wrap(err, errs.CodeInternal, "could not parse %s", keyPath)
	}

	return cert, key, nil
}

func writeCertAndKey(dir, certPath, keyPath string, certDER []byte, key *ecdsa.PrivateKey) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}

	certOut := pem.EncodeToMemory(&pem.Block{Type: pemBlockCert, Bytes: certDER})
	if err := os.WriteFile(certPath, certOut, 0o644); err != nil {
		return err
	}

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyOut := pem.EncodeToMemory(&pem.Block{Type: pemBlockKey, Bytes: keyDER})
	// 0600: this is the private key backing the node's identity. It never
	// leaves the host — see docs/architecture/agent-design.md §4 — and a
	// world- or group-readable key file would make that guarantee false.
	if err := os.WriteFile(keyPath, keyOut, 0o600); err != nil {
		return err
	}
	return nil
}
