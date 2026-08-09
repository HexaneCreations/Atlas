# Troubleshooting Guide

Symptoms, causes, and fixes. Every entry names the signal that identifies it,
because guessing between two causes with the same symptom is where most
incident time goes.

## First moves

```bash
curl -s localhost:8080/api/v1/system/info    | jq   # version, uptime — works without the database
curl -s localhost:8080/readyz                | jq   # which dependency is unhealthy
curl -s localhost:8080/api/v1/system/runtime | jq   # pool, event bus, goroutines
atlas-server config                                 # what configuration actually resolved to
```

`/api/v1/system/info` deliberately touches no dependency, so it answers when
everything else does not. Start there to confirm which version is running.

Every error response carries a `request_id`. Searching logs for it gives the
full server-side story, including the wrapped cause that the response
deliberately omits.

---

## Atlas will not start

### `invalid configuration: ...`

Configuration is validated before any component is constructed, and **all**
problems are reported at once. The message names each offending key.

```
atlas-server: invalid configuration:
  - database.ssl_mode: "require" does not verify the server certificate; production requires verify-ca or verify-full
  - server.allowed_origins: wildcard "*" is not permitted in production
```

Run `atlas-server config` to see which layer supplied a value. Precedence is
defaults, then the YAML file, then `ATLAS_` environment variables — so an
environment variable overriding a file setting is the usual surprise.

### `database is unreachable`

Atlas refuses to start without Postgres, deliberately: an Atlas that started
without a database would accept requests it cannot answer and silently discard
what it collects.

| Check | Command |
| --- | --- |
| Is the database up? | `docker compose ps` / `pg_isready -h <host> -U atlas` |
| Is the host and port right? | `atlas-server config \| jq .database` |
| Is the password reaching Atlas? | Confirm `ATLAS_DATABASE_PASSWORD_FILE` points at a readable file |
| Is TLS the problem? | Try `ATLAS_DATABASE_SSL_MODE=require` **temporarily** to isolate; if that fixes it, the certificate chain is the real issue |

### `N migration(s) are pending and migrate_on_start is disabled`

Expected when migrations are run as a separate deployment step. Run
`atlas-server migrate` first.

Refusing to start is deliberate — an application running against a schema older
than it expects fails in ways that look like data corruption rather than like a
missed migration.

### `migration ... has changed since it was applied`

An already-applied migration file was edited. This is caught by checksum,
because the alternative is that the author's database matches the new file
while everyone else's matches the old one, and nothing reports a difference
until a query fails in production.

- **Development:** `make db-reset`.
- **Production:** never edit the migration. Revert the file to its original
  content and write a **new** migration with the change.

### `bind 127.0.0.1:8080: address already in use`

Another process holds the port. `lsof -i :8080`, or set `ATLAS_SERVER_PORT`.

### `could not acquire the migration lock`

Another instance is migrating. Expected briefly during a rolling deploy — the
advisory lock is what makes concurrent starts safe. If it persists, a previous
migration may have left a session open:

```sql
SELECT pid, state, query, state_change
FROM pg_stat_activity
WHERE application_name = 'atlas' AND state <> 'idle';
```

### `extension "timescaledb" is not available`

The database image does not carry TimescaleDB, which is a hard requirement. Use
`timescale/timescaledb`, or install the extension. A managed Postgres that does
not offer it cannot host Atlas. See
[ADR-0003](../adr/0003-postgresql-timescaledb.md).

---

## Atlas is running but misbehaving

### `/readyz` returns 503

A **critical** dependency is failing. The body names which:

```json
{ "status": "unhealthy",
  "checks": [{ "name": "database", "status": "unhealthy", "critical": true,
               "error": "database is unreachable" }] }
```

A `degraded` status with a 200 means a *non-critical* check is failing —
reduced visibility, but not a reason to drain the instance.

### Requests return 500 with no useful message

By design. `internal` errors never disclose their cause, because it may carry
connection strings, host names, or query fragments. The full error is in the
log under the response's `request_id`:

```bash
grep '<request_id>' /var/log/atlas.log | jq
```

### A write request returns 405

Correct. Atlas is read-only and the router refuses write verbs before they
reach a handler. There is no configuration that enables writes.

### 404 on an endpoint that should exist

Check the path includes the version prefix (`/api/v1/...`) and the method is
`GET`. A wrong *method* on a real path returns 405, not 404, so a 404 means the
path itself is wrong.

