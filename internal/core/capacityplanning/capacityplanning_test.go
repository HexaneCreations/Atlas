package capacityplanning

import (
	"context"
	"errors"
	"testing"
)

type fakeSnapshotSource struct {
	snap Snapshot
	err  error
}

func (f fakeSnapshotSource) Snapshot(context.Context, string) (Snapshot, error) { return f.snap, f.err }

type fakeProvider struct {
	name   string
	domain Domain
}

func (f fakeProvider) Name() string           { return f.name }
func (f fakeProvider) Assess(Snapshot) Domain { return f.domain }

func TestAssessPropagatesSnapshotSourceError(t *testing.T) {
	e := NewEngine(Options{Snapshots: fakeSnapshotSource{err: errors.New("boom")}})

	_, err := e.Assess(context.Background(), "node-1")
	if err == nil {
		t.Fatal("expected the snapshot source error to propagate")
	}
}

func TestOverallStatusIsTheWorstAvailableDomain(t *testing.T) {
	e := NewEngine(Options{
		Snapshots: fakeSnapshotSource{snap: Snapshot{NodeID: "node-1"}},
		Providers: []Provider{
			fakeProvider{name: "a", domain: Domain{Available: true, Status: StatusHealthy}},
			fakeProvider{name: "b", domain: Domain{Available: true, Status: StatusCritical}},
			fakeProvider{name: "c", domain: Domain{Available: true, Status: StatusWarning}},
		},
	})

	summary, err := e.Assess(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if summary.Status != StatusCritical {
		t.Errorf("status = %v, want critical", summary.Status)
	}
	if len(summary.Domains) != 3 {
		t.Fatalf("expected 3 domains, got %d", len(summary.Domains))
	}
}

func TestUnavailableDomainsDoNotAffectOverallStatus(t *testing.T) {
	e := NewEngine(Options{
		Snapshots: fakeSnapshotSource{snap: Snapshot{NodeID: "node-1"}},
		Providers: []Provider{
			fakeProvider{name: "a", domain: Domain{Available: false, Status: StatusCritical}},
			fakeProvider{name: "b", domain: Domain{Available: true, Status: StatusHealthy}},
		},
	})

	summary, err := e.Assess(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if summary.Status != StatusHealthy {
		t.Errorf("status = %v, want healthy — an unavailable domain must not drag the overall status down", summary.Status)
	}
}

func TestOverallStatusHealthyWhenEveryDomainIsUnavailable(t *testing.T) {
	e := NewEngine(Options{
		Snapshots: fakeSnapshotSource{snap: Snapshot{NodeID: "node-1"}},
		Providers: []Provider{
			fakeProvider{name: "a", domain: Domain{Available: false}},
		},
	})

	summary, err := e.Assess(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if summary.Status != StatusHealthy {
		t.Errorf("status = %v, want healthy", summary.Status)
	}
}

// A new capacity domain — or a new forecasting algorithm for an existing
// one — is a new Provider, not a change to how Engine combines them.
func TestEngineCombinesAnyNumberOfProvidersWithoutChange(t *testing.T) {
	e := NewEngine(Options{
		Snapshots: fakeSnapshotSource{snap: Snapshot{NodeID: "node-1"}},
		Providers: []Provider{
			fakeProvider{name: "a", domain: Domain{Available: true, Status: StatusHealthy}},
			fakeProvider{name: "b", domain: Domain{Available: true, Status: StatusHealthy}},
			fakeProvider{name: "c", domain: Domain{Available: true, Status: StatusHealthy}},
			fakeProvider{name: "d", domain: Domain{Available: true, Status: StatusHealthy}},
		},
	})

	summary, err := e.Assess(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("assess: %v", err)
	}
	if len(summary.Domains) != 4 {
		t.Fatalf("expected 4 domains, got %d", len(summary.Domains))
	}
	for _, d := range summary.Domains {
		if d.Name == "" {
			t.Error("domain name was not stamped by the engine")
		}
	}
}

func TestStatusForThresholds(t *testing.T) {
	cases := []struct {
		value float64
		want  Status
	}{
		{0, StatusHealthy},
		{74.9, StatusHealthy},
		{75, StatusWarning},
		{89.9, StatusWarning},
		{90, StatusCritical},
		{150, StatusCritical},
	}
	for _, c := range cases {
		if got := statusFor(c.value, 75, 90); got != c.want {
			t.Errorf("statusFor(%v) = %v, want %v", c.value, got, c.want)
		}
	}
}
