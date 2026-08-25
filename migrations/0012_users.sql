-- 0012_users.sql
--
-- Human-user identity, authentication, and role-based authorization for the
-- Atlas control plane. Deferred by design until now — see
-- docs/adr/0011-deferred-rbac.md — and built to the shape that ADR fixed in
-- advance: a session cookie (never a bearer token in a URL, so the streaming
-- chain's AccessLog never records one), a fixed role set bound to a static
-- permission mapping, and authorization that binds per node rather than
-- globally.
--
-- This is a separate identity domain from Atlas Agents. A human user is never
-- a libp2p Peer ID and never appears in agent_peers or
-- agent_operation_grants; see docs/adr/0012-connect-by-identity.md and
-- internal/core/fleet.

CREATE TABLE users (
    id            text        PRIMARY KEY,
    -- The login identifier. Not email: an operator's login name and their
    -- contact address are two different facts, and conflating them forces a
    -- password-reset flow to double as an email-verification flow before
    -- either is needed.
    username      text        NOT NULL,
    -- bcrypt output (internal/core/user.HashPassword); never a reversible
    -- encoding and never logged.
    password_hash text        NOT NULL,
    -- Optional, non-unique contact address for future password-reset
    -- delivery or display. Never the credential and never used to look up a
    -- user at login.
    email         text,
    disabled_at   timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT users_username_not_blank CHECK (username <> '')
);

-- Case-insensitive: an operator typing "Admin" at login must not create a
-- second identity next to "admin".
CREATE UNIQUE INDEX idx_users_username ON users (lower(username));

COMMENT ON TABLE users IS
    'Human-user identity for the Atlas control plane. Separate from Agent identity: see agent_peers and docs/adr/0012-connect-by-identity.md.';

CREATE TABLE sessions (
    -- SHA-256 of the session token; the raw token lives only in the browser's
    -- cookie and is never stored, the same shape and reasoning as
    -- enrollment_tokens.token_hash.
    token_hash text        PRIMARY KEY,
    user_id    text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    revoked_at timestamptz
);

CREATE INDEX idx_sessions_user_id ON sessions (user_id);

COMMENT ON TABLE sessions IS
    'Server-side record of an issued session cookie. One mechanism serves both the REST chain and the streaming chain, per docs/adr/0011-deferred-rbac.md sec 3.';

CREATE TABLE roles (
    name text PRIMARY KEY
);

-- The fixed set docs/adr/0011-deferred-rbac.md names in its "per-user ACLs"
-- alternative: "an infrastructure console has few, stable actor kinds
-- (viewer, operator, admin)". Not an open registry — see
-- internal/core/user.Role.
INSERT INTO roles (name) VALUES ('viewer'), ('operator'), ('admin');

COMMENT ON TABLE roles IS
    'The fixed role set docs/adr/0011-deferred-rbac.md names: viewer, operator, admin.';

CREATE TABLE permissions (
    key text PRIMARY KEY
);

-- node.logs.read is separate from node.read because ADR-0011 names container
-- log content as uniquely sensitive ("the single most sensitive thing the
-- API serves") — a viewer may see that a container exists without being able
-- to read what it printed.
INSERT INTO permissions (key) VALUES
    ('node.read'),
    ('node.logs.read');

COMMENT ON TABLE permissions IS
    'Actions a role may be granted, checked per node via user_node_roles. See internal/core/user.Permission.';

CREATE TABLE role_permissions (
    role_name      text NOT NULL REFERENCES roles (name),
    permission_key text NOT NULL REFERENCES permissions (key),
    PRIMARY KEY (role_name, permission_key)
);

INSERT INTO role_permissions (role_name, permission_key) VALUES
    ('viewer',   'node.read'),
    ('operator', 'node.read'),
    ('operator', 'node.logs.read'),
    ('admin',    'node.read'),
    ('admin',    'node.logs.read');

COMMENT ON TABLE role_permissions IS
    'Static mapping of role to permission, seeded above. Not an operator-editable table in this phase.';

CREATE TABLE user_node_roles (
    id         text        PRIMARY KEY,
    user_id    text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    -- NULL means fleet-wide: this role applies to every node, not just one.
    -- Deliberately not a sentinel string ('*', ''): NULL cannot be produced
    -- by an empty or omitted field the way a blank string can, so every
    -- caller that grants one must choose it explicitly. See
    -- internal/core/user.GrantSpec and cmd/atlas-server's `user grant`,
    -- which requires --node-id XOR --fleet-wide with no default between
    -- them.
    node_id    text,
    role_name  text        NOT NULL REFERENCES roles (name),
    granted_at timestamptz NOT NULL DEFAULT now(),
    granted_by text,
    revoked_at timestamptz,
    revoked_by text
);

CREATE INDEX idx_user_node_roles_lookup ON user_node_roles (user_id, node_id) WHERE revoked_at IS NULL;

-- Prevents a duplicate active grant of the same role to the same user for
-- the same scope. COALESCE folds NULL (fleet-wide) to '' so two fleet-wide
-- grants of one role collide the way two grants of the same real node_id do
-- — plain NULL never equals NULL in a unique index.
CREATE UNIQUE INDEX idx_user_node_roles_unique_active
    ON user_node_roles (user_id, COALESCE(node_id, ''), role_name)
    WHERE revoked_at IS NULL;

COMMENT ON TABLE user_node_roles IS
    'Explicit, independently revocable authorization for a user to hold a role, scoped to one node or (node_id NULL) fleet-wide. Mirrors agent_operation_grants''s shape for the human-user axis; never joined against it.';
