package healthscore

import (
	"context"
	"testing"

	"github.com/hexane/atlas/internal/core/alert"
)

type fakeAlertStore struct {
	rules  []alert.Rule
	states []alert.AlertState
}

func (f fakeAlertStore) ListRules(context.Context) ([]alert.Rule, error) { return f.rules, nil }

func (f fakeAlertStore) ListActiveStates(context.Context) ([]alert.AlertState, error) {
	return f.states, nil
}

func TestAlertProviderScoresNoFiringAlertsAsHealthy(t *testing.T) {
	store := fakeAlertStore{}
	p := AlertProvider{Rules: store, States: store}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !sig.Available || sig.Score != 100 {
		t.Fatalf("got %+v, want available score 100", sig)
	}
}

func TestAlertProviderPenalizesFiringAlertsBySeverity(t *testing.T) {
	store := fakeAlertStore{
		rules: []alert.Rule{
			{ID: "rule-crit", Severity: alert.SeverityCritical},
			{ID: "rule-warn", Severity: alert.SeverityWarning},
		},
		states: []alert.AlertState{
			{RuleID: "rule-crit", NodeID: "node-1", State: alert.StateFiring},
			{RuleID: "rule-warn", NodeID: "node-1", State: alert.StateFiring},
			// Different node: must not affect node-1's score.
			{RuleID: "rule-crit", NodeID: "node-2", State: alert.StateFiring},
		},
	}
	p := AlertProvider{Rules: store, States: store}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	want := 100.0 - 40 - 15
	if sig.Score != want {
		t.Fatalf("score = %v, want %v", sig.Score, want)
	}
}

func TestAlertProviderIgnoresNonFiringStates(t *testing.T) {
	store := fakeAlertStore{
		rules:  []alert.Rule{{ID: "rule-1", Severity: alert.SeverityCritical}},
		states: []alert.AlertState{{RuleID: "rule-1", NodeID: "node-1", State: alert.StateOK}},
	}
	p := AlertProvider{Rules: store, States: store}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Score != 100 {
		t.Fatalf("score = %v, want 100 — a resolved alert must not penalise", sig.Score)
	}
}
