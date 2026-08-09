-- 0007_incidents.sql
--
-- Incidents group correlated events and alert firings under one durable
-- record. Members reference the event/alert_history rows that make up an
-- incident rather than copying them, so the full underlying history stays
-- exactly where 0005/0006 put it — this table is a view over it, not a
-- second copy.

-- Denormalized onto alert_history so incident correlation does not need a
-- rule lookup per transition.
ALTER TABLE alert_history ADD COLUMN severity text NOT NULL DEFAULT 'warning';

CREATE TABLE incidents (
    id                 text        PRIMARY KEY,
    title              text        NOT NULL,
    status             text        NOT NULL,   -- 'open' | 'resolved'
    severity           text        NOT NULL,   -- 'info' | 'warning' | 'critical'
    root_cause_kind    text,                   -- 'event' | 'alert'
    root_cause_ref_id  text,
    root_cause_topic   text,
    opened_at          timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    resolved_at        timestamptz
);

CREATE INDEX idx_incidents_status_updated ON incidents (status, updated_at DESC);
CREATE INDEX idx_incidents_opened_at ON incidents (opened_at DESC);

-- One row per event or alert-history entry folded into an incident.
-- node_id/topic/severity/time are copied at correlation time rather than
-- joined back to the source table on every read — the source event or
-- alert-history row is still the record of truth for anything else about
-- that occurrence (payload, message).
CREATE TABLE incident_members (
    id            text        PRIMARY KEY,
    incident_id   text        NOT NULL REFERENCES incidents(id) ON DELETE CASCADE,
    kind          text        NOT NULL,   -- 'event' | 'alert'
    ref_id        text        NOT NULL,   -- eventstore record id or alert_history id
    node_id       text        NOT NULL,
    topic         text        NOT NULL,
    severity      text        NOT NULL,
    time          timestamptz NOT NULL,
    is_root_cause boolean     NOT NULL DEFAULT false
);

CREATE INDEX idx_incident_members_incident_time ON incident_members (incident_id, time);
CREATE INDEX idx_incident_members_node_time ON incident_members (node_id, time DESC);

-- A given event or alert-history entry belongs to at most one incident.
CREATE UNIQUE INDEX idx_incident_members_dedup ON incident_members (kind, ref_id);
