# Atlas Agent — Local/Repository-Level Verification (Phase 3, Part 1)

Scope: everything testable on this machine, with no access to Production Atlas, cyrene-dev-v2, or the real MacBook Atlas deployment. No live infrastructure was touched. All commands below were run against this checkout at HEAD (`b3138f7` + the Phase 2 changes), on macOS/arm64, Go 1.25.7.

Legend: **PASS** / **FAIL** / **NOT TESTED**.

Five new test files were added to produce direct, reproducible evidence for this report, alongside the tests already written during Phase 2 implementation. They are ordinary Go tests, live in-package (so they can reach unexported code the same way the existing suite does), and are listed per-section below. They do not touch or modify any production code path — every fix visible in this report was already made in Phase 2. Delete them freely if a permanent addition to the suite isn't wanted; every result below is reproducible by re-running the exact commands shown.

```
internal/agent/zz_localverify_multirel_test.go
internal/agent/zz_localverify_relfail_test.go
internal/agent/zz_localverify_health_test.go
internal/plugin/system/zz_localverify_collectors_test.go
internal/platform/redact/zz_localverify_examples_test.go
internal/platform/httpx/zz_localverify_listener_test.go
```

---

## 1. Build — PASS

```
go build ./...
go vet $(go list ./... | grep -v '/node_modules/')
make build-agent
make build-all
```

- `go build ./...` — exit 0, no output.
- `go vet` (all 45 non-vendor packages) — exit 0, no output.
- `make build-agent`:
  ```
  go build -trimpath -ldflags "-s -w -X '...build.Version=v1.0.8-dirty' -X '...build.Commit=b3138f74a39748f...' -X '...build.BuildTime=2026-08-14T12:13:24Z'" -o bin/atlas-agent ./cmd/atlas-agent
  built bin/atlas-agent (v1.0.8-dirty)
  ```
- `make build-all` built all three binaries (`atlas-server`, `atlas-agent`, `atlas-relay`) with identical ldflags stamping.
- Version stamping confirmed on the running binaries:
  ```
  $ ./bin/atlas-agent --version
  atlas v1.0.8-dirty (b3138f74a397-dirty) built 2026-08-14T12:13:26Z with go1.25.7 for darwin/arm64
  $ ./bin/atlas-server version
  atlas v1.0.8-dirty (b3138f74a397-dirty) built 2026-08-14T12:13:26Z with go1.25.7 for darwin/arm64
  $ ./bin/atlas-relay --version
  atlas v1.0.8-dirty (b3138f74a397-dirty) built 2026-08-14T12:13:26Z with go1.25.7 for darwin/arm64
  ```
  All three report identical version/commit/build-time — the `atlas-agent` build target added in Phase 2 stamps identically to `atlas-server`, closing the "unstamped agent binary" gap from the original production-readiness audit.

## 2. Test suite — PASS

```
go test -count=1 $(go list ./... | grep -v '/node_modules/')
```

**45/45 packages pass, 0 failures** (fresh run, `-count=1` to defeat caching). Full list of `ok` lines captured in this session's log; representative packages: `internal/agent`, `internal/api/agent`, `internal/app`, `internal/core/transport`, `internal/core/transport/{remote,spool,libp2ptransport}`, `internal/platform/{lock,httpx,redact,pki,config}`, `internal/plugin/{system,process,cron,service,ports,docker}`, `cmd/atlas-agent`.

```
go test -race -count=1 ./internal/platform/lock/... ./internal/agent/... \
  ./internal/core/transport/... ./internal/platform/httpx/... \
  ./internal/plugin/... ./internal/api/agent/...
```

**PASS** — all 14 concurrency-sensitive packages clean under `-race`, no data races reported.

## 3. Single-instance lock — PASS

Live demo against the real `bin/atlas-agent` binary (not just the unit tests), using a fresh temp data directory and an unreachable control plane so the first process blocks in its bootstrap retry loop rather than exiting:

```
DATADIR=$(mktemp -d)
ATLAS_AGENT_DATA_DIR="$DATADIR" ATLAS_AGENT_CONTROL_PLANE_URL="https://127.0.0.1:1" \
  ATLAS_AGENT_TOKEN="test-token" ./bin/atlas-agent &
FIRST_PID=$!
# ... wait for lock file to appear ...
ATLAS_AGENT_DATA_DIR="$DATADIR" ATLAS_AGENT_CONTROL_PLANE_URL="https://127.0.0.1:1" \
  ATLAS_AGENT_TOKEN="test-token" ./bin/atlas-agent   # second instance, same data dir
```

