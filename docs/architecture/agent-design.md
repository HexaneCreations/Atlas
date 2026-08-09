# The Atlas Agent

**Status:** Design, pending approval. No ADRs written yet — an ADR records an
*accepted* decision, and the choices below are still proposals.

**Scope:** the design for machine identity, secure enrollment, push transport,
remote inventory, and fleet synchronisation. This is the milestone that turns
Atlas from a tool that observes its own host into one that observes a fleet.

**Prerequisite met:** [ADR-0005](../adr/0005-transport-seam.md) put a
`Transport` interface between collection and storage on day one, and Phase 0
put an `inventory.Scope` seam between the API and the plugins. Both exist
precisely so this milestone is a composition change rather than a rewrite.
This document is largely an account of cashing in those two decisions.

---

## 1. Architecture

### The agent is not a new program

The single most important architectural fact: **`atlas-agent` is the existing
collection pipeline with the transport swapped and the server removed.**

Today `atlas-server` composes:

```
plugins → collect.Registry → scheduler → transport.InProcess → metric.Sink → Postgres
                                                                    ↑
                                                            api/v1 reads ─┘
```

The agent composes the same left-hand side against a different transport:

```
plugins → collect.Registry → scheduler → transport.Remote → spool → HTTPS → control plane
```

No collector changes. No plugin changes. No scheduler changes. The scheduler
already emits `transport.Envelope` values carrying a full `Origin`, because
ADR-0005 made origin unconditional even when it was always the local host.

### Two binaries, one codebase

| | `atlas-server` | `atlas-agent` |
| --- | --- | --- |
| Collects | Yes — its own host | Yes — its own host |
| Stores | Yes (TimescaleDB) | No |
| Serves the API | Yes | No |
| Inbound network surface | HTTPS API | **None** |
| Transport | `InProcess` + ingest from agents | `Remote` |

The control plane keeps collecting its own host. It is a node like any other,
and this is what keeps the single-binary deployment working unchanged — a user
who never deploys an agent sees exactly what they see today.

### New packages

```
internal/agent/                  agent composition root (mirrors internal/app)
internal/core/transport/remote/  the HTTPS transport: batching, retry, backoff
internal/core/transport/spool/   bounded disk buffer between scheduler and network
internal/core/fleet/             control-plane side: nodes, credentials, enrollment
internal/core/inventory/         extended: Source, Snapshot, local + remote resolvers
internal/api/ingest/             the mTLS ingest endpoints
internal/storage/fleet/          node credentials, enrollment tokens, inventory snapshots
internal/storage/inventory/      latest-snapshot-per-(node,subject) storage
cmd/atlas-agent/                 the agent binary
```

The layering rule holds: `platform → core → api`. `internal/core/fleet` knows
nothing about HTTP; `internal/api/ingest` is the only place that touches a
peer certificate.

### Two data paths, deliberately different

Metrics and inventory are not the same kind of data and must not share a path.

**Metrics** are an append-only time series. They are small, frequent, ordered,
and their value is cumulative — a gap is a permanent hole. They go through the
existing `Envelope`/`Sink`/hypertable path, and they must be spooled through an
outage.

**Inventory** is a snapshot of current state. It is large, less frequent,
and only the newest one matters — an inventory snapshot from during an outage
has no value once a newer one arrives. It goes to a separate endpoint and is
stored as **latest-only, one row per (node, subject)**, replaced on write.

Storing inventory as history would be ruinous and pointless: 494 processes at
~200 bytes is ~100 KB per snapshot; at 1,000 nodes every 60 seconds that is
~140 GB/day to store a list that is almost entirely unchanged. Latest-only is
6,000 rows total. Inventory *history* — "what changed on this host last
Tuesday" — is a genuine future feature and a genuinely different design
(change events, not snapshots). It is not this milestone.

Consequently inventory is **never spooled**. If the control plane is
unreachable, the agent skips the push and sends the next one.

---

## 2. Protocol

### HTTPS with JSON, not gRPC

[ADR-0005](../adr/0005-transport-seam.md) sketched "gRPC/mTLS (Tier 4)" as the
likely implementation. Having now sized the problem, **I recommend against
gRPC**, and this design supersedes that hint.

The reasoning:

