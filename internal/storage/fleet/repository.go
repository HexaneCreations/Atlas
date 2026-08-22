// Package fleet is the PostgreSQL-backed implementation of the storage
// interfaces [github.com/hexane/atlas/internal/core/fleet] defines:
// [fleet.TokenStore], [fleet.CredentialStore], and [fleet.DenylistStore].
//
// It exists as its own package, separate from core/fleet, for the same
// reason internal/storage/metric is separate from internal/core/collect: the
// domain rules (which token may redeem, which node may re-enroll) should be
// readable and testable with no database, and the SQL that persists them is
// a different kind of correctness problem entirely.
package fleet

import (
	"context"
	"net"
	"time"

	corefleet "github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Repository is the fleet storage surface: tokens, issued credentials, and
// the denylist.
type Repository struct {
	pool *pgxpool.Pool
}

// NewRepository builds a repository over a started pool.
func NewRepository(pool *pgxpool.Pool) *Repository { return &Repository{pool: pool} }

// CreateToken persists a newly generated enrollment token.
func (r *Repository) CreateToken(ctx context.Context, hash string, spec corefleet.TokenSpec, now time.Time) error {
	const op = "fleet.Repository.CreateToken"

	const q = `
		INSERT INTO enrollment_tokens
			(token_hash, label, environment, allowed_cidr, max_uses, uses_remaining, expires_at, allow_reenroll, created_at)
		VALUES ($1, $2, $3, $4, $5, $5, $6, $7, $8)`

	_, err := r.pool.Exec(ctx, q,
		hash, spec.Label, spec.Environment, spec.AllowedCIDR, spec.MaxUses,
		now.Add(spec.TTL), spec.AllowReenroll, now,
	)
	if err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not create the enrollment token").WithOp(op)
	}
	return nil
}

// Redeem implements [corefleet.TokenStore].
//
// The whole check is one conditional UPDATE: expiry, exhaustion, and
// revocation are all evaluated in the WHERE clause of the statement that
// also decrements the counter, so two enrollments racing for a token's last
// use cannot both succeed — the database's row lock is the only
// synchronisation this needs. See [corefleet.TokenStore] for why that
// atomicity is a correctness requirement, not an optimisation.
func (r *Repository) Redeem(ctx context.Context, tokenHash string, sourceIP net.IP, now time.Time) (corefleet.TokenGrant, error) {
	const op = "fleet.Repository.Redeem"

	const q = `
		UPDATE enrollment_tokens
		SET uses_remaining = uses_remaining - 1
		WHERE token_hash = $1
		  AND revoked_at IS NULL
		  AND expires_at > $2
		  AND uses_remaining > 0
		  AND (allowed_cidr IS NULL OR $3::inet IS NULL OR allowed_cidr >> $3::inet)
		RETURNING environment, allow_reenroll`

	var sourceIPArg any
	if sourceIP != nil {
		sourceIPArg = sourceIP.String()
	}

	var grant corefleet.TokenGrant
	var environment *string
	err := r.pool.QueryRow(ctx, q, tokenHash, now, sourceIPArg).Scan(&environment, &grant.AllowReenroll)
	if err != nil {
		if err == pgx.ErrNoRows {
			return corefleet.TokenGrant{}, corefleet.FormatFailure(op, "token not found, expired, exhausted, revoked, or source outside its allowed network")
		}
		return corefleet.TokenGrant{}, errs.Wrap(err, errs.CodeUnavailable, "could not redeem the enrollment token").WithOp(op)
	}
	if environment != nil {
		grant.Environment = *environment
	}
	return grant, nil
}

// AuthorizedPeer implements [corefleet.PeerStore].
//
// The lookup is by Peer ID only — the identity the Noise handshake already
// proved. Nothing the agent sends in a request body participates in it, which
// is what stops a node_id (public, present in every inventory payload) from
// being usable to admit an unregistered keypair.
func (r *Repository) AuthorizedPeer(ctx context.Context, peerID string) (corefleet.PeerIdentity, error) {
	const op = "fleet.Repository.AuthorizedPeer"

	const q = `
		SELECT peer_id, node_id, environment, role
		FROM agent_peers
		WHERE peer_id = $1 AND status = $2`

	var id corefleet.PeerIdentity
	err := r.pool.QueryRow(ctx, q, peerID, corefleet.PeerStatusActive).
		Scan(&id.PeerID, &id.NodeID, &id.Environment, &id.Role)
	if err != nil {
		if err == pgx.ErrNoRows {
			return corefleet.PeerIdentity{}, corefleet.ErrPeerNotAuthorized
		}
		return corefleet.PeerIdentity{}, errs.Wrap(err, errs.CodeUnavailable,
			"could not check the peer authorization").WithOp(op)
	}
	return id, nil
}

