# libp2p Identity/Address Verification — read-only, no files touched

Direct source re-read at HEAD, this session. No config changed, nothing restarted.

---

## 1. Is the Atlas libp2p Peer ID persisted across restarts?

**Already implemented.** `LoadOrCreateIdentity(dataDir)` — `internal/core/transport/libp2ptransport/libp2ptransport.go:57-86`. Reads `<dataDir>/p2p-identity.key` if present; generates and writes an Ed25519 key only on first run. `NewHost` (`libp2ptransport.go:102-131`) always calls this before building the host, so the Peer ID (a fingerprint of this key) is identical on every restart as long as the key file survives.

## 2. Where is the private key stored?

`<DataDir>/p2p-identity.key`, mode `0600` (`libp2ptransport.go:51,82`). For the Atlas server this is `<ATLAS_FLEET_DATA_DIR>/p2p-identity.key` — `internal/app/fleet.go:145-147` passes `f.cfg.Fleet.DataDir` into `NewHost`. Default `ATLAS_FLEET_DATA_DIR` is `/var/lib/atlas/fleet` (`internal/platform/config/config.go:315`). Separate from the X.509 identity (own PEM files, same directory) — deliberately two keypairs, per the file's own header comment and ADR-0012.

## 3. If the MacBook's IP changes, does the Peer ID stay the same?

**Yes, unaffected.** The Peer ID is derived only from the Ed25519 key in `p2p-identity.key`, never from an address. Nothing in `NewHost`/`LoadOrCreateIdentity` reads or depends on any IP. As long as `ATLAS_FLEET_DATA_DIR` isn't wiped, the laptop's Peer ID is identical on any network.

## 4. How does the Agent discover the current address for a known Peer ID?

**Already implemented — rendezvous-via-relay**, `internal/agent/discovery.go`:
- `newDiscoveryDial` (`discovery.go:121-139`) is the dial function installed when a relationship is configured for it.
- Each dial attempt calls `resolveCandidates` → `lookupAndCache` → `libp2ptransport.Lookup(ctx, h, relayInfo, serverID)` (`discovery.go:77-94`, `libp2ptransport.go:311-335`): a **live** rendezvous query to the relay, by Peer ID, every time a connection is attempted — not a one-time resolution.
- Result is cached to `<dataDir>/p2p-last-known.json` (`discovery.go:20-47`) only as a fallback for when the relay itself is unreachable; the cache is never treated as authoritative over a fresh lookup.
- If the dial using the freshly-looked-up (or cached) candidates fails outright, `newDiscoveryDial` forces one more **uncached** lookup before giving up (`discovery.go:133-137`) — this is exactly the "address changed since last time" recovery path.

## 5. Does the existing rendezvous/relay implementation already handle changing addresses?

**Yes, already implemented**, on both ends:
- **Server side**: `fleetPipeline.reserveRelay` (`internal/app/fleet.go:350-368`) re-announces the server's current `libp2ptransport.Addrs(p2pHost)` (its live, current listen addresses) plus its relay circuit address, every time it's called. `relayRenewalLoop` (`fleet.go:373-388`) calls it on a fixed interval (`relayReservationRenewInterval`) for as long as the process runs — so a server whose IP changes underneath it (new Wi-Fi network, new DHCP lease) simply announces its new address on the next tick, no restart required, same Peer ID throughout.
- **Registry**: `Registry` in `libp2ptransport.go:342-369` stores `{DirectAddrs, CircuitAddr}` keyed by Peer ID, overwritten on every `Announce` — last-write-wins, which is exactly "current address for this identity."
- **Agent side**: as in §4, every dial does a live lookup rather than trusting a stale value.

## 6. Can the Agent connect using Peer ID + direct address, and Peer ID + relay fallback?

