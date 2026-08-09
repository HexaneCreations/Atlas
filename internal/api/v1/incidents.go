package v1

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/core/incident"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

const (
	defaultIncidentsLimit = 50
	maxIncidentsLimit     = 500
)

// IncidentResponse is one incident, without its members — the concise view
// a list renders.
type IncidentResponse struct {
	ID              string    `json:"id"`
	Title           string    `json:"title"`
	Status          string    `json:"status"`
	Severity        string    `json:"severity"`
	RootCauseKind   string    `json:"root_cause_kind,omitempty"`
	RootCauseRefID  string    `json:"root_cause_ref_id,omitempty"`
	RootCauseTopic  string    `json:"root_cause_topic,omitempty"`
	OpenedAt        time.Time `json:"opened_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	ResolvedAt      time.Time `json:"resolved_at,omitzero"`
	DurationSeconds float64   `json:"duration_seconds"`
}

func presentIncident(inc incident.Incident, now time.Time) IncidentResponse {
	return IncidentResponse{
		ID: inc.ID, Title: inc.Title, Status: string(inc.Status), Severity: string(inc.Severity),
		RootCauseKind: string(inc.RootCauseKind), RootCauseRefID: inc.RootCauseRefID, RootCauseTopic: inc.RootCauseTopic,
		OpenedAt: inc.OpenedAt, UpdatedAt: inc.UpdatedAt, ResolvedAt: inc.ResolvedAt,
		DurationSeconds: inc.DurationSeconds(now),
	}
}

// ListIncidentsResponse is a page of incidents.
type ListIncidentsResponse struct {
	Incidents []IncidentResponse `json:"incidents"`
	Total     int                `json:"total"`
}

func (h *Handler) incidents(op string) (IncidentStore, error) {
	if h.deps.Incidents == nil {
		return nil, errs.New(errs.CodeNotImplemented, "the incident timeline is not enabled").WithOp(op)
	}
	return h.deps.Incidents, nil
}

// ListIncidents returns incidents, newest-updated first. Passing
// ?status=open serves the active view; omitting it serves the historical
// view, both filterable by node and time range.
func (h *Handler) ListIncidents(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListIncidents"
	store, err := h.incidents(op)
	if err != nil {
		return err
	}

	q := r.URL.Query()
	filter := incident.Filter{
		Status: incident.Status(strings.TrimSpace(q.Get("status"))),
		NodeID: strings.TrimSpace(q.Get("node")),
		Limit:  defaultIncidentsLimit,
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return errs.New(errs.CodeInvalidArgument, "limit must be a positive number").WithOp(op).WithDetail("field", "limit")
		}
		filter.Limit = min(n, maxIncidentsLimit)
	}
	for field, dst := range map[string]*time.Time{"since": &filter.Since, "before": &filter.Before} {
		raw := q.Get(field)
		if raw == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return errs.New(errs.CodeInvalidArgument, "%s must be RFC3339", field).WithOp(op).WithDetail("field", field)
		}
		*dst = t
	}

	incidents, err := store.ListIncidents(r.Context(), filter)
	if err != nil {
		return err
	}

	now := time.Now()
	out := make([]IncidentResponse, 0, len(incidents))
	for _, inc := range incidents {
		out = append(out, presentIncident(inc, now))
	}
	httpx.JSON(w, r, http.StatusOK, ListIncidentsResponse{Incidents: out, Total: len(out)})
	return nil
}

// IncidentMemberResponse is one event or alert firing folded into an
// incident.
type IncidentMemberResponse struct {
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	RefID       string    `json:"ref_id"`
	NodeID      string    `json:"node_id"`
	Topic       string    `json:"topic"`
	Severity    string    `json:"severity"`
	Time        time.Time `json:"time"`
	IsRootCause bool      `json:"is_root_cause,omitempty"`
}

// IncidentDetailResponse is an incident with its full member history and
// computed fleet impact.
type IncidentDetailResponse struct {
	IncidentResponse
	Members          []IncidentMemberResponse `json:"members"`
	AffectedNodes    []string                 `json:"affected_nodes"`
	AffectedSubjects []string                 `json:"affected_subjects,omitempty"`
}

// GetIncident returns one incident with its complete correlated history.
func (h *Handler) GetIncident(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetIncident"
	store, err := h.incidents(op)
	if err != nil {
		return err
	}

	detail, err := store.GetDetail(r.Context(), r.PathValue("incidentID"))
	if err != nil {
		return err
	}

	now := time.Now()
	members := make([]IncidentMemberResponse, 0, len(detail.Members))
	for _, m := range detail.Members {
		members = append(members, IncidentMemberResponse{
			ID: m.ID, Kind: string(m.Kind), RefID: m.RefID, NodeID: m.NodeID,
			Topic: m.Topic, Severity: string(m.Severity), Time: m.Time, IsRootCause: m.IsRootCause,
		})
	}

	httpx.JSON(w, r, http.StatusOK, IncidentDetailResponse{
		IncidentResponse: presentIncident(detail.Incident, now),
		Members:          members,
		AffectedNodes:    detail.AffectedNodes,
		AffectedSubjects: detail.AffectedSubjects,
	})
	return nil
}
