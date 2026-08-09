package costanalysis

import (
	"context"
	"errors"
	"testing"
)

type fakeUsageSource struct {
	usage Usage
	err   error
}

func (f fakeUsageSource) Usage(context.Context, string) (Usage, error) { return f.usage, f.err }

// fakePricing lets a test dial in each category's cost directly, independent
// of any real pricing formula — the engine's job is to combine them, not to
// price anything itself.
type fakePricing struct {
	name                                  string
	cpu, memory, disk, network, container float64
}

func (p fakePricing) Name() string                { return p.name }
func (p fakePricing) CPU() CPUPricing             { return constPricing(p.cpu) }
func (p fakePricing) Memory() MemoryPricing       { return constPricing(p.memory) }
func (p fakePricing) Disk() DiskPricing           { return constPricing(p.disk) }
func (p fakePricing) Network() NetworkPricing     { return constPricing(p.network) }
func (p fakePricing) Container() ContainerPricing { return constPricing(p.container) }

type constPricing float64

func (c constPricing) Cost(Usage) float64 { return float64(c) }

func TestEstimateSumsEveryCategoryIntoTheHourlyTotal(t *testing.T) {
	e := NewEngine(Options{
		Usage:   fakeUsageSource{usage: Usage{NodeID: "node-1"}},
		Pricing: fakePricing{name: "fake", cpu: 1, memory: 2, disk: 3, network: 4, container: 5},
	})

	res, err := e.Estimate(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if res.NodeID != "node-1" {
		t.Errorf("node id = %q, want node-1", res.NodeID)
	}
	if res.PricingModel != "fake" {
		t.Errorf("pricing model = %q, want fake", res.PricingModel)
	}
	want := 1.0 + 2 + 3 + 4 + 5
	if res.HourlyTotal != want {
		t.Errorf("hourly total = %v, want %v", res.HourlyTotal, want)
	}
	if res.Breakdown.Total() != res.HourlyTotal {
		t.Errorf("breakdown total = %v, hourly total = %v, want equal", res.Breakdown.Total(), res.HourlyTotal)
	}
}

func TestEstimateSinceBootExtrapolatesHourlyTotalAcrossUptime(t *testing.T) {
	e := NewEngine(Options{
		Usage:   fakeUsageSource{usage: Usage{NodeID: "node-1", UptimeSeconds: 2 * 3600}},
		Pricing: fakePricing{name: "fake", cpu: 10},
	})

	res, err := e.Estimate(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if res.EstimatedSinceBoot != 20 {
		t.Errorf("estimated since boot = %v, want 20 (10/hr * 2h)", res.EstimatedSinceBoot)
	}
}

func TestEstimateZeroUptimeYieldsZeroSinceBoot(t *testing.T) {
	e := NewEngine(Options{
		Usage:   fakeUsageSource{usage: Usage{NodeID: "node-1"}},
		Pricing: fakePricing{name: "fake", cpu: 10},
	})

	res, err := e.Estimate(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("estimate: %v", err)
	}
	if res.EstimatedSinceBoot != 0 {
		t.Errorf("estimated since boot = %v, want 0", res.EstimatedSinceBoot)
	}
}

func TestEstimatePropagatesUsageSourceError(t *testing.T) {
	e := NewEngine(Options{
		Usage:   fakeUsageSource{err: errors.New("boom")},
		Pricing: fakePricing{name: "fake"},
	})

	_, err := e.Estimate(context.Background(), "node-1")
	if err == nil {
		t.Fatal("expected the usage source error to propagate")
	}
}

// A new PricingModel is a new implementation of the five interfaces, not a
// change to the engine — this pins that swapping models changes only what
// usage costs.
func TestEnginePricingIsSwappableWithoutEngineChange(t *testing.T) {
	usage := fakeUsageSource{usage: Usage{NodeID: "node-1"}}

	cheap := NewEngine(Options{Usage: usage, Pricing: fakePricing{name: "cheap", cpu: 1}})
	expensive := NewEngine(Options{Usage: usage, Pricing: fakePricing{name: "expensive", cpu: 100}})

	cheapRes, err := cheap.Estimate(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("estimate (cheap): %v", err)
	}
	expensiveRes, err := expensive.Estimate(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("estimate (expensive): %v", err)
	}
	if cheapRes.HourlyTotal >= expensiveRes.HourlyTotal {
		t.Fatalf("cheap (%v) should cost less than expensive (%v)", cheapRes.HourlyTotal, expensiveRes.HourlyTotal)
	}
}