- **We are not near the scale where the wire format matters.** At 1,000 nodes
  × 7 collectors × 4 runs/minute, with 10 envelopes batched per request, that
  is ~47 requests/second. Go's `net/http` is not the constraint; nothing in
  this workload is.
- **gRPC means two server stacks.** Two listeners, two middleware chains, two
  authentication mechanisms, two error-mapping layers. ADR-0011 already
  identifies the duplicated `BaseMiddleware`/`StreamMiddleware` chains as a
  hazard; a third path makes it worse. Over HTTPS, ingest reuses `httpx`, the
  error kernel (ADR-0009), and the access log.
- **It traverses everything.** Corporate proxies, load balancers, service
  meshes, and inspection appliances all handle HTTPS. gRPC is regularly the
  thing that does not work at a customer site, and debugging it there is
  miserable.
- **It is inspectable.** An operator can reproduce an agent's request with
  `curl`. That is worth a great deal during a support call.

The cost is real and should be stated: JSON is roughly 3–5× larger on the wire
than protobuf. **zstd compression closes most of that gap** — metric batches
are highly repetitive and compress by roughly 8–10×. We accept a modestly
larger payload in exchange for one server stack.

**Revisit if** measurements show ingest CPU or bandwidth becoming a real
constraint, or if a deployment needs true bidirectional streaming. The
`Transport` interface means that change stays local, which is the whole point
of having it.

### Endpoints

All under `/api/v1/agent/`, all requiring mTLS except `enroll`.

| Method | Path | Auth | Purpose |
| --- | --- | --- | --- |
| `POST` | `/agent/enroll` | Enrollment token | Exchange a CSR for a client certificate |
| `POST` | `/agent/renew` | mTLS | Rotate a certificate before expiry |
| `POST` | `/agent/ingest` | mTLS | Deliver a batch of metric envelopes |
| `POST` | `/agent/inventory` | mTLS | Deliver an inventory snapshot |
| `POST` | `/agent/heartbeat` | mTLS | Liveness and agent self-telemetry |

### Request shape

`POST /api/v1/agent/ingest`, `Content-Encoding: zstd`:

```json
{
  "protocol_version": 1,
  "envelopes": [ { "id": "...", "origin": {...}, "batch": {...}, "sent_at": "..." } ]
}
```

Batching multiple envelopes per request is what addresses **H2** from the
[readiness review](agent-readiness-review.md): one transaction per envelope
would mean 7,000 transactions per interval at 1,000 nodes.

### Response shape — the backpressure channel

```json
{
  "accepted": 12,
  "rejected": [ { "envelope_id": "...", "reason": "clock_skew" } ],
  "directive": "ok" | "slow_down" | "pause",
  "retry_after_ms": 0
}
```

The `directive` is how the control plane defends itself. **H3** in the
readiness review describes the failure this prevents: when the control plane
restarts, every agent reconnects *and* replays its spool simultaneously — a
self-inflicted denial of service at the exact moment the system is already
degraded. The server can tell agents to back off, and agents must obey.

Note carefully what this channel is and is not. It carries **flow control
only** — a fixed, closed set of three values. It is not a command channel.
See §3.

### Versioning and compatibility

`protocol_version` is negotiated at enrollment and asserted on every request.
The control plane supports agents **two minor versions back** (M1). Outside
that window the request is refused with a typed error naming the versions
involved, rather than failing in an undefined way when a new field meets an
old parser.

### Retry and backoff

- **Full jitter**: `sleep = random(0, min(cap, base · 2ⁿ))`. Not exponential
  backoff — plain exponential still synchronises the fleet, which is what
  produces the thundering herd.
- Spool replay is **rate-limited and interleaved with live data**, so a
  recovering agent does not starve its own current observations while it
  catches up on history.

---

## 3. Security model

### The central property: the agent has no inbound surface

The Atlas agent listens on no network port. It makes outbound HTTPS
connections and nothing else. There is no control channel, and the control
plane cannot ask an agent to do anything.

The consequence is worth stating plainly, because it is unusual and it is the
strongest security property in the design:

> **Compromising the Atlas control plane gives an attacker no ability to
> execute anything on a monitored host.**

This is deliberately unlike Datadog, Elastic Agent, and every configuration
management tool. Those have remote-configuration or remote-execution paths, and
each is a fleet-wide remote code execution primitive waiting for a control
plane breach. Atlas trades that convenience away.

