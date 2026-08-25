# ADR-0013: Human-user authentication and authorization

- **Status:** Accepted
- **Date:** 2026-08-25
- **Phase:** 4

## Context

ADR-0011 deferred human-user authentication and RBAC, but fixed the shape it
must take when built: a shared authentication segment composed into both
`BaseMiddleware` and `StreamMiddleware`, authorization as a handler-level
policy layer rather than middleware, cookie- or ticket-based WebSocket auth
(never a token in the URL), and permissions that bind per node. It also
recorded an explicit exit condition on the deferral itself:

> **Revisit immediately if** Atlas is bound to anything beyond localhost,
> deployed to a shared machine, or demonstrated on a network with untrusted
> parties on it.

That condition is no longer theoretical. `atlas.cyreneai.com` is live,
publicly reachable, and — until this ADR's implementation — unauthenticated,
including `GET /api/v1/containers/{id}/logs`, which ADR-0011 itself names as
"the single most sensitive thing the API serves." This is not a re-litigation
of the deferral; it is that deferral's own stated trigger firing, which by
ADR-0011's own terms moves authentication ahead of remaining feature
milestones regardless of where those milestones stood.

This ADR records what was built, not a new design: every shape decision
below was already fixed by ADR-0011. What this ADR adds is the concrete
schema, the permission vocabulary, and the two decisions ADR-0011 left open
— the credential mechanism and the role set.

## Decision

**Human-user identity, authentication, and authorization are built as a
domain fully parallel to, and never joined with, Agent identity** —
mirroring `internal/core/fleet` / `internal/storage/fleet` /
`internal/api/agent` structurally, for the human-user axis instead of the
machine one:

| Layer | Agent (existing) | Human user (this ADR) |
| --- | --- | --- |
| Domain logic | `internal/core/fleet` | `internal/core/user` |
| Storage | `internal/storage/fleet` | `internal/storage/user` |
| HTTP-layer identity resolution | `internal/api/agent` (`PeerAuthMiddleware`, `PeerIdentityFrom`) | `internal/api/session` (`AuthMiddleware`, `PrincipalFrom`) |
| Authentication proof | libp2p Noise handshake / mTLS | Session cookie (`atlas_session`), resolved server-side |
| Authorization record | `agent_peers`, `agent_operation_grants` | `user_node_roles` + `role_permissions` |

No table, type, or code path is shared between the two columns.
`user_node_roles` is never joined against `agent_peers` or
`agent_operation_grants`, the same independence the doc comment on
`internal/core/fleet/grants.go` requires between authentication and
authorization on the machine axis — applied here to keep the human axis
independent of the machine axis entirely, not just internally consistent.

```
Human User                          Atlas Agent
    │                                    │
Username + password                 libp2p connection
    │                                    │
bcrypt verify                       Noise handshake
    │                                    │
Session (cookie, SHA-256 hash)      Peer ID cryptographically verified
    │                                    │
AuthMiddleware (shared segment)     PeerAuthMiddleware (agent listener only)
    │                                    │
Principal on context                agent_peers lookup
    │                                    │
requireScope/requireNode            NodeID + Environment
(handler-level policy)                  │
    │                                agent_operation_grants
user_node_roles ⋈ role_permissions      │
    │                                    │
Allow / 401 / 403 / 5xx             Allow / 403
```

### Authentication

First-party username + password. No OIDC/OAuth provider: none is evidenced
anywhere in the repository, in `go.mod`, or in `web/package.json` (confirmed
by the investigation report this ADR follows), and introducing one without
evidence is exactly what the safety rules governing this work prohibit.
`username` is the login identifier — not `email`, which is optional,
non-unique, and never a credential (`migrations/0012_users.sql`).

Passwords are hashed with bcrypt (`internal/core/user/password.go`),
deliberately different from the plain SHA-256
`internal/core/fleet.HashToken` uses for enrollment tokens: a token is 256
bits of generated entropy that a fast hash does not weaken, a human password
is not, and an adaptive hash is what makes an offline attack against a
stolen hash expensive. `golang.org/x/crypto` was already an indirect
dependency (pulled in by the libp2p stack); this ADR's implementation is the
first direct use of its `bcrypt` package and required no new module.

### Sessions

