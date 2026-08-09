-- 0002_metric_storage.sql
--
-- The nodes Atlas observes, and the time series it collects from them.
--
-- This migration establishes the storage shape every later tier builds on, so
-- the choices here are load-bearing. See docs/database/schema.md for the
-- conventions and docs/adr/0011-denormalised-metric-storage.md for why samples
-- are stored denormalised rather than behind a series registry.

-- ---------------------------------------------------------------- nodes ----

-- A machine Atlas observes.
--
-- node_id is derived from the OS machine id where one exists (see
-- internal/platform/hostid), so it survives hostname changes and re-imaging.
-- Everything else in this table is mutable display detail that may change
-- between collections without the node becoming a different node.
CREATE TABLE nodes (
    node_id       text        PRIMARY KEY,
    hostname      text        NOT NULL,
    -- Operating system facts, refreshed on every host collection. Nullable
    -- because a node row is created on first sample arrival, which may precede
    -- the first successful host-facts collection.
    os            text,
    platform      text,
    kernel        text,
    architecture  text,
    cpu_cores     integer,
    -- boot_time lets uptime be computed at query time rather than stored as a
    -- sample that is stale the moment it is written.
    boot_time     timestamptz,
    -- agent_version is the build that produced the observations. In a fleet,
    -- agents upgrade at different times, and a metric whose meaning changed
    -- between versions is uninterpretable without this.
    agent_version text,
    first_seen_at timestamptz NOT NULL DEFAULT now(),
    -- last_seen_at is what makes a node "down": no observation for longer than
    -- expected. It is updated on every ingest.
    last_seen_at  timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE nodes IS
    'Machines observed by Atlas. node_id is stable across hostname changes.';

-- Supports the "which nodes have gone quiet" query that drives node status.
CREATE INDEX idx_nodes_last_seen_at ON nodes (last_seen_at DESC);

-- -------------------------------------------------------- metric_samples ----

-- One numeric observation, at one instant, from one node.
--
-- The table is deliberately narrow and denormalised: a plugin architecture
-- means metric names are not known when this migration is written, so there is
-- no schema to widen. Compression below reclaims the cost of repeating the
-- text columns.
CREATE TABLE metric_samples (
    time         timestamptz      NOT NULL,
    node_id      text             NOT NULL,
    collector_id text             NOT NULL,
    metric       text             NOT NULL,
    value        double precision NOT NULL,
    -- unit and kind travel with the sample rather than being looked up by
    -- name. The presentation layer must be able to format a metric it has
    -- never seen, and the query layer must be able to refuse an illegitimate
    -- aggregation -- averaging a counter is meaningless, and summing a gauge
    -- across hosts is usually wrong.
    unit         text             NOT NULL,
    kind         text             NOT NULL,
    -- labels are the dimensions of the sample: which disk, which interface.
    -- JSONB rather than columns because the dimensions differ per metric.
    -- Cardinality must stay bounded: every distinct combination is a distinct
    -- series.
    labels       jsonb            NOT NULL DEFAULT '{}'::jsonb
);

COMMENT ON TABLE metric_samples IS
    'Time-series metric observations. Hypertable partitioned by time.';

-- No foreign key to nodes.
--
-- A foreign key would make every insert take a lock on the parent row, which
-- on a bulk COPY of thousands of samples is real contention for a constraint
-- that buys little: samples arrive from a transport that already validated
-- their origin, and an orphaned sample is a diagnosable curiosity rather than
-- a correctness problem. Node rows are upserted before their samples land.

-- Partition by time. Seven days per chunk suits a fifteen-second cadence:
-- large enough that chunk count stays manageable over years, small enough that
-- a chunk still fits comfortably in memory during compression.
SELECT create_hypertable(
    'metric_samples',
    by_range('time', INTERVAL '7 days')
);

-- The dominant query is "this metric, on this node, over this window", so the
-- index leads with the equality columns and ends with time descending.
-- TimescaleDB adds its own time index; this one serves the filtered case.
CREATE INDEX idx_metric_samples_node_metric_time
    ON metric_samples (node_id, metric, time DESC);

-- Supports collector-level views: "everything system.cpu produced recently".
CREATE INDEX idx_metric_samples_collector_time
    ON metric_samples (collector_id, time DESC);

-- ---------------------------------------------------------- compression ----

-- segmentby groups rows sharing a node and metric into one compressed batch,
-- so the repeated text in those columns is stored once rather than per row.
-- This is what makes the denormalised design affordable.
--
-- orderby time DESC keeps recent data at the front of each batch, which is the
-- direction almost every query reads.
ALTER TABLE metric_samples SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'node_id, metric',
    timescaledb.compress_orderby   = 'time DESC'
);

