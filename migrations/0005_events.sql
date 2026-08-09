-- 0005_events.sql
--
-- Durable log of discrete state-change events (container lifecycle, host
-- reboots, collector failures, ...). The event bus itself is lossy by design
-- (internal/platform/eventbus); this table is where anything that must
-- survive a restart or feed alerting/incidents actually lives.

CREATE TABLE events (
    id          text        NOT NULL,
    time        timestamptz NOT NULL,
    node_id     text        NOT NULL,
    topic       text        NOT NULL,
    source      text        NOT NULL,
    subject     text        NOT NULL DEFAULT '',
    payload     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, time)
);

COMMENT ON TABLE events IS
    'Durable log of bus events, fleet-wide. Hypertable partitioned by time.';

SELECT create_hypertable('events', by_range('time', INTERVAL '7 days'));

-- "This node's events over time" (incident timeline) and "this kind of event,
-- anywhere" (alert rule matching) are the two access patterns.
CREATE INDEX idx_events_node_time ON events (node_id, time DESC);
CREATE INDEX idx_events_topic_time ON events (topic, time DESC);

ALTER TABLE events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'node_id, topic',
    timescaledb.compress_orderby   = 'time DESC'
);
SELECT add_compression_policy('events', INTERVAL '1 day');

-- Longer than raw metrics (30d): this is what incident investigation and
-- alert history read from.
SELECT add_retention_policy('events', INTERVAL '180 days');
