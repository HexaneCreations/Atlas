#!/usr/bin/env bash
# One-shot cutover of the atlas-agent on cyrene-dev-v2 from its bespoke
# root-owned install to the canonical packaging:
#
#   - data dir      <old, e.g. /root/atlas-agent>  ->  /var/lib/atlas-agent
#   - service user  root                           ->  atlas (unprivileged)
#   - unit / env    bespoke                        ->  packaging/atlas-agent/*
#
# The libp2p Peer ID, the relationship state, and every relationship.json
# are preserved BYTE-FOR-BYTE. Nothing old is moved or deleted: the old
# data dir, unit and env file are only ever copied. If any post-flight
# check fails the script stops, restores the old unit + env, restarts the
# old service, and prints exactly what did not match.
#
# Usage (on the box, as root):
#   migrate-cyrene-dev-v2.sh --agent-binary /path/to/atlas-agent-linux-amd64
#
# The fast rollback gate (steps 8a-8e) never depends on a live-connection
# log: the migration's guarantee is a byte-identical identity key and
# relationship.json, plus "$PRIMARY_ID" reaching its dial path with no
# bootstrap error and the service staying active. A real libp2p connection
# is not observable for ~5 min on this box (the undisablable "default"
# relationship blocks the agent's Run() for the full 5-minute https
# bootstrap retry against a dead 127.0.0.1:8443), so confirming it is a
# separate, non-blocking step — see --confirm-connect.
#
# Options:
#   --agent-binary PATH   New, version-stamped binary to install (required
#                          except with --confirm-connect)
#   --new-data-dir DIR    Default /var/lib/atlas-agent
#   --run-user NAME       Default atlas
#   --unit NAME           Default atlas-agent
#   --old-data-dir DIR    Override auto-detection of the current data dir
#   --old-env-file PATH   Default /etc/atlas-agent/atlas-agent.env
#   --backup-root DIR     Default /var/backups/atlas-agent
#   --packaging-dir DIR   Where install.sh + atlas-agent.service live
#                          (default: next to this script)
#   --wait-for-connect N  On the main run: after committing, poll the journal
#                          inline for up to N seconds for a real libp2p
#                          connection to "$PRIMARY_ID"'s server. Off by
#                          default — the main run exits promptly and prints
#                          the command to check later. Never affects rollback.
#   --confirm-connect     Do ONLY the deferred connection check: poll the
#                          journal (up to --wait-for-connect seconds, default
#                          420) for "libp2p direct connection established" to
#                          the migrated relationship's server peer id. Reads
#                          the peer id from --server-peer-id or from
#                          <new-data-dir>/relationships/<id>/relationship.json.
#                          Exit 0 whether or not it is seen; non-zero only if
#                          the service is no longer active.
#   --server-peer-id ID   --confirm-connect: the server peer id to look for
#                          (otherwise read from relationship.json).
#   --dry-run             Off-box rehearsal. Still performs local file copies
#                          into the given --new-data-dir / --backup-root and
#                          runs the identity / peer-id / relationship.json
#                          equality checks. Prints and skips every systemctl /
#                          chown / useradd / install.sh call. Point
#                          --new-data-dir / --backup-root at scratch paths.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

AGENT_BINARY=""
NEW_DATA_DIR="/var/lib/atlas-agent"
RUN_USER="atlas"
UNIT="atlas-agent"
OLD_DATA_DIR=""
OLD_ENV_FILE="/etc/atlas-agent/atlas-agent.env"
BACKUP_ROOT="/var/backups/atlas-agent"
PACKAGING_DIR="$SCRIPT_DIR"
DRY_RUN=0
MODE=migrate
WAIT_FOR_CONNECT=0
SERVER_PEER_ID=""

