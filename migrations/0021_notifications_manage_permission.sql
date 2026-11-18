-- 0021_notifications_manage_permission.sql
--
-- Registers notifications.manage, the permission that gates every
-- /notifications endpoint (channels and the delivery log), and gives it to
-- superadmin only. Not admin and not operator: channel configuration holds
-- bearer secrets (SMTP passwords, Teams webhook URLs), so it sits on the one
-- protected tier. Page access (the full-access bundle) does not grant it —
-- that axis is separate and never consulted for this permission.
--
-- Checked with an empty node id (internal/api/v1.Handler.requirePermission),
-- so only a fleet-wide superadmin grant satisfies it.
--
-- ON CONFLICT keeps the migration safe to re-run against a database where
-- either row already exists.

INSERT INTO permissions (key) VALUES ('notifications.manage')
ON CONFLICT (key) DO NOTHING;

INSERT INTO role_permissions (role_name, permission_key) VALUES
    ('superadmin', 'notifications.manage')
ON CONFLICT (role_name, permission_key) DO NOTHING;