This is "Observe Everything, Control Nothing" as an *architectural* property
rather than a policy one. A policy can be changed by a future contributor who
does not know why it existed. An architecture with no command path cannot grow
one by accident.

**What this costs us**, and future phases must accept it:

- No on-demand inventory refresh. Remote inventory is as fresh as the last
  push (§5).
- No remote configuration. Changing an agent's collection interval means
  changing config on the host, via whatever already manages that host.
- No agent self-update. This is a feature, not a limitation — an agent that
  can replace its own binary is exactly the RCE primitive described above.
  Upgrades belong to the package manager or orchestrator.

### Identity is bound to the credential, never to the payload

This is **C1**, the most serious defect the readiness review identified.

`metric.Repository.InsertBatch` writes `env.Origin.NodeID` straight into
storage. In-process that is safe — the value came from local configuration.
Over a network it is attacker-controlled input, and any agent holding a valid
certificate could submit observations attributed to *any other node*: forge a
healthy CPU reading for a failing host, hide a disk filling up, poison another
team's capacity plan.

The fix is structural, not a check:

```go
// package ingest
//
// Verified wraps an envelope whose origin has been confirmed against the
// authenticated peer. Its fields are unexported and it can only be built by
// authenticate(), so there is no path from a request body to storage that
// skips verification.
type Verified struct {
    env transport.Envelope
}

func (v Verified) Envelope() transport.Envelope { return v.env }
```

The ingest handler extracts the node id from the **verified peer
certificate's URI SAN**, overwrites `Origin.NodeID`, and rejects any envelope
whose payload claimed a different one — logging the mismatch as a security
event, because a mismatch is what impersonation looks like.

The sink accepts `Verified`, not `Envelope`. The type system makes the unsafe
path unrepresentable rather than relying on every future contributor
remembering the rule.

### Certificates

- **Private CA**, generated on first control-plane start, key stored with
  `0600` and never served over the API.
- **Client certificates carry the node id in a URI SAN** (`atlas://node/<id>`),
  not the Common Name — CN for identity is deprecated and ambiguous.
- **24-hour lifetime, auto-renewed at 50%.** Short expiry *is* the revocation
  mechanism (M4): a compromised host loses access within a day with no
  infrastructure, and there is no CRL or OCSP responder to operate.
- **A denylist by node id, checked at handshake**, for ejection that cannot
  wait a day.
- **TLS 1.3 only.** Both ends are ours; there is no legacy client to
  accommodate.

### Ingest correctness as a security property

Two of the readiness review's critical items are correctness bugs that become
security-adjacent over a network:

**C2 — idempotency.** Any networked transport retries. A retry after a timeout
where the write actually succeeded duplicates every sample. Duplicated samples
corrupt exactly the aggregates operators trust, and a continuous aggregate
materialises the wrong value *permanently* — it is not recomputed once the
bucket closes. Fix: an `ingested_envelopes(envelope_id PK, node_id,
received_at)` table written in the same transaction as the samples. A conflict
means "already applied": commit nothing, return success. Retention matched to
the retry horizon (24h), and envelopes older than the window are rejected
because they can no longer be checked.

**H6 — clock skew.** Samples are timestamped by the agent. A host with a broken
clock writes samples hours in the future or past; future-dated samples break
`time_bucket` aggregation, past-dated ones land in already-materialised
aggregate buckets that will never be recomputed. One bad NTP configuration
silently corrupts a node's history. Fix: the ingest handler compares `SentAt`
with server time, records per-node skew, and rejects outside ±5 minutes.
`clock_skew_seconds` becomes a node field — most fleets have a few bad clocks
and nobody knows which.

### Agent privileges

The agent runs as a dedicated unprivileged `atlas` user, not root. What each
collector actually needs:

| Capability | Buys | Without it |
| --- | --- | --- |
| (none) | `/proc`, CPU, memory, disk, network, load | — |
| `atlas` in `docker` group | Container inventory and stats | Docker reported absent |
| (none) | `systemctl show` — unit state and dependencies | — |
| `CAP_NET_ADMIN`/`CAP_SYS_PTRACE` | Socket→PID attribution for ports | Sockets listed as `unattributed` |

Degradation is honest and already modelled: the Ports page distinguishes an
unattributed socket from an unowned one, and the plugin system reports a
capability as absent rather than reporting emptiness.

