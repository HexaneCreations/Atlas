-- 0023_install_codes.sql
--
-- One-time install codes for the "Add server" flow. An admin creates a code
-- in the UI; the server pre-assigns the new machine's node_id at that moment.
-- The installer later presents the code together with the agent's freshly
-- generated libp2p Peer ID, and redemption authorizes exactly that Peer ID
-- for exactly that node_id (agent_peers), in one transaction.
--
-- A new table rather than new columns on enrollment_tokens: those tokens
-- belong to the HTTPS/mTLS enroll flow (multi-use, CIDR-bound, consumed by
-- certificate issuance and referenced by node_credentials.enrolled_via), and
-- that flow stays untouched. An install code is single-use, carries a
-- pre-assigned node_id, and ends in an agent_peers row, not a certificate.
--
-- Only the SHA-256 of the code is stored; the plaintext is returned once, at
-- creation. The row is also the audit record: who created it and when, who
-- revoked it, and which Peer ID from which address redeemed it.

CREATE TABLE install_codes (
    id               text        PRIMARY KEY,
    code_hash        text        NOT NULL UNIQUE,
    -- Unique across every code ever issued, redeemed or not: a node_id is
    -- handed to exactly one install.
    node_id          text        NOT NULL UNIQUE,
    environment      text        NOT NULL,
    -- The creating user's id. Plain text, the same convention as
    -- user_node_roles.granted_by.
    created_by       text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    revoked_at       timestamptz,
    revoked_by       text,
    redeemed_at      timestamptz,
    redeemed_peer_id text,
    redeemed_ip      inet,

    CONSTRAINT install_codes_code_hash_sha256 CHECK (code_hash ~ '^[0-9a-f]{64}$'),
    CONSTRAINT install_codes_node_id_hex CHECK (node_id ~ '^[0-9a-f]{32}$'),
    CONSTRAINT install_codes_environment_allowed CHECK (environment ~ '^[A-Za-z0-9._-]{1,64}$'),
    CONSTRAINT install_codes_ttl_bounded CHECK (
        expires_at > created_at AND expires_at <= created_at + interval '60 minutes'),
    CONSTRAINT install_codes_revoked_or_redeemed CHECK (revoked_at IS NULL OR redeemed_at IS NULL),
    CONSTRAINT install_codes_redeemed_complete CHECK (
        (redeemed_at IS NULL) = (redeemed_peer_id IS NULL))
);

COMMENT ON TABLE install_codes IS
    'One-time install codes: each pre-assigns a node_id and, when redeemed, authorizes one libp2p Peer ID for it in agent_peers. Only the SHA-256 of the code is stored.';

CREATE INDEX idx_install_codes_created_at ON install_codes (created_at DESC);

-- fleet.enroll gates creating, listing and revoking install codes. admin and
-- superadmin only: an install code admits a new machine to the fleet, which
-- is more than operator's configuration writes (fleet.write). Checked
-- fleet-wide (empty node id), like user.manage.
INSERT INTO permissions (key) VALUES ('fleet.enroll')
ON CONFLICT (key) DO NOTHING;

INSERT INTO role_permissions (role_name, permission_key) VALUES
    ('admin',      'fleet.enroll'),
    ('superadmin', 'fleet.enroll')
ON CONFLICT (role_name, permission_key) DO NOTHING;