while [ $# -gt 0 ]; do
  case "$1" in
    --agent-binary) AGENT_BINARY="$2"; shift 2 ;;
    --new-data-dir) NEW_DATA_DIR="$2"; shift 2 ;;
    --run-user) RUN_USER="$2"; shift 2 ;;
    --unit) UNIT="$2"; shift 2 ;;
    --old-data-dir) OLD_DATA_DIR="$2"; shift 2 ;;
    --old-env-file) OLD_ENV_FILE="$2"; shift 2 ;;
    --backup-root) BACKUP_ROOT="$2"; shift 2 ;;
    --packaging-dir) PACKAGING_DIR="$2"; shift 2 ;;
    --wait-for-connect) WAIT_FOR_CONNECT="$2"; shift 2 ;;
    --confirm-connect) MODE=confirm-connect; shift ;;
    --server-peer-id) SERVER_PEER_ID="$2"; shift 2 ;;
    --dry-run) DRY_RUN=1; shift ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "migrate: unknown option $1" >&2; exit 1 ;;
  esac
done

die()  { echo "migrate: $*" >&2; exit 1; }
info() { echo "==> $*"; }
step() { echo; echo "### $*"; }

case "$WAIT_FOR_CONNECT" in ''|*[!0-9]*) die "--wait-for-connect must be a whole number of seconds" ;; esac

# server_peer_id_from RELATIONSHIP_JSON — prints the server's libp2p peer id,
# from the persisted libp2p_server_peer_id, else the /p2p/ tail of
# libp2p_server_addr.
server_peer_id_from() {
  local f="$1" v=""
  [ -f "$f" ] || return 0
  if command -v python3 >/dev/null 2>&1; then
    v="$(python3 -c 'import json,sys;d=json.load(open(sys.argv[1]));print(d.get("libp2p_server_peer_id") or "")' "$f" 2>/dev/null || true)"
    [ -z "$v" ] && v="$(python3 -c 'import json,sys;a=json.load(open(sys.argv[1])).get("libp2p_server_addr","");print(a.rsplit("/p2p/",1)[1] if "/p2p/" in a else "")' "$f" 2>/dev/null || true)"
  else
    v="$(grep -oE '"libp2p_server_peer_id"[[:space:]]*:[[:space:]]*"[^"]*"' "$f" | sed -E 's/.*"([^"]*)"$/\1/' | head -1)"
    [ -z "$v" ] && v="$(grep -oE '"libp2p_server_addr"[[:space:]]*:[[:space:]]*"[^"]*"' "$f" | sed -E 's#.*/p2p/([^"/]*)".*#\1#' | head -1)"
  fi
  printf '%s' "$v"
}

# poll_for_connection PEER_ID SINCE BUDGET_SECONDS
# Polls the unit journal every 15 s for a non-relayed libp2p connection to
# PEER_ID. Returns 0 if seen, 1 on timeout. Never exits the script.
poll_for_connection() {
  local peer="$1" since="$2" budget="$3" waited=0 line=""
  local since_args=()
  [ -n "$since" ] && since_args=(--since "$since")
  local nap
  while :; do
    line="$(journalctl -u "$UNIT" "${since_args[@]}" --no-pager 2>/dev/null \
      | grep -F 'libp2p direct connection established' | grep -F "\"peer\":\"$peer\"" | tail -1 || true)"
    if [ -n "$line" ]; then
      info "confirmed: direct libp2p connection to $peer"
      echo "    $line"
      return 0
    fi
    [ "$waited" -ge "$budget" ] && return 1
    nap=15
    [ $((budget - waited)) -lt 15 ] && nap=$((budget - waited))
    sleep "$nap"
    waited=$((waited + nap))
  done
}

# run_priv: privileged / systemd / user-db mutations that cannot run off-box.
# Printed and skipped under --dry-run.
run_priv() {
  if [ "$DRY_RUN" -eq 1 ]; then echo "  [dry-run] $*"; else "$@"; fi
}

# run_fs: filesystem copies into caller-supplied paths. These DO run under
# --dry-run (into the scratch --new-data-dir / --backup-root) so the identity
# and relationship.json equality checks in step 8 actually execute.
run_fs() {
  if [ "$DRY_RUN" -eq 1 ]; then echo "  [fs] $*"; fi
  "$@"
}

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

