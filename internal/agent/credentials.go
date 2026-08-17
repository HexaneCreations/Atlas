package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/pki"
)

// dialContextFunc overrides how the control plane is reached. Nil selects
// the default TCP dial. Set when the agent is configured for the libp2p POC
// transport (see docs/adr/0012-connect-by-identity.md) — the enrollment and
// renewal HTTP calls below are otherwise unchanged, they simply hand this
// function to http.Transport instead of letting it dial "tcp" itself.
type dialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type certResponse struct {
	Certificate     string    `json:"certificate"`
	CACertificate   string    `json:"ca_certificate,omitempty"`
	NotAfter        time.Time `json:"not_after"`
	ProtocolVersion int       `json:"protocol_version"`
}

// credentialHolder serves the agent's current client certificate to the TLS
// stack, and is updated in place on renewal so no HTTP client needs to be
// rebuilt.
type credentialHolder struct {
	mu   sync.RWMutex
	cert *tls.Certificate
	leaf *x509.Certificate
}

func (h *credentialHolder) set(cert *tls.Certificate, leaf *x509.Certificate) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cert, h.leaf = cert, leaf
}

func (h *credentialHolder) GetClientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.cert, nil
}

func (h *credentialHolder) current() *x509.Certificate {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.leaf
}

// bootstrapClient talks to the control plane before the agent has a client
// certificate: enrollment, and TOFU pinning of the CA when no bundle was
// configured out of band.
type bootstrapClient struct {
	baseURL string
	http    *http.Client
}

func newBootstrapClient(baseURL string, caPool *x509.CertPool, dial dialContextFunc) *bootstrapClient {
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13}
	if caPool != nil {
		tlsCfg.RootCAs = caPool
	} else {
		// No pinned CA yet: trust-on-first-use for the enrollment call only.
		// The CA returned by enroll is pinned for every call after this one.
		tlsCfg.InsecureSkipVerify = true //nolint:gosec
	}
	return &bootstrapClient{
		baseURL: baseURL,
		http:    &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, DialContext: dial}},
	}
}

func (c *bootstrapClient) enroll(ctx context.Context, token, nodeID string, csrDER []byte) (certResponse, error) {
	body, _ := json.Marshal(map[string]string{
		"token": token, "node_id": nodeID, "csr": base64.StdEncoding.EncodeToString(csrDER),
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/v1/agent/enroll", bytes.NewReader(body))
	if err != nil {
		return certResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return certResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return certResponse{}, fmt.Errorf("enroll: server returned %d", resp.StatusCode)
	}
	var out certResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return certResponse{}, err
	}
	return out, nil
}

func renew(ctx context.Context, baseURL string, tlsCfg *tls.Config, csrDER []byte, dial dialContextFunc) (certResponse, error) {
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: tlsCfg, DialContext: dial}}
	body, _ := json.Marshal(map[string]string{"csr": base64.StdEncoding.EncodeToString(csrDER)})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/v1/agent/renew", bytes.NewReader(body))
	if err != nil {
		return certResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return certResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return certResponse{}, fmt.Errorf("renew: server returned %d", resp.StatusCode)
	}
	var out certResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return certResponse{}, err
	}
	return out, nil
}

// loadCABundle reads a PEM-encoded CA certificate from path.
func loadCABundle(path string) (*x509.Certificate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("%s does not contain a PEM certificate", path)
	}
	return x509.ParseCertificate(block.Bytes)
}

func pinnedCAPath(dataDir string) string { return dataDir + "/ca-cert.pem" }

// loadPinnedCA reads back a CA this agent pinned via TOFU on a previous run.
func loadPinnedCA(dataDir string) (*x509.Certificate, error) {
	return loadCABundle(pinnedCAPath(dataDir))
}

// savePinnedCA persists a TOFU-pinned CA so it survives a restart. Without
// this, every restart with no ATLAS_AGENT_CA_BUNDLE configured would either
// refuse to start or — worse — repeat TOFU and trust whatever certificate is
// presented that time, which defeats the point of pinning.
func savePinnedCA(dataDir string, cert *x509.Certificate) error {
	out := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	return os.WriteFile(pinnedCAPath(dataDir), out, 0o644)
}

