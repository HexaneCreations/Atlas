package v1

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hexane/atlas/internal/core/alert"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

const (
	defaultAlertHistoryLimit = 50
	maxAlertHistoryLimit     = 500
)

// AlertRuleRequest is the body of a create or update request.
type AlertRuleRequest struct {
	Name        string  `json:"name"`
	Description string  `json:"description,omitempty"`
	Enabled     *bool   `json:"enabled,omitempty"`
	Kind        string  `json:"kind"`
	Severity    string  `json:"severity"`
	Metric      string  `json:"metric,omitempty"`
	Comparison  string  `json:"comparison,omitempty"`
	Threshold   float64 `json:"threshold,omitempty"`
	ForSeconds  int     `json:"for_seconds,omitempty"`
	NodeID      string  `json:"node_id,omitempty"`
	Topic       string  `json:"topic,omitempty"`
	Subject     string  `json:"subject,omitempty"`
}

func (req AlertRuleRequest) toRule(id string) alert.Rule {
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	return alert.Rule{
		ID: id, Name: req.Name, Description: req.Description, Enabled: enabled,
		Kind: alert.Kind(req.Kind), Severity: alert.Severity(req.Severity),
		Metric: req.Metric, Comparison: alert.Comparison(req.Comparison), Threshold: req.Threshold,
		For: time.Duration(req.ForSeconds) * time.Second, NodeID: req.NodeID,
		Topic: req.Topic, Subject: req.Subject,
	}
}

// AlertRuleResponse is one alert rule.
type AlertRuleResponse struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Enabled     bool      `json:"enabled"`
	Kind        string    `json:"kind"`
	Severity    string    `json:"severity"`
	Metric      string    `json:"metric,omitempty"`
	Comparison  string    `json:"comparison,omitempty"`
	Threshold   float64   `json:"threshold,omitempty"`
	ForSeconds  int       `json:"for_seconds,omitempty"`
	NodeID      string    `json:"node_id,omitempty"`
	Topic       string    `json:"topic,omitempty"`
	Subject     string    `json:"subject,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func presentRule(r alert.Rule) AlertRuleResponse {
	return AlertRuleResponse{
		ID: r.ID, Name: r.Name, Description: r.Description, Enabled: r.Enabled,
		Kind: string(r.Kind), Severity: string(r.Severity),
		Metric: r.Metric, Comparison: string(r.Comparison), Threshold: r.Threshold,
		ForSeconds: int(r.For / time.Second), NodeID: r.NodeID,
		Topic: r.Topic, Subject: r.Subject, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
	}
}

// ListAlertRulesResponse is every configured rule.
type ListAlertRulesResponse struct {
	Rules []AlertRuleResponse `json:"rules"`
	Total int                 `json:"total"`
}

func (h *Handler) alerts(op string) (AlertStore, error) {
	if h.deps.Alerts == nil {
		return nil, errs.New(errs.CodeNotImplemented, "the alert rule engine is not enabled").WithOp(op)
	}
	return h.deps.Alerts, nil
}

// ListAlertRules returns every configured rule.
func (h *Handler) ListAlertRules(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListAlertRules"
	store, err := h.alerts(op)
	if err != nil {
		return err
	}

	rules, err := store.ListRules(r.Context())
	if err != nil {
		return err
	}
	out := make([]AlertRuleResponse, 0, len(rules))
	for _, rule := range rules {
		out = append(out, presentRule(rule))
	}
	httpx.JSON(w, r, http.StatusOK, ListAlertRulesResponse{Rules: out, Total: len(out)})
	return nil
}

// GetAlertRule returns one rule by id.
func (h *Handler) GetAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetAlertRule"
	store, err := h.alerts(op)
	if err != nil {
		return err
	}

	rule, err := store.GetRule(r.Context(), r.PathValue("ruleID"))
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentRule(rule))
	return nil
}

// CreateAlertRule defines a new rule.
func (h *Handler) CreateAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.CreateAlertRule"
	store, err := h.alerts(op)
	if err != nil {
		return err
	}

	var req AlertRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	rule := req.toRule("")
	if err := rule.Validate(); err != nil {
		return err
	}

	created, err := store.CreateRule(r.Context(), rule)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusCreated, presentRule(created))
	return nil
}

// UpdateAlertRule replaces an existing rule's definition.
func (h *Handler) UpdateAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UpdateAlertRule"
	store, err := h.alerts(op)
	if err != nil {
		return err
	}

	var req AlertRuleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	rule := req.toRule(r.PathValue("ruleID"))
	if err := rule.Validate(); err != nil {
		return err
	}

	updated, err := store.UpdateRule(r.Context(), rule)
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, presentRule(updated))
	return nil
}

// DeleteAlertRule removes a rule. Its history survives as an audit trail.
func (h *Handler) DeleteAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.DeleteAlertRule"
	store, err := h.alerts(op)
	if err != nil {
		return err
	}

	if err := store.DeleteRule(r.Context(), r.PathValue("ruleID")); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// AlertStateResponse is one rule's current status against one node's series.
type AlertStateResponse struct {
	RuleID       string    `json:"rule_id"`
	NodeID       string    `json:"node_id"`
	SeriesKey    string    `json:"series_key,omitempty"`
	State        string    `json:"state"`
	Value        float64   `json:"value,omitempty"`
	Message      string    `json:"message,omitempty"`
	PendingSince time.Time `json:"pending_since,omitzero"`
	FiredAt      time.Time `json:"fired_at,omitzero"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// ListActiveAlertsResponse is every rule currently pending or firing.
