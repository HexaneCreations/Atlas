package v1

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/hexane/atlas/internal/api/session"
	"github.com/hexane/atlas/internal/core/notification"
	"github.com/hexane/atlas/internal/core/pageauthz"
	"github.com/hexane/atlas/internal/core/user"
	"github.com/hexane/atlas/internal/platform/errs"
	"github.com/hexane/atlas/internal/platform/httpx"
)

// NodeAlertRuleStore enables and disables catalog alert rules per node.
// Satisfied by [*github.com/hexane/atlas/internal/storage/notification.Repository].
type NodeAlertRuleStore interface {
	ListNodeRules(ctx context.Context, nodeID string) ([]notification.NodeRule, error)
	SetNodeRuleEnabled(ctx context.Context, nodeID string, trigger notification.Trigger, enabled bool, updatedBy string) (notification.NodeRule, error)
	// ClearNodeRuleEnabled removes the node's enabled override; removing one
	// that does not exist is not an error.
	ClearNodeRuleEnabled(ctx context.Context, nodeID string, trigger notification.Trigger, updatedBy string) (notification.NodeRule, error)
}

// NodeAlertRuleResponse is one catalog rule as it applies to a node.
type NodeAlertRuleResponse struct {
	Key             string `json:"key"`
	Area            string `json:"area"`
	Title           string `json:"title"`
	Description     string `json:"description"`
	DefaultSeverity string `json:"default_severity"`
	DefaultEnabled  bool   `json:"default_enabled"`
	// GlobalEnabled is the global rule's value, null when there is none.
	GlobalEnabled *bool `json:"global_enabled"`
	// Override is the node's own value, null when it inherits.
	Override *bool `json:"override"`
	// Enabled is the effective value; Source says where it comes from.
	Enabled bool   `json:"enabled"`
	Source  string `json:"source"`
}

// ListNodeAlertRulesResponse is GET /nodes/{nodeID}/alert-rules.
type ListNodeAlertRulesResponse struct {
	NodeID string                  `json:"node_id"`
	Rules  []NodeAlertRuleResponse `json:"rules"`
}

// SetNodeAlertRuleRequest is the body of PUT /nodes/{nodeID}/alert-rules/{ruleKey}.
type SetNodeAlertRuleRequest struct {
	Enabled *bool `json:"enabled"`
}

func presentNodeRule(r notification.NodeRule) NodeAlertRuleResponse {
	return NodeAlertRuleResponse{
		Key: string(r.Key), Area: string(r.Area), Title: r.Title, Description: r.Description,
		DefaultSeverity: string(r.DefaultSeverity), DefaultEnabled: r.DefaultEnabled,
		GlobalEnabled: r.GlobalEnabled, Override: r.Override, Enabled: r.Enabled, Source: string(r.Source),
	}
}

// nodeAlertRules authorizes the caller like muting does: a rule change
// silences or un-silences a node for everyone.
func (h *Handler) nodeAlertRules(r *http.Request, op string) (NodeAlertRuleStore, error) {
	if err := h.requirePermission(r, user.PermissionNotificationsManage, pageauthz.PageNone); err != nil {
		return nil, err
	}
	if h.deps.NodeAlertRules == nil {
		return nil, errs.New(errs.CodeUnavailable, "node alert rules are not available on this instance").WithOp(op)
	}
	return h.deps.NodeAlertRules, nil
}

func actorName(r *http.Request) string {
	principal, _ := session.PrincipalFrom(r.Context())
	return principal.Username
}

// ListNodeAlertRules lists every catalog rule with its effective value for the node.
func (h *Handler) ListNodeAlertRules(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ListNodeAlertRules"
	store, err := h.nodeAlertRules(r, op)
	if err != nil {
		return err
	}
	nodeID := r.PathValue("nodeID")
	rules, err := store.ListNodeRules(r.Context(), nodeID)
	if err != nil {
		return err
	}
	out := ListNodeAlertRulesResponse{NodeID: nodeID, Rules: make([]NodeAlertRuleResponse, 0, len(rules))}
	for _, rule := range rules {
		out.Rules = append(out.Rules, presentNodeRule(rule))
	}
	httpx.JSON(w, r, http.StatusOK, out)
	return nil
}

// SetNodeAlertRule overrides whether one rule is enabled for the node.
func (h *Handler) SetNodeAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.SetNodeAlertRule"
	store, err := h.nodeAlertRules(r, op)
	if err != nil {
		return err
	}
	key, err := notification.ParseRuleKey(r.PathValue("ruleKey"))
	if err != nil {
		return err
	}
	var req SetNodeAlertRuleRequest
	if err := decodeStrict(r, &req); err != nil {
		return err
	}
	if req.Enabled == nil {
		return errs.New(errs.CodeInvalidArgument, "enabled is required").WithOp(op).WithDetail("field", "enabled")
	}

	nodeID := r.PathValue("nodeID")
	rule, err := store.SetNodeRuleEnabled(r.Context(), nodeID, key, *req.Enabled, actorName(r))
	if err != nil {
		return err
	}
	logAdminAction(r, "node alert rule set", slog.String("node_id", nodeID),
		slog.String("rule_key", string(key)), slog.Bool("enabled", *req.Enabled))
	httpx.JSON(w, r, http.StatusOK, presentNodeRule(rule))
	return nil
}

// ResetNodeAlertRule removes the node's override so the rule inherits again.
func (h *Handler) ResetNodeAlertRule(w http.ResponseWriter, r *http.Request) error {
	const op = "v1.Handler.ResetNodeAlertRule"
	store, err := h.nodeAlertRules(r, op)
	if err != nil {
		return err
	}
	key, err := notification.ParseRuleKey(r.PathValue("ruleKey"))
	if err != nil {
		return err
	}
	nodeID := r.PathValue("nodeID")
	if _, err := store.ClearNodeRuleEnabled(r.Context(), nodeID, key, actorName(r)); err != nil {
		return err
	}
	logAdminAction(r, "node alert rule reset", slog.String("node_id", nodeID), slog.String("rule_key", string(key)))
	w.WriteHeader(http.StatusNoContent)
	return nil
}
