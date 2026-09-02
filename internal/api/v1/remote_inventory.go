package v1

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
)

// resolveInventory answers one inventory question for whatever scope the
// request names: read live from this host, or read the latest snapshot an
// agent pushed for a remote one.
//
// This is where the Scope seam (docs/architecture/agent-design.md §5) is
// actually cashed in — every handler that calls it gained remote-node support
// with no change to its own signature or response shape, because the seam
// was already threaded through the API in Phase 0. `local` is the existing
// per-subject method on [CollectionSource]; this function decides whether to
// call it at all.
//
// It is also the single point that authorizes every inventory.go handler
// (ListProcesses, ListServices, ListCronJobs, ListPorts, ListMounts): all
// five call through here, so gating node.read once here covers all five
// rather than five separate checks — the same reasoning that put the scope
// seam itself in one place. page names which of those five pages this
// particular call is for, so the second, page-visibility check (see
// [Handler.requireScope]) narrows correctly per caller rather than treating
// all five as one.
func resolveInventory[T any](
	h *Handler, r *http.Request, pluginID string, subject inventory.Subject, page pageauthz.Page,
	local func(ctx context.Context, scope inventory.Scope) (T, error),
) (T, inventory.Meta, error) {
	const op = "v1.resolveInventory"
	var zero T

	if h.deps.Collection == nil {
		return zero, inventory.Meta{}, errs.New(errs.CodeUnavailable, "collection is not configured").WithOp(op)
	}

	scope, err := h.requireScope(r, user.PermissionNodeRead, page)
	if err != nil {
		return zero, inventory.Meta{}, err
	}
	if scope.IsLocal(h.deps.Collection.Identity().NodeID) {
		if perr := h.requirePlugin(pluginID); perr != nil {
			// The plugin backing this subject is not active in this process.
			// A co-located agent may have pushed a snapshot for this same
			// node id; prefer it over an absence that is only true of the
			// control plane. If nothing was pushed, perr — which carries
			// reason=no_local_plugin — is the honest answer.
			if errs.CodeOf(perr) != errs.CodeNotImplemented || h.deps.Inventory == nil {
				return zero, inventory.Meta{}, perr
			}
			localID := h.deps.Collection.Identity().NodeID
			data, meta, rerr := resolveRemoteInventory[T](r.Context(), h, localID, subject)
			if rerr != nil {
				return zero, inventory.Meta{}, perr
			}
			return data, meta, nil
		}
		data, err := local(r.Context(), scope)
		if err != nil {
			return zero, inventory.Meta{}, err
		}
		return data, inventory.LiveMeta(h.resolvedNode(scope)), nil
	}

	return resolveRemoteInventory[T](r.Context(), h, scope.NodeID, subject)
}

// resolveRemoteInventory reads the latest pushed snapshot for a node.
//
// It distinguishes three failure shapes that must not be conflated, because
// collapsing any two of them into the others was a real defect caught during
// this milestone's own verification (see the implementation report):
//
//   - the node id names no node Atlas has ever seen           -> not_found
//   - the node exists but has never pushed *any* inventory    -> unavailable
//   - the node has pushed other subjects but never this one   -> not_implemented
//     (its agent legitimately has nothing to report — no Docker on that
//     host is the common case, the same honest absence a local plugin
//     reports for its own host)
//
// Go does not allow a generic method, so this is a free function taking the
// handler explicitly rather than a method on [Handler].
func resolveRemoteInventory[T any](ctx context.Context, h *Handler, nodeID string, subject inventory.Subject) (T, inventory.Meta, error) {
	const op = "v1.resolveRemoteInventory"
	var zero T

	if h.deps.Inventory == nil {
		// Same shape as [inventory.ErrRemoteUnavailable], which this replaces
		// for a caller that has not wired agent support in at all: a UI
		// reading `details.reason` to explain the gap must get one regardless
		// of which of the two unavailable cases produced the error.
		return zero, inventory.Meta{}, errs.New(errs.CodeUnavailable,
			"remote inventory can only be read once an agent has pushed it").
			WithOp(op).
			WithDetail("node", nodeID).
			WithDetail("subject", string(subject)).
			WithDetail("reason", "this Atlas instance has no remote inventory store configured")
	}

	if h.deps.Nodes != nil {
		if _, err := h.deps.Nodes.GetNode(ctx, nodeID); err != nil {
			if errs.CodeOf(err) == errs.CodeNotFound {
				return zero, inventory.Meta{}, errs.New(errs.CodeNotFound, "no such node").
					WithOp(op).WithDetail("node", nodeID)
			}
			return zero, inventory.Meta{}, err
		}
	}

	snap, err := h.deps.Inventory.Get(ctx, nodeID, subject)
	if err != nil {
		if errs.CodeOf(err) != errs.CodeNotFound {
			return zero, inventory.Meta{}, err
		}

		reported, herr := h.deps.Inventory.HasReported(ctx, nodeID)
		if herr != nil {
			return zero, inventory.Meta{}, herr
		}
		if reported {
			return zero, inventory.Meta{}, errs.New(errs.CodeNotImplemented,
				"this node's agent does not report %s", subject).
				WithOp(op).WithDetail("node", nodeID).WithDetail("subject", string(subject)).
				WithDetail("reason", "agent_no_subject")
		}
		return zero, inventory.Meta{}, errs.New(errs.CodeUnavailable,
			"no agent has reported for this node yet").
			WithOp(op).WithDetail("node", nodeID).WithDetail("reason", "no_agent")
	}

	var data T
	if err := json.Unmarshal(snap.Data, &data); err != nil {
		return zero, inventory.Meta{}, errs.Wrap(err, errs.CodeInternal,
			"stored inventory snapshot for %s is corrupt", subject).
			WithOp(op).WithDetail("node", nodeID).WithDetail("subject", string(subject))
	}

	return data, inventory.Meta{NodeID: nodeID, ObservedAt: snap.ObservedAt, Live: false}, nil
}
