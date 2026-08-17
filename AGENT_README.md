file bin/atlas-agent-linux-amd64# Atlas Agent

Operational documentation for `atlas-agent`, the host-observation daemon that runs on every machine Atlas monitors.

This document is derived directly from the source in `internal/agent/`, `cmd/atlas-agent/`, `internal/core/transport/`, `internal/platform/pki/`, `internal/platform/hostid/`, and `packaging/atlas-agent/`. Anything that could not be confirmed from the repository is explicitly marked **Not confirmed** or **TODO**.

---

## 1. Overview

**Atlas Agent** is a single static binary (`cmd/atlas-agent`) that runs on a managed/monitored machine, collects facts about that machine, and pushes them to one or more Atlas Server control planes. It never accepts commands to change anything on the host it runs on — Atlas's stated product principle is *"Observe Everything. Control Nothing."* (`docs/context/IMPLEMENTATION_CONTEXT.md`). The one narrow exception is described in [§12 AgentOps](#12-agentops-container-log-streaming): a control-plane-initiated, read-only container-log stream.

**Where it runs:** the target/managed machine being monitored (e.g. a Linux host such as `cyrene-dev-v2`). It is installed as a systemd service (`packaging/atlas-agent/`).

**Relationship to Atlas Server:** the Agent enrolls once (using a bootstrap token), is issued a short-lived mTLS client certificate, and thereafter periodically pushes metrics, inventory, and events to the Server's Fleet HTTP API (`/api/v1/agent/enroll`, `/api/v1/agent/renew`, `/api/v1/agent/telemetry`). See [§10](#10-enrollment--authentication).

