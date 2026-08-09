package slo

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/alert"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

type fakeSampleSource struct {
	values []float64
	err    error
}

func (f fakeSampleSource) Samples(context.Context, string, string, time.Time, time.Time) ([]float64, error) {
	return f.values, f.err
}

func validDefinition() Definition {
	return Definition{
		Name: "cpu-under-80", NodeID: "node-1", Signal: "saturation",
		Metric: "system.cpu.usage", Comparison: alert.ComparisonLT, Threshold: 80,
		TargetPercentage: 99, Window: time.Hour,
	}
}

func TestValidateRequiresCoreFields(t *testing.T) {
	cases := []struct {
		name string
		def  Definition
	}{
		{"no name", Definition{NodeID: "n", Metric: "m", Comparison: alert.ComparisonLT, TargetPercentage: 99, Window: time.Hour}},
		{"no node", Definition{Name: "x", Metric: "m", Comparison: alert.ComparisonLT, TargetPercentage: 99, Window: time.Hour}},
		{"no metric", Definition{Name: "x", NodeID: "n", Comparison: alert.ComparisonLT, TargetPercentage: 99, Window: time.Hour}},
		{"bad comparison", Definition{Name: "x", NodeID: "n", Metric: "m", TargetPercentage: 99, Window: time.Hour}},
		{"target zero", Definition{Name: "x", NodeID: "n", Metric: "m", Comparison: alert.ComparisonLT, Window: time.Hour}},
		{"target over 100", Definition{Name: "x", NodeID: "n", Metric: "m", Comparison: alert.ComparisonLT, TargetPercentage: 101, Window: time.Hour}},
		{"no window", Definition{Name: "x", NodeID: "n", Metric: "m", Comparison: alert.ComparisonLT, TargetPercentage: 99}},
	}
	for _, c := range cases {
		if err := c.def.Validate(); err == nil {
			t.Errorf("%s: expected a validation error", c.name)
		}
	}
}

func TestValidateAcceptsAWellFormedDefinition(t *testing.T) {
	if err := validDefinition().Validate(); err != nil {
		t.Errorf("unexpected validation error: %v", err)
	}
}

func TestEvaluateRejectsAnInvalidDefinition(t *testing.T) {
	e := NewEngine(Options{Samples: fakeSampleSource{}})
	_, err := e.Evaluate(context.Background(), Definition{})
	if err == nil {
		t.Fatal("expected a validation error")
	}
}

func TestEvaluatePropagatesSampleSourceError(t *testing.T) {
	e := NewEngine(Options{Samples: fakeSampleSource{err: errors.New("boom")}})
	_, err := e.Evaluate(context.Background(), validDefinition())
	if err == nil {
		t.Fatal("expected the sample source error to propagate")
	}
}

func TestEvaluateUnavailableWithNoSamples(t *testing.T) {
	e := NewEngine(Options{Samples: fakeSampleSource{}})
	eval, err := e.Evaluate(context.Background(), validDefinition())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if eval.Available {
		t.Fatal("expected unavailable with no samples in the window — not 0% or 100% compliance")
	}
}

func TestEvaluateComputesComplianceAgainstTheThreshold(t *testing.T) {
	e := NewEngine(Options{Samples: fakeSampleSource{values: []float64{10, 20, 90, 30}}}) // 3 of 4 under 80
	eval, err := e.Evaluate(context.Background(), validDefinition())
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !eval.Available {
		t.Fatal("expected available")
	}
	if eval.SampleCount != 4 {
		t.Errorf("sample count = %d, want 4", eval.SampleCount)
	}
	if eval.Compliance != 75 {
		t.Errorf("compliance = %v, want 75", eval.Compliance)
	}
}

func TestEvaluateErrorBudgetConsumption(t *testing.T) {
	def := validDefinition()
	def.TargetPercentage = 90                                                                                      // error budget = 10
	e := NewEngine(Options{Samples: fakeSampleSource{values: []float64{10, 10, 10, 10, 10, 10, 10, 10, 10, 200}}}) // 90% compliant
	eval, err := e.Evaluate(context.Background(), def)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if eval.Compliance != 90 {
		t.Fatalf("compliance = %v, want 90", eval.Compliance)
	}
	if eval.ErrorBudget != 10 {
		t.Errorf("error budget = %v, want 10", eval.ErrorBudget)
	}
	if !almostEqual(eval.ErrorBudgetConsumedPercent, 100) {
		t.Errorf("consumed = %v, want 100 (all 10 points of budget used)", eval.ErrorBudgetConsumedPercent)
	}
	if eval.Status != StatusBreached {
		t.Errorf("status = %v, want breached", eval.Status)
	}
}

func TestEvaluateStatusHealthyWellWithinBudget(t *testing.T) {
	def := validDefinition()
	def.TargetPercentage = 90
	values := make([]float64, 100)
	for i := range values {
		values[i] = 10 // every sample compliant
	}
	e := NewEngine(Options{Samples: fakeSampleSource{values: values}})
	eval, err := e.Evaluate(context.Background(), def)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if eval.Compliance != 100 {
		t.Errorf("compliance = %v, want 100", eval.Compliance)
	}
	if eval.ErrorBudgetConsumedPercent != 0 {
		t.Errorf("consumed = %v, want 0", eval.ErrorBudgetConsumedPercent)
	}
	if eval.Status != StatusHealthy {
		t.Errorf("status = %v, want healthy", eval.Status)
	}
}

func TestEvaluateStatusWarningAtConfiguredThreshold(t *testing.T) {
	def := validDefinition()
	def.TargetPercentage = 90 // budget = 10
	def.WarningBudgetPercent = 50
	// 95% compliant -> 5 of 10 budget points consumed -> 50% -> warning.
	values := make([]float64, 100)
	for i := range values {
		if i < 95 {
			values[i] = 10
		} else {
			values[i] = 200
		}
	}
	e := NewEngine(Options{Samples: fakeSampleSource{values: values}})
	eval, err := e.Evaluate(context.Background(), def)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !almostEqual(eval.ErrorBudgetConsumedPercent, 50) {
		t.Fatalf("consumed = %v, want 50", eval.ErrorBudgetConsumedPercent)
	}
	if eval.Status != StatusWarning {
		t.Errorf("status = %v, want warning", eval.Status)
	}
}

func TestEvaluateHundredPercentTargetBreachesOnAnyNonCompliance(t *testing.T) {
	def := validDefinition()
	def.TargetPercentage = 100
	e := NewEngine(Options{Samples: fakeSampleSource{values: []float64{10, 10, 200}}})
	eval, err := e.Evaluate(context.Background(), def)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if eval.ErrorBudget != 0 {
		t.Errorf("error budget = %v, want 0", eval.ErrorBudget)
	}
	if eval.Status != StatusBreached {
		t.Errorf("status = %v, want breached", eval.Status)
	}
}
