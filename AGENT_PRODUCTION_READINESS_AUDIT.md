# Atlas Agent — Production-Readiness Implementation Audit

Read-only investigation. No files modified. This is a second, deeper pass building on `AGENT_DATA_AUDIT.md` (data collection/transmission trace) — this document adds production-readiness, upgrade-safety, transport-mechanics, and security-posture findings, then closes with a prioritized gap list, a target spec, and an implementation plan.

Method: 9 independent full-file source-reading passes across two audit rounds, plus direct re-verification of every claim marked **[VERIFIED]** below. One factual conflict between two sub-investigations was found and resolved by direct source read — noted where it occurs (§5, §Part 8 P0-1).

---

## PART 1 — Complete data collection matrix

Legend: `●` collected/present · `○` not collected/absent · `◐` derived/partial · `Snap` = ClassSnapshot (inventory) · `Stream` = ClassStream (metrics/events).

### HOST

| Field | Collected | Source (file:function) | Sent | Transport | Persisted | API | UI | Interval | Class | Retry | Risk |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Hostname | ● | system/gopsutil.go:42 `host.InfoWithContext` | ● | HTTPS/mTLS or libp2p | `nodes` table (upsert) | ● | ● | on Origin, every send | n/a (Origin field) | n/a | none |
| Machine/node ID | ● (hashed) | hostid.go:170-175,251-254 HMAC-SHA256 of `/etc/machine-id` | ● | as above | `nodes.id` | ● | ● | fixed at start | n/a | n/a | none — raw machine-id never leaves the hash function |
| OS / kernel family | ● | system/gopsutil.go:43 | ● | as above | `nodes` | ● | ● | with host facts push | Snap-like (node facts) | n/a | none |
| Distribution | ● | gopsutil.go:44 | ● | " | `nodes` | ● | ● | " | " | n/a | none |
| OS version | ● | gopsutil.go:45 | ● | " | `nodes` | ● | ● | " | " | n/a | none |
| Kernel version | ● | gopsutil.go:46 | ● | " | `nodes` | ● | ● | " | " | n/a | none |
| Architecture | ● | gopsutil.go:47 | ● | " | `nodes` | ● | ● | " | " | n/a | none |
| CPU model string | ○ | not found — no `cpu.InfoWithContext` call | — | — | — | — | — | — | — | — | — |
| CPU topology (sockets/cores/threads) | ○ | not found | — | — | — | — | — | — | — | — | — |
| Logical/physical cores | ● | gopsutil.go:55-60 `cpu.CountsWithContext` | ● | metrics batch | `metric_samples` | ● | ● | 15s | Stream | spooled+retried | none |
| CPU utilization (aggregate + per-core) | ● | gopsutil.go:71-75 | ● | " | " | ● | ● | 15s | Stream | spooled+retried | none |
| CPU state times | ● | gopsutil.go:81-94 | ● | " | " | ◐ | ◐ | 15s | Stream | spooled+retried | none |
| Load average 1/5/15 | ● | gopsutil.go:242-246 | ● | " | " | ● | ● | 15s | Stream | spooled+retried | none |
| RAM total/used/free/available/cached/buffers | ● | gopsutil.go:105-118 | ● | " | " | ● | ● | 15s | Stream | spooled+retried | none |
| Swap total/used/free/Sin/Sout | ● | gopsutil.go:128-131 | ● | " | " | ◐ | ◐ | 15s | Stream | spooled+retried | none |
| Boot time | ● | gopsutil.go:48 | ● | Origin/host facts | `nodes` | ● | ● | with host facts | n/a | n/a | none |
| Uptime | ◐ derived | provider.go:91-96 `time.Since(BootTime)` | ◐ | derivable client-side from boot_time | — | ◐ | ◐ | — | — | — | none |
| Timezone | ○ | not found anywhere | — | — | — | — | — | — | — | — | — |
| FQDN | ○ | only short `Hostname` collected, no FQDN/domain resolution | — | — | — | — | — | — | — | — | — |
| Manufacturer / motherboard / product model | ○ | no DMI/`sys_vendor`/`product_name` read anywhere | — | — | — | — | — | — | — | — | — |
| BIOS/firmware version | ○ | not found | — | — | — | — | — | — | — | — | — |
| Virtualization/hypervisor detection | ○ | not found — no `/sys/hypervisor`, `systemd-detect-virt`, or DMI-based check | — | — | — | — | — | — | — | — | — |

### NETWORK

| Field | Collected | Source | Sent | Transport | Persisted | API | UI | Interval | Class | Retry | Risk |
|---|---|---|---|---|---|---|---|---|---|---|---|
| Interface name | ● | system/gopsutil.go:217-237 `net.IOCountersWithContext` | ● | metrics | `metric_samples` | ● | ● | 15s | Stream | spooled | none |
| Interface state (up/down) | ○ | zero-traffic interfaces merely skipped (collector_network.go:70), not a real state field | — | — | — | — | — | — | — | — | — |
| IPv4 / IPv6 (host NIC) | ○ | no `net.Interfaces()`/`InterfaceAddrs` call anywhere | — | — | — | — | — | — | — | — | — |
| Private / public IPv4 | ○ | not found repo-wide | — | — | — | — | — | — | — | — | — |
| CIDR/prefix | ○ | not found | — | — | — | — | — | — | — | — | — |
| MAC address (host NIC) | ○ | only exists for Docker *container* networks (docker/client.go:212), not host NICs | — | — | — | — | — | — | — | — | — | — |
| MTU | ○ | not found | — | — | — | — | — | — | — | — | — |
| Gateway / default route | ○ | not found for host (exists only for Docker container network, docker/client.go:211) | — | — | — | — | — | — | — | — | — |
| Routing table | ○ | not found | — | — | — | — | — | — | — | — | — |
| DNS servers / search domains | ○ | every `DNS` hit in repo is a TLS cert SAN or Atlas's own cert-gen, none is resolver collection | — | — | — | — | — | — | — | — | — |
| Link speed | ○ | not found | — | — | — | — | — | — | — | — | — |
| RX/TX bytes, packets, errors, drops | ● | provider.go:183-193; rates via rate.go | ● | metrics | `metric_samples` | ● | ● | 15s | Stream | spooled | none |
| Connectivity/reachability | ○ | no ping/reachability probe anywhere | — | — | — | — | — | — | — | — | — |
| DNS resolution health | ○ | not found | — | — | — | — | — | — | — | — | — |
| Listening TCP ports + bind address | ● | ports/gopsutil.go | ● | inventory | `inventory_snapshots` subject `ports` | ● | ● | 60s | Snap, hash-deduped | dropped-not-spooled on fail | none |
| Listening UDP ports + bind address | ● | ports/gopsutil.go:28 | ● | " | " | ● | ● | 60s | Snap | dropped-not-spooled | none |
| Owning process (port) | ● | ports/gopsutil.go:69-70 via `gopsproc` | ● | " | " | ● | ● | 60s | Snap | " | none |
| TLS cert (subject/issuer/SAN/validity/self-signed) | ● | ports/tls.go:55-86, live handshake, `InsecureSkipVerify:true` for the probe itself (legitimate — reading the cert, not trusting it) | ● | " | " | ● | ● | 60s | Snap | " | none |
| Cert expiry | ● | tls.go `NotAfter` | ● | " | " | ● | ● | 60s | Snap | " | none |

### STORAGE

