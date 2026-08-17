package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/hexane/atlas/internal/core/transport"
)

// fanoutTarget is one relationship's delivery endpoint. environment is
// stamped onto every envelope sent through it, so the same host can report
// itself as "development" to one control plane and "production" to another.
type fanoutTarget struct {
	id          string
	environment string
	tr          transport.Transport
}

// fanoutTransport delivers every envelope to every relationship's own
// transport, in parallel and independently. One relationship's failure
// (local spool error, closed transport, or — inside remote.Transport itself
// — a control plane outage) must never block or fail delivery to another:
// this is the "failure of one relationship must not affect another"
// requirement applied to the data path, not just to trust.
//
// It implements [transport.Transport], so the scheduler and eventForwarder
// need no change at all — they already only ever depend on that interface,
// never on remote.Transport directly (see docs/adr/0005-transport-seam.md).
// Callers that must know which relationships individually accepted an
// envelope — the inventoryPusher, whose payloads are dropped rather than
// spooled on failure — use SendTo instead.
type fanoutTransport struct {
	targets []fanoutTarget
}

func newFanoutTransport(targets []fanoutTarget) *fanoutTransport {
	return &fanoutTransport{targets: targets}
}

// targetIDs returns every relationship id this transport delivers to.
func (f *fanoutTransport) targetIDs() []string {
	ids := make([]string, 0, len(f.targets))
	for _, t := range f.targets {
		ids = append(ids, t.id)
	}
	return ids
}

// Send implements [transport.Transport]. Every target is sent to in
// parallel; an error is returned only when every target failed — for as long
// as at least one relationship accepted the envelope, callers must not treat
// this as a delivery failure.
func (f *fanoutTransport) Send(ctx context.Context, env transport.Envelope) error {
	if len(f.targets) == 0 {
		return nil
	}

	results := f.sendTo(ctx, f.targets, env)
	failed := 0
	errsOut := make([]error, 0, len(results))
	for _, err := range results {
		if err != nil {
			failed++
			errsOut = append(errsOut, err)
		}
	}
	if failed == len(f.targets) {
		return errors.Join(errsOut...)
	}
	return nil
}

// SendTo delivers env to the named relationships only, reporting each one's
// outcome separately. A caller whose payload class is dropped rather than
// spooled on failure needs this: an aggregate "someone accepted it" result
// would let it record a delivery that one control plane never received.
func (f *fanoutTransport) SendTo(ctx context.Context, ids []string, env transport.Envelope) map[string]error {
	wanted := make(map[string]bool, len(ids))
	for _, id := range ids {
		wanted[id] = true
	}

	selected := make([]fanoutTarget, 0, len(ids))
	for _, t := range f.targets {
		if wanted[t.id] {
			selected = append(selected, t)
		}
	}
	return f.sendTo(ctx, selected, env)
}

func (f *fanoutTransport) sendTo(ctx context.Context, targets []fanoutTarget, env transport.Envelope) map[string]error {
	results := make(map[string]error, len(targets))
	if len(targets) == 0 {
		return results
	}
	if len(targets) == 1 {
		results[targets[0].id] = targets[0].send(ctx, env)
		return results
	}

	sendErrs := make([]error, len(targets))
	var wg sync.WaitGroup
	wg.Add(len(targets))
	for i, t := range targets {
		go func(i int, t fanoutTarget) {
			defer wg.Done()
			sendErrs[i] = t.send(ctx, env)
		}(i, t)
	}
	wg.Wait()

	for i, t := range targets {
		results[t.id] = sendErrs[i]
	}
	return results
}

// send stamps this relationship's environment onto a copy of the envelope.
// The copy matters: targets run concurrently and must not race on one shared
// Origin.
func (t fanoutTarget) send(ctx context.Context, env transport.Envelope) error {
	scoped := env
	scoped.Origin.Environment = t.environment
	return t.tr.Send(ctx, scoped)
}

// Close implements [transport.Transport]. Every target is closed regardless
// of any individual failure; every error is aggregated and reported.
func (f *fanoutTransport) Close() error {
	closeErrs := make([]error, len(f.targets))
	var wg sync.WaitGroup
	wg.Add(len(f.targets))
	for i, t := range f.targets {
		go func(i int, t fanoutTarget) {
			defer wg.Done()
			closeErrs[i] = t.tr.Close()
		}(i, t)
	}
	wg.Wait()
	return errors.Join(closeErrs...)
}

var _ transport.Transport = (*fanoutTransport)(nil)