### CORS errors in the browser

Atlas returns no CORS headers for an origin that is not allow-listed; the
browser then blocks the response.

- Add the exact origin to `server.allowed_origins` — scheme, host, and port
  must match exactly. `https://atlas.example.com` does not match
  `https://atlas.example.com:443`.
- In production a wildcard is rejected outright.
- In development, prefer the Vite proxy (`make web-dev`) over relaxing CORS.
  A relaxation made to get development working has a habit of surviving into
  deployment.

---

## Performance

### Slow responses; `slow query` in the logs

Every query over 250 ms is logged with its SQL and duration. Correlate by
`request_id` to find the endpoint.

```sql
-- Which queries are actually expensive?
SELECT query, calls, mean_exec_time, total_exec_time
FROM pg_stat_statements
WHERE query LIKE '%atlas%'
ORDER BY total_exec_time DESC LIMIT 20;
```

Usually a missing index or a query reading raw samples where a continuous
aggregate exists.

### `database.empty_acquire_count` rising

The pool is undersized: requests are waiting for a free connection. Raise
`database.max_conns`, but confirm the Postgres server's own `max_connections`
can accommodate it across all Atlas instances.

### `event_bus.dropped` non-zero and rising

A subscriber cannot keep up with its event rate. Events are dropped for that
subscriber alone — the bus never blocks a publisher, by design
([ADR-0008](../adr/0008-lossy-event-bus.md)).

The log names the subscriber:

```json
{"msg":"event dropped: subscriber queue is full","subscriber":"api.websocket-fanout",
 "pattern":"**","dropped_total":1423,"buffer_size":256}
```

Three fixes, in order of preference: make the consumer faster, narrow its
subscription pattern so it receives less, or raise
`event_bus.buffer_size`. Raising the buffer alone only delays the problem.

### `process.goroutines` climbing monotonically

A goroutine leak. The usual cause is a streaming handler or a collector that
does not return when its context is cancelled. Capture a profile:

```bash
curl -s localhost:8080/debug/pprof/goroutine?debug=2   # when profiling is enabled
```

> Profiling endpoints are not exposed in Phase 0. They arrive with the
> observability work in Phase 1, gated so they are never public.

### Memory growth

`heap_alloc_mb` on `/api/v1/system/runtime`. Steady growth with stable
goroutine count usually means retained references — commonly an unbounded cache
or a slice that is appended to and never truncated.

---

## Collector problems

> Applies from Phase 1, when collectors exist.

### A collector shows `healthy: false`

`/api/v1/system/health` (Phase 1) reports per-collector state, including
`consecutive_failures` and `last_error`. Common causes:

| Symptom | Cause |
| --- | --- |
| Timeouts every run | The source is unresponsive — a wedged NFS mount, a hung Docker daemon. The collector is cancelled rather than allowed to stall its schedule. |
| Permission denied | Atlas lacks read access to `/proc/<pid>`, the Docker socket, or a systemd unit. |
| `collector.run.panicked` events | A bug in the collector. It is isolated and the others keep running, but it needs fixing. |

### A plugin reports `not_detected`

Expected when the technology is absent. **This is not a fault** — the
distinction from `detection_failed` is the entire reason detection exists. A
host without Docker should report no Docker integration, not a broken one.

`detection_failed` means the probe itself errored: the socket exists but could
not be read, usually a permissions problem.

---

## Shutdown

### `shutdown deadline exceeded with N component(s) unstopped`

Graceful drain exceeded `server.shutdown_timeout` (default 20s). Usually a
long-running request or a collector ignoring cancellation.

Raise the timeout, and ensure the process manager allows more than it —
`TimeoutStopSec=45` for systemd, or a matching
`terminationGracePeriodSeconds` — so the drain is never cut short from outside.

### The process does not exit on SIGTERM

Check the logs for `stopping component`, which names how far shutdown got. The
last component logged is the one that hung. A second `SIGINT` kills
immediately.

---

## Escalation

When reporting a problem, include:

1. Output of `/api/v1/system/info` — the exact version and whether it is dirty.
2. Output of `/readyz` and `/api/v1/system/runtime`.
3. The `request_id` from a failing response.
4. Log lines around that `request_id`.
5. `atlas-server config` output — it is safe to share, as the password is never
   included.