**Yes, already implemented**, both paths, same mechanism:
- `buildCandidates` (`discovery.go:55-73`) orders direct addresses first, the relay circuit address last.
- `libp2ptransport.DialWithFallback` (`libp2ptransport.go:409-426`) tries each in order, 8s timeout per candidate (`dialCandidateTimeout`), returns the first success.
- So: direct reachable → direct is used, relay never touched for the actual data path. Direct unreachable (typical for a laptop behind NAT/firewall) → falls through to the circuit-relay-v2 address transparently. Both cases still authenticate the same way (libp2p Noise handshake proves the Peer ID; Atlas's own mTLS runs inside the resulting stream).

## 7. Is a source change required, or is this already implemented?

**No source change is required for the stable-identity / changing-IP requirement described.** Every mechanism it needs — persistent Peer ID, live rendezvous lookup by Peer ID, direct-then-relay fallback, re-announce on address change, cache-then-fresh-retry recovery — already exists and is wired together. This is the code path the repo's own docs (`AGENT_README.md §4`, `RELAY_README.md §4/§9`) describe as the "current, demonstrated connectivity model," not aspirational.

What *is* required is **configuration**, not code: the relationship must be set up in **rendezvous mode** (`LIBP2P_RELAY_ADDR` + `LIBP2P_SERVER_PEER_ID`), not the deprecated **static mode** (`LIBP2P_SERVER_ADDR` alone) — see §9, static mode does not get you changing-IP tolerance.

## 8. Exact environment variables for a libp2p relationship

Agent side (`internal/agent/config.go:80-83` for the default relationship; `config.go:171-179` + `relationshipEnvPrefix` for a named one, e.g. `ATLAS_AGENT_RELATIONSHIP_PRODUCTION_*`):

| Variable | Purpose |
|---|---|
| `ATLAS_AGENT_TRANSPORT` (or `..._RELATIONSHIP_<ID>_TRANSPORT`) | must be `libp2p` |
| `ATLAS_AGENT_LIBP2P_RELAY_ADDR` | Relay's full multiaddr incl. its own Peer ID, e.g. `/ip4/<relay-ip>/tcp/4103/p2p/<relay-peer-id>` |
| `ATLAS_AGENT_LIBP2P_SERVER_PEER_ID` | the control plane's Peer ID only — no address |
| `ATLAS_AGENT_LIBP2P_SERVER_ADDR` | deprecated static-mode alternative to the two above — do not combine, see §9 |

Server side (`internal/platform/config/config.go:99-116`, prefix `ATLAS_FLEET_`):

| Variable | Purpose |
|---|---|
| `ATLAS_FLEET_LIBP2P_ENABLED` | `true` to start the libp2p listener at all |
| `ATLAS_FLEET_LIBP2P_LISTEN_ADDRS` | required when enabled, e.g. `/ip4/0.0.0.0/tcp/4102` |
| `ATLAS_FLEET_LIBP2P_RELAY_ADDR` | same relay multiaddr as the agent side, so the server reserves+announces to it |

Relay (its own process, `internal/relay/config.go`, prefix `ATLAS_RELAY_`): `ATLAS_RELAY_LISTEN_ADDRS` (default `/ip4/0.0.0.0/tcp/4103`), `ATLAS_RELAY_DATA_DIR`.

## 9. Is `LIBP2P_SERVER_ADDR` a bootstrap-only value or the permanent address?

**Partially implemented / behaves as a permanent value once set — this is the one place changing-IP tolerance is NOT automatic.**

- `ATLAS_AGENT_LIBP2P_SERVER_ADDR` is a full manually-assembled multiaddr (IP **and** Peer ID together). Precedence in `bootstrapRelationship` (`internal/agent/agent.go:302-329`): rendezvous mode (relay+peer-id) is checked first and used if both are set; static mode is the `else if LibP2PServerAddr != ""` fallback, logged with an explicit warning recommending the relay+peer-id form instead (`agent.go:325-328` per the code; corroborated in `AGENT_README.md:169`, "deprecated").
- In static mode, the dial function is a fixed closure over the parsed target (`agent.go:322-324`, `target, _ := ParseTarget(...)`; `dial = func(...) { return libp2ptransport.Dial(ctx, p2pHost, target) }`) — **no re-resolution ever happens**. If the server's IP changes, this relationship simply fails to connect until the operator edits the config and restarts.
- Worse for a laptop use case: **whichever address form is used first, it gets baked into `relationship.json` on first successful bootstrap** and becomes authoritative — see §10. So even switching the env var later has no effect without deleting the file.

**Conclusion: use rendezvous mode (`LIBP2P_RELAY_ADDR` + `LIBP2P_SERVER_PEER_ID`) for the local/production relationships, not `LIBP2P_SERVER_ADDR`.** Rendezvous mode is what actually delivers "IP may change" tolerance; static mode is a fixed address by design and is explicitly marked deprecated in the code and docs for this reason.

## 10. Does the Agent persist the resolved relationship config after bootstrap?

**Yes, already implemented, and this interacts with §9.** `persistRelationshipConfig` (`internal/agent/relationship.go:196-224`) writes `<relationship-dataDir>/relationship.json` on first successful bootstrap, and is a no-op if the file already exists (`relationship.go:198-200`). `loadOrAdoptRelationshipConfig` (`relationship.go:161-193`) then treats that file as authoritative on every subsequent start — env vars for `ControlPlaneURL`, `Environment`, `Transport`, `LibP2PServerAddr`, `LibP2PRelayAddr`, `LibP2PServerPeerID` are **ignored** once the file exists (only `Token` and `CABundlePath` are re-read live, `relationship.go:178-179`).

Important nuance for rendezvous mode specifically: `LibP2PRelayAddr` and `LibP2PServerPeerID` **are** persisted into `relationship.json` (`relationship.go:150-152`, `relationship.go:207-209`), but that's fine for changing-IP tolerance because neither value is the server's own address — the Peer ID is stable identity by definition, and the relay's address is expected to be a fixed, separate, always-on machine, not the laptop. What must never be persisted-as-permanent is the server's own IP, and in rendezvous mode it never is — the server's actual reachable address is looked up fresh from the relay on every dial (§4), not read from `relationship.json` at all.

---

## Summary

| # | Question | Status |
|---|---|---|
| 1 | Peer ID persisted across restarts | **Already implemented** |
| 2 | Private key location | `<DataDir>/p2p-identity.key`, confirmed |
| 3 | Peer ID survives IP change | **Yes** — no address dependency in identity derivation |
| 4 | Agent discovers current address by Peer ID | **Already implemented** — live rendezvous `Lookup` per dial |
| 5 | Rendezvous/relay handles changing addresses | **Already implemented** — server re-announces on a renewal loop; agent looks up live, caches only as fallback |
| 6 | Peer ID + direct, Peer ID + relay fallback | **Already implemented** — `buildCandidates` + `DialWithFallback` |
| 7 | Source change required | **No**, for the described requirement — configuration choice only (see §9) |
| 8 | Exact env vars | Listed above, agent/server/relay |
| 9 | `LIBP2P_SERVER_ADDR` bootstrap-only or permanent | **Partially implemented as a footgun**: it is a fixed address for the life of the relationship (no re-resolution, and gets persisted) — avoid it for a laptop; use relay+peer-id rendezvous mode instead |
| 10 | Relationship config persisted after bootstrap | **Yes, already implemented** — `relationship.json`, authoritative from first bootstrap onward, only `Token`/`CABundlePath` stay live |

**Nothing missing for the stated goal.** The architecture already treats Peer ID as stable identity and already re-resolves addresses live. The only actionable item is a **configuration** choice for the upcoming local+production test: configure both relationships in rendezvous mode (relay address + server Peer ID), not static `LIBP2P_SERVER_ADDR` — that's what makes the MacBook's own libp2p host (dial-only, never listens — `agent.go:139`, `HostOptions{DataDir: cfg.DataDir}` with no `ListenAddrs`) tolerate its IP changing, and what makes the production server's own address changes (if any) transparent to the agent without a restart.
