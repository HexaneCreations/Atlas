package v1

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/hexane/atlas/internal/core/monitor"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// MonitorAlertStore enables and disables monitor alert rules and mutes
// monitors. Satisfied by
// [*github.com/hexane/atlas/internal/storage/notification.Repository].
type MonitorAlertStore interface {
	ListMonitorRules(ctx context.Context, monitorID string) ([]notification.SubjectRule, error)
	SetMonitorRuleEnabled(ctx context.Context, monitorID string, trigger notification.Trigger, enabled bool, updatedBy string) (notification.SubjectRule, error)
	ClearMonitorRuleEnabled(ctx context.Context, monitorID string, trigger notification.Trigger, updatedBy string) (notification.SubjectRule, error)
	MuteMonitor(ctx context.Context, monitorID string, until time.Time) (*time.Time, error)
	// EndMonitorMuteExplicitly unmutes a monitor and resets its states; on a
	// monitor that is not muted it changes nothing. ok is false for an
	// unknown monitor.
	EndMonitorMuteExplicitly(ctx context.Context, monitorID string) (ok bool, err error)
}

// ListMonitorAlertRulesResponse is GET /monitors/{monitorID}/alert-rules.
// Each rule has the node rule shape; source is "monitor" for the monitor's
// own override.
type ListMonitorAlertRulesResponse struct {
	MonitorID string                  `json:"monitor_id"`
	Rules     []NodeAlertRuleResponse `json:"rules"`
}

// MonitorMuteResponse is the answer to a monitor mute.
type MonitorMuteResponse struct {
	MonitorID        string     `json:"monitor_id"`
	AlertsMutedUntil *time.Time `json:"alerts_muted_until"`
}

// MonitorChannelsRequest is PUT /monitors/{monitorID}/channels: the whole
// selection. An empty list means the monitor notifies no one.
type MonitorChannelsRequest struct {
	ChannelIDs *[]string `json:"channel_ids"`
}

// MonitorChannelsResponse is a monitor's channel selection.
type MonitorChannelsResponse struct {
	MonitorID  string   `json:"monitor_id"`
	ChannelIDs []string `json:"channel_ids"`
}

// monitorAlerts checks the caller like the node equivalents: notifications.manage,
// fleet-wide. It also resolves the monitor, so a change to an unknown one
// is a 404 and the audit line can name its host.
func (h *Handler) monitorAlerts(r *http.Request, op string) (MonitorAlertStore, monitor.View, error) {
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return nil, monitor.View{}, err
	}
	if h.deps.MonitorAlerts == nil {
		return nil, monitor.View{}, errs.New(errs.CodeUnavailable, "website alert settings are not available on this instance").WithOp(op)
	}
	v, err := store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if err != nil {
		return nil, monitor.View{}, err
	}
	return h.deps.MonitorAlerts, v, nil
}

// monitorAuditAttrs name a monitor in an audit line by id and URL host only.
func monitorAuditAttrs(v monitor.View, more ...slog.Attr) []slog.Attr {
	return append([]slog.Attr{slog.String("monitor_id", v.ID), slog.String("host", monitor.Host(v.URL))}, more...)
}

// ListMonitorAlertRules lists the monitor catalog rules with their
// effective value for the monitor.
func (h *Handler) ListMonitorAlertRules(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListMonitorAlertRules"
	store, v, err := h.monitorAlerts(r, op)
	if err != nil {
		return err
	}
	rules, err := store.ListMonitorRules(r.Context(), v.ID)
	if err != nil {
		return err
	}
	out := ListMonitorAlertRulesResponse{MonitorID: v.ID, Rules: make([]NodeAlertRuleResponse, 0, len(rules))}
	for _, rule := range rules {
		out.Rules = append(out.Rules, presentNodeRule(rule))
	}
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}

// SetMonitorAlertRule overrides whether one rule is enabled for the monitor.
func (h *Handler) SetMonitorAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.SetMonitorAlertRule"
	store, v, err := h.monitorAlerts(r, op)
	if err != nil {
		return err
	}
	key, err := notification.ParseRuleKeyFor(notification.SubjectMonitor, r.PathValue("ruleKey"))
	if err != nil {
		return err
	}
	var req SetNodeAlertRuleRequest
	if err = decodeStrict(r, &req); err != nil {
		return err
	}
	if req.Enabled == nil {
		return errs.New(errs.CodeInvalidArgument, "enabled is required").WithOp(op).WithDetail("field", "enabled")
	}
	rule, err := store.SetMonitorRuleEnabled(r.Context(), v.ID, key, *req.Enabled, actorName(r))
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor alert rule set", monitorAuditAttrs(v,
		slog.String("rule_key", string(key)), slog.Bool("enabled", *req.Enabled))...)
	httpx.JSON(w, r, http.StatusOK, presentNodeRule(rule))
	return nil
}

