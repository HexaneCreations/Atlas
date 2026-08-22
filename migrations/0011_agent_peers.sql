-- 0011_agent_peers.sql
--
-- Authorization of a libp2p Peer ID to act as a given node, in a given
-- environment. See docs/adr/0012-connect-by-identity.md.
--
-- The libp2p Noise handshake already proves *which* Peer ID is on the other
-- end of a stream, cryptographically, before any Atlas byte is exchanged.
-- What it cannot say is whether that peer is one Atlas wants to hear from,
-- or which machine it is. This table answers exactly those two questions,
-- and nothing else:
--
--   peer_id     -- transport identity, proven by Noise, never self-asserted
--   node_id     -- stable machine identity, for database association
--
-- The direction of the binding matters. A row authorizes a Peer ID *to be*
-- a node; a node_id presented by an agent is never used to look up or admit
-- a Peer ID. Without that direction, anyone who learned a node_id (it is in
-- every inventory payload, not a secret) could enroll a fresh keypair
-- against it. An operator registers the Peer ID explicitly, out of band, the
-- same way an enrollment token was handed out explicitly before it.

CREATE TABLE agent_peers (
    -- The base58 libp2p Peer ID, exactly as the Noise handshake reports it.
    -- Primary key: one Peer ID authorizes at most one (node, environment)
    -- binding, so a compromised or retired keypair is revoked in one place.
    peer_id     text        PRIMARY KEY,
    -- The machine identity this peer speaks for (see hostid.Resolve). Not
    -- unique: re-registering a rebuilt host with a fresh keypair means a new
    -- row for the same node_id, and the old one is revoked rather than
    -- silently overwritten, so the audit trail survives the rotation.
    node_id     text        NOT NULL,
    -- The relationship/environment this authorization is scoped to. A peer
    -- authorized for development is not thereby authorized for production,
    -- even for the same machine.
    environment text        NOT NULL,
    -- Reserved for future differentiation between kinds of peer (agent,
    -- relay, another control plane). Only 'agent' is accepted today; the
    -- column exists so adding a second role is a value, not a migration.
    role        text        NOT NULL DEFAULT 'agent',
    -- 'active' or 'revoked'. Revocation is a status change, not a delete:
    -- the row is the record that this keypair was once trusted, and by whom.
    status      text        NOT NULL DEFAULT 'active',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT agent_peers_peer_id_not_blank CHECK (peer_id <> ''),
    CONSTRAINT agent_peers_node_id_not_blank CHECK (node_id <> ''),
    CONSTRAINT agent_peers_environment_not_blank CHECK (environment <> ''),
    CONSTRAINT agent_peers_role_known CHECK (role IN ('agent')),
    CONSTRAINT agent_peers_status_known CHECK (status IN ('active', 'revoked'))
);

COMMENT ON TABLE agent_peers IS
    'Authorization of a libp2p Peer ID to act as a node in an environment. Authentication is the Noise handshake; this table is the authorization, and the two are deliberately separate.';

-- "Which peers speak for this node?" is the operator-facing question during
-- rotation and revocation, and the one ContainerLogs asks in reverse.
CREATE INDEX idx_agent_peers_node_id ON agent_peers (node_id, status);