// RegisterPeer implements [corefleet.PeerRegistry].
//
// Re-registering an existing Peer ID updates its binding and reactivates it,
// so an operator correcting a typo'd environment does not have to delete a
// row first. It does not create a second row for the same peer: one keypair
// authorizes one binding, revocable in one place.
func (r *Repository) RegisterPeer(ctx context.Context, spec corefleet.PeerSpec) error {
	const op = "fleet.Repository.RegisterPeer"

	role := spec.Role
	if role == "" {
		role = corefleet.PeerRoleAgent
	}

	const q = `
		INSERT INTO agent_peers (peer_id, node_id, environment, role, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (peer_id) DO UPDATE
		SET node_id = EXCLUDED.node_id, environment = EXCLUDED.environment,
		    role = EXCLUDED.role, status = EXCLUDED.status, updated_at = now()`

	if _, err := r.pool.Exec(ctx, q, spec.PeerID, spec.NodeID, spec.Environment, role, corefleet.PeerStatusActive); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not register the peer authorization").WithOp(op)
	}
	return nil
}

// RevokePeer implements [corefleet.PeerRegistry]. Revoking an unknown or
// already-revoked peer is not an error: the caller's intent — that this Peer
// ID must not be authorized — holds either way.
func (r *Repository) RevokePeer(ctx context.Context, peerID string) error {
	const op = "fleet.Repository.RevokePeer"

	const q = `UPDATE agent_peers SET status = $2, updated_at = now() WHERE peer_id = $1`
	if _, err := r.pool.Exec(ctx, q, peerID, corefleet.PeerStatusRevoked); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the peer authorization").WithOp(op)
	}
	return nil
}

// ListPeers implements [corefleet.PeerRegistry].
func (r *Repository) ListPeers(ctx context.Context) ([]corefleet.PeerAuthorization, error) {
	const op = "fleet.Repository.ListPeers"

	const q = `
		SELECT peer_id, node_id, environment, role, status, created_at, updated_at
		FROM agent_peers ORDER BY created_at DESC`

	rows, err := r.pool.Query(ctx, q)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list peer authorizations").WithOp(op)
	}
	defer rows.Close()

	var out []corefleet.PeerAuthorization
	for rows.Next() {
		var a corefleet.PeerAuthorization
		if err := rows.Scan(&a.PeerID, &a.NodeID, &a.Environment, &a.Role, &a.Status, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read a peer authorization").WithOp(op)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read peer authorizations").WithOp(op)
	}
	return out, nil
}

// ActiveCredential implements [corefleet.CredentialStore].
func (r *Repository) ActiveCredential(ctx context.Context, nodeID string, now time.Time) (*corefleet.Credential, error) {
	const op = "fleet.Repository.ActiveCredential"

	const q = `
		SELECT fingerprint, node_id, issued_at, expires_at, COALESCE(enrolled_via, '')
		FROM node_credentials
		WHERE node_id = $1 AND revoked_at IS NULL AND expires_at > $2
		ORDER BY issued_at DESC
		LIMIT 1`

	var c corefleet.Credential
	err := r.pool.QueryRow(ctx, q, nodeID, now).
		Scan(&c.Fingerprint, &c.NodeID, &c.IssuedAt, &c.ExpiresAt, &c.EnrolledVia)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not check for an active credential").WithOp(op)
	}
	return &c, nil
}

// RecordIssuance implements [corefleet.CredentialStore].
func (r *Repository) RecordIssuance(ctx context.Context, cred corefleet.Credential) error {
	const op = "fleet.Repository.RecordIssuance"

	const q = `
		INSERT INTO node_credentials (fingerprint, node_id, issued_at, expires_at, enrolled_via)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''))`

	if _, err := r.pool.Exec(ctx, q, cred.Fingerprint, cred.NodeID, cred.IssuedAt, cred.ExpiresAt, cred.EnrolledVia); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the issued credential").
			WithOp(op).WithDetail("node_id", cred.NodeID)
	}
	return nil
}

// Revoke implements [corefleet.CredentialStore].
func (r *Repository) Revoke(ctx context.Context, fingerprint, reason string, now time.Time) error {
	const op = "fleet.Repository.Revoke"

	const q = `
		UPDATE node_credentials
		SET revoked_at = $2, revoked_reason = $3
		WHERE fingerprint = $1 AND revoked_at IS NULL`

	if _, err := r.pool.Exec(ctx, q, fingerprint, now, reason); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the credential").WithOp(op)
	}
	return nil
}

