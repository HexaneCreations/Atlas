# Developer Guide

## Prerequisites

| Tool | Version | Notes |
| --- | --- | --- |
| Go | 1.25+ | The `go` directive pins 1.25; the toolchain downloads it if needed. |
| Docker | any recent | Runs the development database. |
| Node.js | 22+ | Frontend only. |
| `golangci-lint` | latest | Optional locally; required in CI. |

## First run

```bash
git clone <repository> && cd Atlas

make db-up          # start TimescaleDB and wait for it to accept queries
make run            # start Atlas against it
```

`make run` supplies the development database password and disables TLS to the
local container. Atlas applies migrations on startup, then listens on
`127.0.0.1:8080`.

Verify:

```bash
curl -s localhost:8080/readyz | jq
curl -s localhost:8080/api/v1/system/info | jq
```

For the frontend, in a second terminal:

```bash
make web-install
make web-dev        # http://localhost:5173, proxying /api to :8080
```

The dev server proxies API traffic rather than enabling CORS, so development
exercises the same same-origin path as production. Relaxing CORS to make
development work is a change that has a habit of surviving into deployment.

## Everyday commands

```bash
make help              # every target, with descriptions

make test              # unit tests with the race detector
make test-integration  # integration tests (needs `make db-up`)
make check             # what CI runs: fmt-check, vet, lint, test
make cover             # coverage report at coverage.html

make build             # bin/atlas-server, version-stamped
make migrate           # apply pending migrations without starting the server
make db-shell          # psql on the development database
make db-reset          # destroy the database and its data, then recreate

make web-check         # frontend typecheck and lint
make web-build         # production frontend bundle
```

## Repository layout

```
cmd/atlas-server/        entry point: flags, subcommands
internal/
  app/                   composition root — the whole dependency graph
  api/                   HTTP surface
    v1/                  version 1 handlers
  core/                  domain contracts
    collect/             Sample, Batch, Collector, Registry
    transport/           Envelope, Transport, Sink, InProcess
    scheduler/           runs collectors safely
    plugin/              the extension contract and registry
  platform/              technical capability, domain-agnostic
    build/ config/ errs/ eventbus/ health/ httpx/ id/ lifecycle/ log/ postgres/
migrations/              embedded SQL, forward-only
web/                     React + TypeScript frontend
docs/                    this documentation
```

The layering rule — `platform` imports nothing above it, `core` does not import
`api` — is described in [backend architecture](../architecture/backend-architecture.md)
and is a review checkpoint.

## Where to start reading

For an accurate mental model in about thirty minutes, in this order:

1. `internal/app/app.go` — the entire dependency graph and startup order.
2. `internal/platform/lifecycle/lifecycle.go` — how components start and stop.
3. `internal/core/collect/collect.go` — what an observation is.
4. `internal/core/transport/transport.go` — the seam that makes Tier 4 cheap.
5. `internal/platform/errs/errs.go` — how errors are classified and redacted.

Package doc comments carry the rationale. Where a decision was significant
enough to have alternatives worth recording, the comment points at an
[ADR](../adr/README.md).

## Making a change

### Adding an endpoint

1. Add a handler to `internal/api/v1/`. Return errors; do not write them —
   `httpx.Handler` renders, logs, and assigns the status.
2. Register it in `Mount` with an explicit method: `mux.Handle("GET "+Prefix+"/path", …)`.
3. Add a test in `internal/api/router_test.go`.
4. Document it in [the API reference](../api/README.md).

### Adding a configuration setting

1. Add the field to the relevant struct in `internal/platform/config/config.go`
   with `yaml`, `json`, and `env` tags.
2. Add its default to `Default()`.
3. Add validation in `validate.go`, including any production-only rule.
4. Add a row to [the configuration guide](../operations/configuration.md).

The environment variable name is derived from the tags, so nothing else is
needed to make it settable.

### Adding a database migration

1. Create `migrations/NNNN_lower_snake_case.sql`.
2. Make it **backward-compatible with the previous release** — see
   [ADR-0007](../adr/0007-forward-only-migrations.md). There are no down
   migrations.
3. Apply it with `make migrate`.
4. Update [the schema reference](../database/schema.md).

**Never edit an applied migration.** The migrator stores each file's checksum
and refuses to start if one changes.

### Adding a plugin

See the [plugin development guide](../plugins/plugin-development.md).

## Before opening a pull request

```bash
make check
make test-integration
make web-check      # if the frontend changed
```

A pull request that changes behaviour without updating the corresponding
document is incomplete, in the same way one without tests is incomplete.

## Troubleshooting the development environment

**`make db-up` times out.** Check `docker compose logs postgres`. The usual
cause is a port 5432 conflict with a locally installed Postgres.

**Atlas exits with "database is unreachable".** The container is not up, or
`ATLAS_DATABASE_SSL_MODE` is not `disable` for the local container.

**"migrations are pending and migrate_on_start is disabled".** Run
`make migrate`.

**"migration ... has changed since it was applied".** An applied migration was
edited. In development, `make db-reset`. Never in production — write a new
migration instead.

More in the [troubleshooting guide](../operations/troubleshooting.md).
