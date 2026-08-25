package v1

import (
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/healthscore"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// HealthScoreResponse is one node's computed health score.
type HealthScoreResponse struct {
	NodeID     string                 `json:"node_id"`
	Score      float64                `json:"score"`
	Available  bool                   `json:"available"`
	Signals    []HealthSignalResponse `json:"signals"`
	ComputedAt time.Time              `json:"computed_at"`
}

// HealthSignalResponse is one provider's contribution to a node's score.
type HealthSignalResponse struct {
	Name      string  `json:"name"`
	Weight    float64 `json:"weight"`
	Score     float64 `json:"score"`
	Available bool    `json:"available"`
	Detail    string  `json:"detail,omitempty"`
}

func presentHealthScore(res healthscore.Result) HealthScoreResponse {
	signals := make([]HealthSignalResponse, 0, len(res.Signals))
	for _, s := range res.Signals {
		signals = append(signals, HealthSignalResponse{
			Name: s.Name, Weight: s.Weight, Score: s.Score, Available: s.Available, Detail: s.Detail,
		})
	}
	return HealthScoreResponse{
		NodeID: res.NodeID, Score: res.Overall, Available: res.Available,
		Signals: signals, ComputedAt: res.ComputedAt,
	}
}

// HealthScore returns one node's computed health score. `node` is optional
// and defaults to the host Atlas runs on — the same convention as the
// inventory endpoints (see [Handler.scopeFrom]).
func (h *Handler) HealthScore(w http.ResponseWriter, r *http.Request) error {
	if h.deps.HealthScore == nil {
		return errs.New(errs.CodeUnavailable, "health score is not configured").WithOp("v1.Handler.HealthScore")
	}

	nodeID, err := h.requireNode(r, user.PermissionNodeRead)
	if err != nil {
		return err
	}
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "a node id is required").WithOp("v1.Handler.HealthScore")
	}

	httpx.JSON(w, r, http.StatusOK, presentHealthScore(h.deps.HealthScore.Score(r.Context(), nodeID)))
	return nil
}

// HealthScoreFleetResponse is every known node's health score.
type HealthScoreFleetResponse struct {
	Nodes []HealthScoreResponse `json:"nodes"`
	Total int                   `json:"total"`
}

// HealthScoreFleet returns every node Atlas has observed, each with its
// computed health score.
func (h *Handler) HealthScoreFleet(w http.ResponseWriter, r *http.Request) error {
	if h.deps.HealthScore == nil {
		return errs.New(errs.CodeUnavailable, "health score is not configured").WithOp("v1.Handler.HealthScoreFleet")
	}

	repo, err := h.repository()
	if err != nil {
		return err
	}
	nodes, err := repo.ListNodes(r.Context())
	if err != nil {
		return err
	}

	out := make([]HealthScoreResponse, 0, len(nodes))
	for _, n := range nodes {
		out = append(out, presentHealthScore(h.deps.HealthScore.Score(r.Context(), n.NodeID)))
	}

	httpx.JSON(w, r, http.StatusOK, HealthScoreFleetResponse{Nodes: out, Total: len(out)})
	return nil
}
