-- 0018_node_hardware_uuid.sql
--
-- The machine's raw hardware identifier: the SMBIOS/DMI product UUID on
-- Linux, the IOPlatformUUID on macOS. Distinct from node_id, which is
-- HMAC-derived from /etc/machine-id (see internal/platform/hostid): this is
-- the unhashed hardware-rooted value, kept so an operator can cross-reference
-- a node against a cloud provider's instance inventory, dmidecode records, or
-- a CMDB that keys on the same UUID.
--
-- Nullable with no default. The agent reads it directly from the host and
-- deliberately does NOT fall back to machine-id or boot_id when it cannot
-- (an unprivileged agent on Linux cannot read product_uuid; a container or
-- non-x86 host has none) -- an unknown hardware UUID is recorded as unknown,
-- never faked from another identifier.
--
-- Stored in the clear, unlike node_id's machine-id input. The trade-off was
-- made deliberately: the value's usefulness is precisely that it matches what
-- an external system already holds, which a hash would defeat. It is a stable
-- cross-system correlator and carries the corresponding privacy consideration
-- (it sits behind the same node.read permission as every other node fact).

ALTER TABLE nodes ADD COLUMN hardware_uuid text;

COMMENT ON COLUMN nodes.hardware_uuid IS
    'Raw hardware UUID (SMBIOS/DMI product UUID on Linux, IOPlatformUUID on macOS). Agent-read, never derived from machine-id. Null when the host cannot supply one.';
