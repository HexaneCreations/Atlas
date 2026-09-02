-- 0019_superadmin_role.sql
--
-- Registers a fourth role, "superadmin": a protected tier that ordinary
-- fleet-wide admins cannot act on. See internal/core/user.RoleSuperadmin and
-- the guard in internal/api/v1/users.go (Handler.guardSuperadminTarget).
--
-- This migration only *defines* the role and its permissions. It grants it
-- to nobody. There is exactly one superadmin, assigned once by a deliberate
-- manual grant after this ships — a direct user_node_roles INSERT, or
-- `atlas-server user grant --role superadmin --fleet-wide` — never by an
-- automated promotion here or anywhere else. The role also cannot be granted
-- through the REST API at all: POST /users/{id}/grants rejects it outright
-- (internal/core/user.ErrSuperadminNotGrantable), from every caller.
--
-- Permissions: superadmin is a strict superset of admin. It holds every
-- permission admin holds (node.read, node.logs.read, fleet.write,
-- user.manage) so a superadmin does everything an admin can — plus it is the
-- one role the user-management guard will not block, in either direction.
--
-- Page access (the separate internal/core/pageauthz axis: user_page_access /
-- user_role_access, keyed per user, not per role) is deliberately NOT seeded
-- here — there is no role dimension to seed it against. The manual grant
-- that assigns the superadmin also assigns the fleet-wide "full-access"
-- bundle, exactly as an admin already carries it via 0016.

INSERT INTO roles (name) VALUES ('superadmin');

INSERT INTO role_permissions (role_name, permission_key) VALUES
    ('superadmin', 'node.read'),
    ('superadmin', 'node.logs.read'),
    ('superadmin', 'fleet.write'),
    ('superadmin', 'user.manage');
