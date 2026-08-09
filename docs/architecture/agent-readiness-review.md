# Agent Architecture Readiness Review

**Scope:** the agent and control-plane architecture as it stands after Phase 1,
assessed against what mature infrastructure agents — Datadog Agent, Grafana
Alloy, Elastic Agent, New Relic Infrastructure — actually do in production.

**Question asked:** can this run on thousands of Linux servers for years with
minimal maintenance?

**Verdict:** the *foundations* are sound and the seams are in the right places.
But there are **four defects that would cause data loss or a security incident**
in a fleet deployment, and roughly a dozen capability gaps. None require
rewriting the architecture. All of them require building things that do not
exist yet.

The rest of this document is the list, in priority order, with the reason each
one matters and what to do about it.

---

## What is genuinely solid

Stated briefly, because the useful part of a review is the problems.

- **The transport seam** ([ADR-0005](../adr/0005-transport-seam.md)) means the
  agent is a composition change, not a rewrite. This is the single decision
  that makes everything below tractable.
- **Collector safety rules** — per-run timeout, non-overlap, concurrency
  ceiling, jitter, panic isolation — are implemented and tested. Most agents
  acquire these after an incident; having them first is the right order.
- **Identity** is derived and stable across hostname changes and re-imaging.
- **Origin on every observation** means the schema is fleet-ready with no
  migration.
- **Layering** is enforced and the dependency direction is correct.

---

# CRITICAL — would cause data loss or a breach

## C1. The server trusts the `node_id` the agent sends

**Current state.** `metric.Repository.InsertBatch` writes
`env.Origin.NodeID` straight into storage
([repository.go:63](../../internal/storage/metric/repository.go)). In-process
this is fine — the value came from local configuration. Over a network it is
attacker-controlled input.

**Why it matters.** Any agent holding a valid certificate could submit
observations attributed to *any other node*. That is enough to forge a healthy
CPU reading for a host that is failing, hide a disk filling up, or poison
another team's capacity plan. It also breaks tenancy: one compromised
low-value host can write to every node's series.

**Recommendation.** Bind identity to the transport credential, never the
payload.

- The client certificate's subject carries the authoritative `node_id`.
- The ingest handler overwrites `Origin.NodeID` from the verified peer
  certificate; a mismatch with the payload is rejected and logged as a
  security event.
- Introduce `transport.Authenticated{Envelope, PeerNodeID}` so the type system
  makes it impossible to reach the repository with an unverified origin.

**Applies to:** Tier 4, but the `Origin` handling should be structured for it
now, while there is one call site.

---

## C2. Ingest is not idempotent, but delivery will be at-least-once

**Current state.** `InsertBatch` uses `COPY` with no deduplication. The
`transport.Sink` doc comment already promises implementations are idempotent on
`Envelope.ID` — storage does not honour that contract.

**Why it matters.** Any networked transport retries. A retry after a timeout
where the write actually succeeded duplicates every sample in the batch.
Duplicated samples corrupt exactly the aggregates operators trust: averages
skew, counters double, and a continuous aggregate materialises the wrong value
permanently — it is not recomputed once the bucket closes.

**Recommendation.**

- An `ingested_envelopes(envelope_id, node_id, received_at)` table, written in
  the same transaction as the samples, with a primary key on `envelope_id`.
  A conflict means "already applied" — commit nothing and return success.
- A retention policy on that table matched to the transport's maximum retry
  horizon (24 hours is ample). It must not grow forever.
- Reject envelopes whose `SentAt` is older than the dedup window, since they
  can no longer be checked.

---

## C3. There is no local spool — an outage loses observations

**Current state.** Nothing buffers. `InProcess.Send` is synchronous; on failure
the scheduler logs and drops.

**Why it matters.** Control-plane restarts, deploys, and network partitions are
routine. Every one of them currently produces a permanent hole in the metrics —
and the holes appear precisely during incidents, when the data is most needed.
Datadog, Elastic, and Alloy all persist to disk for this reason.

