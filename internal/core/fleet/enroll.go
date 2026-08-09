package fleet

import (
	"context"
	"crypto/x509"
	"net"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/pki"
)

// TokenStore redeems enrollment tokens.
//
// Redeem must be atomic: check-then-decrement from two statements would let
// concurrent enrollments over-spend a token's use count, exactly the kind of
// race a security boundary cannot have. Implementations should redeem with a
// single conditional UPDATE.
type TokenStore interface {
	// Redeem consumes one use of the token if it is valid for sourceIP at
	// now, or returns [ErrTokenInvalid] if it is not. A successful redemption
	// is not reversible — a subsequent failure elsewhere in enrollment does
	// not refund the use. That is a deliberate simplification: refunding
	// would reopen the exact race Redeem exists to close, and a spent use on
	// an otherwise-failed attempt is a rounding error against a token's total
	// budget, not a security problem.
	Redeem(ctx context.Context, tokenHash string, sourceIP net.IP, now time.Time) (TokenGrant, error)
}

// Credential is an issued certificate's storage record.
type Credential struct {
	Fingerprint string
	NodeID      string
	IssuedAt    time.Time
	ExpiresAt   time.Time
	EnrolledVia string // token hash, empty for a renewal
}

// CredentialStore records and queries issued certificates.
type CredentialStore interface {
	// ActiveCredential returns the node's current live credential, if any.
	// "Live" means not revoked and not expired as of now. Returns
	// (nil, nil) — not an error — when the node has none, which is the
	// ordinary case for first enrollment.
	ActiveCredential(ctx context.Context, nodeID string, now time.Time) (*Credential, error)
	// RecordIssuance stores a newly issued certificate.
	RecordIssuance(ctx context.Context, cred Credential) error
	// Revoke marks a credential as no longer valid, independent of its
	// expiry. Used when a renewal supersedes an earlier certificate, and
	// available for administrative revocation.
	Revoke(ctx context.Context, fingerprint, reason string, now time.Time) error
}

// DenylistStore checks and records ejected node ids.
//
// This is the revocation path that does not wait for a certificate to expire
// (readiness review M4): an operator can eject a node in seconds regardless
// of how much of its 24-hour certificate lifetime remains.
type DenylistStore interface {
	IsDenied(ctx context.Context, nodeID string) (bool, error)
}

// Enroller is the domain service that turns a certificate request into an
// issued certificate, or refuses to.
type Enroller struct {
	ca          *pki.CA
	tokens      TokenStore
	credentials CredentialStore
	denylist    DenylistStore
	now         func() time.Time
}

// NewEnroller builds an Enroller.
func NewEnroller(ca *pki.CA, tokens TokenStore, credentials CredentialStore, denylist DenylistStore) *Enroller {
	return &Enroller{ca: ca, tokens: tokens, credentials: credentials, denylist: denylist, now: time.Now}
}

// EnrollRequest is what an agent presents to bootstrap a certificate.
type EnrollRequest struct {
	TokenPlaintext string
	NodeID         string
	CSRDER         []byte
	SourceIP       net.IP
}

// Enroll validates a first-time enrollment request and, if it is legitimate,
// issues a certificate.
//
// The ordering of checks matters and is deliberate: the token is checked
// (and consumed) first, because it is the only thing an anonymous caller has
// presented — everything after this point is evaluated only once the caller
// has proven it holds something bounded. The denylist and re-enrollment
// checks come next, both able to refuse an otherwise-valid token. The CSR
// signature is checked last because it is the cheapest possible way to catch
// a malformed request, and there is no reason to spend a token's use on one
// — but note Redeem has already been called by that point, so a malformed
// CSR still consumes a use. That is accepted: legitimate provisioning never
// sends a malformed CSR, so the cost falls only on misuse.
func (e *Enroller) Enroll(ctx context.Context, req EnrollRequest) (*x509.Certificate, error) {
	const op = "fleet.Enroller.Enroll"
	now := e.now()

	if req.NodeID == "" {
		return nil, errs.New(errs.CodeInvalidArgument, "enrollment requires a node ID").WithOp(op)
	}

	grant, err := e.tokens.Redeem(ctx, HashToken(req.TokenPlaintext), req.SourceIP, now)
	if err != nil {
		return nil, err // already a typed ErrTokenInvalid-shaped error; do not leak more detail
	}

	denied, err := e.denylist.IsDenied(ctx, req.NodeID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not check the denylist").WithOp(op)
	}
	if denied {
		return nil, errs.New(errs.CodeUnauthenticated, "this node id has been ejected from the fleet").
			WithOp(op).WithDetail("node_id", req.NodeID)
	}

	// The rule that prevents identity takeover: see
	// docs/architecture/agent-design.md sec 4. Without it, anyone holding a
	// valid (if scoped) token could claim an existing node's identity.
	active, err := e.credentials.ActiveCredential(ctx, req.NodeID, now)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not check for an existing credential").WithOp(op)
	}
	if active != nil && !grant.AllowReenroll {
		return nil, errs.New(errs.CodeAlreadyExists,
			"node %q already holds a live certificate; re-enrollment requires a token with an explicit grant", req.NodeID).
			WithOp(op).WithDetail("node_id", req.NodeID).WithDetail("existing_fingerprint", active.Fingerprint)
	}

	csr, err := pki.ParseCSR(req.CSRDER)
	if err != nil {
		return nil, err
	}
	leaf, err := e.ca.IssueLeaf(csr, req.NodeID)
	if err != nil {
		return nil, err
	}

	if active != nil && grant.AllowReenroll {
		// A deliberate re-provision supersedes the old certificate outright,
		// rather than leaving two live credentials for one node id.
		if err := e.credentials.Revoke(ctx, active.Fingerprint, "superseded by re-enrollment", now); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not supersede the previous credential").WithOp(op)
		}
	}

	if err := e.credentials.RecordIssuance(ctx, Credential{
		Fingerprint: pki.Fingerprint(leaf),
		NodeID:      req.NodeID,
		IssuedAt:    leaf.NotBefore,
		ExpiresAt:   leaf.NotAfter,
		EnrolledVia: HashToken(req.TokenPlaintext),
	}); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not record the issued credential").WithOp(op)
	}

	return leaf, nil
}

