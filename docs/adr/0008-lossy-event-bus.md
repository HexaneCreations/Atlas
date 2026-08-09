# ADR-0008: A lossy, non-blocking event bus

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

The requirements call for event-driven updates in preference to polling, with
Docker events, Linux events, WebSockets, an event bus, and background event
processing feeding near-real-time updates across the dashboard.

An event bus must decide what happens when a subscriber cannot keep up. There
are only three possible answers: block the publisher, grow memory without
bound, or drop events. Every bus picks one, and the choice determines the
system's behaviour under exactly the conditions where behaviour matters most.

The scenario to reason about is concrete. A browser tab holding a WebSocket
subscription is on a laptop that goes to sleep. Events accumulate for a
consumer that is not reading.

## Decision

**Each subscriber gets a bounded queue. When it is full, the event is dropped
for that subscriber alone, a counter is incremented, and a rate-limited warning
is logged. Publishers never block.**

Buffer depth is `event_bus.buffer_size`, default 256. `Bus.Stats()` and
`Subscription.Dropped()` expose the counters, and they are served on
`/api/v1/system/runtime`.

## Alternatives considered

**Block the publisher until the subscriber catches up.** No events are ever
lost, which sounds strictly better. Rejected because it makes the sleeping
laptop above into a production incident: the stalled WebSocket back-pressures
into the event bus, which back-pressures into the collector scheduler, which
delays metric collection, which makes Atlas report a healthy host as
unresponsive. The monitoring system becomes the outage. **A monitoring
system's failure mode must never be to slow down the thing it monitors** —
that principle decides this, and it is the same reasoning behind the
scheduler's timeouts and concurrency ceiling.

**Unbounded queues.** No blocking and no loss, until memory runs out. Rejected
because it converts a slow consumer into an OOM kill of the whole process,
which loses every subscriber's events rather than one's, and takes down
monitoring entirely.

**Drop the oldest event rather than the newest.** Arguably better semantics —
recent state is usually more valuable than stale state. Rejected on
implementation grounds: dropping the oldest from a Go channel requires either a
mutex-guarded ring buffer, which reintroduces contention on the publish path,
or a non-atomic receive-then-send, which is racy. The complexity is not
justified when the correct response to drops is to fix the slow consumer.

**A durable queue (NATS, Redis Streams, Kafka).** Real delivery guarantees, and
the right answer if events were the durable record. Rejected because they are
not: the database is the source of truth, and the bus carries notifications
about it. Adding a message broker to the deployment for in-process
notifications would be a large operational cost for a guarantee Atlas does not
need here.

## Consequences

**Good.**

- A publisher's cost is bounded and predictable regardless of subscriber
  behaviour. Collection latency cannot be affected by a UI client.
- A slow subscriber degrades alone. This is tested: one wedged subscriber does
  not affect a healthy one.
- Memory per subscriber is bounded and known: `buffer_size` events.
- Drops are counted and visible, so back-pressure is an observable condition
  with a number attached rather than a mystery.

**Costs.**

- **Events can be lost.** This is the deliberate trade and it must be
  understood by everyone building on the bus.
  *Mitigation, and the rule for consumers that cannot tolerate loss:* do not
  rely on the bus alone. Persist from a durable subscriber and reconcile on
  restart — the bus says something changed; the database remains the truth.
  The incident timeline and alert engine are built this way.
- A subscriber that is merely bursty, not broken, may drop events during a
  spike. Tuned with `event_bus.buffer_size`.
- Delivery is in-process only. Cross-process events at Tier 4 travel over the
  transport seam ([ADR-0005](0005-transport-seam.md)), not this bus.

**Operational note:** a non-zero and rising `dropped` count on
`/api/v1/system/runtime` means a subscriber is too slow for its event rate.
Fix the consumer, raise its buffer, or narrow its subscription pattern. See the
[troubleshooting guide](../operations/troubleshooting.md).
