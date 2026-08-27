package v1

import (
	"github.com/hexane/atlas/internal/core/pageauthz"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/goldensignals"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// GoldenSignalResponse is one Golden Signal's measurement.
type GoldenSignalResponse struct {
	Name      goldensignals.SignalName `json:"name"`
	Available bool                     `json:"available"`
	Value     float64                  `json:"value,omitempty"`
	Unit      string                   `json:"unit,omitempty"`
	Detail    string                   `json:"detail,omitempty"`
}

// GoldenSignalsResponse is one node's Golden Signals.
type GoldenSignalsResponse struct {
	NodeID     string                 `json:"node_id"`
	Signals    []GoldenSignalResponse `json:"signals"`
	ComputedAt time.Time              `json:"computed_at"`
}

func presentGoldenSignals(res goldensignals.Result) GoldenSignalsResponse {
	signals := make([]GoldenSignalResponse, 0, len(res.Signals))
	for _, s := range res.Signals {
		signals = append(signals, GoldenSignalResponse{
			Name: s.Name, Available: s.Available, Value: s.Value, Unit: s.Unit, Detail: s.Detail,
		})
	}
	return GoldenSignalsResponse{NodeID: res.NodeID, Signals: signals, ComputedAt: res.ComputedAt}
}

// GoldenSignals returns one node's Golden Signals. `node` is optional and
// defaults to the host Atlas runs on — the same convention as the other
// scoped endpoints (see [Handler.scopeFrom]).
func (h *Handler) GoldenSignals(w http.ResponseWriter, r *http.Request) error {
	if h.deps.GoldenSignals == nil {
		return errs.New(errs.CodeUnavailable, "golden signals are not configured").WithOp("v1.Handler.GoldenSignals")
	}

	nodeID, err := h.requireNode(r, user.PermissionNodeRead, pageauthz.PageNone)
	if err != nil {
		return err
	}
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "a node id is required").WithOp("v1.Handler.GoldenSignals")
	}

	result, err := h.deps.GoldenSignals.Measure(r.Context(), nodeID)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentGoldenSignals(result))
	return nil
}

// GoldenSignalsFleetResponse is every known node's Golden Signals.
type GoldenSignalsFleetResponse struct {
	Nodes []GoldenSignalsResponse `json:"nodes"`
	Total int                     `json:"total"`
}

// GoldenSignalsFleet returns every node Atlas has observed, each with its
// Golden Signals.
func (h *Handler) GoldenSignalsFleet(w http.ResponseWriter, r *http.Request) error {
	if h.deps.GoldenSignals == nil {
		return errs.New(errs.CodeUnavailable, "golden signals are not configured").WithOp("v1.Handler.GoldenSignalsFleet")
	}

	repo, err := h.repository()
	if err != nil {
		return err
	}
	nodes, err := repo.ListNodes(r.Context())
	if err != nil {
		return err
	}

	out := make([]GoldenSignalsResponse, 0, len(nodes))
	for _, n := range nodes {
		result, err := h.deps.GoldenSignals.Measure(r.Context(), n.NodeID)
		if err != nil {
			return err
		}
		out = append(out, presentGoldenSignals(result))
	}
	httpx.JSON(w, r, http.StatusOK, GoldenSignalsFleetResponse{Nodes: out, Total: len(out)})
	return nil
}
