-- 0016_page_access_bootstrap.sql
--
-- Closes the bootstrap deadlock 0015 introduces on its own: every /users/*
-- page-access management endpoint requires PageUsers (see
-- internal/api/v1/pageaccess.go's requirePermission calls), and with zero
-- page-access grants existing for anyone the moment 0015 applies, nobody —
-- not even an existing admin — could call the one endpoint that grants
-- PageUsers. There would be no way in through the API at all, not a
-- rollout inconvenience but a hard deadlock.
--
-- This seeds a fleet-wide "full-access" bundle covering every page, and
-- assigns it to every currently-enabled user who already holds the admin
-- role fleet-wide today, so an existing admin's access survives this
-- deploy unaffected and they can restore everyone else's through the
-- normal grant surface (the CLI, or the admin API once the frontend exists)
-- afterward — the same reasoning cmd/atlas-server's `page-access`/
-- `role-access` CLI commands exist for at all.
--
-- Deliberately scoped to admins only, not every user: the entire point of
-- this feature is narrower-than-node.read control, and blanket-granting
-- every page to every viewer/operator here would defeat that on day one —
-- an admin decides what everyone else actually needs.
--
-- A separate migration from 0015 rather than amending it: 0015 has already
-- applied in every environment (including local dev) that ran it before
-- this file existed, and this repository's migrator verifies each applied
-- migration's checksum — editing 0015 after the fact would make every
-- database that already ran it fail that check on the next migrate.

INSERT INTO role_access (name, created_at, created_by)
VALUES ('full-access', now(), 'migration')
ON CONFLICT (name) DO NOTHING;

INSERT INTO role_access_pages (role_access_name, page_key)
SELECT 'full-access', key FROM pages
ON CONFLICT DO NOTHING;

INSERT INTO user_role_access (id, user_id, role_access_name, node_id, granted_at, granted_by)
SELECT gen_random_uuid()::text, u.id, 'full-access', NULL, now(), 'migration'
FROM users u
JOIN user_node_roles unr ON unr.user_id = u.id
WHERE unr.role_name = 'admin'
  AND unr.node_id IS NULL
  AND unr.revoked_at IS NULL
  AND u.disabled_at IS NULL
ON CONFLICT (user_id, role_access_name, COALESCE(node_id, '')) WHERE revoked_at IS NULL DO NOTHING;