// IsDenied implements [corefleet.DenylistStore].
func (r *Repository) IsDenied(ctx context.Context, nodeID string) (bool, error) {
	const op = "fleet.Repository.IsDenied"

	const q = `SELECT 1 FROM node_denylist WHERE node_id = $1`
	var one int
	err := r.pool.QueryRow(ctx, q, nodeID).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case err == pgx.ErrNoRows:
		return false, nil
	default:
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not check the denylist").WithOp(op)
	}
}

// Deny adds a node id to the denylist: ejection that does not wait for its
// certificate to expire (readiness review M4).
func (r *Repository) Deny(ctx context.Context, nodeID, reason string, now time.Time) error {
	const op = "fleet.Repository.Deny"

	const q = `
		INSERT INTO node_denylist (node_id, reason, created_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (node_id) DO UPDATE SET reason = EXCLUDED.reason, created_at = EXCLUDED.created_at`

	if _, err := r.pool.Exec(ctx, q, nodeID, reason, now); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not add the node to the denylist").WithOp(op)
	}
	return nil
}

// PruneIngestedEnvelopes deletes idempotency records older than before. See
// migrations/0004_fleet.sql: this table is not a hypertable, so retention is
// application-driven rather than a Timescale policy.
func (r *Repository) PruneIngestedEnvelopes(ctx context.Context, before time.Time) error {
	const op = "fleet.Repository.PruneIngestedEnvelopes"
	if _, err := r.pool.Exec(ctx, `DELETE FROM ingested_envelopes WHERE received_at < $1`, before); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not prune ingested envelopes").WithOp(op)
	}
	return nil
}

// Allow removes a node id from the denylist.
func (r *Repository) Allow(ctx context.Context, nodeID string) error {
	const op = "fleet.Repository.Allow"
	if _, err := r.pool.Exec(ctx, `DELETE FROM node_denylist WHERE node_id = $1`, nodeID); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not remove the node from the denylist").WithOp(op)
	}
	return nil
}

// IsGranted implements [corefleet.GrantStore].
func (r *Repository) IsGranted(ctx context.Context, nodeID, operation string) (bool, error) {
	const op = "fleet.Repository.IsGranted"

	const q = `SELECT 1 FROM agent_operation_grants WHERE node_id = $1 AND operation = $2 AND revoked_at IS NULL`
	var one int
	err := r.pool.QueryRow(ctx, q, nodeID, operation).Scan(&one)
	switch {
	case err == nil:
		return true, nil
	case err == pgx.ErrNoRows:
		return false, nil
	default:
		return false, errs.Wrap(err, errs.CodeUnavailable, "could not check the agent operation grant").WithOp(op)
	}
}

// Grant implements [corefleet.GrantStore]. ON CONFLICT DO NOTHING is the
// idempotency the interface requires: an existing row, granted or revoked,
// is never overwritten.
func (r *Repository) Grant(ctx context.Context, nodeID, operation, grantedBy string, now time.Time) error {
	const op = "fleet.Repository.Grant"

	const q = `
		INSERT INTO agent_operation_grants (node_id, operation, granted_at, granted_by)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (node_id, operation) DO NOTHING`

	if _, err := r.pool.Exec(ctx, q, nodeID, operation, now, grantedBy); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not record the agent operation grant").WithOp(op)
	}
	return nil
}

// RevokeGrant implements [corefleet.GrantStore].
func (r *Repository) RevokeGrant(ctx context.Context, nodeID, operation, reason string, now time.Time) error {
	const op = "fleet.Repository.RevokeGrant"

	const q = `
		UPDATE agent_operation_grants
		SET revoked_at = $3, revoked_reason = $4
		WHERE node_id = $1 AND operation = $2 AND revoked_at IS NULL`

	if _, err := r.pool.Exec(ctx, q, nodeID, operation, now, reason); err != nil {
		return errs.Wrap(err, errs.CodeUnavailable, "could not revoke the agent operation grant").WithOp(op)
	}
	return nil
}

var (
	_ corefleet.TokenStore      = (*Repository)(nil)
	_ corefleet.CredentialStore = (*Repository)(nil)
	_ corefleet.DenylistStore   = (*Repository)(nil)
	_ corefleet.GrantStore      = (*Repository)(nil)
)
