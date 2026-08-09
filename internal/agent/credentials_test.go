package agent

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	apiagent "github.com/hexane/atlas/internal/api/agent"
	corefleet "github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/pki"
)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

type fakeTokens struct{}

func (fakeTokens) Redeem(context.Context, string, net.IP, time.Time) (corefleet.TokenGrant, error) {
	return corefleet.TokenGrant{Environment: "test"}, nil
}

type fakeCredentials struct {
	byNode map[string]*corefleet.Credential
}

func newFakeCredentials() *fakeCredentials {
	return &fakeCredentials{byNode: map[string]*corefleet.Credential{}}
}

func (f *fakeCredentials) ActiveCredential(_ context.Context, nodeID string, now time.Time) (*corefleet.Credential, error) {
	c, ok := f.byNode[nodeID]
	if !ok || now.After(c.ExpiresAt) {
		return nil, nil
	}
	return c, nil
}

func (f *fakeCredentials) RecordIssuance(_ context.Context, cred corefleet.Credential) error {
	c := cred
	f.byNode[cred.NodeID] = &c
	return nil
}

func (f *fakeCredentials) Revoke(context.Context, string, string, time.Time) error { return nil }

type fakeDenylist struct{}

func (fakeDenylist) IsDenied(context.Context, string) (bool, error) { return false, nil }

// testControlPlane starts a real mTLS-capable agent server, signed by a real
// CA, exactly as internal/app/fleet.go wires one — so bootstrap() exercises
// the genuine HTTPS + enrollment path rather than a mock.
func testControlPlane(t *testing.T) (url string, ca *pki.CA) {
	t.Helper()

	ca, err := pki.New("test-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	router := transport.NewRouter()
	handler := apiagent.NewHandler(apiagent.Deps{
		CA:       ca,
		Enroller: corefleet.NewEnroller(ca, fakeTokens{}, newFakeCredentials(), fakeDenylist{}),
		Router:   router,
	})
	mux := http.NewServeMux()
	handler.Mount(mux)

	srv := httptest.NewUnstartedServer(mux)
	serverLeaf, err := pki.NewServerLeaf(ca, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("NewServerLeaf: %v", err)
	}
	srv.TLS = pki.ServerTLSConfig(serverLeaf, ca)
	srv.TLS.ClientAuth = tls.VerifyClientCertIfGiven
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return srv.URL, ca
}

func testConfig(t *testing.T, controlPlaneURL string) Config {
	t.Helper()
	return Config{
		ControlPlaneURL: controlPlaneURL,
		Token:           "atlas_enroll_test",
		DataDir:         t.TempDir(),
	}
}

func TestBootstrapEnrollsAndPersistsPinnedCA(t *testing.T) {
	t.Parallel()

	url, _ := testControlPlane(t)
	cfg := testConfig(t, url)

	caCert, holder, err := bootstrap(context.Background(), cfg, "node-1", discardLogger(), nil)
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if caCert == nil || holder.current() == nil {
		t.Fatal("bootstrap did not return a usable CA and credential")
	}

	if _, err := os.Stat(pinnedCAPath(cfg.DataDir)); err != nil {
		t.Fatalf("TOFU-pinned CA was not persisted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "agent-cert.pem")); err != nil {
		t.Fatalf("agent certificate was not persisted: %v", err)
	}
}

// This is the regression: a restart with no ATLAS_AGENT_CA_BUNDLE configured
// must reuse the CA pinned during the original TOFU enrollment, not fail and
// not silently re-pin whatever certificate a server presents this time.
func TestBootstrapReusesPinnedCAOnRestartWithoutExplicitBundle(t *testing.T) {
	t.Parallel()

	url, _ := testControlPlane(t)
	cfg := testConfig(t, url)

	firstCA, _, err := bootstrap(context.Background(), cfg, "node-1", discardLogger(), nil)
	if err != nil {
		t.Fatalf("first bootstrap (enroll): %v", err)
	}

	// Simulate a restart: same data dir, no token needed, no CA bundle
	// configured — this must not require enrolling again.
	cfg.Token = ""
	secondCA, holder, err := bootstrap(context.Background(), cfg, "node-1", discardLogger(), nil)
	if err != nil {
		t.Fatalf("second bootstrap (restart) failed: %v — this is the exact crash-loop caught in packaging verification", err)
	}
	if holder.current() == nil {
		t.Fatal("restart produced no usable credential")
	}
	if firstCA.SerialNumber.Cmp(secondCA.SerialNumber) != 0 {
		t.Error("restart loaded a different CA than the one pinned at enrollment")
	}
}

func TestBootstrapPrefersExplicitCABundleOverPin(t *testing.T) {
	t.Parallel()

	url, ca := testControlPlane(t)
	cfg := testConfig(t, url)

	if _, _, err := bootstrap(context.Background(), cfg, "node-1", discardLogger(), nil); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}

	bundlePath := filepath.Join(cfg.DataDir, "explicit-ca.pem")
	if err := savePinnedCA(cfg.DataDir, ca.Cert()); err != nil {
		t.Fatalf("write explicit bundle fixture: %v", err)
	}
	if err := os.Rename(pinnedCAPath(cfg.DataDir), bundlePath); err != nil {
		t.Fatalf("rename: %v", err)
	}
	cfg.CABundlePath = bundlePath
	cfg.Token = ""

	gotCA, _, err := bootstrap(context.Background(), cfg, "node-1", discardLogger(), nil)
	if err != nil {
		t.Fatalf("bootstrap with explicit bundle: %v", err)
	}
	if gotCA.SerialNumber.Cmp(ca.Cert().SerialNumber) != 0 {
		t.Error("did not use the explicitly configured CA bundle")
	}
}

// A certificate with no way to verify the server at all — no explicit
// bundle, no persisted pin — must fail loudly rather than pick either an
// insecure or an arbitrary trust decision.
func TestBootstrapFailsClearlyWhenPinIsMissing(t *testing.T) {
	t.Parallel()

	url, _ := testControlPlane(t)
	cfg := testConfig(t, url)

	if _, _, err := bootstrap(context.Background(), cfg, "node-1", discardLogger(), nil); err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if err := os.Remove(pinnedCAPath(cfg.DataDir)); err != nil {
		t.Fatalf("remove pinned CA fixture: %v", err)
	}
	cfg.Token = ""

	_, _, err := bootstrap(context.Background(), cfg, "node-1", discardLogger(), nil)
	if err == nil {
		t.Fatal("bootstrap succeeded with a certificate but no way to verify the server")
	}
}
