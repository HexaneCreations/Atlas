# Phase Plan

The [roadmap](../../Roadmap.md) defines six tiers of capability. This document
maps them onto delivery phases: what is built, in what order, and why that
order.

Each phase ends with working, tested, documented software. A phase is complete
when its code is merged, its tests pass, and its documentation is updated —
not when its code compiles.

---

## Phase 0 — Architecture and project foundation ✅ Complete

**Goal:** establish the structure everything else is built on, with no
application features.

**Delivered:**

- Three-layer architecture with an enforced dependency direction
  (`platform` → `core` → `api`).
- Platform: typed errors with a redaction boundary, structured logging with
  context propagation and credential redaction, layered configuration with
  startup validation, identifier generation, a non-blocking event bus, a
  lifecycle supervisor, HTTP plumbing, health aggregation, and a
  PostgreSQL/TimescaleDB layer with a forward-only migrator.
- Core contracts: `collect` (Sample, Batch, Collector, Registry), `transport`
  (the seam that makes Tier 4 a swap), `scheduler` (with the safety rules that
  keep Atlas from harming its host), and `plugin` (detect, init, contribute,
  close).
- API: versioned `/api/v1` with system info, health, and runtime telemetry;
  orchestration probes outside versioning.
- Composition root, `atlas-server` with `serve`/`migrate`/`config`/`version`.
- Frontend scaffold: React 19, TypeScript in strict mode, Vite, TanStack Query,
  a typed API client mirroring the Go error envelope.
- Build and CI: Makefile, `scratch`-based container image, GitHub Actions with
  unit, integration, frontend, and image jobs.
- Documentation: this tree, including ten ADRs.

**Not delivered, deliberately:** no collectors, no plugins, and therefore no
composed scheduler. The `scheduler` and `transport` packages are complete and
tested libraries; they are wired into the running server in Phase 1, together
with the first collectors and the storage they write to. Composing a scheduler
with an empty registry and a sink that stores nothing would have been
scaffolding, not architecture.

---

## Phase 1 — Server monitoring (Tier 1, part 1)

**Goal:** the first real observations, end to end, from `/proc` to the browser.

- Metric storage: hypertables for samples, a `Sink` implementation, retention
  and continuous-aggregate policies.
- Compose the scheduler and in-process transport into the server.
- A `system` plugin with collectors for hostname, OS and kernel, uptime, CPU,
  memory, swap, disk usage, disk I/O, network, and load average.
- Node identity: a stable `node_id` that survives hostname changes and
  re-imaging.
- Query API for current and historical metrics, including the list-endpoint
  conventions (pagination, filtering, sorting) that later phases inherit.
- Frontend: a server overview with live metrics and time-series charts.

**Why first:** it exercises the entire pipeline with real data and settles the
storage schema, the query conventions, and the chart primitives that every
later tier reuses.

---

## Phase 2 — Containers, processes, services, and cron (Tier 1, part 2)

- Docker plugin: containers, health, per-container CPU and memory, restart
  counts, images, networks, volumes, live stats.
- Docker events streamed onto the event bus — the first genuinely event-driven
  source.
- WebSocket streaming to the browser, with the fan-out subscriber.
- Live container logs.
- Process and binary monitoring: PID, CPU, memory, user, executable path,
  running time.
- Service monitoring via systemd, plus nginx, Redis, PostgreSQL, MySQL, and SSH.
- Cron monitoring: user, root, and system jobs, with schedules and failure
  detection.
- Ports, listening services, mounted disks, SSL certificate status.
- Also: OpenAPI generation for the now-substantial API surface, replacing
  hand-written TypeScript types.

**Why here:** it completes Tier 1, and Docker events force the event-driven and
streaming paths to be built properly rather than retrofitted.

---

## Phase 3 — Enterprise operations platform (Tier 2)

- Service catalog: logical services over containers, with owner, team,
  repository, documentation, environment, and dependencies.
- Dependency graph with impact analysis.
- Ownership and team metadata across every resource.
- Operational runbooks attached to services.
- Incident timeline, populated from the event bus by a durable subscriber.
- Deployment history and Git integration.
- Health scoring per server and service.
- Alert correlation to group cascading failures.
- Capacity planning from historical metrics.

**Why here:** all of it is context layered over the inventory Phases 1 and 2
produce. It cannot be built before the things it describes exist.

---

## Phase 4 — Reliability engineering (Tier 3)

- SLO dashboard and golden signals: availability, latency, traffic, errors,
  saturation.
- Alert rules, alert history, and a notification service.
- Incident investigation and root-cause analysis views.
- Capacity forecasting.

**Why here:** SLOs need historical data to be meaningful, and alert correlation
from Phase 3 is what keeps alerting from being noise.

---

## Phase 5 — Platform architecture (Tier 4)

- **Agent-based multi-server monitoring.** `atlas-agent` as a second binary; the
  gRPC transport implementation; agent enrollment and lifecycle.
- mTLS between agent and control plane, with certificate rotation.
- Authentication (OIDC and API tokens) and RBAC.
- Audit logging.
- Feature flags and richer configuration management.

**Why last among the capability tiers:** the transport seam
([ADR-0005](../adr/0005-transport-seam.md)) means this phase adds a transport
implementation and a deployment topology rather than restructuring collection.
Doing it earlier would have front-loaded PKI and enrollment work before any
collector existed to justify its requirements.

---

## Tiers 5 and 6 — Engineering excellence and documentation

These are not phases. Clean architecture, SOLID adherence, comprehensive
testing, structured logging, error handling, performance, security,
observability, and complete documentation are **standing requirements applied
within every phase**.

Treating them as a phase would mean deliberately accruing debt and scheduling
its repayment, which is how the repayment gets cut. Concretely:

- Every phase ships tests at the level its code demands
  ([testing strategy](../development/testing.md)).
- Every phase updates the documents its changes affect.
- Every significant decision gets an ADR in the phase that makes it.
- CI enforces formatting, vetting, linting, and race-free tests on every
  change.

---

## Sequencing principles

1. **Deliver a working slice, not a layer.** Phase 1 goes from `/proc` to a
   chart. A phase that delivered "all the storage" would be unverifiable.
2. **Build the seam before you need it, but not the implementation.** The
   transport interface exists from Phase 0; the gRPC transport waits until
   there is a fleet to serve.
3. **Let requirements be discovered by use.** Agent enrollment is designed
   after collectors exist, when what an agent must carry is known rather than
   guessed.
4. **Never defer the foundations.** Testing, documentation, and security are
   inside every phase, because deferred quality is not deferred — it is
   cancelled.
