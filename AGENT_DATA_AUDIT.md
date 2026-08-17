# Atlas Agent — Source-Level Data Audit

Read-only investigation. No files modified as part of producing this report. Every claim below is backed by a file:line reference in the current working tree; the most security-relevant claims were independently re-verified by direct grep/read before inclusion.

Repo scope: `internal/agent`, `internal/plugin/*`, `internal/core`, `internal/api`, `internal/storage`, `web/src`.

---

## 1. Host / system information

Collector: `internal/plugin/system/collector_host.go` + `gopsutil.go` (gopsutil/v4). Node identity: `internal/platform/hostid/hostid.go`.

| Field | Status | Source | Reference |
|---|---|---|---|
| Hostname | collected | `host.InfoWithContext` | gopsutil.go:42 |
| Machine/node ID | collected (hashed) | `/etc/machine-id` → HMAC-SHA256; raw value never leaves the function, only first 32 hex chars of digest kept | hostid.go:170-175, 251-254 |
| OS / kernel family | collected | `host.InfoWithContext` (`OS`: linux/darwin) | gopsutil.go:43 |
| Distribution | collected | `Platform` field (e.g. "ubuntu") | gopsutil.go:44 |
| OS version | collected | `PlatformVersion` (e.g. "24.04") | gopsutil.go:45 |
| Kernel version | collected | `KernelVersion` | gopsutil.go:46 |
| Architecture | collected | `KernelArch` (x86_64/arm64) | gopsutil.go:47 |
| CPU model | **not collected** | no `cpu.InfoWithContext` (model name) call found | — |
| CPU logical/physical cores | collected | `cpu.CountsWithContext(ctx, true/false)` | gopsutil.go:55-60 |
| CPU utilization (aggregate + per-core) | collected | `cpu.PercentWithContext(ctx, 0, true)` | gopsutil.go:71-75 |
| CPU state times | collected | `cpu.TimesWithContext` (user/system/idle/iowait/steal/nice/irq/softirq) | gopsutil.go:81-94 |
| Load average 1/5/15 | collected | `load.AvgWithContext` | gopsutil.go:242-246 |
| RAM total/used/free/available/cached/buffers/used% | collected | `mem.VirtualMemoryWithContext` | gopsutil.go:105-118 |
| Swap total/used/free/used%/Sin/Sout | collected | `mem.SwapMemoryWithContext` | gopsutil.go:128-131 |
| Boot time | collected | `host.InfoWithContext` (unix secs) | gopsutil.go:48 |
| Uptime | derived | `time.Since(BootTime)`, not its own field | provider.go:91-96 |
| Timezone | **not collected** | no timezone collection code anywhere in repo | — |
| Manufacturer/model/DMI | **not collected** | repo-wide grep for `dmi`/`sys_vendor`/`product_name`: zero hits | — |

Host facts feed `FactsRecorder.UpdateNodeFacts` and persist onto the `nodes` row (not a snapshot subject) — see §14.

---

## 2. Network information

Collector: `internal/plugin/system/collector_network.go` (interface counters only) + `internal/plugin/ports` (listening sockets). **There is no host network-interface inventory subject anywhere in the codebase** — no IP addresses, no MAC addresses, no gateway, no DNS.

| Field | Status | Detail | Reference |
|---|---|---|---|
| Interface name | collected | `NetworkIOCounters.Name` ← `net.IOCountersWithContext(ctx, true)` | gopsutil.go:217-237 |
| Interface state (up/down) | **not collected** | no state field; zero-traffic interfaces are merely skipped, not a state | collector_network.go:70 |
| MAC address (host NIC) | **not collected** | only `MacAddress` field in repo belongs to Docker *container* network metadata | docker/client.go:212 |
| IPv4 / IPv6 (host NIC) | **not collected** | no `net.Interfaces()`/`InterfaceAddrs` call in `internal/plugin/system` | — |
| Private IPv4 | **not collected** | — | — |
| Public IPv4 | **not collected** | repo-wide grep for `PublicIP`/`public_ip`: zero hits | — |
| Subnet/prefix | **not collected** | — | — |
| Gateway | **not collected (host)** | `Gateway` field exists only on Docker container network metadata | docker/client.go:211 |
| DNS servers | **not collected** | every `DNS` hit in repo is a TLS cert SAN or Atlas's own cert-gen config, none is resolver collection | ports/tls.go:82; platform/pki |
| RX/TX bytes, packets, errors, drops | collected | `net.IOCountersWithContext`, rates via `rateTracker` | provider.go:183-193; collector_network.go:82-92 |
| Connectivity status | **not collected** | no ping/reachability check anywhere | — |
| Listening TCP ports + bind address | collected | separate `ports` plugin — see §8 | ports/gopsutil.go |
| Listening UDP ports + bind address | collected | all bound UDP sockets kept | ports/gopsutil.go:28 |

