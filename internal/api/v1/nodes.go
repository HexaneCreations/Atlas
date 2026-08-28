package v1

import (
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/storage/metric"
)

// NodeResponse is a machine as the API presents it.
//
// Status and uptime are computed at read time rather than stored. A node that
// stops reporting becomes stale without anything having to notice and write a
// status — which matters, because the most likely reason a node goes quiet is
// that whatever would have written the status is what died.
type NodeResponse struct {
	NodeID   string `json:"node_id"`
	Hostname string `json:"hostname"`

	OS           string `json:"os,omitempty"`
	Platform     string `json:"platform,omitempty"`
	Kernel       string `json:"kernel,omitempty"`
	Architecture string `json:"architecture,omitempty"`
	CPUCores     int    `json:"cpu_cores,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	Environment  string `json:"environment,omitempty"`

	Status        metric.NodeStatus `json:"status"`
	BootTime      time.Time         `json:"boot_time,omitzero"`
	UptimeSeconds float64           `json:"uptime_seconds,omitempty"`
	FirstSeenAt   time.Time         `json:"first_seen_at"`
	LastSeenAt    time.Time         `json:"last_seen_at"`
	// SecondsSinceSeen makes staleness legible without the client doing clock
	// arithmetic against a timestamp that may disagree with its own clock.
	SecondsSinceSeen float64 `json:"seconds_since_seen"`
	// ClockSkewSeconds is absent until an agent has pushed at least once.
	ClockSkewSeconds *float64 `json:"clock_skew_seconds,omitempty"`

	// PublicIP is the source address the control plane observed this node's
	// most recent connection from — server-observed, not agent-reported.
	// Empty until a connection over a path that captures it.
	PublicIP string `json:"public_ip,omitempty"`
	// Addresses is the node's own per-interface addressing, promoted from the
	// "network" inventory snapshot. Populated only on the single-node
	// endpoint, not the list.
	Addresses []metric.NodeAddress `json:"addresses,omitempty"`
}

// ListNodesResponse is the node collection.
type ListNodesResponse struct {
	Nodes []NodeResponse `json:"nodes"`
	Total int            `json:"total"`
}

// ListNodes returns every machine the caller is authorized to see: every
// machine Atlas has observed, filtered to the caller's own user_node_roles
// grants for node.read — a node-scoped viewer sees only the node(s) they
// were granted, a fleet-wide grant sees everything, and a caller with no
// grant at all sees an empty list rather than every node or a 403. Compare
// [Handler.requireScope], which gates a single node-scoped request
// pass/fail; this instead filters a result set, since "which nodes can you
// see" has no one node to gate against.
func (h *Handler) ListNodes(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListNodes"

	repo, err := h.repository()
	if err != nil {
		return err
	}

	nodes, err := repo.ListNodes(r.Context())
	if err != nil {
		return err
	}

	if h.deps.Authz != nil {
		principal, ok := session.PrincipalFrom(r.Context())
		if !ok {
			return errs.New(errs.CodeUnauthenticated, "authentication required").WithOp(op)
		}
		// Nodes is a fleet-only page (see pageauthz.FleetOnlyPages): there is
		// no per-node grant to filter by, so this is one pass/fail check —
		// can this caller reach the Nodes page at all — before the existing
		// node.read filtering below narrows which nodes they see within it.
		// A denial here is a 403, not an empty list: an empty list would be
		// indistinguishable from "no node.read grants", a different fact.
		if h.deps.PageAuthz != nil {
			if err := h.deps.PageAuthz.Require(r.Context(), principal.UserID, pageauthz.PageNodes, ""); err != nil {
				return err
			}
		}
		fleetWide, allowed, err := h.deps.Authz.AuthorizedNodes(r.Context(), principal, user.PermissionNodeRead)
		if err != nil {
			return err
		}
		if !fleetWide {
			filtered := make([]metric.Node, 0, len(nodes))
			for _, n := range nodes {
				if allowed[n.NodeID] {
					filtered = append(filtered, n)
				}
			}
			nodes = filtered
		}
	}

	now := time.Now()
	out := make([]NodeResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, h.presentNode(n, now))
	}

	httpx.JSON(w, r, http.StatusOK, ListNodesResponse{Nodes: out, Total: len(out)})
	return nil
}

// GetNode returns one machine, gated by node.read for that specific node —
// unlike [Handler.ListNodes]'s filtered-list shape, a direct lookup by id
// has exactly one node to gate against, so this uses [Authorizer.Require]
// the same way every other node-scoped endpoint does.
func (h *Handler) GetNode(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetNode"

	repo, err := h.repository()
	if err != nil {
		return err
	}

	nodeID := r.PathValue("nodeID")
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "a node id is required").
			WithOp(op).WithDetail("field", "nodeID")
	}

	if h.deps.Authz != nil {
		principal, ok := session.PrincipalFrom(r.Context())
		if !ok {
			return errs.New(errs.CodeUnauthenticated, "authentication required").WithOp(op)
		}
		if err := h.deps.Authz.Require(r.Context(), principal, user.PermissionNodeRead, nodeID); err != nil {
			return err
		}
		if h.deps.PageAuthz != nil {
			if err := h.deps.PageAuthz.Require(r.Context(), principal.UserID, pageauthz.PageNodes, ""); err != nil {
				return err
			}
		}
	}

	node, err := repo.GetNode(r.Context(), nodeID)
	if err != nil {
		return err
	}

	resp := h.presentNode(node, time.Now())
	addrs, err := repo.ListNodeAddresses(r.Context(), nodeID)
	if err != nil {
		return err
	}
	resp.Addresses = addrs

	httpx.JSON(w, r, http.StatusOK, resp)
	return nil
}

func (h *Handler) presentNode(n metric.Node, now time.Time) NodeResponse {
	return NodeResponse{
		NodeID:           n.NodeID,
		Hostname:         n.Hostname,
		OS:               n.OS,
		Platform:         n.Platform,
		Kernel:           n.Kernel,
		Architecture:     n.Architecture,
		CPUCores:         n.CPUCores,
		AgentVersion:     n.AgentVersion,
		Environment:      n.Environment,
		Status:           n.Status(h.deps.Config.Collection.DefaultInterval, now),
		BootTime:         n.BootTime,
		UptimeSeconds:    n.UptimeSeconds(),
		FirstSeenAt:      n.FirstSeenAt,
		LastSeenAt:       n.LastSeenAt,
		SecondsSinceSeen: now.Sub(n.LastSeenAt).Seconds(),
		ClockSkewSeconds: n.ClockSkewSeconds,
		PublicIP:         n.PublicIP,
	}
}

// repository returns the metric repository, or a typed error explaining why it
// is unavailable.
//
// The pipeline is started before the HTTP listener binds, so in a running
// Atlas this never fails. It can fail in a test that builds the API without a
// pipeline, and returning `unavailable` rather than panicking keeps that a
// diagnosable 503 instead of a crashed process.
func (h *Handler) repository() (*metric.Repository, error) {
	if h.deps.Collection == nil {
		return nil, errs.New(errs.CodeUnavailable, "collection is not configured").
			WithOp("v1.Handler.repository")
	}
	repo := h.deps.Collection.Repository()
	if repo == nil {
		return nil, errs.New(errs.CodeUnavailable, "collection is still starting").
			WithOp("v1.Handler.repository")
	}
	return repo, nil
}
