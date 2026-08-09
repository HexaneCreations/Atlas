-- 0006_alerts.sql
--
-- Alert rules, their current per-node state, and a durable firing/resolved
-- history. Threshold rules are evaluated periodically against metric_samples;
-- event rules fire from the durable event log (see 0005_events.sql).

CREATE TABLE alert_rules (
    id          text        PRIMARY KEY,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    enabled     boolean     NOT NULL DEFAULT true,
    kind        text        NOT NULL,   -- 'threshold' | 'event'
    severity    text        NOT NULL,   -- 'warning' | 'critical'

    -- threshold fields
    metric      text,
    comparison  text,               -- 'gt' | 'gte' | 'lt' | 'lte'
    threshold   double precision,
    for_seconds integer     NOT NULL DEFAULT 0,
    node_id     text,               -- NULL: evaluated against every node

    -- event fields
    topic       text,
    subject     text,

    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_alert_rules_enabled ON alert_rules (enabled) WHERE enabled;

-- Current state per (rule, node, series). series_key distinguishes multiple
-- series of one metric on one node (per-mount disk usage, per-interface
-- network) so they alert independently instead of overwriting each other.
CREATE TABLE alert_states (
    rule_id       text        NOT NULL REFERENCES alert_rules(id) ON DELETE CASCADE,
    node_id       text        NOT NULL,
    series_key    text        NOT NULL DEFAULT '',
    state         text        NOT NULL,   -- 'ok' | 'pending' | 'firing'
    value         double precision,
    message       text        NOT NULL DEFAULT '',
    pending_since timestamptz,
    fired_at      timestamptz,
    resolved_at   timestamptz,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (rule_id, node_id, series_key)
);

CREATE INDEX idx_alert_states_firing ON alert_states (state) WHERE state IN ('pending', 'firing');

-- Append-only transition log. No FK to alert_rules: history is an audit
-- trail and must survive a rule being edited or deleted.
CREATE TABLE alert_history (
    id      text        NOT NULL,
    time    timestamptz NOT NULL,
    rule_id text        NOT NULL,
    node_id text        NOT NULL,
    state   text        NOT NULL,
    value   double precision,
    message text        NOT NULL DEFAULT '',
    PRIMARY KEY (id, time)
);

SELECT create_hypertable('alert_history', by_range('time', INTERVAL '30 days'));

CREATE INDEX idx_alert_history_rule_time ON alert_history (rule_id, time DESC);
CREATE INDEX idx_alert_history_node_time ON alert_history (node_id, time DESC);

-- Longer than events: this is the audit trail of what actually alerted.
SELECT add_retention_policy('alert_history', INTERVAL '1 year');
