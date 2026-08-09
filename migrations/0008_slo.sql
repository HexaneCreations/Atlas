-- 0008_slo.sql
--
-- SLO definitions. Evaluation reads metric_samples directly over each
-- definition's window at request time — there is no durable evaluation
-- history here, the same way alert_states is live and only alert_history is
-- durable. An SLO's compliance is a question about the past window, not a
-- fact that needs a row of its own until something durable (a status
-- transition) happens to it — a later milestone's concern, not this one's.

CREATE TABLE slos (
    id                     text        PRIMARY KEY,
    name                   text        NOT NULL,
    node_id                text        NOT NULL,
    -- signal is informational only: which Golden Signal this SLO belongs
    -- to, for grouping and display. Evaluation reads metric directly and
    -- does not interpret this value.
    signal                 text        NOT NULL DEFAULT '',
    metric                 text        NOT NULL,
    comparison             text        NOT NULL,   -- 'gt' | 'gte' | 'lt' | 'lte'; the compliant condition
    threshold              double precision NOT NULL,
    target_percentage      double precision NOT NULL,
    window_seconds         integer     NOT NULL,
    warning_budget_percent double precision NOT NULL DEFAULT 0,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_slos_node ON slos (node_id);
