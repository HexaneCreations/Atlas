# Atlas libp2p Investigation Report

Investigation date: 2026-08-14
Repo: `HexaneCreations/Atlas`, branch `main`, HEAD `b3138f7` ("publish production libp2p port")
Scope: full trace of libp2p architecture, Agent/Server/Relay flows, enrollment, and the production `139.84.153.127:4102` direct-connection 401. Investigation only — no source, Docker, systemd, or config files were modified to produce this report.

> **Note on provenance:** this investigation was originally started against the wrong repository (`NetSepio/erebrus`), which has no libp2p relay/relationship/enrollment architecture at all. The actual target — this repo — was located at `/Users/shubh/Documents/shubh-codes-here/p-shubh/Hexane/Atlas` by grepping for `ATLAS_AGENT_RELATIONSHIPS`. Confirm you're reading this from the right checkout before acting on it.

---

## 1. libp2p Architecture

Three binaries, one shared transport package:

| Binary | Entry point | Role |
|---|---|---|
| `atlas-agent` | `cmd/atlas-agent/main.go` | Agent — dial-only, never listens |
| `atlas-server` | `cmd/atlas-server/main.go` | Control plane — listens |
| `atlas-relay` | `cmd/atlas-relay/main.go` | Circuit-relay-v2 bootstrap, no Atlas business logic |

Core libp2p code lives in one file: `internal/core/transport/libp2ptransport/libp2ptransport.go` (452 lines). Everything else (`internal/relay`, `internal/agent`, `internal/app/fleet.go`) calls into it. Design doc: `docs/adr/0012-connect-by-identity.md`.

### Identity generation/persistence

`LoadOrCreateIdentity(dataDir)` — `libp2ptransport.go:57-86`:
- Reads `<dataDir>/p2p-identity.key`; if absent, `crypto.GenerateEd25519Key(nil)`, marshals, writes with mode 0600.
- Deliberately a **separate keypair** from the X.509 enrollment identity (ADR-0012, lines 64-81): libp2p Peer ID owns discovery/routing/NAT traversal/relay; X.509 owns enrollment/authorization/certificate lifecycle/trust.
- Agent: keyed off `cfg.DataDir` (top-level agent data dir, `internal/agent/agent.go:139`) — **one identity for the whole Agent process**, shared across every relationship, not per-relationship.
- Server: keyed off `f.cfg.Fleet.DataDir` (`internal/app/fleet.go:145-147`).
- Relay: keyed off `cfg.DataDir` (`internal/relay/relay.go:27`).

### Peer ID

Standard libp2p: `peer.ID` derived from the Ed25519 public key. No custom derivation logic in this repo.

### Host construction

`NewHost(opts HostOptions)` — `libp2ptransport.go:102-131` — used by both Agent and Server:

```go
libp2pOpts := []libp2p.Option{libp2p.Identity(priv), libp2p.EnableRelay()}
if len(opts.ListenAddrs) == 0 {
    libp2pOpts = append(libp2pOpts, libp2p.NoListenAddrs)   // agent: dial-only
} else {
    libp2pOpts = append(libp2pOpts, libp2p.ListenAddrStrings(opts.ListenAddrs...))  // server
}
h, err := libp2p.New(libp2pOpts...)
```

No `libp2p.DisableRelay`, no explicit NAT/AutoNAT/hole-punch options here — transports, security, and stream muxers all come from go-libp2p's defaults (TCP transport, Noise security, yamux muxer per `go.mod`'s `go-libp2p-gostream v0.6.0` / `go-yamux/v5`). No QUIC or WebTransport configured.

`NewRelayHost` (relay only) — `libp2ptransport.go:203-229`:

```go
h, err := libp2p.New(
    libp2p.Identity(priv),
    libp2p.ListenAddrStrings(listenAddrs...),
    libp2p.ForceReachabilityPublic(),
    libp2p.EnableRelayService(relayv2.WithInfiniteLimits()),
)
```

`ForceReachabilityPublic()` skips AutoNAT probing entirely — the comment at lines 217-222 states AutoNAT is unreliable in this small POC topology, so the relay operator's own knowledge that the host is reachable substitutes for the probe.

### Feature matrix