json_canon() {
  # stable, key-sorted rendering for byte-comparison of two relationship.json
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import json,sys; print(json.dumps(json.load(open(sys.argv[1])),sort_keys=True,indent=2))' "$1"
  elif command -v jq >/dev/null 2>&1; then
    jq -S . "$1"
  else
    cat "$1"   # fall back to a raw compare; install.sh writes deterministically
  fi
}

# -------------------------------------------------------------------------
# --confirm-connect: deferred, non-blocking connection check only.
# -------------------------------------------------------------------------
if [ "$MODE" = confirm-connect ]; then
  [ "$WAIT_FOR_CONNECT" -gt 0 ] || WAIT_FOR_CONNECT=420
  command -v systemctl >/dev/null 2>&1 || die "systemctl not found"

  peer="$SERVER_PEER_ID"
  if [ -z "$peer" ]; then
    for rj in "$NEW_DATA_DIR"/relationships/*/relationship.json; do
      [ -f "$rj" ] || continue
      peer="$(server_peer_id_from "$rj")"
      [ -n "$peer" ] && break
    done
  fi
  [ -n "$peer" ] || die "could not determine the server peer id (pass --server-peer-id)"

  if ! systemctl is-active --quiet "$UNIT"; then
    die "$UNIT is not active — nothing to confirm (this IS a problem, investigate)"
  fi

  since="$(systemctl show -p ActiveEnterTimestamp --value "$UNIT" 2>/dev/null || true)"
  info "polling the journal for a direct libp2p connection to $peer (up to ${WAIT_FOR_CONNECT}s)"
  if poll_for_connection "$peer" "$since" "$WAIT_FOR_CONNECT"; then
    exit 0
  fi
  cat <<EOF

==> Not yet observed after ${WAIT_FOR_CONNECT}s. This is NOT a failure on its own:
    - the identity key and relationship.json were verified byte-identical
      during the migration (steps 8a-8c);
    - the agent's Run() — and therefore its first dial — is delayed ~5 min
      every start by the "default" relationship's bootstrap retry.
    Keep watching, and check the control plane shows this node reporting:
        journalctl -u $UNIT -f | grep 'direct connection established'
EOF
  exit 0
fi

[ -n "$AGENT_BINARY" ] || die "--agent-binary is required"
[ -x "$AGENT_BINARY" ] || die "not executable: $AGENT_BINARY"

if [ "$DRY_RUN" -eq 0 ]; then
  [ "$(id -u)" -eq 0 ] || die "must run as root (try sudo)"
  command -v systemctl >/dev/null 2>&1 || die "systemctl not found — this script targets a systemd host"
fi

# --- binary sanity: refuse an unstamped build ------------------------------
ver="$("$AGENT_BINARY" --version 2>/dev/null || true)"
[ -n "$ver" ] || die "--agent-binary --version produced no output"
printf '%s' "$ver" | grep -qE 'dev|-dirty|unknown' && die "refusing an unstamped binary: $ver"
info "new binary: $ver"

# =========================================================================
step "1. Pre-flight snapshot"
# =========================================================================

# 1a. resolve the CURRENT data dir from the live unit / env file, unless
#     the operator pinned it explicitly.
if [ -z "$OLD_DATA_DIR" ]; then
  cand=""
  if [ "$DRY_RUN" -eq 0 ]; then
    envblob="$(systemctl show -p Environment --value "$UNIT" 2>/dev/null || true)"
    cand="$(printf '%s\n' "$envblob" | tr ' ' '\n' | sed -n 's/^ATLAS_AGENT_DATA_DIR=//p' | head -1)"
  fi
  if [ -z "$cand" ] && [ -f "$OLD_ENV_FILE" ]; then
    cand="$(sed -n 's/^ATLAS_AGENT_DATA_DIR=//p' "$OLD_ENV_FILE" | head -1)"
  fi
  [ -n "$cand" ] || cand="/var/lib/atlas-agent"
  OLD_DATA_DIR="$cand"
fi
info "current data dir: $OLD_DATA_DIR"
[ "$OLD_DATA_DIR" != "$NEW_DATA_DIR" ] || die "old and new data dir are identical ($OLD_DATA_DIR) — nothing to migrate"

OLD_KEY="$OLD_DATA_DIR/p2p-identity.key"
[ -f "$OLD_KEY" ] || die "no identity at $OLD_KEY — pass --old-data-dir if the current data dir is elsewhere"

# 1b. relationship set from the live env file, each relationship.json present + parses
RELS=""
if [ -f "$OLD_ENV_FILE" ]; then
  RELS="$(sed -n 's/^ATLAS_AGENT_RELATIONSHIPS=//p' "$OLD_ENV_FILE" | head -1 | tr ',' ' ')"
fi
[ -n "$RELS" ] || die "no ATLAS_AGENT_RELATIONSHIPS in $OLD_ENV_FILE (this migration is for the named-relationship layout)"
info "relationships: $RELS"

for id in $RELS; do
  rj="$OLD_DATA_DIR/relationships/$id/relationship.json"
  [ -f "$rj" ] || die "missing $rj"
  json_canon "$rj" >/dev/null 2>&1 || die "does not parse: $rj"
  info "  $id: $rj OK"
done

# 1c. current Peer ID + key hash — the "before" the whole migration is judged against
OLD_PEER_ID="$(ATLAS_AGENT_DATA_DIR="$OLD_DATA_DIR" "$AGENT_BINARY" peer-id 2>/dev/null || true)"
printf '%s' "$OLD_PEER_ID" | grep -qE '^12D3Koo|^Qm' || die "could not read current Peer ID from $OLD_DATA_DIR"
OLD_KEY_SHA="$(sha256 "$OLD_KEY")"
info "current Peer ID: $OLD_PEER_ID"
info "identity sha256: $OLD_KEY_SHA"

# read the control-plane URL / addresses / environment straight out of the
# authoritative relationship.json so install.sh writes an env file that is
# consistent with the file that actually governs.
PRIMARY_ID="$(printf '%s' "$RELS" | awk '{print $1}')"
PR_JSON="$OLD_DATA_DIR/relationships/$PRIMARY_ID/relationship.json"
rj_get() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import json,sys; print(json.load(open(sys.argv[1])).get(sys.argv[2],""))' "$PR_JSON" "$1"
  else
    grep -oE "\"$1\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$PR_JSON" | sed -E 's/.*"([^"]*)"$/\1/' | head -1
  fi
}
CP_URL="$(rj_get control_plane_url)"
CP_TRANSPORT="$(rj_get transport)"
CP_ENV="$(rj_get environment)"
CP_SERVER_ADDR="$(rj_get libp2p_server_addr)"
CP_RELAY_ADDR="$(rj_get libp2p_relay_addr)"
CP_SERVER_PID="$(rj_get libp2p_server_peer_id)"
[ "$CP_TRANSPORT" = "libp2p" ] || die "relationship '$PRIMARY_ID' transport is '$CP_TRANSPORT', expected libp2p"
info "primary relationship '$PRIMARY_ID': $CP_URL ($CP_TRANSPORT)"

# 1d. OLD agent's telemetry health RIGHT NOW — recorded, never blocking. The
# only signal available is negative: remote.go logs delivery FAILURES
# ("telemetry delivery failed, will retry"), never successes. So the
# baseline is: recent failure count + the spool backlog for "$PRIMARY_ID".
# This is the honest "was it connected before we touched it" reference to
# compare against if --confirm-connect later comes up empty.
OLD_SPOOL_DIR="$OLD_DATA_DIR/relationships/$PRIMARY_ID/spool"
OLD_SPOOL_FILES=0
OLD_SPOOL_KB=0
if [ -d "$OLD_SPOOL_DIR" ]; then
  OLD_SPOOL_FILES="$(find "$OLD_SPOOL_DIR" -type f 2>/dev/null | wc -l | tr -d ' ')"
  OLD_SPOOL_KB="$(du -sk "$OLD_SPOOL_DIR" 2>/dev/null | awk '{print $1}')"
fi
OLD_TELE_FAILS_15M="n/a"
if [ "$DRY_RUN" -eq 0 ]; then
  OLD_TELE_FAILS_15M="$(journalctl -u "$UNIT" --since '-15 min' --no-pager 2>/dev/null \
    | grep -c 'telemetry delivery failed' || true)"
fi
if [ "$DRY_RUN" -eq 1 ]; then
  OLD_TELE_BASELINE="unknown-dryrun"
elif [ "$OLD_TELE_FAILS_15M" = "0" ] && [ "${OLD_SPOOL_FILES:-0}" -lt 20 ]; then
  OLD_TELE_BASELINE="healthy"
else
  OLD_TELE_BASELINE="degraded"
fi
info "old telemetry baseline: $OLD_TELE_BASELINE (spool_files=$OLD_SPOOL_FILES, spool_kb=$OLD_SPOOL_KB, fails_15m=$OLD_TELE_FAILS_15M)"

BACKUP_DIR="$BACKUP_ROOT/$(date -u +%Y%m%dT%H%M%SZ)"
info "backup dir: $BACKUP_DIR"

# Any failure AFTER the old service is stopped funnels through cleanup(),
# which restores the backed-up unit + env (if step 6 had already replaced
# them) and restarts the old service. The old data dir is never touched, so
# rollback is genuinely "put the old unit/env back and start it".
OLD_UNIT_PATH=""
STOPPED_OLD=0
SUCCESS=0

rollback_impl() {
  echo
  echo "!!! MIGRATION FAILED — rolling back to the previous service"
  systemctl stop "$UNIT" 2>/dev/null || true
  if [ -n "$OLD_UNIT_PATH" ] && [ -f "$BACKUP_DIR/$(basename "$OLD_UNIT_PATH")" ]; then
    cp -a "$BACKUP_DIR/$(basename "$OLD_UNIT_PATH")" "$OLD_UNIT_PATH"
  fi
  if [ -f "$BACKUP_DIR/atlas-agent.env" ]; then
    cp -a "$BACKUP_DIR/atlas-agent.env" "$OLD_ENV_FILE"
  fi
  systemctl daemon-reload || true
  systemctl start "$UNIT" || true
  echo
  echo "Rolled back. The old service, config and data ($OLD_DATA_DIR) are"
  echo "startable. Nothing new was enabled. Backup kept at: $BACKUP_DIR"
  if systemctl is-active --quiet "$UNIT"; then
    echo "The old $UNIT is active again."
  else
    echo "WARNING: could not restart $UNIT automatically. Start it by hand:"
    echo "    systemctl start $UNIT"
  fi
}

cleanup() {
  local rc=$?
  trap - EXIT
  [ "$SUCCESS" -eq 1 ] && exit 0
  if [ "$DRY_RUN" -eq 1 ] || [ "$STOPPED_OLD" -eq 0 ]; then
    # failed before the service was touched (or a dry run) — nothing to undo
    exit "$rc"
  fi
  rollback_impl
  exit 1
}
trap cleanup EXIT

# =========================================================================
step "2. Stop the current service"
# =========================================================================
if [ "$DRY_RUN" -eq 0 ]; then
  systemctl stop "$UNIT"
  for _ in 1 2 3 4 5; do systemctl is-active --quiet "$UNIT" || break; sleep 1; done
  systemctl is-active --quiet "$UNIT" && die "could not stop $UNIT"
  STOPPED_OLD=1
fi
info "$UNIT stopped"

# =========================================================================
step "3. Back up the current data dir, unit and env (copy, not move)"
# =========================================================================
run_fs install -d -m 0700 "$BACKUP_DIR"
run_fs cp -a "$OLD_DATA_DIR" "$BACKUP_DIR/data"
[ -f "$OLD_ENV_FILE" ] && run_fs cp -a "$OLD_ENV_FILE" "$BACKUP_DIR/atlas-agent.env"
if [ "$DRY_RUN" -eq 0 ]; then
  OLD_UNIT_PATH="$(systemctl show -p FragmentPath --value "$UNIT" 2>/dev/null || true)"
  if [ -n "$OLD_UNIT_PATH" ] && [ -f "$OLD_UNIT_PATH" ]; then
    cp -a "$OLD_UNIT_PATH" "$BACKUP_DIR/$(basename "$OLD_UNIT_PATH")"
    dropdir="$(dirname "$OLD_UNIT_PATH")/${UNIT}.service.d"
    [ -d "$dropdir" ] && cp -a "$dropdir" "$BACKUP_DIR/dropins"
  fi
fi
{
  echo "old_data_dir=$OLD_DATA_DIR"
  echo "old_env_file=$OLD_ENV_FILE"
  echo "old_unit_path=$OLD_UNIT_PATH"
  echo "peer_id=$OLD_PEER_ID"
  echo "identity_sha256=$OLD_KEY_SHA"
  echo "relationships=$RELS"
  echo "primary_relationship=$PRIMARY_ID"
  echo "server_peer_id=${CP_SERVER_PID:-$(printf '%s' "$CP_SERVER_ADDR" | sed -E 's#.*/p2p/##')}"
  echo "new_binary_version=$ver"
  echo "old_telemetry_baseline=$OLD_TELE_BASELINE"
  echo "old_spool_files=$OLD_SPOOL_FILES"
  echo "old_spool_kb=$OLD_SPOOL_KB"
  echo "old_telemetry_fails_15m=$OLD_TELE_FAILS_15M"
} > "$BACKUP_DIR/preflight.txt"