**Finding:** every IP-adjacent field a monitoring platform would typically expose — interface IPs, MAC, gateway, DNS, public IP, connectivity — is absent for the *host*. The only network identity data anywhere in the agent is (a) interface traffic counters keyed by name, and (b) listening-socket bind addresses/ports. Container-level network metadata (name, IP, MAC, gateway, aliases) is collected by the Docker plugin, but per §14 the UI only renders `name`, `ip_address`, `aliases` — `mac_address`/`gateway` are fetched into the API response and never rendered.

---

## 3. Storage / filesystem

Collector: `internal/plugin/system/collector_disk.go` + `gopsutil.go:145-215`.

| Field | Status | Source | Reference |
|---|---|---|---|
| Partitions (device, mountpoint, fstype, opts) | collected | `disk.PartitionsWithContext(ctx, false)`, filtered against pseudo-filesystem denylist | gopsutil.go:137-150 |
| Total/free/used capacity, used% | collected | `disk.UsageWithContext` (statfs-family syscall per mount) | gopsutil.go:177-191 |
| Inode total/used/used% | collected | same call | gopsutil.go:177-191 |
| Disk I/O read/write bytes, ops | collected | `disk.IOCountersWithContext`, per device | gopsutil.go:197 |
| IOPS | derived | rate of read/write op counters via `rateTracker` | rate.go; collector_disk.go |
| Disk utilization | derived | from `IoTime` rate ÷ 10 | collector_disk.go:149-157 |

---

## 4. Process information

Collector: `internal/plugin/process/gopsutil.go`. Selection logic in `plugin.go`.

| Field | Status | Source | Reference |
|---|---|---|---|
| PID / PPID | collected | `p.Pid`, `PpidWithContext` | gopsutil.go:65-69 |
| Name / executable path | collected | `NameWithContext`, `ExeWithContext` | gopsutil.go:59, 70-72 |
| Command line | collected, unredacted | `CmdlineWithContext`, truncated to 512 chars | gopsutil.go:73-78 |
| User | collected | `UsernameWithContext` | gopsutil.go:79-81 |
| CPU% / memory RSS / memory% | collected | `CPUPercentWithContext`, `MemoryInfoWithContext`, `MemoryPercentWithContext` | gopsutil.go:85-93 |
| State | collected | `StatusWithContext`, normalised | gopsutil.go:82-84 |
| Threads | collected | `NumThreadsWithContext` | gopsutil.go:94-96 |
| Start time | collected | `CreateTimeWithContext` | gopsutil.go:97-99 |
| Open files / connections | **not collected** | no `OpenFilesWithContext`/`ConnectionsWithContext` call | — |
| Top processes | collected | aggregated by name, summed CPU/RSS/instance-count; two sorted top-N lists (`process.top.cpu`, `process.top.memory`), default N=10 | plugin.go:28-33, 233-306 |

**Confirmed risk — secrets in command lines.** Full argv captured verbatim from `/proc/[pid]/cmdline`, no argument-pattern redaction, only a 512-byte cap:

```go
// internal/plugin/process/gopsutil.go:73-78
if cmdline, err := p.CmdlineWithContext(ctx); err == nil {
    if len(cmdline) > maxCmdline {
        cmdline = cmdline[:maxCmdline] + "…"
    }
    proc.Cmdline = cmdline
}
```

Package doc names the trade-off directly: *"Cmdline is the full command line, truncated. Inventory only — it can contain credentials passed as arguments."* (provider.go:64-66). Confirmed transmitted: serialized in the inventory API response at `internal/api/v1/inventory.go:30,69`. A process started as `mysqldump -uroot -pSECRET` or with an inline API token as a flag is captured and exposed as-is. Deliberate, documented trade-off — not a bug — but a real exposure surface.

---

## 5. Service information (systemd)

Collector: `internal/plugin/service/systemd.go`. Shells out to the `systemctl` binary — never go-systemd/dbus — because dbus access is frequently unavailable in containers (documented at systemd.go:16-28).