// bootstrap ensures the agent holds a valid certificate for one relationship,
// enrolling if none exists, and returns the CA certificate and a live
// credential holder kept current by that relationship's renewal loop.
//
// Both success paths below persist relCfg to relationship.json (a no-op if
// already present — see persistRelationshipConfig) — this is what makes a
// relationship's resolved connection config DataDir-authoritative from the
// moment it first has a usable credential, whether that credential was just
// freshly enrolled or was already on disk from before Phase 3 (legacy-layout
// adoption). A failure to persist is logged, not fatal: the credential is
// still valid either way, and the next successful bootstrap will retry the
// write.
func bootstrap(ctx context.Context, relCfg relationshipConfig, nodeID string, logger *slog.Logger, dial dialContextFunc) (*x509.Certificate, *credentialHolder, error) {
	var caCert *x509.Certificate
	if relCfg.CABundlePath != "" {
		cert, err := loadCABundle(relCfg.CABundlePath)
		if err != nil {
			return nil, nil, fmt.Errorf("load CA bundle: %w", err)
		}
		caCert = cert
	} else if pinned, err := loadPinnedCA(relCfg.dataDir); err == nil {
		// A CA pinned by TOFU on a previous enrollment (see below). Loading
		// it back here — rather than repeating TOFU on every restart — is
		// what makes the pin durable: an attacker who was not in position to
		// intercept the original enrollment cannot get trusted later just by
		// being present for a restart.
		caCert = pinned
	}

	holder := &credentialHolder{}

	leaf, key, err := pki.LoadLeaf(relCfg.dataDir)
	if err == nil {
		holder.set(pki.LeafTLSCertificate(leaf, key), leaf)
		if caCert == nil {
			return nil, nil, fmt.Errorf(
				"a certificate exists at %s but its pinned CA (%s) is missing; "+
					"set the CA bundle path or remove the data dir to re-enroll",
				relCfg.dataDir, pinnedCAPath(relCfg.dataDir))
		}
		if err := persistRelationshipConfig(relCfg); err != nil {
			logger.WarnContext(ctx, "could not persist relationship config", slog.String("relationship", relCfg.id), slog.String("error", err.Error()))
		}
		return caCert, holder, nil
	}

	logger.InfoContext(ctx, "no existing certificate found; enrolling", slog.String("relationship", relCfg.id))
	if relCfg.Token == "" {
		return nil, nil, fmt.Errorf("no certificate on disk and no enrollment token is set")
	}

	// Enrolling with no pinned or configured CA means trusting whatever
	// certificate answers, once, permanently. That is a defensible choice on
	// a trusted network and an indefensible default for a fleet, so it must
	// be made explicitly rather than fallen into.
	if caCert == nil && !relCfg.InsecureBootstrap {
		return nil, nil, fmt.Errorf(
			"refusing to enroll without a pinned CA: set a CA bundle path (verified bootstrap), "+
				"or set insecure bootstrap explicitly to trust the certificate presented on first contact (relationship %q)",
			relCfg.id)
	}

	var pool *x509.CertPool
	if caCert != nil {
		pool = x509.NewCertPool()
		pool.AddCert(caCert)
	}

	csrDER, csrKey, err := pki.NewCSR(nodeID)
	if err != nil {
		return nil, nil, err
	}

	bc := newBootstrapClient(relCfg.ControlPlaneURL, pool, dial)
	resp, err := bc.enroll(ctx, relCfg.Token, nodeID, csrDER)
	if err != nil {
		return nil, nil, fmt.Errorf("enroll: %w", err)
	}

	certDER, err := base64.StdEncoding.DecodeString(resp.Certificate)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, err
	}

	if caCert == nil {
		caDER, err := base64.StdEncoding.DecodeString(resp.CACertificate)
		if err != nil {
			return nil, nil, err
		}
		caCert, err = x509.ParseCertificate(caDER)
		if err != nil {
			return nil, nil, err
		}
		if err := savePinnedCA(relCfg.dataDir, caCert); err != nil {
			return nil, nil, fmt.Errorf("persist pinned CA: %w", err)
		}
		logger.WarnContext(ctx, "pinned the control plane's CA on first contact (TOFU); "+
			"set a CA bundle path for a verified bootstrap in production", slog.String("relationship", relCfg.id))
	}

	if err := pki.SaveLeaf(relCfg.dataDir, cert.Raw, csrKey); err != nil {
		return nil, nil, err
	}
	holder.set(pki.LeafTLSCertificate(cert, csrKey), cert)
	logger.InfoContext(ctx, "enrolled", slog.String("relationship", relCfg.id), slog.String("node_id", nodeID), slog.Time("not_after", cert.NotAfter))

	if err := persistRelationshipConfig(relCfg); err != nil {
		logger.WarnContext(ctx, "could not persist relationship config", slog.String("relationship", relCfg.id), slog.String("error", err.Error()))
	}

	return caCert, holder, nil
}

