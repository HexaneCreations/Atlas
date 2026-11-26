-- 0025_website_monitoring.sql
--
-- Website monitoring (internal/core/monitor): HTTP(S) monitors probed from
-- atlas-server, their live status, heartbeat history and up/down events.
--
--   monitor_groups       named, ordered groups for the Websites list.
--   monitors             what to probe. Custom headers, basic/bearer auth and
--                        the request body are one AES-256-GCM secret
--                        (internal/platform/secretbox) bound to the monitor
--                        id, exactly like notification channel secrets;
--                        header_names keeps only the header names.
--   monitor_status       one row per monitor: the prober's counters and the
--                        next due time, so a restart neither forgets a
--                        failure streak nor flips a down monitor to up.
--   monitor_heartbeats   one row per check (hypertable). Never a response
--                        body, never error text.
--   monitor_heartbeats_1h hourly roll-up kept for a year.
--   monitor_events       down/up transitions for the events list.
--   monitor_channels     which notification channels a monitor notifies.
--
-- It also prepares the notification tables for a subject that is not a node
-- (notification_state.monitor_id, notification_deliveries.monitor_id). Both
-- changes keep the previous release working against this schema
-- (docs/adr/0007-forward-only-migrations.md):
--
--   * notification_state keeps a plain unique index on exactly
--     (trigger, node_id, subject), so the previous release's
--     ON CONFLICT (trigger, node_id, subject) still infers it. Node rows are
--     not rewritten: dropping the primary key and NOT NULL are catalog
--     changes, and the new column is NULL for every existing row.
--   * notification_deliveries.monitor_id has a constant default, so adding
--     it is a catalog-only change and old INSERTs that omit it still work.
--
-- notification_rules is deliberately untouched here. Its scope index is the
-- expression (trigger, COALESCE(node_id, '')) that the previous release's
-- ON CONFLICT names; widening it for monitor overrides would break that
-- release's rule writes, so it changes in 13b's migration instead.
--
-- Written to be re-runnable: every object is created only if missing.
-- lock_timeout makes a busy table fail the migration fast (it then retries
-- on the next start) instead of queueing every other writer behind it.

SET LOCAL lock_timeout = '5s';

-- ------------------------------------------------------------- monitors ----

