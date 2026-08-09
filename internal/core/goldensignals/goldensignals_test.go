package goldensignals

import (
	"context"
	"errors"
	"testing"

	"github.com/hexane/atlas/internal/core/capacityplanning"
)

type fakeSnapshotSource struct {
	snap Snapshot
	err  error
}

func (f fakeSnapshotSource) Snapshot(context.Context, string) (Snapshot, error) { return f.snap, f.err }

type fakeCapacitySource struct {
	summary capacityplanning.Summary
	err     error
}

func (f fakeCapacitySource) Assess(context.Context, string) (capacityplanning.Summary, error) {
	return f.summary, f.err
}

func testEngine(snap fakeSnapshotSource, capSource fakeCapacitySource) *Engine {
	return NewEngine(Options{
		Snapshots: snap, Capacity: capSource,
		Providers: []Provider{LatencyProvider{}, TrafficProvider{}, ErrorsProvider{}, SaturationProvider{}},
	})
}

func TestMeasurePropagatesSnapshotSourceError(t *testing.T) {
	e := testEngine(fakeSnapshotSource{err: errors.New("boom")}, fakeCapacitySource{})
	if _, err := e.Measure(context.Background(), "node-1"); err == nil {
		t.Fatal("expected the snapshot source error to propagate")
	}
}

func TestMeasurePropagatesCapacitySourceError(t *testing.T) {
	e := testEngine(fakeSnapshotSource{}, fakeCapacitySource{err: errors.New("boom")})
	if _, err := e.Measure(context.Background(), "node-1"); err == nil {
		t.Fatal("expected the capacity source error to propagate")
	}
}

func TestMeasureReturnsAllFourSignals(t *testing.T) {
	e := testEngine(fakeSnapshotSource{}, fakeCapacitySource{})
	res, err := e.Measure(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if len(res.Signals) != 4 {
		t.Fatalf("expected 4 signals, got %d", len(res.Signals))
	}
	names := map[SignalName]bool{}
	for _, s := range res.Signals {
		names[s.Name] = true
	}
	for _, want := range []SignalName{SignalLatency, SignalTraffic, SignalErrors, SignalSaturation} {
		if !names[want] {
			t.Errorf("missing signal %s", want)
		}
	}
}

func TestLatencyIsAlwaysUnavailable(t *testing.T) {
	e := testEngine(
		fakeSnapshotSource{snap: Snapshot{HasNetworkTraffic: true, HasNetworkErrors: true}},
		fakeCapacitySource{},
	)
	res, err := e.Measure(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	for _, s := range res.Signals {
		if s.Name == SignalLatency && s.Available {
			t.Fatal("latency must never be available — Atlas collects no latency telemetry")
		}
	}
}

func TestTrafficUnavailableWithoutData(t *testing.T) {
	sig := TrafficProvider{}.Measure(Inputs{})
	if sig.Available {
		t.Fatal("expected unavailable with no network traffic data")
	}
}

func TestTrafficSumsRxAndTx(t *testing.T) {
	sig := TrafficProvider{}.Measure(Inputs{Snapshot: Snapshot{
		HasNetworkTraffic: true, NetworkRxBytesPerSec: 100, NetworkTxBytesPerSec: 50,
	}})
	if !sig.Available || sig.Value != 150 {
		t.Errorf("got %+v, want available value 150", sig)
	}
}

func TestErrorsUnavailableWithoutData(t *testing.T) {
	sig := ErrorsProvider{}.Measure(Inputs{})
	if sig.Available {
		t.Fatal("expected unavailable with no network error data")
	}
}

func TestErrorsReportsTheConfiguredRate(t *testing.T) {
	sig := ErrorsProvider{}.Measure(Inputs{Snapshot: Snapshot{HasNetworkErrors: true, NetworkErrorsPerSec: 3.5}})
	if !sig.Available || sig.Value != 3.5 {
		t.Errorf("got %+v, want available value 3.5", sig)
	}
}

func TestSaturationUnavailableWithoutData(t *testing.T) {
	sig := SaturationProvider{}.Measure(Inputs{})
	if sig.Available {
		t.Fatal("expected unavailable with no capacity data")
	}
}

func TestSaturationIsTheWorstResourceDomain(t *testing.T) {
	sig := SaturationProvider{}.Measure(Inputs{Capacity: capacityplanning.Summary{Domains: []capacityplanning.Domain{
		{Name: capacityplanning.DomainCPU, Available: true, UtilizationPercent: 40},
		{Name: capacityplanning.DomainMemory, Available: true, UtilizationPercent: 85},
		{Name: capacityplanning.DomainDisk, Available: true, UtilizationPercent: 20},
	}}})
	if !sig.Available || sig.Value != 85 {
		t.Errorf("got %+v, want available value 85 (memory, the worst)", sig)
	}
}

func TestSaturationIgnoresNonResourceDomainsAndUnavailableOnes(t *testing.T) {
	sig := SaturationProvider{}.Measure(Inputs{Capacity: capacityplanning.Summary{Domains: []capacityplanning.Domain{
		{Name: capacityplanning.DomainCPU, Available: false, UtilizationPercent: 99},
		{Name: capacityplanning.DomainContainerDensity, Available: true, UtilizationPercent: 99},
		{Name: capacityplanning.DomainMemory, Available: true, UtilizationPercent: 30},
	}}})
	if !sig.Available || sig.Value != 30 {
		t.Errorf("got %+v, want available value 30 (only memory counts)", sig)
	}
}
