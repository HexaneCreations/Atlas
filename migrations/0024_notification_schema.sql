-- 0024_notification_schema.sql
--
-- Replaces 0009's webhook-only channels and delivery queue with the schema
-- for current-state notifications (internal/core/notification):
--
--   notification_channels   where to send: email or Teams today. type has no
--                           CHECK, so a new channel type (WhatsApp) needs no
--                           migration. Secrets are AES-256-GCM ciphertext
--                           (internal/platform/secretbox), bound to the
--                           channel id; the nonce is the ciphertext's first
--                           12 bytes, and secret_key_id names the key.
--   notification_rules      what to watch: one global default per trigger,
--                           plus per-node overrides whose NULL fields inherit
--                           the global value.
--   notification_state      the evaluator's per-(trigger, node, subject)
--                           condition state.
--   notification_deliveries one row per (event, channel), the dedupe and
--                           retry unit.
--
-- Nothing references the 0009 tables except notification_deliveries' own
-- foreign key to notification_channels, so dropping both is self-contained.
-- There are no production channel rows to keep.

DROP TABLE notification_deliveries;
DROP TABLE notification_channels;

CREATE TABLE notification_channels (
    id                text        PRIMARY KEY,
    name              text        NOT NULL UNIQUE,
    type              text        NOT NULL,
    enabled           boolean     NOT NULL DEFAULT true,
    min_severity      text        NOT NULL DEFAULT 'warning'
                                  CHECK (min_severity IN ('warning', 'critical')),
    -- Non-secret settings only (SMTP host/port/from/to, ...).
    config            jsonb       NOT NULL DEFAULT '{}'::jsonb
                                  CHECK (jsonb_typeof(config) = 'object'),
    secret_ciphertext bytea,
    secret_key_id     text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    CHECK ((secret_ciphertext IS NULL) = (secret_key_id IS NULL))
);

-- A global row (node_id IS NULL) is the default for its trigger and must be
-- complete; an override row (node_id set) may leave any field NULL to
-- inherit it. Thresholds only mean something for the percent triggers, so a
-- global node_down / container / service rule carries none.
CREATE TABLE notification_rules (
    id               text             PRIMARY KEY,
    trigger          text             NOT NULL,
    node_id          text             REFERENCES nodes (node_id) ON DELETE CASCADE,
    enabled          boolean,
    warn_threshold   double precision,
    crit_threshold   double precision,
    for_seconds      integer          CHECK (for_seconds >= 0),
    recovery_seconds integer          CHECK (recovery_seconds >= 0),
    hysteresis       double precision CHECK (hysteresis >= 0),
    cooldown_seconds integer          CHECK (cooldown_seconds >= 0),
    send_recovery    boolean,
    -- Mountpoints / container names / unit names to ignore.
    subject_excludes text[],
    updated_at       timestamptz      NOT NULL DEFAULT now(),
    updated_by       text             NOT NULL DEFAULT '',
    CHECK (warn_threshold IS NULL OR crit_threshold IS NULL OR warn_threshold <= crit_threshold),
    CONSTRAINT notification_rules_global_complete CHECK (
        node_id IS NOT NULL OR (
            enabled IS NOT NULL AND for_seconds IS NOT NULL AND recovery_seconds IS NOT NULL
            AND hysteresis IS NOT NULL AND cooldown_seconds IS NOT NULL AND send_recovery IS NOT NULL
            AND subject_excludes IS NOT NULL
            AND (trigger NOT IN ('disk_pct', 'ram_pct')
                 OR (warn_threshold IS NOT NULL AND crit_threshold IS NOT NULL))
        )
    )
);

CREATE UNIQUE INDEX ux_notification_rules_scope ON notification_rules (trigger, (COALESCE(node_id, '')));

INSERT INTO notification_rules
    (id, trigger, node_id, enabled, warn_threshold, crit_threshold, for_seconds, recovery_seconds,
     hysteresis, cooldown_seconds, send_recovery, subject_excludes, updated_by)
