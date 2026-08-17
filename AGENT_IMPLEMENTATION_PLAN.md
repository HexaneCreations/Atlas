# Atlas Agent — Production Implementation Plan (Phase 1 output)

No source modified to produce this document. Built on top of `AGENT_DATA_AUDIT.md` (data trace) and `AGENT_PRODUCTION_READINESS_AUDIT.md` (production-readiness trace), both re-verified as current against HEAD `b3138f7`. This document adds the one gap neither prior audit covered: **single-instance protection (Critical Requirement #2)**, confirmed completely absent from the codebase by direct grep (`flock`, `LOCK_EX`, `pidfile`, `already running` — zero hits repo-wide). Everything else below is a synthesis/reprioritization of the two existing audits against the goal document's requirements, not a re-derivation — file:line citations point back to those audits or to source directly re-checked in this pass.

---

## 1. Current implementation (baseline, confirmed)

- Agent (`internal/agent`) is a multi-relationship, dial-only client. One process, one shared libp2p host (if any relationship uses `transport=libp2p`), N independently-certed/spooled/transported relationships fanned out via `fanoutTransport`.
- Transport: HTTPS+mTLS (default) or libp2p (dial-only; TCP/Noise handshake, then app-level mTLS runs inside the stream). Relay is a pure circuit-relay-v2 + rendezvous bootstrap, no auth/business logic.
- Collectors: `system`, `docker`, `process`, `service` (systemd), `cron`, `ports` — compiled-in, self-detecting, per-plugin failure isolation (`internal/core/plugin`).
- Data model: `transport.Envelope{ID, Origin, Payload, SentAt}`; `Payload` is either `ClassStream` (metrics/events, spooled+retried) or `ClassSnapshot` (inventory, dropped-not-spooled, hash-deduped).
- Server ingest: single route `POST /api/v1/agent/telemetry` → `Handler.Telemetry` → `Router.Receive` → `metric.Sink` / `inventory.Receiver`.
- **No single-instance protection exists at any layer** — not in `cmd/atlas-agent/main.go`, not in `internal/agent/agent.go`, not via the systemd unit (which only prevents systemd from double-starting its own unit — a concurrent manual launch is not blocked). Confirmed: `syscall.Flock`/`unix.Flock`/pidfile pattern appears nowhere in the repo.
- `golang.org/x/sys` is already present as an **indirect** dependency (`go.mod`) — promoting it to direct for the lock implementation requires no new dependency addition. Go's stdlib `syscall` package also exposes `Flock` directly on both linux and darwin (the two supported OS families per `AGENT_README.md`), so the lock can be built with **zero new dependencies** either way.

---

## 2. Gap categorization

Priorities below merge both prior audits' P0/P1/P2/P3 lists with the goal document's explicit Critical Requirements. Where a prior audit already assigned a priority, it is kept unless the goal document's explicit requirements justify raising it (Single-Instance is raised to P0 — it is Critical Requirement #2, verbatim-specified, and currently 0% implemented).

### P0 — production blockers

| # | Item | Current state | Files/functions | Source |
|---|---|---|---|---|
| 1 | **Single-instance lock** | Absent entirely | New: `internal/platform/lock/lock.go`; wired into `cmd/atlas-agent/main.go:run()` before `agent.New` | This audit — see §3 |
| 2 | Agent listener hardening (body cap, panic recovery, timeout, security headers) | `fleet.server` bypasses `BaseMiddleware` entirely — zero middleware chain on the only listener reachable by every enrolled agent | `internal/app/fleet.go:117-161`; `internal/platform/httpx/server.go` | `AGENT_PRODUCTION_READINESS_AUDIT.md` Part 5 #1, Part 8 |
| 3 | Envelope/Payload validation not wired into network path | `Validate()` exists, tested, but `Handler.Telemetry` never calls it | `internal/api/agent/handler.go:194-255`; `internal/core/transport/transport.go:108-116` | same, Part 5 #8 |
| 4 | Host network-interface identity collector missing | Zero IPv4/IPv6/MAC/gateway/DNS/up-down for the host anywhere in the codebase | New: `internal/plugin/system/collector_network_identity.go` (or extend `collector_network.go`); `internal/core/inventory/snapshot.go` (new subject) | `AGENT_DATA_AUDIT.md` §2, §11; `AGENT_PRODUCTION_READINESS_AUDIT.md` Part 8 |
| 5 | Agent self-health payload not transmitted | `remote.Transport.Stats()`, `spool.Spool.Dropped()`, cert expiry, relationship status all exist in-process, zero callers push them to the control plane | New: `internal/agent/health.go` or extend `agentops.go`; `internal/core/transport/payload.go` (new Kind); server: `internal/api/agent/handler.go`, new storage table/columns | `AGENT_DATA_AUDIT.md` §11 ("most significant structural gap found"); goal doc §10 |

### P1 — production important

| # | Item | Files | Source |
|---|---|---|---|
| 6 | Per-relationship inventory dedup cache fix | `internal/agent/inventory.go:89-91` (`lastHash` keyed by subject only, shared across relationships) | Both audits, confirmed real; goal doc Critical Requirement #3 explicitly calls this out |
| 7 | Server-side backpressure computation | `internal/api/agent/handler.go:211,275` hardcodes `"ok"`; client-side `slow_down`/`pause` handling is dead code today | `AGENT_PRODUCTION_READINESS_AUDIT.md` Part 8 |
| 8 | Per-node rate limiting on `/telemetry`, `/heartbeat` | `internal/platform/httpx/middleware.go` (no limiter exists); `internal/app/fleet.go` | same |
| 9 | CA bundle required-or-explicit-opt-in, not silent TOFU default | `internal/agent/credentials.go:73-86`; `config.go` | same |
| 10 | Spool `.tmp` sweep leak | `internal/core/transport/spool/spool.go:95-133` (scan) doesn't match write-path temp files | same |
| 11 | Protocol-version enforcement on HTTPS path | `internal/api/agent/handler.go:34,166`; `internal/agent/credentials.go:35` (decoded, never checked) | same |
| 12 | CPU model/topology/virtualization detection | `internal/plugin/system/collector_host.go`, `gopsutil.go` | `AGENT_DATA_AUDIT.md` §1 |
| 13 | Default gateway + DNS resolver collection | New collector, `internal/plugin/system/` | same §2 |
| 14 | `atlas-agent` Makefile build target + ldflags stamping | `Makefile:11` only builds `atlas-server`; no ldflags version/commit/build-time for the agent binary | `AGENT_PRODUCTION_READINESS_AUDIT.md` Part 8 #10 |

### P2 — useful

15. Network connectivity/reachability probe. 16. SELinux/AppArmor enforcement mode. 17. Render already-collected container `mac_address`/`gateway` in `ContainerInspector.tsx`. 18. Wire already-parsed `Unit.CPUSeconds` into a `service.cpu` metric. 19. Process open-file/socket counts. 20. Public egress IP — **must** be server-observed from the connection's own source address, never a third-party lookup (explicit constraint in both the goal doc and the existing audit's "SHOULD NEVER" list). 21. Configurable pattern-based redaction for process `Cmdline`/cron `Command` (off by default, on in a hardened profile).

