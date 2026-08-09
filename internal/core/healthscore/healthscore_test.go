package healthscore

import (
	"context"
	"errors"
	"testing"
)

type fakeProvider struct {
	name   string
	signal Signal
	err    error
}

func (f fakeProvider) Name() string { return f.name }

func (f fakeProvider) Score(context.Context, string) (Signal, error) {
	return f.signal, f.err
}

func TestScoreIsWeightedAverageOfAvailableSignals(t *testing.T) {
	e := NewEngine(Options{Providers: []Weighted{
		{Provider: fakeProvider{name: "a", signal: Signal{Score: 100, Available: true}}, Weight: 1},
		{Provider: fakeProvider{name: "b", signal: Signal{Score: 0, Available: true}}, Weight: 1},
	}})

	res := e.Score(context.Background(), "node-1")
	if !res.Available {
		t.Fatal("expected the result to be available")
	}
	if res.Overall != 50 {
		t.Fatalf("overall = %v, want 50", res.Overall)
	}
}

func TestUnavailableSignalIsExcludedFromWeighting(t *testing.T) {
	e := NewEngine(Options{Providers: []Weighted{
		{Provider: fakeProvider{name: "a", signal: Signal{Score: 40, Available: true}}, Weight: 1},
		{Provider: fakeProvider{name: "b", signal: Signal{Available: false}}, Weight: 100},
	}})

	res := e.Score(context.Background(), "node-1")
	if res.Overall != 40 {
		t.Fatalf("overall = %v, want 40 — an unavailable signal must not dilute the average even with a huge weight", res.Overall)
	}
}

func TestProviderErrorIsExcludedNotFatal(t *testing.T) {
	e := NewEngine(Options{Providers: []Weighted{
		{Provider: fakeProvider{name: "a", signal: Signal{Score: 80, Available: true}}, Weight: 1},
		{Provider: fakeProvider{name: "b", err: errors.New("boom")}, Weight: 1},
	}})

	res := e.Score(context.Background(), "node-1")
	if !res.Available || res.Overall != 80 {
		t.Fatalf("got %+v, want available overall 80 (one signal failing must not fail the whole score)", res)
	}
	if len(res.Signals) != 2 {
		t.Fatalf("expected both signals recorded, got %d", len(res.Signals))
	}
	for _, s := range res.Signals {
		if s.Name == "b" && s.Available {
			t.Fatal("an errored provider must be recorded as unavailable")
		}
	}
}

func TestResultUnavailableWhenEverySignalIsUnavailable(t *testing.T) {
	e := NewEngine(Options{Providers: []Weighted{
		{Provider: fakeProvider{name: "a", signal: Signal{Available: false}}, Weight: 1},
	}})

	res := e.Score(context.Background(), "node-1")
	if res.Available {
		t.Fatal("expected the result unavailable when every signal is unavailable")
	}
}

func TestOverallIsClampedToZeroAndHundred(t *testing.T) {
	e := NewEngine(Options{Providers: []Weighted{
		{Provider: fakeProvider{name: "a", signal: Signal{Score: 150, Available: true}}, Weight: 1},
	}})

	res := e.Score(context.Background(), "node-1")
	if res.Overall != 100 {
		t.Fatalf("overall = %v, want clamped to 100 against a misbehaving provider", res.Overall)
	}
}

// A new signal is a new Provider in the list, not a change to how Score
// combines them — this pins that the combination logic stays generic as
// providers are added.
func TestEngineCombinesAnyNumberOfProvidersByWeightedAverage(t *testing.T) {
	e := NewEngine(Options{Providers: []Weighted{
		{Provider: fakeProvider{name: "a", signal: Signal{Score: 100, Available: true}}, Weight: 1},
		{Provider: fakeProvider{name: "b", signal: Signal{Score: 100, Available: true}}, Weight: 1},
		{Provider: fakeProvider{name: "c", signal: Signal{Score: 0, Available: true}}, Weight: 1},
	}})

	res := e.Score(context.Background(), "node-1")
	want := 200.0 / 3
	if res.Overall != want {
		t.Fatalf("overall = %v, want %v", res.Overall, want)
	}
	if len(res.Signals) != 3 {
		t.Fatalf("expected 3 signals, got %d", len(res.Signals))
	}
}

func TestWeightsNeedNotSumToAnyParticularTotal(t *testing.T) {
	e := NewEngine(Options{Providers: []Weighted{
		{Provider: fakeProvider{name: "a", signal: Signal{Score: 60, Available: true}}, Weight: 3},
		{Provider: fakeProvider{name: "b", signal: Signal{Score: 0, Available: true}}, Weight: 1},
	}})

	res := e.Score(context.Background(), "node-1")
	want := 45.0 // (60*3 + 0*1) / 4
	if res.Overall != want {
		t.Fatalf("overall = %v, want %v", res.Overall, want)
	}
}