type ListActiveAlertsResponse struct {
	Alerts []AlertStateResponse `json:"alerts"`
	Total  int                  `json:"total"`
}

// ListActiveAlerts returns every alert currently pending or firing.
func (h *Handler) ListActiveAlerts(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListActiveAlerts"
	store, err := h.alerts(op)
	if err != nil {
		return err
	}

	states, err := store.ListActiveStates(r.Context())
	if err != nil {
		return err
	}
	out := make([]AlertStateResponse, 0, len(states))
	for _, s := range states {
		out = append(out, AlertStateResponse{
			RuleID: s.RuleID, NodeID: s.NodeID, SeriesKey: s.SeriesKey, State: string(s.State),
			Value: s.Value, Message: s.Message, PendingSince: s.PendingSince, FiredAt: s.FiredAt, UpdatedAt: s.UpdatedAt,
		})
	}
	httpx.JSON(w, r, http.StatusOK, ListActiveAlertsResponse{Alerts: out, Total: len(out)})
	return nil
}

// AlertHistoryEntryResponse is one durable firing or resolution.
type AlertHistoryEntryResponse struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	RuleID   string    `json:"rule_id"`
	NodeID   string    `json:"node_id"`
	State    string    `json:"state"`
	Severity string    `json:"severity,omitempty"`
	Value    float64   `json:"value,omitempty"`
	Message  string    `json:"message,omitempty"`
}

// ListAlertHistoryResponse is a page of the alert history log.
type ListAlertHistoryResponse struct {
	History []AlertHistoryEntryResponse `json:"history"`
	Total   int                         `json:"total"`
}

// ListAlertHistory returns durable alert transitions, newest first.
func (h *Handler) ListAlertHistory(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListAlertHistory"
	store, err := h.alerts(op)
	if err != nil {
		return err
	}

	q := r.URL.Query()
	filter := alert.HistoryFilter{
		RuleID: strings.TrimSpace(q.Get("rule_id")),
		NodeID: strings.TrimSpace(q.Get("node")),
		Limit:  defaultAlertHistoryLimit,
	}
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			return errs.New(errs.CodeInvalidArgument, "limit must be a positive number").WithOp(op).WithDetail("field", "limit")
		}
		filter.Limit = min(n, maxAlertHistoryLimit)
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

	entries, err := store.QueryHistory(r.Context(), filter)
	if err != nil {
		return err
	}
	out := make([]AlertHistoryEntryResponse, 0, len(entries))
	for _, e := range entries {
		out = append(out, AlertHistoryEntryResponse{
			ID: e.ID, Time: e.Time, RuleID: e.RuleID, NodeID: e.NodeID,
			State: string(e.State), Severity: string(e.Severity), Value: e.Value, Message: e.Message,
		})
	}
	httpx.JSON(w, r, http.StatusOK, ListAlertHistoryResponse{History: out, Total: len(out)})
	return nil
}