### P3 — future

22. On-disk state schema/version tagging (`relationship.json`, spool files). 23. Agent upgrade runbook. 24. Disk I/O latency. 25. Firewall enabled/disabled + rule count (count only, never full rule dump — explicit "SHOULD NEVER" constraint carried over from the existing audit).

### Explicitly SHOULD NEVER (carried forward, unchanged)

Container/process env var collection, arbitrary log file collection, arbitrary filesystem content reads, full firewall rule dumps, any control/mutation capability beyond the existing revocable container-log exception, relay making auth/identity decisions, third-party public-IP lookups.

---

## 3. Single-instance implementation (Critical Requirement #2 — full design)

### Mechanism

OS-level exclusive advisory lock via `flock(2)`, **not** a pidfile-existence check (explicitly disallowed — TOCTOU-unsafe). `LOCK_EX | LOCK_NB` is atomic at the kernel level: two processes racing to acquire the same fd-backed lock always resolve to exactly one winner, with no window for both to proceed.

- **Package:** new `internal/platform/lock/` (sits alongside `internal/platform/pki`, `internal/platform/hostid` — same layer, same convention).
- **API:**
  ```go
  func Acquire(path string) (*Lock, error)   // returns ErrHeld{PID int} if another holder is active
  func (l *Lock) Release() error             // closes fd; flock auto-releases on close or process exit/crash
  ```
