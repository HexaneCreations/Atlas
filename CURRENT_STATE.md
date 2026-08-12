# Atlas — Current State Handoff Document

Generated as a handoff for a fresh Claude session continuing this work. Every
claim below was verified against the actual repository contents at the time
of writing (commit `0f1bf9a`, branch `main`), not recalled from memory or
inferred. Where something is unknown or unverified, it is stated as such
rather than guessed.

---

## 1. Project Overview

**Atlas** is an infrastructure observability platform. Its own tagline
(`web/src/pages/../` sidebar copy): "Observe everything. Control nothing." —
it is deliberately read-only: there is no code path anywhere in the
repository that lets the Control Plane mutate a monitored host's state
(no restart, no exec, no process control). The one exception is streaming a
container's existing logs back to an operator, which is a read, not a
mutation.

**Current architecture — components:**

- **Control Plane** (`atlas-server`, `cmd/atlas-server`) — the central
  service: HTTP API, the built React UI (served separately in production),
  PostgreSQL-backed storage, and the "Fleet" subsystem that lets remote
  Agents enroll and push telemetry.
- **Agent** (`atlas-agent`, `cmd/atlas-agent`) — runs on a monitored host,
  collects local metrics/inventory/logs, pushes them to exactly one
  configured Control Plane over mTLS.
- **Relay** (`atlas-relay`, `cmd/atlas-relay`) — a minimal libp2p
  circuit-relay-v2 bootstrap. Lets a NAT'd Control Plane and a NAT'd Agent
  reach each other by Peer ID. Carries no Atlas business logic, no Postgres,
  no HTTP API, no mTLS termination — forwards already-encrypted bytes it
  never authenticates or decrypts.
- **UI** (`web/`) — a Vite + React 19 + TypeScript single-page app, built to
  static assets and served (in production) by nginx, which also
  reverse-proxies `/api`, `/healthz`, `/readyz` to the Control Plane.
- **PostgreSQL** — required, with the **TimescaleDB extension** (hard
  requirement — migration `0001` installs it; a Postgres without the
  extension available fails to migrate).
- **libp2p** — an alternate transport (ADR-0012) alongside plain HTTPS,
  letting an Agent reach the Control Plane by Peer ID through a Relay when
  direct address-based reachability isn't available (NAT). Used for both the
  Agent's ordinary enroll/renew/telemetry traffic and for one
  Control-Plane-initiated operation (remote container log streaming).
- **TLS/PKI** — the Control Plane runs its own private CA
  (`internal/platform/pki`), no external PKI dependency. Documented in full
  in §6.

---

## 2. Current Repository Structure

Only the directories/files relevant to Control Plane / Agent / Relay /
Fleet / security / deployment are listed — the full tree is much larger
(plugins, other API domains, etc.) and out of scope for this handoff.

```
cmd/
  atlas-server/          entry point for the Control Plane binary
  atlas-agent/            entry point for the Agent binary
  atlas-relay/            entry point for the Relay binary

internal/
  app/                    composition root — wires config, DB, Fleet,
                          API router, lifecycle. fleet.go is the agent-
                          facing mTLS + libp2p listener pipeline.
  api/
    agent/                Agent-facing HTTP handlers: enroll, renew,
                          telemetry, heartbeat (handler.go)
    v1/                   Operator-facing HTTP API (containers, nodes,
                          metrics, etc.) — router.go, containers.go, etc.
  core/
    fleet/                Domain logic: enrollment tokens, credential
                          issuance/renewal rules (enroll.go, token.go)
    transport/
      libp2ptransport/     libp2p host setup, rendezvous discovery,
                          AgentOps protocol (agentops.go)
  agent/                   Agent process implementation: identity
                          resolution, bootstrap/enroll, renewal loop,
                          credential holder (agent.go, credentials.go,
                          agentops.go)
  relay/                   Relay implementation (relay.go, config.go)
  platform/
    pki/                   The private CA: ca.go, tls.go, csr.go, store.go
    hostid/                Stable node-id resolution (see §4)
    config/                ATLAS_* environment variable binding
  storage/
    fleet/                 Postgres-backed TokenStore/CredentialStore/
                          DenylistStore (repository.go)

migrations/                0001–0009, forward-only SQL migrations
docs/
  architecture/security.md   The proposed security architecture (read,
                          not yet implemented — see §7, §12–17)
  adr/0012-connect-by-identity.md   libp2p/relay design rationale
  operations/deployment.md   Production deployment guide (pre-dates the
                          Docker Compose work in this project)

docker-compose.yml          Development-only Postgres (untouched throughout
                          this project)
docker-compose.prod.yml     Production Compose stack (this project)
Dockerfile                  Production atlas-server image (untouched)
web/Dockerfile, web/nginx.conf, web/.dockerignore   Production UI image (new)
.env.example, .env.prod.example   Dev and prod environment templates (new)
Makefile                    Existing CI/dev targets, plus new server/ui/
                          dev/env-check targets (see §9)
```

---

## 3. Current Control Plane Implementation

**Startup** (`internal/app/app.go`, composition root): loads config
(`internal/platform/config`), starts the Postgres pool, runs migrations (if
`ATLAS_DATABASE_MIGRATE_ON_START=true`), builds the collection pipeline, the
Fleet pipeline, and the HTTP API router, then starts every component via a
lifecycle manager.

**HTTP server**: binds `ATLAS_SERVER_HOST:ATLAS_SERVER_PORT` (default
`0.0.0.0:8080` inside the production image; `127.0.0.1:8080` by Go-level
default otherwise). Serves the operator-facing API under `/api/v1/...` plus
`/healthz` and `/readyz`.

**Fleet** (`internal/app/fleet.go`): a second, separate listener pipeline,
active only when `ATLAS_FLEET_ENABLED=true`. On start it:
1. Loads or creates the Control Plane's own private CA
   (`pki.LoadOrCreateCA(Fleet.DataDir, "atlas-control-plane")`).
2. Mints its own TLS server leaf certificate signed by that CA.
3. Starts an mTLS HTTPS listener (`Fleet.Addr()`, default port `8443`)
   serving enroll/renew/telemetry/heartbeat.
