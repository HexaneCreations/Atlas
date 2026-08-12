-- 0010_agent_operation_grants.sql
--
-- Explicit, independently revocable authorization for privileged AgentOps
-- operations. Before this, a node's live certificate was itself treated as
-- sufficient authorization (the mtls handshake and the operation grant were
-- conflated). See docs/architecture/security.md: authentication is never
-- sufficient authorization.

CREATE TABLE agent_operation_grants (
    node_id        text        NOT NULL,
    -- The operation name, e.g. 'container_logs' (see fleet.OperationContainerLogs).
    -- Not a foreign key to any enum: AgentOps operations are a small,
    -- explicitly-coded set (see libp2ptransport.AgentOpsProtocolID's doc),
    -- not a registry a row here should be able to invent membership in.
    operation      text        NOT NULL,
    granted_at     timestamptz NOT NULL DEFAULT now(),
    -- Free-text provenance ("enrollment", an operator's name) — informational
    -- only, never interpreted.
    granted_by     text,
    revoked_at     timestamptz,
    revoked_reason text,
    PRIMARY KEY (node_id, operation)
);

COMMENT ON TABLE agent_operation_grants IS
    'Explicit authorization for a node to have a privileged AgentOps operation performed against it, independent of and never implied by certificate validity alone.';

-- Backfill: preserve current behavior for the existing fleet. Every node
-- that already holds a live credential today has, in practice, always been
-- allowed container_logs (the only gap being that this was implicit, not a
-- revocable record) — this makes that grant explicit without changing
-- anyone's actual access on upgrade.
INSERT INTO agent_operation_grants (node_id, operation, granted_at, granted_by)
SELECT DISTINCT node_id, 'container_logs', now(), 'migration-0010-backfill'
FROM node_credentials
WHERE revoked_at IS NULL AND expires_at > now();
