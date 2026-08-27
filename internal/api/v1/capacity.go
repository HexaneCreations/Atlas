package v1

import (
	"github.com/hexane/atlas/internal/core/pageauthz"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/capacityplanning"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// CapacityDomainResponse is one provider's capacity assessment.
type CapacityDomainResponse struct {
	Name               string                  `json:"name"`
	Available          bool                    `json:"available"`
	UtilizationPercent float64                 `json:"utilization_percent"`
	RemainingCapacity  float64                 `json:"remaining_capacity"`
	RemainingUnit      string                  `json:"remaining_unit,omitempty"`
	Status             capacityplanning.Status `json:"status"`
	Detail             string                  `json:"detail,omitempty"`
}

// CapacitySummaryResponse is one node's capacity assessment.
type CapacitySummaryResponse struct {
	NodeID     string                   `json:"node_id"`
	Status     capacityplanning.Status  `json:"status"`
	Domains    []CapacityDomainResponse `json:"domains"`
	ComputedAt time.Time                `json:"computed_at"`
}

func presentCapacitySummary(s capacityplanning.Summary) CapacitySummaryResponse {
	domains := make([]CapacityDomainResponse, 0, len(s.Domains))
	for _, d := range s.Domains {
		domains = append(domains, CapacityDomainResponse{
			Name: d.Name, Available: d.Available, UtilizationPercent: d.UtilizationPercent,
			RemainingCapacity: d.RemainingCapacity, RemainingUnit: d.RemainingUnit,
			Status: d.Status, Detail: d.Detail,
		})
	}
	return CapacitySummaryResponse{NodeID: s.NodeID, Status: s.Status, Domains: domains, ComputedAt: s.ComputedAt}
}

// CapacitySummary returns one node's capacity assessment. `node` is
// optional and defaults to the host Atlas runs on — the same convention as
// the inventory, health score, and cost estimate endpoints (see
// [Handler.scopeFrom]).
func (h *Handler) CapacitySummary(w http.ResponseWriter, r *http.Request) error {
	if h.deps.CapacityPlanning == nil {
		return errs.New(errs.CodeUnavailable, "capacity planning is not configured").WithOp("v1.Handler.CapacitySummary")
	}

	nodeID, err := h.requireNode(r, user.PermissionNodeRead, pageauthz.PageNone)
	if err != nil {
		return err
	}
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "a node id is required").WithOp("v1.Handler.CapacitySummary")
	}

	summary, err := h.deps.CapacityPlanning.Assess(r.Context(), nodeID)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentCapacitySummary(summary))
	return nil
}

// CapacityFleetResponse is every known node's capacity assessment, plus a
// fleet-wide rollup.
type CapacityFleetResponse struct {
	Nodes        []CapacitySummaryResponse `json:"nodes"`
	Total        int                       `json:"total"`
	Status       capacityplanning.Status   `json:"status"`
	StatusCounts map[string]int            `json:"status_counts"`
}

func statusRank(s capacityplanning.Status) int {
	switch s {
	case capacityplanning.StatusCritical:
		return 2
	case capacityplanning.StatusWarning:
		return 1
	default:
		return 0
	}
}

// CapacityFleet returns every node Atlas has observed, each with its
// capacity assessment, plus the fleet-wide worst status.
func (h *Handler) CapacityFleet(w http.ResponseWriter, r *http.Request) error {
	if h.deps.CapacityPlanning == nil {
		return errs.New(errs.CodeUnavailable, "capacity planning is not configured").WithOp("v1.Handler.CapacityFleet")
	}

	repo, err := h.repository()
	if err != nil {
		return err
	}
	nodes, err := repo.ListNodes(r.Context())
	if err != nil {
		return err
	}

	out := make([]CapacitySummaryResponse, 0, len(nodes))
	fleetStatus := capacityplanning.StatusHealthy
	counts := map[string]int{
		string(capacityplanning.StatusHealthy):  0,
		string(capacityplanning.StatusWarning):  0,
		string(capacityplanning.StatusCritical): 0,
	}
	for _, n := range nodes {
		summary, err := h.deps.CapacityPlanning.Assess(r.Context(), n.NodeID)
		if err != nil {
			return err
		}
		out = append(out, presentCapacitySummary(summary))
		counts[string(summary.Status)]++
		if statusRank(summary.Status) > statusRank(fleetStatus) {
			fleetStatus = summary.Status
		}
	}

	httpx.JSON(w, r, http.StatusOK, CapacityFleetResponse{
		Nodes: out, Total: len(out), Status: fleetStatus, StatusCounts: counts,
	})
	return nil
}