4. If `ATLAS_FLEET_LIBP2P_ENABLED=true`, additionally starts a libp2p host
   and a **second** listener for the identical HTTP surface, reachable by
   Peer ID instead of network address (see §3's libp2p subsection).
5. If `ATLAS_FLEET_LIBP2P_RELAY_ADDR` is set, reserves a relay circuit slot
   and announces it to the Relay's rendezvous registry, so Agents can
   discover this Control Plane's current address without a manually
   assembled multiaddr.

**Agent enrollment** — see §6 in detail; summary: bounded, single-use
(or multi-use, capped) tokens, hashed at rest, redeemed atomically, gate
issuance of a short-lived (24h) client certificate.

**Agent communication** (`internal/api/agent/handler.go`): four routes —
`POST /api/v1/agent/{enroll,renew,telemetry,heartbeat}`. Every route except
`enroll` requires and reads a verified mTLS client certificate; node identity
is always re-derived from that certificate (`pki.PeerNodeID`), never trusted
from the request body.

**AgentOps** (`internal/core/transport/libp2ptransport/agentops.go`): a
**separate, narrow** libp2p protocol (`/atlas/agentops/1.0.0`) the Control
Plane uses to ask an already-connected Agent to stream one specific
container's logs. It is Control-Plane-initiated (reversed roles: Control
Plane is the TLS client, Agent is the TLS server) over a connection the
Agent itself already established by dialing out — the Agent never gains a
new inbound listening surface. Exactly one operation exists
(`container_logs`); there is no generic command dispatch. Full detail in §7.

**libp2p**: `internal/core/transport/libp2ptransport` — host construction,
rendezvous-based discovery (Agent looks up the Control Plane's current
direct/circuit address via the Relay rather than needing a hand-assembled
multiaddr), dial-with-fallback (direct then circuit), and the AgentOps
protocol above. This is a real, working, previously-verified transport (a
live 3-machine Mac↔Relay↔Linux-Agent test passed earlier in this project),
not a stub.

**WebSockets**: used for two things, both operator-facing (browser ↔
Control Plane, not Agent-facing): live container log follow
(`GET /api/v1/containers/{id}/logs/follow`) for a local container, and the
same endpoint proxied to a remote Agent's `container_logs` AgentOps session
for a non-local node — the frontend code is identical either way
(`internal/api/v1/containers.go`'s `followLogs`, shared by both paths).

**Persistence**: PostgreSQL only, via `internal/storage/*` repositories over
`pgxpool`. No other datastore. The Control Plane's own CA key/cert live on
disk under `Fleet.DataDir`, not in the database (see §6).

**Configuration**: `internal/platform/config` — `ATLAS_*` prefixed
environment variables, layered over compiled-in defaults and an optional
YAML file. Documented in full in `internal/platform/config/config.go`'s
package doc; the specific variables this project's deployment work uses are
listed in §9.

---

## 4. Current Agent Implementation

**Stable node identity** (`internal/platform/hostid`): resolved once at
Agent startup, in order — (1) explicitly configured id
(`ATLAS_AGENT_NODE_ID`), (2) derived from the OS's `/etc/machine-id` (or the
D-Bus fallback path), HMAC-SHA256'd under a fixed domain-separator key so
the raw machine-id (documented by systemd as confidential) is never exposed,
(3) a previously persisted state file (`DataDir/node-id`), (4) freshly
generated and persisted. **Explicitly not** derived from hostname, IP,
container ID, or MAC address. This is a genuinely durable identity
mechanism and is the one to reuse for any future work — see §12.

**Certificates / private key**: an ECDSA P-256 keypair is generated fresh
by `pki.NewCSR` at first enrollment **and again at every certificate
renewal** (the key itself rotates roughly every 12h, at half of the 24h
certificate lifetime). The current cert+key pair is persisted at
`DataDir/agent-cert.pem` and `DataDir/agent-key.pem` (key file mode `0600`).
The private key is generated locally and never transmitted — only the CSR
(public key + self-signature) crosses the network.

**CA trust**: the Agent pins **exactly one** CA certificate for its entire
process lifetime, either from `ATLAS_AGENT_CA_BUNDLE` (a pre-distributed
file) or, if unset, via trust-on-first-use (TOFU) at first enrollment — the
CA presented by the Control Plane's enroll response is accepted once and
then persisted to `DataDir/ca-cert.pem` for every subsequent run, so a
restart does not repeat TOFU.

**Enrollment**: if `DataDir` holds no existing certificate, the Agent
requires `ATLAS_AGENT_TOKEN` to be set, generates a fresh CSR, and calls
`POST /api/v1/agent/enroll` with bounded exponential-backoff retry (up to
5 minutes total). On success it persists the issued certificate and (if not
already pinned) the CA.

**Renewal**: a background loop checks hourly whether the current certificate
has crossed 50% of its lifetime and, if so, generates a fresh CSR and calls
`POST /api/v1/agent/renew` over the existing mTLS connection — no token
involved; the currently valid certificate is the proof of identity.

**`ControlPlaneURL`**: a single configured value
(`ATLAS_AGENT_CONTROL_PLANE_URL`, default `https://127.0.0.1:8443`). The
Agent process has **exactly one** target Control Plane for its entire
lifetime — there is no list, no multiple-target concept anywhere in
`internal/agent`.