| Field | Status | Source | Reference |
|---|---|---|---|
| Name / description | collected | `systemctl list-units --type=service --all` | systemd.go:77-78, 117-123 |
| Active/sub/load state | collected | same `list-units` call | systemd.go:118-120 |
| Enabled state | collected | `systemctl show --property=UnitFileState`, one batched call for all units | systemd.go:160-163, 208 |
| Failed state | derived | `ActiveState == Failed`, not stored | provider.go:96 |
| Restart count | collected | `show --property=NRestarts` | systemd.go:197-199 |
| Uptime | derived | `time.Since(ActiveEnterTimestamp)` | provider.go:106-112 |
| Memory usage (cgroup) | collected | `show --property=MemoryCurrent` | systemd.go:202-204 |
| CPU usage (cgroup) | collected, **not emitted** | `CPUUsageNSec` parsed onto `Unit.CPUSeconds` but never referenced by the metrics emitter | systemd.go:205-207; plugin.go:318-346 |
| Service dependencies | collected, actively used | full graph: `Requires/Wants/BindsTo/PartOf/After/Before/…` per-unit, built into a real traversable graph with working `Impact()`/`Propagate()` | systemd.go:243-293, 393-426; graph.go:131-175, 422-552 |

Metrics actually emitted: `service.up`, `service.restarts`, `service.enabled`, `service.uptime`, `service.memory`, plus aggregates `service.count`, `service.total`, `service.failed`. Per-unit series only for failed units or units on a default watch list, to bound cardinality (plugin.go:283-346).

---

## 6. Cron information

Collector: `internal/plugin/cron/cron.go`. Reads files directly off disk — deliberately never shells out to `crontab -l` (cron.go:10-17: *"crontab is a setuid-adjacent tool … one mistyped argument is the difference between listing a user's jobs and replacing them"*).

| Field | Status | Source | Reference |
|---|---|---|---|
| System crontab | collected | `/etc/crontab` | cron.go:79, 147-149 |
| cron.d | collected | `/etc/cron.d` | cron.go:80, 151 |
| User spool | collected | `/var/spool/cron/crontabs`, `/var/spool/cron` | cron.go:83-86, 153-169 |
| Periodic (run-parts) | collected | `/etc/cron.{hourly,daily,weekly,monthly}`, owner hardcoded `root` | cron.go:87-92, 172-189 |
| Schedule | collected | first 5 whitespace fields, or `@`-token | cron.go:279-286, 309 |
| Command | collected, unredacted | raw command string, truncated to 300 chars | cron.go:96, 309-324 |
| Owner/user | collected | column 6 (system sources), spool filename (user crontabs), or `root` (periodic) | cron.go:168, 184, 268-311 |

**Confirmed risk — secrets in cron commands.** No secret-pattern redaction anywhere in cron.go. The codebase is explicitly self-aware: cron.go:54-56 — *"Command is what runs. Truncated, and never used as a metric label — a command line can contain credentials."* Mitigation is metrics-only: the metrics collector emits counts by source/root, never command strings (plugin.go:92-96, 120-160). The raw (truncated, unredacted) `Command` string is still returned by `Plugin.Inventory()` and served live through the API as inventory data — a job like `curl -H "Authorization: Bearer xyz" …` is exposed as-is, up to 300 characters.

---

## 7. Docker / container information

Collector: `internal/plugin/docker/` (client.go, collectors.go, engine.go, events.go).

| Field | Status | Reference |
|---|---|---|
| Docker daemon version / API version | collected | client.go:63-64; engine.go:57-60 |
| Container ID / name / image / image ID | collected | client.go:106-112; engine.go:132-140 |
| Status / health | collected | client.go:114-115; engine.go:144-151 |
| Uptime | derived from `StartedAt` | collectors.go:96-99 |
| CPU / memory usage | collected via one-shot stats | client.go:243-247; engine.go:312-341 |
| Network RX/TX bytes | collected | client.go:251-252; engine.go:347-350 |
| Restart count | collected | client.go:126; engine.go:153 |
| Ports/exposed ports | collected, unbound ports kept with empty host IP/port | client.go:164; engine.go:252-277 |
| Mounts | collected | client.go:162; engine.go:224-234 |
| Volumes | collected (count/size only, not per-mount) | client.go:260-265; engine.go:389-404 |
| Networks (name, IP, MAC, gateway, aliases) | collected | client.go:163; engine.go:236-250 |
| Container events | collected, streamed from Docker events API | client.go:268-279; engine.go:406-457; events.go |
| Container logs | transmitted, **on-demand only** | see below |
| Environment variables | **deliberately never collected** | client.go:136-141; engine.go:202-208 |
| Labels | collected in full; only a curated subset becomes a metric label | client.go:132; engine.go:138; collectors.go:24-33 |
| Command / entrypoint | collected | client.go:154-155; engine.go:203-204 |

