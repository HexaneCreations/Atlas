package v1

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/core/slo"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// SLORequest is the body of a create or update request.
type SLORequest struct {
	Name                 string  `json:"name"`
	NodeID               string  `json:"node_id"`
	Signal               string  `json:"signal,omitempty"`
	Metric               string  `json:"metric"`
	Comparison           string  `json:"comparison"`
	Threshold            float64 `json:"threshold"`
	TargetPercentage     float64 `json:"target_percentage"`
	WindowSeconds        int     `json:"window_seconds"`
	WarningBudgetPercent float64 `json:"warning_budget_percent,omitempty"`
}

func (req SLORequest) toDefinition(id string) slo.Definition {
	return slo.Definition{
		ID: id, Name: req.Name, NodeID: req.NodeID, Signal: req.Signal,
		Metric: req.Metric, Comparison: alert.Comparison(req.Comparison), Threshold: req.Threshold,
		TargetPercentage: req.TargetPercentage, Window: time.Duration(req.WindowSeconds) * time.Second,
		WarningBudgetPercent: req.WarningBudgetPercent,
	}
}

// SLOResponse is one SLO definition.
type SLOResponse struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	NodeID               string    `json:"node_id"`
	Signal               string    `json:"signal,omitempty"`
	Metric               string    `json:"metric"`
	Comparison           string    `json:"comparison"`
	Threshold            float64   `json:"threshold"`
	TargetPercentage     float64   `json:"target_percentage"`
	WindowSeconds        int       `json:"window_seconds"`
	WarningBudgetPercent float64   `json:"warning_budget_percent,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

func presentSLO(d slo.Definition) SLOResponse {
	return SLOResponse{
		ID: d.ID, Name: d.Name, NodeID: d.NodeID, Signal: d.Signal,
		Metric: d.Metric, Comparison: string(d.Comparison), Threshold: d.Threshold,
		TargetPercentage: d.TargetPercentage, WindowSeconds: int(d.Window / time.Second),
		WarningBudgetPercent: d.WarningBudgetPercent, CreatedAt: d.CreatedAt, UpdatedAt: d.UpdatedAt,
	}
}

// ListSLOsResponse is every configured SLO.
type ListSLOsResponse struct {
	SLOs  []SLOResponse `json:"slos"`
	Total int           `json:"total"`
}

func (h *Handler) slos(op string) (SLOStore, error) {
	if h.deps.SLOStore == nil {
		return nil, errs.New(errs.CodeUnavailable, "SLOs are not configured").WithOp(op)
	}
	return h.deps.SLOStore, nil
}

// ListSLOs returns every configured SLO.
func (h *Handler) ListSLOs(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListSLOs"
	store, err := h.slos(op)
	if err != nil {
		return err
	}

	defs, err := store.ListSLOs(r.Context())
	if err != nil {
		return err
	}
	out := make([]SLOResponse, 0, len(defs))
	for _, d := range defs {
		out = append(out, presentSLO(d))
	}
	httpx.JSON(w, r, http.StatusOK, ListSLOsResponse{SLOs: out, Total: len(out)})
	return nil
}

// GetSLO returns one SLO definition by id.
func (h *Handler) GetSLO(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetSLO"
	store, err := h.slos(op)
	if err != nil {
		return err
	}

	def, err := store.GetSLO(r.Context(), r.PathValue("sloID"))
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentSLO(def))
	return nil
}

// CreateSLO defines a new SLO.
func (h *Handler) CreateSLO(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CreateSLO"
	store, err := h.slos(op)
	if err != nil {
		return err
	}

	var req SLORequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	def := req.toDefinition("")
	if err := def.Validate(); err != nil {
		return err
	}

	created, err := store.CreateSLO(r.Context(), def)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusCreated, presentSLO(created))
	return nil
}

// UpdateSLO replaces an existing SLO's definition.
func (h *Handler) UpdateSLO(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UpdateSLO"
	store, err := h.slos(op)
	if err != nil {
		return err
	}

	var req SLORequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	def := req.toDefinition(r.PathValue("sloID"))
	if err := def.Validate(); err != nil {
		return err
	}

	updated, err := store.UpdateSLO(r.Context(), def)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentSLO(updated))
	return nil
}

// DeleteSLO removes an SLO definition.
func (h *Handler) DeleteSLO(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.DeleteSLO"
	store, err := h.slos(op)
	if err != nil {
		return err
	}

	if err := store.DeleteSLO(r.Context(), r.PathValue("sloID")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// SLOStatusResponse is one SLO's current compliance, error budget, and
// status, evaluated over its window at request time.
type SLOStatusResponse struct {
	SLO                        SLOResponse `json:"slo"`
	Available                  bool        `json:"available"`
	SampleCount                int         `json:"sample_count"`
	Compliance                 float64     `json:"compliance"`
	ErrorBudget                float64     `json:"error_budget"`
	ErrorBudgetConsumedPercent float64     `json:"error_budget_consumed_percent"`
	Status                     slo.Status  `json:"status"`
	WindowStart                time.Time   `json:"window_start"`
	WindowEnd                  time.Time   `json:"window_end"`
	EvaluatedAt                time.Time   `json:"evaluated_at"`
}

// SLOStatus evaluates one SLO's current compliance over its window.
func (h *Handler) SLOStatus(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.SLOStatus"
	store, err := h.slos(op)
	if err != nil {
		return err
	}
	if h.deps.SLO == nil {
		return errs.New(errs.CodeUnavailable, "SLO evaluation is not configured").WithOp(op)
	}

	def, err := store.GetSLO(r.Context(), r.PathValue("sloID"))
	if err != nil {
		return err
	}
	eval, err := h.deps.SLO.Evaluate(r.Context(), def)
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, SLOStatusResponse{
		SLO: presentSLO(eval.Definition), Available: eval.Available, SampleCount: eval.SampleCount,
		Compliance: eval.Compliance, ErrorBudget: eval.ErrorBudget, ErrorBudgetConsumedPercent: eval.ErrorBudgetConsumedPercent,
		Status: eval.Status, WindowStart: eval.WindowStart, WindowEnd: eval.WindowEnd, EvaluatedAt: eval.EvaluatedAt,
	})
	return nil
}