One mechanism serves both the REST chain and the streaming chain, per
ADR-0011 §3: a server-side session row (`sessions`, keyed by the SHA-256 hash
of a 256-bit token, mirroring `enrollment_tokens`' shape) behind an
`HttpOnly`, `SameSite=Lax` cookie, `Secure` whenever the environment is
production. Never a bearer token in a URL query string — `AccessLog` runs in
the streaming chain and would record one in plaintext, which is the exact
failure ADR-0011 §3 rules out.

`AuthMiddleware` (`internal/api/session/session.go`) is the one shared
definition both `httpx.BaseMiddleware` and `httpx.StreamMiddleware` splice
in — the composition happens once, in `internal/api/router.go`, and the
identical `Middleware` value is passed into both constructors. Installing it
on one chain and not the other is the specific failure mode ADR-0011 names
as highest-risk; passing one value into two call sites makes it a compile-time
impossibility rather than a review-time hope.

The middleware **never rejects a request itself**. A missing, expired,
revoked, or otherwise invalid cookie all leave the request anonymous — the
same collapse `user.ErrSessionInvalid` already performs at the store layer,
so an unauthenticated caller cannot distinguish "no session" from "revoked"
from "expired." Whether anonymous is acceptable is answered downstream, per
ADR-0011 §2.

### Authorization

A handler-level policy layer, not middleware, per ADR-0011 §2: middleware
runs before a handler has resolved which node a request names, and the
question here — *may this principal act on this node* — needs that node.
`user.Authorizer.Require` (`internal/core/user/authz.go`) is called from
`Handler.requireScope`/`requireNode` (`internal/api/v1/auth.go`), which is
itself the single point every node-scoped handler now calls through — the
same chokepoint `scopeFrom` already was for scope resolution, confirmed by
grep to have exactly the call sites listed below and no others.

Permissions bind per node, per ADR-0011 §4: `user_node_roles.node_id` names
one node, or is `NULL` for fleet-wide. There is no default between them —
`GrantSpec.Validate` and `atlas-server user grant`'s `--node-id`/`--fleet-wide`
flags require the caller to choose explicitly, so an empty or omitted field
can never silently become the broadest possible grant.

Two permissions, not a generic CRUD set, matching the resources Atlas
actually has:

| Permission | Covers | Held by |
| --- | --- | --- |
| `node.read` | Every node-scoped inventory read: containers, processes, services, ports, mounts, cron, the service graph, per-node health/cost/capacity/signals | viewer, operator, admin |
| `node.logs.read` | Container log content specifically | operator, admin |

`node.logs.read` is separate because ADR-0011 names container log content as
uniquely sensitive; a viewer can see that a container exists without being
able to read what it printed. The role set is exactly the three ADR-0011's
own alternatives section names — viewer, operator, admin — with no fourth
role added.

### Fail-closed property

Both functions on the critical path were traced explicitly (see the
accompanying review) and each carries a test proving it: a backend failure —
not merely a normal denial — in either function never results in an allowed
request.

- `AuthMiddleware`: a session-store error of any kind (not just
  `ErrSessionInvalid`) takes the same branch as an absent cookie — no
  principal is attached, the request proceeds anonymous. Proven by
  `TestAuthMiddlewareTreatsADatabaseErrorAsAnonymousNeverAuthenticated`.
- `Authorizer.Require`: `if err != nil { return err }` runs before the `!ok`
  check, so a store that fails — even one that incorrectly also reports
  `true` — still returns a non-nil error, which `requireScope` propagates,
  which `httpx.Handler.ServeHTTP` renders as a non-2xx response by
  construction (there is no path back to `nil` once an error is returned).
  Proven at the unit level by
  `TestAuthorizerRequirePropagatesStoreFailureRatherThanTreatingItAsDenial`
  and end-to-end over real HTTP by `TestDBErrorDuringAuthzNeverProducesA200`
  (authenticated caller, failing permission store, asserts the response is
  never 200 and the error is never misreported as `permission_denied`).

### Hardening added during review

Three gaps identified during review of this same implementation were closed
before acceptance, not deferred to a later ADR:

- **Privileged writes.** `POST/PUT/DELETE` on `/alerts/rules`, `/slo`, and
  `/notifications/channels` configure Atlas itself rather than describing a
  node, so they don't fit `node.read`/`node.logs.read`. They reuse the
  existing fleet-wide grant mechanism instead of a second grant table: a
  `user_node_roles` row with `node_id IS NULL` already matches any node id
  `HasPermission` is asked about, so a new permission,
  `fleet.write` (`migrations/0013_fleet_write_permission.sql`, seeded to
  `operator`/`admin`, not `viewer`), checked via
  `Handler.requirePermission` (`internal/api/v1/auth.go`) with an empty node
  id, is a global permission with no schema change beyond registering it.
  All 9 write handlers call it.
- **Login throttling.** `user.LoginLimiter` (`internal/core/user/loginlimit.go`)
  bounds `POST /auth/login` by two independent token-bucket budgets —
  5 attempts/15 min per username, 20/15 min per source IP — checked, and
  charged, before password verification. The username budget resets on a
  successful login for that username so past typos don't penalise a later
  visit; the IP budget deliberately never resets, closing the gap where one
  valid low-privilege credential could otherwise be used to keep clearing an
  attacker's own IP budget while guessing at other accounts from it.
- **Startup validation.** The session cookie's `Secure` attribute is only
  set when `Environment.IsProduction()`; binding non-loopback while still in
  development or staging would serve that same non-Secure cookie onto a
  reachable network. `config.Validate` (`internal/platform/config/validate.go`)
  now refuses to start in that combination — the same fail-fast posture
  every other production-hardening rule in that file already uses, applied
  to the specific misconfiguration that made `atlas.cyreneai.com`'s exposure
  possible in the first place.
- **Real-client-IP resolution behind this deployment's actual two-hop
  proxy chain.** `httpx.ClientIP` (`internal/platform/httpx/clientip.go`)
  now parses `X-Forwarded-For` counting from the right by exactly
  `TrustedProxyHops` (2: Caddy, then atlas-ui's nginx), rather than trusting
  `RemoteAddr` alone — the login limiter's per-IP budget otherwise collapsed
  every caller behind the same reverse proxy onto one shared bucket.
  Verified against Caddy's own documented default (no `trusted_proxies`
  configured in `deploy/caddy/Caddyfile`, so Caddy already discards any
  client-supplied `X-Forwarded-For` before setting it fresh) rather than
  assumed; the right-counting algorithm stays correct even if that default
  ever changes. Falls back to `RemoteAddr` — never a guess — when the
  header has fewer entries than the topology guarantees.

## Alternatives considered

**Unify human-user identity with Agent identity (one `Principal` type, one
authorization table).** Rejected outright — this is the one thing every
safety rule governing this work explicitly forbids, and for good reason: a
human logging in and an Agent's Peer ID being authorized are different
questions with different threat models. A compromised session cookie must
never grant Agent-level fleet access, and a compromised Agent keypair must
never grant a human login.

**JWT instead of a server-side session.** Rejected: the moment a token is
revoked (logout, an operator disabling an account, a compromised session),
a signed JWT is either still valid until expiry or requires a blocklist —
which is a session table by another name, built worse. A server-side session
row is instantly and unconditionally revocable, which is what
`RevokeSession`'s idempotent revoke and `Logout`'s behavior depend on.

**Authorization in middleware.** Reaffirms ADR-0011 §2 rather than
reopening it: middleware still runs before the resource is resolved, so a
finer-than-route check still has to re-resolve it, still inviting two
resolutions to disagree.

**Per-user ACLs instead of roles.** Reaffirms ADR-0011's own alternatives
section: an infrastructure console has few, stable actor kinds, and per-user
grants would need administrative UI this scale does not justify.

**OIDC/OAuth against an external identity provider.** Rejected for now,
not forever: nothing in the repository evidences a provider, and the
Development Rules governing this work prohibit introducing one without
evidence. First-party username/password is the smallest mechanism that
satisfies ADR-0011's shape; nothing about the shape (session cookie,
`Principal`, `Authorizer`) forecloses adding OIDC as a second credential
path into the same session mechanism later.

