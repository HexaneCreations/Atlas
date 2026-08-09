package fleet_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/pki"
)

// fakeTokens is an in-memory TokenStore.
type fakeTokens struct {
	grant       fleet.TokenGrant
	usesLeft    int
	expiresAt   time.Time
	revoked     bool
	allowedCIDR *net.IPNet
	redeemed    int
}

func (f *fakeTokens) Redeem(_ context.Context, _ string, sourceIP net.IP, now time.Time) (fleet.TokenGrant, error) {
	switch {
	case f.revoked, f.usesLeft <= 0, now.After(f.expiresAt):
		return fleet.TokenGrant{}, fleet.ErrTokenInvalid
	case f.allowedCIDR != nil && sourceIP != nil && !f.allowedCIDR.Contains(sourceIP):
		return fleet.TokenGrant{}, fleet.ErrTokenInvalid
	}
	f.usesLeft--
	f.redeemed++
	return f.grant, nil
}

// fakeCredentials is an in-memory CredentialStore.
type fakeCredentials struct {
	byNode map[string]*fleet.Credential
	byFP   map[string]*fleet.Credential
}

func newFakeCredentials() *fakeCredentials {
	return &fakeCredentials{byNode: map[string]*fleet.Credential{}, byFP: map[string]*fleet.Credential{}}
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
	f.byFP[cred.Fingerprint] = &c
	return nil
}

func (f *fakeCredentials) Revoke(_ context.Context, fingerprint, _ string, _ time.Time) error {
	if c, ok := f.byFP[fingerprint]; ok {
		if existing, ok := f.byNode[c.NodeID]; ok && existing.Fingerprint == fingerprint {
			delete(f.byNode, c.NodeID)
		}
	}
	return nil
}

// fakeDenylist is an in-memory DenylistStore.
type fakeDenylist struct{ denied map[string]bool }

func (f *fakeDenylist) IsDenied(_ context.Context, nodeID string) (bool, error) {
	return f.denied[nodeID], nil
}

func testEnv(t *testing.T) (*pki.CA, *fakeTokens, *fakeCredentials, *fakeDenylist, *fleet.Enroller) {
	t.Helper()
	ca, err := pki.New("test-ca")
	if err != nil {
		t.Fatalf("pki.New: %v", err)
	}
	tokens := &fakeTokens{grant: fleet.TokenGrant{Environment: "production"}, usesLeft: 1, expiresAt: time.Now().Add(time.Hour)}
	creds := newFakeCredentials()
	deny := &fakeDenylist{denied: map[string]bool{}}
	return ca, tokens, creds, deny, fleet.NewEnroller(ca, tokens, creds, deny)
}

func csrFor(t *testing.T, nodeID string) []byte {
	t.Helper()
	der, _, err := pki.NewCSR(nodeID)
	if err != nil {
		t.Fatalf("NewCSR: %v", err)
	}
	return der
}

