# Agent (Phase 2) ↔ Server libp2p Compatibility Audit — read-only

No files modified, nothing restarted. "Server implementation" below = current repo source (what `make run` / a rebuilt server would run). Production's already-deployed binary predates every Phase-2 server-side file (`internal/api/agent/handler.go` enforcement, `internal/app/fleet.go` `AgentMiddleware`) — those exist only in this working tree, uncommitted. Called out per-item where that gap matters.

**Bottom line: new Agent is compatible with the currently-deployed (pre-Phase-2) production Server binary. No server rebuild is required to communicate.** Phase 2 touched zero lines in `internal/core/transport/libp2ptransport/` — the entire dial/listen/rendezvous/relay/mTLS-over-stream mechanism is byte-identical to what's already deployed.

| # | Item | Agent | Server | Compatible | Server change needed |
|---|---|---|---|---|---|
| 1 | libp2p transport init | `agent.go:139` dial-only `NewHost(HostOptions{DataDir})`, unchanged in Phase 2 | `fleet.go:145-148` `NewHost(HostOptions{DataDir, ListenAddrs})`, unchanged | **YES** | None |
| 2 | Peer ID / identity | `libp2ptransport.go:57-86` `LoadOrCreateIdentity`, unchanged | same function, same file, unchanged | **YES** | None |
| 3 | Rendezvous discovery | `discovery.go:77-139` live `Lookup` per dial, unchanged | `libp2ptransport.go:310-402` `Lookup`/`RegisterRendezvousHandlers` (relay, separate binary), unchanged | **YES** | None |
| 4 | Relay reservation + fallback | `discovery.go:55-73,409-426` direct-first/circuit-last, unchanged | `fleet.go:350-388` `reserveRelay`+20min renewal loop, unchanged | **YES** | None |
| 5 | Direct P2P dialing | `libp2ptransport.go:153-165` `Dial`, unchanged | same, listener side `Listen` (`libp2ptransport.go:170-177`), unchanged | **YES** | None |
| 6 | TLS/auth over libp2p | mTLS `http.Client` over the stream, unchanged | mTLS `httpx.TLSServer` over the stream (`fleet.go:161`), unchanged | **YES** | None |
| 7 | Enrollment | `credentials.go` — **new**: refuses to enroll with no CA bundle unless `InsecureBootstrap` set (agent-local gate, added Phase 2) | `handler.go:96-131` `Enroll`, wire format (`EnrollRequest`/`CertResponse`) unchanged | **YES** | None — the new check is agent-side only, doesn't touch the wire |
| 8 | Certificate exchange/persistence | `pki.SaveLeaf`/`LoadLeaf`, unchanged | `pki.CA.IssueLeaf`, unchanged | **YES** | None |
| 9 | Telemetry transport | `remote.go:293` sends `protocol_version:1` (unchanged value, was already sent pre-Phase-2) | `handler.go:217` **new**: now enforces `req.ProtocolVersion == 1`; `handler.go` **new**: `Envelope.Validate()` now called before dispatch | **YES** | None — agent already sends `1` and well-formed envelopes; an un-rebuilt server simply doesn't check either (same net behavior for a compliant agent) |
| 10 | Inventory transport | `inventory.go` **new** subjects `network`, `host`, `agent_health`; per-relationship dedup fix (agent-local, no wire change) | `storage/inventory/repository.go:37-54` — generic upsert, `subject` is `text NOT NULL` with **no enum/CHECK constraint** (`migrations/0004_fleet.sql:150-164`) | **YES** | None to *store* the new subjects — an old server accepts and persists them unmodified. A server rebuild is only needed to *read them back out* via a typed API endpoint (none exists yet for these three subjects — pre-existing P2 gap, not a compatibility break) |
| 11 | Heartbeat / agent_health transport | `agent_health` is **not** sent via `POST /api/v1/agent/heartbeat` — it rides the same inventory pipeline as item 10, subject `agent_health` (`internal/agent/health.go`). The heartbeat endpoint itself is still never called by the agent (unchanged pre-Phase-2 behavior) | `handler.go:264-277` `Heartbeat` endpoint exists, unused by any agent version; inventory receiver accepts `agent_health` generically per item 10 | **YES** | None |
| 12 | Protocol version enforcement | `remote.go:293` — unchanged value `1` | `handler.go:217-224` **new** on telemetry only; enroll/renew carry no protocol_version check on either version of the server (`EnrollRequest`/`RenewRequest` have no such field) | **YES** | None — value matches on both sides regardless of which server build is running |
| 13 | Envelope validation | Agent constructs envelopes via `transport.NewEnvelopeOf`, unchanged — always produces valid `Origin`/`Payload` | `handler.go:227-235` **new**: `env.Validate()` called after identity rebind, rejects malformed batches | **YES** | None — a compliant agent never trips this; it only rejects genuinely malformed input, which the new agent never produces |
| 14 | Stream/connection lifecycle | `agent.go` bootstrap/spool/renewal wiring around dial, unchanged in Phase 2 (only fanout/environment plumbing added, outside the dial path) | `fleet.go` listener lifecycle, unchanged | **YES** | None |
| 15 | Reconnect after network/IP change | `discovery.go:96-139` cache-then-fresh-lookup, unchanged | `fleet.go:373-388` `relayRenewalLoop` re-announces every 20 min, unchanged | **YES** | None |
| 16 | Spool/retry behavior | `spool.go` **new**: `.tmp` orphan sweep on `Open()`; `remote.go` **new**: `Stats()` records last-success/last-failure+reason, all agent-local | No server-side spool; retry/backoff already server-agnostic (`remote.go` replay loop, pre-existing) | **YES** | None — purely agent-local reliability fix, no wire impact |

## Summary

- **15/16 items: fully compatible, zero server code touched or needed.**
- **Item 9/10/11/12/13** are the only ones where Phase 2 added *server-side* logic — every one of them is additive/stricter-only, and the new Agent already satisfies the stricter checks, so it works identically against an old (unenforced) or new (enforced) server build.
- **No server rebuild is required** to run the fresh Agent test against the currently deployed Production libp2p Server or a locally-run one built from the pre-Phase-2 commit.
- The only thing a server rebuild would *add*, not fix: reading the new `network`/`host`/`agent_health` inventory subjects back out through a typed API/UI (storage already accepts them today, per item 10).

No source files modified. No configuration changed. Nothing restarted.