b_sha="$(sha256 "$BACKUP_DIR/data/p2p-identity.key")"
[ "$b_sha" = "$OLD_KEY_SHA" ] || die "backup identity hash mismatch ($b_sha != $OLD_KEY_SHA)"
info "backup complete; original $OLD_DATA_DIR untouched"

# =========================================================================
step "4. Create the new data dir; copy identity + relationship state + certs"
# =========================================================================
run_fs install -d -m 0700 "$NEW_DATA_DIR"
run_fs cp -a "$OLD_DATA_DIR/p2p-identity.key" "$NEW_DATA_DIR/p2p-identity.key"
if [ -d "$OLD_DATA_DIR/relationships" ]; then
  run_fs cp -a "$OLD_DATA_DIR/relationships" "$NEW_DATA_DIR/relationships"
fi
# any root-level certs / spool the bespoke layout may have kept alongside
for extra in ca-cert.pem agent-cert.pem agent-key.pem spool; do
  [ -e "$OLD_DATA_DIR/$extra" ] && run_fs cp -a "$OLD_DATA_DIR/$extra" "$NEW_DATA_DIR/$extra"
done
info "copied into $NEW_DATA_DIR (still a copy — old dir intact)"

# =========================================================================
step "5. Ownership and permissions for the '$RUN_USER' service user"
# =========================================================================
if [ "$RUN_USER" != "root" ] && [ "$DRY_RUN" -eq 0 ]; then
  if ! getent passwd "$RUN_USER" >/dev/null; then
    useradd --system --no-create-home --home-dir "$NEW_DATA_DIR" \
      --shell /usr/sbin/nologin "$RUN_USER"
  fi
