package v1

import (
	"net/http"
	"time"

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
}

// ListNodesResponse is the node collection.
type ListNodesResponse struct {
	Nodes []NodeResponse `json:"nodes"`
	Total int            `json:"total"`
}

// ListNodes returns every machine Atlas has observed.
func (h *Handler) ListNodes(w http.ResponseWriter, r *http.Request) error {
	repo, err := h.repository()
	if err != nil {
		return err
	}

	nodes, err := repo.ListNodes(r.Context())
	if err != nil {
		return err
	}

	now := time.Now()
	out := make([]NodeResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, h.presentNode(n, now))
	}

	httpx.JSON(w, r, http.StatusOK, ListNodesResponse{Nodes: out, Total: len(out)})
	return nil
}

// GetNode returns one machine.
func (h *Handler) GetNode(w http.ResponseWriter, r *http.Request) error {
	repo, err := h.repository()
	if err != nil {
		return err
	}

	nodeID := r.PathValue("nodeID")
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "a node id is required").
			WithOp("v1.Handler.GetNode").WithDetail("field", "nodeID")
	}

	node, err := repo.GetNode(r.Context(), nodeID)
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, h.presentNode(node, time.Now()))
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
