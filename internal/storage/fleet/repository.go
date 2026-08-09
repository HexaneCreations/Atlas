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

var (
	_ corefleet.TokenStore      = (*Repository)(nil)
	_ corefleet.CredentialStore = (*Repository)(nil)
	_ corefleet.DenylistStore   = (*Repository)(nil)
)
