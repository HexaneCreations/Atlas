-- 0015_page_access.sql
--
-- Page-visibility authorization: a second, independent access-control axis
-- from user_node_roles/role_permissions (which stays untouched). That layer
-- answers "may this principal perform this operation against this node";
-- this one answers "may this principal reach this page at all, for this
-- node" — a narrower question a single node.read grant currently cannot
-- express (it reaches Containers, Processes, Services, Scheduled jobs,
-- Ports, and Disks all at once). A user needs both: page access from this
-- migration's tables, and the existing node.read/node.logs.read grant for
-- that node — this layer only narrows what is reachable, never substitutes
-- for the existing check. See internal/core/pageauthz.

-- The fixed page catalog — mirrors roles/permissions' seeded-table
-- convention (migrations/0012_users.sql), not an operator-editable
-- registry. fleet_only pages (overview, nodes, users) have no per-node
-- concept: a grant naming one must always be fleet-wide, and — per
-- pageauthz.ValidateBundleMembership — none of them may be added to a
-- reusable role_access bundle at all, enforced at the application layer
-- before every insert into role_access_pages and user_page_access below.
CREATE TABLE pages (
    key        text    PRIMARY KEY,
    fleet_only boolean NOT NULL DEFAULT false
);

INSERT INTO pages (key, fleet_only) VALUES
    ('overview',   true),
    ('nodes',      true),
    ('containers', false),
    ('processes',  false),
    ('services',   false),
    ('cron',       false),
    ('ports',      false),
    ('disks',      false),
    ('users',      true);

COMMENT ON TABLE pages IS
    'The fixed set of gateable frontend pages — see internal/core/pageauthz.KnownPages, web/src/shell/pages.ts.';

-- A named, reusable bundle of pages (e.g. "Container Related" -> containers,
-- ports, disks). Assigning a bundle to a user (user_role_access below)
-- grants every page it contains, at one scope.
CREATE TABLE role_access (
    name       text        PRIMARY KEY,
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by text
);

-- A bundle's membership. Never contains a fleet_only page — see this
-- migration's header: a bundle's own assignment carries one scope applied
-- to every page it contains, and a fleet_only page cannot honor a
-- node-scoped assignment. Enforced in Go
-- (pageauthz.ValidateBundleMembership) before every insert here, not by a
-- CHECK constraint that would need a subquery against pages.fleet_only —
-- Postgres CHECK constraints cannot reference another table, and a trigger
-- would duplicate logic already required at the application layer to give
-- a clean, typed, client-safe rejection rather than a raw constraint-
-- violation error.
CREATE TABLE role_access_pages (
    role_access_name text NOT NULL REFERENCES role_access (name) ON DELETE CASCADE,
    page_key         text NOT NULL REFERENCES pages (key),
    PRIMARY KEY (role_access_name, page_key)
);

-- A user holding a role_access bundle, scoped to a node or fleet-wide —
-- the exact node_id-nullable-means-fleet-wide shape user_node_roles already
-- established (migrations/0012_users.sql), not a different convention.
CREATE TABLE user_role_access (
    id               text        PRIMARY KEY,
    user_id          text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role_access_name text        NOT NULL REFERENCES role_access (name),
    node_id          text,
    granted_at       timestamptz NOT NULL DEFAULT now(),
    granted_by       text,
    revoked_at       timestamptz,
    revoked_by       text
);

CREATE INDEX idx_user_role_access_lookup ON user_role_access (user_id) WHERE revoked_at IS NULL;

CREATE UNIQUE INDEX idx_user_role_access_unique_active
    ON user_role_access (user_id, role_access_name, COALESCE(node_id, ''))
    WHERE revoked_at IS NULL;

COMMENT ON TABLE user_role_access IS
    'A user holding a role_access bundle, scoped to one node or (node_id NULL) fleet-wide. Mirrors user_node_roles''s shape for the page-access axis.';

-- A page granted directly to a user, independent of any role_access bundle.
-- Same node_id-nullable convention. See pageauthz.HasConflict: creating one
-- of these is refused when an active user_role_access grant already covers
-- the same page for an overlapping scope — a data-hygiene rule enforced in
-- internal/storage/pageauthz.Repository.GrantPageAccess, not by a
-- constraint here (the check spans two tables and needs the scope-overlap
-- algorithm, not equality).
CREATE TABLE user_page_access (
    id         text        PRIMARY KEY,
    user_id    text        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    page_key   text        NOT NULL REFERENCES pages (key),
    node_id    text,
    granted_at timestamptz NOT NULL DEFAULT now(),
    granted_by text,
    revoked_at timestamptz,
    revoked_by text
);

CREATE INDEX idx_user_page_access_lookup ON user_page_access (user_id) WHERE revoked_at IS NULL;

CREATE UNIQUE INDEX idx_user_page_access_unique_active
    ON user_page_access (user_id, page_key, COALESCE(node_id, ''))
    WHERE revoked_at IS NULL;

COMMENT ON TABLE user_page_access IS
    'A page granted directly to a user, independent of any role_access bundle, scoped to one node or (node_id NULL) fleet-wide.';
