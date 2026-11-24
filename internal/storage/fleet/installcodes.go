package fleet

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	corefleet "github.com/hexane/atlas/internal/core/fleet"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/id"
	"github.com/jackc/pgx/v5"
)

// nodeIDAttempts bounds retries when a generated node id is already taken.
// With 128 random bits a single collision is already implausible.
const nodeIDAttempts = 3

const installCodeColumns = `id, node_id, environment, created_by, created_at, expires_at,
	revoked_at, COALESCE(revoked_by, ''), redeemed_at, COALESCE(redeemed_peer_id, ''),
	COALESCE(host(redeemed_ip), '')`

func scanInstallCode(row pgx.Row) (corefleet.InstallCode, error) {
	var c corefleet.InstallCode
	err := row.Scan(&c.ID, &c.NodeID, &c.Environment, &c.CreatedBy, &c.CreatedAt, &c.ExpiresAt,
		&c.RevokedAt, &c.RevokedBy, &c.RedeemedAt, &c.RedeemedPeerID, &c.RedeemedIP)
	return c, err
}

// CreateInstallCode implements [corefleet.InstallCodeStore]. The node id is
// generated here and inserted only if no node, peer authorization or other
// install code already uses it; the UNIQUE constraint on install_codes.node_id
// closes the race between two concurrent creates.
func (r *Repository) CreateInstallCode(ctx context.Context, codeHash string, spec corefleet.InstallCodeSpec, now time.Time) (corefleet.InstallCode, error) {
	const op = "fleet.Repository.CreateInstallCode"

	ttl, err := spec.TTL()
	if err != nil {
		return corefleet.InstallCode{}, err
	}

	const q = `
		INSERT INTO install_codes (id, code_hash, node_id, environment, created_by, created_at, expires_at)
		SELECT $1, $2, $3, $4, $5, $6, $7
		WHERE NOT EXISTS (SELECT 1 FROM nodes WHERE node_id = $3)
		  AND NOT EXISTS (SELECT 1 FROM agent_peers WHERE node_id = $3)
		ON CONFLICT (node_id) DO NOTHING
		RETURNING ` + installCodeColumns

	for range nodeIDAttempts {
		nodeID, err := corefleet.NewNodeID()
		if err != nil {
			return corefleet.InstallCode{}, err
		}
		c, err := scanInstallCode(r.pool.QueryRow(ctx, q,
			id.New(), codeHash, nodeID, spec.Environment, spec.CreatedBy, now, now.Add(ttl)))
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return corefleet.InstallCode{}, errs.Wrap(err, errs.CodeUnavailable, "could not create the install code").WithOp(op)
		}
		return c, nil
	}
	return corefleet.InstallCode{}, errs.New(errs.CodeUnavailable, "could not allocate an unused node id").WithOp(op)
}

// ListInstallCodes implements [corefleet.InstallCodeStore], newest first.
func (r *Repository) ListInstallCodes(ctx context.Context) ([]corefleet.InstallCode, error) {
	const op = "fleet.Repository.ListInstallCodes"

	rows, err := r.pool.Query(ctx, `SELECT `+installCodeColumns+` FROM install_codes ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not list install codes").WithOp(op)
	}
	defer rows.Close()

	out := []corefleet.InstallCode{}
	for rows.Next() {
		c, err := scanInstallCode(rows)
		if err != nil {
			return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read an install code").WithOp(op)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(err, errs.CodeUnavailable, "could not read install codes").WithOp(op)
	}
	return out, nil
}

// RevokeInstallCode implements [corefleet.InstallCodeStore].
func (r *Repository) RevokeInstallCode(ctx context.Context, codeID, revokedBy string, now time.Time) (corefleet.InstallCode, error) {
	const op = "fleet.Repository.RevokeInstallCode"

	const q = `
		UPDATE install_codes SET revoked_at = $2, revoked_by = $3
		WHERE id = $1 AND revoked_at IS NULL AND redeemed_at IS NULL AND expires_at > $2
		RETURNING ` + installCodeColumns

	c, err := scanInstallCode(r.pool.QueryRow(ctx, q, codeID, now, revokedBy))
	if err == nil {
		return c, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return corefleet.InstallCode{}, errs.Wrap(err, errs.CodeUnavailable, "could not revoke the install code").WithOp(op)
	}

	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM install_codes WHERE id = $1)`, codeID).Scan(&exists); err != nil {
		return corefleet.InstallCode{}, errs.Wrap(err, errs.CodeUnavailable, "could not revoke the install code").WithOp(op)
	}
	if !exists {
		return corefleet.InstallCode{}, errs.New(errs.CodeNotFound, "no such install code").WithOp(op)
	}
	return corefleet.InstallCode{}, errs.New(errs.CodeFailedPrecondition,
		"only a pending install code can be revoked; this one is already redeemed, revoked or expired").WithOp(op)
}

