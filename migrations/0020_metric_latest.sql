-- 0020_metric_latest.sql
--
-- The newest value of every series, one row per series, replaced in place.
-- Written in the same transaction as the metric_samples COPY (see
-- internal/storage/metric.Repository.InsertBatch) and read by Latest and
-- LatestForMetric, so "what is the value now" no longer depends on scanning
-- the time series.
--
-- The key is exactly the grouping Latest has always used — DISTINCT ON
-- (metric, collector_id, labels) per node — so its semantics do not change.
-- series_key stands in for labels in the key: it is the canonical JSON
-- encoding of the label set (sorted keys, escaped values), which is equal for
-- two label sets exactly when the jsonb values are equal, and is a plain text
-- column a btree primary key handles reliably.
--
-- No foreign key to nodes, for the same reason metric_samples has none (see
-- 0002_metric_storage.sql); the node row is upserted earlier in the same
-- transaction.
--
-- fillfactor 70 leaves room on each page for the new row version, and no
-- index covers a column the upsert changes (value, time, unit, kind, labels),
-- so the steady-state update is a HOT update that writes no index entries.
-- That is also why there is no index on time: the hourly prune
-- (Repository.PruneLatest) scans a table bounded by series count, not by
-- history.
CREATE TABLE metric_latest (
    node_id      text             NOT NULL,
    metric       text             NOT NULL,
    collector_id text             NOT NULL,
    series_key   text             NOT NULL,
    labels       jsonb            NOT NULL DEFAULT '{}'::jsonb,
    unit         text             NOT NULL,
    kind         text             NOT NULL,
    value        double precision NOT NULL,
    time         timestamptz      NOT NULL,
    PRIMARY KEY (node_id, metric, collector_id, series_key)
) WITH (fillfactor = 70);

-- LatestForMetric reads one metric across every node. Latest is served by the
-- primary key's leading node_id.
CREATE INDEX idx_metric_latest_metric ON metric_latest (metric, node_id);

COMMENT ON TABLE metric_latest IS
    'Newest value per series, upserted on ingest. Rows older than 24h are pruned by the control plane.';
