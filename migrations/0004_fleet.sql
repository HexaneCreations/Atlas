-- 0004_fleet.sql
--
-- Storage for the Atlas Agent milestone: enrollment tokens, issued
-- credentials, ingest idempotency, and latest-only inventory snapshots. See
-- docs/architecture/agent-design.md for the design this schema implements.

-- ------------------------------------------------------------- nodes ------

-- Clock skew, surfaced per node (readiness review H6). A host with a broken
-- clock silently corrupts its own series; this is what lets an operator find
-- out which host that is instead of everyone's dashboards being subtly wrong.
ALTER TABLE nodes ADD COLUMN clock_skew_seconds double precision;

COMMENT ON COLUMN nodes.clock_skew_seconds IS
    'Agent-reported send time minus control-plane receive time, from the most recent ingest. Null until an agent has pushed.';

-- ------------------------------------------------------- enrollment_tokens ----

-- A bounded credential an operator hands to provisioning, not to an agent
-- directly. See docs/architecture/agent-design.md §4: every column here
-- bounds the blast radius of a leaked token, because the token itself is the
-- only proof of identity Atlas has before a certificate exists.
CREATE TABLE enrollment_tokens (
    -- The token is shown to the operator exactly once and never stored. Only
    -- its hash is kept, the same treatment credentials get everywhere else in
    -- the platform.
    token_hash    text        PRIMARY KEY,
    -- A label for the operator's own bookkeeping ("provisioning script",
    -- "staging autoscaler"). Not interpreted by Atlas.
    label         text        NOT NULL DEFAULT '',
    -- The environment a node enrolling with this token is tagged with. Every
    -- node enrolled through one token shares an environment, because the
    -- token is provisioned for one deployment context.
    environment   text,
    -- CIDR restricting which source addresses may redeem this token. Null
    -- means unrestricted, which an operator must set explicitly — there is no
    -- implicit "any network" default silently baked into token creation.
    allowed_cidr  cidr,
    max_uses      integer     NOT NULL,
    uses_remaining integer    NOT NULL,
    expires_at    timestamptz NOT NULL,
    -- Explicit grant to re-enroll a node id that already holds a live
    -- certificate. Off by default: without it, enrollment for an
    -- already-enrolled node id is refused outright (see
    -- docs/architecture/agent-design.md sec 4), which is what stops a stolen
    -- token from taking over an existing node's identity. An operator sets
    -- this only for a deliberate re-provisioning token.
    allow_reenroll boolean    NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    -- Set the moment the token is administratively revoked, distinct from
    -- exhaustion (uses_remaining = 0) or expiry (expires_at passed). All three
    -- make the token unusable; this column records which one an operator
    -- chose.
    revoked_at    timestamptz,
    CONSTRAINT enrollment_tokens_max_uses_positive CHECK (max_uses > 0),
    CONSTRAINT enrollment_tokens_uses_remaining_bounded
        CHECK (uses_remaining >= 0 AND uses_remaining <= max_uses)
);

COMMENT ON TABLE enrollment_tokens IS
    'Bounded, single-purpose credentials used once per host to bootstrap a node certificate. See docs/architecture/agent-design.md sec 4.';

-- Expired and exhausted tokens accumulate; this index is what makes "list
-- tokens still worth showing an operator" cheap without a table scan.
CREATE INDEX idx_enrollment_tokens_expires_at ON enrollment_tokens (expires_at);

-- --------------------------------------------------------- node_credentials ----

-- One row per certificate Atlas has ever issued. Unlike enrollment_tokens
-- this is a durable record, not a bounded consumable: it is the fleet's
-- certificate history, and it is what enrollment's re-issue check (refuse
-- enrolling an already-active node id) and the audit trail both read.
CREATE TABLE node_credentials (
    -- The certificate's serial number, hex-encoded (see pki.Fingerprint).
    -- Stable per issuance, so a renewed certificate for the same node gets
    -- its own row rather than overwriting history.
    fingerprint   text        PRIMARY KEY,
    node_id       text        NOT NULL,
    issued_at     timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    -- The token that authorised this issuance. Null for a renewal, which
    -- authenticates with the certificate being renewed rather than a token —
    -- see docs/architecture/agent-design.md sec 4: "no token involved."
    enrolled_via  text        REFERENCES enrollment_tokens (token_hash),
    revoked_at    timestamptz,
    revoked_reason text
);