-- Compress chunks older than a day. Recent data stays uncompressed because it
-- is both the most queried and still being written to.
SELECT add_compression_policy('metric_samples', INTERVAL '1 day');

-- ------------------------------------------------------------ retention ----

-- Raw samples are kept for 30 days. Longer horizons are served by the
-- continuous aggregates below, which survive this policy.
SELECT add_retention_policy('metric_samples', INTERVAL '30 days');

-- -------------------------------------------------- continuous aggregates ----

-- Rollups are materialised incrementally by TimescaleDB rather than computed
-- per query. A month-wide chart reads pre-aggregated buckets instead of
-- scanning millions of raw rows.
--
-- min and max are kept alongside avg because an average hides exactly the
-- spike an operator is looking for. A CPU that averaged 40% while touching
-- 100% is not a CPU that sat at 40%.

CREATE MATERIALIZED VIEW metric_samples_1m
    WITH (timescaledb.continuous) AS
SELECT
    time_bucket(INTERVAL '1 minute', time) AS bucket,
    node_id,
    collector_id,
    metric,
    unit,
    kind,
    labels,
    avg(value)   AS avg_value,
    min(value)   AS min_value,
    max(value)   AS max_value,
    count(*)     AS sample_count
FROM metric_samples
GROUP BY bucket, node_id, collector_id, metric, unit, kind, labels
WITH NO DATA;

CREATE MATERIALIZED VIEW metric_samples_1h
    WITH (timescaledb.continuous) AS
SELECT
    time_bucket(INTERVAL '1 hour', bucket) AS bucket,
    node_id,
    collector_id,
    metric,
    unit,
    kind,
    labels,
    -- Averaging the minute averages weighted by their sample counts, rather
    -- than averaging the averages, which would misweight buckets that
    -- collected fewer samples.
    sum(avg_value * sample_count) / nullif(sum(sample_count), 0) AS avg_value,
    min(min_value) AS min_value,
    max(max_value) AS max_value,
    sum(sample_count) AS sample_count
FROM metric_samples_1m
GROUP BY 1, node_id, collector_id, metric, unit, kind, labels
WITH NO DATA;

-- Refresh policies.
--
-- start_offset bounds how far back each run reconsiders; end_offset leaves the
-- most recent window alone because samples for it may still be arriving, and
-- materialising a partial bucket would publish a wrong value that is never
-- revisited.
SELECT add_continuous_aggregate_policy('metric_samples_1m',
    start_offset      => INTERVAL '2 hours',
    end_offset        => INTERVAL '1 minute',
    schedule_interval => INTERVAL '1 minute');

SELECT add_continuous_aggregate_policy('metric_samples_1h',
    start_offset      => INTERVAL '3 days',
    end_offset        => INTERVAL '1 hour',
    schedule_interval => INTERVAL '10 minutes');

-- Rollups are cheap to keep and are what makes long-horizon capacity planning
-- possible after raw samples have aged out.
SELECT add_retention_policy('metric_samples_1m', INTERVAL '90 days');
SELECT add_retention_policy('metric_samples_1h', INTERVAL '2 years');