| Field | Collected | Source | Sent | Transport | Persisted | API | UI | Interval | Class | Retry |
|---|---|---|---|---|---|---|---|---|---|---|
| Partitions/mounts/fstype | ● | system/gopsutil.go:137-150 `disk.PartitionsWithContext` | ● | inventory | `inventory_snapshots` subject `mounts` | ● | ● | 60s | Snap | dropped-not-spooled |
| Total/free/used, used% | ● | gopsutil.go:177-191 `disk.UsageWithContext` | ● | inventory + metrics | both | ● | ● | 15s(metric)/60s(inv) | both | spooled(metric)/drop(inv) |
| Inode total/used/used% | ● | gopsutil.go:177-191 | ● | metrics | `metric_samples` | ◐ | ◐ | 15s | Stream | spooled |
| Read/write bytes, ops | ● | gopsutil.go:197 `disk.IOCountersWithContext` | ● | metrics | `metric_samples` | ● | ● | 15s | Stream | spooled |
| IOPS | ◐ derived | rate.go, rate of ops counters | ◐ | metrics | " | ◐ | ◐ | 15s | Stream | spooled |
| Latency | ○ | not found — gopsutil `IOCounters` has no per-op latency, none computed | — | — | — | — | — | — | — |
| SMART / disk health | ○ | not found — would require `smartctl` shellout, not present | — | — | — | — | — | — | — |
| Filesystem health (e.g. fsck status) | ○ | not found | — | — | — | — | — | — | — |

### PROCESSES

| Field | Collected | Source | Sent | Transport | Persisted | API | UI | Interval | Class | Retry | Risk |
|---|---|---|---|---|---|---|---|---|---|---|---|
| PID/PPID | ● | process/gopsutil.go:65-69 | ● | inventory | `inventory_snapshots` subject `processes` | ● | ● | 60s | Snap | drop-on-fail | none |
| Name/executable | ● | gopsutil.go:59,70-72 | ● | " | " | ● | ● | 60s | Snap | " | none |
| Command line | ● | gopsutil.go:73-78, truncated 512B, **unredacted** | ● | " | " | ● | ● | 60s | Snap | " | **can carry secrets — see §Part 2** |
| User | ● | gopsutil.go:79-81 | ● | " | " | ● | ● | 60s | Snap | " | none |
| CPU%/mem RSS/mem% | ● | gopsutil.go:85-93 | ● | inventory + top-N metrics | both | ● | ● | 15s/60s | both | " | none |
| State | ● | gopsutil.go:82-84 | ● | inventory | " | ● | ● | 60s | Snap | " | none |
| Threads | ● | gopsutil.go:94-96 | ● | " | " | ● | ● | 60s | Snap | " | none |
| Start time | ● | gopsutil.go:97-99 | ● | " | " | ● | ● | 60s | Snap | " | none |
| Open files | ○ | no `OpenFilesWithContext` call | — | — | — | — | — | — | — | — |
| Sockets/connections | ○ | no `ConnectionsWithContext` call in process package (ports plugin covers listening sockets only, not per-process connection lists) | — | — | — | — | — | — | — | — |
| Top CPU / top memory | ● | plugin.go:28-33,233-306, aggregated by name, top-10 default | ● | metrics | `metric_samples` `process.top.cpu/.memory` | ● | ● | 15s | Stream | spooled | none |
| Restart/crash information | ○ | not found — no OOM-kill detection, no exit-code/crash-loop tracking for arbitrary processes (systemd-managed services DO get restart count, §5 below) | — | — | — | — | — | — | — | — |

### SERVICES (systemd)

| Field | Collected | Source | Sent | Persisted | API | UI | Interval | Class | Notes |
|---|---|---|---|---|---|---|---|---|---|
| Name/description | ● | service/systemd.go:77-78,117-123 `list-units` | ● | `inventory_snapshots` `services` | ● | ● | 60s | Snap | |
| Active/sub/load state | ● | systemd.go:118-120 | ● | " | ● | ● | 60s | Snap | |
| Enabled state | ● | systemd.go:160-163,208 `show --property=UnitFileState` | ● | " | ● | ● | 60s | Snap | |
| Failed state | ◐ derived | provider.go:96 | ◐ | " | ◐ | ◐ | — | — | derived from ActiveState |
| Restart count | ● | systemd.go:197-199 `NRestarts` | ● | " | ● | ● | 60s | Snap | |
| Uptime | ◐ derived | provider.go:106-112 | ◐ | — | ◐ | ◐ | — | — | |
| Memory usage (cgroup) | ● | systemd.go:202-204 `MemoryCurrent` | ● | `metric_samples` `service.memory` | ● | ● | 15s | Stream | |
| CPU usage (cgroup) | ● parsed, **○ never emitted** | systemd.go:205-207 `CPUUsageNSec` → `Unit.CPUSeconds` | ○ | — | — | — | — | — | **collected but dropped before the metrics emitter — plugin.go:318-346 never references it** |
| Dependencies / dependency graph | ● real, actively used | systemd.go:243-293,393-426; graph.go:131-175,422-552 — full traversable graph, `Impact()`/`Propagate()` implemented and exercised | ◐ (graph served via API, not pushed as inventory subject) | in-memory cache (5 min TTL, plugin.go:166-233), not persisted server-side | ● `Plugin.Graph(ctx)` | ● DependencyGraph.tsx | live, 5min cache | n/a | not part of the push pipeline — pulled on demand |
| Service logs (journald) | ○ | not found — no `journalctl`/journald read anywhere in `internal/plugin/service` | — | — | — | — | — | — | |

### CRON

| Field | Collected | Source | Sent | Persisted | API | UI | Interval | Class | Risk |
|---|---|---|---|---|---|---|---|---|---|
| System crontab / cron.d / user spool / periodic | ● | cron.go:79-92,147-189 | ● | `inventory_snapshots` `cron_jobs` | ● | ● | 60s | Snap | none |
| Schedule | ● | cron.go:279-286,309 | ● | " | ● | ● | 60s | Snap | none |
| Command | ● **unredacted**, 300B cap | cron.go:96,309-324 | ● | " | ● | ● | 60s | Snap | **can carry secrets — see §Part 2** |
| Owner/user | ● | cron.go:168,184,268-311 | ● | " | ● | ● | 60s | Snap | none |
| Execution status (last run, exit code) | ○ | not found — Atlas reads static crontab files only, never correlates with cron's own execution/log history | — | — | — | — | — | — |

### DOCKER / CONTAINERS

| Field | Collected | Source | Sent | Persisted | API | UI | Notes |
|---|---|---|---|---|---|---|---|
| Daemon version / API version | ● | docker/client.go:63-64; engine.go:57-60 | ● | `inventory_snapshots` `containers` | ● | ● | |
| ID/name/image/image ID | ● | client.go:106-112; engine.go:132-140 | ● | " | ● | ● | |
| Status/health | ● | client.go:114-115; engine.go:144-151 | ● | " | ● | ● | |
| Uptime | ◐ derived from StartedAt | collectors.go:96-99 | ◐ | — | ◐ | ◐ | |
| CPU/memory usage | ● | client.go:243-247; engine.go:312-341 | ● | `metric_samples` | ● | ● | |
| Network RX/TX | ● | client.go:251-252; engine.go:347-350 | ● | `metric_samples` | ● | ● | |
| Restart count | ● | client.go:126; engine.go:153 | ● | `inventory_snapshots` | ● | ● | |
| Ports/exposed ports | ● | client.go:164; engine.go:252-277 | ● | " | ● | ● | |
| Mounts | ● | client.go:162; engine.go:224-234 | ● | " | ● | ● | |
| Volumes | ● (count/size, not per-mount) | client.go:260-265; engine.go:389-404 | ● | " | ● | ● | |
| Networks — name/IP/aliases | ● | client.go:163; engine.go:236-250 | ● | " | ● | ● rendered | |
| Networks — MAC/gateway | ● collected | same | ● | " | ● in API response | **○ never rendered** | fetched into `web/src/api/types.ts:314-321`, no component reads it |
| Events | ● streamed | client.go:268-279; engine.go:406-457 | ● | events pipeline | ◐ | ◐ | separate from inventory |
| Logs | ● **on-demand only**, never routine | AgentOps protocol, §Part 3 | ● (live view only) | ○ never stored | live WS only | live view | 6h session cap, dual-gated |
| Environment variables | ○ **deliberately, permanently excluded** | client.go:136-141, `Config.Env` never dereferenced | — | — | — | — | policy, not a gap — see §Part 2 |
| Labels | ● full set; curated subset becomes metric label | client.go:132; engine.go:138; collectors.go:24-33 | ● | " | ● | ● | |
| Command/entrypoint | ● | client.go:154-155; engine.go:203-204 | ● | " | ● | ● | |
| Resource limits (CPU/mem limit, cpuset) | ○ | not found — `HostConfig.Resources` (NanoCPUs, Memory limit, CPUShares) never read | — | — | — | — | |
| Resource reservations | ○ | not found | — | — | — | — | |

