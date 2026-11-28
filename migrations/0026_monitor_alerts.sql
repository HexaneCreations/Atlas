-- 0026_monitor_alerts.sql
--
-- Website monitor alerts (13b): monitors become alert subjects next to
-- nodes.
--
--   notification_rules      gains monitor_id, a per-monitor override scope
--                           beside node_id. The scope index widens to
--                           (trigger, node_id, monitor_id), and the two new
--                           triggers get their global rows.
--   notification_deliveries gains details: the facts a monitor
--                           notification renders (failure, status code,
--                           response time, outage, certificate expiry).
--                           Never a name, URL or credential; those are
--                           resolved when the delivery is sent.
--   monitor_events          gains the certificate transitions and the
--                           expiry they were recorded with.
--
-- Node-scoped rows are not rewritten: every ALTER here is a catalog change
-- or a constraint check, and the new columns are NULL (or a constant
-- default) for every existing row.
--
-- The previous release (v1.1.5) cannot write rules against this schema: its
-- ON CONFLICT names the old (trigger, COALESCE(node_id, '')) index, which a
-- second per-monitor override of one trigger would violate, so it is
-- replaced. See the 13b report for what a rollback to v1.1.5 requires.
--
-- Re-runnable: every object is created only if missing. lock_timeout makes
-- a busy table fail the migration fast instead of queueing other writers.

SET LOCAL lock_timeout = '5s';

-- ------------------------------------------------------ rule scopes ----

ALTER TABLE notification_rules ADD COLUMN IF NOT EXISTS monitor_id text REFERENCES monitors (id) ON DELETE CASCADE;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'notification_rules_one_scope') THEN
        ALTER TABLE notification_rules ADD CONSTRAINT notification_rules_one_scope
            CHECK (num_nonnulls(node_id, monitor_id) <= 1);
    END IF;
END
$$;

-- A monitor override, like a node override, may leave any field NULL.
ALTER TABLE notification_rules DROP CONSTRAINT IF EXISTS notification_rules_global_complete;
ALTER TABLE notification_rules ADD CONSTRAINT notification_rules_global_complete CHECK (
    node_id IS NOT NULL OR monitor_id IS NOT NULL OR (
        enabled IS NOT NULL AND for_seconds IS NOT NULL AND recovery_seconds IS NOT NULL
        AND hysteresis IS NOT NULL AND cooldown_seconds IS NOT NULL AND send_recovery IS NOT NULL
        AND subject_excludes IS NOT NULL
        AND (trigger NOT IN ('disk_pct', 'ram_pct')
             OR (warn_threshold IS NOT NULL AND crit_threshold IS NOT NULL))
    )
);

-- Built before the old index is dropped, so a rule scope is never without a
-- unique index.
CREATE UNIQUE INDEX IF NOT EXISTS ux_notification_rules_subject_scope
    ON notification_rules (trigger, (COALESCE(node_id, '')), (COALESCE(monitor_id, '')));
DROP INDEX IF EXISTS ux_notification_rules_scope;

-- Down and recovery come from the prober, which has already applied the
-- monitor's retries and recovery successes, so neither rule holds again.
INSERT INTO notification_rules
    (id, trigger, node_id, enabled, warn_threshold, crit_threshold, for_seconds, recovery_seconds,
     hysteresis, cooldown_seconds, send_recovery, subject_excludes, updated_by)
VALUES
    ('global-website_down',        'website_down',        NULL, true, NULL, NULL, 0, 0, 0, 1800, true, '{}', 'migration'),
    ('global-website_cert_expiry', 'website_cert_expiry', NULL, true, NULL, NULL, 0, 0, 0, 1800, true, '{}', 'migration')
ON CONFLICT DO NOTHING;

-- ----------------------------------------------- delivery details ----

ALTER TABLE notification_deliveries ADD COLUMN IF NOT EXISTS details jsonb NOT NULL DEFAULT '{}'::jsonb;

-- ------------------------------------------------ certificate events ----

ALTER TABLE monitor_events ADD COLUMN IF NOT EXISTS cert_expires_at timestamptz;
ALTER TABLE monitor_events DROP CONSTRAINT IF EXISTS monitor_events_kind_check;
ALTER TABLE monitor_events ADD CONSTRAINT monitor_events_kind_check
    CHECK (kind IN ('down', 'up', 'cert_warn', 'cert_crit', 'cert_renewed'));
