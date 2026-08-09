# API Versioning Policy

Atlas versions its API in the URL path: `/api/v1/...`. The reasoning, and the
alternatives rejected, are in
[ADR-0010](../adr/0010-url-path-api-versioning.md).

## The contract

Atlas's API is consumed by its own console, by operators using `curl`, by
scripts, by dashboards, and eventually by alerting rules. **None of these are
upgraded in lockstep with the server.** A script written during an incident two
years ago should still work.

## What may change within a version

**Allowed — clients must tolerate these:**

- A new endpoint.
- A new field in a response object.
- A new optional query parameter.
- A new value in an open-ended enumeration, such as a new `status` on a health
  check.
- A new error `code`, provided existing codes keep their meaning.

**Not allowed — these require a new version:**

- Removing or renaming a field.
- Changing a field's type, unit, or meaning.
- Changing the HTTP status for an existing condition.
- Removing an endpoint.
- Making an optional parameter required, or narrowing accepted values.
- Changing an existing error `code` for an existing condition.

## What this asks of clients

Clients must ignore unknown fields rather than reject them. Both first-party
clients do: the Go tests decode into typed structs, and the TypeScript client
uses interfaces that tolerate extra keys.

Clients should branch on `error.code`, never on the HTTP status or the message
text. Codes are the stable contract; messages are prose and may be reworded for
clarity within a version.

## Endpoints outside versioning

`/healthz` and `/readyz` are **not** versioned and never will be. They are
consumed by orchestrators, whose probe configuration should never have to
change because an API version did. Their response bodies may gain fields; their
status-code semantics are frozen.

## Introducing a new version

1. Add `internal/api/v2/` with its own handlers.
2. Mount it in `api.New` alongside v1 — one line.
3. Both versions serve from the same binary while clients migrate.

Handlers are kept thin — read a request, call a collaborator, render — so a
second version re-renders the same domain result rather than reimplementing
behaviour. That is what keeps running two versions from doubling the
maintenance surface.

## Deprecation

> **Status: policy defined, mechanics not yet implemented.** No deprecation has
> occurred, and none can until there is more than one version. The supporting
> headers are due in Phase 5, alongside the authentication work that gives
> Atlas a way to identify which clients are still on an old version.

When a version is deprecated:

1. **Announce**, with the replacement and the migration path documented.
2. **Signal in every response** of the deprecated version:
   ```http
   Deprecation: true
   Sunset: Sat, 01 Aug 2026 00:00:00 GMT
   Link: <https://docs/api/v2>; rel="successor-version"
   ```
   `Deprecation` and `Sunset` are the standard headers (RFC 8594) so tooling
   can detect them without knowing about Atlas.
3. **Minimum twelve months** between announcement and removal. Atlas is
   infrastructure; the things calling it are often themselves infrastructure.
4. **Log usage** of the deprecated version so the remaining callers can be
   identified and contacted rather than surprised.
5. **Remove** only after the sunset date, and only once logs show no
   significant traffic.

## Version discovery

`GET /api/v1/system/info` returns the version serving the request:

```json
{ "api_version": "v1", "version": "1.4.0" }
```

`api_version` is the API contract; `version` is the build. They move
independently — many Atlas releases will serve the same API version, which is
the intended outcome of the additive rule.