**Environment variables are never collected, from processes or containers.**
This is a standing project constraint and it is restated here because the agent
is the component that has the access. It is enforced in the collectors, and the
agent adds no new path to them.

### Audit

Enrollment, renewal, revocation, authentication failure, and node-id mismatch
are written to a structured audit stream separate from application logs (M5).
These are the events an incident review will need and they must not be
interleaved with debug output.

---

## 4. Enrollment

### The problem enrollment actually solves

A host that has never spoken to the control plane must obtain a credential.
Whatever proves it deserves one is the root of trust for the whole fleet, and
it is the design's weakest point.

Options considered:

| Approach | Verdict |
| --- | --- |
| Long-lived shared secret in config | Rejected. One leak enrolls anything, forever, with no revocation and no attribution. |
| Single-use token per host | Best security, but requires an operator action per host. Unworkable for autoscaling. |
| **Bounded multi-use token** | **Chosen.** TTL + use count + CIDR + environment binding. What kubeadm and Nomad do. |
| Platform attestation (cloud IID, TPM) | Strongest, and cloud-specific. The design leaves room; not Phase 1. |

### The token

```
atlas-server enroll-token create \
  --ttl 1h --uses 50 --cidr 10.0.0.0/8 --environment production
```

Printed **once**, stored only as a hash. Every constraint bounds the blast
radius of a leak: it expires, it exhausts, it only works from the right
network, and it can only enroll nodes into the environment it names.

### The flow

1. Operator creates a token; provisioning delivers it to the host
   (cloud-init, Ansible, Kubernetes secret) along with the CA bundle.
2. Agent starts, finds no certificate in its state directory.
3. Agent resolves its node id via the existing `hostid` package — the same
   HMAC-of-machine-id derivation the server already uses, so identity survives
   hostname changes and reboots.
4. Agent generates a keypair **locally**. The private key never leaves the
   host and is never transmitted.
5. Agent posts a CSR + token + node id + hostname + version, over TLS,
   verifying the server against the pinned CA bundle.
6. Server validates: token hash exists, unexpired, uses remaining, source IP
   within CIDR, **and the node id is not already actively enrolled**.
7. Server signs a 24-hour client certificate with the node id in a URI SAN,
   decrements the token, writes the `nodes` and `node_credentials` rows, and
   emits an audit event.
8. Agent persists cert and key at `0600` and begins collecting.
9. At 50% of lifetime the agent renews over mTLS at `/agent/renew` — **no
   token involved**. This is why 24-hour certificates are painless: the token
   is used once in a host's life.

### The rule that prevents impersonation

Step 6's last clause is the one that matters. Without it, anyone holding a
valid token could enroll claiming an existing node's id and take over its
identity — the C1 problem moved from ingest to enrollment.

**Enrollment for an already-enrolled node id is refused** unless the existing
certificate has expired, has been explicitly revoked, or the token carries an
explicit re-enroll grant. Every refusal is audit-logged as a security event.

Re-imaging is not affected: a re-imaged host has a new machine id and is
therefore a genuinely new node. A rebooted host keeps its certificate on disk
and never re-enrolls.

### Honest limitation

The control plane cannot verify that a host is who it claims to be. The token
is the only proof, and a stolen token within its TTL, use count, and CIDR is
sufficient to enroll. This is inherent to bootstrapping without platform
attestation. It is why the bounds exist, why enrollment is audited, and why
attestation is the natural upgrade.

### Failure handling

An agent whose certificate expired while the host was off cannot renew (mTLS
fails). It falls back to enrollment if a token is still configured. If it
cannot authenticate at all it **keeps running, keeps collecting, keeps
spooling, retries with backoff, and logs at error level on a rate limit**. It
does not exit — a monitoring agent that silently stops is worse than one that
complains — and the bounded spool means it cannot fill the disk while trying.

---

## 5. How remote inventory satisfies the Scope seam

### What Phase 0 built

Phase 0 introduced `inventory.Scope` and threaded it through every inventory
method, with this preflight:

```go
scope := h.scopeFrom(r)
if !scope.IsLocal(h.deps.Collection.Identity().NodeID) {
    return scope, inventory.ErrRemoteUnavailable(op, scope.NodeID, subject)
}
if err := h.requirePlugin(pluginID); err != nil { ... }
```

Every remote scope returns `unavailable` with the reason "remote inventory
requires an agent on that host". Phase 1 is the phase that makes that sentence
stop being true.