### Container log streaming — what the guard string actually means

The string `"container log streaming is not authorized on this agent"` is a real, live authorization refusal inside a working, on-demand log-streaming feature — **not a stub, not dead code.**

```go
// internal/core/transport/libp2ptransport/agentops.go:403
if !allowContainerLogs {
    _ = enc.Encode(AgentOpFrame{Type: "error", Reason: "container log streaming is not authorized on this agent"})
    return
}
```

Full chain, confirmed hop-by-hop: Docker daemon (`ContainerLogs` API) → Agent's `docker.Client.Logs` (engine.go:459-561) → Agent's AgentOps stream handler encodes each line as `AgentOpFrame{Type:"line", Message:…}` onto a TLS-wrapped libp2p stream (agentops.go:437-469) → control plane's `fleetPipeline.ContainerLogs` decodes frames back into log lines (internal/app/fleet.go:255-342) → forwarded over a WebSocket to the operator's browser (internal/api/v1/containers.go:439-548).

Two independent, stacked authorization gates protect this path:

- **Agent-local opt-out** — `AgentOpsContainerLogsDisabled` env var (internal/agent/config.go:48-56, 77, 167). This produces the exact refusal string when set.
- **Control-plane grant** — a per-node `GrantStore` checked before the control plane even opens a request to the agent (internal/core/fleet/grants.go:16-28; check site internal/app/fleet.go:277-290); granted by default at enrollment, independently revocable.