Evidence, exact captured output:

```
first pid=11904
lock file present: yes
11904
2026-08-14T12:18:15Z
=== second instance, same data dir ===
atlas-agent: another agent instance is already running
pid: 11904
data_dir: /var/folders/.../tmp.YkaMUxnfSL/atlas-agent.lock
second exit code: 1
=== first instance still running? ===
yes, still running
```

Then: killed the first process, confirmed it exited, and started a **third** instance against the same data directory:

```
first process gone, lock released by OS
=== fresh instance can now acquire the lock ===
third instance running fine, pid=12556 (lock reacquired after release)
{"...","msg":"identity resolved",...}
{"...","msg":"no existing certificate found; enrolling",...}
```

Checklist:
- First agent starts — **PASS**
- Second agent, same data dir, fails immediately with `pid:`/`data_dir:` message and exit code 1 — **PASS**
- First agent unaffected (still running, unblocked) — **PASS**
- Lock released after process exit (verified by successful reacquisition, not just absence of error) — **PASS**
- New agent starts afterward — **PASS**

Also covered by the existing automated suite:
- `internal/platform/lock/lock_test.go` — concurrent-acquire race (exactly one winner, N=2/5/10), stale-lock-after-crash (simulated via abrupt `fd.Close()`), independent lock paths for independent data directories.
- `cmd/atlas-agent/main_test.go::TestSecondInstanceRefusesToStart` — same scenario as above, built into the automated suite (`go test ./cmd/atlas-agent/... -run TestSecondInstance`), **PASS**.

## 4. Multi-relationship configuration — PASS

```
go test ./internal/agent/... -run TestLocalVerify -v
```

New test: `internal/agent/zz_localverify_multirel_test.go`. Configured `ATLAS_AGENT_RELATIONSHIPS=local,production` with distinct `CONTROL_PLANE_URL`, `TOKEN`, `ENVIRONMENT`, `TRANSPORT`, and (for production only) libp2p relay/peer-id fields, plus the legacy flat `ATLAS_AGENT_*` vars for the implicit `default` relationship.

Result: **PASS**. Captured field dump:

```
default:    {ControlPlaneURL:https://local-default:8443 ... Environment:default-env Transport:https}
local:      {ControlPlaneURL:https://local-atlas:8443 ... Environment:development Transport:https}
production: {ControlPlaneURL:https://prod-atlas:8443 ... Environment:production Transport:libp2p
             LibP2PRelayAddr:/ip4/198.51.100.1/tcp/4103/p2p/12D3KooWRelay LibP2PServerPeerID:12D3KooWProdServer}
```