| Feature | Status | Where |
|---|---|---|
| Direct connection | Supported | `Dial`, `libp2ptransport.go:153-165` |
| Relay connection (circuit-v2) | Supported | `EnableRelay()` (client side, all hosts) / `EnableRelayService` (relay only) |
| Relay reservation | Supported | `ReserveRelay`, `libp2ptransport.go:437-451` |
| Rendezvous discovery | Supported (custom protocol, not the standard libp2p rendezvous spec) | `Announce`/`Lookup`, `libp2ptransport.go:281-402` |
| Static multiaddr dialing | Supported | `ParseTarget` + `Dial`, `libp2ptransport.go:136-165` |
| NAT traversal | Only via relay fallback | no dedicated NAT-traversal code found |
| AutoNAT | Bypassed on relay (`ForceReachabilityPublic`); default go-libp2p behavior elsewhere, unconfigured | — |
| Hole punching | Not found | — |
| Circuit relay v2 | Supported | `relayclient`/`relayv2` imports, `libp2ptransport.go:30-31` |

---

## 2. Atlas Server libp2p Flow

`atlas-server serve` → `app.New` → `fleetPipeline.Start` (`internal/app/fleet.go:82-199`), gated on `cfg.Fleet.Enabled`.

Sequence inside `Start`:

1. `pki.LoadOrCreateCA(f.cfg.Fleet.DataDir, "atlas-control-plane")` — line 87.
2. `pki.NewServerLeaf(ca, f.cfg.Fleet.AdvertisedHosts)` — line 91. This mTLS server cert is shared by both the HTTPS listener and the libp2p listener.
3. HTTPS mTLS listener bound at `f.cfg.Fleet.Addr()` (`Host:Port`, default `0.0.0.0:8443`) — lines 121-127.
4. If `cfg.Fleet.LibP2PEnabled` (line 144): `libp2ptransport.NewHost(HostOptions{DataDir, ListenAddrs: cfg.Fleet.LibP2PListenAddrs})` → `libp2ptransport.Listen(p2pHost)` → wraps the **same mux** (`recordAgentPeer(mux)`) in a second `httpx.TLSServer` on the libp2p listener — lines 145-169.
5. If `cfg.Fleet.LibP2PRelayAddr != ""` (line 181): `reserveRelay` → `ReserveRelay` (dial out, reserve circuit slot) → `Announce` (push `h.Addrs()` + circuit addr to relay's rendezvous registry) → renewed every 20 minutes (`relayReservationRenewInterval`, line 39; `relayRenewalLoop`, lines 371-387).

### Environment variables

Binding: `ATLAS_` + struct `env` tags, joined by section (`internal/platform/config/env.go:22-49`, prefix defined `internal/platform/config/config.go:33`).

| Var | Field | Default |
|---|---|---|
| `ATLAS_FLEET_ENABLED` | `Fleet.Enabled` | `false` |
| `ATLAS_FLEET_HOST` / `ATLAS_FLEET_PORT` | `Fleet.Addr()` | `0.0.0.0:8443` |
| `ATLAS_FLEET_DATA_DIR` | | `/var/lib/atlas/fleet` |
| `ATLAS_FLEET_ADVERTISED_HOSTS` | server cert SANs | unset → `["localhost","127.0.0.1"]` (`internal/platform/pki/tls.go:44-46`) |
| `ATLAS_FLEET_LIBP2P_ENABLED` | | `false` |
| `ATLAS_FLEET_LIBP2P_LISTEN_ADDRS` | | required when enabled (`internal/platform/config/validate.go:76-78`) |
| `ATLAS_FLEET_LIBP2P_RELAY_ADDR` | | optional |

### Does production advertise `139.84.153.127`, or only container addresses?

Neither, automatically. `ATLAS_FLEET_LIBP2P_LISTEN_ADDRS=0.0.0.0:4102` binds wildcard; `h.Addrs()` (used in `Announce`'s `direct_addrs` and in `LibP2PPeerAddrs()`'s fallback, `fleet.go:207-217`) reports whatever go-libp2p's interface enumeration finds — container-internal/Docker-bridge addresses, not the host's public IP. **The public IP is never derived or advertised by any code path.** It only enters the system because an operator manually types it into the Agent's `LIBP2P_SERVER_ADDR` static multiaddr.

`.env.prod.example` (lines 74-77) still comments "only needs to be reachable *inside* the container" — this comment is now **stale**. `docker-compose.prod.yml` was changed in commit `b3138f7` to `ports: ["4102:4102"]`, publishing it to the host specifically so a static-multiaddr Agent can reach it directly (compose comment lines 176-184 names `139.84.153.127:4102` explicitly).

---

## 3. Agent libp2p Flow

Config load: `agent.LoadConfig()` (`internal/agent/config.go:62-81`) for the implicit `default` relationship; `LoadRelationshipConfigs()` (lines 122-140) for every id named in `ATLAS_AGENT_RELATIONSHIPS`.

| Var | Field | Notes |
|---|---|---|
| `ATLAS_AGENT_TRANSPORT` | `Config.Transport` | `"https"` (default) or `"libp2p"` |
| `ATLAS_AGENT_LIBP2P_SERVER_ADDR` | `Config.LibP2PServerAddr` | deprecated static multiaddr, `.../p2p/<id>` |
| `ATLAS_AGENT_LIBP2P_RELAY_ADDR` | `Config.LibP2PRelayAddr` | relay multiaddr, used for rendezvous |
| `ATLAS_AGENT_LIBP2P_SERVER_PEER_ID` | `Config.LibP2PServerPeerID` | paired with relay addr |
| `ATLAS_AGENT_RELATIONSHIPS` | comma-separated ids | `"default"` is reserved and rejected |
| `ATLAS_AGENT_RELATIONSHIP_<ID>_*` | `RelationshipBootstrap` | `<ID>` = uppercased, non-alphanumeric → `_` (`relationshipEnvPrefix`, `config.go:145-155`) |

### Precedence

`bootstrapRelationship` (`internal/agent/agent.go:290-324`), checked in this exact order:

1. `LibP2PRelayAddr != "" && LibP2PServerPeerID != ""` → rendezvous-discovery dial (`newDiscoveryDial`).
2. else `LibP2PServerAddr != ""` → static multiaddr dial (logs a `WarnContext` recommending the relay+peer-id form instead).
3. else → configuration error: libp2p transport requires one of the two.

Once bootstrapped once, **`relationship.json` becomes authoritative forever** (`loadOrAdoptRelationshipConfig`, `internal/agent/relationship.go:152-179`). Env vars are read again only if that file doesn't exist yet — editing env vars after first successful bootstrap has no effect until the file is deleted.

---

## 4. Direct Connection (static multiaddr — your current production config)

`ATLAS_AGENT_RELATIONSHIP_PRODUCTION_LIBP2P_SERVER_ADDR=/ip4/139.84.153.127/tcp/4102/p2p/<PEER_ID>`

Trace, `internal/agent/agent.go:309-320`:

```go
target, err := libp2ptransport.ParseTarget(relCfg.LibP2PServerAddr)   // ParseTarget: libp2ptransport.go:136-147
dial = func(ctx, _, _) (net.Conn, error) {
    return libp2ptransport.Dial(ctx, p2pHost, target)                // Dial: libp2ptransport.go:153-165
}
peerID = target.ID
```

`Dial` → `h.Connect(ctx, target)` (raw TCP + Noise handshake — cryptographic peer-ID verification happens here, inside go-libp2p itself, not custom code) → `gostream.Dial(ctx, h, target.ID, protocolID)` opens a stream on `/atlas/transport/1.0.0`. That `net.Conn` is handed to `http.Transport.DialContext` (`internal/agent/credentials.go:84`), and the app-level mTLS handshake (`newBootstrapClient`, `credentials.go:73-86`) runs on top of it.

### Answers

- **Does the Agent require the server to advertise `139.84.153.127`?** No. Static multiaddr dialing is fully operator-supplied and never consults `Announce`/rendezvous.
- **Does static dialing completely bypass server advertisement?** Yes.
- **Does Docker NAT affect Peer ID validation?** No. Peer-ID verification happens inside the libp2p Noise handshake, which runs *after* the TCP connection is established. Docker's DNAT (`4102:4102`) only affects whether the TCP SYN lands.
- **Does `/p2p/<PEER_ID>` provide enough identity info?** Yes — `ParseTarget` errors outright if it's missing (`libp2ptransport.go:144`).
- **Does the Agent verify the remote Peer ID matches the configured one?** Yes, but this is go-libp2p's own guarantee (`h.Connect` refuses a mismatch), not app code in this repo.

**Getting an HTTP 401 back means TCP, Docker DNAT, the libp2p Noise handshake, peer-ID match, and the app-level TLS handshake (TOFU on first enroll, `credentials.go:73-86`) all already succeeded.** The 401 is purely an application-layer (enrollment-token) failure — see §7.

---

## 5. Relay Connection (dev/working path)

`internal/relay/relay.go`: `New` builds `NewRelayHost` then `RegisterRendezvousHandlers` (lines 27-39).

- **Server → Relay:** outbound only. `fleetPipeline.reserveRelay` (`internal/app/fleet.go:348-366`) calls `h.Connect`, then `relayclient.Reserve`, then `Announce`. The Server never listens for the relay to connect to it.
- **Agent → Relay:** outbound only, same pattern, via `newDiscoveryDial`/`lookupAndCache` (`internal/agent/discovery.go:77-94`).
- **Discovery:** Agent calls `Lookup(ctx, h, relayInfo, serverID)` (`libp2ptransport.go:311-335`) → relay's `Registry.get` (in-memory, never persisted — lost on relay restart, lines 342-351) → returns `{DirectAddrs, CircuitAddr}`.
- **Circuit address shape:** `<relay-multiaddr>/p2p/<relay-id>/p2p-circuit/p2p/<server-id>` (`ReserveRelay`, line 449).
- **Direct before relay?** Yes — `buildCandidates` orders direct addrs first, circuit last (`discovery.go:55-73`); `DialWithFallback` tries each in order with an 8-second timeout per candidate (`dialCandidateTimeout`, `libp2ptransport.go:245`), falling through on failure.
- **Relay mandatory or fallback?** Fallback only — reached when every direct candidate fails.
- **Peer-ID exchange:** the relay authenticates callers "for free" via the Noise handshake itself — the registry key is the stream's real remote peer ID, never a client-claimed value (`Announce`'s doc comment, `libp2ptransport.go:278-280`).

