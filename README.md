# Atlas

> **Observe Everything. Control Nothing.**

Atlas is a production-grade Internal Developer Platform for infrastructure
observability — a single pane of glass over servers, containers, services,
processes, binaries, cron jobs, logs, and operational health.

It is **strictly read-only**. Atlas has no code path that modifies an observed
system: no container restarts, no service reloads, no configuration writes.
That is the property that makes it safe to run on production hosts and safe to
give broad access to.

## Status

**Phase 0 (Architecture & Project Foundation) is complete.** The platform
foundation, domain contracts, HTTP surface, and delivery pipeline are built,
tested, and documented. Collectors begin in Phase 1 — see the
[phase plan](docs/roadmap/phases.md).

## Quick start

Requires Go 1.25+, Docker, and Node 22+.

```bash
make db-up      # start PostgreSQL + TimescaleDB, wait until it accepts queries
make run        # start Atlas — applies migrations, listens on 127.0.0.1:8080
```

```bash
curl -s localhost:8080/readyz                | jq
curl -s localhost:8080/api/v1/system/info    | jq
curl -s localhost:8080/api/v1/system/runtime | jq
```

For the console, in a second terminal:

```bash
make web-install
make web-dev    # http://localhost:5173
```

`make help` lists every target.

## What is built

| Layer | Contents |
| --- | --- |
| **Platform** | Typed errors with a redaction boundary · structured logging with context propagation and credential redaction · layered configuration validated at startup · non-blocking event bus · lifecycle supervisor · HTTP plumbing · health aggregation · PostgreSQL/TimescaleDB with a forward-only migrator |
| **Core** | `collect` (what an observation is) · `transport` (the seam that makes fleet monitoring a swap, not a rewrite) · `scheduler` (bounded, non-overlapping, panic-isolated collection) · `plugin` (detect, init, contribute, close) |
| **API** | Versioned `/api/v1`, uniform error envelope, orchestration probes outside versioning |
| **Frontend** | React 19 · TypeScript in strict mode · Vite · TanStack Query · a typed client mirroring the Go error model |
| **Delivery** | Makefile · 17 MB `scratch` container image · GitHub Actions running unit, integration, frontend, and image jobs |

## Guiding principles

**Never become a load source.** A monitoring system that degrades the host it
watches turns a minor incident into a major one. The scheduler enforces per-run
timeouts, non-overlapping runs, a concurrency ceiling, and start jitter; the
event bus drops rather than blocks. Each is a deliberate choice to lose some
observations rather than slow down the observed system.

**Fail loudly at startup, never silently at runtime.** Configuration is
validated before any component is constructed, and every violation is reported
at once. Atlas refuses to start against an unreachable database or a schema
older than it expects, because an Atlas that appears to be running while
discarding what it collects is worse than one that did not start.

**Internal detail cannot leak.** The client-safe and operator-only halves of an
error are separated in the error type itself, not left to each handler to
remember. Credential-shaped log keys are redacted before they reach the writer.

**Document decisions, not just code.** Ten [ADRs](docs/adr/README.md) record
what was chosen, what was rejected, and what each choice costs.

## Documentation

Start at [`docs/README.md`](docs/README.md).

| | |
| --- | --- |
| [System architecture](docs/architecture/system-architecture.md) | How the parts fit together |
| [ADRs](docs/adr/README.md) | Why it is built this way |
| [Developer guide](docs/development/developer-guide.md) | Setup and workflow |
| [API reference](docs/api/README.md) | Endpoints and the error envelope |
| [Configuration](docs/operations/configuration.md) | Every setting and its variable |
| [Deployment](docs/operations/deployment.md) | Container, Kubernetes, systemd |
| [Security](docs/security/security-guide.md) | Threat model and controls |
| [Plugin development](docs/plugins/plugin-development.md) | Adding a technology |
| [Phase plan](docs/roadmap/phases.md) | What is built and what is next |

## Security notice

**Phase 0 has no authentication.** Any client that can reach the port can read
every endpoint. The default bind address is `127.0.0.1` so that exposing Atlas
is a deliberate act. Until authentication ships in Tier 4, deploy behind an
authenticating reverse proxy or restrict access at the network layer. See the
[security guide](docs/security/security-guide.md).

## Testing

```bash
make test              # unit tests, race detector on
make test-integration  # against real PostgreSQL + TimescaleDB (needs `make db-up`)
make check             # what CI runs: format, vet, lint, test
```

The race detector is not optional here: the scheduler, event bus, and lifecycle
supervisor are all concurrent, and a data race in any of them corrupts the
observations the platform exists to produce. See the
[testing strategy](docs/development/testing.md).