**Recommendation.** A bounded, disk-backed spool between scheduler and
transport.

- Size-bounded (e.g. 512 MB) and age-bounded, both configurable.
- Drop **oldest** on overflow, with a counter and a rate-limited warning.
  Recent data is more valuable than old data during a recovery.
- `fsync` policy configurable: durability versus disk wear on SSD-constrained
  hosts.
- Replay is ordered and idempotent — which depends on C2.

---

## C4. No cardinality enforcement

**Current state.** The docs say "keep label cardinality bounded". Nothing
checks. `collect.Sample.Validate` does not count series.

**Why it matters.** This is the single most common way a metrics platform is
destroyed. One collector labelling by PID, container ID, or request path
generates unbounded series; the hypertable, its indexes, and every continuous
aggregate grow without limit until the database is unusable. It is usually a
one-line bug in a plugin, and by the time it is noticed the damage is done.

**Recommendation.** Enforce in the scheduler, where every sample already passes.

- A per-collector series budget (default ~1000 distinct label combinations).
- On exceeding it: drop the new series, keep existing ones, increment a
  counter, publish `collector.cardinality.exceeded`, and mark the collector
  degraded.
- Expose current series count per collector on `/api/v1/collectors`.
- A hard global ceiling as a backstop.

---

# HIGH — would not scale, or would fail under stress

## H1. Node liveness updates will not survive a fleet

**Current state.** Every batch runs `upsertNodeSQL` — an `UPDATE` on the
`nodes` row.

**Why it matters.** At 1,000 agents × 7 collectors × 4 runs/minute that is
**28,000 UPDATEs per minute** on one small table. Each produces a new row
version, WAL, and index churn; autovacuum will struggle, and rows will contend.
This is a classic hot-row problem and it arrives well before 1,000 nodes.

**Recommendation.**

- Separate liveness from ingest. Keep `last_seen_at` in memory on the control
  plane and flush periodically (every 15–30s) in one batched `UPDATE ... FROM
  (VALUES ...)`.
- Or move liveness to an explicit lightweight heartbeat, decoupled from
  collection frequency.
- Either way, `last_seen_at` should not be written on the sample path.

## H2. One transaction per batch will not scale

**Current state.** Each envelope is its own transaction.

**Why it matters.** 1,000 agents × 7 collectors = 7,000 transactions per
interval. Transaction overhead dominates, and connection pool pressure becomes
the bottleneck long before disk does.

**Recommendation.** An ingest queue on the control plane that accumulates
envelopes across agents for a short window (100–250 ms) and writes them in one
`COPY`. Preserves idempotency if C2 is keyed per envelope.

## H3. No backpressure, and a guaranteed thundering herd

**Current state.** No flow control. Reconnection has no jitter (the agent does
not exist yet, but the design has not specified it).

**Why it matters.** When the control plane restarts, every agent reconnects at
once *and* replays its spool at once. That is a self-inflicted denial of
service at the moment of recovery — the system's worst behaviour occurs exactly
when it is already degraded.

**Recommendation.**

- Server-signalled backpressure in the protocol: `SLOW_DOWN`, `PAUSE`, and a
  suggested retry delay.
- Full jitter on reconnect (`random(0, min(cap, base·2^n))`), not fixed
  exponential backoff.
- Spool replay rate-limited and interleaved with live data, so a recovering
  agent does not starve its own current observations.
- Per-agent admission control on the control plane.

## H4. No streaming-collector support in the scheduler

**Current state.** `collect.Collector` is pull-only. The scheduler has no
concept of a long-running source. Docker events and log tails would have to
spawn unmanaged goroutines inside `Plugin.Init`.

**Why it matters.** This is Phase 2, and it is the one place the plugin
architecture is *not* currently sufficient. Unmanaged goroutines get no
timeout, no panic isolation, no restart, no health reporting, and no clean
shutdown — every safety property the scheduler provides for pull collectors
would be absent for exactly the collectors that run continuously.