---

## 6. Multi-Relationship Agent

`internal/agent/agent.go:88-238` (`New`), `internal/agent/relationship.go` (bootstrap/persist), `agent.go:363-393` (`resolvePeerIDConflicts`).

| Question | Answer | Source |
|---|---|---|
| Same libp2p Host for both relationships? | **Yes — one shared `p2pHost` for the whole Agent process.** | `agent.go:44,137-144`: built once from `cfg.DataDir` (root), reused by every relationship whose `Transport=="libp2p"` |
| Same Peer ID for both? | **Yes**, necessarily — same host, same key file. | same |
| Is that safe? | By design, per ADR-0012: the libp2p ID is routing-only, never trust. Trust comes from per-relationship X.509 (own CA, own leaf cert). | `libp2ptransport/agentops.go:174-229` |
| Two Atlas servers, different Peer IDs? | **Yes** — each `atlas-server` builds its host from its own `Fleet.DataDir`; dev and prod are separate processes with separate keys. | `fleet.go:145-147` |
| One Agent → two different Server Peer IDs at once? | **Yes.** `resolvePeerIDConflicts` only rejects *duplicate* peer IDs across relationships, never rejects distinct ones. | `agent.go:374-393` |
| One relationship's failure affects another? | **No.** Bootstrapped concurrently (one goroutine per relationship, `bootstrapAllRelationships`); each has its own `cancelRenewal` and its own error, fully isolated. | `agent.go:60-78`, `agent.go:244-276` |
| Certificates independent? | **Yes** — own `caCert`/`holder` per relationship. | `relationshipRuntime`, `agent.go:66-78` |
| Data directories independent? | **Yes, except the libp2p identity file.** `default` stays at `DataDir` root; every other relationship gets `DataDir/relationships/<id>/`. The libp2p key (`p2p-identity.key`) lives at the **root** `DataDir` and is shared. | `relationship.go:47-57`, `agent.go:139` |

**Your intended "one Agent, two relationships (dev + production)" is exactly what this code was built for** (Phase 3, per the ADR and code comments). It works as configured.

---

## 7. Enrollment + Authentication — the 401

Endpoint: `POST /api/v1/agent/enroll` (`internal/api/agent/handler.go:69,96-125`). `ClientAuth: tls.VerifyClientCertIfGiven` (`internal/platform/pki/tls.go:99-118`) — no client cert required for this route, since a first-time Agent doesn't have one yet.

