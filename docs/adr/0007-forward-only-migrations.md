# ADR-0007: Forward-only database migrations

- **Status:** Accepted
- **Date:** 2026-08-06
- **Phase:** 0

## Context

Atlas needs schema evolution across a long-lived product, applied by
deployments that may start several instances simultaneously during a rolling
update.

The conventional approach pairs each migration with a `down` file that reverses
it, on the theory that a bad deploy can be rolled back by running the down
migrations.

## Decision

**Migrations are forward-only.** There are no `down` files.

Mechanics:

- Files are `migrations/NNNN_lower_snake_case.sql`, embedded in the binary.
- The whole run is serialised by a session-level PostgreSQL advisory lock, so
  concurrent instances cannot apply the same migration twice.
- Each migration runs in its own transaction, together with the insert that
  records it — so a migration is either fully applied and recorded, or neither.
- Every applied migration's SHA-256 is stored and re-verified on each run.
  Editing an already-applied migration is a startup failure.

**Rollback is performed by deploying the previous application version against
the newer schema.** This requires every migration to be backward-compatible
with the release immediately before it: add columns as nullable or with
defaults, never rename in one step, and remove a column only after the release
that stopped using it has shipped.

## Alternatives considered

**Paired up and down migrations.** The default in most frameworks. Rejected for
three reasons. Down migrations are written once and almost never executed,
so they are almost never correct — they rot silently and are discovered to be
broken at the exact moment they are needed. More seriously, a down migration
that drops a column destroys data the forward path cannot recreate, so
"rolling back" a deploy can lose production data irreversibly. And the safety
they appear to offer encourages migrations that are *not* backward-compatible,
because the author believes there is an escape hatch — which is precisely the
habit this decision is trying to prevent.

**A third-party migration library (`golang-migrate`, `goose`).** Mature and
well understood. Rejected in favour of a small in-house migrator because the
requirements here are specific — advisory locking, checksum verification,
forward-only, embedded, and errors phrased in Atlas's own typed error kernel —
and the implementation is about 250 lines that Atlas fully controls and fully
tests. The libraries' extra features are ones this decision explicitly does not
want.

**Auto-migration from struct definitions (ORM style).** Rejected: it hides what
is actually executed against the database, cannot express data backfills or
index strategies, and makes review of the highest-risk change in any deploy
impossible.

## Consequences

**Good.**

- The rollback path is exercised on every deploy, because it is just "run the
  previous version". A down migration's path is exercised approximately never.
- No possibility of a rollback destroying data.
- Checksum verification catches the single most damaging thing a team can do to
  a shared schema — editing an applied migration, so the author's database
  matches the new file while everyone else's matches the old one, with nothing
  reporting a difference until a query fails in production.
- The advisory lock makes rolling deploys safe rather than merely unlikely to
  collide. This is tested: five concurrent migrators against one database apply
  each migration exactly once.
- Migrations ship inside the binary, so a deployed `atlas-server` can never be
  paired with the wrong migration set.

**Costs.**

- **Every migration must be backward-compatible with the previous release.**
  This is a real, permanent constraint on how schema changes are written, and
  it makes some changes take two or three deploys instead of one. Renaming a
  column becomes: add the new one, write to both, backfill, switch reads,
  then drop the old one in a later release.
- A genuinely bad migration requires a hand-written corrective migration under
  pressure. Mitigated by the transaction-per-migration guarantee, which means
  the database is never left in a partially migrated state.
- Restoring from backup remains the last resort for a data-destroying mistake,
  which makes tested backups a hard operational requirement rather than a
  nice-to-have. See the [deployment guide](../operations/deployment.md).

**Revisit** — unlikely. This decision becomes more correct as the deployment
grows, not less.