// RedeemInstallCode implements [corefleet.InstallCodeStore].
//
// One transaction: the conditional UPDATE spends the code (its row lock makes
// a concurrent second redeem wait, then see redeemed_at set and match
// nothing), then the node must still be unknown and the Peer ID must not
// exist in agent_peers in any status. The peer insert never updates an
// existing row — unlike [Repository.RegisterPeer]. Any refusal rolls back,
// so a refused redeem leaves the code unspent and writes nothing.
func (r *Repository) RedeemInstallCode(ctx context.Context, codeHash, peerID string, sourceIP net.IP, now time.Time) (corefleet.Redemption, error) {
	const op = "fleet.Repository.RedeemInstallCode"
	unavailable := func(err error) error {
		return errs.Wrap(err, errs.CodeUnavailable, "could not redeem the install code").WithOp(op)
	}

	var ipArg any
	if addr, ok := netip.AddrFromSlice(sourceIP); ok && sourceIP != nil {
		ipArg = addr.Unmap().String()
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return corefleet.Redemption{}, unavailable(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var red corefleet.Redemption
	err = tx.QueryRow(ctx, `
		UPDATE install_codes
		SET redeemed_at = $2, redeemed_peer_id = $3, redeemed_ip = $4::inet
		WHERE code_hash = $1 AND revoked_at IS NULL AND redeemed_at IS NULL AND expires_at > $2
		RETURNING id, node_id, environment`,
		codeHash, now, peerID, ipArg).Scan(&red.CodeID, &red.NodeID, &red.Environment)
	if errors.Is(err, pgx.ErrNoRows) {
		return corefleet.Redemption{}, corefleet.ErrInstallCodeRejected
	}
	if err != nil {
		return corefleet.Redemption{}, unavailable(err)
	}

	var nodeTaken bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM nodes WHERE node_id = $1)
		    OR EXISTS (SELECT 1 FROM agent_peers WHERE node_id = $1)`, red.NodeID).Scan(&nodeTaken); err != nil {
		return corefleet.Redemption{}, unavailable(err)
	}
	if nodeTaken {
		return corefleet.Redemption{}, errs.Wrap(corefleet.ErrInstallCodeRejected, errs.CodeUnauthenticated,
			"the pre-assigned node already exists").WithOp(op).WithDetail("install_code_id", red.CodeID)
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO agent_peers (peer_id, node_id, environment, role, status)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (peer_id) DO NOTHING`,
		peerID, red.NodeID, red.Environment, corefleet.PeerRoleAgent, corefleet.PeerStatusActive)
	if err != nil {
		return corefleet.Redemption{}, unavailable(err)
	}
	if tag.RowsAffected() != 1 {
		return corefleet.Redemption{}, errs.Wrap(corefleet.ErrInstallCodeRejected, errs.CodeUnauthenticated,
			"the peer id is already registered").WithOp(op).WithDetail("install_code_id", red.CodeID)
	}

	// The same default grant HTTPS enrollment records (core/fleet.Enroller):
	// container_logs. ON CONFLICT DO NOTHING never resurrects a revoked grant.
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_operation_grants (node_id, operation, granted_at, granted_by)
		VALUES ($1, $2, $3, 'install-code')
		ON CONFLICT (node_id, operation) DO NOTHING`,
		red.NodeID, corefleet.OperationContainerLogs, now); err != nil {
		return corefleet.Redemption{}, unavailable(err)
	}

	if err := tx.Commit(ctx); err != nil {
		return corefleet.Redemption{}, unavailable(err)
	}
	return red, nil
}

var _ corefleet.InstallCodeStore = (*Repository)(nil)