`Enroll` → `fleet.Enroller.Enroll` (`internal/core/fleet/enroll.go:100-171`), checked in this order:

1. `tokens.Redeem(HashToken(token), sourceIP, now)` — **first**, and this is exactly where "server returned 401" originates.
2. denylist check.
3. re-enrollment-without-grant check.
4. CSR parse/sign.

`Redeem`'s implementation (`internal/storage/fleet/repository.go:62-70`) is a single conditional `UPDATE`:

```sql
UPDATE enrollment_tokens SET uses_remaining = uses_remaining - 1
WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > $2
  AND uses_remaining > 0
  AND (allowed_cidr IS NULL OR $3::inet IS NULL OR allowed_cidr >> $3::inet)
```

Zero rows updated → `pgx.ErrNoRows` → `corefleet.FormatFailure` → `ErrTokenInvalid` = `errs.New(errs.CodeUnauthenticated, ...)` (`internal/core/fleet/token.go:117-136`) → `errs.CodeUnauthenticated` maps to **HTTP 401** (`internal/platform/httpx/response.go:48`) → the Agent wraps it as `fmt.Errorf("enroll: server returned %d", resp.StatusCode)` (`internal/agent/credentials.go:103-104`) — **this is the literal "server returned 401" string you're seeing.**

The WHERE clause is deliberately non-distinguishing (`fleet/token.go:117-124`: telling a caller *why* would hand an attacker a probe), so the HTTP response gives nothing more specific. Every one of these collapses to the same 401:

- **Token doesn't exist in this server's Postgres row set** — the most likely candidate. Tokens are created via `atlas-server enroll-token` (`cmd/atlas-server/main.go:182-232`) and persisted only in the database that specific `atlas-server` process is pointed at. Dev and production are separate `atlas-server` + Postgres stacks — a token minted against dev's DB is simply not a row in production's `enrollment_tokens` table → `pgx.ErrNoRows` → 401.
- Token expired — default TTL is **1 hour** (`enroll-token`'s `--ttl` flag default, `main.go:187`).
- Token exhausted — default `--max-uses` is **1** (`main.go:186`) — already redeemed once.
- Token revoked.
- Source IP outside `allowed_cidr` — irrelevant on the libp2p path specifically: `sourceIP(r)` (`handler.go:311-317`) tries `net.SplitHostPort`/`net.ParseIP` on `r.RemoteAddr`, which on a libp2p-listener connection is a **Peer ID string**, not an IP. Parsing fails, `sourceIPArg` is `nil`, and `$3::inet IS NULL OR ...` makes the CIDR check a no-op. This cannot be the cause on the libp2p path.

### Direct answers

- **Token format:** `atlas_enroll_<64 hex chars>` (`TokenPrefix`, `fleet/token.go:26`), sent as plaintext JSON, hashed with SHA-256 server-side.
- **Storage:** Postgres `enrollment_tokens` table, per-`atlas-server`-instance.
- **Expiration:** yes, `expires_at`, operator-set TTL at creation (default 1h).
- **Environment/DB dependency:** entirely scoped to whichever Postgres database the issuing `atlas-server` process is wired to.
- **Tied to a specific Atlas server?** Yes, structurally — no shared token store across deployments.
- **Dev and production tokens separate?** Yes, necessarily.
- **Can the same token be reused across control planes?** **No.**
- **Enrollment over libp2p stream or plain HTTPS?** Same HTTP handler either way — `POST /api/v1/agent/enroll` is served on **both** listeners (`fleetPipeline.Start`, lines 121-127 for HTTPS, line 161 for libp2p) off the identical `mux`. When `Transport=libp2p`, the HTTP request rides inside the libp2p stream instead of a raw TCP socket; the enrollment protocol itself is unchanged.

**This is not a libp2p or networking bug.** It is almost certainly that `ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN` holds a token that was never created against production's own Postgres, or has expired, or has already been used once.

**Corroboration:** `AGENT_README.md:475-480` (§16 Troubleshooting, "Agent cannot enroll") independently documents the identical root cause and fix: *"wrong/expired/already-used token... issue a fresh token (`atlas-server enroll-token`, on the Server)."*

**Fix (not executed — investigation only):**

```
# on the production host, against the production atlas-server/Postgres:
atlas-server enroll-token --environment production --cidr 0.0.0.0/0 --max-uses 1 --ttl 1h

# use the printed atlas_enroll_... value as:
ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN=<value>
```

---

## 8. Docker Networking

- Container listens on `0.0.0.0:4102` inside the container (`ATLAS_FLEET_LIBP2P_LISTEN_ADDRS`, `.env.prod.example:77`).
- Published: `docker-compose.prod.yml:187-188`, `ports: ["4102:4102"]` — added in commit `b3138f7`; comment (lines 181-182) names `139.84.153.127:4102` explicitly as the intended reachable address.
- Bridge network: `atlas-internal`, driver `bridge` (`docker-compose.prod.yml:266-268`); `atlas-server` is also on this internal network for Postgres/atlas-ui, but 4102 is separately host-published.
- Docker does not touch Peer ID or libp2p protocol behavior — it's an ordinary TCP DNAT rule, transparent to everything above the TCP layer.
- **Is `4102:4102` sufficient for direct inbound connectivity?** Yes, mechanically — your own `nc -vz` success already proves the TCP hop works. Combined with the peer-ID-verified handshake reasoning in §4, `139.84.153.127:4102/p2p/12D3KooWKyBgvvWxK9yyRgpHNDkSa8rn4uNTM5v2QXNryWiZaL7e` is a structurally valid, working target against this implementation. The 401 is downstream of all of this.

---

## 9. Configuration Matrix

| Mode | Agent config | Server config | Connection path | Discovery | Requires relay |
|---|---|---|---|---|---|
| development/local | `TRANSPORT=https` (default) or `libp2p` + static addr | `Fleet.Enabled=true`, listens on `127.0.0.1:8443` or LAN | TCP+mTLS direct, or libp2p static | none | no |
| production direct | `TRANSPORT=libp2p`, `LIBP2P_SERVER_ADDR=/ip4/139.84.153.127/tcp/4102/p2p/<id>` | `LIBP2P_ENABLED=true`, `LIBP2P_LISTEN_ADDRS=0.0.0.0:4102`, port published | libp2p static dial → Noise → app-mTLS | none (operator-supplied addr) | no |
| production relay | `TRANSPORT=libp2p`, `LIBP2P_RELAY_ADDR=<relay>`, `LIBP2P_SERVER_PEER_ID=<id>` | `LIBP2P_RELAY_ADDR=<relay>` (server reserves+announces) | direct-first, circuit fallback | rendezvous via relay | fallback only |
| multi-relationship | `ATLAS_AGENT_RELATIONSHIPS=production,development` + per-id vars | independent per-server config, one server per relationship | independent per relationship, shared libp2p host if any use libp2p | per-relationship | per-relationship choice |

---

## 10. Current Production Setup — Verdict

Matches the implementation structurally at every networking/protocol layer: static multiaddr, port publish, TCP reachability, and the peer-ID-authenticated handshake are all consistent with the source and already proven working — you're past all of them; you have a 401, not a connection failure. The one thing not confirmed is `ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN`; per §7, that's where the fault almost certainly is.

---

## 11. Final Answer

### A. Architecture

```
                    ┌─────────────┐
                    │ Atlas Relay │  (public host, circuit-relay-v2 + rendezvous)
                    └──────┬──────┘
                 dial out  │  dial out
        ┌───────────────┬─┴─┬──────────────┐
        │                   │              │
  ┌─────▼─────┐      ┌──────▼─────┐  ┌─────▼──────┐
  │Atlas Agent│      │Atlas Server│  │Atlas Server│
  │(1 process,│      │ production │  │ dev/local  │
  │1 shared   │◄─────┤ Docker,    │  │ (native)   │
  │libp2p host│ dial │ :4102 pub  │  └────────────┘
  │N relations│      │ own PeerID │
  └───────────┘      └────────────┘
    per-relationship: own CA, own cert, own dataDir
    shared: 1 libp2p PeerID, 1 process
```

### B. Production direct flow

Agent static-dials `/ip4/139.84.153.127/tcp/4102/p2p/<id>` → TCP (Docker DNAT) → libp2p Noise handshake (peer-ID verified here) → yamux stream on `/atlas/transport/1.0.0` → app-level TLS (TOFU on first contact) → HTTP `POST /api/v1/agent/enroll` → **fails at token redemption → 401**.

### C. Production relay flow

Agent dials relay → `Lookup(serverID)` → gets server's `direct_addrs` (container-internal, likely useless) + `circuit_addr` → tries direct, falls back to circuit → same enrollment path once connected.

### D. Development/local flow

Same code paths, whichever transport dev is configured for (https or libp2p); independently bootstrapped relationship, own token/CA/cert.

### E. Multi-relationship design correct?

Yes — this is exactly the feature the code implements (§6), built for precisely the dev+production use case.

### F. Current direct production config correct?

Structurally yes. The port publish, static multiaddr, and Docker setup are all consistent with what the server code actually does.

### G. Exact cause of the 401

`fleet.Enroller.Enroll` → `TokenStore.Redeem` returns no matching row (`internal/storage/fleet/repository.go:62-70`) → `ErrTokenInvalid` → HTTP 401 (`httpx/response.go:48`). Overwhelmingly likely: the token in `ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN` was never minted against production's own Postgres (dev/prod tokens are not interchangeable, §7), or it's expired (default 1h TTL) or already used (default 1 use).

### H. Config for one Agent → dev + production simultaneously

```
ATLAS_AGENT_RELATIONSHIPS=production,development

ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TRANSPORT=libp2p
ATLAS_AGENT_RELATIONSHIP_PRODUCTION_LIBP2P_SERVER_ADDR=/ip4/139.84.153.127/tcp/4102/p2p/<prod-peer-id>
ATLAS_AGENT_RELATIONSHIP_PRODUCTION_CONTROL_PLANE_URL=https://139.84.153.127
ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN=<fresh token minted on PRODUCTION's own atlas-server>

ATLAS_AGENT_RELATIONSHIP_DEVELOPMENT_TRANSPORT=libp2p   # or https, whatever dev already uses
ATLAS_AGENT_RELATIONSHIP_DEVELOPMENT_LIBP2P_RELAY_ADDR=<dev relay multiaddr>   # if using relay path
ATLAS_AGENT_RELATIONSHIP_DEVELOPMENT_LIBP2P_SERVER_PEER_ID=<dev server peer id>
ATLAS_AGENT_RELATIONSHIP_DEVELOPMENT_CONTROL_PLANE_URL=https://<dev host>
ATLAS_AGENT_RELATIONSHIP_DEVELOPMENT_TOKEN=<dev token>
```

No `default`-named relationship needed unless the legacy flat vars should also stay active.

### I. Code changes required

None found. The multi-relationship and direct-dial code paths already exist and function as the target design requires.

### J. Networking/Docker changes required

None — `4102:4102` is already published (commit `b3138f7`). Confirm the host firewall (UFW, per compose comment) still allows `4102/tcp` — already verified via `nc`.

### K. Exact commands needed (not executed — reference only)

```
# on the production host, against the production atlas-server/Postgres:
atlas-server enroll-token --environment production --cidr 0.0.0.0/0 --max-uses 1 --ttl 1h

# use the printed atlas_enroll_... value as:
ATLAS_AGENT_RELATIONSHIP_PRODUCTION_TOKEN=<value>
```

---

## Appendix: files read during this investigation

- `docs/adr/0012-connect-by-identity.md`
- `internal/core/transport/libp2ptransport/libp2ptransport.go`
- `internal/core/transport/libp2ptransport/agentops.go`
- `internal/relay/relay.go`, `internal/relay/config.go`
- `internal/agent/agent.go`, `config.go`, `relationship.go`, `credentials.go`, `discovery.go`
- `internal/app/fleet.go`
- `internal/api/agent/handler.go`
- `internal/core/fleet/enroll.go`, `token.go`
- `internal/storage/fleet/repository.go`
- `internal/platform/pki/tls.go`, `ca.go`, `csr.go`, `store.go`
- `internal/platform/config/config.go`, `env.go`, `validate.go`
- `cmd/atlas-agent/main.go`, `cmd/atlas-server/main.go`, `cmd/atlas-relay/main.go`
- `docker-compose.prod.yml`, `.env.prod.example`, `Dockerfile`
- `AGENT_README.md` §16 (Troubleshooting), corroboration only
