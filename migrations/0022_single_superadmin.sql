-- 0022_single_superadmin.sql
--
-- Enforces at most one active superadmin grant, fleet-wide only. "Active"
-- means revoked_at IS NULL; whether the holder is disabled does not matter —
-- a disabled superadmin still holds the slot (and stays protected from
-- admins) until `atlas-server user transfer-superadmin` replaces them.
--
-- The DO block refuses to proceed, with a readable message, on a database
-- that already holds more than one active superadmin grant or a node-scoped
-- one, instead of letting CREATE UNIQUE INDEX fail with a bare constraint
-- error. A database with zero or one fleet-wide grant passes unchanged.
-- Resolve with `atlas-server user grants --user-id <id>` and
-- `atlas-server user revoke-grant --grant-id <id>`, then restart.

DO $$
DECLARE
    active_count int;
    scoped_count int;
BEGIN
    SELECT count(*), count(*) FILTER (WHERE node_id IS NOT NULL)
    INTO active_count, scoped_count
    FROM user_node_roles
    WHERE role_name = 'superadmin' AND revoked_at IS NULL;

    IF active_count > 1 THEN
        RAISE EXCEPTION 'found % active superadmin grants; exactly one is allowed — revoke all but one before upgrading', active_count;
    END IF;
    IF scoped_count > 0 THEN
        RAISE EXCEPTION 'found a node-scoped superadmin grant; superadmin must be fleet-wide — revoke it before upgrading';
    END IF;
END
$$;

CREATE UNIQUE INDEX idx_user_node_roles_one_superadmin
    ON user_node_roles ((true))
    WHERE role_name = 'superadmin' AND revoked_at IS NULL;

ALTER TABLE user_node_roles
    ADD CONSTRAINT user_node_roles_superadmin_fleet_wide
    CHECK (role_name <> 'superadmin' OR node_id IS NULL);
