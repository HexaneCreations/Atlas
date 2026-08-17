# Direct P2P NAT Traversal (DCUtR) — Implementation Report

Scope: Agent (cyrene-dev-v2) ↔ Local Atlas (MacBook, behind NAT), relay used only for bootstrap/coordination. No Production files touched, no Relay protocol changes, no custom NAT-traversal protocol — this turns on and observes go-libp2p's own DCUtR (`go-libp2p v0.49.0`, confirmed via `go.mod`/`go list -m`).

## What was implemented

1. **DCUtR hole punching enabled on every Atlas libp2p host** (Agent and Server both — they share one constructor, `libp2ptransport.NewHost`), via `libp2p.EnableHolePunching(holepunch.WithTracer(...))` + `libp2p.NATPortMap()`.
2. **Connection-path observability**: every host logs, per remote peer, when a connection is direct/relayed, established/lost/re-established, and every DCUtR event (attempted/succeeded/failed/protocol error) — via go-libp2p's own `network.Notifiee` and `holepunch.EventTracer` hooks.
3. **Direct-preferred steady state**: once a hole-punched direct connection to a relationship's target peer exists, the Agent drops its pooled (possibly relay-backed) HTTP connection so the next request re-dials onto the connection swarm already prefers — `libp2ptransport.PreferDirectConnection`, wired into `bootstrapRelationship`.
4. **No protocol, wire format, enrollment, telemetry, inventory, or relay changes.** Everything above sits entirely inside host construction and observation; the mTLS/HTTP/enroll/renew/telemetry code path is byte-for-byte unchanged.

## Exact files changed