func TestEnrollIssuesACertificate(t *testing.T) {
	t.Parallel()
	_, _, _, _, enroller := testEnv(t)

	leaf, err := enroller.Enroll(context.Background(), fleet.EnrollRequest{
		TokenPlaintext: "atlas_enroll_whatever",
		NodeID:         "node-1",
		CSRDER:         csrFor(t, "node-1"),
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	id, err := pki.PeerNodeID(leaf)
	if err != nil || id != "node-1" {
		t.Errorf("issued certificate identity = %q, %v; want node-1", id, err)
	}
}

func TestEnrollRefusesAnExhaustedToken(t *testing.T) {
	t.Parallel()
	_, tokens, _, _, enroller := testEnv(t)
	tokens.usesLeft = 0

	_, err := enroller.Enroll(context.Background(), fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err == nil {
		t.Fatal("Enroll succeeded with an exhausted token")
	}
}

func TestEnrollRefusesAnExpiredToken(t *testing.T) {
	t.Parallel()
	_, tokens, _, _, enroller := testEnv(t)
	tokens.expiresAt = time.Now().Add(-time.Minute)

	_, err := enroller.Enroll(context.Background(), fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err == nil {
		t.Fatal("Enroll succeeded with an expired token")
	}
}

func TestEnrollRefusesOutsideAllowedCIDR(t *testing.T) {
	t.Parallel()
	_, tokens, _, _, enroller := testEnv(t)
	_, cidr, _ := net.ParseCIDR("10.0.0.0/8")
	tokens.allowedCIDR = cidr

	_, err := enroller.Enroll(context.Background(), fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
		SourceIP: net.ParseIP("192.168.1.5"),
	})
	if err == nil {
		t.Fatal("Enroll succeeded from outside the token's allowed CIDR")
	}
}

// This is the test that pins the exact defect the design set out to prevent:
// a second enrollment claiming an identity that is already live must be
// refused, or a stolen token could take over an existing node.
func TestEnrollRefusesReenrollmentOfAnActiveNode(t *testing.T) {
	t.Parallel()
	_, tokens, _, _, enroller := testEnv(t)
	ctx := context.Background()

	if _, err := enroller.Enroll(ctx, fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	}); err != nil {
		t.Fatalf("first Enroll: %v", err)
	}

	tokens.usesLeft = 1 // a second, different token attempt
	_, err := enroller.Enroll(ctx, fleet.EnrollRequest{
		TokenPlaintext: "y", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err == nil {
		t.Fatal("Enroll re-issued a certificate for an already-active node id")
	}
	if errs.CodeOf(err) != errs.CodeAlreadyExists {
		t.Errorf("code = %q, want already_exists", errs.CodeOf(err))
	}
}

// The explicit grant is the one documented way past the refusal above.
func TestEnrollAllowsReenrollmentWithAnExplicitGrant(t *testing.T) {
	t.Parallel()
	_, tokens, creds, _, enroller := testEnv(t)
	ctx := context.Background()

	first, err := enroller.Enroll(ctx, fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err != nil {
		t.Fatalf("first Enroll: %v", err)
	}

	tokens.usesLeft = 1
	tokens.grant.AllowReenroll = true
	second, err := enroller.Enroll(ctx, fleet.EnrollRequest{
		TokenPlaintext: "y", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err != nil {
		t.Fatalf("re-enrollment with grant: %v", err)
	}
	if pki.Fingerprint(first) == pki.Fingerprint(second) {
		t.Error("re-enrollment did not issue a distinct certificate")
	}
	// And the old credential must no longer read as active.
	active, _ := creds.ActiveCredential(ctx, "node-1", time.Now())
	if active == nil || active.Fingerprint != pki.Fingerprint(second) {
		t.Error("the superseded credential is still considered active")
	}
}

func TestEnrollRefusesADeniedNode(t *testing.T) {
	t.Parallel()
	_, _, _, deny, enroller := testEnv(t)
	deny.denied["node-bad"] = true

	_, err := enroller.Enroll(context.Background(), fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-bad", CSRDER: csrFor(t, "node-bad"),
	})
	if err == nil {
		t.Fatal("Enroll succeeded for a denylisted node")
	}
}

func TestRenewIssuesAFreshCertificateForTheSameIdentity(t *testing.T) {
	t.Parallel()
	_, _, _, _, enroller := testEnv(t)
	ctx := context.Background()

	leaf, err := enroller.Enroll(ctx, fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	renewed, err := enroller.Renew(ctx, fleet.RenewRequest{PeerCert: leaf, CSRDER: csrFor(t, "node-1")})
	if err != nil {
		t.Fatalf("Renew: %v", err)
	}
	id, _ := pki.PeerNodeID(renewed)
	if id != "node-1" {
		t.Errorf("renewed identity = %q, want node-1", id)
	}
	if pki.Fingerprint(renewed) == pki.Fingerprint(leaf) {
		t.Error("Renew returned the same certificate rather than a fresh one")
	}
}

// A renewed certificate immediately supersedes the one it replaced — trying
// to renew again from the now-superseded certificate must fail, closing the
// window where two live-looking credentials exist for one node.
func TestRenewRefusesAnAlreadySupersededCertificate(t *testing.T) {
	t.Parallel()
	_, _, _, _, enroller := testEnv(t)
	ctx := context.Background()

	leaf, err := enroller.Enroll(ctx, fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}
	if _, err := enroller.Renew(ctx, fleet.RenewRequest{PeerCert: leaf, CSRDER: csrFor(t, "node-1")}); err != nil {
		t.Fatalf("first Renew: %v", err)
	}

	_, err = enroller.Renew(ctx, fleet.RenewRequest{PeerCert: leaf, CSRDER: csrFor(t, "node-1")})
	if err == nil {
		t.Fatal("Renew succeeded from an already-superseded certificate")
	}
}

func TestRenewRefusesADeniedNode(t *testing.T) {
	t.Parallel()
	_, _, _, deny, enroller := testEnv(t)
	ctx := context.Background()

	leaf, err := enroller.Enroll(ctx, fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	deny.denied["node-1"] = true
	if _, err := enroller.Renew(ctx, fleet.RenewRequest{PeerCert: leaf, CSRDER: csrFor(t, "node-1")}); err == nil {
		t.Fatal("Renew succeeded for a node ejected after enrollment")
	}
}

func TestShouldRenewCrossesAtHalfLifetime(t *testing.T) {
	t.Parallel()
	_, _, _, _, enroller := testEnv(t)

	leaf, err := enroller.Enroll(context.Background(), fleet.EnrollRequest{
		TokenPlaintext: "x", NodeID: "node-1", CSRDER: csrFor(t, "node-1"),
	})
	if err != nil {
		t.Fatalf("Enroll: %v", err)
	}

	if fleet.ShouldRenew(leaf, leaf.NotBefore.Add(time.Minute)) {
		t.Error("ShouldRenew fired immediately after issuance")
	}
	if !fleet.ShouldRenew(leaf, leaf.NotBefore.Add(13*time.Hour)) {
		t.Error("ShouldRenew did not fire past half the 24h lifetime")
	}
}

func TestNewTokenProducesUniqueUnpredictableValues(t *testing.T) {
	t.Parallel()

	a, err := fleet.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	b, err := fleet.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if a.Plaintext == b.Plaintext {
		t.Error("two generated tokens are identical")
	}
	if a.Hash == b.Hash {
		t.Error("two generated tokens hashed to the same value")
	}
	if fleet.HashToken(a.Plaintext) != a.Hash {
		t.Error("HashToken is not consistent with the hash returned by NewToken")
	}
}

func TestTokenSpecValidation(t *testing.T) {
	t.Parallel()

	valid := fleet.TokenSpec{Environment: "production", AllowedCIDR: "0.0.0.0/0", MaxUses: 1, TTL: time.Hour}
	if err := valid.Validate(); err != nil {
		t.Errorf("a well-formed spec was rejected: %v", err)
	}

	cases := []fleet.TokenSpec{
		{AllowedCIDR: "0.0.0.0/0", MaxUses: 1, TTL: time.Hour},                // no environment
		{Environment: "production", MaxUses: 1, TTL: time.Hour},               // no CIDR
		{Environment: "production", AllowedCIDR: "0.0.0.0/0", TTL: time.Hour}, // no max uses
		{Environment: "production", AllowedCIDR: "0.0.0.0/0", MaxUses: 1},     // no TTL
	}
	for i, c := range cases {
		if err := c.Validate(); err == nil {
			t.Errorf("case %d: an incomplete spec was accepted", i)
		}
	}
}