**Recommendation.** Add a second contract before Phase 2, not during it.

```go
// Streamer is a collector whose source pushes.
type Streamer interface {
    Descriptor() Descriptor
    // Stream runs until ctx is cancelled. The scheduler restarts it with
    // backoff if it returns early.
    Stream(ctx context.Context, out chan<- Sample) error
}
```

The scheduler supervises these with the same guarantees: panic isolation,
restart with backoff, health reporting, bounded shutdown.

## H5. Plugins have no configuration

**Current state.** `plugin.Env` carries a logger, the bus, and a node ID.
Nothing else.

**Why it matters.** The Redis plugin needs an address and credentials. Postgres
needs a DSN. Docker needs a socket path. Nginx needs a status URL. Without a
typed per-plugin configuration mechanism, Phase 2 will bolt one on ad hoc and
every subsequent plugin will copy it.

**Recommendation.**

- `plugin.Env.Config json.RawMessage`, unmarshalled by the plugin into its own
  typed struct, populated from a `plugins.<id>` YAML section.
- Secrets follow the existing `_FILE` convention — never inline in YAML.
- Plugins expose a `Validate(config) error` so a typo fails at startup with
  the same "all violations at once" treatment as core configuration.

## H6. Clock skew will corrupt series

**Current state.** Samples are timestamped by the agent (`time.Now()` in each
collector). Nothing validates them.

**Why it matters.** A host with a broken clock writes samples hours in the
future or past. Future-dated samples break `time_bucket` aggregation and can
sit beyond retention forever; past-dated samples land in already-materialised
continuous aggregate buckets that will never be recomputed. One bad NTP
configuration silently corrupts a node's history.

**Recommendation.**

- The ingest handler compares `Envelope.SentAt` with server time and records
  the skew per node.
- Reject or clamp samples outside a tolerance (e.g. ±5 minutes), with a
  counter and an event.
- Surface `clock_skew_seconds` on the node, and alert on it. Most fleets have
  a few bad clocks and nobody knows which.

---

# MEDIUM — needed for "years with minimal maintenance"

## M1. No protocol or schema versioning, and no capability negotiation

An agent fleet is never uniformly versioned. Without a handshake, a new agent
sending a new field to an old control plane — or the reverse — fails in
undefined ways.

**Recommend:** a connect handshake exchanging protocol version, agent version,
and capability flags; explicit min/max supported versions on both sides; a
documented compatibility window (agent may be N-2 relative to server); refusal
with a clear diagnostic outside it.

## M2. No configuration hot-reload

Configuration is read once at startup (deliberately — it makes config immutable
during a run). But restarting 1,000 agents to change a collection interval is
not viable.

**Recommend:** SIGHUP triggers re-resolution and rebuilds only the affected
components via the existing lifecycle supervisor. Bind address and database DSN
remain restart-only; collection intervals, log level, and plugin enablement
become reloadable.

## M3. No upgrade strategy

**Recommend:** the control plane already receives `agent_version` on every
observation — build the fleet inventory view on it. Add staged rollout support
(canary cohort by label), an explicit "agent is outdated" signal, and a
documented rule that the control plane must support agents two minor versions
back.

Self-update is deliberately *not* recommended: an agent that can replace its
own binary is an agent that can be turned into a fleet-wide remote code
execution primitive. Leave upgrades to the package manager or orchestrator,
which is also what Elastic Agent's critics wish it did.

## M4. No certificate revocation path

Certificates are proposed; revoking one is not designed. A compromised host
must be ejectable in minutes.

**Recommend:** short-lived certificates (24h) with automatic renewal — the
simplest revocation is expiry. Plus a server-side denylist by node ID checked
at connect, so ejection does not wait for expiry.

## M5. No audit logging

Atlas exposes a complete infrastructure inventory. There is currently no record
of who read what.

