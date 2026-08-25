-- 0013_fleet_write_permission.sql
--
-- Adds the permission that gates the privileged-configuration write
-- endpoints (alert rules, SLO definitions, notification channels) added in
-- migrations/0012_users.sql's authorization model. These are not node-scoped
-- — they configure Atlas itself, not a single observed host — so unlike
-- node.read/node.logs.read this permission is checked without a node
-- dimension. It reuses the existing node_id IS NULL "fleet-wide" grant
-- semantics from user_node_roles as its global grant, rather than adding a
-- second grant table: a fleet-wide grant already matches any node_id passed
-- to HasPermission, so it already behaves as a global permission with no
-- schema change required beyond registering the permission itself.
--
-- Before this, POST/PUT/DELETE on /alerts/rules, /slo, and
-- /notifications/channels were reachable with no authentication at all —
-- including registering a notification channel with an arbitrary webhook
-- URL. See docs/adr/0013-human-user-authentication-and-authorization.md.

INSERT INTO permissions (key) VALUES ('fleet.write');

-- viewer does not get it: a read-only role must not be able to write
-- configuration. operator and admin both can — an infrastructure console
-- has no third tier between "can look" and "can change things" per
-- docs/adr/0011-deferred-rbac.md's role-set reasoning.
INSERT INTO role_permissions (role_name, permission_key) VALUES
    ('operator', 'fleet.write'),
    ('admin',    'fleet.write');
