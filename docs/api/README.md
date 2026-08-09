# API Reference

Base path: `/api/v1`. See [versioning](versioning.md) for the compatibility
policy and [ADR-0010](../adr/0010-url-path-api-versioning.md) for why versions
live in the path.

## Read-only by construction

Atlas has no write endpoint, and this is enforced by the router rather than by
convention. Any request using `POST`, `PUT`, `PATCH`, or `DELETE` is answered
with `405 method_not_allowed` before reaching a handler:

```console
$ curl -s -X POST localhost:8080/api/v1/system/info | jq
{
  "error": {
    "code": "method_not_allowed",
    "message": "method POST is not allowed on this endpoint",
    "details": {
      "allowed": "Atlas is read-only; only GET, HEAD, and OPTIONS are supported",
      "method": "POST",
      "path": "/api/v1/system/info"
    },
    "request_id": "5b9b9e3df3afb4c50000000000000005"
  }
}
```

## The error envelope

**Every** failure returns the same shape — including router-level 404s and
405s, which means a client writes one error handler rather than one per
endpoint:

```json
{
  "error": {
    "code": "not_found",
    "message": "no such endpoint",
    "details": { "path": "/api/v1/nope" },
    "request_id": "5b9b9e3df3afb4c50000000000000003"
  }
}
```

| Field | Meaning |
| --- | --- |
| `code` | Stable classification. **Branch on this**, not on the message or the status. |
| `message` | Human-readable and safe to display. For internal errors this is a fixed generic string. |
| `details` | Optional structured context, such as the field that failed validation. |
| `request_id` | Correlates with server logs. Quote it in a bug report and the exact request can be found. |

### Codes and statuses

| Code | HTTP | Meaning |
| --- | --- | --- |
| `invalid_argument` | 400 | Malformed or semantically invalid input. Retrying unchanged will fail. |
| `unauthenticated` | 401 | No valid credentials presented. |
| `permission_denied` | 403 | Known caller, not permitted. |
| `not_found` | 404 | No such resource. |
| `method_not_allowed` | 405 | The endpoint exists, but not for this method. |
| `already_exists` | 409 | Would collide with an existing resource. |
| `failed_precondition` | 412 | System state does not permit the operation. |
| `rate_limited` | 429 | Quota exceeded; back off. |
| `internal` | 500 | Unexpected failure. The message is always generic. |
| `not_implemented` | 501 | In the API surface but unsupported by this deployment. |
| `unavailable` | 503 | A dependency is down. Retrying later may succeed. |
| `deadline_exceeded` | 504 | The operation ran out of time. |

`unavailable`, `deadline_exceeded`, and `rate_limited` are the retryable codes;
the TypeScript client exposes this as `ApiError.retryable`.

**An `internal` error never discloses its cause.** The message is fixed and
details are omitted, because the underlying error may contain connection
strings, host names, or query fragments. The full error is in the server log
under the same `request_id`. See
[ADR-0009](../adr/0009-typed-error-kernel.md).

## Headers

| Header | Direction | Purpose |
| --- | --- | --- |
| `X-Request-Id` | Both | Correlation id. An inbound value is honoured if it is alphanumeric with `-`, `_`, or `.` and at most 128 characters; anything else is replaced, because this value reaches log files and JSON responses. |
| `X-Content-Type-Options: nosniff` | Response | |
| `X-Frame-Options: DENY` | Response | Infrastructure data must never be framed. |
| `Referrer-Policy: no-referrer` | Response | |
| `Cache-Control: no-store` | Response | Responses are per-request infrastructure state. |

## Endpoints

### `GET /healthz` — liveness

Answers `200 {"status":"ok"}` whenever the process can serve.

**It checks no dependencies, deliberately.** Liveness asks only whether the
process is able to serve; probing dependencies here is a well-known way to turn
a brief database outage into a cascading restart of every instance, where the
orchestrator kills healthy processes for a fault they did not have and cannot
fix by restarting.

### `GET /readyz` — readiness

Returns the full health report. `200` when the instance should receive traffic,
`503` when it should not.

```json
{
  "status": "healthy",
  "checks": [
    { "name": "database", "status": "healthy", "critical": true, "duration_ms": 3 }
  ],
  "checked_at": "2026-08-06T01:48:48.120175+05:30"
}
```

A failing **critical** check gives `unhealthy` and `503`. A failing
non-critical check gives `degraded` and still `200` — reduced visibility is not
a reason to drain the last working instance.

### `GET /api/v1/system/info` — instance identity

Answers without touching the database, so it works when everything else does
not. This is the first endpoint to reach for during an incident.

```json
{
  "version": "1.4.0",
  "commit": "5f1d9272175a19363d63068a26b9691763cf685e",
  "build_time": "2026-08-05T20:18:40Z",
  "go_version": "go1.25.0",
  "platform": "linux/amd64",
  "dirty": false,
  "environment": "production",
  "started_at": "2026-08-06T01:48:45.577947+05:30",
  "uptime_seconds": 2.604461875,
  "api_version": "v1"
}
```

`dirty` reports whether the working tree had uncommitted changes at build time.
A dirty binary in production is worth knowing about: its commit hash does not
fully describe what is running.

### `GET /api/v1/system/health` — detailed health

The same report as `/readyz`, plus version and uptime.

**Always returns `200` when Atlas can answer at all.** It is a diagnostic for
humans and dashboards, and a non-200 would make the detail unreadable in
exactly the situation it is needed. The machine verdict lives in the `status`
field, and in `/readyz` for load balancers.

### `GET /api/v1/system/runtime` — Atlas's own resource use

```json
{
  "database": {
    "acquired_conns": 0, "idle_conns": 3, "total_conns": 3, "max_conns": 16,
    "empty_acquire_count": 1, "canceled_acquire_count": 0
  },
  "event_bus": { "subscribers": 0, "published": 0, "delivered": 0, "dropped": 0 },
  "process": {
    "goroutines": 8, "heap_alloc_mb": 0, "heap_sys_mb": 7,
    "gc_cycles": 0, "num_cpu": 12, "max_procs": 12
  }
}
```

A monitoring platform that cannot be monitored is a blind spot at the centre of
the observability strategy. Two fields deserve alerts:

- **`event_bus.dropped`** rising means a subscriber cannot keep up.
- **`database.empty_acquire_count`** rising means the pool is undersized.

## Client considerations

**Pagination, filtering, and sorting** are not yet defined; no Phase 0 endpoint
returns a collection. The conventions land in Phase 1 with the first list
endpoints and will be documented here before any are shipped.

**Authentication** is not yet implemented. Phase 0 deployments must be
protected at the network layer; see the
[security guide](../security/security-guide.md). Authentication and RBAC are
Tier 4 items, and the `unauthenticated` and `permission_denied` codes are
already reserved for them.

**Type definitions** for TypeScript clients are maintained by hand in
`web/src/api/types.ts`. At three endpoints, a code generator is more machinery
than the surface justifies. That trade flips once drift becomes likely; the
answer then is generation from an OpenAPI document, which is tracked as a
Phase 2 item in the [phase plan](../roadmap/phases.md).