// RenewRequest is what an already-enrolled agent presents to rotate its
// certificate before expiry. No token: the currently valid mTLS connection
// (verified by the TLS stack before this is ever called) is the proof of
// identity.
type RenewRequest struct {
	// PeerCert is the certificate the caller authenticated with — already
	// verified against the CA by the TLS stack. Renew re-derives the node id
	// from it rather than trusting a value in the request body, for the same
	// reason ingest does (see docs/architecture/agent-design.md sec 3, C1).
	PeerCert *x509.Certificate
	CSRDER   []byte
}

// Renew issues a fresh certificate for an already-enrolled node and
// supersedes the one it renews.
func (e *Enroller) Renew(ctx context.Context, req RenewRequest) (*x509.Certificate, error) {
	const op = "fleet.Enroller.Renew"
	now := e.now()

	nodeID, err := pki.PeerNodeID(req.PeerCert)
	if err != nil {
		return nil, err
	}

	fp := pki.Fingerprint(req.PeerCert)
	active, err := e.credentials.ActiveCredential(ctx, nodeID, now)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not check the current credential").WithOp(op)
	}
	if active == nil || active.Fingerprint != fp {
		// The presented certificate verified against the CA (TLS already
		// checked that) but is not the credential Atlas currently considers
		// live for this node — for instance, one already superseded by an
		// earlier renewal. Refusing here is what stops a stale-but-unexpired
		// certificate from being renewed indefinitely after its successor
		// exists.
		return nil, errs.New(errs.CodeFailedPrecondition,
			"the presented certificate is not this node's active credential").
			WithOp(op).WithDetail("node_id", nodeID)
	}

	denied, err := e.denylist.IsDenied(ctx, nodeID)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not check the denylist").WithOp(op)
	}
	if denied {
		return nil, errs.New(errs.CodeUnauthenticated, "this node id has been ejected from the fleet").
			WithOp(op).WithDetail("node_id", nodeID)
	}

	csr, err := pki.ParseCSR(req.CSRDER)
	if err != nil {
		return nil, err
	}
	leaf, err := e.ca.IssueLeaf(csr, nodeID)
	if err != nil {
		return nil, err
	}

	if err := e.credentials.Revoke(ctx, fp, "superseded by renewal", now); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not supersede the renewed credential").WithOp(op)
	}
	if err := e.credentials.RecordIssuance(ctx, Credential{
		Fingerprint: pki.Fingerprint(leaf),
		NodeID:      nodeID,
		IssuedAt:    leaf.NotBefore,
		ExpiresAt:   leaf.NotAfter,
	}); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not record the renewed credential").WithOp(op)
	}

	return leaf, nil
}

// ShouldRenew reports whether a certificate has crossed [pki.RenewAt] of its
// lifetime and an agent holding it should renew now rather than wait for
// expiry.
func ShouldRenew(cert *x509.Certificate, now time.Time) bool {
	total := cert.NotAfter.Sub(cert.NotBefore)
	elapsed := now.Sub(cert.NotBefore)
	return elapsed >= time.Duration(float64(total)*pki.RenewAt)
}