fi
run_priv chown -R "$RUN_USER:$RUN_USER" "$NEW_DATA_DIR"
run_fs chmod 0700 "$NEW_DATA_DIR"
run_fs chmod 0600 "$NEW_DATA_DIR/p2p-identity.key"
info "ownership set to $RUN_USER"

# =========================================================================
step "6. Install the new binary + unit + env (install.sh --no-start)"
# =========================================================================
INSTALL_ARGS=(
  --binary "$AGENT_BINARY"
  --transport libp2p
  --relationship "$PRIMARY_ID"
  --control-plane-url "$CP_URL"
  --no-start --force-env
)
[ -n "$CP_ENV" ]         && INSTALL_ARGS+=(--environment "$CP_ENV")
if [ -n "$CP_SERVER_ADDR" ]; then
  INSTALL_ARGS+=(--server-addr "$CP_SERVER_ADDR")
else
  INSTALL_ARGS+=(--relay-addr "$CP_RELAY_ADDR" --server-peer-id "$CP_SERVER_PID")
fi
info "install.sh ${INSTALL_ARGS[*]}"
run_priv bash "$PACKAGING_DIR/install.sh" "${INSTALL_ARGS[@]}"

# =========================================================================
step "7. Start the new service"
# =========================================================================
START_TS=""
if [ "$DRY_RUN" -eq 0 ]; then
  systemctl daemon-reload
  START_TS="$(date '+%Y-%m-%d %H:%M:%S')"
  systemctl start "$UNIT"
  # "$PRIMARY_ID"'s bootstrap goroutine logs its dial marker within a second
  # or two of start (it runs concurrently with, and independent of, the
  # slow "default" retry). 8 s is slack for a loaded box.
  sleep 8
