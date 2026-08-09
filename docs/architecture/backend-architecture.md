# Backend Architecture

## The layering rule

Three layers, one direction of dependency:

```
cmd/         entry points — parse flags, call app
  └── internal/app/        composition root — constructs everything
        ├── internal/api/    HTTP surface
        ├── internal/core/   domain contracts
        └── internal/platform/  technical capability
```

**`platform` may not import `core` or `api`. `core` may not import `api`.**

This is enforced by review and by the shape of the packages, and it is the
single most valuable structural property of the codebase. It means:

- Every platform package can be tested with no domain concept present. The
  event bus test knows nothing about containers; the HTTP test knows nothing
  about metrics.
- Every core package can be tested with no infrastructure. A collector is
  tested by calling `Collect` and inspecting the samples, with no database and
  no HTTP server.
- A new feature tier adds packages rather than modifying the foundation.

## Package reference

### `internal/platform` — technical capability

Nothing here knows what Atlas observes. Each package solves one infrastructure
problem and would be equally at home in an unrelated service.

| Package | Responsibility | Notable property |
| --- | --- | --- |
| `errs` | Typed error kernel | Separates client-safe message from operator-only cause; internal errors cannot leak their detail |
| `log` | Structured logging on `log/slog` | Context-propagated attributes; credential-shaped keys redacted before they reach the writer |
| `config` | Layered configuration | Defaults → YAML → environment; validated once at startup; secrets only from environment or mounted file |
| `id` | Identifier generation | Process-random prefix plus monotonic counter; cheap enough for a per-event hot path |
| `eventbus` | In-process publish/subscribe | Bounded per-subscriber queues; drops rather than blocks |
| `lifecycle` | Component supervisor | Ordered start, reverse-order stop, rollback on failed start, bounded shutdown |
| `httpx` | HTTP plumbing | Middleware chain, uniform error envelope, supervised server |
| `health` | Dependency health aggregation | Critical versus non-critical distinction drives readiness |
| `build` | Binary identity | Link-time stamping with VCS fallback |
| `postgres` | Database access | pgx pool, forward-only migrator with advisory locking and checksum verification, query tracer |

### `internal/core` — domain contracts

What Atlas means by an observation, and how observations are produced and moved.

| Package | Responsibility |
| --- | --- |
| `collect` | `Sample`, `Batch`, `Collector`, `Descriptor`, and the collector registry. The narrow waist: everything that produces data produces these types. |
| `transport` | `Envelope`, `Origin`, `Transport`, `Sink`, and the in-process implementation. The seam between collection and storage. |
| `scheduler` | Runs collectors on their intervals with the safety rules that keep Atlas from harming its host. |
| `plugin` | The extension contract: descriptor, detect, init, collectors, close — plus the registry that activates them. |

### `internal/api` — HTTP surface

| Package | Responsibility |
| --- | --- |
| `api` | Route composition, orchestration probes (`/healthz`, `/readyz`), middleware assembly |
| `api/v1` | Version 1 handlers. Thin: read a request, call a collaborator, render. |

### `internal/app` — composition root

One file constructs the entire dependency graph and registers components with
the supervisor in dependency order. There is no container and no reflection;
see [ADR-0004](../adr/0004-constructor-injection.md).

## Cross-cutting mechanics

### Error handling

Every error crossing a layer boundary carries an `errs.Code`. The HTTP layer
maps codes to statuses in exactly one table, so no handler picks a status and
no two endpoints disagree about what "not found" means.

The type separates two audiences and the separation is enforced, not merely
recommended:

- `errs.Message(err)` and `errs.Details(err)` return client-safe values, and
  return a fixed generic message for internal or unclassified errors.
- `err.Error()` returns the full chain including wrapped causes, for logs only.

A database driver error containing a host name and a failed credential
therefore cannot reach an API response, regardless of what a handler does with
it. See [ADR-0009](../adr/0009-typed-error-kernel.md).

### Logging

`log.New` returns an `*slog.Logger` wrapped in a context-aware handler.
Attributes attached with `log.WithAttrs(ctx, …)` appear on every record logged
with that context downstream — so the request id set once by middleware lands
on every line emitted while serving that request, including lines from the
database query tracer, without any function threading a logger through its
signature.

Attribute keys that look like credentials (`password`, `token`, `secret`,
`api_key`, and similar, matched as substrings) have their values replaced
before reaching the writer.

### Request pipeline

Middleware is applied outermost first:

```
Recoverer          catches panics in everything below, including the logger
  RequestID        correlation id on context, response header, and log attrs
    AccessLog      one structured line per completed request
      SecurityHeaders
        CORS
          MaxBodyBytes
            Timeout
              JSONErrorFallback   rewrites router 404/405 into the envelope
                ServeMux          method-and-pattern routing
                  Handler
```

The order is load-bearing and asserted by tests. `Recoverer` outermost means a
panic anywhere is caught; `RequestID` above `AccessLog` means every log line
including a recovered panic carries the correlation id.

`JSONErrorFallback` sits directly outside the mux rather than being registered
as a catch-all route. A catch-all matches before the router can determine that
a path exists under a different method, which would silently turn every `405`
into a `404` — and `405` is exactly the signal that tells a caller they tried
to write to a read-only API.

### Concurrency

Atlas is concurrent in four places, and each has an explicit discipline:

| Where | Discipline |
| --- | --- |
| Event bus | `RWMutex` around the subscriber map; sends are non-blocking under the read lock, so a publisher never serialises behind a subscriber and `Close` cannot race a send |
| Scheduler | One goroutine per collector; a buffered channel as a concurrency semaphore; per-collector state behind its own mutex |
| Lifecycle | Mutex around the component list; the started list is captured under lock before shutdown |
| HTTP server | Mutex guarding the listener and server handoff between `Start` and `Addr` |

Every test suite runs under `-race` in CI. This is not optional for Atlas: a
data race in the scheduler or the bus corrupts the observations the platform
exists to produce.

### Database access

Atlas uses pgx directly rather than `database/sql`, because it depends on
Postgres specifics — TimescaleDB hypertables, `COPY` for bulk metric ingest,
`LISTEN`/`NOTIFY`, array and JSONB parameters — and the portable interface
would hide exactly the features the platform is built on.

Migrations are embedded in the binary, forward-only, applied under a
session-level advisory lock, and checksum-verified so an already-applied
migration cannot be edited. See
[ADR-0007](../adr/0007-forward-only-migrations.md) and the
[schema reference](../database/schema.md).

## Adding to the backend

| To add… | Do this |
| --- | --- |
| A new observable technology | Write a plugin under `internal/plugin/<name>/`. See the [plugin guide](../plugins/plugin-development.md). |
| A new metric from an existing technology | Add a `Collector` in that plugin. Nothing else changes. |
| A new endpoint | Add a handler in `internal/api/v1/` and one line in `Mount`. |
| A new API version | Add `internal/api/v2/` and one line in `api.New`. |
| A new technical capability | Add a package under `internal/platform/`, importing nothing above it. |
| A schema change | Add `migrations/NNNN_description.sql`. Never edit an applied migration. |