Assertions verified:
- Both `local` and `production` parsed, plus the untouched `default` — **PASS**
- Relationship-specific fields isolated (production's libp2p relay/peer-id do **not** leak into `local`) — **PASS**
- Default relationship (legacy flat `ATLAS_AGENT_*` vars) still works unmodified — **PASS**
- Tokens differ between relationships (no accidental sharing) — **PASS**
- `ATLAS_AGENT_RELATIONSHIPS=default` is rejected outright (reserved name) — **PASS** (`TestLocalVerifyDefaultReservedNameRejected`)

## 5. Relationship failure isolation — PASS

Two layers of evidence, both directions.

**Config-resolution layer** (existing suite, `internal/agent/agent_test.go`):
```
go test ./internal/agent/... -run "TestNewHealthyProductionSurvivesCorruptedDevelopment|TestNewHealthyDevelopmentSurvivesCorruptedProduction|TestNewFailsWhenEveryRelationshipIsCorrupted" -v
```
All **PASS**. Case 1: healthy `production` + corrupted `development` → `Agent.New` succeeds, production available, development dropped. Case 2 (reversed): corrupted `production` + healthy `development` → mirror result. Case 3: everything corrupted → `New` fails outright (only case it should).

**Network/delivery layer** (new, real sockets — `internal/agent/zz_localverify_relfail_test.go`):
```
go test ./internal/agent/... -run TestLocalVerifyRelationshipIsolation -v
```
Two real transports wired through the actual `fanoutTransport`: one pointed at a real `httptest.Server` answering normally, the other at a `127.0.0.1` port bound-then-closed (guaranteed connection refused).

Case 1 — local healthy, production down:
```
local:      {Sent:2 Failed:0 ... LastFailure:0001-01-01 ...}
production: {Sent:0 Failed:1 Spooled:2 SpooledBytes:868 Retries:1
             LastFailureReason:Post "http://127.0.0.1:50847/...": dial tcp 127.0.0.1:50847: connect: connection refused}
```
Case 2 — reversed, production healthy, local down:
```
production: {Sent:2 Failed:0 ...}
local:      {Sent:0 Failed:1 Spooled:2 Retries:1 LastFailureReason: connection refused}
```

Both **PASS**. Healthy relationship: 100% delivered, zero failures, unaffected by its sibling. Down relationship: nothing lost (spooled, not dropped — it's stream-class), retried independently, correct failure reason recorded. Confirms the fanout/failure-isolation design end to end over real TCP, not just corrupted-config resolution.

## 6. Self-health — PASS

```
go test ./internal/agent/... -run "TestHealth|TestLocalVerifyHealthSnapshotFieldByField" -v
```

New test `internal/agent/zz_localverify_health_test.go` builds a health snapshot from a relationship with a **real issued X.509 leaf certificate** (via the same `pki.CA` the production code uses) and a **real libp2p Ed25519 Peer ID**, then dumps the full JSON:

```json
{
  "node_id": "node-1", "version": "dev", "commit": "unknown", "build_time": "unknown",
  "started_at": "...", "uptime_seconds": 300.000000125, "observed_at": "...",
  "single_instance_lock": "/var/lib/atlas-agent/atlas-agent.lock",
  "collectors": [
    {"id": "system", "status": "active"},
    {"id": "docker", "status": "not_detected"}
  ],
  "relationships": [
    {
      "id": "production", "environment": "production", "transport": "libp2p",
      "connected": false, "peer_id": "12D3KooWREZi46ZTRHmvnc8hRojdzP1rcGKygXj3Wx1NeqRyK233",
      "sent": 0, "failed": 0, "rejected": 0, "retries": 0,
      "spool_depth": 0, "spool_bytes": 0, "spool_dropped": 0,
      "certificate_expiry": "2026-08-15T12:22:46Z", "certificate_valid": true
    }
  ]
}
```

Every field from the goal checklist confirmed present, both as Go struct fields and — separately asserted — as actual JSON keys on the wire shape: connection state, transport, Peer ID, sent/failed/rejected/retries, spool depth/bytes, last success/failure+reason (see `TestHealthSnapshotCarriesNoPrivateKeyMaterial` and the `remote.Stats` test below for those two), certificate status/expiry, version/commit/build-time, uptime, collector outcomes. Also verified: **no private key material** ever appears in the marshaled payload (`TestHealthSnapshotCarriesNoPrivateKeyMaterial` greps for `PRIVATE KEY`/`private_key`/PEM headers — none found).

Keyed by node **and** relationship: `TestHealthReportsEveryRelationshipIndependently` builds a report with two relationships (`development`/`https`, `production`/`libp2p`) and confirms both appear with their own environment and transport — **PASS**.

`last_success`/`last_failure`/`last_failure_reason` end-to-end against a real HTTP round trip: `internal/core/transport/remote/remote_test.go::TestStatsRecordDeliveryOutcomes` (new, added during Phase 2) — sends successfully, checks `LastSuccess` set; forces the test server to fail, checks `LastFailure` + `LastFailureReason` set and the earlier success preserved. **PASS**.

## 7. Network collector — PASS (with one noted platform gap, see Findings)

```
go test ./internal/plugin/system/... -run TestLocalVerifyNetworkIdentityRealOutput -v
```

Real output from this machine (macOS/arm64), full JSON in the test log. Confirmed present: 21 real interfaces enumerated (`lo0`, `en0`...`en6`, `utun0-3`, `bridge0`, `awdl0`, etc.), each with name, up/loopback state, MAC (where applicable), MTU, IPv4/IPv6 in CIDR form, and raw flags (`up`, `broadcast`, `multicast`, `running`, `point-to-point`). Example: `en0: mac=2a:a7:1f:99:27:c6 mtu=1500 ipv4=[192.168.29.96/24]`. DNS servers populated (`192.168.29.1`, from `/etc/resolv.conf`).

- Interfaces — **PASS**
- IPv4 — **PASS** (`en0` → `192.168.29.96/24`)
- IPv6 — **PASS** (`lo0`, `awdl0`, `utun0-3` all report link-local IPv6)
- CIDR/prefix — **PASS** (every address carries its prefix length)
- MAC — **PASS**
- MTU — **PASS**
- Interface state (up/down) — **PASS**
- Default gateway — **NOT TESTED on this platform** — see Findings §A below; DNS-server/search-domain parsing is cross-platform and confirmed working, gateway parsing is Linux-only in the current implementation and was not exercised end-to-end on macOS. Linux gateway parsing (`/proc/net/route`, `/proc/net/ipv6_route`) is unit-tested directly against synthetic fixture files (`TestParseProcRouteFindsOnlyTheDefaultRoute`, `TestParseProcRoute6FindsDefaultRoute`) — **PASS** for the parsing logic itself, just not exercised against a real Linux `/proc` on this machine.
- DNS servers — **PASS**
- DNS search domains — **PASS logic**, empty on this host (this Mac's `/etc/resolv.conf` has no `search` line) — confirmed via the synthetic-fixture test `TestReadResolvConf`, **PASS**.

## 8. Host collector — PASS

```
go test ./internal/plugin/system/... -run TestLocalVerifyHostFactsRealOutput -v
```

Real output from this machine:

```json
{
  "hostname": "Shubhs-MacBook-Pro.local", "os": "darwin", "platform": "darwin",
  "platform_version": "15.3.1", "kernel_version": "24.3.0", "kernel_arch": "arm64",
  "boot_time": "2026-08-07T11:50:39+05:30", "logical_cores": 12, "physical_cores": 12,
  "cpu_model": "Apple M4 Pro", "timezone": "IST"
}
```
`uptime: 174h2m27s` (derived, matches `boot_time`).

- Hostname — **PASS**
- FQDN — **PASS (logic)**, empty on this host — normal (no resolvable domain from this Mac's network), confirmed the field exists and the resolver path runs without error.
- OS — **PASS** (`darwin`)
- Kernel — **PASS** (`24.3.0`)
- Architecture — **PASS** (`arm64`)
- CPU model — **PASS** (`Apple M4 Pro`, real `cpu.InfoWithContext` read)
- CPU topology (sockets/cores) — **PASS**, 12 logical/12 physical cores; `cpu_sockets` omitted (0) on this host — gopsutil reports no `PhysicalID` grouping on Apple Silicon, so socket count is legitimately unavailable here (best-effort field, documented as such in the code).
- Virtualization — **PASS (logic)**, empty — correct, this is bare-metal macOS, not a VM/container.
- Timezone — **PASS** (`IST`)
- Uptime — **PASS**

## 9. Secret redaction — PASS

```
go test ./internal/platform/redact/... -run TestLocalVerifyRedactionExamples -v
```

Exact examples from the checklist, input → output:

| Input | Output |
|---|---|
| `--password=secret123` | `--password=[REDACTED]` |
| `--token=abc123` | `--token=[REDACTED]` |
| `--api-key=abc123` | `--api-key=[REDACTED]` |
| `--secret=value` | `--secret=[REDACTED]` |
| `Authorization: Bearer abc123` | `Authorization: Bearer [REDACTED]` |
| `--port 8080` | `--port 8080` (unchanged) |
| `-Xmx4g` | `-Xmx4g` (unchanged) |
| `-jar app.jar` | `-jar app.jar` (unchanged) |

All 8 sub-tests **PASS**. Also covered by the broader `internal/platform/redact` suite (22 cases including `-pSECRET` MySQL short form, URL userinfo, quoted values, case-insensitivity) and by the collector-level tests proving redaction is wired in and **on by default**:
```
go test ./internal/plugin/process/... ./internal/plugin/cron/... -run Redact -v
```
`TestInventoryRedactsCommandLineSecretsByDefault`, `TestInventoryRedactsJobCommandSecretsByDefault`, and the explicit-opt-out tests `TestInventoryRedactionCanBeDisabledExplicitly` (both packages) — all **PASS**.

## 10. Envelope validation — PASS

```
go test ./internal/api/agent/... -run "TestTelemetryRejectsInvalidEnvelope|TestTelemetryRejectsMalformedEnvelopeBatch|TestTelemetryAcceptsWithinToleranceAndRecordsSkew|TestTelemetryBindsOriginToVerifiedIdentity" -v
```
All **PASS**. Malformed envelope (`"payload":null`) → HTTP 400, never reaches the receiver. Structurally valid but missing a required field (empty `Origin.Hostname`+no collector ID) → accepted at decode, rejected at `Envelope.Validate()`, reported as `invalid_envelope` in the response, never reaches the receiver. A well-formed envelope continues through the normal path and is recorded by the (fake) receiver, with `Origin.NodeID` correctly rebound to the verified peer identity.

## 11. Listener hardening — PASS

```
go test ./internal/platform/httpx/... -run TestLocalVerifyListenerHardeningOverRealSocket -v
```

New test `internal/platform/httpx/zz_localverify_listener_test.go` — runs `AgentMiddleware` behind a **real bound TCP listener**, driven by a real `net/http.Client` and one raw TCP connection (not an in-process handler call). All 6 sub-tests **PASS**:

- Valid request → 200 — **PASS**
- Oversized body (4096 bytes against a 1024-byte cap) → 413, connection stays usable — **PASS**
- Handler panic → 500, recovered, listener continues serving subsequent requests on the same process — **PASS**
- Slow handler (2s of work) against a 200ms `RequestTimeout` → cut off at ~200ms with a 504 — **PASS** (measured: 204ms)
- Malformed HTTP request line, written directly to the raw socket → `net/http`'s own parser returns `400 Bad Request` before any handler runs — **PASS**
- Rate limit: proven separately with real mTLS certificates (`TestPerNodeRateLimitBoundsOneNode`, `TestPerNodeRateLimitIsolatesNodes`, `TestPerNodeRateLimitPassesUnauthenticatedRequestsThrough` — all **PASS**, re-run for this report); this socket-level test additionally confirms an unauthenticated plain request is correctly *not* limited (limiting is keyed on the mTLS peer certificate, which a plain connection has none of).

## 12. Spool — PASS

```
go test ./internal/core/transport/spool/... -v
go test ./internal/core/transport/remote/... -run "TestOutageThenRecoveryReplaysSpooledEnvelopes|TestStreamEnvelopeIsSpooledAndDelivered|TestSnapshotEnvelopeFailureIsDroppedNotRetried|TestMultipleSendsBatchIntoOneRequest" -v
```

10/10 spool-package tests **PASS**: normal enqueue/peek/dequeue round-trip, FIFO order, overflow drops oldest-not-newest, reopen resumes queued entries after a simulated restart, reopen discards entries past `MaxAge`, **orphaned `.tmp` file sweep on reopen** (`TestReopenSweepsOrphanedTempFiles` — the Phase 2 fix, verified: an orphan file survives being written but is gone after `Open()`, and doesn't count toward `Len()`), batch peek/dequeue, dequeue-on-empty fails cleanly.

4/4 remote-transport tests **PASS**: stream-class envelope spooled then delivered; snapshot-class failure dropped (never retried, by design); outage-then-recovery replays everything that was spooled; multiple sends batch into one request.

## 13. Protocol version — PASS

```
go test ./internal/api/agent/... -run TestTelemetryRejectsProtocolVersionMismatch -v
```
**PASS**. Tested both `0` (unset) and `agent.ProtocolVersion + 1` (future version) — both rejected with HTTP 400, zero envelopes reach the receiver even though the batch also contained an otherwise-valid envelope. The matching, currently-supported version succeeds (proven by every other passing telemetry test, which all send `ProtocolVersion: agent.ProtocolVersion`).

## 14. Per-relationship inventory dedup — PASS

```
go test ./internal/agent/... -run TestInventoryPusherRetriesOnlyTheRelationshipThatFailed -v
```
**PASS**. Exact scenario requested: two relationships (`healthy`, `broken`) fed the identical unchanged inventory content.
1. First push: `healthy` accepts (1 envelope recorded), `broken` fails (its transport returns an error).
2. `broken` "recovers" (stops failing), content is pushed again **unchanged**.
3. Result: `broken` receives exactly 1 envelope on retry (it was correctly retried despite unchanged content, because its last *accepted* hash never advanced) — **PASS**. `healthy` receives 0 additional envelopes (correctly not re-sent, since it already has this exact content) — **PASS**.

This is the fix for the audit's "global dedup cache" finding: dedup state is now keyed per relationship, not globally, so one relationship's failure to accept a subject no longer silently marks it "delivered" for a sibling that never got it.

## 15. Environment isolation — PASS

```
go test ./internal/agent/... -run "TestFanoutStampsEachRelationshipsOwnEnvironment|TestInventoryPusherStampsPerRelationshipEnvironment" -v
```
Both **PASS**. `local`/`development` and `production`/`production` relationships, sharing one source envelope object, each observe their own `Origin.Environment` on the copy delivered to their transport; the original caller-held envelope is provably unmutated (`env.Origin.Environment != "unset"` check). Confirms one relationship cannot overwrite another's environment tag — each relationship's transport receives an independent copy, stamped in `fanoutTarget.send`. Also exercised at the config layer in §4 (`local` → `development`, `production` → `production`, both parsed and isolated correctly with no leakage).

---

## Summary table

| # | Area | Result |
|---|---|---|
| 1 | Build (go build/vet, make targets, version stamping) | PASS |
| 2 | Full test suite (45/45 packages) + race | PASS |
| 3 | Single-instance lock (live binary demo) | PASS |
| 4 | Multi-relationship config parsing/isolation | PASS |
| 5 | Relationship failure isolation, both directions | PASS |
| 6 | Self-health, all fields, per node+relationship | PASS |
| 7 | Network collector | PASS (gateway detection: Linux-only, see Findings) |
| 8 | Host collector | PASS |
| 9 | Secret redaction, spec examples | PASS |
| 10 | Envelope validation | PASS |
| 11 | Listener hardening, real socket | PASS |
| 12 | Spool | PASS |
| 13 | Protocol version enforcement | PASS |
| 14 | Per-relationship inventory dedup | PASS |
| 15 | Environment isolation | PASS |

**45/45 Go packages pass. 0 build failures. 0 vet issues. 0 race conditions detected in concurrency-sensitive packages.**

---

## Findings

**A. Gateway detection is Linux-only; macOS/BSD is a silent gap, not a crash.**

`internal/plugin/system/network_identity.go` reads `/proc/net/route` and `/proc/net/ipv6_route` for default-gateway detection. Those paths do not exist on macOS (confirmed on this machine: `Gateways` came back empty, while every other field — interfaces, IPs, MACs, MTU, DNS — was populated correctly). The code degrades gracefully (empty slice, no error, matching the documented "best-effort" contract), so this is **not a bug** in the sense of a crash or wrong data — but it does mean **gateway is never reported on a Mac dev agent**, which the goal document's own canary-rollout plan names as one of the two target platforms ("mix of the two target OS families — Linux systemd hosts and Mac dev machines"). Reporting this per your instruction to stop and show evidence before fixing rather than silently patching it. If you want it closed, the fix is a macOS-specific gateway reader (`route -n get default` shellout, or the `route/rt_msghdr` sysctl interface) behind the same `Provider` seam — a similar-sized addition to what gateway parsing already is for Linux. Not fixed in this pass.

**B. `internal/platform/redact` is currently untracked in git** (confirmed via `git status internal/platform/redact/` → `?? internal/platform/redact/`), along with the other Phase 2 additions (`internal/platform/lock/`, and the modified files reported by `git status` at session start). Not a defect — just confirming nothing from Phase 2 has been committed yet, so `git diff`/`git add` will show everything as new/changed when you're ready to commit.

No other defects found. Nothing in this pass required a source change to make a broken test pass — the one test-fixture bug encountered (a scratch verification handler that didn't read its request body, so `MaxBytesReader` never fired) was in the new local-verification test itself, not in shipped code, and was fixed in place before recording the result above.

---

## MANUAL INFRASTRUCTURE TESTS STILL REQUIRED

Everything below needs the real Production Atlas server, the real Relay, cyrene-dev-v2, and/or the real MacBook Atlas environment, and was intentionally not attempted here:

- Agent → Production Atlas libp2p connection (direct and/or via Relay)
- Agent → Local/development Atlas connection
- Both relationships (local + production) running simultaneously against real servers
- Production telemetry actually arriving and stored
- Local telemetry actually arriving and stored
- Production inventory actually arriving and stored
- Local inventory actually arriving and stored
- New `network` and `host` inventory subjects arriving at a real Atlas and visible via its API (no UI/API surface exists yet for these — server-side storage is generic and already accepts them, but rendering is P2/future work per the implementation plan)
- `agent_health` arriving at both control planes independently and reflecting real, distinct delivery state for each
- Relationship isolation over a real network split (e.g. block the production route with a real firewall rule, confirm local keeps delivering)
- Spool drain after a real network failure and recovery (not a simulated `httptest` outage)
- Restart/reconnect behavior against real servers, including certificate renewal timing over a real multi-hour run
- Real libp2p Relay rendezvous discovery and circuit-relay fallback against the actual Relay process
- CA bootstrap behavior against a real control plane's enrollment endpoint with `ATLAS_AGENT_INSECURE_BOOTSTRAP` unset (should now refuse) and set (should TOFU-pin as before)