COMMENT ON TABLE node_credentials IS
    'Every certificate Atlas has issued. Drives the re-enrollment refusal that prevents identity takeover, and the denylist for out-of-band ejection.';

-- "Does this node already hold a live certificate" is the re-enrollment
-- check on every enrollment attempt, and it must be cheap: it runs on the
-- hot path of a fleet bootstrapping at once.
CREATE INDEX idx_node_credentials_node_active
    ON node_credentials (node_id, expires_at DESC) WHERE revoked_at IS NULL;

-- ------------------------------------------------------------ node_denylist ----

-- Ejection that cannot wait for a 24-hour certificate to expire (readiness
-- review M4). Checked at every TLS handshake alongside chain verification.
CREATE TABLE node_denylist (
    node_id    text        PRIMARY KEY,
    reason     text        NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE node_denylist IS
    'Node ids refused at connect regardless of certificate validity. The out-of-band revocation path short-lived certificates otherwise do not need.';

-- --------------------------------------------------------- ingested_envelopes ----

-- Deduplication record for at-least-once delivery (readiness review C2). A
-- retry after a timeout where the write actually succeeded must not
-- duplicate the batch; a conflict on this primary key is how the ingest path
-- recognises "already applied" and commits nothing a second time.
CREATE TABLE ingested_envelopes (
    envelope_id  text        PRIMARY KEY,
    node_id      text        NOT NULL,
    kind         text        NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE ingested_envelopes IS
    'One row per envelope ID ever accepted, written in the same transaction as its payload. Bounds retry duplication; pruned by the control plane to the transport''s maximum retry horizon.';

-- This is deliberately a plain table, not a hypertable. A hypertable's
-- uniqueness constraints must include the partitioning column, which here
-- would mean the primary key becomes (envelope_id, received_at) instead of
-- envelope_id alone — and a retried envelope arrives with a *different*
-- received_at than the original, so that composite key would let the retry
-- insert a second row for the same envelope_id, silently defeating the
-- dedup this table exists to provide. Correctness here matters more than the
-- partitioned-drop convenience metric_samples gets; see
-- internal/app/fleet.go for the periodic delete that keeps this table
-- bounded instead.
--
-- 24 hours comfortably covers any transport's retry horizon (see
-- docs/architecture/agent-design.md sec 3, C2). An envelope older than the
-- window is rejected outright by the ingest handler rather than risked as a
-- duplicate — see internal/api/agent.
CREATE INDEX idx_ingested_envelopes_received_at ON ingested_envelopes (received_at);

-- ------------------------------------------------------- inventory_snapshots ----

-- Latest-only inventory, one row per (node, subject). Replaced on arrival,
-- never appended to: see docs/architecture/agent-design.md sec 1 for why
-- inventory is not stored as history the way metrics are. "subject" is a
-- plugin-scoped name such as "docker.containers" or "service.units".
CREATE TABLE inventory_snapshots (
    node_id      text        NOT NULL,
    subject      text        NOT NULL,
    -- When the *host* was observed, not when the control plane received the
    -- push. This is the timestamp every "as of Ns ago" freshness indicator in
    -- the UI reads (transport.Snapshot.ObservedAt).
    observed_at  timestamptz NOT NULL,
    received_at  timestamptz NOT NULL DEFAULT now(),
    -- A content hash lets an agent skip re-sending an unchanged snapshot; the
    -- control plane still refreshes received_at on a hash-only push so
    -- liveness reflects it. See docs/architecture/agent-design.md sec 5.
    content_hash text        NOT NULL,
    data         jsonb       NOT NULL,
    PRIMARY KEY (node_id, subject)
);

COMMENT ON TABLE inventory_snapshots IS
    'The current state of each inventory subject per node, replaced in place. Never queried for history; see metric_samples for anything that needs a time series.';
