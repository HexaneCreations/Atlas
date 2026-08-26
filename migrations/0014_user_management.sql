-- 0014_user_management.sql
--
-- Backend support for the admin "Users" management page: a fleet-wide-only
-- permission gating user-management actions themselves (managing other
-- users' access is not a per-node concept), and an append-only audit trail
-- of who performed which grant/revoke/disable/enable/reset/logout action on
-- whom — otherwise that data has nowhere to live and nobody could answer
-- "who changed this and when."

INSERT INTO permissions (key) VALUES ('user.manage');

-- Admin-only: deliberately not extended to operator, the same restraint
-- role_permissions already applies to node.logs.read.
INSERT INTO role_permissions (role_name, permission_key) VALUES
    ('admin', 'user.manage');

CREATE TABLE user_audit_log (
    id             text        PRIMARY KEY,
    -- Who performed the action. Plain text, not a foreign key: the CLI
    -- records an operator label here (see cmd/atlas-server/user.go), not a
    -- real user id, the same convention user_node_roles.granted_by/
    -- revoked_by already use.
    actor_user_id  text        NOT NULL,
    target_user_id text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    action         text        NOT NULL,
    detail         jsonb       NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_user_audit_log_target ON user_audit_log (target_user_id, created_at DESC);

COMMENT ON TABLE user_audit_log IS
    'Append-only record of every user-management action (create, grant, revoke, disable, enable, reset-password, force-logout), who performed it, and when. See internal/core/user.AuditEntry.';