// bootstrapBackoffMin/Max/MaxElapsed bound [bootstrapWithRetry]: a transient
// relay outage, network blip, or Server unavailability during startup must
// not permanently exit the process, but a genuinely broken configuration
// (bad token, unreachable control plane) must still fail loudly rather than
// retry forever. Vars, not consts, so tests can shrink them instead of
// waiting out real minutes of backoff.
var (
	bootstrapBackoffMin = 2 * time.Second
	bootstrapBackoffMax = 30 * time.Second
	bootstrapMaxElapsed = 5 * time.Minute
)

// bootstrapWithRetry wraps [bootstrap] in a bounded exponential backoff:
// each failure is retried with jittered, doubling delay up to
// bootstrapBackoffMax, until either it succeeds, ctx is cancelled, or
// bootstrapMaxElapsed has passed since the first attempt — at which point
// the last error is returned so the caller can still exit fatally on a
// truly stuck configuration.
func bootstrapWithRetry(ctx context.Context, relCfg relationshipConfig, nodeID string, logger *slog.Logger, dial dialContextFunc) (*x509.Certificate, *credentialHolder, error) {
	deadline := time.Now().Add(bootstrapMaxElapsed)
	backoff := bootstrapBackoffMin

	for attempt := 1; ; attempt++ {
		caCert, holder, err := bootstrap(ctx, relCfg, nodeID, logger, dial)
		if err == nil {
			return caCert, holder, nil
		}

		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("bootstrap did not succeed within %s (%d attempts): %w", bootstrapMaxElapsed, attempt, err)
		}

		wait := jitter(backoff)
		logger.WarnContext(ctx, "bootstrap attempt failed, retrying",
			slog.Int("attempt", attempt), slog.Duration("backoff", wait), slog.String("error", err.Error()))

		select {
		case <-ctx.Done():
			return nil, nil, fmt.Errorf("bootstrap cancelled: %w", ctx.Err())
		case <-time.After(wait):
		}
		backoff = min(backoff*2, bootstrapBackoffMax)
	}
}

func jitter(d time.Duration) time.Duration {
	return time.Duration(rand.Int64N(int64(d) + 1))
}

// renewalLoop renews one relationship's certificate at [pki.RenewAt] of its
// lifetime, updating holder in place. Independent of every other
// relationship's loop — a failure here (unreachable control plane, denied
// node) is logged and retried on the next tick, and never touches another
// relationship's holder, caCert, or dataDir.
func renewalLoop(ctx context.Context, relCfg relationshipConfig, nodeID string, caCert *x509.Certificate, holder *credentialHolder, logger *slog.Logger, dial dialContextFunc) {
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	const checkInterval = time.Hour
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		leaf := holder.current()
		if leaf == nil || !fleet.ShouldRenew(leaf, time.Now()) {
			continue
		}

		csrDER, key, err := pki.NewCSR(nodeID)
		if err != nil {
			logger.ErrorContext(ctx, "could not generate a renewal CSR", slog.String("error", err.Error()))
			continue
		}

		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, GetClientCertificate: holder.GetClientCertificate}
		resp, err := renew(ctx, relCfg.ControlPlaneURL, tlsCfg, csrDER, dial)
		if err != nil {
			logger.ErrorContext(ctx, "certificate renewal failed", slog.String("error", err.Error()))
			continue
		}

		certDER, err := base64.StdEncoding.DecodeString(resp.Certificate)
		if err != nil {
			logger.ErrorContext(ctx, "could not decode renewed certificate", slog.String("error", err.Error()))
			continue
		}
		cert, err := x509.ParseCertificate(certDER)
		if err != nil {
			logger.ErrorContext(ctx, "could not parse renewed certificate", slog.String("error", err.Error()))
			continue
		}

		if err := pki.SaveLeaf(relCfg.dataDir, cert.Raw, key); err != nil {
			logger.ErrorContext(ctx, "could not persist renewed certificate", slog.String("error", err.Error()))
			continue
		}
		holder.set(pki.LeafTLSCertificate(cert, key), cert)
		logger.InfoContext(ctx, "certificate renewed", slog.Time("not_after", cert.NotAfter))
	}
}
