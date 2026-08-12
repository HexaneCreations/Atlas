package agent

import (
	"context"
	"errors"
	"sync"

	"github.com/hexane/atlas/internal/core/transport"
)

// fanoutTransport delivers every envelope to every relationship's own
// transport, in parallel and independently. One relationship's failure
// (local spool error, closed transport, or — inside remote.Transport itself
// — a control plane outage) must never block or fail delivery to another:
// this is the "failure of one relationship must not affect another"
// requirement applied to the data path, not just to trust.
//
// It implements [transport.Transport], so the scheduler, inventoryPusher,
// and eventForwarder need no change at all — they already only ever depend
// on that interface, never on remote.Transport directly (see
// docs/adr/0005-transport-seam.md).
type fanoutTransport struct {
	targets []transport.Transport
}

func newFanoutTransport(targets []transport.Transport) *fanoutTransport {
	return &fanoutTransport{targets: targets}
}

// Send implements [transport.Transport]. Every target is sent to in
// parallel; an error is returned only when every target failed — for as long
// as at least one relationship accepted the envelope, callers must not treat
// this as a delivery failure.
func (f *fanoutTransport) Send(ctx context.Context, env transport.Envelope) error {
	switch len(f.targets) {
	case 0:
		return nil
	case 1:
		return f.targets[0].Send(ctx, env)
	}

	sendErrs := make([]error, len(f.targets))
	var wg sync.WaitGroup
	wg.Add(len(f.targets))
	for i, t := range f.targets {
		go func(i int, t transport.Transport) {
			defer wg.Done()
			sendErrs[i] = t.Send(ctx, env)
		}(i, t)
	}
	wg.Wait()

	failed := 0
	for _, err := range sendErrs {
		if err != nil {
			failed++
		}
	}
	if failed == len(f.targets) {
		return errors.Join(sendErrs...)
	}
	return nil
}

// Close implements [transport.Transport]. Every target is closed regardless
// of any individual failure; every error is aggregated and reported.
func (f *fanoutTransport) Close() error {
	closeErrs := make([]error, len(f.targets))
	var wg sync.WaitGroup
	wg.Add(len(f.targets))
	for i, t := range f.targets {
		go func(i int, t transport.Transport) {
			defer wg.Done()
			closeErrs[i] = t.Close()
		}(i, t)
	}
	wg.Wait()
	return errors.Join(closeErrs...)
}

var _ transport.Transport = (*fanoutTransport)(nil)