### What changes

**No handler signature changes.** That is the payoff, and it is worth being
precise about why: the six `CollectionSource` methods already accept a `Scope`,
every response already echoes `node_id`, and the frontend already round-trips
`?node=`. Phase 1 replaces the *implementation* behind the seam.

`CollectionSource` gains a resolver with two implementations:

- **local** — the existing live plugin call, unchanged.
- **remote** — the latest pushed snapshot, read from `internal/storage/inventory`.

The preflight becomes: resolve scope → if local, require plugin → if remote,
require a snapshot.

### The honesty problem, and the type that solves it

Local inventory is **live**: the process list is what is running this
millisecond. Remote inventory is a **snapshot**, up to a push interval old.

Presenting a 47-second-old container list as current is exactly the class of
lie this project has spent every milestone eliminating — the same family as
Docker's frozen healthcheck, the fabricated fleet summary, and "0 tiers" on a
failed query. It cannot be left to each call site to remember.

So freshness is carried in the type:

```go
// Snapshot wraps inventory with the provenance a reader needs to know how
// much to trust it.
type Snapshot[T any] struct {
    Data       T
    NodeID     string
    ObservedAt time.Time // when the host was actually observed
    Live       bool      // true only when read directly from the host
}
```

Every inventory response gains `observed_at` and `live`. Local responses set
`live: true`. There is no way to return inventory without stating its age,
because there is no unwrapped value to return.

### Three distinct outcomes, three distinct answers

A fleet has states a single host does not, and collapsing them would recreate
the bug Phase 0 caught:

| Situation | Response |
| --- | --- |
| Node is local | `200`, `live: true` |
| Node has an agent that pushed this subject | `200`, `live: false`, `observed_at` set |
| Node has an agent, but no Docker on that host | `501 not_implemented`, scoped to *that* node |
| Node is known but has never reported | `503 unavailable`, reason: no agent |
| Node id is unknown | `404 not_found` |

The distinction between rows three and four is the one Phase 0's precedence
bug got wrong in the other direction — answering "no systemd on this host"
about a *remote* node using local facts. The typed error kernel already
carries the `reason` detail that separates them, so this needs no API change.

### Push cadence

Inventory pushes are per-subject and slower than metrics — 60 seconds by
default, against 15 for metrics. Subjects whose content hash is unchanged send
the hash alone rather than the payload, which for containers, services, ports
and mounts skips most pushes on a stable host. Process lists churn constantly
and will rarely benefit; that is expected and acceptable at 60-second cadence.

---

## 6. Scalability

Target: **hundreds of nodes comfortably, low thousands without architectural
change.**

### Sizing at 1,000 nodes

| Path | Volume | Assessment |
| --- | --- | --- |
| Metric requests | 28k envelopes/min, batched ×10 → **47 req/s** | Trivial for Go |
| Samples | ~200/node/interval × 4/min → **13k rows/s** | Comfortable for `COPY` into a hypertable |
| Inventory pushes | 6 subjects × 1,000 / 60s → **100/s** worst case | Content-hash skip removes most |
| Inventory storage | 6,000 rows, replaced in place | Negligible |
| Node liveness | See H1 below | **Was the real problem** |

### The two changes that actually matter

**H1 — the hot row.** Today every batch runs an `UPDATE` on the node's row. At
1,000 agents × 7 collectors × 4 runs/minute that is **28,000 UPDATEs per
minute on one small table**. Each produces a new row version, WAL, and index
churn; autovacuum will not keep up and rows will contend. This arrives well
before 1,000 nodes.

Fix: take `last_seen_at` off the sample path entirely. Liveness lives in memory
on the control plane and flushes every 15 seconds as one batched
`UPDATE ... FROM (VALUES ...)` — from 28,000 writes/minute to 4.

**H2 — transaction count.** One transaction per envelope means 7,000
transactions per interval; transaction overhead and connection-pool pressure
become the bottleneck long before disk does. Fix: agents batch envelopes per
request, and the control plane accumulates across agents for 100–250 ms into a
single `COPY`. Idempotency (C2) is keyed per envelope, so batching does not
weaken it.

### Keeping horizontal scaling open