- **Lock file location:** `<DataDir>/atlas-agent.lock`, mode `0600`. `DataDir` is the existing per-host-identity root (`ATLAS_AGENT_DATA_DIR`, default `/var/lib/atlas-agent`) — this makes the lock scope exactly match "the Agent data directory / host identity" as specified, and gives "different data directories run independently" for free (each gets its own lock file, no cross-talk).
- **Implementation:** `syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)`. On `EWOULDBLOCK`: read the file's existing contents (PID written by the current holder — see below) and return a typed error carrying that PID for the caller to format. Uses Go's stdlib `syscall` package (available on both linux and darwin; no new dependency).
- **Holder identification:** immediately after acquiring, truncate-and-write the current PID (`os.Getpid()`) and start time to the file, then `fsync`. This is diagnostic only — exclusivity itself comes from the `flock`, not from this content — but it's what lets a rejected second instance print `pid: 12345` per the goal doc's example.
- **When acquired:** first action in `cmd/atlas-agent/main.go:run()`, immediately after `os.MkdirAll(cfg.DataDir, 0o700)` and strictly before `agent.New(...)` — i.e. before identity resolution, before any relationship bootstrap, before spool open, before collector init. This satisfies every "must be acquired before X" bullet in the goal doc in one call site, since `agent.New` is where all of those currently begin.
- **When released:** held via a live file descriptor for the entire process lifetime; explicit `Release()` deferred in `main()` on clean shutdown. Because `flock` is process-lifetime-bound at the kernel level, a crash (including `SIGKILL`, OOM-kill) releases it automatically with no cleanup code required — satisfies "released automatically when the process exits/crashes" and "not depend on a specific PID" (the kernel tracks the open file description, not a PID value checked by user code).
- **Lock file itself is never deleted** — deleting-and-recreating a lock file is a classic reintroduction of the exact TOCTOU race this mechanism exists to avoid (a second process could create-and-lock a fresh inode between the first process's unlink and its own exit). The file persists harmlessly across restarts; the next start just re-`flock`s the same inode.
- **systemd interaction:** requires no unit-file change. `Restart=always` already relies on the old process having fully exited before a new one starts, which is exactly when the kernel releases the flock — the new instance under systemd acquires cleanly. A simultaneous `systemctl start atlas-agent` + manual `/usr/local/bin/atlas-agent` resolves correctly: whichever wins the race holds the lock; the other exits immediately with the clear error and non-zero status (systemd will log the failed manual attempt's exit code, not retry it, since only the *systemd-managed* process is subject to `Restart=always`).
- **Error surface:** `main()`'s existing `fmt.Fprintf(os.Stderr, "atlas-agent: %v\n", err); os.Exit(1)` path already matches the goal doc's example output shape once `Acquire`'s error implements `Error() string` as:
  ```
  another agent instance is already running
  pid: 12345
  data_dir: /var/lib/atlas-agent
  ```

### Test plan (per goal doc's SINGLE INSTANCE section)

New `internal/platform/lock/lock_test.go`, integration-style (real files in `t.TempDir()`, no mocks — per `CLAUDE.md`/`ENGINEERING_GUIDE.md`):

- First `Acquire` on a fresh path succeeds.
- Second `Acquire` on the same path, same process, fails with `ErrHeld{PID: os.Getpid()}`.
- `Release()` then re-`Acquire()` succeeds (clean release path).
- Concurrent goroutines (simulating a startup race) calling `Acquire` on the same path: exactly one succeeds, table-driven over N=2..10 attempts, deterministic (`flock` guarantees this — the test is a regression guard, not a probabilistic check).
- Stale lock after crash: acquire, kill the holding process's fd out from under it via `f.Close()` without `Release()` semantics (simulating a crash that skips defer), confirm a fresh `Acquire` on the same path succeeds immediately (no manual cleanup needed).
- Two different `DataDir` paths acquire independently and simultaneously (multi-server / multi-data-dir simulation).
- End-to-end: spawn `atlas-agent` as a real subprocess (`os/exec`) against a temp `DataDir` with no control-plane reachable (bootstrap will retry/fail, that's fine — the lock check must happen before bootstrap), spawn a second subprocess against the *same* `DataDir`, assert the second exits non-zero with the expected stderr shape within a few seconds.

---

## 4. Schema changes

- `internal/core/transport/payload.go` — new `Kind` for the self-health payload (Phase 1 of the production-readiness audit's own Part 10 already scopes this; reuse that ordering).
- `internal/core/inventory/snapshot.go` — new `Subject` for host network identity (`network_identity` or similar), following the existing subject-registry pattern (`processes`, `services`, `mounts`, etc.) — no wire-incompatible change, purely additive.
- Server-side: new migration under `migrations/` only if self-health lands as new `nodes` columns rather than a JSON blob on an existing table — decide the exact shape before collector work starts (per the existing audit's Phase 1 ordering), not decided in this document.

## 5. Collector changes

Host network-interface collector, gateway/DNS collector, CPU model/topology/virtualization detector — all follow the existing `internal/core/plugin` four-stage lifecycle (`Detect`/`Init`/`Collect or Inventory`/`Close`) already used by `system`/`service`/`cron`/`ports`. No new collector pattern needed; reuse `internal/plugin/system`'s existing structure. Every new collector must degrade to "not detected" rather than error on a host missing the underlying facility (containers without `/sys` access, hosts without a default route, etc.) — this is the existing, already-tested behavior for every current collector and must not regress.

## 6. Transport changes

Listener hardening (P0 #2) and validation wiring (P0 #3) both apply to `internal/app/fleet.go`'s agent-facing `mux` — reuse `internal/platform/httpx.BaseMiddleware` (already built and used by the browser-facing API server, `internal/api/router.go:98`) rather than inventing a second middleware chain; if the agent listener needs different limits (larger body cap for batched telemetry vs. UI requests) that's a parameter to the existing middleware, not a new implementation.

## 7. Relationship changes

Dedup-cache fix (P1 #6): move `inventoryPusher.lastHash` from a single `map[Subject]string` to being keyed or scoped per-relationship, matching how spool/cert/transport are already isolated. `Origin.Environment` remains global by explicit existing design decision (documented as an accepted Phase-3 scope cut, not a bug) — do not change this without an explicit product decision, since it's a wire-format question, not a bugfix.

## 8. Security changes

TOFU-by-default → required-or-opt-in CA bundle (P1 #9); per-node rate limiting (P1 #8); optional pattern-based redaction for `Cmdline`/`Command` (P2 #21, off by default to preserve current diagnostic value — this is a configuration addition, not a removal of existing observability, consistent with the goal doc's "never blindly delete observability categories" instruction).

## 9. Migration concerns

- New inventory subject and payload kind are additive — old agents talking to a new server, and new agents talking to an old server, both continue to function (unknown subjects/kinds are simply not sent/not understood, no breaking change).
- Self-health payload's exact storage shape (new `nodes` columns vs. new table) must be decided before Phase 2 collector work starts, per the existing audit's own Phase 1 note — this is one of the few remaining open questions in this plan.
- Single-instance lock is purely additive to process startup — no wire/schema impact, no migration required. The only behavior change existing deployments will observe is that a previously-possible (accidental) double-launch now fails fast instead of silently corrupting state — this is the intended fix, not a regression.

## 10. Deployment concerns

- Single-instance lock requires no packaging change — `install.sh`/systemd unit already point at one `DataDir`; the lock file is created there automatically on first start.
- `atlas-agent` Makefile target (P1 #14) is a prerequisite for shipping a version-stamped binary fleet-wide; currently agent builds are unstamped/manual, which undermines "one stable binary, many servers" traceability.
- Canary rollout (per existing audit Part 10 Phase 10): verify on both Linux-systemd and Mac-dev-native targets before broad rollout, specifically re-verifying that the new lock does not interfere with the documented `Restart=always`/`RestartSec=5s` crash-recovery loop.

---

## 11. Open questions requiring explicit approval before Phase 2

1. Self-health payload storage shape (new `nodes` columns vs. new table) — architectural/schema decision.
2. Whether `Origin.Environment` becomes per-relationship — deferred by existing design comment as a Phase-3 scope cut; changing it now is a wire-format decision, not in scope of this pass unless explicitly requested.
3. Redaction-by-default policy for `Cmdline`/`Command` — security posture decision (default off, hardened-profile on, per existing audit recommendation) needs explicit sign-off since it changes what a hardened deployment sees by default.

Everything else in P0/P1 is mechanical, already fully scoped by file/function, and ready to implement without further architectural input.
