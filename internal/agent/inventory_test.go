package agent

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	coreinventory "github.com/hexane/atlas/internal/core/inventory"
	"github.com/hexane/atlas/internal/core/transport"
)

type flakyTransport struct {
	recordingTransport
	failing bool
}

func (f *flakyTransport) Send(ctx context.Context, env transport.Envelope) error {
	if f.failing {
		return errors.New("control plane unreachable")
	}
	return f.recordingTransport.Send(ctx, env)
}

func staticSource(subject coreinventory.Subject, data *string) inventorySource {
	return inventorySource{subject, func(context.Context) (any, error) {
		return map[string]string{"state": *data}, nil
	}}
}

func testPusher(t *testing.T, targets []fanoutTarget, sources []inventorySource) *inventoryPusher {
	t.Helper()
	return newInventoryPusher(newFanoutTransport(targets), transport.Origin{NodeID: "node-1"},
		time.Minute, slog.New(slog.DiscardHandler), sources)
}

func TestInventoryPusherSkipsUnchangedSubjectPerRelationship(t *testing.T) {
	t.Parallel()

	a, b := &flakyTransport{}, &flakyTransport{}
	state := "one"
	p := testPusher(t, []fanoutTarget{{id: "a", tr: a}, {id: "b", tr: b}},
		[]inventorySource{staticSource(coreinventory.SubjectProcesses, &state)})

	p.pushAll(context.Background())
	p.pushAll(context.Background())

	if len(a.envs) != 1 || len(b.envs) != 1 {
		t.Errorf("sent %d and %d envelopes, want 1 each — unchanged content must not be re-pushed", len(a.envs), len(b.envs))
	}

	state = "two"
	p.pushAll(context.Background())
	if len(a.envs) != 2 || len(b.envs) != 2 {
		t.Errorf("sent %d and %d envelopes after a content change, want 2 each", len(a.envs), len(b.envs))
	}
}

// The isolation requirement: inventory is dropped rather than spooled on
// failure, so a relationship that rejected a subject must be retried on the
// next tick even though another relationship accepted the same content.
func TestInventoryPusherRetriesOnlyTheRelationshipThatFailed(t *testing.T) {
	t.Parallel()

	healthy := &flakyTransport{}
	broken := &flakyTransport{failing: true}
	state := "one"
	p := testPusher(t, []fanoutTarget{{id: "healthy", tr: healthy}, {id: "broken", tr: broken}},
		[]inventorySource{staticSource(coreinventory.SubjectProcesses, &state)})

	p.pushAll(context.Background())
	if len(healthy.envs) != 1 {
		t.Fatalf("healthy relationship got %d envelopes, want 1", len(healthy.envs))
	}

	broken.failing = false
	p.pushAll(context.Background())

	if len(broken.envs) != 1 {
		t.Errorf("recovered relationship got %d envelopes, want 1 — a failed push must be retried without waiting for the data to change", len(broken.envs))
	}
	if len(healthy.envs) != 1 {
		t.Errorf("healthy relationship got %d envelopes, want 1 — content it already accepted must not be re-sent", len(healthy.envs))
	}
}

func TestInventoryPusherStampsPerRelationshipEnvironment(t *testing.T) {
	t.Parallel()

	dev, prod := &flakyTransport{}, &flakyTransport{}
	state := "one"
	p := testPusher(t, []fanoutTarget{
		{id: "development", environment: "development", tr: dev},
		{id: "production", environment: "production", tr: prod},
	}, []inventorySource{staticSource(coreinventory.SubjectProcesses, &state)})

	p.pushAll(context.Background())

	if got := dev.last().Origin.Environment; got != "development" {
		t.Errorf("development relationship saw environment %q, want development", got)
	}
	if got := prod.last().Origin.Environment; got != "production" {
		t.Errorf("production relationship saw environment %q, want production", got)
	}
}