**`DataDir`** (`ATLAS_AGENT_DATA_DIR`, default `/var/lib/atlas-agent`):
holds `node-id`, `agent-cert.pem`, `agent-key.pem`, `ca-cert.pem`, and (if
the libp2p transport is active) the libp2p host's own identity material.
Must be a persistent volume/directory across restarts for identity and
trust to survive — this repository's Docker Compose work did **not** touch
Agent deployment (Agent runs under systemd on a separate Linux host, per
this project's stated architecture), so whether that host's `DataDir` is
already on durable storage was not verified as part of this project.

**libp2p**: when `ATLAS_AGENT_TRANSPORT=libp2p`, the Agent starts a
dial-only libp2p host (`NoListenAddrs` — it only ever dials out, never
accepts inbound connections) and routes its enroll/renew/telemetry HTTP
calls over a libp2p stream instead of a plain TCP dial. Two addressing
modes: rendezvous discovery (`ATLAS_AGENT_LIBP2P_RELAY_ADDR` +
`ATLAS_AGENT_LIBP2P_SERVER_PEER_ID` — looks up the Control Plane's current
address via the Relay on every dial) or the deprecated static
`ATLAS_AGENT_LIBP2P_SERVER_ADDR` (a hand-assembled multiaddr, kept for
backward compatibility).

**AgentOps**: when the libp2p transport is active, the Agent additionally
registers a handler for the AgentOps protocol on the same dial-only host —
this is what lets the Control Plane ask it (over the connection the Agent
itself made) to stream one container's logs. An HTTPS-only Agent has no such
connection for the Control Plane to reuse, so remote log streaming is simply
unavailable for it (by design, not a bug).

**Persistent state, summary**: everything under `DataDir` — `node-id`,
current cert/key, pinned CA cert, and (libp2p mode) the libp2p host key.
All file-based, no database, no external dependency.

---

## 5. Current Relay Implementation

**Architecture**: `internal/relay/relay.go` — a minimal libp2p
circuit-relay-v2 node. It runs on a publicly reachable host and does
nothing but forward already-encrypted streams between peers that dial
through it. No Postgres, no HTTP API, no mTLS termination, no Atlas
business logic of any kind.

**Transport behavior**: pure circuit-relay-v2 — a Control Plane and an
Agent, each behind NAT, both dial out to the Relay; the Relay lets them
establish a relayed connection to each other. It never decrypts or inspects
the payload passing through.

**Ports**: `ATLAS_RELAY_LISTEN_ADDRS`, default
`/ip4/0.0.0.0/tcp/4103` (`internal/relay/config.go`).

**systemd deployment**: per this project's stated architecture, the Relay
runs under systemd on a separate Linux server, independent of the
Control Plane's own deployment lifecycle. **No systemd unit file or
deployment script for the Relay exists in this repository** — its
deployment is managed entirely outside this codebase; this project did not
create, inspect, or modify one.

**Authentication/authorization**: confirmed — **none**. Verified by reading
`internal/relay/relay.go` in full: no certificate verification, no peer
authorization list, no concept of "who may relay through me." This is
correct and intentional per this project's explicit requirement that the
Relay remain a pure transport component; authorization must happen only
between Control Plane and Agent identities, never at the Relay.

**What must NOT be changed**: the Relay's code
(`internal/relay/`), its configuration contract
(`ATLAS_RELAY_*` variables), and its deployment (systemd, external to this
repo) were not touched by this project and must remain untouched by any
future work arising from this handoff unless a design explicitly requires
it and is separately approved.

---

## 6. Current PKI / Security Model

**CA generation**: `pki.New(commonName)` — ECDSA P-256 root key, self-signed,
`RootLifetime = 10 years`. Exactly one CA per Control Plane deployment (i.e.
per `Fleet.DataDir`) — there is no concept of a CA relating to any other CA.

**CA persistence**: `pki.LoadOrCreateCA(dir, commonName)` —
`dir/ca-cert.pem` + `dir/ca-key.pem`, generated once at first Control Plane
start, loaded on every subsequent start. In the production Compose stack
built by this project, `dir` = `ATLAS_FLEET_DATA_DIR`, mounted from the
`atlas-fleet-data` named volume (see §9) — this is what makes the CA (and
therefore every Agent's trust in this Control Plane) survive container
recreation.

**Agent certificates**: signed by the Control Plane's CA via
`ca.IssueLeaf(csr, nodeID)`. The CSR's own subject/SAN list is ignored — the
issued certificate carries exactly one identity, the node ID the enrollment
flow decided to grant, as a **URI SAN** in the form `atlas://node/<id>`
(deliberately not the Common Name, which has no defined structure).
`Subject.CommonName` on an agent leaf is set to the node ID as a
human-readable label only — the URI SAN is authoritative
(`pki.PeerNodeID` reads only the URI SAN).

**Control Plane certificates**: `pki.NewServerLeaf(ca, hosts)` — signed by
the same CA, `Subject.CommonName` is always the fixed constant
`"atlas-control-plane"` (`pki.ControlPlaneCommonName`), valid for the
DNS names / IPs in `Fleet.AdvertisedHosts`. `ServerLeafLifetime` = 397 days
(just under the public-CA/Browser-Forum ceiling, chosen out of habit even
though this is a private PKI).

**Certificate lifetime**: agent leaf = 24h (`pki.LeafLifetime`); Control
Plane server leaf = 397 days; CA root = 10 years.

**Renewal**: agent leaf renews automatically at 50% of its lifetime
(`pki.RenewAt = 0.5`, i.e. every ~12h) — see §4. There is no automatic
renewal mechanism for the Control Plane's own server leaf; an operator
restart is what refreshes it, per the code's own comment (no hot-reload
implemented).

**SANs**: agent leaf → one URI SAN, `atlas://node/<id>`. Control Plane
server leaf → DNS/IP SANs from `Fleet.AdvertisedHosts`, no URI SAN.

**CommonName**: agent leaf → the node ID (informational only). Control
Plane server leaf → always the literal string `"atlas-control-plane"`,
identical across every possible Control Plane deployment — this string
alone cannot distinguish one Control Plane instance from another; only
"which CA signed this leaf" can, today.

**Certificate verification / node identity verification**: two distinct
paths, both implemented, both verified by reading the code directly:
- **Ordinary HTTPS (Agent → Control Plane)**: `pki.ServerTLSConfig` sets
  `ClientAuth: VerifyClientCertIfGiven` (not `RequireAndVerifyClientCert`,
  because the `enroll` route must be reachable with no certificate at all).
  Every other handler (`renew`, `telemetry`, `heartbeat`) independently
  checks for and derives identity from `r.TLS.PeerCertificates[0]` via
  `pki.PeerNodeID` — never trusts a request-body claim.
- **Reversed AgentOps (Control Plane → Agent, over libp2p)**: both sides use
  `InsecureSkipVerify: true` plus an explicit `VerifyPeerCertificate`
  callback, because standard hostname verification has no meaning on a raw
  libp2p stream. The Control Plane's check
  (`verifyAgentCertificate`) confirms chain-to-CA **and**
  `pki.PeerNodeID(cert) == expectedNodeID` — it will only accept a response
  from the exact node it meant to reach. The Agent's check
  (`verifyControlPlaneCertificate`) confirms chain-to-CA **and**
  `cert.Subject.CommonName == "atlas-control-plane"` — see §7 for why this
  is currently a materially weaker check than the Control Plane's side.

**Denylist**: `node_denylist` table — immediate ejection that does not wait
for a certificate to expire. Checked on every `telemetry`/`heartbeat`/
`renew`/`enroll` call.

**Enrollment tokens**: 256 bits of entropy, prefixed `atlas_enroll_` (so a
secret scanner or a log reader can recognise one on sight), SHA-256 hashed
at rest — plaintext is shown to the operator exactly once at creation and
never stored, and there is no "reveal token" API. Bounded by `max_uses`,
`expires_at`, an optional `allowed_cidr`, and an `allow_reenroll` flag
(off by default — this is what stops a stolen token from taking over an
already-enrolled node's identity). Redemption is a single atomic
conditional `UPDATE`, race-safe against concurrent enrollments spending the
same token.

---

## 7. Current Authorization Model

**Agent → Control Plane authentication**: strong. Every authenticated route
re-derives node identity from the *verified* TLS peer certificate
(`pki.PeerNodeID`), never from the request body. A telemetry envelope
claiming a different node ID than the connection's own certificate is
rejected outright and logged as `"identity_mismatch"`.

**Control Plane → Agent authentication** (the reversed AgentOps
handshake): the Agent verifies the presented certificate chains to its one
pinned CA **and** carries `CommonName == "atlas-control-plane"`. Because
that CommonName is identical for every possible Control Plane deployment,
and because the Agent today only ever pins one CA, this check currently
reduces to "chains to the one CA I trust" — it does not, and structurally
cannot yet, distinguish *which* Control Plane if more than one CA were ever
trusted at once.

**Current authorization** (as opposed to authentication): for the
privileged AgentOps `container_logs` operation, the Control Plane looks up
the requested node's last-known libp2p Peer ID in an in-memory map
populated by *any* successfully authenticated request that node has ever
made (enroll, renew, or telemetry) — there is **no separate, explicit,
revocable record** of "this node is authorized for privileged operations"
distinct from "this node has a live, non-denylisted certificate." A
successful, valid connection is currently sufficient authorization for the
one privileged operation that exists.

**AgentOps authorization**: as above — authentication (valid cert, correct
node) is currently being used as the sole authorization gate. No second,
independent check exists on either side.

**User authentication**: does not exist. No middleware, no session/cookie/
token handling anywhere in `internal/api`. Verified by reading
`internal/api/router.go` in full and grepping for auth-related terms —
none found.

**User authorization / RBAC**: does not exist, for the same reason — there
is no user identity for it to attach to yet. `scopeFrom(r)` (used
throughout `internal/api/v1`) reads a plain, unauthenticated `?node=` query
parameter — any caller able to reach the HTTP port can request any node's
data.

**Known security gaps** (see also §14 for the full itemized list with
severity/blocking assessment):
1. No mechanism anywhere in the Agent for holding more than one Control
   Plane trust relationship simultaneously — `internal/agent`'s `Agent`
   struct holds exactly one CA cert, one credential holder, one
   `ControlPlaneURL`. This is the central blocker for the multi-Control-
   Plane requirement (§15).
2. `verifyControlPlaneCertificate`'s CommonName check does not distinguish
   between Control Plane instances, only "a Control Plane" from "an Agent."
3. AgentOps authorization is currently equivalent to authentication —
   no separate, explicit, revocable per-relationship record exists.
4. User→Control-Plane authentication/authorization layer is entirely
   absent (expected/documented as future work, not a regression).

This entire section, and the gaps above, were produced by a dedicated
Step-1 security inspection performed earlier in this project (no code was
changed during that inspection) and are reproduced here for continuity —
see `docs/architecture/security.md` for the target architecture that
inspection was measured against.

---

## 8. Current Database

PostgreSQL, with the TimescaleDB extension (installed by migration `0001`).
Accessed exclusively via `pgxpool` through `internal/storage/*`
repositories — no ORM. Migrations are sequential and forward-only
(no down migrations exist or are planned, per the project's own
forward-compatibility discipline).

**Migrations present**: `0001_extensions.sql` through
`0009_notifications.sql`. This handoff only describes their purpose in
outline, per the "do not propose new tables" instruction:

| Migration | Contents |
|---|---|
| 0001 | TimescaleDB extension |
| 0002 | `nodes`, `metric_samples` (core inventory/metrics) |
| 0003 | Adds an environment tag to `nodes` |
| 0004 | Fleet: `enrollment_tokens`, `node_credentials`, `node_denylist`, `ingested_envelopes`, `inventory_snapshots` (see below) |
| 0005 | `events` |
| 0006 | `alert_rules`, `alert_states`, `alert_history` |
| 0007 | `incidents`, `incident_members` |
| 0008 | `slos` |
| 0009 | `notification_channels`, `notification_deliveries` |

**Fleet-related tables (migration 0004), current purpose**:

- **`enrollment_tokens`** — one row per bounded credential an operator
  hands to provisioning. `token_hash` (primary key, SHA-256 of the
  plaintext — plaintext itself is never stored), `label`, `environment`,
  `allowed_cidr`, `max_uses`/`uses_remaining`, `expires_at`,
  `allow_reenroll`, `revoked_at`.
- **`node_credentials`** — one row per certificate the Control Plane has
  ever issued (a durable history, not overwritten on renewal).
  `fingerprint` (primary key, the certificate's serial, hex-encoded),
  `node_id`, `issued_at`, `expires_at`, `enrolled_via` (the token hash that
  authorised issuance, null for a renewal), `revoked_at`, `revoked_reason`.
  Drives the re-enrollment-refusal check (an already-certified node id
  cannot be re-enrolled without an explicit `allow_reenroll` grant).
- **`node_denylist`** — `node_id` (primary key), `reason`, `created_at`.
  Immediate ejection independent of certificate expiry.
- **`ingested_envelopes`** — idempotency/dedup record for at-least-once
  telemetry delivery. `envelope_id` (primary key), `node_id`, `kind`,
  `received_at`. A plain table (not a hypertable, deliberately — see the
  migration's own comment), pruned periodically by the Control Plane.
- **`inventory_snapshots`** — latest-only inventory per (node, subject),
  replaced in place on arrival, never appended to; not a time series.

**Other relevant tables**: `nodes` (0002) is the core node-registry table
every other table's `node_id` conceptually refers to (not all are declared
as formal foreign keys — not verified either way as part of this project).

No table in the current schema has any concept of "which Control Plane
instance" issued or owns a row — every deployment's database is implicitly
scoped to that one Control Plane, because today there is exactly one CA and
one Fleet pipeline per database. This is a structural fact relevant to any
future multi-Control-Plane design (§15–17), not a defect to fix in this
document.

---

## 9. Current Deployment

**`docker-compose.yml`** (repository root) — **untouched throughout this
entire project**, per explicit instruction on every task that touched
deployment. Development-only: provisions a single `postgres`
(`timescale/timescaledb:2.17.2-pg17`) service, bound to `127.0.0.1:5432`,
named volume `postgres-data`. The Control Plane and UI are expected to run
on the host directly (`make run` / `make server` / `make ui`) during
development, not in containers — collectors need the real `/proc`, the real
Docker socket, the real systemd, which a containerized Control Plane would
not see correctly.

**`docker-compose.prod.yml`** (repository root, new this project) — the
production stack. Three services:

- **`atlas-fleet-volume-init`** — a one-shot `alpine:3.21` container,
  `command: ["chown", "10001:10001", "/fleet-data"]`, `network_mode: none`,
  `restart: "no"`. Exists because a freshly created Docker named volume is
  owned `root:root 0755` by default, and `atlas-server` runs as uid `10001`
  with `cap_drop: ALL` — verified directly (see §19) that without this,
  `atlas-server` cannot write its own Fleet CA into a fresh volume at all.
  Also verified that baking ownership into the image via an empty
  pre-owned directory does **not** work — Docker's volume-seed-from-image
  only copies file *contents*, not an empty directory's own ownership.
  `atlas-server` depends on this completing successfully
  (`condition: service_completed_successfully`) before it starts.
- **`atlas-server`** — builds from the existing, unmodified root
  `Dockerfile`. `image: ${ATLAS_SERVER_IMAGE:-atlas-server:local}` (no
  registry established in this repository — see below). Not published to
  the host (`expose: 8080` only — `atlas-ui`'s nginx reaches it over the
  internal Compose network by service name). No Fleet/libp2p port is
  published or even internally exposed — verified from
  `internal/app/fleet.go`'s own code/comments that when
  `ATLAS_FLEET_LIBP2P_RELAY_ADDR` is set, the Control Plane dials **out**
  to the Relay; it needs no inbound-reachable port for that path, matching
  the real working Mac→Relay→Agent setup, which also never forwarded a
  port. `read_only: true`, `tmpfs: [/tmp]`, `cap_drop: [ALL]`,
  `security_opt: [no-new-privileges:true]`. No Docker-level healthcheck —
  the runtime image is `scratch` (no shell, no curl); health is via
  `/healthz`/`/readyz`, checked externally. Fleet's CA/identity state is
  persisted to the **`atlas-fleet-data`** named volume, mounted at
  `${ATLAS_FLEET_DATA_DIR:-/var/lib/atlas/fleet}` — the mount path is
  driven by the same variable Atlas itself reads, so they cannot drift
  apart.
- **`atlas-ui`** — builds from the new `web/Dockerfile` (multi-stage:
  `node:22-alpine` build stage running `npm ci && npm run build`, runtime
  stage `nginx:1.27-alpine` serving the static bundle and reverse-proxying
  `/api/`, `/healthz`, `/readyz` to `atlas-server:8080` via
  `web/nginx.conf`). This is the **only** published port in the stack
  (`${ATLAS_UI_PORT:-80}:80`). `read_only: true` with `tmpfs` mounts for
  nginx's own writable paths, `cap_drop: [ALL]` plus a minimal
  `cap_add: [CHOWN, SETUID, SETGID]` — verified directly that nginx's own
  startup (chowning its tmpfs cache dirs) fails without exactly these three
  even running as root, because `cap_drop: ALL` removes `CAP_CHOWN` too.
  Has a working `wget`-based healthcheck (this image, unlike
  `atlas-server`'s, has a shell).

PostgreSQL is **not** part of `docker-compose.prod.yml` — deliberately.
`docs/operations/deployment.md`'s own examples all point Atlas at an
already-existing `postgres.internal`, never at a Postgres the same
deployment provisions; this was treated as the documented intent (external/
managed database), stated explicitly in this project's own report rather
than assumed silently. *(Note: an earlier iteration of this Compose file
did include an optional `with-db` Postgres profile for local production
testing; it was subsequently removed at explicit request so external
PostgreSQL is the only supported configuration — the profile no longer
exists in the current file.)*

**`web/nginx.conf`** — SPA fallback (`try_files $uri /index.html` — needed
because the frontend uses `react-router`'s `BrowserRouter`), reverse-proxies
`/api/`, `/healthz`, `/readyz`. WebSocket upgrade handled correctly via an
nginx `map` block that only sets `Connection: upgrade` on genuine upgrade
requests (verified this matters — a blanket `Connection: upgrade` on every
proxied request is incorrect and was fixed during testing). Uses lazy DNS
resolution (`resolver 127.0.0.11` + a `set` variable in `proxy_pass`)
rather than a bare hostname in `proxy_pass` — verified directly that
without this, nginx refuses to start at all if `atlas-server` isn't
DNS-resolvable at the exact moment nginx's config loads, which would
crash-loop the whole UI on ordinary restart-order timing.

**`.env.prod.example`** (repository root, new, committed) — documents every
`ATLAS_*` variable this deployment needs: image tags (optional, default to
local builds), `ATLAS_UI_PORT`, external database connection
(`ATLAS_DATABASE_HOST=postgres.internal` placeholder,
`ATLAS_DATABASE_SSL_MODE=verify-full`), and Fleet/libp2p — **currently
documented as enabled** (`ATLAS_FLEET_ENABLED=true`,
`ATLAS_FLEET_LIBP2P_ENABLED=true`, a placeholder
`ATLAS_FLEET_LIBP2P_RELAY_ADDR`) because this deployment is intended to be
the Fleet control plane. The real `.env.prod` file is gitignored and does
not exist in the repository (nor was one committed at any point — verified
via `git status`/`git log`).

**Secrets**: the database password is handled via Docker Compose's native
`secrets:` mechanism (not a plain environment variable) — a local file
`./secrets/atlas_db_password.txt`, gitignored (`/secrets/` added to
`.gitignore`), mounted at `/run/secrets/atlas_db_password`, referenced via
`ATLAS_DATABASE_PASSWORD_FILE` — matching `docs/operations/deployment.md`'s
own documented pattern for production secrets exactly. No secrets directory
or file exists in the repository itself.

**External PostgreSQL**: as above — this deployment connects out to a
database it does not provision. No specifics about the actual production
database host are recorded anywhere in this repository (correctly — that
belongs in the operator's real, gitignored `.env.prod`).

**External Relay**: as above — this deployment dials out to a Relay it does
not provision or manage; the Relay's own deployment is entirely outside
this repository (§5).

**Ports**: only `atlas-ui`'s `${ATLAS_UI_PORT:-80}` is published to the
host. Everything else (`atlas-server:8080`) is reachable only inside the
Compose-internal `atlas-internal` bridge network.

**Networks**: one — `atlas-internal` (bridge driver), shared by all three
services.

**Volumes**: one — `atlas-fleet-data` (named, persistent).

**Caddy/Cloudflare status**: **not implemented, not present anywhere in
this repository.** Cloudflare Tunnel is mentioned exactly once, in
`docs/adr/0012-connect-by-identity.md`, as an alternative that was
*considered and not chosen* for Agent↔Control-Plane connectivity (libp2p+
Relay was chosen instead) — this is a design-discussion reference, not an
implemented or planned component. No Caddy configuration exists anywhere.
`docs/operations/deployment.md`'s own reverse-proxy example uses nginx.

**What is implemented vs. not, summary**:

| Component | Status |
|---|---|
| `docker-compose.prod.yml` | Implemented, tested (§19) |
| `web/Dockerfile` + `nginx.conf` | Implemented, tested (§19) |
| `atlas-fleet-data` volume + init container | Implemented, tested (§19) |
| `.env.prod.example` | Implemented |
| Real `.env.prod` / real secrets | **Not present** — operator-provided, out of scope for this repo |
| CI/CD image publishing | **Not implemented** — no registry established (see §16) |
| Reverse proxy / TLS termination in front of `atlas-ui` | **Not implemented** — documented as required by `deployment.md` (auth proxy mandatory pre-auth-ship) but not built as part of this project |
| Relay deployment automation | **Not implemented, not in scope** — Relay is deployed and managed entirely outside this repository |
| Agent deployment automation | **Not implemented, not in scope** — same |

---

## 10. Current Production Architecture

```
Internet
  |
  v
[ NOT IMPLEMENTED: Cloudflare / Caddy / any TLS-terminating,
  authenticating reverse proxy — docs/operations/deployment.md states
  this is mandatory pre-authentication, but nothing in this repository
  builds one yet. ]
  |
  v
Atlas UI  (nginx, container, the only published port: ATLAS_UI_PORT)
  |
  |  reverse-proxies /api, /healthz, /readyz
  v
Atlas Server  (container, internal-network-only, port 8080)
  |
  |  ATLAS_DATABASE_* (verify-full TLS)
  v
External PostgreSQL + TimescaleDB  (NOT provisioned by this repo —
                                     an operator-managed instance)
```

And, separately, the Fleet/Agent path (does not go through `atlas-ui` at
all):

```
Atlas Server  (Fleet pipeline: HTTPS mTLS on 8443 [not published],
               libp2p on 4102 [not published])
  |
  |  dials OUT (no inbound port required — see §9)
  v
Atlas Relay  (external to this repo/Compose stack; systemd, separate host;
              pure transport, no auth — see §5)
  |
  |  Agent also dials OUT to the same Relay
  v
Atlas Agent  (external to this repo/Compose stack; systemd, separate host)
```

Both diagrams reflect only what actually exists/is wired today — no
component is shown that isn't real.

---

## 11. Local Development Architecture

Unrelated to `docker-compose.prod.yml` and not modified by any of this
project's deployment work.

```
docker compose up -d          (docker-compose.yml — Postgres only,
                                127.0.0.1:5432, named volume postgres-data)
        +
make server / make run         (Atlas Server runs directly on the host,
                                NOT in a container — needs the real
                                /proc, Docker socket, systemd)
        +
make ui / make web-dev         (Vite dev server, port 5173, proxies
                                /api, /healthz, /readyz to
                                127.0.0.1:8080 by default, overridable
                                via ATLAS_API)
```

`make dev` (new target, this project) runs the Atlas Server and the
frontend dev server together in one terminal (backgrounds the server,
traps `EXIT` to kill both). `make env-check` (new) validates that the
required `ATLAS_*` variables are present in the loaded `.env`, without
printing secret values. `.env` (gitignored, real values) and `.env.example`
(committed, template) both exist; the Makefile auto-loads `.env` via a
GNU Make `include`/`export` block if present.

The Agent and Relay are **not** part of local development's Docker Compose
in this repository — per this project's architecture, a real Agent runs on
a separate Linux server and a real Relay runs on a separate publicly
reachable host; local development connects to those same real, external
processes when Fleet/libp2p testing is needed (verified working in this
project prior to the current work, per project history: Mac Atlas Server →
Relay → Linux Agent).

---

## 12. Important Decisions Already Made

All of the following were explicitly stated by the project owner across
this project's history and are treated as settled constraints, not open
questions:

- The Relay remains a pure transport component — it must never become an
  authorization authority. (Currently true — verified in §5.)
- Production Atlas and Development Atlas must be able to coexist.
- The same physical Agent must be able to connect to both Production and
  Development Atlas simultaneously.
- There is **no** separate testing Agent — one Agent identity, used
  everywhere.
- Agents must not need to be reconfigured or reinstalled every time Atlas
  (the Control Plane) is redeployed.
- The Relay should not require redeployment for a normal Atlas Control
  Plane deployment.
- Agent identity must remain stable (already true today — §4).
- Production and Development trust relationships must be independently
  revocable.
- Authentication and authorization must be treated as separate concerns
  ("who are you" vs. "are you allowed to do this").
- Future User/RBAC must be added at the Control Plane layer, never inside
  the Agent — the Agent should only ever need to know "this request came
  from a Control Plane identity I trust."
- The existing PKI (`internal/platform/pki`) should be reused, not
  replaced, for any future work.
- Do not blindly introduce `control_planes` / `agents` /
  `control_plane_agents` tables without architectural justification — the
  Step-1 inspection (§7) already identified that a naive reading of that
  suggestion doesn't map cleanly onto where the Agent's trust state
  actually needs to live (see §7's closing paragraph on this).
- The Agent must be able to cryptographically distinguish which Control
  Plane it is talking to — not currently true (§7, gap #2).
- AgentOps must require explicit authorization, not merely successful
  authentication — not currently true (§7, gap #3).

---

## 13. Security Requirements

Numbered, as established across this project (mirrors
`docs/architecture/security.md` §1–2 plus the additional constraints given
directly):

1. An Agent must only accept control operations from a Control Plane that
   is both cryptographically authenticated **and** explicitly authorized to
   control that Agent.
2. Authentication alone is never sufficient authorization.
3. The same physical Agent must be connectable to multiple independent
   Control Planes (at minimum: Production and Development) without a
   separate testing Agent.
4. An Agent must never blindly trust any Control Plane merely because a
   connection is authenticated.
5. A Control Plane must be explicitly authorized to control a particular
   Agent — this must not follow automatically from successful
   authentication to *some* Agent.
6. Agent identity must be cryptographically strong and persistent, and must
   survive Agent/container restart and redeployment.
7. Control Plane identity must be cryptographically verifiable, and must be
   distinguishable from any other Control Plane's identity.
8. Revoking one Control-Plane↔Agent trust relationship must not affect any
   other such relationship for the same Agent.
9. IP address, hostname, container name, Docker network membership, HTTP
   headers, static shared secrets, source IP, and "a TLS/libp2p connection
   succeeded" must never, individually or together, be treated as
   sufficient authorization on their own.
10. Every privileged Agent operation must be attributable to: Control
    Plane identity + Agent identity + an explicit authorized relationship +
    the specific requested operation.
11. No privileged operation may bypass this model through an alternate
    transport/API path (verified in §7 that none currently exists for the
    one privileged operation AgentOps implements).
12. Private keys must never be exposed through normal APIs or logs.
13. Future User authentication/RBAC must be insertable above the Control
    Plane layer without redesigning Agent↔Control-Plane trust.
14. Existing production deployment architecture (§9–10) must remain
    compatible with whatever multi-Control-Plane design is eventually
    built.

---

## 14. Known Security Gaps

Each entry: current behavior → why it's a problem → whether it blocks the
multi-Control-Plane architecture. All four were identified during the
Step-1 security inspection performed earlier in this project (inspection
only — no code was changed).

**Gap 1 — Agent has no multi-Control-Plane capability.**
*Current behavior*: `internal/agent`'s `Agent` struct holds exactly one CA
certificate, one credential holder, one `ControlPlaneURL`. `bootstrap()`
treats the mere existence of *any* certificate in `DataDir` as "already
enrolled," regardless of which Control Plane originally issued it.
*Why it's a problem*: pointing the same Agent process/`DataDir` at a second
Control Plane does not add a second trust relationship — it reuses (and
mismatches against) the first one's CA.
*Blocking*: **Yes — this is the central blocker** for requirements #3
and the whole Layer 2 model in `docs/architecture/security.md`.

**Gap 2 — `verifyControlPlaneCertificate`'s identity check does not
distinguish Control Planes.**
*Current behavior*: checks chain-to-CA plus a fixed, shared
`CommonName == "atlas-control-plane"` string, identical across every
possible Control Plane deployment.
*Why it's a problem*: safe only because exactly one CA is ever pinned
today; the check itself provides no "which specific Control Plane" signal.
*Blocking*: **Yes** — a multi-CA-trusting Agent needs a real per-relationship
identity check, not this.

**Gap 3 — AgentOps authorization equals authentication.**
*Current behavior*: `fleetPipeline.ContainerLogs` authorizes based solely on
"this node has a live, non-denylisted certificate and I have its current
Peer ID" — no separate, explicit, revocable "authorized for privileged
operations" record exists.
*Why it's a problem*: violates security requirement #2 directly.
*Blocking*: **Partially** — orthogonal to the multi-Control-Plane identity
problem (gaps 1–2), but must be solved before AgentOps can be considered
secure in a multi-tenant (multi-CP) world, since "which CP is even allowed
to ask" becomes a real question once more than one CP exists.

**Gap 4 — User→Control-Plane layer entirely absent.**
*Current behavior*: no authentication, no session, no RBAC anywhere in
`internal/api`.
*Why it's a problem*: none yet — this is documented, expected future work,
not a regression.
*Blocking*: **No** — explicitly out of scope for the immediate
multi-Control-Plane work; requirement is only that the eventual design not
preclude adding this later (§12, §13 #13).

---

## 15. Multi-Control-Plane Requirement

**Not implemented. Documented here as a requirement only — no design
decision has been made yet.**

```
Production Atlas
       \
        \
         Agent-001
        /
       /
Development Atlas
```

The same Agent-001 process/identity must maintain **two independent,
simultaneous, independently revocable** trust relationships — one with
Production Atlas, one with Development Atlas. Precisely:

- Production Atlas being authorized to control Agent-001 must not imply
  Development Atlas is also authorized (and vice versa).
- Revoking the Development relationship must leave the Production
  relationship fully intact, and vice versa.
- This must be accomplished with **one** Agent identity (one node ID, one
  physical/logical Agent process) — not two separately-provisioned Agents,
  and not two separate binaries.
- Both relationships must be able to be active at the same time (Agent-001
  simultaneously reachable by and reporting to both Control Planes), not
  merely "switchable."

This directly requires solving Gap 1 and Gap 2 above (§14) — the Agent's
current single-CA, single-credential, single-`ControlPlaneURL` model has no
place to hold a second, independent relationship.

---

## 16. Pending Architectural Work

**Primary pending item: the Multi-Control-Plane trust architecture** (§15),
covering:

- How the Agent stores and manages more than one (CA, credential,
  Control-Plane-target) tuple at once, keyed by something that identifies
  each relationship independently.
- How each relationship is independently created, authorized, verified,
  and revoked, without affecting any other relationship for the same
  Agent.
- Whether/how the Control Plane's own database schema needs to change to
  support this — and specifically, per the decision in §12, *not* to
  assume the `control_planes`/`agents`/`control_plane_agents` shape is
  correct without re-deriving where each piece of state actually needs to
  live (some of it may belong on the Agent's side, in its `DataDir`, not
  in either Control Plane's Postgres — see §7's closing paragraph and §8's
  closing paragraph).
- How `verifyControlPlaneCertificate` (or its replacement) distinguishes
  specific Control Plane identities once more than one CA can be trusted.
- How AgentOps authorization becomes a genuine, separate, revocable check
  rather than a byproduct of authentication (Gap 3).

Secondary, lower-priority pending items noted elsewhere in this document:
no registry established for CI/CD image publishing (§9); no reverse
proxy/TLS-termination component built yet in front of `atlas-ui` (§9, §10).

---

## 17. Pending Step 2 Requirements

The next Claude session, when it begins Step 2, must:

- Design the multi-Control-Plane trust architecture (§15–16).
- Distinguish Production vs. Development Control Planes cryptographically
  (fixing Gap 2 — §14).
- Redesign the Agent's trust state to hold multiple independent
  relationships (fixing Gap 1 — §14).
- Redesign Control-Plane-side authorization for privileged operations so
  it is explicit and separate from authentication (fixing Gap 3 — §14).
- Preserve the Agent's one stable identity (§4's `hostid` mechanism —
  already correct, must not be replaced or duplicated).
- Support independent revocation of each Control-Plane↔Agent relationship.
- Support Production and Development being simultaneously active against
  the same Agent (§15).
- Preserve local development as it currently works (§11) — not to be
  redesigned as a side effect.
- Preserve the Relay as a transport-only component (§5, §12) — no
  authorization logic added there.
- Preserve the future-User/RBAC seam (§12, §13 #13) — the design must not
  make adding that later require redesigning Agent↔Control-Plane trust.

---

## 18. Files Changed During This Project

Based directly on `git diff --stat 5bebe90 HEAD` (the commit immediately
before this project's work began, through the current, merged, clean
working tree at `0f1bf9a`) — 31 files, 3166 insertions, 116 deletions.

**Created (new files):**
```
.env.example
.env.prod.example
docker-compose.prod.yml
docs/architecture/security.md
internal/agent/agentops.go
internal/agent/agentops_test.go
internal/app/fleet_test.go
internal/core/transport/libp2ptransport/agentops.go
internal/core/transport/libp2ptransport/agentops_test.go
web/.dockerignore
web/Dockerfile
web/nginx.conf
web/src/pages/ContainerLogsPage.tsx
web/src/pages/containers/logViewerModel.test.ts
web/src/pages/containers/logViewerModel.ts
```

**Modified (existing files):**
```
.gitignore
Makefile
internal/agent/agent.go
internal/api/router.go
internal/api/v1/containers.go
internal/api/v1/containers_test.go
internal/api/v1/system.go
internal/app/app.go
internal/app/fleet.go
internal/app/libp2p_transport_integration_test.go
internal/platform/pki/tls.go
web/src/App.tsx
web/src/api/logFollow.ts
web/src/api/queries.ts
web/src/pages/ContainersPage.tsx
web/src/pages/containers/LogViewer.tsx
```

**Intentionally untouched** (explicitly required by every task in this
project that could have touched them):
```
docker-compose.yml           (development Postgres — untouched throughout)
Dockerfile                    (production atlas-server image — untouched;
                              one exploratory edit was made and tested
                              during the atlas-fleet-data volume work,
                              found not to solve the problem, and reverted
                              before commit — see §19)
migrations/*.sql              (no schema change made)
internal/relay/*               (no Relay code touched)
internal/agent/config.go       (Agent's own config surface untouched —
                              only agent.go [+18 lines, AgentOps handler
                              registration] and the new agentops.go were
                              touched, both from the earlier AgentOps
                              milestone, not the deployment work)
```

Current `git status`: clean (`nothing to commit, working tree clean`) as
of this document's creation, aside from `CURRENT_STATE.md` itself being
new/untracked. The repository is on `main`, 2 commits ahead of
`origin/main` at the time of writing (a `pull`/merge of a second, parallel
line of the same work happened during this project — reconciled into a
single clean history at commit `0f1bf9a`; no unresolved conflicts, no
data loss observed — verified by reading the full accumulated diff).

---

## 19. Deployment Validation Already Performed

Only tests actually run and observed are listed — nothing here is
projected or assumed.

- **Docker Compose validation**: `docker compose -f docker-compose.prod.yml
  config` — passed. Later re-validated with `ATLAS_ENV_FILE=.env.prod.example
  docker compose --env-file .env.prod.example -f docker-compose.prod.yml
  config` specifically so a real `.env.prod` never had to be created for
  validation — passed, confirmed no `.env.prod` file existed afterward.
- **Image builds**: `docker compose -f docker-compose.prod.yml build` —
  both `atlas-server` and `atlas-ui` images built successfully. The
  `web/Dockerfile` multi-stage build (Node build stage → nginx runtime
  stage) was separately built and smoke-tested standalone (SPA root,
  SPA client-route fallback, and a static asset all returned 200; an
  `/api/*` request correctly returned 502 with no backend present, rather
  than crashing — this is what surfaced the nginx lazy-DNS-resolution fix
  described in §9).
- **Fleet startup**: real `atlas-server` container, real (throwaway)
  Postgres, `ATLAS_FLEET_ENABLED=true` — Fleet's HTTPS listener (8443) and,
  separately, its libp2p listener (4102) both came up and logged ready.
- **Persistent Fleet volume**: verified the exact failure mode first
  (a fresh named volume is `root:root 0755`; `atlas-server` as uid 10001
  with all capabilities dropped cannot write into it — reproduced directly
  with a raw `docker run` probe). Verified the fix (the
  `atlas-fleet-volume-init` one-shot container) actually changes ownership
  and that `atlas-server` can then write into the volume under the exact
  same hardened flags. Also verified, and then reverted, an alternative
  fix (pre-owning an empty directory baked into the image) — confirmed by
  direct test that Docker's volume-seed-from-image does not preserve an
  empty directory's ownership, only file contents.
- **Peer ID persistence**: brought the real Compose stack up with Fleet +
  libp2p enabled (no relay address configured — this test never contacted
  the real, external Relay), recorded the generated Peer ID, ran
  `docker compose down` (no `-v`) then `up -d` again, and confirmed the
  **same** Peer ID was reported both times.
- **UI → nginx → Atlas Server**: with a real `atlas-server` and throwaway
  Postgres both running, `curl` through `atlas-ui`'s published port to
  `/readyz` returned `{"status":"healthy",...}` and to
  `/api/v1/system/info` returned real Atlas Server JSON — both proxied
  correctly.
- **WebSocket upgrade**: a raw WebSocket-upgrade `curl` request through the
  same proxy path reached the real Go handler (confirmed via
  `atlas-server`'s own request logs) and received the correct, expected
  `501` (no Docker socket in that test container — an application-level
  response, not a proxy failure).
- **Fleet/libp2p startup under full hardening**: after an initial
  non-reproducible crash (`ThreadContextFcntl.cpp` — identified as a
  one-off local Docker Desktop VM artifact, not a real bug, after three
  consecutive clean runs with identical settings and isolated capability
  bisection testing that ruled out `cap_drop: ALL` as the cause), Fleet +
  libp2p was confirmed starting reliably under the exact production
  hardening (`read_only: true`, `cap_drop: ALL`, no `cap_add` on
  `atlas-server`).
- **External PostgreSQL configuration**: validated that `atlas-server`
  correctly refuses to start with `ATLAS_DATABASE_SSL_MODE=disable` under
  `ATLAS_ENVIRONMENT=production` (the app's own built-in production safety
  check, not a Compose-level check) — confirmed working as intended rather
  than worked around.
- **Cleanup after tests**: every test container, throwaway image, test
  volume, test network, and throwaway `.env.prod`/`secrets/` file created
  during validation was explicitly removed afterward — verified via
  `docker ps -a`, `docker volume ls`, `docker network ls`, and `ls` showing
  only the user's own pre-existing, unrelated dev containers remaining.

---

## 20. Important Operational Notes

- **`atlas-fleet-data` (Docker named volume)**: contains the production
  Control Plane's CA private key and certificate. **Must never be deleted
  casually** — doing so orphans every currently-enrolled Agent's trust
  (they would all need to re-enroll against a brand-new CA). Not created
  by `docker compose down` alone; only `down -v` removes it.
- **`secrets/atlas_db_password.txt`**: the production database password,
  gitignored, must be created by the operator, never committed. Referenced
  by `docker-compose.prod.yml`'s `secrets:` block.
- **`.env.prod`**: gitignored, must be created by the operator from
  `.env.prod.example`. Does not exist anywhere in this repository or its
  git history.
- **External PostgreSQL**: not managed by this repository at all — its
  own backup/retention/access policy is entirely the operator's
  responsibility, outside this project's scope.
- **External Relay**: not managed by this repository. Its own systemd
  deployment, host, and configuration are unknown to this repository by
  design (§5) — nothing here should ever assume specifics about it beyond
  its public multiaddr, which itself is operator-supplied via
  `ATLAS_FLEET_LIBP2P_RELAY_ADDR` / `ATLAS_AGENT_LIBP2P_RELAY_ADDR`.
- **Agent identity** (on whatever Linux host actually runs it): its
  `DataDir` (`node-id`, `agent-cert.pem`, `agent-key.pem`, `ca-cert.pem`)
  must be on persistent storage and must never be deleted casually —
  doing so forces re-enrollment and, if `ATLAS_AGENT_TOKEN` isn't newly
  provisioned, may require operator intervention to regain connectivity.
  This project did not verify the actual Agent host's storage
  configuration — flagged, not assumed.
- **CA identity** (Control Plane side): see `atlas-fleet-data` above — the
  same warning applies.
- **Nothing in this project's deployment work touched, restarted, or
  reconfigured the real, running Relay or the real, running Agent** — every
  test in §19 that involved Fleet/libp2p explicitly avoided setting a real
  relay address, so no test traffic ever reached the production Relay or
  Agent.

---

## 21. Handoff Instructions

**STOP HERE. The next Claude session must read this document before making
any changes.**

Next task: **Step 2 — Design the Multi-Control-Plane Security
Architecture.**

Per this project's own established process (§16–17): design only, produce
a written, internally consistent proposal (files to modify/add, database
changes if any, API changes, protocol changes, migration strategy,
backward-compatibility considerations, deployment implications) and stop
for explicit approval before writing any code.

**Do NOT implement Step 2 in this session.**