## Consequences

**Good.**

- `GET /api/v1/containers/{id}/logs` and `/logs/follow` — the endpoint
  ADR-0011 named explicitly — now require `node.logs.read`, checked before
  the WebSocket handshake and before Docker is ever touched.
- The fail-closed property is not just designed but tested at three levels
  (session resolution, permission check, end-to-end HTTP), specifically
  because this is the highest-stakes change in the repository to date.
- Agent authentication and authorization are unmodified and independently
  verified still working (existing `internal/core/fleet`,
  `internal/api/agent`, and libp2p integration tests unchanged and passing).
- ADR-0011's shape held without rework: nothing here required revisiting the
  shared-middleware-segment or handler-level-policy decisions, which is the
  outcome "fix the shape now" was for.
- Privileged configuration writes (notification channels — including
  webhook registration — alert rules, SLO definitions) and login itself are
  no longer reachable, or no longer unboundedly guessable, without
  authentication — see "Hardening added during review" above.
- A non-loopback bind outside production — the specific misconfiguration
  that made the live, unauthenticated exposure possible — is now a startup
  failure, not a deployment-time surprise.

**Costs.**

- Fleet-wide list/rollup endpoints (`/nodes`, `*Fleet` variants) and reads
  on alert rules, SLO definitions, and notification channels remain
  unauthenticated; the node-scoped hookpoint this ADR used does not cover
  them, and they need a separate answer (per-row filtering, not a single
  node-scoped or fleet-wide check).
- Session TTL is a fixed 24-hour constant, not yet operator-configurable.

**Revisit when** fleet-wide/list-endpoint authorization is designed — at
that point, decide whether it extends `fleet.write`'s pattern to reads
(`fleet.read`) or needs genuine per-row filtering that a single permission
check cannot express. Also revisit if a fourth role becomes necessary —
nothing here forecloses it, but it is a schema and code change, not a value
an operator can introduce by typing a new string — or if `TrustedProxyHops`
(`internal/platform/httpx/clientip.go`) ever needs to change: this
deployment's reverse-proxy chain (Caddy, then atlas-ui's nginx — see
`deploy/caddy/Caddyfile`, `docker-compose.prod.yml`) is fixed at exactly two
hops today, and `ClientIP` counts `X-Forwarded-For` positions from the right
against that exact number, verified against
[Caddy's own documented default](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy)
("by default, the proxy will ignore \[X-Forwarded-For's] values from
incoming requests, to prevent spoofing") — a hop added or removed from that
chain must update the constant alongside it.

## Supersedes

Formally supersedes ADR-0011's `Accepted` status: ADR-0011's decision — the
shape fixed in advance of building — is not reversed by this ADR, it is
fulfilled by it. ADR-0011's Context, Decision, and Alternatives sections
remain the accurate record of *why* this shape was chosen and are preserved
unedited, per this document set's own immutability rule; only its Status
line changes, to point here.