fi
info "$UNIT started"

# =========================================================================
step "8. Post-flight verification (identity + config + service)"
# =========================================================================
FAILED=""

# 8a. identity byte-identical
NEW_KEY_SHA="$(sha256 "$NEW_DATA_DIR/p2p-identity.key")"
if [ "$NEW_KEY_SHA" = "$OLD_KEY_SHA" ]; then
  info "8a OK  identity key byte-identical ($NEW_KEY_SHA)"
else
  info "8a FAIL identity key changed: $NEW_KEY_SHA != $OLD_KEY_SHA"; FAILED="8a"
fi

# 8b. peer-id resolves to the same string against the NEW dir
NEW_PEER_ID="$(ATLAS_AGENT_DATA_DIR="$NEW_DATA_DIR" "$AGENT_BINARY" peer-id 2>/dev/null || true)"
if [ "$NEW_PEER_ID" = "$OLD_PEER_ID" ]; then
  info "8b OK  peer-id unchanged ($NEW_PEER_ID)"
else
  info "8b FAIL peer-id changed: $NEW_PEER_ID != $OLD_PEER_ID"; FAILED="${FAILED:+$FAILED,}8b"
fi

# 8c. every relationship.json identical to the pre-flight backup
for id in $RELS; do
  a="$NEW_DATA_DIR/relationships/$id/relationship.json"
  b="$BACKUP_DIR/data/relationships/$id/relationship.json"
  if diff -q <(json_canon "$a") <(json_canon "$b") >/dev/null 2>&1; then
    info "8c OK  relationship.json unchanged: $id"
  else
    info "8c FAIL relationship.json differs: $id"; FAILED="${FAILED:+$FAILED,}8c:$id"
  fi
