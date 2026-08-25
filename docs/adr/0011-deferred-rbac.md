# ADR-0011: Authentication and RBAC, deferred with a fixed shape

- **Status:** Superseded by [ADR-0013](0013-human-user-authentication-and-authorization.md)
- **Date:** 2026-08-07
- **Phase:** 4

## Context

Atlas has no authentication. Every endpoint is open, including
`/api/v1/containers/{id}/logs`, which streams container output — the single
most sensitive thing the API serves, because application logs routinely carry
credentials, tokens, and customer data that no other endpoint exposes.

RBAC is a Tier 4 item. The decision recorded here is *when* to build it and
*what shape it must take*, taken deliberately in advance of building it, because
two structural facts in the current code will silently make the wrong shape
easier than the right one:

**The middleware chain is duplicated, not shared.** `BaseMiddleware`
(`internal/platform/httpx/server.go`) and `StreamMiddleware`
(`internal/platform/httpx/middleware.go`) are two independently maintained
`Chain(...)` lists. They differ only in that the streaming chain omits `CORS`
and `Timeout`, both for documented reasons. A WebSocket upgrade is dispatched to
the streaming chain by a branch in `api.New` that runs *before* the mux, so both
chains serve the same routes. Middleware added to one is not added to the other,
and nothing in the code makes that visible.

**Endpoints are host-scoped, not node-scoped.** `/containers`, `/ports`,
`/processes`, and `/mounts` describe the host Atlas runs on. This is already
recorded as the largest architectural debt in the project. It matters here
because permissions in a fleet tool are naturally expressed per node.

## Decision

**Defer implementation until the remaining feature milestones are complete.
Fix the shape now, and hold two constraints in the meantime.**

When it is built:

1. **Authentication belongs to a shared chain segment that both
   `BaseMiddleware` and `StreamMiddleware` compose from** — not to either
   chain directly. Adding it to one list and not the other must be impossible
   by construction rather than caught by review.

2. **Authorization is a policy layer called from handlers, not middleware.**
   Middleware answers *who* (identity, and the coarse authenticated/anonymous
   gate). Whether an identity may act on a resource requires the resolved
   resource, which middleware does not have. A denial returns a typed error;
   the error kernel (ADR-0009) maps it to 403 in one place.

3. **WebSocket authentication uses a cookie or a short-lived single-use
   ticket.** The browser WebSocket API accepts a URL and subprotocols and
   nothing else, so an `Authorization` header is not available to the console.
   A long-lived token must never be passed as a query parameter: `AccessLog`
   runs in the streaming chain and would record it.

4. **Permissions bind per node**, not globally.

Two constraints hold while this is deferred:

- **No third middleware chain.** Every chain added now is another place
  authentication must later be threaded through.
- **New endpoints must be able to name their node**, even while they answer for
  the local host only.

## Alternatives considered

**Implement authentication and RBAC now, before feature completion.** The
security-correct answer, and rejected knowingly: Atlas is not deployed or
exposed, and stopping feature work to build auth plumbing that has no consumer
yet would slow the milestones that make the product real. The cost is recorded
in Consequences and is not small.

**Route-prefix RBAC using a router with subrouter groups (chi, gin, echo).**
The common approach: group routes and attach role middleware per group.
Rejected because Atlas's authorization questions are resource-scoped — *may
this user stream logs from this container*, *may they see this node* — and a
path prefix cannot express them. It would also mean migrating the router for
roughly a tenth of the problem. Stdlib `http.ServeMux` with Go 1.22+ method
patterns already covers the routing this needs; chi remains a mechanical swap
later if per-route wrapping becomes repetitive, precisely because both are
`http.Handler`.

**Authorization entirely in middleware.** Uniform and easy to audit, and it
works for the coarse gate. Rejected as the general mechanism: middleware runs
before the handler has resolved the resource, so anything finer than
path-and-method has to re-resolve it — duplicating the lookup and inviting the
two resolutions to disagree.

**Per-user ACLs rather than roles.** Maximum expressiveness. Rejected as
disproportionate: an infrastructure console has few, stable actor kinds
(viewer, operator, admin), and per-user grants would need an administrative UI
that is not otherwise justified.

**Bearer token in the WebSocket URL query string.** The usual workaround for
the missing header. Rejected outright: it is recorded by access logging, proxy
logs, and browser history. The ticket alternative gives the same ergonomics
without a durable credential in a logged field.

## Consequences

**Good.**

- Feature milestones proceed without auth plumbing that has no consumer yet.
- `httpx.Handler` returns `error`, so adding an authorization check to a handler
  is a return statement, not a response rewrite. The 403 path is already built.
- Fixing the shape now means the deferral costs implementation time only, not
  redesign time.

**Costs.**

- **Every endpoint built between now and then is written, tested, and reviewed
  against an unauthenticated baseline.** Retrofitting means auditing all of
  them, not adding one middleware. This is the real price of the deferral.
- The node-scoping debt compounds with each host-scoped endpoint added.
- The duplicated-chain hazard is live for the entire deferral window: whoever
  eventually adds authentication must add it in two places, and the failure mode
  — WebSocket endpoints silently unauthenticated, container logs first among
  them — produces no error and no failing test.
- Frontend work will assume an unauthenticated API: no login state, no token
  refresh, no 401 handling anywhere in the console.

**Revisit immediately if** Atlas is bound to anything beyond localhost, deployed
to a shared machine, or demonstrated on a network with untrusted parties on it.
At that point authentication moves ahead of the remaining feature work
regardless of milestone order, because the exposure is no longer theoretical.