**Relationship to Atlas Relay:** only relevant when a relationship uses `Transport=libp2p`. The Agent is a **dial-only** libp2p host — it never listens for inbound connections (`internal/core/transport/libp2ptransport/agentops.go:24-27`). When the Server is not directly reachable, the Agent asks the Relay (a rendezvous + circuit-relay-v2 bootstrap service) how to reach the Server, then dials it directly if possible or through a relayed circuit if not. See [§4](#4-connectivity-architecture).

**Role in the architecture:** the Agent is the only Atlas component that touches the monitored host's OS/Docker/systemd surfaces directly. Everything it produces is carried, unmodified in shape, through a pluggable `Transport` interface (`internal/core/transport`, ADR-0005) to the Server, which stores and interprets it.

As of `internal/agent/agent.go` (Phase 3), **one Agent process can maintain simultaneous, fully independent connections to more than one Atlas Server** — see [§4](#4-connectivity-architecture) and [§6](#6-configuration).

### Architecture diagram

```text
                         Atlas Server (Control Plane)
                    mTLS CA  +  libp2p Peer ID (if enabled)
                                       ▲
                                       │  HTTPS + mTLS
                                       │  (enroll / renew / telemetry —
                                       │   identical request shape either transport)
                     ┌─────────────────┴─────────────────┐
                     │                                    │
           transport = https                    transport = libp2p
        (default; plain TCP dial to        (dial-only libp2p host; two dial
         ATLAS_AGENT_CONTROL_PLANE_URL)      strategies — see §4)
                     │                                    │
                     │                       ┌────────────┴────────────┐
                     │                       │                         │
                     │              direct dial to a           rendezvous lookup,
                     │              manually-assembled          then dial direct-
                     │              multiaddr (deprecated)      addrs-first, relay-
                     │                       │              circuit-last (recommended)
                     │                       │                         │
                     │                       │                ┌────────┴────────┐
                     │                       │                │   Atlas Relay   │
                     │                       │                │ (circuit-relay- │
                     │                       │                │  v2 + rendezvous)│
                     │                       │                └────────┬────────┘
                     │                       │                         │
                     └───────────────────────┴─────────────────────────┘
                                       │
                                  Atlas Agent
                        (one node identity + one shared
                         libp2p host, fanned out across
                          every configured relationship)
                                       │
                              Managed / Monitored Machine
                    (plugins read local OS/Docker/systemd state only)
```

---

## 2. Agent Responsibilities

Confirmed by code:

| Responsibility                                                               | Confirmed                                   | Source                                                                   |
| ---------------------------------------------------------------------------- | ------------------------------------------- | ------------------------------------------------------------------------ |
| Enrollment (bootstrap token → mTLS client cert)                             | ✅                                          | `internal/agent/credentials.go`                                        |
| TOFU CA pinning + persisted CA trust                                         | ✅                                          | `credentials.go:151-165`                                               |
| Automatic certificate renewal                                                | ✅                                          | `credentials.go:322-378`                                               |
| Metric collection (via plugins)                                              | ✅                                          | `internal/core/collect`, `agent.go:190-205`                          |
| Inventory push (deduplicated by content hash)                                | ✅                                          | `internal/agent/inventory.go`                                          |
| Event forwarding (local bus → control plane)                                | ✅                                          | `internal/agent/events.go`                                             |
| Disk-backed spool + replay on outage                                         | ✅                                          | `internal/core/transport/spool`, `internal/core/transport/remote`    |
| Docker container discovery/inventory                                         | ✅                                          | `internal/plugin/docker`                                               |
| Process, systemd service, cron, port, and system-fact collection             | ✅                                          | `internal/plugin/{process,service,cron,ports,system}`                  |
| Remote container**log** streaming (read-only, control-plane-initiated) | ✅                                          | `internal/agent/agentops.go`, `libp2ptransport/agentops.go`          |
| Multiple simultaneous control-plane connections                              | ✅                                          | `internal/agent/relationship.go`, `agent.go:44-58`                   |
| Command execution on the host                                                | ❌ Not implemented                          | No execution surface exists anywhere in`internal/agent` or its plugins |
| Configuration mutation / control of the host                                 | ❌ Not implemented                          | Consistent with "Observe Everything. Control Nothing."                   |
| Standalone heartbeat call                                                    | ⚠️**Discrepancy** — see note below |                                                                          |

> **Discrepancy note:** `docs/context/ARCHITECTURE.md` lists the fleet pipeline as "Enroll / Renew / Heartbeat / Telemetry", and the Server exposes `POST /api/v1/agent/heartbeat` (`internal/api/agent/handler.go:72`). However, no code path in `internal/agent` ever calls it — a repo-wide search of `internal/agent` for "heartbeat" returns nothing. The Agent's actual liveness signal is the recurring telemetry/inventory push cadence, not a distinct heartbeat message. Treat the executable behavior (no heartbeat call) as authoritative.

---

## 3. Agent Working Model

Traced from `cmd/atlas-agent/main.go` → `internal/agent/agent.go` (`New`, `Run`) → `internal/agent/credentials.go` / `relationship.go` / `discovery.go`.

```text
atlas-agent process starts (cmd/atlas-agent/main.go)
        │
        ▼
Load Config from ATLAS_AGENT_* env vars (agent.LoadConfig)
        │
        ▼
Resolve node identity (hostid.Resolve — configured id, /etc/machine-id,
                        state file, or freshly generated + persisted)
        │
        ▼
Discover every relationship to bootstrap:
   - "default"  (always, from the flat ATLAS_AGENT_* vars)
   - any id listed in ATLAS_AGENT_RELATIONSHIPS
        │
        ▼
For each relationship, resolve its connection config
   (relationship.json on disk if it exists → authoritative;
    otherwise the env-sourced bootstrap values)
        │
        ▼
Decide whether a shared libp2p host is needed at all
   (only if at least one relationship uses transport=libp2p)
        │
        ▼
Bootstrap every relationship concurrently, independently:
   ├─ libp2p relationships: resolve dial strategy
   │    (rendezvous-via-relay, or deprecated static multiaddr)
   ├─ Load existing cert from disk, OR enroll with ATLAS_AGENT_TOKEN
   │    (TOFU-pin the CA on first contact if no CA bundle configured)
   ├─ Start that relationship's renewal loop (hourly check, renews at 50% of lifetime)
   └─ Open that relationship's disk spool + remote.Transport
        │
        ▼
A relationship that fails to bootstrap is logged and dropped;
the Agent only exits if EVERY relationship fails
        │
        ▼
Wire the (single, shared) collection pipeline:
   register plugins → detect → init → register collectors
        │
        ▼
Fan every relationship's transport out through one fanoutTransport
        │
        ▼
Run(): start scheduler, inventory pusher, event forwarder
        │
        ▼
Loop until process signal (SIGINT/SIGTERM):
   ├─ Scheduler polls collectors on their interval → sends metrics
   ├─ Inventory pusher sends changed snapshots on ATLAS_AGENT_INVENTORY_INTERVAL
   ├─ Event forwarder relays local bus events as they occur
   ├─ Failed sends spool to disk (stream-class) and retry with backoff
   └─ Each relationship's renewal loop renews its cert before expiry
        │
        ▼
Close(): cancel renewal loops, stop scheduler/plugins, close transports,
         close the shared libp2p host
```

There is no separate "connect" state machine distinct from bootstrap — dialing, enrollment, and the HTTP request itself are the same code path (`remote.Transport` + a `DialContext` override for libp2p). Reconnection is handled per-request, not as an explicit reconnect step (see [§14](#14-reconnection-and-failure-handling)).

---

## 4. Connectivity Architecture

### Two transports, selected per relationship

`ATLAS_AGENT_TRANSPORT` (or `ATLAS_AGENT_RELATIONSHIP_<ID>_TRANSPORT`) is either:

- **`https`** (default) — a plain TCP dial to `ControlPlaneURL`, mTLS on top. No libp2p involved at all.
- **`libp2p`** — the connection is a libp2p stream carrying the identical HTTP traffic (`internal/core/transport/libp2ptransport/libp2ptransport.go:1-12`: *"libp2p answers how to reach this peer, X.509 answers whether to trust it."*). Only the dial changes.

### Two dial strategies under `transport=libp2p` (`internal/agent/agent.go:290-323`)

1. **Rendezvous discovery via Relay (recommended)** — set when both `LIBP2P_RELAY_ADDR` and `LIBP2P_SERVER_PEER_ID` are set.
2. **Static multiaddr (deprecated)** — set when only `LIBP2P_SERVER_ADDR` (a full, manually-assembled multiaddr including `/p2p-circuit/` if relayed) is set. Logged as a warning every time it is used (`agent.go:318-320`): *"dialing control plane by static multiaddr; set a relay address and server peer id instead."*

### How rendezvous discovery works (`internal/agent/discovery.go`)

1. The Agent's shared libp2p host connects to the Relay and opens the `/atlas/rendezvous/lookup/1.0.0` stream, asking for the Server's Peer ID (`libp2ptransport.Lookup`).
2. The Relay returns whatever the Server most recently **announced**: its direct listen multiaddrs (possibly empty, e.g. if the Server is behind NAT) and, if it reserved one, its relay circuit multiaddr.
3. The Agent builds a dial candidate list — **direct addresses first, the relay circuit address last** (`discovery.go:55-73`, `buildCandidates`).
4. `DialWithFallback` tries each candidate in order, 8 seconds per attempt, and uses the first that connects (`libp2ptransport.go:404-426`).
5. The successful result is cached to `<data-dir>/p2p-last-known.json` — a Relay outage does not prevent reconnecting to a Server the Agent has already reached once (`discovery.go:96-114`).
6. If the *cached* dial also fails, the Agent forces one fresh (non-cached) lookup before giving up on that connection attempt (`discovery.go:116-139`).

This is the **direct-preferred, relay-fallback** behavior at the mechanism level: it is not two different operator-chosen paths, it is one lookup whose candidate ordering naturally prefers a direct address when the Server has one to advertise, and falls back to the relay circuit otherwise.

### Proven / current path

```text
Agent  ──rendezvous lookup──▶  Atlas Relay  ──relayed circuit stream──▶  Atlas Server
```

This is the path the repository's own comments describe as verified: `docker-compose.prod.yml` states *"The verified working Mac→Relay→Agent path already proved this — no port was forwarded for it either"* (referring to the Server not needing an inbound port when it dials out to the Relay and reserves a circuit). The current focus, per this repository's state, is this relay/rendezvous path.

### Direct path

**Supported by design, not confirmed as currently verified in production.** The code path exists and is exercised whenever a Server announces a reachable direct address (`buildCandidates` tries it first) — but whether a direct address is actually reachable depends entirely on network topology (NAT, firewall, published ports) that lives outside this repository. Do not treat "direct connectivity works" as proven; treat it as "supported, contingent on the Server's direct address being externally reachable."

### Peer identity and the Agent

- Every libp2p host (the Agent's shared host) has a persistent Ed25519 keypair, generated once and stored at `<data-dir>/p2p-identity.key` (`libp2ptransport.go:47-86`, `LoadOrCreateIdentity`). The Agent's own Peer ID is this key's fingerprint.
- **The Agent's libp2p host never listens** — `NewHost` is called with no `ListenAddrs` (`agent.go:139`, `HostOptions{DataDir: cfg.DataDir}`), so `libp2p.NoListenAddrs` applies. It can dial out and accept a *new stream* on a connection it initiated, but it can never receive a fresh inbound connection. This is what makes the Agent safe to run with no forwarded/open port.
- The Server identifies which libp2p Peer ID belongs to which enrolled node by recording it the moment a request arrives with a verified mTLS client certificate (`internal/app/fleet.go:240-253`, `recordAgentPeer`) — the mapping is peer-id-from-traffic, not something the Agent declares.

### Which side initiates connections

Always the Agent, for every connection: to the Server (directly, or via relay), and to the Relay itself (for rendezvous lookups). The Server never dials the Agent — even AgentOps (§12), which is logically "Server asks Agent for logs," rides on a connection the Agent already opened by dialing out; the Server just opens a *new stream* on it.

### What happens if direct connectivity fails / Relay is unavailable

Covered fully in [§14](#14-reconnection-and-failure-handling).

---

## 5. Connectivity Details

| Component                         | Address/Port                                                                                     | Protocol                                                     | Purpose                                   | Required                                                             |
| --------------------------------- | ------------------------------------------------------------------------------------------------ | ------------------------------------------------------------ | ----------------------------------------- | -------------------------------------------------------------------- |
| Atlas Server (https transport)    | `ATLAS_AGENT_CONTROL_PLANE_URL`, default `https://127.0.0.1:8443`                            | HTTPS + mTLS (TLS 1.3)                                       | Enroll, renew, telemetry                  | Required unless every relationship uses libp2p                       |
| Atlas Server (libp2p, static)     | `ATLAS_AGENT_LIBP2P_SERVER_ADDR` (deprecated)                                                  | libp2p stream,`/atlas/transport/1.0.0`                     | Same HTTP surface, carried over libp2p    | Only if using the deprecated static-multiaddr mode                   |
| Atlas Server (libp2p, discovered) | Resolved dynamically via Relay lookup                                                            | libp2p stream,`/atlas/transport/1.0.0`                     | Same HTTP surface, carried over libp2p    | Only if using rendezvous discovery                                   |
| Atlas Relay                       | `ATLAS_AGENT_LIBP2P_RELAY_ADDR`, e.g. `/ip4/<RELAY_IP>/tcp/<RELAY_PORT>/p2p/<RELAY_PEER_ID>` | libp2p,`/atlas/rendezvous/lookup/1.0.0` + circuit-relay-v2 | Server discovery + NAT-traversal fallback | Only if a relationship uses libp2p**and** rendezvous discovery |
| Agent's own libp2p host           | None — dial-only, no listener                                                                   | —                                                           | N/A                                       | —                                                                   |

Agent-initiated outbound only. No inbound port is ever required by the Agent itself.

---

## 6. Configuration

Source of truth: `internal/agent/config.go`, and `cmd/atlas-agent/main.go`'s `--help` text (§6 below mirrors it exactly).

### Default relationship (`ATLAS_AGENT_*`)

| Variable                                         | Required                                            | Example                                                  | Description                                                                                                                                                                                                     |
| ------------------------------------------------ | --------------------------------------------------- | -------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ATLAS_AGENT_CONTROL_PLANE_URL`                | Recommended (defaults to`https://127.0.0.1:8443`) | `https://control-plane.example.internal:8443`          | Control plane base URL. For libp2p transport, still a syntactically valid`https://` URL — the host portion is never actually dialed (the custom `DialContext` ignores it), only used to build request URLs |
| `ATLAS_AGENT_TOKEN`                            | Required on first run only                          | `<agent-token>`                                        | One-time enrollment token; ignored once a certificate is persisted                                                                                                                                              |
| `ATLAS_AGENT_DATA_DIR`                         | No (default`/var/lib/atlas-agent`)                | `/var/lib/atlas-agent`                                 | Certificate, spool, node-id, and (if libp2p) Peer ID keypair storage                                                                                                                                            |
| `ATLAS_AGENT_CA_BUNDLE`                        | Recommended                                         | `/etc/atlas-agent/ca-bundle.pem`                       | PEM CA to pin — a verified bootstrap. Required unless`ATLAS_AGENT_INSECURE_BOOTSTRAP=true`                                                                                                                   |
| `ATLAS_AGENT_INSECURE_BOOTSTRAP`               | No (default`false`)                               | `true`                                                 | Enroll with no CA bundle, trusting the certificate presented on first contact and pinning it (TOFU). Must be set explicitly; without it, and without a CA bundle, enrollment is refused                         |
| `ATLAS_AGENT_NODE_ID`                          | No                                                  | (unset)                                                  | Pin the node id explicitly; otherwise derived from`/etc/machine-id`                                                                                                                                           |
| `ATLAS_AGENT_ENVIRONMENT`                      | No                                                  | `production`                                           | Operator-assigned environment tag, attached to every envelope                                                                                                                                                   |
| `ATLAS_AGENT_TRANSPORT`                        | No (default`https`)                               | `libp2p`                                               | `https` or `libp2p`                                                                                                                                                                                         |
| `ATLAS_AGENT_LIBP2P_SERVER_ADDR`               | No (deprecated)                                     | `/ip4/203.0.113.5/tcp/4102/p2p/<SERVER_PEER_ID>`       | Full manually-assembled multiaddr; ignored if the two below are set                                                                                                                                             |
| `ATLAS_AGENT_LIBP2P_RELAY_ADDR`                | Conditional                                         | `/ip4/<RELAY_IP>/tcp/<RELAY_PORT>/p2p/<RELAY_PEER_ID>` | Relay's multiaddr, for rendezvous discovery                                                                                                                                                                     |
| `ATLAS_AGENT_LIBP2P_SERVER_PEER_ID`            | Conditional                                         | `<SERVER_PEER_ID>`                                     | Control plane's Peer ID (no address)                                                                                                                                                                            |
| `ATLAS_AGENT_COLLECTION_INTERVAL`              | No (default`15s`)                                 | `15s`                                                  | Metric collection interval                                                                                                                                                                                      |
| `ATLAS_AGENT_COLLECTION_TIMEOUT`               | No (default`10s`)                                 | `10s`                                                  | Per-collector timeout                                                                                                                                                                                           |
| `ATLAS_AGENT_INVENTORY_INTERVAL`               | No (default`60s`)                                 | `60s`                                                  | Inventory push interval                                                                                                                                                                                         |
| `ATLAS_AGENT_AGENTOPS_CONTAINER_LOGS_DISABLED` | No (default`false`)                               | `false`                                                | Local opt-out of remote container-log streaming, independent of Server authorization                                                                                                                            |
| `ATLAS_AGENT_SECRET_REDACTION_DISABLED`        | No (default`false`)                               | `false`                                                | Transmit process command lines and cron commands unredacted. Redaction is on by default — see §15                                                                                                             |
| `ATLAS_AGENT_LOG_LEVEL`                        | No (default`info`)                                | `debug`                                                | `info` or `debug`                                                                                                                                                                                           |

### Additional relationships (multiple control planes at once)

| Variable                            | Required         | Example                                                       | Description                                                                                                                                                     |
| ----------------------------------- | ---------------- | ------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `ATLAS_AGENT_RELATIONSHIPS`       | No               | `production,development`                                    | Comma-separated relationship ids.`default` is reserved                                                                                                        |
| `ATLAS_AGENT_RELATIONSHIP_<ID>_*` | Per-relationship | e.g.`ATLAS_AGENT_RELATIONSHIP_PRODUCTION_CONTROL_PLANE_URL` | Every variable above, minus`DATA_DIR`/`NODE_ID`/collection tunables (which stay process-global), scoped to `<ID>` (uppercased, non-alphanumeric → `_`) |

Once a relationship first bootstraps successfully, its resolved config is persisted to `<data-dir-for-that-relationship>/relationship.json` and becomes authoritative — changing the env var afterward has no effect until that file is edited or removed (`internal/agent/relationship.go:145-179`).

### Plugin / Docker / retry / timeout configuration

- **Plugins:** no per-plugin environment variables exist for the Agent binary. Each of the six plugins self-detects (see [§12](#12-plugins)); there is no `ATLAS_AGENT_PLUGIN_*` family in `config.go`. **Not confirmed / not implemented**: no plugin-disable mechanism exists at the Agent level (contrast with the Server's `plugin.Registry.disabled`, which is driven by Server-side config, not the Agent binary).
- **Retry/backoff:** not environment-configurable; hardcoded constants — see [§14](#14-reconnection-and-failure-handling).
- **Logging:** `ATLAS_AGENT_LOG_LEVEL` only (`info`/`debug`); output is structured JSON to stdout (`cmd/atlas-agent/main.go:56`).

---

## 7. Installation

Confirmed method: **prebuilt binary + systemd**, via `packaging/atlas-agent/install.sh`.

```bash
# From source (requires Go 1.25+, per go.mod / Dockerfile)
go build -o atlas-agent ./cmd/atlas-agent

# Install as a systemd service (must run as root)
sudo ./packaging/atlas-agent/install.sh \
  --binary ./atlas-agent \
  --control-plane-url https://control-plane.example.internal:8443 \
  --token <agent-token> \
  --environment production
```

`install.sh` options (from its own usage header, `packaging/atlas-agent/install.sh:9-17`):

| Flag                        | Purpose                                                                                                                             |
| --------------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `--binary PATH`           | Agent binary to install (default:`./atlas-agent` next to the script)                                                              |
| `--control-plane-url URL` | Required                                                                                                                            |
| `--token TOKEN`           | Enrollment token (see`atlas-server enroll-token`)                                                                                 |
| `--environment NAME`      | Operator-assigned environment tag                                                                                                   |
| `--ca-bundle PATH`        | CA certificate to pin (verified bootstrap)                                                                                          |
| `--insecure-bootstrap`    | Enroll with no CA bundle, trusting the certificate presented on first contact. Required explicitly when`--ca-bundle` is not given |
| `--no-start`              | Enable the service but don't start it — edit the env file first                                                                    |
| `--force-env`             | Overwrite an existing`/etc/atlas-agent/atlas-agent.env`                                                                           |

**No Docker image is built for the Agent.** The repository's only `Dockerfile` (root) builds `atlas-server` exclusively; there is no `atlas-agent` or `atlas-relay` target. Container-based Agent deployment is **Not confirmed / not currently provided**.

---

## 8. Running the Agent

```bash
# Normal (systemd, after install.sh)
sudo systemctl start atlas-agent
sudo systemctl status atlas-agent

# Foreground, ad hoc (e.g. local testing)
export ATLAS_AGENT_CONTROL_PLANE_URL=https://127.0.0.1:8443
export ATLAS_AGENT_TOKEN=<agent-token>
export ATLAS_AGENT_DATA_DIR=/tmp/atlas-agent-data
./atlas-agent

# Debug logging
ATLAS_AGENT_LOG_LEVEL=debug ./atlas-agent

# Version / build info
./atlas-agent --version

# Full flag/env reference
./atlas-agent --help
```

There is no separate "foreground vs daemon" flag — `atlas-agent` always runs in the foreground; systemd is what daemonizes it (`Type=simple` in the unit file).

---

## 9. Deployment on Linux

Everything below is exactly what `install.sh` + `atlas-agent.service` do — nothing invented.

| Item             | Value                                                                                                                                                                                                   |
| ---------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Binary path      | `/usr/local/bin/atlas-agent` (mode `0755`, root:root)                                                                                                                                               |
| Config directory | `/etc/atlas-agent` (mode `0750`, root:atlas)                                                                                                                                                        |
| Env file         | `/etc/atlas-agent/atlas-agent.env` (mode `0640`, root:atlas) — **never overwritten** on re-run unless `--force-env`                                                                        |
| Data directory   | `/var/lib/atlas-agent` (systemd-managed `StateDirectory`, mode `0700`)                                                                                                                            |
| Service user     | `atlas` (system user, no shell, no home creation — `useradd --system --no-create-home --home-dir /var/lib/atlas-agent --shell /usr/sbin/nologin atlas`)                                            |
| Docker access    | Only if a`docker` group exists on the host: a systemd drop-in (`/etc/systemd/system/atlas-agent.service.d/10-docker-group.conf`) grants `SupplementaryGroups=docker`                              |
| Systemd unit     | `packaging/atlas-agent/atlas-agent.service`                                                                                                                                                           |
| Restart policy   | `Restart=always`, `RestartSec=5s`, `StartLimitIntervalSec=0` (no burst limit — a control-plane outage must never leave the host permanently unmonitored)                                         |
| Sandboxing       | `ProtectSystem=strict`, `ProtectHome=true`, `NoNewPrivileges=true`, `CapabilityBoundingSet=` (empty), `SystemCallFilter=@system-service`, and more — see the unit file's `[Service]` block |
| Logs             | stdout/stderr → journald (structured JSON); view with`journalctl -u atlas-agent`                                                                                                                     |

Uninstall (`packaging/atlas-agent/uninstall.sh`):

```bash
sudo ./uninstall.sh          # stops/disables/removes binary+unit; keeps data+config
sudo ./uninstall.sh --purge  # also removes /var/lib/atlas-agent, /etc/atlas-agent, and the 'atlas' user
```

---

## 10. Enrollment / Authentication

Source: `internal/agent/credentials.go`.

- **`ATLAS_AGENT_TOKEN`** is a one-time bootstrap secret. It is sent, base64-encoded alongside a freshly generated CSR and the resolved node id, to `POST {ControlPlaneURL}/api/v1/agent/enroll` (`credentials.go:88-111`).
- The Server (out of scope of this repo section — see `internal/api/agent`) validates the token and returns a signed leaf certificate plus (on first contact) the fleet's CA certificate.
- **Where it's configured:** `ATLAS_AGENT_TOKEN` in the env file, or `ATLAS_AGENT_RELATIONSHIP_<ID>_TOKEN` for a non-default relationship. Never persisted to `relationship.json` (`relationship.go:20-25`) — it is re-read from the environment on every start, but only *used* if no certificate is on disk yet.
- **When it's validated:** only at enrollment time. Once a certificate exists at `<data-dir>/agent-cert.pem` (default relationship) or `<data-dir>/relationships/<id>/agent-cert.pem`, the token is never consulted again — safe to delete from the env file.
- **Invalid token:** `enroll()` returns a non-200 response, wrapped as `"enroll: server returned %d"`, which is retried with bounded exponential backoff (2s → 30s, up to 5 minutes total) before the relationship is given up on for that run (`credentials.go:273-320`). This does not crash the process if other relationships are healthy.
- **Persistence:** the issued certificate and its private key are saved to disk via `pki.SaveLeaf` (`internal/platform/pki/store.go`). The private key **never leaves the host** and is never transmitted — only the CSR (public key + signature) crosses the network (`internal/platform/pki/csr.go:13-19`).
- **Identity association:** the certificate's Subject carries the node id; the Server's mTLS verification on every subsequent request is what associates traffic with a specific enrolled node (`internal/platform/pki`, `internal/api/agent`).
- **Certificate lifetime:** 24 hours (`pki.LeafLifetime`, `internal/platform/pki/ca.go:51`), renewed automatically once 50% elapsed (`pki.RenewAt = 0.5`), checked hourly by each relationship's independent renewal loop (`credentials.go:322-378`).

```text
ATLAS_AGENT_TOKEN=<your-agent-token>
```

---

## 11. Agent Identity

Two entirely separate identities exist per ADR-0012 (`docs/adr/0012-connect-by-identity.md`) — deliberately not unified:

| Identity                        | Purpose                                                                           | Generation                                                                                                                                                                                                     | Storage                                                                                      | Survives restart                              |
| ------------------------------- | --------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- | --------------------------------------------- |
| Node ID (`hostid`)            | Attributes every observation to a stable machine, independent of hostname/IP/DHCP | Configured explicitly, or derived via HMAC-SHA256 from`/etc/machine-id` (never the raw machine id itself — see `internal/platform/hostid/hostid.go:20-30`), or persisted state file, or freshly generated | `<data-dir>/node-id`, or `/var/lib/atlas/node-id`, or `$XDG_CONFIG_HOME/atlas/node-id` | Yes, unless nowhere is writable               |
| mTLS client certificate (X.509) | Authorization: what this Agent is allowed to do against the fleet CA              | Issued by the Server at enrollment, from a CSR the Agent generates                                                                                                                                             | `<data-dir>/agent-cert.pem` + key                                                          | Yes (renewed automatically before it expires) |
| libp2p Peer ID                  | Routing/reachability only (transport=libp2p) —*not* an authorization mechanism | Ed25519 keypair generated on first use                                                                                                                                                                         | `<data-dir>/p2p-identity.key`                                                              | Yes                                           |

The Server identifies which libp2p Peer ID currently belongs to which enrolled node id **dynamically**, from traffic (`internal/app/fleet.go:219-253`) — the Agent's libp2p Peer ID is not itself a trust credential; the mTLS certificate is what the Server actually authenticates.

---

## 12. Plugins

### Architecture (`internal/core/plugin`)

A plugin has four lifecycle stages: **Registered → Detected → Initialised → Closed** (`internal/core/plugin/plugin.go:7-16`). Detection is what lets one binary run correctly across a heterogeneous fleet — a host with no Docker daemon reports "no Docker integration," not a broken one. Plugins are compiled in, not dynamically loaded (`docs/adr/0006-compiled-in-plugins.md`).

- `Descriptor()` — static ID/name/description, never changes.
- `Detect(ctx)` — cheap, side-effect-free presence check, run for every registered plugin at startup.
- `Init(ctx, env)` — called only if Detect returned true; contributes collectors (and, optionally, streamers).
- `Close(ctx)` — releases resources at shutdown, idempotent.

A plugin that fails detection or init does **not** stop the Agent or any other plugin — every outcome is recorded as one of `active`, `not_detected`, `detection_failed`, `init_failed`, `disabled` (`plugin.go:139-155`) and logged individually.

### Plugins registered by the Agent binary (`internal/agent/agent.go:163-175`)

| ID          | Name           | Detects                             | Contributes                                                               |
| ----------- | -------------- | ----------------------------------- | ------------------------------------------------------------------------- |
| `system`  | System         | Host facts readable                 | CPU, memory, swap, disk, network, load, mounts                            |
| `docker`  | Docker         | Docker daemon present and answering | Containers, resource use, health, images, networks, volumes, events, logs |
| `process` | Processes      | Processes can be enumerated         | Process counts by state, heaviest consumers, live inventory               |
| `service` | Services       | A service manager is present        | systemd unit states, failures, restarts, resource use                     |
| `cron`    | Scheduled jobs | Any cron source is readable         | System/user/packaged cron jobs and schedules                              |
| `ports`   | Ports          | Connection table can be read        | Listening TCP/UDP ports, TLS certificate expiry behind them               |

### Failure behavior

Per-plugin, independent, non-fatal to the process (`internal/core/plugin/registry.go:103-133`). `Detect` is even run with panic recovery (`registry.go:196-208`) — a panic in one plugin's detection cannot take down activation of the rest.

---

## 13. Telemetry

Confirmed behavior only (`internal/core/collect`, `internal/agent/inventory.go`, `internal/agent/events.go`, `internal/core/transport/remote`):

- **What's collected:** whatever the active plugins' collectors produce — metrics on their own interval (default 15s, `scheduler.New` in `agent.go:190-205`), plus periodic inventory snapshots (default 60s).
- **Where it goes:** every relationship's own `remote.Transport`, via a shared `fanoutTransport` (`internal/agent/fanout.go`) — the same observation is sent to every configured Server.
- **When it's sent:**
  - Metrics: on the scheduler's configured interval, per collector.
  - Inventory: on `ATLAS_AGENT_INVENTORY_INTERVAL`, but **only if the subject's content hash changed** since the last successful push (`inventory.go:115-156`) — a stable host produces very little steady-state traffic.
  - Events: forwarded from the local event bus as they occur, not batched (`events.go:25-48`).
- **Delivery classes** (`internal/core/transport/payload.go:45-59`):
  - `ClassStream` (metrics, events) — spooled to disk if delivery fails, replayed later; never silently lost.
  - `ClassSnapshot` (some inventory) — sent immediately, dropped on failure (a stale snapshot has no value once a newer one exists).
- **Connection/status reporting:** no distinct "status" message type was found — see the heartbeat discrepancy note in [§2](#2-agent-responsibilities). Liveness is observable only via the recurring telemetry/inventory traffic itself.
- **Error reporting:** transport-level failures are logged (`slog.Warn`/`slog.Error`) on the Agent side; they are not themselves telemetry payloads sent to the Server.

---

## 14. Reconnection and Failure Handling

| Scenario                                                             | Behavior                                                                                                                                                                                                      | Source                                                                  |
| -------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------- |
| Server unreachable during bootstrap (enroll)                         | Bounded exponential backoff: 2s → doubling → capped at 30s, jittered, up to 5 minutes total elapsed, then that relationship fails for this run (others unaffected)                                          | `credentials.go:273-320`                                              |
| Server unreachable during steady-state telemetry                     | `remote.Transport`'s replay loop retries with the same doubling backoff (1s → 2 minutes), spooling everything to disk in between                                                                           | `internal/core/transport/remote/remote.go:179-238`                    |
| Server rejects/throttles (`directive: "slow_down"` or `"pause"`) | Transport sleeps for the Server-specified`retry_after_ms` (or one poll interval, 2s, if unspecified) before resuming                                                                                        | `remote.go:225-235`                                                   |
| Relay unreachable during rendezvous lookup                           | Falls back to the last cached dial result (`p2p-last-known.json`); if that also fails, forces one fresh lookup before giving up on that attempt                                                             | `discovery.go:96-139`                                                 |
| Rendezvous has no record for the Server's Peer ID                    | `"relay has no announced address for peer %s"`; same cache-then-fresh-retry fallback applies                                                                                                                | `discovery.go:77-94`                                                  |
| Authentication fails (bad/expired token, denied node)                | Enrollment/renewal HTTP call returns non-200; enrollment retries per above, renewal logs and retries on the next hourly tick                                                                                  | `credentials.go:230-233`, `327-358`                                 |
| Certificate renewal fails                                            | Logged, retried on the next hourly check; the still-valid (not-yet-expired) certificate keeps being used in the meantime                                                                                      | `credentials.go:355-358`                                              |
| One relationship fails entirely (never bootstraps)                   | Dropped; does not prevent any other relationship from running. The whole Agent only fails to start if**every** relationship fails                                                                       | `agent.go:83-153`                                                     |
| Agent process restarts                                               | Loads its persisted cert/CA/spool/`relationship.json`/node-id/Peer-ID from disk — resumes without re-enrolling                                                                                             | `credentials.go:196-211`, `relationship.go:145-179`                 |
| Server restarts                                                      | No special handling needed — the next telemetry attempt or renewal simply succeeds or is retried like any other failure above                                                                                | (inferred from the retry model; no Server-restart-specific code exists) |
| Relay restarts                                                       | Rendezvous registry is in-memory only and is lost (`libp2ptransport.go:337-341`); every Server re-announces on its own renewal tick (well within minutes) — a fresh Agent lookup after that succeeds again | `internal/core/transport/libp2ptransport/libp2ptransport.go`          |
| Systemd process crash / OOM / panic                                  | `Restart=always`, `RestartSec=5s`, no burst limit                                                                                                                                                         | `atlas-agent.service:19-20`                                           |

Spooled data survives an Agent restart on disk (`<data-dir>/spool`, bounded to 512MB / 24h by default — `internal/core/transport/spool/spool.go:27-36`); entries older than 24h are discarded on load rather than replayed, matching the Server's own idempotency window.

---

## 15. Security

- **Authentication:** mutual TLS (TLS 1.3 minimum). The Agent presents a Server-issued client certificate on every request after enrollment; the Server verifies it against the fleet CA.
- **Tokens:** `ATLAS_AGENT_TOKEN` is single-use in effect (consumed at enrollment, then irrelevant) and is never persisted to `relationship.json`.
- **Identity:** node id is HMAC-derived, never the raw `/etc/machine-id` — deliberately not directly correlatable across systems (`hostid.go:20-30`).
- **Secret redaction:** process command lines and cron commands are collected in full (they are how an operator tells one JVM or backup job from another) but passed through pattern-based redaction before transmission — `--password=`, `--token=`, `--api-key=`, `-pSECRET`, `Authorization: Bearer …`, and URL userinfo become `[REDACTED]`, while ordinary arguments (`--port 8080`, `-Xmx4g`, `-jar app.jar`) are untouched. On by default; `ATLAS_AGENT_SECRET_REDACTION_DISABLED=true` turns it off for a trusted debugging session. Redaction defends against accident, not against a determined operator: a credential passed in an unrecognised application-specific flag still gets through, so collected command lines remain sensitive data.
- **CA trust:** either an operator-supplied `ATLAS_AGENT_CA_BUNDLE` (verified bootstrap) or an explicit `ATLAS_AGENT_INSECURE_BOOTSTRAP=true` trust-on-first-use, logged as a warning every time TOFU is used (`credentials.go:256-257`): *"pinned the control plane's CA on first contact (TOFU); set a CA bundle path for a verified bootstrap in production."* Once pinned, the CA is persisted and never re-trusted from a new source without operator intervention.
- **libp2p transport encryption:** libp2p's own Noise-protocol encrypted channel underneath; Atlas's own mTLS then runs *inside* that stream — two independent layers, neither substituting for the other (see `docs/adr/0012-connect-by-identity.md`).
- **Private keys never transmitted:** both the CSR keypair and the libp2p identity keypair are generated locally and never leave the host.
- **Exposed ports:** none. The Agent is dial-only for both HTTPS and libp2p transports.
- **AgentOps (container logs) authorization is two-layered:** the Server must independently grant the operation per node (`fleet.grants`, `internal/core/fleet/grants.go` — not itself part of this Agent), *and* the Agent has a local kill-switch, `ATLAS_AGENT_AGENTOPS_CONTAINER_LOGS_DISABLED`, independent of anything the Server decides (`config.go:48-56`).
- **Trust boundary:** the Server is trusted based on its certificate chaining to the pinned CA (for HTTPS/mTLS) — and, for libp2p, additionally checked against the specific control-plane identity derived from that CA's own fingerprint, not merely "any cert this CA happens to have signed" (`libp2ptransport/agentops.go:174-205`). The Relay is **not** part of this trust boundary at all — see [RELAY_README.md §14](RELAY_README.md#14-security).

---

## 16. Troubleshooting

### Agent cannot start

- **Symptoms:** `systemctl status atlas-agent` shows `failed`; process exits immediately.
- **Check:** `journalctl -u atlas-agent -n 100`; verify `ATLAS_AGENT_CONTROL_PLANE_URL` is set (required, though it has a default); verify `/var/lib/atlas-agent` is writable by the `atlas` user.
- **Likely cause:** malformed env file, unwritable data directory, or (if any relationship uses libp2p) a malformed relay/server-peer-id multiaddr.
- **Fix:** correct the env file; `sudo systemctl daemon-reload && sudo systemctl restart atlas-agent`.

### Agent cannot enroll

- **Symptoms:** repeated `"bootstrap attempt failed, retrying"` log lines; eventually `"bootstrap did not succeed within 5m0s"`.
- **Check:** `journalctl -u atlas-agent | grep enroll`; confirm `ATLAS_AGENT_TOKEN` is set and unexpired; confirm the Server is reachable at `ATLAS_AGENT_CONTROL_PLANE_URL`.
- **Likely cause:** wrong/expired/already-used token, or the Server is unreachable from this host at all (network/firewall, not an Atlas bug).
- **Fix:** issue a fresh token (`atlas-server enroll-token`, on the Server), correct the URL, retry.

### Agent cannot discover Server (rendezvous)

- **Symptoms:** `"rendezvous lookup failed, trying cached address"`, then `"relay has no announced address for peer %s"`.
- **Check:** confirm `ATLAS_AGENT_LIBP2P_SERVER_PEER_ID` exactly matches the Server's actual current Peer ID (see [RELAY_README.md §16](RELAY_README.md#16-troubleshooting) for the matching check on the Relay/Server side); confirm the Relay is reachable and its Peer ID in `ATLAS_AGENT_LIBP2P_RELAY_ADDR` is correct.
- **Likely cause:** stale/incorrect Server Peer ID, or the Server has not (yet) announced to this Relay.
- **Fix:** re-verify the Server's real Peer ID from its own startup log; confirm the Server's `ATLAS_FLEET_LIBP2P_RELAY_ADDR` points at the same Relay the Agent is using.

### Agent cannot connect directly

- **Symptoms:** the first dial candidate(s) time out (8s each), then a relay-circuit candidate succeeds — or all candidates fail.
- **Likely cause:** the Server's direct address is not externally reachable (NAT/firewall) — this is expected in most deployments; the relay circuit is the intended fallback.
- **Fix:** none needed if a relay circuit is configured and succeeds. If no relay is configured and no direct address is reachable, connectivity cannot succeed at all — configure a Relay.

### Agent cannot connect through Relay

- **Symptoms:** `"all dial candidates failed"`.
- **Check:** the Relay process is running and reachable on its configured TCP port (default 4103) from this host; confirm no firewall blocks outbound TCP to the Relay's address.
- **Likely cause:** Relay down, wrong port/IP in `ATLAS_AGENT_LIBP2P_RELAY_ADDR`, or a network path issue between Agent and Relay.
- **Fix:** verify the Relay is up ([RELAY_README.md §11](RELAY_README.md#11-running-the-relay)); test raw TCP reachability (`nc -vz <relay-ip> <relay-port>`).

### Rendezvous discovery fails

- Same checks as "cannot discover Server" above. Also confirm the Server itself is actually running and has completed its own relay reservation/announce cycle — see [RELAY_README.md §9](RELAY_README.md#9-rendezvous--discovery).

### Authentication fails

- **Symptoms:** enroll/renew HTTP calls return non-200; `"enroll: server returned %d"`.
- **Check:** token validity, CA bundle correctness (if configured), system clock skew (certificate validity windows).
- **Fix:** correct the token or CA bundle; if TOFU was used and the pinned CA is now wrong (e.g. Server was reinstalled with a new CA), remove `<data-dir>/ca-cert.pem` and the leaf cert to force re-enrollment.

### Agent connects but Server cannot communicate

- Not a distinct failure mode found in the code — every Server-facing operation (enroll, renew, telemetry) is a single request/response over the same authenticated connection. If the connection succeeds but requests fail, check the HTTP status/error the Server returned in the Agent's logs.

### Telemetry is missing

- **Check:** `<data-dir>/spool` — a growing spool directory means the Agent is collecting but cannot deliver (connectivity or auth issue above). An empty spool with no data on the Server side means collection itself isn't happening — check plugin activation logs.
- **Check plugin activation:** `journalctl -u atlas-agent | grep "plugin active\|plugin failed\|not_detected"`.

### Plugin does not load

- **Symptoms:** `"plugin subject not present on this host"` (expected/benign) vs `"plugin failed to initialise"` (a real fault).
- **Check:** the specific plugin's `Detect`/`Init` error in the log, tagged with `plugin_id`.
- **Fix:** depends entirely on the plugin — e.g. for `docker`, confirm `/var/run/docker.sock` is reachable and the `atlas` user has the `docker` supplementary group (re-run `install.sh` after creating the `docker` group if it didn't exist at install time).

---

## 17. Logs and Diagnostics

- **Location:** stdout/stderr, captured by systemd → journald.
- **View:** `journalctl -u atlas-agent -f` (follow), `journalctl -u atlas-agent --since "10 min ago"`.
- **Format:** structured JSON (`slog.NewJSONHandler`, `cmd/atlas-agent/main.go:56`) — pipe through `jq` for readability: `journalctl -u atlas-agent -o cat | jq .`.
- **Levels:** `info` (default) or `debug`, via `ATLAS_AGENT_LOG_LEVEL`.
- **Key log messages to watch for:**
  - `"identity resolved"` — startup, confirms node id source.
  - `"agent running"` — confirms which relationships are active and their control-plane URLs (`agent.go:453-458`).
  - `"enrolled"` / `"pinned the control plane's CA on first contact (TOFU)"` — enrollment outcome.
  - `"certificate renewed"` — renewal succeeded.
  - `"dialing control plane via rendezvous discovery"` / `"dialing control plane by static multiaddr"` — which libp2p dial strategy is active.
  - `"rendezvous lookup failed, trying cached address"` — Relay degraded, using cache.
  - `"telemetry delivery failed, will retry"` — Server unreachable, spooling.
  - `"plugin active"` / `"plugin subject not present on this host"` / `"plugin failed to initialise"` — per-plugin activation outcome.
- **Verify connectivity quickly:** `--version` confirms the binary runs at all; `systemctl status atlas-agent` for process health; `journalctl -u atlas-agent -f` while restarting to watch the full bootstrap sequence live.

---

## 18. Operational Checklist

```text
Agent deployment checklist

[ ] Binary built/installed (go build ./cmd/atlas-agent, or install.sh)
[ ] /etc/atlas-agent/atlas-agent.env created with CONTROL_PLANE_URL and TOKEN
[ ] Token obtained from the Server (atlas-server enroll-token)
[ ] (libp2p only) Relay address and Server Peer ID confirmed correct
[ ] systemd service enabled and started
[ ] Node identity resolved (check "identity resolved" in logs)
[ ] Enrollment succeeded (check "enrolled" in logs; agent-cert.pem exists)
[ ] (libp2p only) Rendezvous discovery succeeded, or relay circuit connected
[ ] Server connectivity confirmed ("agent running" log line lists the relationship)
[ ] Plugins activated as expected (check "plugin active" entries match the host's actual services)
[ ] Telemetry visible on the Server side
[ ] Spool directory not growing unboundedly (confirms steady-state delivery)
[ ] Reconnection tested (stop the Server briefly, confirm spool grows then drains on restart)
[ ] (multi-relationship only) Every configured relationship id appears in "agent running"
```