| File                                                              | Change                                                                                                                                                                                                                                         |
| ----------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/core/transport/libp2ptransport/libp2ptransport.go`    | `NewHost`: added `Logger *slog.Logger` to `HostOptions`; added `libp2p.EnableHolePunching(holepunch.WithTracer(...))` and `libp2p.NATPortMap()` to `libp2pOpts`; registers the connection-path notifiee before returning the host. |
| `internal/core/transport/libp2ptransport/pathlog.go` (new)      | `isRelayedConn`, `newConnPathNotifiee`, `holePunchTracer`/`newHolePunchTracer`, `PreferDirectConnection`.                                                                                                                            |
| `internal/core/transport/libp2ptransport/pathlog_test.go` (new) | Deterministic tests, see below.                                                                                                                                                                                                                |
| `internal/agent/agent.go`                                       | `NewHost` call now passes `Logger: logger`; `bootstrapRelationship` calls `libp2ptransport.PreferDirectConnection` when the relationship is on libp2p.                                                                                 |
| `internal/app/fleet.go`                                         | `NewHost` call now passes `Logger: f.logger`.                                                                                                                                                                                              |

No other file touched. No Relay (`internal/relay/relay.go`, `NewRelayHost`) change — relay fallback is unmodified.

## Exact libp2p APIs used

- `libp2p.EnableHolePunching(opts ...holepunch.Option)` — `go-libp2p/options.go`. Requires `EnableRelay` (already on).
- `libp2p.NATPortMap()` — best-effort UPnP/NAT-PMP port mapping; no-op on a dial-only (no listen addr) host.
- `github.com/libp2p/go-libp2p/p2p/protocol/holepunch`: `holepunch.WithTracer(EventTracer)`, `holepunch.Event`, `StartHolePunchEvtT`/`EndHolePunchEvtT`/`ProtocolErrorEvtT`, `EndHolePunchEvt{Success, EllapsedTime, Error}`.
- `github.com/libp2p/go-libp2p/core/network`: `Notifiee`/`NotifyBundle` (`Connected`/`Disconnected`), `Conn.Stat().Limited`, `Conn.RemoteMultiaddr()`.
- `github.com/multiformats/go-multiaddr`: `ma.P_CIRCUIT`, `Multiaddr.ValueForProtocol` — same idiom go-libp2p's own `swarm_dial.go:isRelayAddr` uses.

No new module dependency: `holepunch`/`autonatv2`/`circuitv2` are already part of the `go-libp2p` module already in `go.mod`.

### A real finding from this work: `Stat().Limited` is not a reliable relay signal for Atlas's relay

Atlas's relay (`NewRelayHost`, unchanged) runs `relayv2.WithInfiniteLimits()`. Traced into `circuitv2/client/dial.go`: the client only sets `stat.Limited = true` when the relay's STATUS response carries a resource limit; an infinite-limits relay sends none, so **every circuit connection through Atlas's relay reports `Stat().Limited == false`**, even though it is plainly relayed (confirmed empirically: `pathlog_test.go` originally asserted on `Limited` alone and failed against a real 3-host relay test — logged 0 "relay" events, all "direct" — before the fix). `isDirectConn` in go-libp2p's own `swarm.go` (used by `bestConnToPeer`, the actual stream-routing preference) does **not** have this problem — it checks `!conn.Transport().Proxy()`, unaffected by relay limit config. Only Atlas's own new observability code needed the fix: `isRelayedConn` (`pathlog.go`) now checks `Stat().Limited` **or** the multiaddr's `/p2p-circuit` component (`ma.P_CIRCUIT`), the same check `swarm_dial.go:isRelayAddr` uses internally. Verified against a real relay in `TestConnPathNotifieeLogsRelayFallback`.

## Connection state machine

```
Agent dials Local Atlas's circuit multiaddr (relay-mediated)
        │
        ▼
 Relayed connection established  ──log──▶ "libp2p relay fallback used"
        │
        │  (both sides have EnableHolePunching; the side that received
        │   the inbound relayed connection — Local Atlas — initiates
        │   DCUtR per go-libp2p's holepunch.Service; Agent participates)
        ▼
 DCUtR hole punch attempted     ──log──▶ "libp2p hole punch attempted"
        │
   ┌────┴─────┐
   ▼          ▼
success     failure
   │          │
   │          └──log──▶ "libp2p hole punch failed"
   │                     (relayed connection stays open, unaffected —
   │                      no action needed, it was never closed)
   ▼
Direct connection established  ──log──▶ "libp2p direct connection established"
   │
   │  swarm.bestConnToPeer now prefers this connection for new streams
   │  (existing streams on the relayed conn are NOT migrated — this is
   │   documented go-libp2p behavior, not an Atlas gap)
   ▼
Agent's PreferDirectConnection callback fires
   → httpClient.CloseIdleConnections() on the relationship's pooled
     HTTP transport → next telemetry/inventory request re-dials →
     new gostream stream lands on the direct connection (bestConnToPeer)
   │
   ▼
 [ if the direct connection later drops, e.g. NAT mapping expiry ]
   ──log──▶ "libp2p direct connection lost"
   │  Agent's existing rendezvous/retry logic (discovery.go, unchanged)
   │  redials — first candidate tried is again direct-first, else relay
   ▼
 Direct connection re-established ──log──▶ "libp2p direct connection re-established"
```

## Direct-vs-relay selection logic

Two independent layers, deliberately not merged:

1. **Which connection a new libp2p stream uses** — entirely go-libp2p's own `Swarm.bestConnToPeer` (unmodified): prefers an unlimited connection over a limited one, then a direct connection (`!Transport().Proxy()`) over a relayed one, then the connection with more open streams. Atlas does not reimplement this.
2. **Whether Atlas's own pooled HTTP client keeps using an old (relay-backed) stream** — this is the one thing DCUtR does not do automatically (it never migrates already-open streams, per its own doc comment in `options.go`). `PreferDirectConnection` closes idle pooled connections on the relationship's `http.Client` the moment a direct connection to that specific peer ID appears, so the *next* request naturally re-dials and picks up (1) above. Verified peer-ID-scoped in `TestPreferDirectConnectionFiresOnlyForMatchingPeerID` — it does not fire for an unrelated peer.

`resolveCandidates`/`DialWithFallback` (`discovery.go`, unchanged) already try direct addresses before the relay circuit address on every fresh dial or reconnect — this implementation adds the *in-connection* upgrade path (DCUtR) on top of that pre-existing *dial-time* preference; neither replaces the other.

## NAT limitations (requirement 9)

- **Cone NATs (full/restricted/port-restricted cone)**: hole punching reliably succeeds — the common case for home routers, which is what the MacBook is behind.
- **Symmetric NAT**: hole punching reliably **fails** — the NAT assigns a different external port per destination, so the address the relay observes is never valid for a peer dialing in from elsewhere. DCUtR will attempt, fail, log `"libp2p hole punch failed"`, and the connection **stays on the relay** — this is not a regression, it is the documented fallback working correctly.
- **Double NAT / CGNAT**: behaves like an extra layer of (often symmetric) NAT — same failure mode as above, same fallback.
- **A loopback/CI environment cannot exercise a real hole-punch failure** — both peers are always mutually reachable, so DCUtR either succeeds trivially or (as observed while testing) doesn't get a chance to run before the relay connection itself is what's actually exercised. `TestHolePunchFailureLeavesRelayPathUsable` covers what *is* deterministically provable locally: a failed-hole-punch event is logged correctly (fed synthetically, since go-libp2p's own event type is the thing under test, not a fabricated NAT), and a relay connection independently keeps carrying traffic. Genuine hole-punch failure against symmetric NAT is a manual test (see below) — the log line to watch for is `"libp2p hole punch failed"` with no matching `"libp2p direct connection established"` for that peer afterward, and telemetry continuing to flow with only `"libp2p relay fallback used"` present. The implementation never claims direct connectivity in that case — the "direct connection established" log line is only ever emitted from a real non-relayed `Connected` event.

## Configuration changes

**None required.** No new environment variables, no new config fields — `EnableHolePunching`/`NATPortMap`/the notifiee are unconditional inside `NewHost`, applied identically to every existing relationship (rendezvous, static-address, and relay hosts alike) with zero opt-in. The existing rendezvous configuration from the prior audit is unchanged and is what this builds on:

```
ATLAS_AGENT_RELATIONSHIP_LOCAL_TRANSPORT=libp2p
ATLAS_AGENT_RELATIONSHIP_LOCAL_LIBP2P_RELAY_ADDR=/ip4/45.77.172.153/tcp/4103/p2p/<RELAY_PEER_ID>
ATLAS_AGENT_RELATIONSHIP_LOCAL_LIBP2P_SERVER_PEER_ID=12D3KooWAG8Ks6fnAuXcq5CzjmFHkrhJc3kRxy1YGh8BTUtAFaf
```

## Test results

```
go build ./...      → clean
go vet ./...         → clean
go test ./...         → ok, all 45 packages
go test -race ./...   → ok, all 45 packages (ld LC_DYSYMTAB warnings are pre-existing macOS linker cosmetics, unrelated to this change)
```

New tests, `internal/core/transport/libp2ptransport/pathlog_test.go` (all real libp2p hosts, loopback network, nothing faked except the two explicitly-synthetic hole-punch event traces called out below):

| Test                                                        | Covers                                                                                                                    |
| ----------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| `TestHolePunchTracerLogsEachEventType`                    | hole punch attempted/succeeded/failed/protocol-error logging (synthetic`holepunch.Event`s — deterministic, no network) |
| `TestConnPathNotifieeLogsDirectConnectionAndPeerID`       | direct connection available + Peer ID verification                                                                        |
| `TestConnPathNotifieeLogsRelayFallback`                   | relay fallback logging over a real 3-host relay                                                                           |
| `TestHolePunchFailureLeavesRelayPathUsable`               | hole punch failure → relay fallback (failure traced synthetically; relay-still-usable proven with a real relay+transfer) |
| `TestPreferDirectConnectionFiresOnlyForMatchingPeerID`    | Peer-ID-scoped callback firing, real direct connection                                                                    |
| `TestConnPathNotifieeLogsReconnectAfterDirectLoss`        | reconnect after direct connection loss                                                                                    |
| `TestEnrollmentAndTelemetryStyleHTTPOverDirectConnection` | enrollment/telemetry-shaped HTTP round trip over a direct libp2p connection                                               |

All deterministic, no Production dependency, no sleeps beyond short bounded polls with timeouts.

## Exact commands for manual testing

**MacBook (Local Atlas)** — unchanged startup, no config needed for this feature:

```
# confirm relay reservation still active (existing behavior)
grep "fleet libp2p listener ready" <local-atlas-log>
```

Watch for, once the Agent connects:

```
"libp2p relay fallback used" peer=<agent-peer-id>
"libp2p hole punch attempted" peer=<agent-peer-id>
"libp2p hole punch succeeded" peer=<agent-peer-id>      # or "failed" if the NAT is symmetric
"libp2p direct connection established" peer=<agent-peer-id>
```

**cyrene-dev-v2 (Agent)** — same rendezvous config as before, restart the Agent, watch the same log sequence for `peer=12D3KooWAG8Ks6fnAuXcq5CzjmFHkrhJc3kRxy1YGh8BTUtAFaf`:

```
grep -E "libp2p (relay fallback used|hole punch|direct connection)" <agent-log>
```

Confirm the spool stays empty (delivery healthy) and, after `"libp2p direct connection established"` appears, that traffic keeps flowing — no gap, no re-enrollment, no error — proving requirements 7/8 (enrollment/telemetry continue working, transparently, across the transport upgrade):

```
find <agent-data-dir>/relationships/local/spool -name "*.envelope.json"
```

Nothing in this test requires touching Production Atlas, the Relay's own code, or any deployment file — only observing the two processes' own logs.
