-- 0001_extensions.sql
--
-- Establishes the database capabilities every later migration depends on.
-- This runs before any table exists, so it is the point at which a database
-- that cannot support Atlas fails loudly, during migration, rather than
-- silently later when a hypertable creation fails.
--
-- Both extensions must be available in the server image. The reference
-- deployment uses timescale/timescaledb, which ships both.

-- TimescaleDB turns ordinary tables into hypertables: transparently
-- partitioned by time, with native compression and retention policies.
-- Atlas stores every metric sample and every observed event as time-series
-- data, so this is a hard requirement rather than an optimisation. Choosing
-- it over a separate metrics store is recorded in
-- docs/adr/0003-postgresql-timescaledb-datastore.md.
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- pgcrypto supplies gen_random_uuid() and the digest functions used for
-- content-addressed identifiers in the service catalog and deployment
-- history. Generating identifiers in the database keeps them correct for rows
-- inserted by migrations and by bulk COPY, neither of which passes through
-- application code.
CREATE EXTENSION IF NOT EXISTS pgcrypto;
