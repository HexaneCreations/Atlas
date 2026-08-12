package fleet

import (
	"context"
	"time"
)

// OperationContainerLogs is the only privileged AgentOps operation today —
// see libp2ptransport.AgentOpContainerLogs, which this string must match.
const OperationContainerLogs = "container_logs"

// GrantStore records and checks explicit authorization for a node to have a
// privileged AgentOps operation performed against it. This is deliberately
// separate from CredentialStore: a live certificate is authentication, not
// authorization, and the two must be independently revocable.
type GrantStore interface {
	// IsGranted reports whether nodeID currently holds a live (non-revoked)
	// grant for operation.
	IsGranted(ctx context.Context, nodeID, operation string) (bool, error)
	// Grant records authorization for nodeID to perform operation. Must be
	// idempotent and must never resurrect an already-revoked grant — an
	// existing row, granted or revoked, is left untouched. This is what
	// stops a re-enrollment from silently undoing an operator's revocation.
	Grant(ctx context.Context, nodeID, operation, grantedBy string, now time.Time) error
	// RevokeGrant withdraws a previously granted authorization, independent
	// of the node's certificate/credential state.
	RevokeGrant(ctx context.Context, nodeID, operation, reason string, now time.Time) error
}
