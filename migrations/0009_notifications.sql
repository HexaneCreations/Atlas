-- 0009_notifications.sql
--
-- Notification channels and a durable delivery queue. A channel is a
-- configured destination (a webhook today); a delivery is one (event,
-- channel) attempt record, the unit retries operate on so a retry can never
-- create a duplicate — see internal/core/notification.
--
-- webhook_secret is stored in plain text. Atlas has no at-rest encryption
-- primitive anywhere in this codebase (TLS private keys are protected by
-- filesystem permissions alone, per internal/platform/pki/store.go) — the
-- database is the existing trust boundary. The API layer never serializes
-- this column; see internal/api/v1/notifications.go.

CREATE TABLE notification_channels (
    id             text        PRIMARY KEY,
    name           text        NOT NULL,
    type           text        NOT NULL,   -- 'webhook'
    enabled        boolean     NOT NULL DEFAULT true,
    webhook_url    text        NOT NULL DEFAULT '',
    webhook_secret text        NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

-- One delivery attempt record per (event, channel). status stays 'pending'
-- across retries — only a terminal outcome moves it to 'delivered' or
-- 'failed' — so "due for retry" is just "pending, next_attempt_at <= now()".
CREATE TABLE notification_deliveries (
    id              text        PRIMARY KEY,
    event_id        text        NOT NULL,
    channel_id      text        NOT NULL REFERENCES notification_channels(id) ON DELETE CASCADE,
    trigger         text        NOT NULL,
    node_id         text        NOT NULL DEFAULT '',
    severity        text        NOT NULL DEFAULT '',
    title           text        NOT NULL DEFAULT '',
    message         text        NOT NULL DEFAULT '',
    event_time      timestamptz NOT NULL,
    status          text        NOT NULL,   -- 'pending' | 'delivered' | 'failed'
    attempts        integer     NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL,
    last_error      text        NOT NULL DEFAULT '',
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (event_id, channel_id)
);

CREATE INDEX idx_notification_deliveries_due ON notification_deliveries (status, next_attempt_at)
    WHERE status = 'pending';