CREATE TABLE IF NOT EXISTS monitor_groups (
    id         text        PRIMARY KEY,
    name       text        NOT NULL UNIQUE CHECK (length(name) BETWEEN 1 AND 100),
    position   integer     NOT NULL DEFAULT 0,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS monitors (
    id                     text        PRIMARY KEY,
    name                   text        NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    type                   text        NOT NULL DEFAULT 'http' CHECK (type IN ('http')),
    url                    text        NOT NULL CHECK (length(url) BETWEEN 1 AND 2048),
    method                 text        NOT NULL DEFAULT 'GET' CHECK (method IN ('GET', 'HEAD', 'POST')),
    interval_seconds       integer     NOT NULL DEFAULT 60 CHECK (interval_seconds BETWEEN 20 AND 86400),
    retries                integer     NOT NULL DEFAULT 3 CHECK (retries BETWEEN 0 AND 10),
    retry_interval_seconds integer     NOT NULL DEFAULT 60 CHECK (retry_interval_seconds BETWEEN 20 AND 86400),
    timeout_seconds        integer     NOT NULL DEFAULT 30 CHECK (timeout_seconds BETWEEN 1 AND 120),
    max_redirects          integer     NOT NULL DEFAULT 10 CHECK (max_redirects BETWEEN 0 AND 10),
    -- "200-299", "301": validated in Go (internal/core/monitor.ParseStatusCodes).
    accepted_status        text[]      NOT NULL DEFAULT '{200-299}',
    body_encoding          text        NOT NULL DEFAULT 'json' CHECK (body_encoding IN ('json', 'form', 'text')),
    keyword                text        CHECK (keyword IS NULL OR length(keyword) BETWEEN 1 AND 500),
    recovery_successes     integer     NOT NULL DEFAULT 1 CHECK (recovery_successes BETWEEN 1 AND 10),
    cert_notify            boolean     NOT NULL DEFAULT true,
    cert_warn_days         integer     NOT NULL DEFAULT 14 CHECK (cert_warn_days BETWEEN 1 AND 365),
    cert_crit_days         integer     NOT NULL DEFAULT 3 CHECK (cert_crit_days BETWEEN 0 AND 365),
    group_id               text        REFERENCES monitor_groups (id) ON DELETE SET NULL,
    description            text        NOT NULL DEFAULT '' CHECK (length(description) <= 1000),
    tags                   text[]      NOT NULL DEFAULT '{}',
    active                 boolean     NOT NULL DEFAULT true,
    alerts_muted_until     timestamptz,
    auth_type              text        NOT NULL DEFAULT 'none' CHECK (auth_type IN ('none', 'basic', 'bearer')),
    -- Names only; the values live in the sealed secret.
    header_names           text[]      NOT NULL DEFAULT '{}',
    secret_ciphertext      bytea,
    secret_key_id          text,
    created_at             timestamptz NOT NULL DEFAULT now(),
    created_by             text        NOT NULL DEFAULT '',
    updated_at             timestamptz NOT NULL DEFAULT now(),
    updated_by             text        NOT NULL DEFAULT '',
    CHECK ((secret_ciphertext IS NULL) = (secret_key_id IS NULL)),
    CHECK (cert_crit_days < cert_warn_days),
    CHECK (timeout_seconds < interval_seconds)
);

CREATE INDEX IF NOT EXISTS idx_monitors_group ON monitors (group_id);
CREATE INDEX IF NOT EXISTS idx_monitors_tags ON monitors USING gin (tags);

CREATE TABLE IF NOT EXISTS monitor_status (
    monitor_id            text        PRIMARY KEY REFERENCES monitors (id) ON DELETE CASCADE,
    -- unknown: not checked yet (new, or just re-activated).
    status                text        NOT NULL DEFAULT 'unknown'
                                      CHECK (status IN ('unknown', 'up', 'pending', 'down')),
    consecutive_failures  integer     NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    consecutive_successes integer     NOT NULL DEFAULT 0 CHECK (consecutive_successes >= 0),
    next_check_at         timestamptz NOT NULL,
    last_check_at         timestamptz,
    last_status_code      integer,
    last_response_ms      integer,
    last_failure          text        NOT NULL DEFAULT '',
    status_since          timestamptz NOT NULL DEFAULT now(),
    down_since            timestamptz,
    cert_expires_at       timestamptz,
    updated_at            timestamptz NOT NULL DEFAULT now()
);

-- The prober's claim query: due, oldest first.
CREATE INDEX IF NOT EXISTS idx_monitor_status_due ON monitor_status (next_check_at);

-- No foreign key to monitors: cascading a delete into compressed chunks is
-- slow and version-sensitive. The repository deletes a monitor's heartbeats
-- in the same transaction as the monitor, and retention removes the rest.
CREATE TABLE IF NOT EXISTS monitor_heartbeats (
    monitor_id  text        NOT NULL,
    time        timestamptz NOT NULL,
    -- unknown: the probe itself had no connectivity; excluded from uptime.
    status      text        NOT NULL CHECK (status IN ('up', 'pending', 'down', 'unknown')),
    status_code smallint,
    response_ms integer,
    failure     text        NOT NULL DEFAULT ''
                            CHECK (failure IN ('', 'dns', 'connect', 'tls', 'timeout', 'http_status',
                                               'keyword', 'redirect', 'blocked'))
);

-- Timescale-only (see the report): hypertable, compression, retention and
-- the hourly continuous aggregate. Every query the repository runs against
-- these is plain SQL.
SELECT create_hypertable('monitor_heartbeats', by_range('time', INTERVAL '1 day'), if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS idx_monitor_heartbeats_monitor_time ON monitor_heartbeats (monitor_id, time DESC);

ALTER TABLE monitor_heartbeats SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'monitor_id',
    timescaledb.compress_orderby   = 'time DESC'
);
SELECT add_compression_policy('monitor_heartbeats', INTERVAL '2 days', if_not_exists => TRUE);
SELECT add_retention_policy('monitor_heartbeats', INTERVAL '30 days', if_not_exists => TRUE);

CREATE MATERIALIZED VIEW IF NOT EXISTS monitor_heartbeats_1h
    WITH (timescaledb.continuous, timescaledb.materialized_only = false) AS
SELECT
    time_bucket(INTERVAL '1 hour', time)            AS bucket,
    monitor_id,
    count(*) FILTER (WHERE status = 'up')           AS up_count,
    count(*) FILTER (WHERE status = 'pending')      AS pending_count,
    count(*) FILTER (WHERE status = 'down')         AS down_count,
    count(*) FILTER (WHERE status = 'unknown')      AS unknown_count,
    avg(response_ms)                                AS avg_response_ms,
    max(response_ms)                                AS max_response_ms
FROM monitor_heartbeats
GROUP BY bucket, monitor_id
WITH NO DATA;

SELECT add_continuous_aggregate_policy('monitor_heartbeats_1h',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval => INTERVAL '30 minutes',
    if_not_exists     => TRUE);
SELECT add_retention_policy('monitor_heartbeats_1h', INTERVAL '365 days', if_not_exists => TRUE);

CREATE TABLE IF NOT EXISTS monitor_events (
    id          text        PRIMARY KEY,
    monitor_id  text        NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    time        timestamptz NOT NULL,
    kind        text        NOT NULL CHECK (kind IN ('down', 'up')),
    status_code integer,
    failure     text        NOT NULL DEFAULT '',
    -- For 'up': how long the monitor had been down.
    duration_seconds integer
);

-- Newest first per monitor; also serves the daily prune of rows older than
-- a year.
CREATE INDEX IF NOT EXISTS idx_monitor_events_monitor_time ON monitor_events (monitor_id, time DESC);
CREATE INDEX IF NOT EXISTS idx_monitor_events_time ON monitor_events (time);

CREATE TABLE IF NOT EXISTS monitor_channels (
    monitor_id text NOT NULL REFERENCES monitors (id) ON DELETE CASCADE,
    channel_id text NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    PRIMARY KEY (monitor_id, channel_id)
);

-- Pre-selected for a new monitor (13b).
ALTER TABLE notification_channels ADD COLUMN IF NOT EXISTS monitor_default boolean NOT NULL DEFAULT false;

-- ------------------------------------------------ notification subjects ----

ALTER TABLE notification_state ADD COLUMN IF NOT EXISTS monitor_id text REFERENCES monitors (id) ON DELETE CASCADE;

-- Built before the primary key is dropped, so (trigger, node_id, subject) is
-- never without a unique index.
CREATE UNIQUE INDEX IF NOT EXISTS ux_notification_state_node ON notification_state (trigger, node_id, subject);
CREATE UNIQUE INDEX IF NOT EXISTS ux_notification_state_monitor ON notification_state (trigger, monitor_id, subject)
    WHERE monitor_id IS NOT NULL;
ALTER TABLE notification_state DROP CONSTRAINT IF EXISTS notification_state_pkey;
ALTER TABLE notification_state ALTER COLUMN node_id DROP NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'notification_state_one_subject') THEN
        ALTER TABLE notification_state ADD CONSTRAINT notification_state_one_subject
            CHECK (num_nonnulls(node_id, monitor_id) = 1);
    END IF;
END
$$;

ALTER TABLE notification_deliveries ADD COLUMN IF NOT EXISTS monitor_id text NOT NULL DEFAULT '';

-- ---------------------------------------------------------------- pages ----

INSERT INTO pages (key, fleet_only) VALUES ('websites', true) ON CONFLICT (key) DO NOTHING;

-- Existing admins keep seeing every page, as 0016 arranged for the rest.
INSERT INTO role_access_pages (role_access_name, page_key)
SELECT 'full-access', 'websites'
WHERE EXISTS (SELECT 1 FROM role_access WHERE name = 'full-access')
ON CONFLICT DO NOTHING;
