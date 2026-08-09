package healthscore

import (
	"context"
	"testing"

	"github.com/hexane/atlas/internal/core/incident"
)

type fakeIncidentReader struct {
	incidents []incident.Incident
	gotFilter incident.Filter
}

func (f *fakeIncidentReader) ListIncidents(_ context.Context, filter incident.Filter) ([]incident.Incident, error) {
	f.gotFilter = filter
	return f.incidents, nil
}

func TestIncidentProviderScoresNoOpenIncidentsAsHealthy(t *testing.T) {
	store := &fakeIncidentReader{}
	p := IncidentProvider{Store: store}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !sig.Available || sig.Score != 100 {
		t.Fatalf("got %+v, want available score 100", sig)
	}
	if store.gotFilter.NodeID != "node-1" || store.gotFilter.Status != incident.StatusOpen {
		t.Fatalf("filter = %+v, want scoped to node-1/open", store.gotFilter)
	}
}

func TestIncidentProviderPenalizesOpenIncidentsBySeverity(t *testing.T) {
	store := &fakeIncidentReader{incidents: []incident.Incident{
		{Severity: incident.SeverityCritical},
		{Severity: incident.SeverityWarning},
	}}
	p := IncidentProvider{Store: store}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	want := 100.0 - 50 - 20
	if sig.Score != want {
		t.Fatalf("score = %v, want %v", sig.Score, want)
	}
}