// ResetMonitorAlertRule removes the monitor's override so the rule inherits.
func (h *Handler) ResetMonitorAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ResetMonitorAlertRule"
	store, v, err := h.monitorAlerts(r, op)
	if err != nil {
		return err
	}
	key, err := notification.ParseRuleKeyFor(notification.SubjectMonitor, r.PathValue("ruleKey"))
	if err != nil {
		return err
	}
	if _, err := store.ClearMonitorRuleEnabled(r.Context(), v.ID, key, actorName(r)); err != nil {
		return err
	}
	logAdminAction(r, "monitor alert rule reset", monitorAuditAttrs(v, slog.String("rule_key", string(key)))...)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// MuteMonitorAlerts mutes a monitor's notifications, replacing any mute.
func (h *Handler) MuteMonitorAlerts(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.MuteMonitorAlerts"
	store, v, err := h.monitorAlerts(r, op)
	if err != nil {
		return err
	}
	var req MuteNodeRequest
	if err = decodeStrict(r, &req); err != nil {
		return err
	}
	until, err := notification.MuteUntil(req.Duration, time.Now())
	if err != nil {
		return err
	}
	stored, err := store.MuteMonitor(r.Context(), v.ID, until)
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor alerts muted", monitorAuditAttrs(v,
		slog.String("duration", req.Duration), slog.Time("until", until))...)
	httpx.JSON(w, r, http.StatusOK, MonitorMuteResponse{MonitorID: v.ID, AlertsMutedUntil: presentMute(stored, time.Now())})
	return nil
}

// UnmuteMonitorAlerts ends a monitor's mute early.
func (h *Handler) UnmuteMonitorAlerts(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.UnmuteMonitorAlerts"
	store, v, err := h.monitorAlerts(r, op)
	if err != nil {
		return err
	}
	ok, err := store.EndMonitorMuteExplicitly(r.Context(), v.ID)
	if err != nil {
		return err
	}
	if !ok {
		return notFoundMonitor(op, v.ID)
	}
	logAdminAction(r, "monitor alerts unmuted", monitorAuditAttrs(v)...)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func notFoundMonitor(op, monitorID string) error {
	return errs.New(errs.CodeNotFound, "monitor not found").WithOp(op).WithDetail("monitor_id", monitorID)
}

// GetMonitorChannels returns the monitor's channel selection.
func (h *Handler) GetMonitorChannels(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.GetMonitorChannels"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	v, err := store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if err != nil {
		return err
	}
	httpx.JSON(w, r, http.StatusOK, MonitorChannelsResponse{MonitorID: v.ID, ChannelIDs: nonNilIDs(v.ChannelIDs)})
	return nil
}

// SetMonitorChannels replaces the monitor's channel selection.
func (h *Handler) SetMonitorChannels(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.SetMonitorChannels"
	store, err := h.monitorWriter(r, op)
	if err != nil {
		return err
	}
	var req MonitorChannelsRequest
	if decodeStrict(r, &req) != nil {
		return errs.New(errs.CodeInvalidArgument, "invalid request body").WithOp(op)
	}
	if req.ChannelIDs == nil {
		return errs.New(errs.CodeInvalidArgument, "channel_ids is required").WithOp(op).WithDetail("field", "channel_ids")
	}
	if err = validateChannelIDs(*req.ChannelIDs); err != nil {
		return err
	}
	v, err := store.GetMonitor(r.Context(), r.PathValue("monitorID"))
	if err != nil {
		return err
	}
	ids, err := store.SetMonitorChannels(r.Context(), v.ID, *req.ChannelIDs)
	if err != nil {
		return err
	}
	logAdminAction(r, "monitor channels set", monitorAuditAttrs(v, slog.Int("channels", len(ids)))...)
	httpx.JSON(w, r, http.StatusOK, MonitorChannelsResponse{MonitorID: v.ID, ChannelIDs: nonNilIDs(ids)})
	return nil
}

// maxMonitorChannels bounds a channel selection.
const maxMonitorChannels = 50

func validateChannelIDs(ids []string) error {
	if len(ids) > maxMonitorChannels {
		return errs.New(errs.CodeInvalidArgument, "at most %d channels", maxMonitorChannels).WithDetail("field", "channel_ids")
	}
	for _, id := range ids {
		if id == "" || len(id) > 64 {
			return errs.New(errs.CodeInvalidArgument, "a channel id is not valid").WithDetail("field", "channel_ids")
		}
	}
	return nil
}

func nonNilIDs(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}
