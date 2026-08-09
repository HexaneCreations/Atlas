# ADR-0001: Go backend with a React and TypeScript frontend

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

Atlas must, over its roadmap:

- Read Linux host state directly: `/proc`, `/sys`, cgroup v2 hierarchies,
  mount tables, socket tables.
- Talk to the Docker Engine API, systemd over D-Bus, and eventually Kubernetes.
- Hold many long-lived concurrent streams — live container logs, Docker event
  streams, WebSocket fan-out to browsers — while also running scheduled
  collectors.
- Ship an agent to arbitrary hosts in a heterogeneous fleet, where installing a
  language runtime is a negotiation with whoever owns those hosts.
- Be maintained by multiple senior engineers for years.

The frontend is a dense operational console: live-updating tables, dependency
graphs, log tails, and time-series charts.

## Decision

**Backend, agent, and workers in Go. Frontend in React 19 with TypeScript,
built by Vite.**

## Alternatives considered

**Node.js or TypeScript for the backend.** One language across the stack, and
shared types between API and client, are genuine benefits. Rejected because the
agent story is poor: shipping a Node runtime to every monitored host is a
significant imposition, and a single-threaded event loop is a bad fit for
high-frequency metric collection combined with many long-lived log streams. A
split — TypeScript control plane, Go agent — keeps the frontend benefit but
doubles the toolchain, and puts the two halves of the collection pipeline in
different languages, which is exactly the boundary that must stay cheap to
cross.

**Python with FastAPI.** The fastest to prototype, with the best libraries for
the Tier 3 forecasting and capacity-planning work, and `psutil` covers much of
Tier 1 immediately. Rejected on deployment and concurrency: the GIL makes a
collector competing with a busy API server unpredictable, and there is no
single-binary agent. The forecasting advantage is real but arrives at Tier 3,
and can be served by a separate analysis service if it ever justifies one.

**Rust.** Best-in-class performance and the strongest safety guarantees.
Rejected on ecosystem and staffing: the Docker and systemd client libraries are
markedly less mature than Go's, and the roadmap's stated constraint is a system
maintainable by a rotating team of senior engineers over years. Go is
substantially easier to hand over.

**A server-rendered frontend (Go templates, HTMX).** Simpler, one language,
no build step. Rejected because the dependency graph, live log viewer, and
correlated time-series charts in Tiers 2 and 3 are genuinely interactive; the
approach would be abandoned mid-roadmap, and the migration would be worse than
starting with a component model.

## Consequences

**Good.**

- A single static binary, roughly 15 MB, with no runtime dependency. It ships
  to any Linux host and runs in a `scratch` container.
- Direct, allocation-cheap access to `/proc` and cgroup files, which is
  precisely the hot path.
- Goroutines make one-per-log-stream and one-per-collector natural rather than
  an architectural problem.
- Mature first-party libraries: the Docker SDK and `go-systemd` are both Go.
- The race detector is built in, and Atlas relies on it heavily — the
  scheduler, event bus, and supervisor are all concurrent, and every test suite
  runs under `-race` in CI.
- TypeScript's strict mode catches missing API fields at build time rather than
  rendering them as blank cells in an operator's console.

**Costs.**

- Two languages and two toolchains, with the associated context switch.
- No shared type definitions between Go and TypeScript. The API types are
  hand-written in `web/src/api/types.ts` and can drift. Acceptable at the
  current API size; the mitigation, when the surface grows, is generation from
  an OpenAPI document, noted in the [API reference](../api/README.md).
- Go's error handling is verbose. Partly addressed by the typed error kernel in
  [ADR-0009](0009-typed-error-kernel.md).
- Statistical forecasting for Tier 3 will be more work in Go than in Python.
  Revisit if that tier grows beyond straightforward regression; a separate
  analysis service is the escape hatch.

**Revisit if** the fleet agent requirement disappears entirely, or if Tier 3
forecasting becomes the dominant workload.
