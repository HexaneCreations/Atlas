package healthscore

import (
	"context"
	"testing"

	"github.com/hexane/atlas/internal/core/eventstore"
)

type fakeEventReader struct {
	records   []eventstore.Record
	gotFilter eventstore.Filter
}

func (f *fakeEventReader) Query(_ context.Context, filter eventstore.Filter) ([]eventstore.Record, error) {
	f.gotFilter = filter
	return f.records, nil
}

func TestEventProviderScoresNoNotableEventsAsHealthy(t *testing.T) {
	reader := &fakeEventReader{}
	p := EventProvider{Events: reader}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	if !sig.Available || sig.Score != 100 {
		t.Fatalf("got %+v, want available score 100", sig)
	}
	if reader.gotFilter.NodeID != "node-1" {
		t.Fatalf("filter node = %q, want node-1", reader.gotFilter.NodeID)
	}
}

func TestEventProviderPenalizesNotableEventsBySeverity(t *testing.T) {
	reader := &fakeEventReader{records: []eventstore.Record{
		{Topic: "docker.container.oom"},     // danger
		{Topic: "collector.run.failed"},     // warning
		{Topic: "docker.container.started"}, // routine, not notable
	}}
	p := EventProvider{Events: reader}

	sig, err := p.Score(context.Background(), "node-1")
	if err != nil {
		t.Fatalf("score: %v", err)
	}
	want := 100.0 - 10 - 4
	if sig.Score != want {
		t.Fatalf("score = %v, want %v", sig.Score, want)
	}
}
