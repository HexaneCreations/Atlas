-- Environment tagging for nodes.
--
-- An environment is not something Atlas can discover: nothing on a machine
-- says "I am production". It is a label the operator applies, so it arrives
-- as configuration and travels with the observations on the transport origin,
-- alongside the node id and agent version.
--
-- Nullable with no default: a node whose operator has not set one is honestly
-- untagged, and defaulting it to 'production' would invent a fact. The API
-- groups untagged nodes under "unassigned" rather than hiding them.

ALTER TABLE nodes ADD COLUMN environment text;

COMMENT ON COLUMN nodes.environment IS
    'Operator-assigned environment (production, staging, ...). Null when untagged.';

-- Supports grouping the fleet by environment, which is how the overview
-- summarises it.
CREATE INDEX idx_nodes_environment ON nodes (environment) WHERE environment IS NOT NULL;
