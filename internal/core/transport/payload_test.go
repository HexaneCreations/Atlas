package transport_test

import (
	"testing"
	"time"

	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

func TestEventsPayloadRoundTripsThroughJSON(t *testing.T) {
	env := transport.Envelope{
		ID:     "env-1",
		Origin: testOrigin(),
		Payload: transport.Events{Event: eventbus.Event{
			ID: "evt-1", Topic: "docker.container.started", Source: "plugin.docker",
			Subject: "abc123", Time: time.Now().UTC().Truncate(time.Second),
			Payload: map[string]any{"name": "nginx"},
		}},
		SentAt: time.Now().UTC().Truncate(time.Second),
	}

	raw, err := env.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got transport.Envelope
	if err := got.UnmarshalJSON(raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	event, ok := transport.EventsOf(got)
	if !ok {
		t.Fatal("round-tripped envelope does not carry an events payload")
	}
	if event.ID != "evt-1" || event.Topic != "docker.container.started" || event.Subject != "abc123" {
		t.Errorf("event did not round-trip: %+v", event)
	}
}

func TestEventsPayloadRejectsInvalidTopic(t *testing.T) {
	e := transport.Events{Event: eventbus.Event{Topic: ""}}
	if err := e.Validate(); err == nil {
		t.Fatal("expected an error for an empty topic")
	}
}

func TestEventsOfReportsFalseForOtherKinds(t *testing.T) {
	env := transport.Envelope{Origin: testOrigin(), Payload: transport.Metrics{Batch: testBatch()}}
	if _, ok := transport.EventsOf(env); ok {
		t.Fatal("expected EventsOf to report false for a metrics envelope")
	}
}
