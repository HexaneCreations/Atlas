# Runbooks

Procedures for operating Atlas itself.

> Runbooks *attached to monitored services* — the Tier 2 feature where a
> service in the catalog carries its own troubleshooting and recovery
> documentation — are a Phase 3 item. These are runbooks **for Atlas**.

Each is written to be followed under pressure by someone who did not write it:
a stated trigger, numbered steps, and a verification at the end.

---

## Restart Atlas

**Trigger:** unresponsive process, or a configuration change that requires a
restart.

1. Confirm what is running: `curl -s <atlas>/api/v1/system/info | jq`
2. Send `SIGTERM` (`systemctl restart atlas`, or a pod delete). **Do not
   `SIGKILL`** — Atlas drains in-flight requests and closes its pool in order.
3. Watch the shutdown sequence in the logs; `stopping component` lines name how
   far it got.
4. Verify: `curl -sf <atlas>/readyz | jq .status` returns `healthy`.

**If shutdown exceeds its budget**, the log reports
`shutdown deadline exceeded with N component(s) unstopped`. Raise
`server.shutdown_timeout`, and make sure the process manager allows more than
it — `TimeoutStopSec=45`, or a matching `terminationGracePeriodSeconds`.

---

## Apply a schema migration

**Trigger:** deploying a release containing a new migration.

1. **Back up first.** Migrations are forward-only; there is no down path.
   ```bash
   pg_dump --format=custom --compress=9 -h <host> -U atlas atlas > pre-migrate.dump
   ```
2. Check what is pending, using the new image:
   ```bash
   docker run --rm --env-file atlas.env atlas:<new> migrate
   ```
3. Confirm the ledger:
   ```sql
   SELECT version, name, applied_at, execution_ms
   FROM atlas_schema_migrations ORDER BY version DESC LIMIT 5;
   ```
4. Roll out the application.

**If a migration fails:** the transaction rolled back and nothing was recorded,
so the database is not partially migrated. Fix the migration, release a new
version, and re-run. **Never edit the failed migration if it has already
succeeded anywhere** — the checksum will reject it.

---

## Roll back a release

**Trigger:** a deployed version is faulty.

1. **Deploy the previous application version.** That is the whole procedure.
2. **Do not attempt to reverse the migration.** Every migration is required to
   be backward-compatible with the release before it, so the previous version
   runs correctly against the newer schema. See
   [ADR-0007](../../adr/0007-forward-only-migrations.md).
3. Verify: `/readyz` is healthy and `/api/v1/system/info` reports the expected
   version.

**If the newer schema genuinely broke the older version**, the migration was
not backward-compatible — a defect in that migration. Restore from backup and
treat it as an incident.

---

## Rotate the database password

**Trigger:** scheduled rotation, or suspected exposure.

1. Create the new credential in Postgres:
   ```sql
   ALTER ROLE atlas WITH PASSWORD '<new>';
   ```
2. Update the mounted secret file that `ATLAS_DATABASE_PASSWORD_FILE` points at.
3. Restart Atlas — configuration is read once at startup and is immutable
   thereafter, so a restart is required.
4. Verify `/readyz` is healthy.

With more than one instance, restart them one at a time and confirm each is
ready before the next. The old password stops working the moment step 1 runs,
so keep the gap between steps 1 and 3 short.

---

## Respond to dropped events

**Trigger:** `event_bus.dropped` on `/api/v1/system/runtime` is non-zero and
rising.

1. Find the subscriber. The log names it:
   ```
   msg="event dropped: subscriber queue is full" subscriber="api.websocket-fanout"
   pattern="**" dropped_total=1423 buffer_size=256
   ```
2. Decide which of three applies:
   - **A slow consumer** — the real fix. Profile and make it faster.
   - **Too broad a subscription** — narrow the pattern so it receives less.
   - **A genuinely bursty source** — raise `event_bus.buffer_size` and restart.
3. Verify the counter stops rising.

Dropping is by design and is not itself a fault
([ADR-0008](../../adr/0008-lossy-event-bus.md)). It means one consumer is
behind — never that a publisher is blocked.

---

## Respond to pool exhaustion

**Trigger:** `database.empty_acquire_count` rising; slow responses.

1. Check saturation on `/api/v1/system/runtime`: compare `acquired_conns` with
   `max_conns`.
2. Look for `slow query` log lines — a single expensive query holding
   connections is a more common cause than genuine load.
3. Check the server side:
   ```sql
   SELECT count(*), state FROM pg_stat_activity
   WHERE application_name = 'atlas' GROUP BY state;
   ```
4. Raise `database.max_conns` and restart — but first confirm Postgres's own
   `max_connections` can accommodate the new total across every Atlas instance.

---

## Investigate a failing request

**Trigger:** a user reports an error.

1. Get the `request_id` from the response body. Every error carries one.
2. Search the logs for it. The full cause is there — the response deliberately
   omits it, because it may contain connection strings or credentials.
   ```bash
   grep '<request_id>' /var/log/atlas.log | jq
   ```
3. The log line carries the error code, status, method, path, and the wrapped
   cause chain with its `Op` path.

---

## Recover from a corrupt migration ledger

**Trigger:** `migration ... has changed since it was applied`.

1. **Do not "fix" it by editing the ledger.** Establish which is wrong: the
   file or the database.
2. Compare the checksum:
   ```sql
   SELECT version, name, checksum FROM atlas_schema_migrations WHERE version = <n>;
   ```
   ```bash
   shasum -a 256 migrations/<file>.sql
   ```
3. Almost always the file was edited after being applied. **Revert the file to
   its original content** and put the intended change in a new migration.
4. If the file is genuinely correct and the ledger is wrong — for example, a
   restore from an inconsistent backup — treat it as an incident and reconcile
   against a known-good database before touching the ledger.

---

## Writing a new runbook

- State the **trigger** first: the observable condition, not the diagnosis.
- Numbered steps, each a single action.
- Name the exact command, endpoint, or query.
- End with a **verification** step.
- Say what to do when a step fails.
- Assume the reader is tired, on-call, and has not read the architecture docs.
