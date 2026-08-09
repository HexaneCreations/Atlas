# ADR-0010: URL-path API versioning

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

The Atlas API will be consumed by its own web console, by operators using
`curl`, by scripts, by dashboards, and eventually by alerting rules and CI
integrations. Those consumers are not upgraded in lockstep with the server —
a script written during an incident two years ago should still work.

## Decision

**Version in the URL path: `/api/v1/...`. A version is additive-only, and a
breaking change means serving a new version alongside the old.**

Within a version:

- Adding an endpoint, adding a response field, or adding an optional query
  parameter is allowed.
- Removing or renaming a field, changing a field's type or meaning, changing a
  status code, removing an endpoint, or making an optional parameter required
  is **not** allowed.

Orchestration probes — `/healthz` and `/readyz` — sit outside versioning
entirely, so a Kubernetes probe configuration never has to change because the
API version did.

## Alternatives considered

**Header-based versioning (`Accept: application/vnd.atlas.v1+json`).** More
theoretically correct: the URL identifies the resource, the header negotiates
its representation. Rejected on practicality, which matters more here than
theory. It is invisible in logs and dashboards, easy to omit accidentally —
producing a silent default rather than an explicit choice — awkward to use with
`curl` during an incident, and unusable in a browser address bar. For an
operations tool, being able to paste a URL to a colleague is a real feature.

**Query-parameter versioning (`?version=1`).** Easy to use. Rejected because it
mixes an API contract with request parameters, interacts badly with caching,
and makes it ambiguous whether the version applies to the route or the query.

**No versioning; never break.** Tempting for an internal tool. Rejected as
unrealistic over the lifetime described in the roadmap. The result is not an
unbroken API but an unversioned one that breaks anyway, with no way for a
client to detect it.

**Per-endpoint versioning.** Maximum flexibility, minimum comprehensibility.
Rejected: a client would have to track a version per route.

## Consequences

**Good.**

- Obvious in logs, dashboards, proxies, and access records.
- Trivially usable from `curl`, a browser, or a script.
- A new version is a new package (`internal/api/v2/`) plus one line in
  `api.New`, so v1 and v2 can be served simultaneously from one binary while
  clients migrate.
- The additive rule means most changes need no version bump at all.

**Costs.**

- Supporting two versions means real duplication. Mitigated by keeping handlers
  thin — read, delegate, render — so a second version re-renders the same
  domain result rather than reimplementing behaviour.
- Path versioning is often criticised as conflating identity with
  representation. Accepted knowingly; the operational benefits outweigh it.
- A deprecation policy is required and is not yet written. See
  [API versioning](../api/versioning.md), which records the policy and the
  Phase in which the supporting mechanics (deprecation headers, a sunset
  timeline) are due.

**Revisit if** Atlas ever needs content negotiation for genuinely different
representations of the same resource, at which point headers can supplement —
not replace — the path version.
