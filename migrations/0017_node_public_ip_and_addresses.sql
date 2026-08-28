-- 0017_node_public_ip_and_addresses.sql
--
-- Two pieces of node addressing that until now lived only inside a JSONB
-- inventory_snapshots blob, promoted to queryable columns:
--
--   nodes.public_ip -- the source address the CONTROL PLANE observed a
--   connection from this node arriving at. Server-side truth, never
--   self-reported: it is taken from the Noise-authenticated observed
--   multiaddr on the libp2p listener, or from the HTTPS enrollment request's
--   source address. A single current value, overwritten on each new
--   observation, the same convention nodes.last_seen_at already uses.
--
--   node_addresses -- the host's OWN view of its per-interface addressing,
--   promoted from the "network" inventory subject so it can be queried
--   without decoding JSON. Replaced wholesale per node on each push, matching
--   inventory_snapshots' own replace-on-arrival convention for this data.

ALTER TABLE nodes ADD COLUMN public_ip inet;

COMMENT ON COLUMN nodes.public_ip IS
    'Source address the control plane observed the node''s most recent connection from. Server-observed, never agent-reported. Overwritten on each observation.';

-- node_addresses carries a real foreign key to nodes, the one deliberate
-- exception to the no-FK-on-node_id convention the rest of the schema keeps
-- (see metric_samples' "No foreign key to nodes" note). The trade-off that
-- rules a FK out there -- a per-row parent lock on a bulk COPY of thousands
-- of samples -- does not apply here: these are low-frequency,
-- replace-wholesale writes of a few rows per node, not a hot ingest path.
-- What the FK buys is real: ON DELETE CASCADE means removing a node cannot
-- leave orphaned address rows behind.
CREATE TABLE node_addresses (
    node_id     text        NOT NULL REFERENCES nodes (node_id) ON DELETE CASCADE,
    -- The interface the address is bound to, as the host names it ("eth0",
    -- "en0"). Part of the key: one interface carries several addresses.
    interface   text        NOT NULL,
    -- Stored in CIDR form ("10.0.0.4/24", "fe80::1/64"), exactly as the agent
    -- reports it, so the prefix length is not lost. inet accepts the mask.
    address     inet        NOT NULL,
    -- When the host was observed, copied from the inventory snapshot's
    -- observed_at -- not the control plane's receive time.
    observed_at timestamptz NOT NULL,
    PRIMARY KEY (node_id, interface, address)
);

COMMENT ON TABLE node_addresses IS
    'Per-interface addresses a node reports for itself, promoted from the "network" inventory snapshot. Replaced wholesale per node on each push; no history.';

-- "What addresses does this node have" is the only query shape, and it is
-- already served by the primary key's leading column; this index exists so
-- the ON DELETE CASCADE probe does not scan.
CREATE INDEX idx_node_addresses_node_id ON node_addresses (node_id);
