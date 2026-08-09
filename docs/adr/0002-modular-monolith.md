# ADR-0002: Modular monolith with enforced layering

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

The roadmap describes something that looks like several services: metrics
collectors, event collectors, an alert engine, a notification service, a
scheduler, data aggregation, cleanup jobs, and historical metric processing.
The requirements explicitly say to avoid placing all responsibilities inside
the API server.

That instruction is about coupling, not about process boundaries — and the two
are routinely confused. A system can be one process and well separated, or five
services and hopelessly entangled.

Atlas's realistic deployment is also relevant: a single node observing itself,
or a modest control plane serving a fleet. It is not a system with independently
scaling components under wildly different load.

## Decision

**One deployable — `atlas-server` — internally divided into three layers with a
strictly one-directional dependency rule:**

```
internal/api/       HTTP surface
internal/core/      domain contracts
internal/platform/  technical capability
```

`platform` may not import `core` or `api`. `core` may not import `api`.

Background responsibilities are **components**, not services: each satisfies
`lifecycle.Component`, has its own goroutines and its own start and stop, and
communicates with the rest through the event bus and the transport seam rather
than by direct calls. Extracting one into a separate process later means
replacing its in-process transport, not restructuring it.

## Alternatives considered

**Microservices from the start.** Genuinely independent scaling and failure
isolation, matching how the roadmap describes the workers. Rejected because it
buys those properties at a price Atlas cannot yet justify: service discovery,
network serialisation between collection and storage, distributed tracing to
follow a single collection run, and a deployment story that is hostile to the
single-node case that Atlas must serve well. The failure isolation is also
partly illusory — every service depends on the same database.

**An unstructured monolith.** Fastest initially. Rejected because it is the
outcome the requirements explicitly warn against, and because at Tier 4 the
extraction becomes a rewrite rather than a refactor.

**A plugin architecture using Go's `plugin` package, with separate `.so`
files.** Rejected for the reasons in
[ADR-0006](0006-compiled-in-plugins.md).

## Consequences

**Good.**

- One binary to build, ship, and debug. During an incident, one process holds
  the whole picture.
- The layering rule makes the platform testable with no domain concept present
  and the domain testable with no infrastructure — which is why the Phase 0
  test suite runs in seconds without a database.
- A function call between components costs nothing, so the seams can be fine
  without a performance penalty for having them.
- Extraction later is bounded: a component already has a lifecycle, already
  communicates over a seam, and already has no hidden coupling to its callers.

**Costs.**

- The layering rule is enforced by review, not by the compiler. Go's `internal`
  visibility prevents external import but does not express intra-module
  layering. A violation is a normal-looking import statement.
  *Mitigation:* the rule is stated in [coding standards](../development/coding-standards.md)
  and is a review checklist item. If violations recur, add an import-boundary
  linter.
- Everything shares a process, so a panic anywhere could in principle take down
  everything. *Mitigation:* panic isolation at all three boundaries where
  untrusted-ish code runs — the HTTP recoverer, the scheduler's `safeCollect`,
  and the plugin registry's `safeDetect` — each tested.
- Components cannot be scaled independently. Acceptable for the deployments
  Atlas targets; revisit if metric ingest volume alone justifies horizontal
  scaling.

**Revisit if** ingest volume requires scaling collection separately from the
API, or if a single team boundary needs to own one component's release cycle.
