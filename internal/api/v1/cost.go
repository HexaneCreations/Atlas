package v1

import (
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/costanalysis"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// CostUsageResponse is the resource usage a cost estimate was computed from
// — carried alongside the estimate so a caller never has to take a cost
// number on faith.
type CostUsageResponse struct {
	CPUCores              int     `json:"cpu_cores"`
	CPUUtilizationPercent float64 `json:"cpu_utilization_percent"`
	MemoryTotalBytes      float64 `json:"memory_total_bytes"`
	MemoryUsedBytes       float64 `json:"memory_used_bytes"`
	DiskTotalBytes        float64 `json:"disk_total_bytes"`
	DiskUsedBytes         float64 `json:"disk_used_bytes"`
	NetworkRxBytesPerSec  float64 `json:"network_rx_bytes_per_sec"`
	NetworkTxBytesPerSec  float64 `json:"network_tx_bytes_per_sec"`
	RunningContainers     int     `json:"running_containers"`
	RunningProcesses      int     `json:"running_processes"`
	UptimeSeconds         float64 `json:"uptime_seconds"`
}

// CostBreakdownResponse is estimated cost per hour, by resource category.
type CostBreakdownResponse struct {
	CPU       float64 `json:"cpu"`
	Memory    float64 `json:"memory"`
	Disk      float64 `json:"disk"`
	Network   float64 `json:"network"`
	Container float64 `json:"container"`
}

// CostEstimateResponse is one node's estimated cost.
type CostEstimateResponse struct {
	NodeID             string                `json:"node_id"`
	PricingModel       string                `json:"pricing_model"`
	Usage              CostUsageResponse     `json:"usage"`
	Breakdown          CostBreakdownResponse `json:"breakdown"`
	HourlyTotal        float64               `json:"hourly_total"`
	EstimatedSinceBoot float64               `json:"estimated_since_boot"`
	ComputedAt         time.Time             `json:"computed_at"`
}

func presentCostEstimate(r costanalysis.Result) CostEstimateResponse {
	return CostEstimateResponse{
		NodeID: r.NodeID, PricingModel: r.PricingModel,
		Usage: CostUsageResponse{
			CPUCores: r.Usage.CPUCores, CPUUtilizationPercent: r.Usage.CPUUtilizationPercent,
			MemoryTotalBytes: r.Usage.MemoryTotalBytes, MemoryUsedBytes: r.Usage.MemoryUsedBytes,
			DiskTotalBytes: r.Usage.DiskTotalBytes, DiskUsedBytes: r.Usage.DiskUsedBytes,
			NetworkRxBytesPerSec: r.Usage.NetworkRxBytesPerSec, NetworkTxBytesPerSec: r.Usage.NetworkTxBytesPerSec,
			RunningContainers: r.Usage.RunningContainers, RunningProcesses: r.Usage.RunningProcesses,
			UptimeSeconds: r.Usage.UptimeSeconds,
		},
		Breakdown: CostBreakdownResponse{
			CPU: r.Breakdown.CPU, Memory: r.Breakdown.Memory, Disk: r.Breakdown.Disk,
			Network: r.Breakdown.Network, Container: r.Breakdown.Container,
		},
		HourlyTotal: r.HourlyTotal, EstimatedSinceBoot: r.EstimatedSinceBoot, ComputedAt: r.ComputedAt,
	}
}

// CostEstimate returns one node's estimated cost. `node` is optional and
// defaults to the host Atlas runs on — the same convention as the inventory
// and health score endpoints (see [Handler.scopeFrom]).
func (h *Handler) CostEstimate(w http.ResponseWriter, r *http.Request) error {
	if h.deps.CostAnalysis == nil {
		return errs.New(errs.CodeUnavailable, "cost analysis is not configured").WithOp("v1.Handler.CostEstimate")
	}

	nodeID, err := h.requireNode(r, user.PermissionNodeRead)
	if err != nil {
		return err
	}
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "a node id is required").WithOp("v1.Handler.CostEstimate")
	}

	result, err := h.deps.CostAnalysis.Estimate(r.Context(), nodeID)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentCostEstimate(result))
	return nil
}

// CostFleetResponse is every known node's estimated cost, plus the fleet
// total.
type CostFleetResponse struct {
	Nodes                   []CostEstimateResponse `json:"nodes"`
	Total                   int                    `json:"total"`
	FleetHourlyTotal        float64                `json:"fleet_hourly_total"`
	FleetEstimatedSinceBoot float64                `json:"fleet_estimated_since_boot"`
}

// CostFleet returns every node Atlas has observed, each with its estimated
// cost, plus the fleet-wide total.
func (h *Handler) CostFleet(w http.ResponseWriter, r *http.Request) error {
	if h.deps.CostAnalysis == nil {
		return errs.New(errs.CodeUnavailable, "cost analysis is not configured").WithOp("v1.Handler.CostFleet")
	}

	repo, err := h.repository()
	if err != nil {
		return err
	}
	nodes, err := repo.ListNodes(r.Context())
	if err != nil {
		return err
	}

	out := make([]CostEstimateResponse, 0, len(nodes))
	var fleetHourly, fleetSinceBoot float64
	for _, n := range nodes {
		result, err := h.deps.CostAnalysis.Estimate(r.Context(), n.NodeID)
		if err != nil {
			return err
		}
		out = append(out, presentCostEstimate(result))
		fleetHourly += result.HourlyTotal
		fleetSinceBoot += result.EstimatedSinceBoot
	}

	httpx.JSON(w, r, http.StatusOK, CostFleetResponse{
		Nodes: out, Total: len(out), FleetHourlyTotal: fleetHourly, FleetEstimatedSinceBoot: fleetSinceBoot,
	})
	return nil
}
