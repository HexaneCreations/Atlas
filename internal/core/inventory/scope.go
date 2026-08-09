// Package inventory defines how a caller asks for live inventory from a
// particular host.
//
// Atlas reads its own host today. It will read many hosts once agents push
// their inventory to a control plane, and the difference between those two
// worlds should be an implementation detail rather than an API redesign — so
// every inventory call already carries the scope it is asking about.
//
// The scope is a struct rather than a bare node id on purpose. Multi-tenancy
// is a planned addition, and when it arrives it is a new field here plus the
// code that reads it, not a signature change rippling through six methods,
// their handlers, their fakes and their tests.
package inventory

import "github.com/hexane/atlas/internal/platform/errs"

// Scope identifies which host an inventory question is about.
//
// The zero value means "the host this Atlas instance runs on", which is what
// every current caller wants and what keeps existing endpoints working without
// a required parameter.
type Scope struct {
	// NodeID names the host. Empty means the local node.
	NodeID string

	// TenantID is reserved. Atlas is single-tenant today; the field exists so
	// that adding tenancy later is a change to this package and its readers
	// rather than to every inventory signature in the codebase.
	TenantID string
}

// Local is the scope for the host Atlas is running on.
var Local = Scope{}

// IsLocal reports whether the scope refers to the running host.
//
// resolvedLocalID is the node id this Atlas instance collects for. An empty
// NodeID is local by definition; a NodeID equal to the local one is the same
// host addressed explicitly, which callers do routinely once a node picker
// exists.
func (s Scope) IsLocal(resolvedLocalID string) bool {
	return s.NodeID == "" || s.NodeID == resolvedLocalID
}

// ErrRemoteUnavailable reports that a scope names a host this Atlas instance
// cannot read.
//
// Returned rather than an empty result, and that distinction is the whole
// point: an empty container list for a remote host reads as "nothing is
// running there", which is a claim Atlas has no basis to make. Until agents
// push remote inventory, the honest answer is that the question cannot be
// answered here.
func ErrRemoteUnavailable(op, nodeID, subject string) error {
	return errs.New(errs.CodeUnavailable,
		"live inventory can only be read for the host Atlas runs on").
		WithOp(op).
		WithDetail("subject", subject).
		WithDetail("node", nodeID).
		WithDetail("reason", "remote inventory requires an agent on that host")
}