**Recommend:** a structured audit stream (separate from application logs) for
authentication, authorisation failures, enrollment, revocation, and reads of
sensitive endpoints. Required for SOC 2 and ISO 27001, which an enterprise
buyer will ask about.

## M6. Agent self-limiting is incomplete

The scheduler bounds concurrency and per-run time. The *process* has no ceiling.

**Recommend:** `GOMEMLIMIT` set from configuration; a self-monitoring watchdog
that sheds collectors (longest interval first) if the agent exceeds its own CPU
or memory budget; document cgroup limits as the deployment-level backstop.
An agent that OOMs the host it monitors is worse than no agent.

## M7. Observability of the agent is thin

`CollectorHealth` reports last run, failures, and last error. Missing: run
**duration** distribution, samples produced per run, spool depth, delivery lag,
bytes sent, reconnect count, and dropped-sample counters.

**Recommend:** extend `CollectorHealth`; expose a Prometheus-format endpoint so
Atlas agents can be monitored by whatever the organisation already runs. A
monitoring agent that cannot be monitored by other tooling is a hard sell.

## M8. No pprof, no debug surface

**Recommend:** `net/http/pprof` on a separate loopback-only port, disabled by
default. Diagnosing a goroutine leak on a production agent without it means
guessing.

## M9. Continuous aggregate refresh at fleet scale

The refresh policies were sized for one node. At 1,000 nodes the 1-minute
aggregate refresh scans substantially more data every minute.

**Recommend:** load-test before Tier 4; be prepared to widen
`schedule_interval`, narrow `start_offset`, or partition by node group.

## M10. Plugin failure isolation is incomplete

`Detect` and `Collect` have panic isolation. A plugin's own goroutines do not —
a panic there kills the process. This is resolved by H4 if streaming
goroutines become scheduler-supervised.

---

# What does *not* need changing

Stated explicitly, so the review is not read as "rewrite everything":

- **The transport seam.** Correct, and it is what makes all of the above
  incremental.
- **The plugin lifecycle** (detect → init → contribute → close). Adding
  `Streamer` and per-plugin config extends it; it does not replace it.
- **The layering rule.** Every fix above lands in a clear layer.
- **Denormalised metric storage.** Compression makes it viable; cardinality
  control (C4) is what it actually needs.
- **The lossy event bus.** Correct for notifications. Observations correctly do
  not use it.
- **Forward-only migrations.** More correct at fleet scale, not less.

---

# Recommended sequencing

| When | Items | Rationale |
| --- | --- | --- |
| **Phase 1 (now)** | C4 cardinality limits | One buggy collector destroys the database. Cheapest to add while there is one plugin. |
| **Phase 2** | H4 Streamer, H5 plugin config, M8 pprof | Docker forces both, and retrofitting after three plugins exist costs three times as much. |
| **Phase 2–3** | C2 idempotent ingest, H1 liveness, H6 clock skew | Correctness and hot-row problems; fix before the data volume makes migration painful. |
| **Phase 4–5** | C1 identity binding, C3 spool, H2 batch ingest, H3 backpressure, M1 negotiation, M4 revocation, M5 audit | The agent phase itself. C1 and C3 are the two that must not slip. |
| **Ongoing** | M2 reload, M3 upgrade, M6 self-limits, M7 telemetry, M9 aggregates | Operability hardening. |

---

# Summary

The architecture is right. The gaps are real.

The four that would actually hurt are **C1** (a compromised agent can forge
other nodes' data), **C2** (retries will duplicate samples), **C3** (any outage
loses data permanently), and **C4** (one buggy collector can destroy the
database). C4 is cheap and should be done now; the other three land with the
agent work.

The most under-appreciated item is **H4**. The plugin architecture is genuinely
extensible for *pull* collectors and is not yet extensible for *streaming*
ones — and Phase 2 is entirely streaming sources. Adding `Streamer` before
Docker rather than during it is the difference between a clean extension and
three plugins' worth of unmanaged goroutines to retrofit.
