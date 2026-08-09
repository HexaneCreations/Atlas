package eventstore_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/eventstore"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

type fakeStore struct {
	mu      sync.Mutex
	records []eventstore.Record
}

func (f *fakeStore) Insert(_ context.Context, rec eventstore.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, rec)
	return nil
}

func (f *fakeStore) all() []eventstore.Record {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]eventstore.Record(nil), f.records...)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestWriterPersistsBusEvents(t *testing.T) {
	bus := eventbus.New(eventbus.Options{})
	defer bus.Close()
	store := &fakeStore{}

	w := eventstore.New(eventstore.Options{Bus: bus, Store: store, NodeID: "node-1"})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer w.Stop(context.Background())

	bus.Publish(context.Background(), eventbus.Event{
		Topic: "docker.container.started", Source: "plugin.docker", Subject: "abc123",
	})

	waitFor(t, func() bool { return len(store.all()) == 1 })

	rec := store.all()[0]
	if rec.NodeID != "node-1" || rec.Topic != "docker.container.started" || rec.Subject != "abc123" {
		t.Errorf("unexpected record: %+v", rec)
	}
	if rec.ID == "" {
		t.Error("record has no id")
	}
}

func TestWriterStartRequiresBusAndStore(t *testing.T) {
	w := eventstore.New(eventstore.Options{})
	if err := w.Start(context.Background()); err == nil {
		t.Fatal("expected an error with no bus or store configured")
	}
}

func TestWriterStopIsIdempotent(t *testing.T) {
	bus := eventbus.New(eventbus.Options{})
	defer bus.Close()
	w := eventstore.New(eventstore.Options{Bus: bus, Store: &fakeStore{}})
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := w.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}
