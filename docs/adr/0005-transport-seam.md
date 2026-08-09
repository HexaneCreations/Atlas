# ADR-0005: A transport seam between collection and storage

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

Atlas ships first as a single binary observing its own host. Tier 4 requires
agent-based monitoring of many servers, with secure communication between
agents and a control plane.

These are the same collectors and the same storage with a different distance in
between. The question is when to acknowledge that distance in the design.

The failure mode being avoided is concrete and common. If the scheduler calls
storage directly, then adding agents means every collector's output path
changes: batches must become serialisable, origin must be threaded through the
schema, the ingest path must handle duplicates and out-of-order arrival, and
every repository must learn that data can come from elsewhere. That is not a
refactor. It is the largest single rewrite visible anywhere in the roadmap, and
it would land at the moment the product is under the most pressure to add
features.

## Decision

**Put a `Transport` interface between collection and storage from the first
day, and make every observation carry an `Origin`.**

```
Collector → Scheduler → Transport → Sink → Storage
                            ▲
              ┌─────────────┴─────────────┐
       InProcess (today)          gRPC/mTLS (Tier 4)
```

- `transport.Envelope` wraps a `collect.Batch` with an `Origin` (node id,
  hostname, agent version), an envelope id, and a send timestamp.
- `transport.Transport` has one method that matters: `Send(ctx, Envelope)`.
- `transport.Sink` is the receiving end.
- `transport.InProcess` delivers straight to the sink with no serialisation and
  no copying.

**Origin is unconditional**, including in single-node deployments where it is
always the local host. The storage schema, query layer, and UI are therefore
multi-node from the start, and Tier 4 adds agents without a data migration.

## Alternatives considered

**Call storage directly; add the seam at Tier 4.** Less code now. Rejected for
the reason above: it converts a bounded interface change into an unbounded
rewrite touching every collector and every repository, at the worst possible
time.

**Build the full agent and control plane immediately.** The most correct
end-state, and it would settle the design questions early. Rejected because it
front-loads certificate authority setup, agent enrollment, and agent lifecycle
management before a single metric is on screen. Those are substantial systems
whose requirements are better understood after the collectors exist.

**Use the event bus as the transport.** It already exists and is already a
seam. Rejected because the guarantees are wrong in a way that would be
silently harmful: the bus is deliberately lossy and non-blocking
([ADR-0008](0008-lossy-event-bus.md)), which is correct for notifications and
unacceptable for the durable metric record. Overloading one mechanism with two
delivery guarantees is how a system ends up quietly dropping the data it is
paid to keep.

## Consequences

**Good.**

- Tier 4 is a transport implementation plus a deployment topology. No collector
  changes; no repository changes.
- The seam is free where it is not needed: `InProcess` is a direct method call,
  no serialisation, no queueing, no copy. An abstraction that taxed the common
  case would eventually be removed, and then the rewrite would happen anyway.
- Envelope validation gives one choke point where a malformed observation is
  rejected before it can reach storage — far cheaper than finding and removing
  it from a time-series store later.
- The transport interface is a natural place for the buffering, retry, and
  batching that a networked agent needs, with none of it polluting collectors.

**Costs.**

- One extra indirection, and one extra concept to learn.
- `Origin` is carried in every envelope and stored on every row, including in
  single-node deployments where it is constant. That is real storage overhead
  for data that is currently redundant. Accepted deliberately: the alternative
  is a schema migration over the largest tables in the system, at exactly the
  point when they are largest.
- The in-process transport delivers synchronously, so a slow sink is
  back-pressure onto collection. This is intended — unlike events, observations
  must not be silently dropped — and is bounded by the scheduler's per-run
  timeout.

**Revisit if** Tier 4 is removed from the roadmap entirely.