### SECURITY (agent's own identity/posture, and host security surfaces)

| Field | Collected | Source | Sent | Notes |
|---|---|---|---|---|
| Agent identity (NodeID) | ● | hostid.go | ● | Origin.NodeID on every envelope |
| Agent client cert | ● (local) | pki/store.go | ○ never transmitted (used to authenticate, not reported) | correct — a cert isn't telemetry |
| Cert expiry (not_after) | ● (local log only) | credentials.go:264 | ○ | **gap — see Part 8** |
| CA/trust state (pinned CA) | ● (local) | credentials.go:151-165 | ○ | |
| Enrollment status | ● (local log only) | credentials.go | ○ | **gap** |
| Renewal status | ● (local log only) | credentials.go:376 | ○ | **gap** |
| Authorization (AgentOps grants) | ● control-plane side (`GrantStore`) | internal/core/fleet/grants.go | n/a — server-side state, not agent-collected | |
| Linux capabilities (agent's own) | ● enforced, not "collected" | packaging/atlas-agent/atlas-agent.service:60-61 — empty `CapabilityBoundingSet`/`AmbientCapabilities` | n/a | agent runs unprivileged by design, doesn't report this fact anywhere |
| Firewall status (iptables/nftables/ufw/firewalld) | ○ | grep confirms zero references repo-wide | — | not collected at all |
| SELinux/AppArmor status | ○ | only a `selinuxfs` string exists, in the disk-mount exclusion list (gopsutil.go:141) — not a status read | — | not collected at all |
| Suspicious exposure info (e.g. world-writable configs, exposed ports w/o TLS) | ○ | no such analysis exists; ports plugin reports raw facts (bind address, TLS presence) but does not classify exposure | — | classification logic doesn't exist, raw facts do |

### AGENT SELF-HEALTH

| Field | Collected | Sent | Evidence |
|---|---|---|---|
| Connected/disconnected | ○ | ○ | server infers liveness only from telemetry arrival timing (internal/app/health.go:73-82) |
| Current relationship / status | ● (local) | ○ | in-memory `relationshipRuntime`, never reported |
| Peer ID (libp2p) | ● (local log only) | ○ | agent.go:306-308,318-320 |
| Transport in use | ● (local) | ○ | not on Origin/Envelope |
| Relay status | ● (local, implicit via dial success/fail) | ○ | not reported |
| Direct-connection status | ● (local) | ○ | not reported |
| Cert expiry | ● (local log) | ○ | credentials.go:264 |
| Last successful/failed delivery | ○ not tracked as a reportable field | ○ | `remote.Transport.Stats()` exists (Sent/Failed/Rejected/Spooled) but **zero callers in internal/agent** |
| Delivery/failure/retry counts | ● in-process only, unread | ○ | same `Stats()` struct, dead from the agent's own perspective |
| Spool depth / disk usage | ● in-process | ○ | `spool.Spool.Dropped()` exists, uncalled |
| Dropped envelopes | ● in-process (spool eviction) | ○ | spool.go:193-199 oldest-first eviction, count not surfaced |
| Inventory/telemetry delivery state | ○ | ○ | no explicit state machine reported; server only sees "did a POST arrive" |
| Agent uptime | ○ | ○ | not computed/sent (Origin has no start-time field) |
| Agent version | ● | ● | `Origin.AgentVersion`, populated from `build.Current().Version` — agent.go:195,214; confirmed round-trips to `internal/api/v1/nodes.go:27,101` |
| Build commit / build time | ● (binary-embedded, if built with ldflags) | ○ | `internal/platform/build/build.go:23-30` has Commit/BuildTime fields but only `Version` is ever put on `Origin` — commit/build-time never transmitted |
| Configuration health (e.g. "3 of 5 collectors degraded") | ○ | ○ | no aggregate self-diagnostic exists; each collector's `Available()`/`Detect()` is internal, not surfaced |

---

## PART 2 — Information that should NOT be collected

| Field | Currently collected? | Classification | Reasoning |
|---|---|---|---|
| Container environment variables | ○ never read | **1. Deliberate security boundary** | `docker/client.go:136-141`: explicit, permanent policy. `Config.Env` is never dereferenced anywhere in the codebase. |
| Process environment variables (host) | ○ never read | **1. Deliberate security boundary** | Same stated policy, referenced explicitly: "The same decision governs process environments elsewhere in Atlas" (docker/client.go:140). No `EnvironWithContext` call anywhere in `internal/plugin/process`. |
| Process command-line arguments | ● collected, unredacted | **4. Dangerous, but currently included — should be constrained, not excluded outright** | Command lines are operationally essential (identifying *what* is running), but captured verbatim with only a length cap, no pattern-based redaction. Documented as a known trade-off (provider.go:64-66), not a bug. For production hardening this should move toward pattern-based redaction (`--password=`, `-p `, `Authorization:`, `Bearer `, `token=` etc.) rather than blanket exclusion — full exclusion would remove real diagnostic value (e.g. seeing *which* Java process, with which `-Xmx` flags). |
| Cron command strings | ● collected, unredacted, 300B cap | **4. Dangerous, same reasoning as above** | Same trade-off as process Cmdline; codebase is self-aware (cron.go:54-56) and already keeps command strings out of metric labels (cardinality/security reason combined) — the residual risk is the inventory API surface, not metrics. |
| Container logs | ● transmitted, on-demand only | **1. Deliberate, tightly-scoped security boundary** | Not a background pipeline: dual-gated (agent opt-out + control-plane grant), 6h session cap, never stored server-side. Appropriate design for a read-only observability tool that still needs a "show me what this container is saying right now" capability. |
| Private keys (agent's own TLS key) | ○ never transmitted | **1. Deliberate security boundary** | Used locally to authenticate outbound connections; never serialized into any payload. No collector touches it. |
| Application log file contents (arbitrary files under `/var/log`, app-specific logs) | ○ not collected at all | **2. Unnecessary data / correctly out of scope** | Atlas's stated mandate ("Observe Everything. Control Nothing.") is infrastructure observability, not log aggregation; arbitrary log-file reading would be a categorically different (and much riskier) product surface. Correctly absent. |
| Sensitive filesystem contents (config files, `.env` files, secrets on disk) | ○ not collected | **1. Deliberate security boundary (by omission — no collector reads arbitrary file contents)** | No collector in the repo reads file *contents* outside of narrowly-scoped, structurally-known files (crontabs, `/etc/machine-id`, `/proc` pseudo-files). This is the correct posture. |
| Firewall rule state (iptables/nftables) | ○ not collected | **3. Currently missing, arguably should be added — but low priority** | Useful for "why is this port unreachable" diagnosis, but reading firewall rule *content* (not just enabled/disabled) risks exposing security posture details to anyone with API read access; if added, should be limited to enabled/disabled + rule *count*, not full rule dump. |
| SELinux/AppArmor enforcement mode | ○ not collected | **3. Currently missing, should be added — genuinely low-risk, high-value** | Enforcing/permissive/disabled is a single enum with no sensitive content; directly useful for diagnosing "why did this process get denied access." Good candidate for Part 9/10. |
| Full route table dump | ○ not collected | **2/3 borderline** | Full table (all routes) has moderate value and low risk; default-route-only would cover most decision-making needs with less surface. |

---

## PART 3 — Transport architecture, traced end to end

### Common backbone
Every payload — telemetry, inventory, events — is wrapped in `transport.Envelope{ID, Origin, Payload, SentAt}` (`internal/core/transport/transport.go:75-93`) and travels the same wire path: agent-initiated **HTTPS POST** (or, if `ATLAS_AGENT_TRANSPORT=libp2p`, the same JSON body over a libp2p stream instead of TCP) to `/api/v1/agent/telemetry`, decoded server-side, identity-rebound to the mTLS peer cert (`internal/api/agent/handler.go:224`), then dispatched by `Router.Receive` on `Envelope.Kind()`.

### Telemetry (metrics)
```
system/process/service/cron/docker collectors (15s tick)
  → collect.Batch
  → transport.Payload{Kind: KindMetrics, Class: ClassStream}
  → Envelope{Origin, Payload, SentAt}
  → agent-side spool (disk-backed, atomic write, 512MB/24h caps)
  → remote.Transport: HTTPS POST /api/v1/agent/telemetry (batched, max 200/batch)
  → Handler.Telemetry → Router.Receive → metric.Sink.Receive
  → metric_samples (TimescaleDB hypertable, insert-append, 7-day chunks, 30-day retention)
  → chart components in web UI
```
Retry: on failure, envelope stays in the spool; `replayLoop` retries with capped exponential backoff (1s→2m, jittered) forever, honoring server `slow_down`/`pause` directives — **but the server never emits anything but `"ok"`, so this backpressure channel is currently a no-op** (verified: `Directive` is hardcoded `"ok"` at `internal/api/agent/handler.go:211,275`, no other code path sets it).

### Inventory
```
process/service/cron/ports/mounts/docker collectors (60s tick)
  → inventory.Payload{Subject, ObservedAt, ContentHash, Data} (ClassSnapshot)
  → SHA-256 content-hash check against inventoryPusher.lastHash — skip if unchanged
  → Envelope → same HTTPS/libp2p POST, same endpoint
  → sent immediately, NOT spooled — dropped on delivery failure
  → Handler.Telemetry → Router.Receive → inventory.Receiver.Receive
  → inventory_snapshots (plain Postgres table, upsert on (node_id, subject) — latest only)
  → inventory pages in web UI (ProcessesPage, ServicesPage, ContainersPage, etc.)
```
Retry: **none** for a failed POST — the subject is simply retried on its next natural 60s tick, or sooner if the underlying data changes and the hash differs. Under `ATLAS_AGENT_RELATIONSHIPS`, the hash-dedup cache is global across relationships (see Part 4), which can delay one relationship's re-delivery after a partial fanout failure.

### Events
```
eventbus (in-process pub/sub, bounded per-subscriber queue, drop+count on overflow)
  → eventForwarder picks events off the bus as they occur (not interval-based)
  → transport.Payload{Kind: KindEvents, Class: ClassStream}
  → same spool + retry path as telemetry
  → Router.Receive → eventstore.Receiver
  → events table (durable, per ARCHITECTURAL_CONSTRAINTS.md)
```

### AgentOps (container logs) — the one non-push, request/response channel
```
Operator clicks "view logs" in Atlas UI
  → internal/api/v1/containers.go ContainerLogsFollow (WebSocket)
  → internal/app/fleet.go fleetPipeline.ContainerLogs
  → GrantStore.IsGranted(nodeID, OperationContainerLogs)?  [control-plane gate]
  → libp2ptransport.RequestContainerLogs opens an AgentOps stream over libp2p
     (reversed mTLS: control plane is TLS client, agent is TLS server)
  → agent: handleAgentOpsStream checks AgentOpsContainerLogsDisabled  [agent-local gate]
  → if allowed: docker.Client.Logs() reads from Docker daemon
  → each line encoded as AgentOpFrame{Type:"line", Message:...} on the stream
  → decoded back into LogLine on the control-plane side
  → forwarded to the browser over the WebSocket
```
This is **exclusively** a libp2p-transport feature — there is no HTTPS equivalent; `container_logs` is the only operation the AgentOps protocol implements at all (verified — no exec/file-read/signal op exists, even partially). Session capped at 6h. Never stored server-side. Never part of routine telemetry.

### HTTPS + mTLS
Standard Go `net/http` client/server, `tls.VersionTLS13` minimum everywhere it's configured. **Listener-level `ClientAuth` is `VerifyClientCertIfGiven`, not `RequireAndVerifyClientCert`** (`internal/platform/pki/tls.go:101-119`) — deliberate, because `/enroll` has no cert yet; every other handler (`Renew`, `Telemetry`, `Heartbeat`) independently checks for a verified peer cert via `peerCert`/`peerNodeID` (`internal/api/agent/handler.go:295-309`) and rejects with `CodeUnauthenticated` if absent.

### libp2p
Agent-side host is dial-only (`libp2p.NoListenAddrs` when no `ListenAddrs` configured) with a persistent Ed25519 identity keypair on disk (`<dataDir>/p2p-identity.key`). Dialing targets a **Peer ID**, not an address — the address (direct multiaddr or relay circuit multiaddr) is only a routing hint; libp2p's Noise handshake cryptographically proves the responder holds the target Peer ID's private key before the Atlas protocol stream even opens. The existing X.509/mTLS handshake then runs *inside* that already-authenticated stream, unmodified. Two independent trust layers, neither of which is the network address.

### Relay
Genuine libp2p circuit-relay-v2 (`internal/relay/relay.go`, `internal/core/transport/libp2ptransport/libp2ptransport.go:203-229`). Forwards Noise-encrypted byte streams it cannot decrypt — no protocol handler for `/atlas/transport/*` or `/atlas/agentops/*` is ever registered on the relay host, only the relay service itself and a rendezvous announce/lookup handler (which carries only address JSON, never application payload). **Confirmed: the relay makes zero authorization or identity decisions** — no import of `internal/core/fleet` (grants), `internal/platform/pki` (certs/CA), or any enrollment/storage package exists anywhere in `internal/relay/`. Rendezvous lookups are filtered client-side against the operator-pinned target Peer ID (`buildCandidates`, discovery.go:55-73) — a compromised relay can at most hand back a bad *address* for the correct Peer ID, which then fails the libp2p handshake if it doesn't actually lead to that peer. This upholds the "connect by identity, never by address" ADR-0012 constraint and the "relay is transport infrastructure only" requirement in the current code, as implemented.

Known relay-specific risk (not an identity/auth violation, but a resource-exhaustion one): `NewRelayHost` uses `relayv2.WithInfiniteLimits()` — no per-connection duration/bandwidth cap — on what's documented as a "publicly reachable host." Scoped to the libp2p transport, which is off by default (`LibP2PEnabled` defaults false).

### Multi-relationship fanout
A single `fanoutTransport` wraps one `remote.Transport` (or libp2p equivalent) per configured relationship. `Send` fans out in parallel goroutines, waits on all, and only returns an error if **every** target failed — one relationship's failure never blocks or fails another's delivery. Each relationship has its own certs, own spool (isolated data directory), own transport instance, own renewal loop. See Part 4 for the full isolation matrix and the one confirmed cross-relationship leak (shared inventory dedup cache).

---

## PART 4 — Multi-control-plane (`ATLAS_AGENT_RELATIONSHIPS`) — full audit

| Question | Answer | Evidence |
|---|---|---|
| Can one Agent simultaneously connect to local + production? | **Yes** | config.go:122-140, unlimited relationship count |
| Independent certificate per relationship? | **Yes** | credentials.go:179-271, own cert/key per relationship |
| Independent CA/trust per relationship? | **Yes** | own pinned CA file per relationship data dir |
| Independent enrollment per relationship? | **Yes** | `bootstrapAllRelationships` bootstraps each independently, concurrently (agent.go:240-276) |
| Independent spool per relationship? | **Yes** | own disk-backed spool at `DataDir/relationships/<id>/spool` (relationship.go:52-57) |
| Independent retry per relationship? | **Yes** for stream-class (metrics/events) — each relationship's own `remote.Transport` spools and retries independently |
| Independent transport per relationship? | **Yes** — `Transport` (`https`/`libp2p`) is a per-relationship config value (config.go:70,163) |
| Independent server identity (Peer ID) per relationship? | **Yes, and enforced** — duplicate Peer IDs across relationships is a **hard error at agent startup**, aborting the whole process, not a per-relationship skip (`resolvePeerIDConflicts`, agent.go:374-393) |
| Can one relationship fail without affecting another? | **Yes** — bootstrap failures are dropped per-relationship (process only fails if *all* relationships fail); fanout `Send` only errors if every target fails |
| Can one relationship be revoked independently? | **Yes** — control-plane side, `GrantStore`/denylist are per-node, and each relationship presents a distinct node identity/cert to its control plane |
| Does inventory get delivered independently? | **Yes, sent independently — with one caveat below** |
| Does telemetry get delivered independently? | **Yes**, no caveats found — each relationship's spool/retry is fully isolated |
| Can dedup cause one Control Plane to become stale? | **Yes — confirmed real gap.** `inventoryPusher.lastHash` (inventory.go:89-91) is keyed only by subject, **shared across all relationships**. Inventory is snapshot-class and dropped-not-spooled on failure; `fanoutTransport.Send` reports success if *any* target accepted the envelope. So: relationship A succeeds, relationship B fails → pusher still marks the subject "sent" → B doesn't get that subject retried until the underlying data next actually changes. One control plane can carry indefinitely stale inventory for a subject while another has current data. |
| Can event delivery diverge? | Not found to diverge beyond normal independent-retry behavior — each relationship's spool replays independently, no shared event-level cache found (unlike inventory's hash cache) |
| Are configuration values correctly isolated? | **Yes**, per-relationship env-var prefix (`ATLAS_AGENT_RELATIONSHIP_<ID>_*`) parsed independently; duplicate IDs are a hard config error (config.go:134-136) |
| Is `Origin.Environment` incorrectly shared? | **Yes, confirmed and explicitly a known scope decision, not a bug** — a single global environment tag is used across every relationship's envelopes (agent.go:187-189, comment: "not per-relationship (Phase 3 scope decision)"). This means the same Agent cannot currently tag itself `environment=dev` toward a local-dev Atlas and `environment=prod` toward a production Atlas simultaneously — both control planes see the same tag. |
| Is any security context shared between relationships? | **No** — certs, spool, transport, and Peer ID are all independently scoped; the only shared state found across relationships is (a) the inventory dedup cache (data-freshness issue, not a security issue) and (b) `Origin.Environment` (a labeling issue, not a security issue). No cert, grant, or auth state crosses relationship boundaries. |

**Race conditions / shared state inventory:**
1. `inventoryPusher.lastHash map[Subject]string` — shared, unsynchronized-across-relationships mutable state (data-freshness gap, not a race in the concurrency-bug sense; it's a single map correctly used from one goroutine, just conceptually scoped wrong for multi-relationship semantics).
2. `Origin.Environment` — shared global config value, not a runtime race, but a design gap for multi-environment tagging.
3. `p2pHost` — one shared libp2p host object across all relationships using the libp2p transport (agent.go:137-144). This is intentional (one physical network identity, many logical relationships) and is exactly why Peer ID collision is treated as a hard startup error — sharing the host is safe *because* collisions are rejected upfront.

No other shared mutable state or unguarded concurrent access was found across the relationship boundary in this audit.

---

## PART 5 — Production readiness

**[VERIFIED]** One direct conflict was found between two sub-investigations and resolved by reading `internal/app/fleet.go` and `internal/platform/httpx/{server,middleware,tlsserver}.go` directly:

> The agent-facing HTTP listener (`fleet.server` in `internal/app/fleet.go:117-125`, serving `/enroll`, `/renew`, `/heartbeat`, `/telemetry`) is built by handing `mux` directly to `httpx.NewTLSServer(...)`. `BaseMiddleware` — which is where `MaxBodyBytes`, `Recoverer`, `Timeout`, `SecurityHeaders`, `AccessLog`, and `CORS` all live — is called in exactly one place in the entire repo: `internal/api/router.go:98`, which builds the **browser-facing** API server. `internal/app/fleet.go` never calls `BaseMiddleware`. **Confirmed: the agent-facing listener has no request body size cap, no panic recovery, no request timeout beyond a 10s `ReadHeaderTimeout`, and no security headers.** The 1 MiB `MaxRequestBytes` config value exists and is enforced — but only on the UI/API surface, not on the surface that accepts data from potentially hundreds of remote agents.

| # | Category | Finding | Verdict | Files |
|---|---|---|---|---|
| 1 | Middleware gap | Agent listener has zero middleware chain — no body cap, no panic recovery, no timeout, no security headers | **Confirmed real, load-bearing gap** | internal/app/fleet.go:117-125; internal/platform/httpx/server.go:167-177 |
| 2 | TOFU on first enrollment | `InsecureSkipVerify: true` in `newBootstrapClient` whenever no CA bundle is configured (default) — first enrollment trusts whatever cert the endpoint presents, no verification | **Real risk, scoped to first-contact only** (CA is pinned to disk after, reloaded every restart) | internal/agent/credentials.go:73-86,151-165 |
| 3 | Backpressure directive unimplemented server-side | Client honors `slow_down`/`pause`; server only ever sends `"ok"` | **Confirmed dead code path** — the safety valve doesn't function | internal/api/agent/handler.go:211,275; internal/core/transport/remote/remote.go:225-235 |
| 4 | No rate limiting anywhere in the HTTP layer | No limiter/throttle middleware exists in `internal/platform/httpx` or `internal/app`; a valid-cert node can call telemetry/heartbeat at unlimited frequency | **Confirmed gap** | internal/platform/httpx/middleware.go (absence); internal/app/fleet.go |
| 5 | Orphaned spool `.tmp` files | Directory scan on `Open()` only matches `*.envelope.json`; the write-path temp file (`....envelope.json.tmp`) is invisible to that scan. A crash between `os.OpenFile` and the deferred cleanup on an error path leaks the temp file forever — uncounted against `MaxBytes`, unswept by `MaxAge` | **Real minor gap** — disk-exhaustion risk under repeated crash cycles, not routine operation | internal/core/transport/spool/spool.go:95-133 (scan), :161-188 (write) |
| 6 | Relay `WithInfiniteLimits()` | No per-connection duration/bandwidth cap on the relay's circuit-relay-v2 service, described as running on a "publicly reachable host" | **Real, but scoped to the opt-in libp2p transport (off by default)** | internal/core/transport/libp2ptransport/libp2ptransport.go:198-202,223 |
| 7 | Protocol version not enforced on HTTPS path | `TelemetryRequest.ProtocolVersion` is decoded but never compared against the server's constant; enroll/renew responses' `ProtocolVersion` is decoded and never checked by the agent either. Only the libp2p AgentOps sidecar enforces version match. | **Real gap for the primary (HTTPS) path** — an old agent talking to a new control plane (or vice versa) is not blocked by version, only by incidental JSON-shape compatibility | internal/api/agent/handler.go:34,166; internal/agent/credentials.go:35 (unused field); libp2ptransport/agentops.go:393-395 (the one path that DOES check) |
| 8 | Payload `Validate()` not wired into network path | `Envelope.Validate()`/`Payload.Validate()` exist and check required fields, but `Handler.Telemetry` never calls them — only `InProcess.Send` (local same-process transport) does | **Confirmed gap** — schema/required-field validation is coded but dormant on the path that matters | internal/core/transport/transport.go:108-116; internal/api/agent/handler.go:194-255; internal/core/transport/inprocess.go:53 |
| 9 | No `ObservedAt` sanity bound | Only `SentAt` is clock-skew checked (`DefaultMaxClockSkew = 5m`); `ObservedAt` (the inventory subject's own timestamp) is never bounded | **Minor gap** | internal/api/agent/handler.go:226-235 |
| 10 | No `atlas-agent` build target | Root `Makefile` only builds `atlas-server`; `atlas-agent` has no `make` target and isn't built by the `Dockerfile` — version/commit ldflags stamping for the agent is manual/undocumented | **Real packaging gap**, orthogonal to runtime safety but directly relevant to "ship one stable binary" | Makefile:11 (BINARY := atlas-server, no agent equivalent); Dockerfile (server-only) |
| — | Everything else checked | Goroutine lifecycles (all bound to ctx/wg/session caps), channel bounding (all bounded or paired with select), cardinality bounding (top-N/watch-list, unchanged), client-side payload size caps (`maxBatchSize=200`), TLS min version (1.3 everywhere), HTTP client timeouts (all explicit), spool atomic-write correctness (temp+rename, correct except for #5), enrollment token security (256-bit, hashed, TTL, CIDR, atomic redemption) | **Already correctly implemented** | — |
| — | TODO/FIXME/POC markers | All are either accurate labels for an off-by-default experimental feature (libp2p "POC") or documented, accepted scope cuts (server leaf cert renewal requires restart; remote service-graph traversal deferred) | **Cosmetic / accepted**, none are silent gaps | various, see production-readiness scan detail |

---

## PART 6 — Agent version / upgrade model

| Question | Answer | Evidence |
|---|---|---|
| What state lives on disk? | Client cert (`<DataDir>/agent-cert.pem`), client key (`agent-key.pem`, mode 0600), pinned CA (`ca-cert.pem`), per-relationship config (`relationship.json`, or `relationships/<id>/relationship.json`), spool files (`*.envelope.json`), libp2p identity (`p2p-identity.key`), cached rendezvous target (`p2p-last-known.json`), node identity fallback state (`/var/lib/atlas/node-id` — note: different default path than `DataDir`) | internal/platform/pki/store.go:58,68; internal/agent/credentials.go:151; internal/agent/relationship.go:52-57,141-143; internal/core/transport/spool/spool.go:161-163; internal/platform/hostid/hostid.go:128 |
| Is on-disk state versioned/schema-tagged? | **No** — `relationship.json` has no version field, PEM certs are unversioned, spool files carry no version marker (the *wire* protocol has `ProtocolVersion`, individual files do not). A newer binary blindly assumes current format. | internal/agent/relationship.go:130-139; internal/platform/pki/store.go:17-20 |
| Does node identity survive a reimage? | **No** — machine-id (the primary identity source) is regenerated by most reimage/provisioning flows, and the derived NodeID changes with it; there is no fallback that preserves identity across a genuinely new machine-id when machine-id is present (it always wins over the state-file fallback) | internal/platform/hostid/hostid.go:142-211 |
| Does node identity survive a DataDir wipe (machine-id untouched)? | **Yes** — identity doesn't come from DataDir when machine-id is available; but DataDir wipe forces re-enrollment (certs gone) | hostid.go:170-175 vs credentials.go:198-211 |
| Binary-only replace: certs | **Reused, not re-enrolled** — `bootstrap()` loads existing cert/key first, skips enrollment if present | credentials.go:179-211 |
| Binary-only replace: spool | **Replayed, not lost** — resumed on startup if not older than 24h | spool.go:88-133 |
| Binary-only replace: relationship config | **Stable, file-authoritative** — once `relationship.json` exists, corresponding env vars are ignored (only `Token`/`CABundlePath` are still live-read); changing the systemd env file post-bootstrap has no effect unless the file is deleted/edited | relationship.go:152-179 |
| Version negotiation, HTTPS path | **None enforced** — `ProtocolVersion` is carried but never checked on enroll/renew/telemetry | handler.go:166,194-255; credentials.go:35 |
| Version negotiation, libp2p AgentOps path | **Enforced** — mismatch produces an explicit error frame | agentops.go:393-395 |
| Build identity (version/commit/time) embedded? | **Yes, via ldflags — but only wired for `atlas-server`.** `atlas-agent` has no build target, so agent build stamping is manual | Makefile:22-25 (server); no agent equivalent found |
| Is `AgentVersion` sent/stored? | **Yes** — `Origin.AgentVersion`, round-trips to `nodes.agent_version` and the node-listing API. **Commit/build-time are not**, despite existing as build-time fields. | agent.go:195,214; api/v1/nodes.go:27,101; build/build.go:23-30 |
| Rollback safety (newer→older binary) | **No evidence of a format the older binary couldn't read** (Go JSON is lenient about unknown fields) — but also **no explicit version guard that would detect a future incompatibility** if one is ever introduced. Compatibility today is implicit/structural, not contractual. | store.go:17-20; relationship.go; spool.go |
| Does install/uninstall preserve state across reinstall/upgrade? | **Yes, explicitly** — `uninstall.sh` default (non-`--purge`) leaves `/var/lib/atlas-agent` and `/etc/atlas-agent` in place; `install.sh` preserves an existing env file unless `--force-env`; binary is unconditionally replaced (the actual upgrade mechanism) | packaging/atlas-agent/uninstall.sh:29-37; install.sh:66,87-100 |
| Documented upgrade runbook? | **No** — `docs/operations/deployment.md`'s only "Upgrades" section is about `atlas-server`/DB migrations, nothing agent-specific | docs/operations/deployment.md:223-233 |

---

## PART 7 — UI / server decision-making: collected → sent → stored → API → UI

Where the chain stops, exactly:

| Category | Collected | Sent | Stored | API | UI | Stops at |
|---|---|---|---|---|---|---|
| Container MAC / gateway | ● | ● | ● | ● | **○** | UI — typed at `web/src/api/types.ts:314-321`, zero rendering components |
| Service cgroup CPU seconds | ● (parsed) | **○** | — | — | — | metrics emitter — `plugin.go:318-346` never references `Unit.CPUSeconds` |
| Agent connection/health state | **○** (not even collected as a payload) | — | — | — | — | source — no health payload type exists at all |
| Transport delivery stats (Sent/Failed/Rejected/Spooled) | ● (in-process) | **○** | — | — | — | agent code itself — `remote.Transport.Stats()` has zero callers in `internal/agent` |
| Host network identity (IP/MAC/gateway/DNS) | **○** | — | — | — | — | source — no collector exists |
| Listening-port TLS info | ● | ● | ● | ● | ● | fully wired, no stop |
| Everything else in Part 1 marked `●/●/●/●/●` | — | — | — | — | — | fully wired |

No inventory *category* is collected-and-persisted-but-unexposed at the category level — every `inventory.Subject` has a storage path and a UI page. All confirmed gaps are field-level (container mac/gateway) or entirely-uncollected (host network identity, agent health).

---

## PART 8 — Final gap list

| Priority | Category | Current state | Required change | Why | Files |
|---|---|---|---|---|---|
| **P0** | Reliability/Security | Missing | Wrap the agent-facing listener (`fleet.server`, both HTTPS and libp2p variants) with body-size cap, panic recovery, and request timeout | An unbounded request body on a listener reachable by every enrolled agent (and, pre-enrollment, by anyone who can reach the port) is a trivial memory-exhaustion vector; a single unrecovered panic in any handler currently takes the whole listener down | internal/app/fleet.go:117-161; internal/platform/httpx/server.go (reuse `BaseMiddleware` or a purpose-built agent variant) |
| **P0** | Security | Missing | Wire `Envelope.Validate()`/`Payload.Validate()` into `Handler.Telemetry` before dispatch | Malformed/empty-required-field payloads currently reach storage untouched; validation logic already exists and is tested elsewhere, it's just not called | internal/api/agent/handler.go:194-255 |
| **P0** | Data | Missing | Add a host network-interface collector: name, IPv4/IPv6, MAC, up/down state, MTU | This is the single largest data-collection gap; nearly every "is this server reachable" or "which address is this alert about" decision needs it, and it doesn't exist at any layer today | internal/plugin/system/ (new collector_network_identity.go or extend collector_network.go); internal/core/inventory/snapshot.go (new subject) |
| **P0** | Architecture | Missing | Add an agent self-health payload (connected state is implicit today; make spool depth, last successful delivery, cert expiry, delivery/failure counts explicit and transmitted) | Currently "is monitoring itself broken" is invisible to the control plane; this is the precondition for trusting every other metric in the system | internal/agent/agentops.go or new internal/agent/health.go; internal/core/transport/payload.go (new Kind); server: internal/api/agent/handler.go, internal/storage |
| **P1** | Reliability | Broken/dead | Implement server-side backpressure computation (queue depth / DB write latency → `slow_down`/`pause`) so the already-honored client directive actually does something | Client-side honoring code is dead weight without a server that ever sends anything but "ok"; under real load this is a missing safety valve, not a hypothetical one | internal/api/agent/handler.go:211,275; wherever ingest load should be measured (internal/app or internal/storage) |
| **P1** | Security | Missing | Add per-node rate limiting on `/telemetry` and `/heartbeat` | A single compromised or misconfigured agent can currently flood the ingest path at unlimited rate with no server-side check beyond the 1 MiB-if-it-existed body cap | internal/platform/httpx/middleware.go (new limiter middleware); internal/app/fleet.go |
| **P1** | Security | Unsafe default | Require `ATLAS_AGENT_CA_BUNDLE` to be set (or an explicit `ATLAS_AGENT_INSECURE_BOOTSTRAP=true` opt-in) rather than silently defaulting to TOFU/`InsecureSkipVerify` on first enrollment | First enrollment currently trusts whatever certificate the endpoint presents by default — acceptable for a lab, not for a fleet provisioning pipeline that should pin CA out of band | internal/agent/credentials.go:73-86; internal/agent/config.go |
| **P1** | Reliability | Unsafe | Fix orphaned spool `.tmp` file leak — include `*.envelope.json.tmp` in the startup sweep and delete anything past a short age threshold | Repeated crash-during-write cycles (flaky host, OOM-killer) leak disk space outside the spool's own `MaxBytes`/`MaxAge` accounting indefinitely | internal/core/transport/spool/spool.go:95-133 |
| **P1** | Transport | Missing | Enforce `ProtocolVersion` on the HTTPS enroll/renew/telemetry path, matching what the libp2p AgentOps path already does | Prevents undefined behavior when an old agent talks to a new control plane or vice versa, instead of relying on incidental JSON-shape tolerance | internal/api/agent/handler.go; internal/agent/credentials.go |
| **P1** | Architecture | Missing | Fix the multi-relationship inventory dedup cache to be per-relationship, not global | Prevents one control plane from silently carrying stale inventory for a subject indefinitely after a partial fanout failure while another control plane has current data | internal/agent/inventory.go:89-91 |
| **P1** | Data | Missing | Collect CPU model string, CPU topology (sockets/cores/threads), and virtualization/hypervisor detection | Cheap (existing gopsutil/`cpu.Info` and standard `/sys` reads), directly useful for fleet capacity-planning classification | internal/plugin/system/collector_host.go, gopsutil.go |
| **P1** | Data | Missing | Collect default gateway + DNS resolver configuration (search domains + server list) | Common root cause of "unreachable"/"DNS broken" incidents currently undiagnosable from Atlas data alone | internal/plugin/system/ (new collector) |
| **P2** | Data | Missing | Collect network connectivity/reachability probe (e.g. to the configured control plane, or a configurable target) | Direct answer to "is the server reachable" instead of inferring from telemetry staleness | internal/plugin/system/ or internal/agent/ |
| **P2** | Data | Missing | Collect SELinux/AppArmor enforcement mode | Single low-risk enum, directly useful for "why was this process denied" diagnosis | internal/plugin/system/ |
| **P2** | UI | Present-but-unexposed | Render container `mac_address`/`gateway` (already in the API response) in `ContainerInspector.tsx` | Zero new collection work; pure UI gap | web/src/pages/containers/ContainerInspector.tsx |
| **P2** | Data | Present-but-unemitted | Wire `Unit.CPUSeconds` (already parsed) into a `service.cpu` metric | Zero new collection work; closes a per-service resource-usage blind spot | internal/plugin/service/plugin.go:318-346 |
| **P2** | Data | Missing | Collect process open-file/socket counts | Early indicator of fd-leak/connection-exhaustion incidents on a specific process | internal/plugin/process/gopsutil.go |
| **P2** | Data | Missing | Collect public egress IP (best-effort, e.g. via the control-plane connection's observed source, not a third-party lookup) | Useful for fleet-wide network-path/geography correlation; must be sourced carefully to avoid a third-party dependency — see Part 9 exclusions | internal/plugin/system/ or server-observed (see design note in Part 9) |
| **P2** | Packaging | Missing | Add an `atlas-agent` build target to the Makefile with the same ldflags version/commit/build-time stamping as `atlas-server`, and send commit/build-time on `Origin` alongside `AgentVersion` | "Ship one stable binary" requires the binary to actually assert its own provenance; currently agent builds are unstamped/manual | Makefile; internal/core/transport/transport.go (Origin fields); internal/agent/agent.go |
| **P3** | Data | Missing | Firewall enabled/disabled + rule count (not full rule dump) | Lower-value than SELinux mode; full rule dump would be a privacy/security-posture disclosure risk, so scope narrowly if implemented at all | internal/plugin/system/ |
| **P3** | Data | Missing | Disk I/O latency (if gopsutil/OS exposes it cheaply) | Nice-to-have precision improvement over the current rate-derived utilization approximation | internal/plugin/system/collector_disk.go |
| **P3** | Reliability | Missing | Add explicit schema/version tagging to on-disk state (`relationship.json`, spool files) | No current incompatibility exists, but there's no guard that would catch one introduced in a future release | internal/agent/relationship.go; internal/core/transport/spool/spool.go |
| **P3** | Ops | Missing | Write an agent-specific upgrade runbook | Operational documentation gap, not a code gap | docs/operations/ |

---

## PART 9 — Final production-ready Agent spec

### A. MUST HAVE before production
1. Body-size cap, panic recovery, and request timeout on the agent-facing listener (P0).
2. Envelope/payload validation wired into the network ingest path (P0).
3. Host network-interface identity collection: name, IPv4/IPv6, MAC, up/down, MTU (P0).
4. An explicit agent self-health signal reaching the control plane — at minimum: last successful delivery time, current spool depth, cert expiry (P0).
5. Per-node rate limiting on telemetry/heartbeat (P1).
6. CA bundle required (or explicit opt-in) rather than silent TOFU default (P1).
7. Spool `.tmp` leak fix (P1).
8. Protocol-version enforcement on the HTTPS path (P1).
9. Per-relationship inventory dedup cache fix (P1) — this is specifically required given the stated goal of simultaneous, independent local+production delivery.
10. A real, ldflags-stamped `atlas-agent` build target, so "one binary, many servers" is actually a reproducible artifact with known provenance (P1/packaging).

### B. SHOULD HAVE
1. Default gateway + DNS resolver collection.
2. Server-side backpressure computation (making the existing client-side `slow_down`/`pause` handling meaningful).
3. CPU model, CPU topology, virtualization/hypervisor detection.
4. Network connectivity/reachability probe.
5. SELinux/AppArmor enforcement mode.
6. Container mac/gateway rendered in UI (already collected — pure UI work).
7. Service cgroup CPU wired into metrics (already collected — pure wiring work).
8. Process open-file/socket counts.
9. Pattern-based redaction option for process `Cmdline`/cron `Command` (configurable, off by default to preserve current diagnostic value, on by default in a hardened profile) — a middle ground between "capture everything" and "capture nothing" for the one confirmed, deliberate secrets-exposure surface.

### C. NICE TO HAVE
1. Disk I/O latency, if cheaply available.
2. Firewall enabled/disabled + rule count (narrowly scoped).
3. Explicit schema/version tags on on-disk state, for future-proofing rollback safety.
4. Agent build commit/build-time transmitted alongside version.
5. Route table (default-route-only is a MUST/SHOULD; full table is NICE).
6. Manufacturer/motherboard/BIOS/firmware identification (useful for fleet hardware inventory, low urgency for monitoring/alerting decisions).

### D. SHOULD NEVER BE IMPLEMENTED
1. Container/process environment variable collection — current exclusion is correct and should remain permanent policy, not a gap.
2. Arbitrary application log file collection (contents of `/var/log/*` or app-specific logs) — out of Atlas's stated mandate; would turn a read-only observability agent into a log-aggregation/exfiltration surface.
3. Arbitrary filesystem content reads (config files, `.env` files, secret material on disk) — no legitimate monitoring use case justifies this; the risk profile is categorically different from structural facts like "this file exists, this many bytes."
4. Full firewall rule *content* dumps (as opposed to enabled/disabled + count) — discloses security posture detail disproportionate to monitoring value.
5. Any control/mutation capability beyond the two narrowly-scoped exceptions that already exist and should stay exceptions (container log viewing, on-demand and revocable) — "Observe Everything. Control Nothing." is a stated architectural principle (IMPLEMENTATION_CONTEXT.md) and should gate every future AgentOps operation proposal, not just log streaming.
6. Relay making any authorization/identity decision — confirmed clean today; any future change that has the relay check grants, verify certs, or gate operations would violate ADR-0012 and must be rejected in review, not just avoided by convention.
7. Public-IP lookup via a third-party service (e.g. calling an external "what's my IP" API from the agent) — if public IP is ever collected (Part 8, P2), it must be sourced from information the control plane already observes (the connection's own source address) rather than adding an outbound third-party dependency to every agent in the fleet.

---

## PART 10 — Implementation plan (no code — ordering and file scope only)

**Phase 1 — Data model**
Define new inventory subjects/payload kinds before writing any collector: host network identity, agent self-health. Extend `internal/core/transport/payload.go` (Kind registry) and `internal/core/inventory/snapshot.go` (Subject registry) with the new types and their JSON shapes. Decide the self-health payload's exact field set here, not during collector work.
*Files:* `internal/core/transport/payload.go`, `internal/core/inventory/snapshot.go`, `internal/core/inventory/payload.go`.

**Phase 2 — Collectors**
Implement host network-interface collector (name/IP/MAC/up-down/MTU), default gateway + DNS collector, CPU model/topology/virtualization detection, SELinux/AppArmor mode, process open-file/socket counts, and the service cgroup-CPU wiring fix (already-collected data, just needs an emit call). Each collector follows the existing plugin pattern (`Detect`/`Available`/`Collect`/`Inventory`) already established in `internal/plugin/system` and `internal/plugin/service`.
*Files:* `internal/plugin/system/collector_network.go` (extend) or new `collector_network_identity.go`, `internal/plugin/system/collector_host.go`, `internal/plugin/service/plugin.go` (CPU wiring fix), `internal/plugin/process/gopsutil.go`.

**Phase 3 — Transport**
Add body-size cap/panic-recovery/timeout middleware to the agent-facing listener; wire `Envelope.Validate()` into `Handler.Telemetry`; enforce `ProtocolVersion` on the HTTPS path; fix the spool `.tmp` sweep gap; implement server-side backpressure computation feeding the already-functional client-side directive handling.
*Files:* `internal/app/fleet.go`, `internal/platform/httpx/{server.go,middleware.go}`, `internal/api/agent/handler.go`, `internal/core/transport/spool/spool.go`, `internal/core/transport/remote/remote.go` (verify no client change needed — it already honors directives).

**Phase 4 — Multi-relationship isolation**
Fix the inventory dedup cache to be keyed per-relationship, not globally. Decide (needs a product/architecture call, not just code) whether `Origin.Environment` should become per-relationship — this affects the wire format, so resolve before Phase 3's transport work ships if both land in the same release.
*Files:* `internal/agent/inventory.go`, `internal/agent/agent.go` (Origin construction), `internal/core/transport/transport.go` (if Origin shape changes).

**Phase 5 — Security**
Change the CA-bundle default from silent TOFU to required-or-explicit-opt-in; add per-node rate limiting middleware; add pattern-based redaction option for `Cmdline`/`Command` fields (configurable).
*Files:* `internal/agent/credentials.go`, `internal/agent/config.go`, `internal/platform/httpx/middleware.go`, `internal/plugin/process/gopsutil.go`, `internal/plugin/cron/cron.go`.

**Phase 6 — Server/API/storage**
Add server-side handling for the new self-health payload kind and host-network-identity inventory subject: new `Router` registration, new storage receiver/repository methods, schema migration for any new persisted fields (host network identity likely fits into a new `inventory_snapshots` subject, needing no schema change; self-health likely needs new columns on `nodes` or a new small table).
*Files:* `internal/app/fleet.go` (router registration), `internal/storage/inventory/*`, `internal/storage/fleet/*` (if self-health lands on `nodes`), new file under `migrations/`.

**Phase 7 — UI**
Render container `mac_address`/`gateway` (already fetched). Add a network-identity view/section per node. Add an agent-health indicator (connected/stale, spool depth, cert expiry) to the node inspector, since Phase 1-6 will have made this data available for the first time.
*Files:* `web/src/pages/containers/ContainerInspector.tsx`, `web/src/pages/nodes/NodeInspector.tsx`, `web/src/api/types.ts`.

**Phase 8 — Testing**
Integration tests per the project's stated preference (CLAUDE.md: "Prefer integration tests over mocks"). Priority coverage: multi-relationship fanout with one relationship's transport forced to fail (verify isolation and the dedup-cache fix); spool crash-recovery including the `.tmp` sweep fix; server-side validation rejecting malformed envelopes; rate limiter behavior under burst load; protocol-version mismatch handling on the HTTPS path.
*Files:* `internal/agent/*_test.go`, `internal/core/transport/spool/spool_test.go`, `internal/api/agent/handler_test.go`, new test files as needed alongside each Phase 2-6 change.

**Phase 9 — Upgrade/rollback**
Add explicit version/schema tagging to `relationship.json` and spool file format (Part 8 P3) so a future incompatible change can be detected rather than silently misread. Write the agent-specific upgrade runbook. Add the `atlas-agent` Makefile build target with ldflags version/commit/build-time stamping, matching the existing `atlas-server` pattern, and start transmitting commit/build-time on `Origin` alongside the existing `AgentVersion`.
*Files:* `internal/agent/relationship.go`, `internal/core/transport/spool/spool.go`, `Makefile`, `internal/agent/agent.go`, `docs/operations/` (new runbook).

**Phase 10 — Production deployment**
Roll out to a small canary set of servers first (mix of the two target OS families — Linux systemd hosts and Mac dev machines, since the spec explicitly requires both). Verify: identity persists across a real reimage-free restart, multi-relationship delivery to both local-dev and production Atlas instances is independently confirmed via the new self-health signal, and the rate limiter/backpressure changes don't reject legitimate bursts (e.g. after a control-plane outage, when spools everywhere replay simultaneously). Only then broaden rollout.
*Files:* none (operational phase) — `packaging/atlas-agent/install.sh` and the systemd unit are the only artifacts touched, and only if canary findings require install-script changes.

---

*End of audit. No files were modified in the course of this investigation. All findings above are traceable to file:line references in the working tree at the time of this audit (2026-08-14).*
