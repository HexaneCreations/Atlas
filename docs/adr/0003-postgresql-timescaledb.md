# ADR-0003: PostgreSQL with TimescaleDB as the single datastore

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

Atlas stores two kinds of data with genuinely different shapes:

- **Time series.** Metric samples at a fifteen-second cadence per collector per
  host, and observed events. High write volume, append-only, queried by time
  range, and needing retention and downsampling.
- **Relational state.** Service catalog, ownership and team metadata,
  dependency edges, deployment history, runbooks, incidents, alert rules, RBAC.
  Modest volume, heavily interrelated, needing joins and referential integrity.

Tiers 2 and 3 require these to be queried *together*: "show CPU saturation for
every service owned by the payments team during last Tuesday's incident" joins
time-series data to catalog data to incident data in one question.

## Decision

**PostgreSQL with the TimescaleDB extension, as the only datastore.**

Time series live in hypertables — transparently time-partitioned tables —
with continuous aggregates for rollups, native compression, and retention
policies. Relational state lives in ordinary tables in the same database.

Migration `0001_extensions.sql` installs `timescaledb` and `pgcrypto`, so a
database that cannot support Atlas fails during migration rather than at the
first hypertable creation.

## Alternatives considered

**PostgreSQL for state, Prometheus for metrics.** The industry-standard split.
PromQL is genuinely excellent for the Tier 3 golden-signals and SLO work, and
the ecosystem is enormous. Rejected on three counts. First, it means two
datastores, two query languages, two retention models, and two backup
procedures — permanently. Second, Prometheus is pull-based, which fights the
event-driven requirement and the agent-push topology of Tier 4. Third, and
decisively, joining metrics to catalog data across two systems has to happen in
application code: the cross-store query above becomes fetch-then-correlate in
Go, which is slower, more code, and cannot be expressed as a single view.

**PostgreSQL alone, without TimescaleDB.** One fewer dependency, and plain
Postgres handles more time-series volume than people expect. Rejected because
Atlas would then have to reimplement what the extension provides: partition
management, downsampling jobs, retention enforcement, and compression. That is
a large amount of infrastructure code to own, in the part of the system where
correctness is least visible until data is already lost.

**SQLite by default, PostgreSQL optionally.** A zero-dependency single-binary
install is a genuinely attractive story, and the demo experience would be
excellent. Rejected because it means maintaining two SQL dialects and two
migration paths for the life of the project, and because SQLite's single-writer
model is a poor fit for concurrent metric ingest. The cost is paid on every
schema change, forever, to save a one-time setup step.

**ClickHouse for metrics.** Outstanding at analytical scale over time series.
Rejected as disproportionate: Atlas's volume does not warrant it, and it
reintroduces the two-store split with a less familiar operational profile.

## Consequences

**Good.**

- One connection pool, one migration path, one backup, one set of credentials,
  one thing to monitor.
- Cross-domain queries are ordinary SQL joins. This is the property that makes
  Tiers 2 and 3 tractable.
- Transactional consistency between a metric and the catalog row it refers to.
- Continuous aggregates give Atlas its rollups without a rollup service.
- Postgres operational knowledge is common; most teams already have it.

**Costs.**

- TimescaleDB is a hard dependency. The database image must carry the
  extension, which rules out a plain managed Postgres instance that does not
  offer it. Documented prominently in the
  [deployment guide](../operations/deployment.md).
- TimescaleDB's licensing differs from PostgreSQL's; the Community Edition
  covers Atlas's needs, but this must be reviewed before any redistribution.
- Postgres is a hard startup dependency. Atlas refuses to start without it,
  deliberately: an Atlas that started without a database would accept requests
  it cannot answer and silently discard the samples it collects, which is worse
  than not starting because monitoring would appear to be running.
- No PromQL. The Tier 3 query layer is built on SQL, which is more work.

**Revisit if** metric volume outgrows a single Postgres instance, or if
integration with an existing organisational Prometheus becomes a requirement
rather than a preference.
