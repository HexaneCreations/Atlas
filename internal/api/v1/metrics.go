package v1

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
	"github.com/hexane/atlas/internal/storage/metric"
)

// maxQuerySpan bounds how wide a single query may be.
//
// Two years of hourly rollups is already a large response; beyond that a
// caller wants an export, not a chart. The cap exists so one request cannot
// consume the database for minutes.
const maxQuerySpan = 2 * 365 * 24 * time.Hour

// QueryMetrics returns time series for a node.
//
//	GET /api/v1/metrics?node=<id>&metric=a,b&from=<rfc3339>&to=<rfc3339>
//	                   &resolution=raw|1m|1h&max_points=<n>
//
// Relative ranges are supported and are what a dashboard actually uses:
// `?range=6h` is resolved server-side, so every panel on a page shares one
// definition of "now" instead of each computing its own and disagreeing at the
// edges.
func (h *Handler) QueryMetrics(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.QueryMetrics"

	repo, err := h.repository()
	if err != nil {
		return err
	}

	q := r.URL.Query()

	query := metric.Query{NodeID: strings.TrimSpace(q.Get("node"))}
	if query.NodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "the node parameter is required").
			WithOp(op).WithDetail("field", "node")
	}

	if raw := q.Get("metric"); raw != "" {
		for _, name := range strings.Split(raw, ",") {
			if name = strings.TrimSpace(name); name != "" {
				query.Metrics = append(query.Metrics, name)
			}
		}
	}

	now := time.Now()
	if err := applyTimeRange(&query, q, now); err != nil {
		return err
	}

	if raw := q.Get("resolution"); raw != "" {
		query.Resolution = metric.Resolution(raw)
	}
	if raw := q.Get("max_points"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			return errs.New(errs.CodeInvalidArgument, "max_points must be a number").
				WithOp(op).WithDetail("field", "max_points")
		}
		query.MaxPoints = n
	}

	if span := query.To.Sub(query.From); span > maxQuerySpan {
		return errs.New(errs.CodeInvalidArgument,
			"the requested range is too wide; the maximum is %s", maxQuerySpan).
			WithOp(op).WithDetail("field", "from")
	}

	result, err := repo.Query(r.Context(), query)
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, result)
	return nil
}

// applyTimeRange resolves either a relative range or explicit bounds.
func applyTimeRange(query *metric.Query, q map[string][]string, now time.Time) error {
	const op = "v1.applyTimeRange"

	get := func(key string) string {
		if v, ok := q[key]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}

	if rel := get("range"); rel != "" {
		span, err := time.ParseDuration(rel)
		if err != nil || span <= 0 {
			return errs.New(errs.CodeInvalidArgument,
				"range must be a positive duration such as 15m, 6h, or 7d").
				WithOp(op).WithDetail("field", "range")
		}
		query.To = now
		query.From = now.Add(-span)
		return nil
	}

	if from := get("from"); from != "" {
		t, err := time.Parse(time.RFC3339, from)
		if err != nil {
			return errs.New(errs.CodeInvalidArgument, "from must be an RFC 3339 timestamp").
				WithOp(op).WithDetail("field", "from")
		}
		query.From = t
	}
	if to := get("to"); to != "" {
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return errs.New(errs.CodeInvalidArgument, "to must be an RFC 3339 timestamp").
				WithOp(op).WithDetail("field", "to")
		}
		query.To = t
	}
	return nil
}

// LatestResponse is the current value of every series on a node.
type LatestResponse struct {
	NodeID string               `json:"node_id"`
	Values []metric.LatestValue `json:"values"`
	Total  int                  `json:"total"`
}

// LatestMetrics returns the most recent value of every series on a node.
//
// This backs the overview, where the question is "what is happening now"
// rather than "what happened over time". Answering it with a range query and
// taking the last point would read orders of magnitude more rows for the same
// answer.
func (h *Handler) LatestMetrics(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.LatestMetrics"

	repo, err := h.repository()
	if err != nil {
		return err
	}

	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "the node parameter is required").
			WithOp(op).WithDetail("field", "node")
	}

	within := 5 * time.Minute
	if raw := r.URL.Query().Get("within"); raw != "" {
		d, err := time.ParseDuration(raw)
		if err != nil || d <= 0 {
			return errs.New(errs.CodeInvalidArgument,
				"within must be a positive duration such as 5m").
				WithOp(op).WithDetail("field", "within")
		}
		within = d
	}

	values, err := repo.Latest(r.Context(), nodeID, within)
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, LatestResponse{
		NodeID: nodeID, Values: values, Total: len(values),
	})
	return nil
}

// MetricNamesResponse lists the metrics a node reports.
type MetricNamesResponse struct {
	NodeID  string   `json:"node_id"`
	Metrics []string `json:"metrics"`
	Total   int      `json:"total"`
}

// ListMetricNames returns the distinct metrics a node has reported recently.
//
// Scoped to a recent window rather than all history: an unbounded DISTINCT
// over a hypertable is expensive, and a metric nothing has produced in a day
// should not be offered as a current choice.
func (h *Handler) ListMetricNames(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListMetricNames"

	repo, err := h.repository()
	if err != nil {
		return err
	}

	nodeID := strings.TrimSpace(r.URL.Query().Get("node"))
	if nodeID == "" {
		return errs.New(errs.CodeInvalidArgument, "the node parameter is required").
			WithOp(op).WithDetail("field", "node")
	}

	names, err := repo.ListMetricNames(r.Context(), nodeID, 24*time.Hour)
	if err != nil {
		return err
	}

	httpx.JSON(w, r, http.StatusOK, MetricNamesResponse{
		NodeID: nodeID, Metrics: names, Total: len(names),
	})
	return nil
}
