# Atlas Human-User Authentication & Authorization — Investigation Report

**Date:** 2026-08-25
**Scope:** Investigation only. No application code was modified, no files were created besides this report, nothing was installed, no migrations were run.

## PHASE 1 — Repository Structure

- **Backend/API server:** `cmd/atlas-server` → `internal/app` (composition root) → `internal/api` (HTTP surface: `router.go` + `internal/api/v1`, `internal/api/agent`).
- **Frontend:** `web/` — Vite + React 19 + TypeScript SPA, routes in `web/src/App.tsx`, API client in `web/src/api/client.ts`.
- **Database layer:** `internal/storage/*` (pgx, no ORM), embedded migrations in `migrations/`.
- **Migrations:** `migrations/0001`–`0011`, forward-only.
- **Authentication-related packages found:** none named `auth`, `session`, or `user`. The closest are `internal/platform/pki` (X.509/mTLS, machine certs) and `internal/api/agent/peerauth.go` (libp2p Peer ID authorization) — both machine-identity, not human.
- **Authorization-related packages found:** `internal/core/fleet` (`grants.go`, `peer.go`) — machine/agent-scoped only.
- **Middleware:** `internal/platform/httpx/server.go` (`BaseMiddleware`, `AgentMiddleware`), `middleware.go` (`Chain`, `StreamMiddleware`, individual middlewares).
- **API handlers:** `internal/api/v1/*.go` (operator-facing), `internal/api/agent/handler.go` (agent-facing).
- **Models/entities:** Go structs in `internal/core/fleet` (`PeerIdentity`, `PeerSpec`, `Credential`), no `User`/`Role`/`Permission` struct anywhere in the repo.
- **Configuration:** `internal/platform/config` — no auth-provider config (no OIDC issuer URL, no session secret, no JWT signing key settings exist).
- **Deployment config:** `docker-compose.yml`/`docker-compose.prod.yml` — no identity-provider service, no auth proxy container defined (one is *documented as required* in `docs/operations/deployment.md` but not present as infrastructure).

## PHASE 2 — Codebase-Wide Search

Full-repo grep for every term list (Go backend + frontend + configs), verified individually, not by filename alone:

- `user`/`users`/`login`/`logout`/`signin`/`register` — **zero implementation hits.** Only doc/comment mentions (ADR-0011, security docs) discussing the *absence* of these.
- `session`/`cookie` — **zero real hits.** Matches were: libp2p "AgentOps session" (a log-streaming session, not a user session), Postgres "session-level advisory lock" (migration locking mechanism), and — most directly — `internal/platform/httpx/middleware.go:251`, the `CORS` middleware's own doc comment: *"Credentials are not enabled. Atlas has no session cookie yet; the header will be added alongside authentication..."* This is code-level, not doc-level, confirmation of absence.
- `jwt`/`oauth`/`oauth2`/`oidc` — **zero hits anywhere in the repo**, Go or TypeScript, including `go.mod` and `web/package.json`.
- `bearer`/`claims`/`access_token`/`refresh_token` — only in `internal/platform/redact/redact.go` (lines 38, 58-66): patterns used to **redact** `Authorization: Bearer ...`-shaped strings that might appear in *collected* application logs/process data (a defensive scrubber for what Atlas *observes*, unrelated to authenticating Atlas's own API).
- `role`/`roles`/`rbac`/`permission`/`permissions`/`policy`/`acl`/`principal` — hits only in: (a) ADR-0011 and security docs discussing future RBAC, (b) `fleet.PeerRoleAgent`/`agent_peers.role` (machine-peer role, constrained to `'agent'` only — not a human role concept), (c) `agent_operation_grants`/`GrantStore` (machine operation grants, see Phase 3).
- `tenant`/`organization`/`org_id`/`tenant_id` — **zero hits anywhere**, code or schema.
- `admin`/`superadmin` — **zero hits in code.** Docs mention "admin" only as a future role name in `security.md`'s proposed diagram.
- `middleware` — real hits are `BaseMiddleware`/`AgentMiddleware`/`StreamMiddleware`/`PeerAuthMiddleware`, all covered in Phase 4.
- `permission_denied`/`unauthenticated` — these **error codes exist** (`internal/platform/errs`) and are actively used, but only on the machine-identity paths (peer authorization, denylist). Confirmed no user-facing usage.

Dependency manifests: `go.mod` direct deps are `coder/websocket`, `docker/docker`, `pgx/v5`, `libp2p` stack, `gopsutil`, `x/time`, `yaml.v3` — no auth/identity library. `web/package.json` — no auth library (no `next-auth`, `oidc-client`, `auth0`, `firebase`, `passport`, etc.).

## PHASE 3 — Database

Every migration `0001`–`0011` read in full (not filename-inferred). **Definitive table list:**

| Table | Migration | Purpose | Human or Machine |
|---|---|---|---|
| `nodes` | 0002, altered 0003/0004 | Machine inventory record | **Machine** |
| `enrollment_tokens` | 0004 | Bounded HTTPS bootstrap credential for an **agent host** | **Machine** |
| `node_credentials` | 0004 | X.509 certs issued to **agent hosts** | **Machine** |
| `node_denylist` | 0004 | Out-of-band ejection of a **node** | **Machine** |
| `ingested_envelopes` | 0004 | Telemetry idempotency dedup | **Machine** (infra bookkeeping) |
| `inventory_snapshots` | 0004 | Latest inventory per node/subject | **Machine** |
| `agent_operation_grants` | 0010 | Explicit authorization for a **node** to have a privileged AgentOps operation performed against it | **Machine** |
| `agent_peers` | 0011 | Authorization of a libp2p **Peer ID** to act as a node/environment | **Machine** |

**No table exists, anywhere, for:** `users`, `sessions`, `roles`, `permissions`, `user_roles`, `role_permissions`, `user_permissions`, `organizations`, `tenants`, `memberships`, `invitations`, API tokens (human-scoped), refresh tokens, or audit logs. This is not an inference from missing filenames — every `CREATE TABLE` statement in every migration file was read directly.

**Specific determination on the five flagged tables — all five are MACHINE/AGENT mechanisms, none are human-user mechanisms:**

| Table | Foreign keys / linkage | Used by application code |
|---|---|---|
| `agent_operation_grants` | `node_id` (no FK constraint — node row not guaranteed to pre-exist), `(node_id, operation)` PK | **Yes** — `internal/storage/fleet/repository.go` (`Grant`/`RevokeGrant`/`IsGranted`), called from `internal/core/fleet/enroll.go:166` and `internal/app/fleet.go:262` |
| `agent_peers` | `peer_id` PK, `node_id` (no FK), `idx_agent_peers_node_id` | **Yes** — `repository.go` (`RegisterPeer`/`RevokePeer`/`AuthorizedPeer`), called from `cmd/atlas-server/peer.go` and `internal/api/agent/peerauth.go:65` |
| `node_credentials` | `fingerprint` PK, `enrolled_via` FK → `enrollment_tokens` | **Yes** — `CredentialStore` impl, used by `Enroller.Enroll`/`Renew` |
| `enrollment_tokens` | `token_hash` PK | **Yes** — `TokenStore` impl, used by `Enroller.Enroll` |
| `node_denylist` | `node_id` PK | **Yes** — `DenylistStore` impl, checked in `Enroller.Enroll`/`Renew` and `agent.Handler.checkDenylist` |

None of the five carries a column, comment, or FK referencing anything resembling a human account. `node_id` throughout means machine identity (see Phase 7).

## PHASE 4 — Backend Authentication

| Capability | Status |
|---|---|
| Login endpoint | Does not exist. |
| Logout endpoint | Does not exist. |
| User registration | Does not exist. |
| OIDC callback | Does not exist. |
| OAuth callback | Does not exist. |
| JWT validation | Does not exist. |
| API token validation (human-scoped) | Does not exist. |
| Session creation/lookup | Does not exist. Confirmed by `middleware.go:251`'s own comment. |
| Current-user / user-profile endpoint | Does not exist. |

What *does* exist, with full detail:

**FILE:** `internal/api/router.go`
**FUNCTION:** `New(deps Deps)`, line 58
**ROUTE:** every `/api/v1/*` route
**PURPOSE:** builds the whole operator-facing HTTP surface
**CURRENTLY USED:** YES — this is the live router
**AUTHENTICATION METHOD:** none
**AUTHORIZATION METHOD:** none

**FILE:** `internal/platform/httpx/server.go`
**FUNCTION:** `BaseMiddleware(cfg, requestTimeout)`, lines 167-177
**ROUTE:** applies to all `/api/v1/*` and orchestration routes
**PURPOSE:** `Recoverer → RequestID → AccessLog → SecurityHeaders → CORS → MaxBodyBytes → Timeout`
**CURRENTLY USED:** YES
**AUTHENTICATION METHOD:** none — verified: no step in this chain extracts or validates any identity
**AUTHORIZATION METHOD:** none

**FILE:** `internal/api/agent/peerauth.go`
**FUNCTION:** `PeerAuthMiddleware(store, logger, onAuthorized)`, line 47
**ROUTE:** `/api/v1/agent/*`, **libp2p listener only**
**PURPOSE:** resolves an already-Noise-authenticated libp2p Peer ID against `agent_peers`
**CURRENTLY USED:** YES — mounted in `internal/app/fleet.go:160-162`
**AUTHENTICATION METHOD:** libp2p Noise handshake (upstream of this middleware, not performed by it)
**AUTHORIZATION METHOD:** `fleet.PeerStore.AuthorizedPeer` lookup against `agent_peers`
**Human relevance:** none — this authenticates/authorizes a machine's transport identity, never a human.

**FILE:** `internal/api/agent/handler.go`
**FUNCTION:** `peerCert(r)` (line 344), `peerNodeID(r)` (line 361)
**ROUTE:** `/api/v1/agent/enroll`, `/renew`, `/telemetry`, `/heartbeat`
**PURPOSE:** extract machine identity from a verified TLS client cert or resolved Peer ID
**CURRENTLY USED:** YES
**AUTHENTICATION METHOD:** mTLS client certificate (HTTPS) or Noise+`agent_peers` (libp2p)
**AUTHORIZATION METHOD:** none at this layer for enroll/renew/telemetry/heartbeat; `agent_operation_grants` is checked only for the separate AgentOps `ContainerLogs` path (`internal/app/fleet.go:262`)
**Human relevance:** none.

No dead/unused auth code was found — there is no vestigial login handler, no commented-out JWT middleware, nothing scaffolded and abandoned. The absence is total, not partial.

## PHASE 5 — Frontend

Inspected `web/src/App.tsx`, `web/src/shell/Topbar.tsx`, `web/src/api/client.ts`, `web/src/api/types.ts`, full `web/src` directory tree, `web/package.json`.

| Feature | Status |
|---|---|
| Login page | **PLANNED ONLY** — `App.tsx:44` comment: *"a level of indirection with nothing on the other side of it"* referring to a hypothetical future "auth screen"; no route, no component exists |
| Logout functionality | Does not exist |
| Authentication state / auth context / provider | Does not exist — no context provider of any kind for identity |
| OIDC/OAuth integration | Does not exist |
| JWT handling | Does not exist |
| Cookie-based auth | Does not exist — `client.ts:78` sets `credentials: "same-origin"` (a fetch default, not an auth mechanism; no cookie is ever set for it to carry) |
| Route protection | Does not exist — every route in `App.tsx` is reachable unconditionally |
| Role-based / permission-based UI | Does not exist |
| Current-user API call | Does not exist |
| Admin UI / user-management UI | Does not exist |

Every item is **PLANNED ONLY or NOT PRESENT** — none are partially implemented or dead code.

## PHASE 6 — Documentation, Status-Tagged

| Document | Status | What it says about human auth |
|---|---|---|
| `docs/adr/0011-deferred-rbac.md` | **CURRENT** (accepted decision) | *"Atlas has no authentication."* Fixes the future shape (shared middleware auth, handler-level authorization, per-node permissions) — **DEFERRED**, not built. |
| `docs/adr/0012-connect-by-identity.md` | **CURRENT** (amended 2026-08-18) | About machine/Peer ID identity only. Amendment table explicitly separates this from any human concern. |
| `docs/context/ARCHITECTURE.md` | **CURRENT** | `## Identity` section covers HTTPS X.509 and libp2p Peer ID only — no human-identity section exists at all. |
| `docs/architecture/agent-design.md` | **CURRENT** for HTTPS transport | §9: *"Human authentication (Phase 6) must use a separate credential type... A node certificate must never grant a UI session, and a user session must never permit ingest."* — **FUTURE**, explicitly Phase 6. |
| `docs/architecture/security.md` | **PROPOSED** (`Status: Proposed` in its own header) | Diagram shows a "User RBAC (Future)" box explicitly labeled future. Describes target state, not built state. |
| `docs/security/security-guide.md` | **CURRENT** | *"Phase 0 has no authentication... Authentication and RBAC are Tier 4 items."* Planned-controls table lists OIDC/RBAC at Tier 4 — **DEFERRED**. |
| `docs/api/README.md` | **CURRENT** | Confirms `unauthenticated`/`permission_denied` error codes are reserved but says nothing implements them for users yet. |
| `docs/roadmap/phases.md` | **CURRENT** | Phase 5: *"Authentication (OIDC and API tokens) and RBAC"* — **FUTURE**, not yet reached (current work is pre-Phase-5 fleet/libp2p work). |
| `Roadmap.md` | **CURRENT**, high-level | No implementation detail; tier references only. |
| `CURRENT_STATE.md` | **HISTORICAL/SUPERSEDED** — dated 2026-08-13, predates migration `0011_agent_peers` (~08-17) and the ADR-0012 amendment (08-18) | §7/§14 claim agent-operation authorization has *"no separate, explicit, revocable record"* — **false today**; `agent_operation_grants` (migration 0010) already existed at the time this doc was written and is wired into current code. Its statements *about human auth* ("does not exist") remain accurate; its statements about *agent operation grants* are stale. |

No document claims human auth is currently implemented. All are internally consistent on that single point; the only contradiction found is `CURRENT_STATE.md`'s stale agent-grant gap claim (see Phase 10).

## PHASE 7 — Machine Identity vs Human Identity

**Human user identity: does not exist as a concept anywhere in the code.** There is no `User` type, no `UserID`, no field that could hold one.

**Machine/Agent identity — three distinct, code-verified concepts:**

| Identity | What it proves | Proven by | Where it lives |
|---|---|---|---|
| **NodeID** | Which physical/virtual machine this is | `hostid.Resolve` (stable across reboots/hostname changes) | `nodes.node_id`, threaded through every fleet table |
| **libp2p Peer ID** | Which cryptographic keypair is on the other end of *this connection* | Noise handshake, before any Atlas byte is exchanged | `agent_peers.peer_id`, resolved to NodeID+Environment by `PeerAuthMiddleware` |
| **X.509 certificate** | Same question, for the HTTPS transport | mTLS handshake, verified against Atlas's private CA | `node_credentials.fingerprint`, resolved to NodeID via URI SAN |

**Direct answers:**

- **Can a human user currently log into Atlas?** No. No login mechanism of any kind exists.
- **Can Atlas currently distinguish User A from User B?** No. There is no user concept; every unauthenticated caller of `/api/v1/*` is indistinguishable from any other.
- **Can Atlas currently assign a role to a human user?** No. `role` exists only on `agent_peers` (constrained to the single value `'agent'`) — not a human-facing concept.
- **Can Atlas currently grant a permission to a human user?** No. `agent_operation_grants` grants a *node* a permission to have an operation performed *against it* — it has no notion of who is asking.
- **Can Atlas currently restrict a human user's access to specific agents?** No — because there's no human identity to restrict in the first place. `scopeFrom(r)` (`internal/api/v1/system.go:214-216`) accepts an unauthenticated `?node=` query param; any caller can address any node.
- **Can Atlas currently restrict a human user's access to specific environments?** No, same reason. (`environment` exists as a node/peer attribute, not a user-access-scoping mechanism.)
- **Can Atlas currently implement organization/tenant isolation?** No. No `organization`/`tenant` concept exists anywhere in schema or code — Atlas is single-deployment, single-tenant by construction today.

## PHASE 8 — Actual Request Trace

**Representative endpoint 1 — operator-facing, `GET /api/v1/containers/{containerID}/logs?node=<id>`:**

```
HTTP request
    ↓
Router: internal/api/router.go New() (line 58) → mux match
    ↓
Middleware: httpx.BaseMiddleware (server.go:167)
    Recoverer → RequestID → AccessLog → SecurityHeaders → CORS →
    MaxBodyBytes → Timeout
    — NO identity/auth check occurs in any of these seven steps
    ↓
Handler: v1.Handler.ContainerLogs (internal/api/v1/containers.go:346)
    scope := h.scopeFrom(r) — reads raw "?node=" param, UNAUTHENTICATED
    ↓
Service: fleetPipeline.ContainerLogs (internal/app/fleet.go:247)
    grants.IsGranted(ctx, nodeID, "container_logs") — checks whether the
    NODE is authorized, not whether the CALLER is authorized. No caller
    identity exists to check.
    ↓
Database: agent_operation_grants row lookup
```

**Verdict:** zero user authentication or user authorization occurs at any stage. The one check present (`IsGranted`) answers a machine-authorization question, not a human one.

**Representative endpoint 2 — agent-facing, `POST /api/v1/agent/telemetry` (libp2p):**

```
HTTP request (over an already Noise-authenticated libp2p stream)
    ↓
Router: mux in internal/app/fleet.go:115-116
    ↓
Middleware: httpx.AgentMiddleware (server.go:184) then
    agentapi.PeerAuthMiddleware (fleet.go:160-162)
    — PeerAuthMiddleware DOES check identity: peer.Decode(r.RemoteAddr)
    (peerauth.go:58) then store.AuthorizedPeer (line 65) against agent_peers
    ↓
Handler: agent.Handler.Telemetry (handler.go:208)
    nodeID from PeerIdentityFrom(ctx) — the authenticated/authorized
    machine identity, never from the request body
    ↓
Service: transport.Router.Receive
    ↓
Database: metric_samples / inventory_snapshots / events
```

**Verdict:** authentication (Noise) and authorization (`agent_peers`) both occur, correctly separated — but this is machine identity throughout. No human identity is or could be involved on this path.

## PHASE 9 — Final Verdict

### 1. Executive Summary

- **Does Atlas currently have human-user login?** No.
- **Does Atlas currently have human-user RBAC?** No.
- **Does Atlas currently have permissions?** Only machine-scoped ones (`agent_operation_grants`); none for humans.
- **Does Atlas currently have organization/tenant access control?** No.
- **Does Atlas currently have API authentication?** For the agent-facing API (`/api/v1/agent/*`), yes — mTLS or libp2p Noise. For the operator-facing API (`/api/v1/*` everything else), no.
- **Does Atlas currently have machine/agent authorization?** Yes, and it's genuinely separated from authentication on both the Peer-ID (`agent_peers`) and operation-grant (`agent_operation_grants`) axes.

### 2. What Is Actually Implemented

- mTLS-based agent authentication with a private CA (HTTPS transport).
- Bounded, hashed enrollment tokens for first-contact agent bootstrap.
- libp2p Noise-based agent authentication (no cert, no token) — `docs/adr/0012` amendment.
- `agent_peers`: explicit, revocable authorization of a Peer ID to act as a node/environment.
- `agent_operation_grants`: explicit, revocable authorization for a node to have the privileged `container_logs` operation performed against it, independent of and checked separately from authentication, on **both** the control-plane side (`fleet.go:262`) and the agent side (`agentops.go:263`).
- Node denylist for out-of-band ejection.
- Credential redaction utilities (`internal/platform/redact`) that scrub token/secret-shaped strings from *collected* data — a defensive measure, not an auth mechanism.

Everything above is machine-identity. Nothing human-identity-related is implemented.

### 3. Human User Architecture

| Item | Status |
|---|---|
| User login | NO |
| User identity | NO |
| Sessions | NO |
| OIDC | NO |
| OAuth | NO |
| JWT | NO |
| API tokens (human-scoped) | NO |
| Roles | NO |
| Permissions | NO |
| RBAC | NO |
| Organization/Tenant | NO |

Every item is a flat NO — nothing is PARTIAL. There is no scaffolding, no stub, no half-built piece anywhere in the codebase.

### 4. Machine / Agent Architecture

- **Node identity**: `hostid.Resolve`-derived `NodeID`, stable across hostname changes/reboots — the row every fleet table hangs off.
- **Peer ID**: libp2p transport identity, cryptographically proven by the Noise handshake before any Atlas byte moves. Answers "who is on this connection," never "what may they do."
- **Certificates**: HTTPS-transport equivalent of Peer ID — mTLS client cert with NodeID in a URI SAN, issued by Atlas's private CA.
- **Enrollment**: the one-time act of trading a bounded token for a certificate (HTTPS only; libp2p has no enrollment step at all — see ADR-0012 amendment).
- **`agent_peers`**: authorization — "is this Peer ID allowed to act as this node in this environment." Independent table, independently revocable, never conflated with the Noise handshake that authenticates the connection.
- **`agent_operation_grants`**: a second, orthogonal authorization — "may `container_logs` be performed against this node at all," checked on top of (not instead of) a live authenticated session, on both ends of the connection.
- **What each protects**: `agent_peers` protects "can this keypair speak for this machine." `agent_operation_grants` protects "even a legitimate, currently-connected agent, is this one privileged action allowed against it." Neither protects, restricts, or has any awareness of a human caller.

### 5. Database Evidence

| Table | Exists | Purpose | Human or Machine | Used by Code |
|---|---|---|---|---|
| `nodes` | Yes | Machine inventory | Machine | Yes |
| `enrollment_tokens` | Yes | HTTPS bootstrap credential | Machine | Yes |
| `node_credentials` | Yes | Issued X.509 certs | Machine | Yes |
| `node_denylist` | Yes | Ejected node ids | Machine | Yes |
| `agent_operation_grants` | Yes | Privileged-operation authorization | Machine | Yes |
| `agent_peers` | Yes | Peer ID → node/environment authorization | Machine | Yes |
| `users` | No | — | — | — |
| `sessions` | No | — | — | — |
| `roles` | No | — | — | — |
| `permissions` | No | — | — | — |
| `organizations`/`tenants` | No | — | — | — |
| audit logs | No | — | — | — |

### 6. Code Evidence

```
internal/api/router.go
  Function: New(deps Deps)
  Status: IMPLEMENTED (no auth)
  Purpose: builds the full operator HTTP surface with zero identity checks

internal/platform/httpx/server.go
  Function: BaseMiddleware(cfg, requestTimeout)
  Status: IMPLEMENTED (no auth step present)
  Purpose: the middleware chain every /api/v1/* request runs through

internal/platform/httpx/middleware.go:251
  Comment on: CORS(allowedOrigins)
  Status: authoritative in-code confirmation
  Purpose: "Atlas has no session cookie yet"

internal/api/agent/peerauth.go
  Function: PeerAuthMiddleware(store, logger, onAuthorized)
  Status: IMPLEMENTED, machine-only
  Purpose: authorizes a Noise-proven Peer ID against agent_peers

internal/app/fleet.go:247-275
  Function: fleetPipeline.ContainerLogs
  Status: IMPLEMENTED, machine-only
  Purpose: checks agent_operation_grants before permitting a privileged op

internal/agent/config.go / relationship.go
  Status: IMPLEMENTED, machine-only
  Purpose: per-relationship (control-plane) trust config, no user concept
```

No file, function, or route claiming human auth was found to invent or omit.

### 7. Documentation Evidence

| Document | Status | What it says |
|---|---|---|
| `docs/adr/0011-deferred-rbac.md` | CURRENT | No auth exists; fixes future shape; DEFERRED |
| `docs/adr/0012-connect-by-identity.md` | CURRENT (amended) | Machine identity only |
| `docs/context/ARCHITECTURE.md` | CURRENT | No human-identity section exists |
| `docs/architecture/agent-design.md` | CURRENT (HTTPS parts) | Human auth explicitly named FUTURE (Phase 6) |
| `docs/architecture/security.md` | PROPOSED | User RBAC explicitly labeled "(Future)" in its own diagram |
| `docs/security/security-guide.md` | CURRENT | "Phase 0 has no authentication"; RBAC = Tier 4, DEFERRED |
| `docs/api/README.md` | CURRENT | Error codes reserved, unimplemented for users |
| `docs/roadmap/phases.md` | CURRENT | Human auth = Phase 5, FUTURE |
| `Roadmap.md` | CURRENT | No detail |
| `CURRENT_STATE.md` | HISTORICAL/SUPERSEDED | Correct on human auth (absent); stale on agent-grant "gap" claim — contradicted by current `agent_operation_grants` wiring |

### 8. Actual Current Architecture

```
Human User
    |
    X   ← no authentication exists; any caller reaches every /api/v1/* route
    |
Atlas API (internal/api)
    |
    ├── Operator-facing (/api/v1/*) — no auth, no authz
    |
    └── Agent-facing (/api/v1/agent/*)
            |
        Machine Identity
            |
        ┌───┴────┐
        │        │
    HTTPS mTLS   libp2p Noise
    (X.509,      (agent_peers
     enrollment_   allowlist)
     tokens)
        │        │
        └───┬────┘
            |
    agent_operation_grants (privileged-op authorization)
            |
        Atlas Agent
```

Only components verified present are drawn. No box exists for User/Session/Role/Permission/Organization because none exist in code.

### 9. Missing Architecture

Genuinely absent, and required for human login + RBAC:

- User table
- Session (or token) mechanism and its storage
- An identity provider integration (OIDC/OAuth) or a first-party credential store, whichever direction is chosen
- Authentication middleware (a shared chain segment per ADR-0011 §1, not duplicated across `BaseMiddleware`/`StreamMiddleware`)
- Role table
- Permission table
- User-to-role relationship
- Role-to-permission relationship (or a direct user-to-permission model, per ADR-0011's rejection of per-user ACLs in favor of roles)
- Resource-scoped authorization layer called from handlers (ADR-0011 §2 — explicitly not middleware, because the resource isn't resolved yet at that point)
- Membership/organization table, only if multi-tenant isolation becomes a real requirement (nothing today indicates it's an immediate one)
- Audit logging of user actions
- Frontend: login page, auth context/provider, route guards, current-user API integration

Not proposing an implementation — this is strictly an inventory of what's missing, mirroring the shape ADR-0011 already specifies.

### 10. Documentation vs Code Contradictions

**One real contradiction found, and it runs opposite to what you might expect:**

- **`CURRENT_STATE.md` §7 and §14** (dated 2026-08-13) claim: *"no separate, explicit, revocable record of 'this node is authorized for privileged operations'... A successful, valid connection is currently sufficient authorization."* This is false against current code. `agent_operation_grants` (migration `0010`) already existed and was already wired into `Enroller.Enroll` and `fleetPipeline.ContainerLogs` by that date — the doc simply wasn't updated to reflect work that had already landed. This is a **stale/historical** document, not a current contradiction to fix in code.

**No contradiction exists regarding human authentication specifically** — every document, including the stale `CURRENT_STATE.md`, agrees it doesn't exist. ADR-0011, the security docs, and the code are all mutually consistent on that one point: nothing claims human auth is implemented anywhere, so there is nothing to reconcile there.

### 11. Final One-Line Conclusion

**Atlas currently has no human-user authentication, identity, sessions, RBAC, permissions, or tenant isolation of any kind — its entire current security model is machine/agent identity (mTLS or libp2p Noise) and machine-scoped, independently-revocable authorization (`agent_peers`, `agent_operation_grants`), with human authentication and RBAC deliberately deferred (ADR-0011) and not yet begun.**

---

## Summary

1. **What Atlas supports today:** machine authentication (mTLS + libp2p Noise) and machine authorization (`agent_peers`, `agent_operation_grants`), both genuinely separated from each other; no human-facing auth of any kind.
2. **What Atlas does not support today:** user login, sessions, OIDC/OAuth/JWT, roles, permissions, RBAC, organizations/tenants, resource-level access control by a human caller, audit trails of human actions, any frontend auth UI.
3. **What documentation says is planned:** human authentication + RBAC as a Phase 5/Tier 4 item (`docs/roadmap/phases.md`, `security-guide.md`), with the shape fixed in advance by ADR-0011 (shared-chain authentication, handler-level resource-scoped authorization, per-node permissions, cookie/ticket-based WebSocket auth) and explicitly deferred until the remaining feature milestones are complete.
4. **What would need to be added later:** everything listed in Phase 9 §9 above — user/session/role/permission storage, an authentication mechanism (OIDC or first-party), a shared authentication middleware segment, a handler-level authorization policy layer, and (only if needed) an organization/membership model — built to the shape ADR-0011 already specifies rather than invented fresh at that time.
