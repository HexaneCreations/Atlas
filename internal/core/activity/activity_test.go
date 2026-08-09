package activity

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/hexane/atlas/internal/platform/eventbus"
)

// The two properties that matter: the buffer is bounded whatever the event
// rate, and a payload that can name itself gets to. Everything else about
// this package is a lookup table.

func newStarted(t *testing.T, capacity int) (*Recorder, *eventbus.Bus) {
	t.Helper()

	bus := eventbus.New(eventbus.Options{BufferSize: 256})
	t.Cleanup(func() { _ = bus.Close() })

	r := New(Options{Bus: bus, Capacity: capacity})
	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = r.Stop(context.Background()) })
	return r, bus
}

// waitFor polls until cond holds, so a test never depends on how quickly the
// bus's delivery goroutine gets scheduled.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within the deadline")
}

func TestRecordsOnlyNotableTopics(t *testing.T) {
	r, bus := newStarted(t, DefaultCapacity)

	bus.Publish(context.Background(), eventbus.Event{Topic: "docker.container.started", Subject: "web"})
	// Routine churn: a CI host emits thousands of these an hour, and burying
	// the entries that matter under them is the whole reason for the filter.
	bus.Publish(context.Background(), eventbus.Event{Topic: "docker.image.changed", Subject: "nginx"})
	bus.Publish(context.Background(), eventbus.Event{Topic: "docker.volume.changed", Subject: "data"})

	waitFor(t, func() bool { return len(r.Recent(0)) == 1 })

	got := r.Recent(0)
	if got[0].Topic != "docker.container.started" {
		t.Errorf("recorded %q, want only the notable topic", got[0].Topic)
	}
}

func TestBufferStaysBoundedUnderFlood(t *testing.T) {
	const capacity = 10
	r, bus := newStarted(t, capacity)

	// Ten times the capacity. A ring that grew with the event rate would be a
	// memory leak driven by exactly the conditions — a host in trouble,
	// churning containers — where Atlas must stay cheap.
	for i := range capacity * 10 {
		bus.Publish(context.Background(), eventbus.Event{
			Topic: "docker.container.died", Subject: "c" + strconv.Itoa(i),
		})
	}

	waitFor(t, func() bool {
		entries := r.Recent(0)
		return len(entries) == capacity && entries[0].Detail == "c99"
	})

	if got := len(r.Recent(0)); got != capacity {
		t.Fatalf("buffer holds %d entries, want the capacity of %d", got, capacity)
	}
}

func TestRecentReturnsNewestFirst(t *testing.T) {
	r, bus := newStarted(t, DefaultCapacity)

	for _, name := range []string{"first", "second", "third"} {
		bus.Publish(context.Background(), eventbus.Event{Topic: "docker.container.started", Subject: name})
	}
	waitFor(t, func() bool { return len(r.Recent(0)) == 3 })

	got := r.Recent(0)
	// Newest first is what a feed reads as: an operator scanning the top of
	// the list is looking for what just happened.
	want := []string{"third", "second", "first"}
	for i, w := range want {
		if got[i].Detail != w {
			t.Errorf("position %d = %q, want %q", i, got[i].Detail, w)
		}
	}

	if limited := r.Recent(2); len(limited) != 2 || limited[0].Detail != "third" {
		t.Errorf("Recent(2) = %+v, want the two newest", limited)
	}
}

func TestSeverityIsAssignedPerTopic(t *testing.T) {
	r, bus := newStarted(t, DefaultCapacity)

	cases := map[eventbus.Topic]Severity{
		"docker.container.started": SeveritySuccess,
		"docker.container.died":    SeverityWarning,
		"docker.container.oom":     SeverityDanger,
		"collector.run.recovered":  SeveritySuccess,
		"collector.run.panicked":   SeverityDanger,
		"system.host.renamed":      SeverityInfo,
	}
	for topic := range cases {
		bus.Publish(context.Background(), eventbus.Event{Topic: topic, Subject: "x"})
	}
	waitFor(t, func() bool { return len(r.Recent(0)) == len(cases) })

	for _, e := range r.Recent(0) {
		if want := cases[e.Topic]; e.Severity != want {
			t.Errorf("%s severity = %q, want %q", e.Topic, e.Severity, want)
		}
	}
}

// describingPayload stands in for a plugin's event type. The real one lives in
// internal/plugin/docker, which this package must never import — that is the
// entire reason [Describer] exists.
type describingPayload struct{ title, detail string }

func (d describingPayload) ActivityDescription() (string, string) { return d.title, d.detail }

func TestDescriberPayloadSuppliesTitleAndDetail(t *testing.T) {
	r, bus := newStarted(t, DefaultCapacity)

	bus.Publish(context.Background(), eventbus.Event{
		Topic:   "docker.container.died",
		Subject: "9f2c1a4b7e88d3f0aa61", // the raw id, useless to a human
		Payload: describingPayload{title: "Container exited", detail: "nginx-proxy · exit 137"},
	})
	waitFor(t, func() bool { return len(r.Recent(0)) == 1 })

	got := r.Recent(0)[0]
	if got.Title != "Container exited" || got.Detail != "nginx-proxy · exit 137" {
		t.Errorf("got %q / %q, want the payload's own description", got.Title, got.Detail)
	}
}

func TestFallsBackToTopicWhenPayloadCannotDescribeItself(t *testing.T) {
	r, bus := newStarted(t, DefaultCapacity)

	// A payload with no Describer must still produce a readable entry rather
	// than being dropped: less detail beats a missing event.
	bus.Publish(context.Background(), eventbus.Event{
		Topic: "collector.run.failed", Subject: "system.cpu", Payload: struct{ N int }{1},
	})
	waitFor(t, func() bool { return len(r.Recent(0)) == 1 })

	got := r.Recent(0)[0]
	if got.Title != "Collector failed" {
		t.Errorf("title = %q, want the topic's phrase", got.Title)
	}
	if got.Detail != "system.cpu" {
		t.Errorf("detail = %q, want the subject", got.Detail)
	}
}

func TestStopIsIdempotentAndSurvivesNoStart(t *testing.T) {
	bus := eventbus.New(eventbus.Options{BufferSize: 8})
	t.Cleanup(func() { _ = bus.Close() })

	r := New(Options{Bus: bus})
	// Stopping something never started is what happens when an earlier
	// component fails and the supervisor unwinds.
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop before Start: %v", err)
	}

	if err := r.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStartRequiresABus(t *testing.T) {
	if err := New(Options{}).Start(context.Background()); err == nil {
		t.Error("Start with no bus succeeded, want an error")
	}
}