Nothing in the ingest path holds per-agent state that cannot be reconstructed.
The liveness buffer is best-effort by construction; the batch accumulator is
bounded and flushed on shutdown. Several control planes therefore run behind a
plain load balancer with **no sticky sessions and no coordination**.

This is a constraint on the batching design, not a consequence of it, and it is
recorded here so a future optimisation that introduces per-agent affinity is
recognised as the significant decision it would be.

### Deferred, with reasons

- **M9 — continuous aggregate refresh at fleet scale.** The refresh policies
  were sized for one node. Load-test before claiming 1,000; be prepared to
  widen `schedule_interval` or partition by node group. Cannot be sized
  honestly without a fleet to measure, which this milestone produces.
- **M2 hot reload, M3 staged rollout** — operability, not capability.
- **M8 pprof** — cheap, but no evidence it is needed yet.

`GOMEMLIMIT` from configuration is included, not deferred (part of M6). An
agent that OOMs the host it monitors is worse than no agent, and it is a
one-line change.

### Readiness review status after this milestone

| Item | Status |
| --- | --- |
| C1 trusted node id | **Fixed here** — type-enforced |
| C2 non-idempotent ingest | **Fixed here** |
| C3 no local spool | **Fixed here** |
| C4 cardinality | Already done (`scheduler/cardinality.go`) |
| H1 liveness hot row | **Fixed here** |
| H2 transaction count | **Fixed here** |
| H3 backpressure / herd | **Fixed here** |
| H4 streaming collectors | Already done (`scheduler/stream.go`) |
| H5 plugin config | Already done (`plugin.Env.Config`) |
| H6 clock skew | **Fixed here** |
| M1 version negotiation | **Fixed here** |
| M4 revocation | **Fixed here** — short certs + denylist |
| M5 audit logging | **Partial** — enrollment and auth events only |
| M6 self-limits | **Partial** — `GOMEMLIMIT` only |
| M7 agent telemetry | **Partial** — spool depth, lag, drops on heartbeat |
| M2, M3, M8, M9 | Deferred |

---

## 7. Verification plan

A design for a distributed system verified on one node is not verified.

**The fleet:** the macOS host as control plane, plus three to four Linux
containers running `atlas-agent` only, with *deliberately different* inventory
— different container sets, different systemd units, different listening
ports. Identical nodes would not catch a cross-node attribution bug, which is
the exact failure this milestone risks.

**Negative tests, each pinned by an automated test:**

- Agent A submits an envelope claiming node B's id → rejected, audit-logged.
- The same envelope delivered twice → one set of samples, second call succeeds.
- Control plane stopped for 60s → agent spools → no gap after recovery.
- Expired token, exhausted token, token from outside its CIDR → all refused.
- Re-enrollment of a node with a live certificate → refused.
- Clock skewed by an hour → samples rejected, skew surfaced on the node.
- Remote inventory returns the **agent's** containers, not the control
  plane's. This is the precise bug Phase 0 caught in a pre-scoping binary, and
  it is the single most important assertion in the suite.

---

## 8. The minimal validation UI

Per the backend-first strategy: only what is required to prove distributed
functionality works. No polish.

- A **node switcher** in the header, listing `/api/v1/nodes`, setting `?node=`
  on every inventory query.
- A **freshness indicator** driven by the new `observed_at` / `live` fields:
  "live" or "as of 47s ago".

No new pages. The existing pages already accept a node id and need no changes.

One existing behaviour must change: `usePrimaryNodeID` currently resolves the
control plane's own node. In a fleet, "primary node" stops being a meaningful
concept, and the switcher's selection replaces it.

---

## 9. Decisions that constrain later phases

1. **No control channel, ever.** Phase 2+ cannot add on-demand checks,
   remote configuration, or remote diagnostics. This is the cost of the
   security property in §3 and it is accepted deliberately.
2. **Remote inventory is a snapshot with disclosed freshness.** Every
   consumer, forever, handles `observed_at`. Phase 3's incident timeline
   inherits this correctly rather than assuming liveness.
3. **HTTPS/JSON, not gRPC.** Supersedes the hint in ADR-0005.
4. **Certificates authenticate machines only.** Human authentication (Phase 6)
   must use a separate credential type and a separate middleware path. A
   node certificate must never grant a UI session, and a user session must
   never permit ingest.
5. **The control plane holds no per-agent state.** Horizontal scaling stays
   available; any future per-agent affinity is a decision, not an
   optimisation.