done

# 8d + 8e. service active, and the journal shows the MIGRATED relationship
# specifically reached its dial path with no bootstrap error — a positive,
# relationship-scoped check.
#
# Two things this check must NOT depend on, both verified against source:
#
#  - Any absence-of-"enroll"/"FATAL": the implicit, undisablable "default"
#    relationship (https -> 127.0.0.1:8443, no cert) logs "enrolling" and
#    "relationship failed to bootstrap" every start, and the migrated
#    libp2p relationship logs "needs no enrollment credential". None of it
#    bears on "$PRIMARY_ID".
#
#  - "agent running" (internal/agent/agent.go): it is logged only after
#    agent.New() returns, and New blocks on bootstrapAllRelationships()'
#    wg.Wait() (agent.go ~177/306) until EVERY relationship's goroutine
#    returns — including "default", whose https bootstrapWithRetry
#    (credentials.go ~306) runs the full bootstrapMaxElapsed = 5 minutes
#    against a refused 127.0.0.1:8443 before giving up. So "agent running"
#    (and plugin activation, and the scheduler) do not appear for ~5 min
#    after start on this box, regardless of "$PRIMARY_ID" health. The
#    migrated relationship's own bootstrap goroutine runs concurrently and
#    logs its dial marker within a second or two.
if [ "$DRY_RUN" -eq 0 ]; then
  if systemctl is-active --quiet "$UNIT"; then info "8d OK  $UNIT is active"
  else info "8d FAIL $UNIT is not active"; FAILED="${FAILED:+$FAILED,}8d"; fi

  since_args=()
  [ -n "$START_TS" ] && since_args=(--since "$START_TS")
  jlog="$(journalctl -u "$UNIT" "${since_args[@]}" --no-pager 2>/dev/null || true)"
  reltag="\"relationship\":\"$PRIMARY_ID\""

  if printf '%s' "$jlog" | grep -F "$reltag" \
       | grep -qE 'dialing control plane (via rendezvous discovery|by static multiaddr)'; then
    info "8e OK  '$PRIMARY_ID' reached its dial path"
  else
    info "8e FAIL no dial marker for '$PRIMARY_ID' in the journal since start"
    FAILED="${FAILED:+$FAILED,}8e"
  fi

  if printf '%s' "$jlog" | grep -F 'relationship failed to bootstrap; it will not be available this run' \
       | grep -qF "$reltag"; then
    info "8e FAIL journal reports '$PRIMARY_ID' failed to bootstrap"
    FAILED="${FAILED:+$FAILED,}8e-bootstrap"
  fi
