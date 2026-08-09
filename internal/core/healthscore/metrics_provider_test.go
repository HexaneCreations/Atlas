package healthscore

import (
	"context"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/alert"
)

type fakeMetricRules struct{ rules []alert.Rule }

func (f fakeMetricRules) ListRules(context.Context) ([]alert.Rule, error) { return f.rules, nil }

type fakeMetricSource struct {
	samples map[string][]alert.FleetSample
}

func (f fakeMetricSource) LatestForMetric(_ context.Context, metric string, _ time.Duration) ([]alert.FleetSample, error) {
	return f.samples[metric], nil
}

func TestMetricsProviderUnavailableWithNothingToEvaluate(t *testing.T) {
	p := MetricsProvider{Rules: fakeMetricRules{}, Metrics: fakeMetricSource{}}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Available {
		t.Fatal("expected unavailable when there are no threshold rules to evaluate")
	}
}

func TestMetricsProviderPenalizesViolatedThresholds(t *testing.T) {
	rules := fakeMetricRules{rules: []alert.Rule{
		{Enabled: true, Kind: alert.KindThreshold, Metric: "cpu", Comparison: alert.ComparisonGT, Threshold: 90, Severity: alert.SeverityCritical},
		{Enabled: true, Kind: alert.KindThreshold, Metric: "mem", Comparison: alert.ComparisonGT, Threshold: 90, Severity: alert.SeverityWarning},
	}}
	metrics := fakeMetricSource{samples: map[string][]alert.FleetSample{
		"cpu": {{NodeID: "node-1", Value: 95}},
		"mem": {{NodeID: "node-1", Value: 50}}, // under threshold: not a violation
	}}
	p := MetricsProvider{Rules: rules, Metrics: metrics}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	want := 100.0 - 40 // one critical violation
	if sig.Score != want {
		t.Fatalf("score = %v, want %v", sig.Score, want)
	}
}

func TestMetricsProviderIgnoresDisabledAndOtherNodeRules(t *testing.T) {
	rules := fakeMetricRules{rules: []alert.Rule{
		{Enabled: false, Kind: alert.KindThreshold, Metric: "cpu", Comparison: alert.ComparisonGT, Threshold: 10, Severity: alert.SeverityCritical},
		{Enabled: true, Kind: alert.KindThreshold, Metric: "disk", Comparison: alert.ComparisonGT, Threshold: 10, Severity: alert.SeverityCritical, NodeID: "node-2"},
	}}
	metrics := fakeMetricSource{samples: map[string][]alert.FleetSample{
		"cpu":  {{NodeID: "node-1", Value: 99}},
		"disk": {{NodeID: "node-2", Value: 99}},
	}}
	p := MetricsProvider{Rules: rules, Metrics: metrics}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if sig.Available {
		t.Fatalf("expected unavailable: a disabled rule and a rule scoped to another node give nothing to check, got %+v", sig)
	}
}