VALUES
    ('global-disk_pct',          'disk_pct',          NULL, true, 80,   90,   300, 120, 5, 1800, true, '{}', 'migration'),
    ('global-ram_pct',           'ram_pct',           NULL, true, 85,   95,   300, 120, 5, 1800, true, '{}', 'migration'),
    -- node_down for_seconds is 480, not 300 like the others: a rebooting or
    -- briefly unreachable host must not page anyone, so the hold has to cover
    -- a boot plus a missed collection window, not just one missed collection.
    ('global-node_down',         'node_down',         NULL, true, NULL, NULL, 480, 120, 0, 1800, true, '{}', 'migration'),
    ('global-service_failed',    'service_failed',    NULL, true, NULL, NULL, 300, 120, 0, 1800, true, '{}', 'migration'),
    -- Events, not conditions: they fire on the observed change itself.
    ('global-container_exit',    'container_exit',    NULL, true, NULL, NULL, 0,   120, 0, 1800, true, '{}', 'migration'),
    ('global-container_restart', 'container_restart', NULL, true, NULL, NULL, 0,   120, 0, 1800, true, '{}', 'migration');

CREATE TABLE notification_state (
    trigger            text             NOT NULL,
    node_id            text             NOT NULL REFERENCES nodes (node_id) ON DELETE CASCADE,
    -- '' for node-level triggers, else the mountpoint / container name / unit.
    subject            text             NOT NULL DEFAULT '',
    state              text             NOT NULL CHECK (state IN ('ok', 'pending', 'firing', 'recovering')),
    severity           text             NOT NULL DEFAULT '' CHECK (severity IN ('', 'warning', 'critical')),
    value              double precision,
    condition_since    timestamptz,
    state_since        timestamptz      NOT NULL,
    last_notified_at   timestamptz,
    last_notified_kind text             NOT NULL DEFAULT '',
    flap_count         integer          NOT NULL DEFAULT 0 CHECK (flap_count >= 0),
    flap_window_start  timestamptz,
    -- Trigger-specific memory, e.g. {"restart_count":3,"container_id":"..."}.
    baseline           jsonb            NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(baseline) = 'object'),
    updated_at         timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (trigger, node_id, subject)
);

CREATE INDEX idx_notification_state_active ON notification_state (state) WHERE state <> 'ok';

CREATE TABLE notification_deliveries (
    id              text        PRIMARY KEY,
    -- The state-transition id: one notification per (event, channel).
    event_id        text        NOT NULL,
    channel_id      text        NOT NULL REFERENCES notification_channels (id) ON DELETE CASCADE,
    channel_type    text        NOT NULL,
    kind            text        NOT NULL CHECK (kind IN ('firing', 'escalated', 'recovered', 'flapping', 'test')),
    trigger         text        NOT NULL,
    node_id         text        NOT NULL DEFAULT '',
    subject         text        NOT NULL DEFAULT '',
    severity        text        NOT NULL DEFAULT '' CHECK (severity IN ('', 'warning', 'critical')),
    -- Rendered text, never a secret.
    title           text        NOT NULL,
    message         text        NOT NULL,
    status          text        NOT NULL CHECK (status IN ('pending', 'delivered', 'failed')),
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL,
    -- Redacted and truncated by the repository before it is written.
    last_error      text        NOT NULL DEFAULT '' CHECK (length(last_error) <= 512),
    delivered_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, channel_id)
);

CREATE INDEX idx_notification_deliveries_due ON notification_deliveries (next_attempt_at) WHERE status = 'pending';
-- Keyset pagination of the delivery log, newest first.
CREATE INDEX idx_notification_deliveries_created ON notification_deliveries (created_at DESC, id DESC);

-- An archived node is retired from monitoring without deleting its history
-- (metrics, events, incidents, grants): the evaluator must skip it, the same
-- way it already has no reason to watch a node nobody enrolled. No API or UI
-- sets this in step 6 — only the column exists, for the evaluator to read
-- once it is written.
ALTER TABLE nodes ADD COLUMN archived_at timestamptz;

-- No partial index on (archived_at) WHERE archived_at IS NULL here. The
-- evaluator that would run this predicate does not exist yet (step 7+), so
-- there is no real query shape to optimize for — it may well filter by
-- archived_at alongside something else (environment, a join on
-- notification_rules), which would make a single-column partial index on
-- archived_at the wrong index anyway. nodes is one row per host, not per
-- sample like metric_samples/events — a sequential scan over it costs
-- nothing at any fleet size this product targets. Add the index once the
-- evaluator's actual WHERE clause is known, in the same change that writes it.