By design this is narrow and single-purpose (docstring, agentops.go:30-34: *"is, deliberately, the only operation this protocol implements"*). Logs are **never** part of routine telemetry or a background pipeline, never stored, and only flow while an operator is actively viewing them — capped at a 6-hour session (agentops.go:109). Runs over the libp2p transport specifically (see §12 for libp2p's overall wiring status).

### Environment variables — explicit non-collection

Container environment variables are never read. Stated as policy in three places; `Config.Env` is simply never dereferenced anywhere in `internal/plugin/docker/`:

```go
// internal/plugin/docker/client.go:136-141
// ContainerDetail is one container's configuration, read on demand.
//
// Environment variables are deliberately absent and will stay absent. They
// routinely carry database passwords, API tokens and signing keys; a
// monitoring tool that reads them turns every dashboard viewer into a holder
// of every secret on the host. The same decision governs process environments
// elsewhere in Atlas. This is a boundary, not a gap.
```

---

## 8. Port information

Collector: `internal/plugin/ports/gopsutil.go` + `tls.go`. **The only place listening sockets are gathered anywhere in the agent.**

| Field | Status | Source | Reference |
|---|---|---|---|
| Protocol (tcp/udp) | collected | derived from socket type (SOCK_STREAM/SOCK_DGRAM) | gopsutil.go:88-96 |
| Bind address | collected | `Laddr.IP`, wildcard normalised to `0.0.0.0` | gopsutil.go:57 |
| Port number | collected | `Laddr.Port` | gopsutil.go:68 |
| Owning PID/process name | collected | `c.Pid` resolved via `gopsproc.NewProcessWithContext` | gopsutil.go:69-70 |
| Port state | collected | only TCP `LISTEN`-state sockets kept for TCP; all bound sockets kept for UDP | gopsutil.go:28 |
| TLS certificate info | collected | live handshake per open port: subject, issuer, SANs, not-before/not-after, self-signed flag | tls.go:55-86 |
| Certificate expiration | collected | `NotAfter` from the live handshake above | tls.go:55-86 |

Confirms §2: the system/network collector does not independently gather listening ports — this plugin is the sole source. TLS dial uses `InsecureSkipVerify:true` because the point is to read the certificate, not validate the chain (tls.go:60).

---

## 9. Telemetry

"Telemetry" is `transport.KindMetrics` — a wrapped `collect.Batch`, class `ClassStream` (internal/core/transport/payload.go:30-98). No separate "telemetry struct" exists distinct from the metrics payload; every numeric time-series sample from every plugin above rides in this envelope.

| Metric family | Source collector | Interval | Push/stream |
|---|---|---|---|
| cpu.*, memory.*, load.*, swap.* | internal/plugin/system | 15s default | Push, spooled + retried on failure |
| diskio.*, disk usage | internal/plugin/system | 15s default | Push, spooled + retried |
| network I/O counters | internal/plugin/system | 15s default | Push, spooled + retried |
| process.top.cpu / process.top.memory | internal/plugin/process | 15s default | Push, spooled + retried |
| service.up/.restarts/.enabled/.uptime/.memory/.count/.total/.failed | internal/plugin/service | 15s default | Push, spooled + retried |
| cron job counts (source/root only) | internal/plugin/cron | 15s default | Push, spooled + retried |
| Docker container stats | internal/plugin/docker | 15s default | Push, spooled + retried |

Interval default: `ATLAS_AGENT_COLLECTION_INTERVAL`, default 15s (config.go:74). Destination: agent-initiated HTTPS+mTLS POST to `/api/v1/agent/telemetry` (remote/remote.go:101,246). Storage: appended to the `metric_samples` TimescaleDB hypertable (§14) — historical, not latest-only.

---

## 10. Inventory

Inventory is `transport.KindInventory`, class `ClassSnapshot` (internal/core/inventory/payload.go:13-18): `Subject`, `ObservedAt`, `ContentHash`, `Data json.RawMessage`.

| Subject | Source plugin |
|---|---|
| processes | internal/plugin/process |
| services | internal/plugin/service |
| service_graph | internal/plugin/service |
| cron_jobs | internal/plugin/cron |
| ports | internal/plugin/ports |
| mounts | internal/plugin/system |
| containers | internal/plugin/docker |

- Destination: `POST /api/v1/agent/telemetry` (same route as metrics, routed by envelope kind).
- Persistence: `inventory_snapshots` table, keyed `(node_id, subject)` — **upsert, latest-only** (§14).
- Collection interval: 60s default (`ATLAS_AGENT_INVENTORY_INTERVAL`, config.go:76; ticker internal/agent/inventory.go:103).
- Per-subject SHA-256 content-hash skip if unchanged since last push (inventory.go:134-140).
- Snapshot-class envelopes are sent immediately and **dropped, not spooled**, on delivery failure (remote.go:121-137) — see the fanout caveat in §12.

---

## 11. Agent health / connection reporting

**There is no dedicated agent health/status payload.** This is the most significant structural gap found in the audit.

| Signal | Status | Evidence |
|---|---|---|
| Connected/disconnected | **not transmitted** | server infers liveness only from telemetry receipt timestamps (internal/app/health.go:73-82) — no explicit agent-pushed status exists |
| Heartbeat endpoint | **unused** | `POST /api/v1/agent/heartbeat` exists server-side (internal/api/agent/handler.go:72,265-277) but zero call sites from the agent |
| Delivery stats (sent/failed/rejected/spooled) | collected, never read or sent | `remote.Transport.Stats()` and `spool.Spool.Dropped()` exist as in-process counters with zero callers in internal/agent |
| Retries | happens, not reported | spool replay loop retries stream-class envelopes (remote.go:139-238) but counts never leave the process |
| Relay/transport status | local log only | dial-path decisions logged via slog (agent.go:290-323) |
| libp2p peer ID | local log only | agent.go:306-308, 318-320 |
| Certificate status/not-after | local log only | credentials.go:264 |
| Enrollment/renewal status | local log only | credentials.go:376 |
| Spool/backlog depth | **not transmitted** | never placed on an Envelope/Origin |
| Last successful delivery | **not transmitted** | no such field anywhere |

"Local log only" means: visible in the agent's own stdout/structured log stream on the host it runs on, invisible to the control plane and UI. The control plane's only signal about agent health today is indirect — whether telemetry/inventory keeps arriving on schedule.

---

## 12. Multi-control-plane support (`ATLAS_AGENT_RELATIONSHIPS`)

| Question | Answer | Reference |
|---|---|---|
| Same inventory sent independently to every relationship? | **Yes** — one `fanoutTransport` wraps every relationship's transport; each posts the full payload to its own `BaseURL` | agent.go:155-159,190-192,216-218; fanout.go:34-63 |
| Same telemetry sent independently to every relationship? | **Yes** — same fanout path | fanout.go:34-63 |
| Independent certificates per relationship? | **Yes** | credentials.go:179-271 |
| Independent spool/backlog per relationship? | **Yes** — own disk-backed spool under isolated data dir (`DataDir/relationships/<id>`) | agent.go:338-354; relationship.go:52-57 |
| Does one relationship's failure affect another? | **No** — bootstrapped concurrently, failed ones dropped without failing the process unless all fail; fanout only errors if every target fails | agent.go:60-65,240-276; fanout.go:59-62 |
| Local-dev + production Atlas simultaneously? | **Yes**, architecturally supported, unlimited relationship count | config.go:122-140 |

**Finding — inventory dedup cache is global, not per-relationship.** The content-hash "skip if unchanged" cache in `inventoryPusher` (inventory.go:89-91) is keyed only by subject, shared across all relationships. Because inventory is snapshot-class and dropped (not spooled) on a failed POST, and `fanoutTransport.Send` reports success if *any* target accepted the envelope, a scenario exists where: relationship A's POST succeeds, relationship B's fails, and the pusher still marks the subject as "sent" — relationship B does not get that subject retried until the underlying data next actually changes. This can leave one control plane with indefinitely stale inventory for a subject while another has current data, with no per-relationship retry. Metrics/events don't have this problem — each relationship's own spool retries independently.

Also confirmed: `Origin.Environment` is global, not per-relationship — a single environment tag shared across every relationship's envelopes, by explicit design comment (agent.go:187-189: "not per-relationship (Phase 3 scope decision)").

**libp2p transport status:** `docs/context/IMPLEMENTATION_CONTEXT.md` marks libp2p as "⏳ deferred," but the code contradicts that status — `internal/core/transport/libp2ptransport/libp2ptransport.go` (451 lines) is fully implemented and actively wired into `internal/agent/agent.go`, `internal/agent/discovery.go`, `internal/app/fleet.go`, and `internal/relay/relay.go`. Selectable via `ATLAS_AGENT_TRANSPORT=libp2p` (config.go:70,25). Code comments call it a "POC" (config.go:22-24; libp2ptransport.go:1-11) — functional but explicitly not production-hardened. This is a documentation/status discrepancy worth flagging separately from the data-path findings above.

---

## 13. Data flow

```
HOST
 |
 +-- system collector      (host/cpu/mem/disk/net counters)
 +-- process collector
 +-- service collector     (systemd, incl. dependency graph)
 +-- cron collector
 +-- docker collector
 +-- ports collector       (+ live TLS probe)
 |   [no host-NIC IP/MAC/gateway/DNS collector exists]
 |
 +--> Inventory payload  (ClassSnapshot, 60s, hash-deduped, drop-on-fail)
 +--> Telemetry payload  (ClassStream,  15s, spooled + retried)
 +--> Events             (forwarded live, spooled + retried)
 +--> AgentOps (container logs) -- on-demand only, libp2p transport only
 |
 +--> fanoutTransport (parallel per relationship, isolated failure)
       |
       +--> Relationship A (own cert + spool) --> local-dev Atlas
       +--> Relationship B (own cert + spool) --> production Atlas
       +--> Relationship N... (unlimited count)
                |
                v
        POST /api/v1/agent/telemetry
                |
                v
        Router (dispatch by Envelope.Kind())
           |                    |
           v                    v
   metric.Sink.Receive   inventory.Receiver.Receive
           |                    |
           v                    v
   metric_samples          inventory_snapshots
   (hypertable, append)    (upsert, latest-only)
           |                    |
           +--------+  +--------+
                    v  v
                 Atlas UI (React)
```

---

## 14. Server-side API, storage, and UI exposure

Single ingress route for all agent-pushed data: `POST /api/v1/agent/telemetry` → `Handler.Telemetry` (internal/api/agent/handler.go:71,194-255). Accepts `{Envelopes []transport.Envelope}`; each envelope's identity is rebound to the mTLS peer certificate (`env.Origin.NodeID = nodeID`, handler.go:224) before dispatch through `Router.Receive`, which fans out by `Envelope.Kind()` to `metric.Sink.Receive` or `inventory.Receiver.Receive`. Enroll/renew/heartbeat are separate routes and carry no telemetry.

| Category | Collected | Persisted | UI-visible |
|---|---|---|---|
| Host/system | yes | `nodes` table columns (upsert) | yes — NodeInspector, NodesPage, FleetDistribution |
| Network (host) | no | — | — |
| Storage/disk | yes (subject `mounts`) | `inventory_snapshots` | yes — DisksPage |
| Process | yes (subject `processes`) | `inventory_snapshots` | yes — ProcessesPage / ProcessInspector |
| Service | yes (subject `services`) | `inventory_snapshots` | yes — ServicesPage / DependencyGraph |
| Cron | yes (subject `cron_jobs`) | `inventory_snapshots` | yes — CronPage |
| Docker/container | yes (subject `containers`) | `inventory_snapshots` (remote) / live Docker read (local) | partial — mac_address/gateway fetched, never rendered |
| Ports | yes (subject `ports`) | `inventory_snapshots` | yes — PortsPage (port, protocol, address, TLS posture, owning process) |
| Metrics (all) | yes | `metric_samples` hypertable + 1m/1h continuous aggregates | yes — chart components |

**Schema confirmation:** `inventory_snapshots(node_id, subject, observed_at, received_at, content_hash, data jsonb, PRIMARY KEY(node_id, subject))` — a plain table, `ON CONFLICT (node_id, subject) DO UPDATE`, confirming ARCHITECTURAL_CONSTRAINTS.md's "Latest Only" claim in actual code (migrations/0004_fleet.sql:150-167; internal/storage/inventory/repository.go:37-54). `metric_samples` is a real TimescaleDB hypertable (`create_hypertable`, 7-day chunks, 30-day retention, compression policy) with insert-append writes (migrations/0002_metric_storage.sql:56-89; internal/storage/metric/sink.go:68).

No inventory *category* is collected-but-unpersisted. The one confirmed gap is field-level: container `mac_address`/`gateway` reach the API response (internal/api/v1/containers.go:278-283, typed at web/src/api/types.ts:314-321) and are simply never read by any React component.

---

## 15. Decision-making gap analysis

| Class | Meaning | Examples in current implementation |
|---|---|---|
| A | Already available and useful | CPU/mem/load/swap time series; disk usage %/inode %; per-process CPU/RSS top-N; systemd failed/restart/enabled + live dependency graph; container state/health/restart-count/CPU/mem; listening ports with live TLS cert + expiry |
| B | Collected but not exposed/stored properly | Container mac_address/gateway (fetched, never rendered); systemd cgroup CPU seconds (parsed, never emitted); transport delivery stats (never read); agent's own cert/enrollment/spool state (log-only) |
| C | Collected but insufficiently detailed | Disk I/O utilization is a rate-derived approximation, not true device utilization; container "uptime" is derived, not a monitored liveness signal; interface counters have no up/down state alongside them |
| D | Not currently collected | Host IPv4/IPv6, MAC, gateway, DNS servers, public IP, subnet/prefix, connectivity/reachability; CPU model string; manufacturer/model/DMI; timezone; process open files/connections; explicit agent health payload; heartbeat (endpoint exists, unused) |
| E | Should NOT be collected (security/privacy) | Container environment variables — explicitly, permanently excluded by design (§7); process/container secrets in raw form generally — stance is redact-by-omission for env vars, not for command lines (§16) |

---

## 16. Security review

**Confirmed: does transmit**
- Process command lines verbatim (up to 512 chars) — includes any credentials passed as CLI flags. Reaches the inventory API (internal/api/v1/inventory.go:30,69).
- Cron job command strings verbatim (up to 300 chars) — same risk, same lack of redaction, reaches inventory API.
- Container log line content — but only on-demand, operator-initiated, dual-gated (agent opt-out + control-plane grant), 6-hour session cap, never stored, never routine (§7). If an application logs a secret to stdout, that line can be viewed live by an authorized operator.

**Confirmed: does NOT transmit**
- Container environment variables — never read at all, by explicit, documented design (§7).
- Raw `/etc/machine-id` — only an HMAC-SHA256 derivative ever leaves the hashing function (hostid.go:251-254).
- Process environment variables (host processes) — same stated policy as container env vars (docker/client.go:140: "The same decision governs process environments elsewhere in Atlas").
- Private keys — TLS client keys are used locally for mTLS and never serialized into any payload; not part of any collector.

**Net assessment:** Atlas does not accidentally leak secrets through a bug — the two real exposure paths (process `Cmdline`, cron `Command`) are deliberate, documented trade-offs of collecting operationally useful data, and both are explicitly commented in source as known risk. Neither is redacted by pattern-matching (e.g. stripping `-p`/`--password`/`--token` style flags). Container logs are the one path capable of carrying arbitrary application-level PII or secrets, but it is opt-in-by-default-grant, revocable, and on-demand rather than continuously harvested.

---

## 17. Final summary

### A. Complete capability matrix

| Category | Collected? | Sent? | Stored? | UI-visible? | Section |
|---|---|---|---|---|---|
| Host/system | yes | yes | yes (nodes) | yes | §1 |
| Network (host) | no | — | — | — | §2 |
| Listening ports + TLS | yes | yes | yes | yes | §8 |
| Storage/disk | yes | yes | yes | yes | §3 |
| Process | yes | yes | yes | yes | §4 |
| Service (systemd) | yes | yes | yes | yes | §5 |
| Cron | yes | yes | yes | yes | §6 |
| Docker/containers | yes | yes | yes | partial | §7 |
| Container logs | on-demand | on-demand | no | live view only | §7 |
| Agent health/connection | partial (local) | no | no | no | §11 |

### B. Confirmed inventory/telemetry categories

**Telemetry** (metrics, 15s, ClassStream, spooled+retried): system/cpu/memory/load/swap, diskio, network counters, process top-N, service samples, cron counts, container stats.

**Inventory** (60s, ClassSnapshot, hash-deduped, dropped-not-spooled on failure): `processes`, `services`, `service_graph`, `cron_jobs`, `ports`, `mounts`, `containers`.

### C. Network/IP capability — exact answer

| Item | Status |
|---|---|
| Private IPv4 | Not available — no collection code exists |
| Public IPv4 | Not available — no collection code exists |
| IPv6 | Not available — no collection code exists |
| MAC address | Not available for host; collected for Docker containers but not rendered in UI |
| Gateway | Not available for host; collected for Docker containers but not rendered in UI |
| DNS servers | Not available — no resolver-config collection exists |
| Interface stats (RX/TX/errors/drops) | Available — collected, sent, stored, chartable |
| Listening ports (TCP+UDP, bind address, owning process, TLS cert) | Available — fully collected, sent, stored, displayed |

### D. Multi-Atlas capability

Confirmed: one agent can send the same host's full inventory and telemetry independently and simultaneously to a local development Atlas and a production Atlas (or any number of additional control planes) via `ATLAS_AGENT_RELATIONSHIPS`. Each relationship has its own certificate, transport, and spool; failures are isolated per relationship. One caveat (§12): the inventory change-detection cache is shared, so a partial delivery failure to one relationship among several successes can delay that relationship's next inventory refresh for an affected subject until the underlying data changes again.

### E. Decision-making readiness: **55%**

Strong on host/process/service/container resource and state signals. Structurally blind on network reachability/identity and on the agent's own operational health — both are common inputs to "is this thing actually okay" decisions.

Reasoning: categories in class A (host, disk, process, service, container, ports+TLS) cover the classic "is it overloaded / full / down / expiring" questions well and are end-to-end wired (collected → sent → stored → displayed). What caps the score at roughly half:
1. Zero network-identity/reachability data means Atlas cannot itself answer "is this server reachable" or correlate an alert with a specific IP.
2. The agent cannot report its own health to the control plane, so "is monitoring itself working" is inferred indirectly from staleness rather than known directly.
3. A few already-collected signals (container mac/gateway, systemd cgroup CPU) stop short of the UI.

### F. Top 10 missing data points (highest value first)

| # | Data point | Why it matters |
|---|---|---|
| 1 | Agent health payload (connected, spool depth, last successful delivery, cert expiry) | "Is monitoring itself broken" is currently invisible — largest blind spot for trusting any other metric |
| 2 | Host network interface IPv4/IPv6 + up/down state | Needed to answer "is this server reachable" and correlate alerts/incidents with an actual address |
| 3 | Default gateway + DNS resolver config (host) | Common root cause of "server unreachable"/"DNS resolution failing" incidents; currently undiagnosable from Atlas data alone |
| 4 | Public IP (if egress-routed) | Useful for fleet-wide network-path/geography correlation and allowlist auditing |
| 5 | Network connectivity/reachability probe | Direct answer to "is the server reachable," rather than an inference from missing telemetry |
| 6 | Render already-collected container mac_address/gateway in UI | Zero new collection work — pure UI gap, cheapest item on this list |
| 7 | Wire already-collected systemd cgroup CPU seconds into a metric | Also zero new collection work; closes a service resource-usage blind spot |
| 8 | CPU model string | Useful for fleet inventory/capacity-planning classification, cheap via existing gopsutil `cpu.Info` |
| 9 | Process open file/socket counts | Early indicator of fd-leak or connection-exhaustion incidents on a specific process |
| 10 | Per-relationship inventory delivery retry (fix the dedup-cache gap in §12) | Correctness fix: prevents one control plane from silently going stale relative to another |

### G. Methodology

Five independent full-file source reads covered: (1) system/network/storage/process collectors, (2) Docker plugin including the container-log authorization path, (3) service/cron plugins, (4) inventory/telemetry payload assembly, transport, and multi-relationship fan-out, (5) server-side API routing, Postgres/Timescale schema, and web UI rendering. The most security-relevant claims — the container-log authorization string, process `Cmdline` capture, the Docker env-var exclusion comment, and the inventory upsert SQL — were independently re-read and confirmed against the working tree before inclusion in this report.
