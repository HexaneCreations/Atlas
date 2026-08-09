package agent

import (
	"context"
	"log/slog"

	"github.com/hexane/atlas/internal/core/transport"
	"github.com/hexane/atlas/internal/platform/eventbus"
)

// eventForwarder relays this agent's local bus events to the control plane,
// so they survive an agent restart and reach fleet-wide alerting and
// correlation instead of only existing in this process's memory.
type eventForwarder struct {
	bus    *eventbus.Bus
	tr     transport.Transport
	origin transport.Origin
	logger *slog.Logger
}

func newEventForwarder(bus *eventbus.Bus, tr transport.Transport, origin transport.Origin, logger *slog.Logger) *eventForwarder {
	return &eventForwarder{bus: bus, tr: tr, origin: origin, logger: logger}
}

func (f *eventForwarder) run(ctx context.Context) {
	sub, err := f.bus.Subscribe("agent.forwarder", eventbus.MatchAll)
	if err != nil {
		f.logger.ErrorContext(ctx, "could not subscribe to the local event bus", slog.String("error", err.Error()))
		return
	}
	defer sub.Close()

	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-sub.C:
			if !open {
				return
			}
			env := transport.NewEnvelopeOf(f.origin, transport.Events{Event: event})
			if err := f.tr.Send(ctx, env); err != nil {
				f.logger.WarnContext(ctx, "event push failed",
					slog.String("topic", string(event.Topic)), slog.String("error", err.Error()))
			}
		}
	}
}