else
  info "8d/8e skipped (--dry-run: no systemd)"
fi

# =========================================================================
step "9. Result"
# =========================================================================
if [ -n "$FAILED" ]; then
  echo "FAILED CHECKS: $FAILED"
  # cleanup() (EXIT trap) rolls back the service on a non-dry run.
  exit 1
fi

if [ "$DRY_RUN" -eq 0 ]; then
  systemctl enable "$UNIT" >/dev/null
fi

SUCCESS=1
SERVER_PID_RESOLVED="${CP_SERVER_PID:-$(printf '%s' "$CP_SERVER_ADDR" | sed -E 's#.*/p2p/##')}"
cat <<EOF

==> Migration verified.
    Peer ID:      $OLD_PEER_ID   (unchanged)
    Data dir:     $OLD_DATA_DIR  ->  $NEW_DATA_DIR
    Service user: $RUN_USER
    Backup:       $BACKUP_DIR
    Old telemetry baseline (pre-migration): $OLD_TELE_BASELINE
        spool_files=$OLD_SPOOL_FILES spool_kb=$OLD_SPOOL_KB fails_15m=$OLD_TELE_FAILS_15M

    The identity key and every relationship.json are byte-identical to before
    (steps 8a-8c), the service is active, and '$PRIMARY_ID' reached its dial
    path cleanly. The migration stands.

    A REAL libp2p connection is not observable yet: the agent's Run() — and
    its first dial — is delayed ~5 min every start by the "default"
    relationship. Confirm it on your own time, without holding this script:
        journalctl -u $UNIT -f | grep 'direct connection established'
    or:
        $0 --confirm-connect --unit $UNIT --new-data-dir "$NEW_DATA_DIR"

    The OLD data dir was NOT deleted. Verify the box (Ports page attribution,
    live metrics), then remove it by hand once satisfied:
        rm -rf "$OLD_DATA_DIR"

    Rollback (if needed before you delete anything):
        systemctl stop $UNIT
        cp -a "$BACKUP_DIR"/*.service /etc/systemd/system/   # if present
        cp -a "$BACKUP_DIR/atlas-agent.env" "$OLD_ENV_FILE"
        systemctl daemon-reload && systemctl start $UNIT
EOF

# Optional, opt-in only: block here and poll, for an operator who wants to
# watch the connection come back before walking away. Never changes the
# outcome — the migration already stands.
if [ "$DRY_RUN" -eq 0 ] && [ "$WAIT_FOR_CONNECT" -gt 0 ]; then
  echo
  info "--wait-for-connect $WAIT_FOR_CONNECT: polling for the real connection (Ctrl-C is safe, migration stands)"
  if poll_for_connection "$SERVER_PID_RESOLVED" "$START_TS" "$WAIT_FOR_CONNECT"; then
    info "live connection confirmed."
  else
    info "not observed within ${WAIT_FOR_CONNECT}s — see the journalctl command above. Migration still stands."
  fi
fi
