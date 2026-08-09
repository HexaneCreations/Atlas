package healthscore

import (
	"context"
	"fmt"

	"github.com/hexane/atlas/internal/core/alert"
)

// AlertRules is the alert rule read path, shared by [AlertProvider] and
// [MetricsProvider]. Satisfied directly by
// [github.com/hexane/atlas/internal/core/alert.Store] and by the app
// layer's existing lazy alert store — see internal/app.
type AlertRules interface {
	ListRules(ctx context.Context) ([]alert.Rule, error)
}

// ActiveAlertStates is the active alert state read path. Satisfied directly
// by [github.com/hexane/atlas/internal/core/alert.Store] and by the app
// layer's existing lazy alert store — see internal/app.
type ActiveAlertStates interface {
	ListActiveStates(ctx context.Context) ([]alert.AlertState, error)
}

// AlertProvider scores a node on its currently firing alert rules: no firing
// alerts scores 100, and each firing alert lowers the score by its
// severity.
type AlertProvider struct {
	Rules  AlertRules
	States ActiveAlertStates
}

func (p AlertProvider) Name() string { return "alerts" }

func (p AlertProvider) Score(ctx context.Context, nodeID string) (Signal, error) {
	states, err := p.States.ListActiveStates(ctx)
	if err != nil {
		return Signal{}, err
	}
	rules, err := p.Rules.ListRules(ctx)
	if err != nil {
		return Signal{}, err
	}

	severityOf := make(map[string]alert.Severity, len(rules))
	for _, r := range rules {
		severityOf[r.ID] = r.Severity
	}

	var critical, warning int
	for _, s := range states {
		if s.NodeID != nodeID || s.State != alert.StateFiring {
			continue
		}
		if severityOf[s.RuleID] == alert.SeverityCritical {
			critical++
		} else {
			warning++
		}
	}

	score := max(100.0-float64(critical)*40-float64(warning)*15, 0)
	detail := "no active alerts"
	if critical > 0 || warning > 0 {
		detail = fmt.Sprintf("%d critical, %d warning firing", critical, warning)
	}
	return Signal{Score: score, Available: true, Detail: detail}, nil
}
