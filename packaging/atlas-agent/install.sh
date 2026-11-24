#!/usr/bin/env bash
# Installs atlas-agent as a systemd service, or verifies an existing install.
#
# Usage:
#   install.sh --control-plane-url URL --token TOKEN [options]     # https
#   install.sh --transport libp2p --relationship NAME \            # libp2p
#     --control-plane-url URL --server-addr MULTIADDR [options]
#   install.sh --install-code atlas_install_<64 hex> \              # install code
#     --server-url URL --version vX.Y.Z
#   install.sh --verify [--relationship NAME] [--data-dir DIR] [options]
#   install.sh --upgrade --version vX.Y.Z [--sha256 HEX]           # binary only
#   install.sh --upgrade --binary PATH [--sha256 HEX]
#
# Binary source (install and --upgrade):
#   --binary PATH (default ./atlas-agent next to this script), or
#   --version vX.Y.Z to download atlas-agent-linux-<arch> and SHA256SUMS from
#   that GitHub Release. The download is verified against SHA256SUMS (and
#   --sha256 when given) before it is executed or installed; any mismatch or
#   a missing SHA256SUMS aborts with nothing changed. Its --version output is
#   then checked, run as the unprivileged 'atlas' user, against vX.Y.Z.
#
# Upgrade:
#   --upgrade replaces only /usr/local/bin/atlas-agent (keeping the old one as
#   atlas-agent.prev), restarts the service, and requires it to stay active
#   for 20 seconds with no automatic restart and to report the target
#   version. Otherwise the previous binary is restored and restarted. It takes
#   no connection flags and needs an existing /etc/atlas-agent/atlas-agent.env.
#
# Transports:
#   https (default)  First run enrolls with --token and pins a CA (either
#                    --ca-bundle or, deliberately, --insecure-bootstrap).
#                    Every run after reuses the persisted certificate.
#   libp2p           No token, no certificate. The agent's persistent Peer ID
#                    is its identity; an operator authorizes it once on the
#                    server. --token / --ca-bundle / --insecure-bootstrap are
#                    rejected. Requires exactly one address shape:
#                      --server-addr MULTIADDR                    (static), or
#                      --relay-addr MULTIADDR --server-peer-id ID (rendezvous)
#
# Install codes:
#   --install-code CODE --server-url URL --version vX.Y.Z installs with no
#   manual token or peer authorization at all: the agent generates its own
#   identity, presents CODE and that identity to POST /agent/install/redeem
#   on URL, and configures itself (implicit "default" relationship, libp2p
#   transport) from the response — the server address, environment and node
#   id all come from there, never from a flag. CODE has the exact shape
#   atlas_install_<64 lowercase hex> (see fleet.InstallCodePrefix) and is
#   never written to disk, a log, or an error message: pass it as the flag's
#   value, as the ATLAS_INSTALL_CODE environment variable (what the Atlas
#   UI's generated command uses), or as "-" to read one line from stdin.
#   A 401 from redeem means the code is invalid, expired, or already used.
#   Any failure after a successful redeem has already spent the code on the
#   server; this host is rolled back to its pre-install state so it can
#   retry cleanly with a freshly generated one. Refused on an already
#   configured host, same as a plain re-install, to prevent cloning an
#   identity, and --force-env is not accepted together with it.
#
# Preflight (install mode only, not --upgrade/--verify):
#   Before anything on the host changes: OS/arch, systemd running, free disk
#   space, and HTTPS reachability of whatever this run needs (the GitHub
#   release host, and --server-url). The plain (non-install-code) libp2p
#   flow also checks its --server-addr is open before anything changes, with
#   a plain TCP connect — no payload, nothing read. Install-code mode only
#   learns its server address from the redeem response, after the code is
#   already spent, so that address is not preflight-checked at all: a
#   false negative there would burn a still-valid code for no reason, and
#   the service health check after start (service_healthy, 20s) is the
#   actual safety net for a server that turns out not to be reachable.
#

# Relationships:
#   Without --relationship the flags configure the implicit "default"
#   relationship via flat ATLAS_AGENT_* variables (unchanged behaviour).
#   With --relationship NAME every connection flag is written in the
#   ATLAS_AGENT_RELATIONSHIP_<NAME>_* form and NAME is added to
#   ATLAS_AGENT_RELATIONSHIPS. "default" is reserved.
#
# Options:
#   --binary PATH          Agent binary to install (default: ./atlas-agent
#                           next to this script). In --verify mode, the
#                           installed binary to check (default
#                           /usr/local/bin/atlas-agent).
#   --control-plane-url URL
#   --token TOKEN           Enrollment token (https only)
#   --environment NAME      Operator-assigned environment tag
#   --ca-bundle PATH        CA certificate to pin (https, verified bootstrap)
#   --insecure-bootstrap    Trust and pin the cert presented on first contact
#                            (https). Required when --ca-bundle is not given.
#   --transport https|libp2p          (default https)
#   --relationship NAME               Named relationship id ([A-Za-z0-9_-]+)
#   --server-addr MULTIADDR           libp2p static server multiaddr
#   --relay-addr MULTIADDR            libp2p relay multiaddr (rendezvous)
#   --server-peer-id ID               libp2p server Peer ID (rendezvous)
#   --install-code CODE|-             One-time install code; "-" reads it from
#                            stdin. Required together with --server-url;
#                            ATLAS_INSTALL_CODE in the environment works the
#                            same as the flag when the flag is not given.
#   --server-url URL        https base URL to redeem --install-code against.
#                            Required together with --install-code.
#   --agentops-container-logs-disabled
#                            Set ..._AGENTOPS_CONTAINER_LOGS_DISABLED=true
#   --version vX.Y.Z        Download and verify this release instead of --binary
#   --sha256 HEX            Expected sha256 of the binary; must also match
#   --upgrade               Replace only the binary of an existing install
#   --no-start              Enable the service but do not start it
#   --force-env             Overwrite an existing atlas-agent.env. Without it,
#                            an install onto a host that already has one fails.
#   --verify               Read-only: run every check against an existing
#                            install, print ok/FAIL per line, exit non-zero if
#                            any failed. Makes no changes.
#   --data-dir DIR         --verify only: data directory to inspect
#                            (default /var/lib/atlas-agent)
#   --expect-peer-id ID    --verify only: assert `atlas-agent peer-id` equals ID
#   --unit NAME            --verify only: systemd unit name (default atlas-agent)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

die() { echo "install.sh: $*" >&2; exit 1; }

# json_get FILE KEY -> prints the string value of a top-level "KEY". Used by
# --verify and by install-code mode to read the redeem response, without
# assuming jq is installed (a minimal Ubuntu host has neither python3 nor jq
# guaranteed, hence the grep fallback).
json_get() {
  local f="$1" k="$2"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$f" "$k" <<'PY' 2>/dev/null || true
import json, sys
try:
    with open(sys.argv[1]) as fh:
        print(json.load(fh).get(sys.argv[2], ""))
except Exception:
    sys.exit(1)
PY
  elif command -v jq >/dev/null 2>&1; then
    jq -r --arg k "$k" '.[$k] // ""' "$f" 2>/dev/null || true
  else
    grep -oE "\"$k\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" "$f" 2>/dev/null \
      | sed -E "s/.*:[[:space:]]*\"([^\"]*)\"/\1/" | head -1
  fi
}
json_parses() {
  if command -v python3 >/dev/null 2>&1; then
    python3 -c 'import json,sys; json.load(open(sys.argv[1]))' "$1" 2>/dev/null
  elif command -v jq >/dev/null 2>&1; then
    jq -e . "$1" >/dev/null 2>&1
  else
    # last resort: balanced braces and a trailing "}"
    head -c1 "$1" | grep -q '{' && tail -c2 "$1" | grep -q '}'
  fi
}

MODE=install
BINARY="$SCRIPT_DIR/atlas-agent"
CONTROL_PLANE_URL=""
TOKEN=""
ENVIRONMENT=""
CA_BUNDLE=""
INSECURE_BOOTSTRAP=0
START=1
FORCE_ENV=0
TRANSPORT="https"
RELATIONSHIP=""
LIBP2P_SERVER_ADDR=""
LIBP2P_RELAY_ADDR=""
LIBP2P_SERVER_PEER_ID=""
AGENTOPS_CONTAINER_LOGS_DISABLED=0
DATA_DIR="/var/lib/atlas-agent"
EXPECT_PEER_ID=""
UNIT="atlas-agent"
BINARY_SET=0
RELEASE_VERSION=""
EXPECT_SHA256=""
UPGRADE=0
INSTALL_CODE="${ATLAS_INSTALL_CODE:-}"
# The value is captured above; the environment variable itself has no
# further use and must not be inherited by anything this script spawns
# (runuser, atlas-agent, curl, systemctl, ...) from here on.
unset ATLAS_INSTALL_CODE
INSTALL_CODE_FROM_STDIN=0
SERVER_URL=""
NODE_ID=""
# Set the instant a redeem response comes back 200 — the code is spent on
# the server from that moment on, regardless of what this process does
# next. cleanup() below checks this, not any particular call site, so the
# "already consumed" warning fires on every exit path: an explicit die(),
# an unguarded command failing under set -e, a signal, anything.
CODE_REDEEMED=0
# Set once the on-disk state (env file + unit) matches a successful redeem;
# until then, a failure rolls everything install-code mode wrote back out —
# see the cleanup() trap below and the "install-code mode" section's comment.
INSTALL_CODE_COMMITTED=0
# Set only once the already-configured-host guard confirms this host had no
# $ENV_FILE before this run — so the cleanup trap below knows it is safe to
# delete $BIN_DEST/$ENV_FILE/$UNIT_DEST: they can only be what this run
# itself wrote, never a pre-existing install the guard would otherwise have
# refused to touch.
INSTALL_CODE_FRESH_INSTALL=0
# Set only when $CONF_DIR did not exist before this run touched it (checked
# right before the install -d that creates it) — so a rollback's rmdir
# never removes a directory this run did not create. It also only ever
# succeeds if the directory is empty, the second half of "never remove
# what was pre-existing or non-empty".
CONF_DIR_CREATED=0
# Every connection/config flag given, so an install onto an existing env
# file and an --upgrade can refuse them instead of silently ignoring them.
CONN_FLAGS=()

# The public repository whose GitHub Releases carry atlas-agent-linux-<arch>
# and SHA256SUMS (see the agent-release job in .github/workflows/ci.yml).
RELEASE_REPO="HexaneCreations/Atlas"
# How long a restarted service must stay active after --upgrade.
HEALTH_SECONDS=20

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY="$2"; BINARY_SET=1; shift 2 ;;
    --version) RELEASE_VERSION="$2"; shift 2 ;;
    --sha256) EXPECT_SHA256="$2"; shift 2 ;;
    --upgrade) UPGRADE=1; shift ;;
    --control-plane-url) CONTROL_PLANE_URL="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --token) TOKEN="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --environment) ENVIRONMENT="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --ca-bundle) CA_BUNDLE="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --insecure-bootstrap) INSECURE_BOOTSTRAP=1; CONN_FLAGS+=("$1"); shift ;;
    --transport) TRANSPORT="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --relationship) RELATIONSHIP="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --server-addr) LIBP2P_SERVER_ADDR="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --relay-addr) LIBP2P_RELAY_ADDR="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --server-peer-id) LIBP2P_SERVER_PEER_ID="$2"; CONN_FLAGS+=("$1"); shift 2 ;;
    --agentops-container-logs-disabled) AGENTOPS_CONTAINER_LOGS_DISABLED=1; CONN_FLAGS+=("$1"); shift ;;
    --install-code)
      if [ "$2" = "-" ]; then INSTALL_CODE_FROM_STDIN=1; else INSTALL_CODE="$2"; fi
      shift 2 ;;
    --server-url) SERVER_URL="$2"; shift 2 ;;
    --no-start) START=0; shift ;;
    --force-env) FORCE_ENV=1; shift ;;
    --verify) MODE=verify; shift ;;
    --data-dir) DATA_DIR="$2"; shift 2 ;;
    --expect-peer-id) EXPECT_PEER_ID="$2"; shift 2 ;;
    --unit) UNIT="$2"; shift 2 ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) die "unknown option $1" ;;
  esac
done

if [ "$INSTALL_CODE_FROM_STDIN" -eq 1 ]; then
  IFS= read -r INSTALL_CODE || die "--install-code - expected a code on stdin"
fi
INSTALL_CODE_MODE=0
[ -n "$INSTALL_CODE" ] && INSTALL_CODE_MODE=1

# relationshipEnvPrefix mirrors internal/agent/config.go: upper-case the id,
# then replace every character that is not A-Z or 0-9 with "_".
relationship_prefix() {
  local up
  up="$(printf '%s' "$1" | tr '[:lower:]' '[:upper:]' | sed 's/[^A-Z0-9]/_/g')"
  printf 'ATLAS_AGENT_RELATIONSHIP_%s_' "$up"
}

# DESTDIR follows the GNU staged-install convention: empty (the default) is a
# real install to the system; set it to stage the whole tree under a prefix
# (packaging, testing). It does not affect --verify.
DESTDIR="${DESTDIR:-}"
BIN_DEST="$DESTDIR/usr/local/bin/atlas-agent"
CONF_DIR="$DESTDIR/etc/atlas-agent"
ENV_FILE="$CONF_DIR/atlas-agent.env"
UNIT_DEST="$DESTDIR/etc/systemd/system/atlas-agent.service"
DROPIN_DIR="$DESTDIR/etc/systemd/system/atlas-agent.service.d"
# DATA_DIR is fixed by the unit's StateDirectory= for a real install; the
# --data-dir flag only redirects where --verify looks. AGENT_DATA_DIR is the
# same path, hoisted here (rather than only where install-code mode used to
# set it) so preflight's disk-space check and the cleanup trap can both see
# it regardless of which mode or how far the script got.
AGENT_DATA_DIR="$DESTDIR/var/lib/atlas-agent"

if [ "$MODE" = verify ]; then
  [ "$UPGRADE" -eq 0 ] || die "--verify and --upgrade cannot be combined"
  [ -z "$RELEASE_VERSION$EXPECT_SHA256" ] || die "--version and --sha256 are not used with --verify"
elif [ "$UPGRADE" -eq 1 ]; then
  MODE=upgrade
fi

# ---------------------------------------------------------------------------
# verify mode
# ---------------------------------------------------------------------------
if [ "$MODE" = verify ]; then
  [ "$BINARY" = "$SCRIPT_DIR/atlas-agent" ] && BINARY="$BIN_DEST"
  fails=0
  pass() { printf '  ok    %s\n' "$1"; }
  fail() { printf '  FAIL  %s\n' "$1" >&2; fails=$((fails + 1)); }
  warn() { printf '  warn  %s\n' "$1"; }   # advisory only — does not fail --verify

  echo "==> Verifying atlas-agent (data dir: $DATA_DIR, relationship: ${RELATIONSHIP:-<from env>})"

  # 1. binary present and executable
  if [ -x "$BINARY" ]; then pass "binary present and executable ($BINARY)"
  else fail "no executable binary at $BINARY"; fi

  # 2. --version is stamped (not a bare dev / dirty build)
  ver=""
  [ -x "$BINARY" ] && ver="$("$BINARY" --version 2>/dev/null || true)"
  if [ -z "$ver" ]; then fail "--version produced no output"
  elif printf '%s' "$ver" | grep -qE 'dev|-dirty|unknown'; then
    fail "--version looks unstamped: $ver"
  else pass "--version is stamped: $ver"; fi

  # resolve the relationship id from the env file if not given
  rel="$RELATIONSHIP"
  if [ -z "$rel" ] && [ -f "$ENV_FILE" ]; then
    rel="$(grep -E '^ATLAS_AGENT_RELATIONSHIPS=' "$ENV_FILE" | head -1 | cut -d= -f2- | tr ',' ' ')"
  fi
  rel="$(printf '%s' "$rel" | awk '{print $1}')"   # first / only id
  [ -n "$rel" ] || rel="default"

  # 3. identity file present, mode 0600, owned by the unit user
  key="$DATA_DIR/p2p-identity.key"
  unit_user="$(systemctl show -p User --value "$UNIT" 2>/dev/null || true)"
  [ -n "$unit_user" ] || unit_user="atlas"
  if [ ! -f "$key" ]; then fail "no identity file at $key"
  else
    mode="$(stat -c '%a' "$key" 2>/dev/null || stat -f '%Lp' "$key" 2>/dev/null || true)"
    owner="$(stat -c '%U' "$key" 2>/dev/null || stat -f '%Su' "$key" 2>/dev/null || true)"
    [ "$mode" = "600" ] && pass "identity file mode 0600" || fail "identity file mode is $mode, want 600"
    [ "$owner" = "$unit_user" ] && pass "identity file owned by $unit_user" \
      || fail "identity file owned by $owner, unit runs as $unit_user"
  fi

  # 4. peer-id resolves
  pid_out=""
  if [ -x "$BINARY" ] && [ -f "$key" ]; then
    pid_out="$(ATLAS_AGENT_DATA_DIR="$DATA_DIR" "$BINARY" peer-id 2>/dev/null || true)"
  fi
  if printf '%s' "$pid_out" | grep -qE '^12D3Koo|^Qm'; then pass "peer-id resolves: $pid_out"
  else fail "peer-id did not resolve (got: ${pid_out:-<empty>})"; fi

  # 5. peer-id matches the expected value, if one was given
  if [ -n "$EXPECT_PEER_ID" ]; then
    [ "$pid_out" = "$EXPECT_PEER_ID" ] && pass "peer-id matches --expect-peer-id" \
      || fail "peer-id $pid_out != expected $EXPECT_PEER_ID"
  fi

  # relationship.json for the resolved id
  rjson="$DATA_DIR/relationship.json"
  [ "$rel" != "default" ] && rjson="$DATA_DIR/relationships/$rel/relationship.json"

  # 6. relationship.json present and parses
  if [ ! -f "$rjson" ]; then fail "no relationship.json at $rjson"
  elif json_parses "$rjson"; then pass "relationship.json parses ($rjson)"
  else fail "relationship.json does not parse ($rjson)"; fi

  if [ -f "$rjson" ]; then
    t="$(json_get "$rjson" transport)"
    u="$(json_get "$rjson" control_plane_url)"
    sa="$(json_get "$rjson" libp2p_server_addr)"
    ra="$(json_get "$rjson" libp2p_relay_addr)"
    sp="$(json_get "$rjson" libp2p_server_peer_id)"

    # 7. transport matches expectation
    if [ "$TRANSPORT" = "https" ] && [ -z "$RELATIONSHIP" ]; then
      : # no expectation asserted for a bare default-https verify
    else
      [ "$t" = "$TRANSPORT" ] && pass "transport is $t" || fail "transport is $t, want $TRANSPORT"
    fi

    # 8. control-plane URL matches, if one was given
    if [ -n "$CONTROL_PLANE_URL" ]; then
      [ "$u" = "$CONTROL_PLANE_URL" ] && pass "control_plane_url matches" \
        || fail "control_plane_url is $u, want $CONTROL_PLANE_URL"
    fi

    # 9. libp2p address shape is internally complete
    if [ "$t" = "libp2p" ]; then
      if [ -n "$sa" ] && [ -z "$ra$sp" ]; then pass "libp2p static shape (server_addr)"
      elif [ -z "$sa" ] && [ -n "$ra" ] && [ -n "$sp" ]; then pass "libp2p rendezvous shape (relay + server_peer_id)"
      else fail "libp2p shape incomplete/ambiguous (server_addr='$sa' relay='$ra' server_peer_id='$sp')"; fi
    fi

    # 11. no 127.0.0.1:8443 default leaking into the resolved config
    if printf '%s' "$u$sa$ra" | grep -q '127.0.0.1:8443'; then
      fail "resolved config references the 127.0.0.1:8443 default"
    else pass "no 127.0.0.1:8443 default reference"; fi
  fi

  # 10. ATLAS_AGENT_RELATIONSHIPS is exactly the expected set (no stray default)
  if [ -f "$ENV_FILE" ]; then
    rels="$(grep -E '^ATLAS_AGENT_RELATIONSHIPS=' "$ENV_FILE" | head -1 | cut -d= -f2- || true)"
    if [ "$rel" != "default" ]; then
      if [ "$rels" = "$rel" ]; then pass "ATLAS_AGENT_RELATIONSHIPS=$rels"
      else fail "ATLAS_AGENT_RELATIONSHIPS='$rels', want '$rel'"; fi
    fi
  fi

  # 12. service active, and the journal shows THIS relationship reached its
  # dial path with no bootstrap failure. Scoped to "$rel" and deliberately
  # NOT gated on "agent running":
  #   - the implicit "default" relationship (https -> 127.0.0.1:8443, no
  #     cert) logs "enrolling" and "relationship failed to bootstrap" every
  #     start regardless of "$rel";
  #   - "agent running" is logged only after agent.New() returns, and New
  #     blocks until every relationship's bootstrap goroutine returns —
  #     "default" takes the full 5-minute bootstrapMaxElapsed against a
  #     refused 127.0.0.1:8443 first — so it is absent for ~5 min after any
  #     restart even when "$rel" is perfectly healthy.
  if command -v systemctl >/dev/null 2>&1; then
    if systemctl is-active --quiet "$UNIT"; then pass "$UNIT is active"
    else fail "$UNIT is not active"; fi
    since="$(systemctl show -p ActiveEnterTimestamp --value "$UNIT" 2>/dev/null || true)"
    since_args=()
    [ -n "$since" ] && since_args=(--since "$since")
    jlog="$(journalctl -u "$UNIT" "${since_args[@]}" --no-pager 2>/dev/null || true)"
    reltag="\"relationship\":\"$rel\""
    if [ "$rel" != "default" ]; then
      if printf '%s' "$jlog" | grep -F "$reltag" \
           | grep -qE 'dialing control plane (via rendezvous discovery|by static multiaddr)'; then
        pass "journal: '$rel' reached its dial path"
      else fail "journal: no dial marker for '$rel' since last start"; fi
      if printf '%s' "$jlog" | grep -F 'relationship failed to bootstrap; it will not be available this run' \
           | grep -qF "$reltag"; then
        fail "journal: '$rel' failed to bootstrap"
      else pass "journal: no bootstrap failure for '$rel'"; fi

      # Soft: a genuine, completed direct libp2p connection to this
      # relationship's server (pathlog.go's ConnectedF -> Noise handshake
      # done). Tagged "peer":"<id>", not "relationship". Absent for ~5 min
      # after a restart (see the "agent running" note above), so a miss is
      # advisory, never a --verify failure.
      spid="$sp"
      [ -z "$spid" ] && spid="$(printf '%s' "$sa" | sed -E 's#.*/p2p/##')"
      if [ -n "$spid" ]; then
        if printf '%s' "$jlog" | grep -F 'libp2p direct connection established' \
             | grep -qF "\"peer\":\"$spid\""; then
          pass "journal: live direct libp2p connection to '$rel' ($spid)"
        else
          warn "journal: no live-connection line for '$rel' yet ($spid) — expected ~5-7 min after a restart"
        fi
      fi
    fi
  else
    fail "systemctl not available — cannot check service state"
  fi

  echo
  if [ "$fails" -eq 0 ]; then echo "==> All checks passed."; exit 0
  else echo "==> $fails check(s) failed." >&2; exit 1; fi
fi

# ---------------------------------------------------------------------------
# install and upgrade: shared setup
# ---------------------------------------------------------------------------
# STAGED is true when DESTDIR is set: write the tree under the prefix, own
# files as the calling user, and touch nothing system-wide (no useradd, no
# systemctl). A real install (DESTDIR empty) needs root and does all of it.
STAGED=0
[ -n "$DESTDIR" ] && STAGED=1
OWN_ROOT=(-o root -g root)
OWN_ATLAS=(-o root -g atlas)
# Distinct from OWN_ATLAS on purpose: $CONF_DIR is root-owned, atlas-group-
# readable (atlas must read its config, never write it), but $AGENT_DATA_DIR
# is the agent's own runtime state — identity key, spool, relationship
# files — the same thing systemd's StateDirectory=atlas-agent would hand it
# at 0700 if systemd created it. It must be owned BY atlas, not just
# group-readable by it: root:atlas at 0700 grants the owner (root) access
# and the group (atlas) none at all, which is exactly the bug this guards
# against — the atlas user getting EACCES on its own data directory.
OWN_ATLAS_DATA=(-o atlas -g atlas)
if [ "$STAGED" -eq 1 ]; then OWN_ROOT=(); OWN_ATLAS=(); OWN_ATLAS_DATA=(); fi

if [ "$STAGED" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
  die "must run as root (try sudo), or set DESTDIR to stage the install"
fi

CLEANUP_DIRS=()
# Set once `systemctl enable atlas-agent` is actually reached, so cleanup
# knows there might be a running or enabled unit to stop and disable — never
# set at all for a failure that happens before the unit is even registered,
# so an early failure's rollback does not call systemctl at all.
INSTALL_CODE_SERVICE_TOUCHED=0
# Real hosts always; staged tests only when they opt in with
# ATLAS_INSTALL_TEST=1, the same escape hatch used everywhere else in this
# script to exercise the stub systemctl.
systemctl_rollback_allowed() { [ "$STAGED" -eq 0 ] || [ "${ATLAS_INSTALL_TEST:-}" = 1 ]; }
cleanup() {
  local d
  for d in "${CLEANUP_DIRS[@]+"${CLEANUP_DIRS[@]}"}"; do rm -rf "$d"; done
  # install-code mode: anything written before INSTALL_CODE_COMMITTED is set
  # (see the "install-code mode" section) is only ever half of a redeem —
  # on ANY exit before that point, undo it so the host is clean and
  # re-installable with a freshly generated code, per the script's own doc.
  if [ "${INSTALL_CODE_MODE:-0}" -eq 1 ] && [ "${INSTALL_CODE_FRESH_INSTALL:-0}" -eq 1 ] && [ "${INSTALL_CODE_COMMITTED:-0}" -ne 1 ]; then
    if [ "${CODE_REDEEMED:-0}" -eq 1 ]; then
      echo "install.sh: the install code has already been consumed on the server; the pre-assigned node is burned. Local changes have been rolled back. Generate a new install code in the Atlas UI before retrying." >&2
    fi
    if [ "${INSTALL_CODE_SERVICE_TOUCHED:-0}" -eq 1 ] && systemctl_rollback_allowed && command -v systemctl >/dev/null 2>&1; then
      # A deleted unit left running (or left enabled to start on the next
      # boot) is not a clean rollback — the whole point is a host that is
      # re-installable, not one quietly running an agent from files that no
      # longer exist.
      systemctl stop atlas-agent 2>/dev/null || true
      systemctl disable atlas-agent 2>/dev/null || true
    fi
    rm -f "$BIN_DEST" "$ENV_FILE" "$UNIT_DEST" 2>/dev/null || true
    # Only ever removes $CONF_DIR when this run created it (CONF_DIR_CREATED)
    # and rmdir's own "only if empty" — never a pre-existing or non-empty
    # /etc/atlas-agent.
    if [ "${CONF_DIR_CREATED:-0}" -eq 1 ] && [ -n "${CONF_DIR:-}" ]; then rmdir "$CONF_DIR" 2>/dev/null || true; fi
    if [ -n "${DROPIN_DIR:-}" ]; then
      rm -f "$DROPIN_DIR/10-docker-group.conf" 2>/dev/null
      rmdir "$DROPIN_DIR" 2>/dev/null || true   # only succeeds if now empty
    fi
    [ -n "${AGENT_DATA_DIR:-}" ] && rm -rf "$AGENT_DATA_DIR" 2>/dev/null
    if systemctl_rollback_allowed && command -v systemctl >/dev/null 2>&1; then systemctl daemon-reload 2>/dev/null || true; fi
  fi
}
trap cleanup EXIT

# validate FLAG VALUE REGEX: an empty value is allowed (the caller decides
# whether the flag is required); anything else must match REGEX in full.
# Runs before anything is written, so a rejected value changes nothing.
validate() {
  local flag="$1" value="$2" re="$3" shown
  [ -n "$value" ] || return 0
  # --install-code is the one value this script must never echo, to a log,
  # an error message, or anywhere else — not even a rejected one, which
  # could still be a real (if malformed) secret an operator pasted wrong.
  if [ "$flag" = --install-code ]; then
    case "$value" in
      *$'\n'* | *$'\r'*) die "$flag must not contain a newline or carriage return" ;;
    esac
    [[ "$value" =~ $re ]] \
      || die "$flag has the wrong format (expected atlas_install_ followed by 64 lowercase hex characters)"
    return 0
  fi
  printf -v shown '%q' "$value"
  case "$value" in
    *$'\n'* | *$'\r'*) die "$flag must not contain a newline or carriage return: $shown" ;;
  esac
  [[ "$value" =~ $re ]] || die "$flag contains characters that are not allowed: $shown"
}

URL_RE='^https://([A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(:[0-9]{1,5})?(/[A-Za-z0-9._~/-]*)?$'
MULTIADDR_RE='^/[A-Za-z0-9./:_-]+$'
PEER_ID_RE='^[1-9A-HJ-NP-Za-km-z]{40,128}$'
NAME_RE='^[A-Za-z0-9_-]+$'
ENVIRONMENT_RE='^[A-Za-z0-9._-]{1,64}$'
TOKEN_RE='^[A-Za-z0-9_-]{1,255}$'
PATH_RE='^/[A-Za-z0-9._/-]+$'
VERSION_RE='^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$'
SHA256_RE='^[0-9a-f]{64}$'
INSTALL_CODE_RE='^atlas_install_[0-9a-f]{64}$'

validate --control-plane-url "$CONTROL_PLANE_URL" "$URL_RE"
validate --token "$TOKEN" "$TOKEN_RE"
validate --environment "$ENVIRONMENT" "$ENVIRONMENT_RE"
validate --ca-bundle "$CA_BUNDLE" "$PATH_RE"
validate --relationship "$RELATIONSHIP" "$NAME_RE"
validate --server-addr "$LIBP2P_SERVER_ADDR" "$MULTIADDR_RE"
validate --relay-addr "$LIBP2P_RELAY_ADDR" "$MULTIADDR_RE"
validate --server-peer-id "$LIBP2P_SERVER_PEER_ID" "$PEER_ID_RE"
validate --version "$RELEASE_VERSION" "$VERSION_RE"
EXPECT_SHA256="$(printf '%s' "$EXPECT_SHA256" | tr 'A-F' 'a-f')"
validate --sha256 "$EXPECT_SHA256" "$SHA256_RE"
validate --install-code "$INSTALL_CODE" "$INSTALL_CODE_RE"
validate --server-url "$SERVER_URL" "$URL_RE"
if [ -n "$RELEASE_VERSION" ] && [ "$BINARY_SET" -eq 1 ]; then
  die "--version and --binary are alternatives; pass one"
fi

if [ -n "$INSTALL_CODE$SERVER_URL" ] && { [ -z "$INSTALL_CODE" ] || [ -z "$SERVER_URL" ]; }; then
  die "--install-code and --server-url must be given together"
fi
if [ "$INSTALL_CODE_MODE" -eq 1 ]; then
  # MODE can only be "install" or "upgrade" here — a --verify run already
  # exited, above, before this point. --upgrade is still reachable (it sets
  # UPGRADE=1 and only turns into MODE=upgrade; see the MODE resolution near
  # the top of the script), so it still needs an explicit refusal.
  [ "$UPGRADE" -eq 0 ] || die "--install-code is not used with --upgrade"
  [ "$FORCE_ENV" -eq 0 ] || die "--force-env is not used with --install-code (it would risk cloning an existing identity)"
  [ "${#CONN_FLAGS[@]}" -eq 0 ] \
    || die "--install-code does not take connection flags (got: ${CONN_FLAGS[*]}); the server supplies them via redeem"
fi

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

fetch() {
  curl --proto '=https' --tlsv1.2 -fsSL -o "$2" "$1"
}

# download_release sets BINARY to a verified copy of the $RELEASE_VERSION
# asset for this machine, in a fresh 0700 directory. Nothing is executed or
# installed before both checksums match.
download_release() {
  local os machine arch asset base work expected count actual
  os="$(uname -s)"
  [ "$os" = Linux ] || die "release binaries are built for Linux only (this host is $os)"
  machine="$(uname -m)"
  case "$machine" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture '$machine': releases exist for x86_64 (amd64) and aarch64/arm64 only" ;;
  esac
  asset="atlas-agent-linux-$arch"
  base="https://github.com/$RELEASE_REPO/releases/download/$RELEASE_VERSION"

  work="$(mktemp -d)"
  CLEANUP_DIRS+=("$work")
  chmod 0700 "$work"

  echo "==> Downloading $asset and SHA256SUMS for $RELEASE_VERSION"
  fetch "$base/SHA256SUMS" "$work/SHA256SUMS" \
    || die "could not download SHA256SUMS for $RELEASE_VERSION; refusing an unverified binary. Nothing was changed."
  fetch "$base/$asset" "$work/$asset" \
    || die "could not download $asset for $RELEASE_VERSION. Nothing was changed."

  expected="$(awk -v a="$asset" '$2 == a || $2 == "*" a {print $1}' "$work/SHA256SUMS")"
  count="$(printf '%s' "$expected" | grep -c . || true)"
  if [ "$count" -ne 1 ] || ! [[ "$expected" =~ $SHA256_RE ]]; then
    die "SHA256SUMS for $RELEASE_VERSION has no single valid entry for $asset. Nothing was changed."
  fi
  actual="$(sha256_of "$work/$asset")"
  if [ "$actual" != "$expected" ]; then
    die "checksum mismatch for $asset: SHA256SUMS has $expected, the download is $actual. Nothing was changed."
  fi
  if [ -n "$EXPECT_SHA256" ] && [ "$actual" != "$EXPECT_SHA256" ]; then
    die "checksum mismatch for $asset: --sha256 is $EXPECT_SHA256, the download is $actual. Nothing was changed."
  fi
  echo "    sha256 $actual verified"
  chmod 0755 "$work/$asset"
  BINARY="$work/$asset"
}

ATLAS_USER_READY=0
ensure_atlas_user() {
  [ "$STAGED" -eq 0 ] && [ "$ATLAS_USER_READY" -eq 0 ] || return 0
  ATLAS_USER_READY=1
  echo "==> Creating system user 'atlas'"
  if ! getent passwd atlas >/dev/null; then
    useradd --system --no-create-home --home-dir /var/lib/atlas-agent \
      --shell /usr/sbin/nologin atlas
  fi
}

# binary_version PATH sets GOT_VERSION to the version PATH reports, running
# it as the unprivileged 'atlas' user when this script is root. Returns
# non-zero when it cannot be run or does not report "atlas <version> ...".
binary_version() {
  local out first rest
  GOT_VERSION=""
  if [ "$(id -u)" -ne 0 ]; then
    out="$("$1" --version 2>/dev/null)" || return 1
  elif command -v runuser >/dev/null 2>&1; then
    out="$(runuser -u atlas -- "$1" --version 2>/dev/null)" || return 1
  elif command -v setpriv >/dev/null 2>&1; then
    out="$(setpriv --reuid=atlas --regid=atlas --init-groups -- "$1" --version 2>/dev/null)" || return 1
  else
    die "need runuser or setpriv to run the new binary as the atlas user"
  fi
  read -r first GOT_VERSION rest <<<"$out"
  : "$rest"
  [ "$first" = atlas ] && [ -n "$GOT_VERSION" ]
}

# probe_version PATH sets GOT_VERSION for a not-yet-installed binary. As root,
# the binary is copied into a directory only 'atlas' can enter, so the atlas
# user can execute the copy while the file root installs stays out of its
# reach.
probe_version() {
  local bin="$1" probe
  if [ "$(id -u)" -eq 0 ]; then
    probe="$(mktemp -d)"
    CLEANUP_DIRS+=("$probe")
    chmod 0700 "$probe"
    install -m 0755 "$bin" "$probe/atlas-agent"
    chown atlas "$probe"
    bin="$probe/atlas-agent"
  fi
  binary_version "$bin" || die "the new binary did not report a version (as the atlas user). Nothing was installed."
}

# agent_peer_id DIR prints the persistent libp2p Peer ID `atlas-agent
# peer-id` resolves for data directory DIR, generating the identity file
# there if absent — run as the unprivileged 'atlas' user, the same identity
# the installed service will run with. Called only after $BIN_DEST already
# holds the verified binary (world-executable at 0755), so no probe-copy
# trick like [probe_version]'s is needed here.
agent_peer_id() {
  local dir="$1" out
  if [ "$STAGED" -eq 1 ]; then
    out="$(ATLAS_AGENT_DATA_DIR="$dir" "$BIN_DEST" peer-id 2>&1)" || { echo "$out" >&2; return 1; }
  elif command -v runuser >/dev/null 2>&1; then
    out="$(runuser -u atlas -- env ATLAS_AGENT_DATA_DIR="$dir" "$BIN_DEST" peer-id 2>&1)" || { echo "$out" >&2; return 1; }
  elif command -v setpriv >/dev/null 2>&1; then
    out="$(setpriv --reuid=atlas --regid=atlas --init-groups -- env ATLAS_AGENT_DATA_DIR="$dir" "$BIN_DEST" peer-id 2>&1)" \
      || { echo "$out" >&2; return 1; }
  else
    echo "need runuser or setpriv to generate the agent identity as the atlas user" >&2
    return 1
  fi
  printf '%s' "$out"
}

# acquire_binary leaves a verified binary at $BINARY, from --version or
# --binary.
acquire_binary() {
  if [ -n "$RELEASE_VERSION" ]; then
    download_release
    return
  fi
  if [ ! -x "$BINARY" ]; then
    die "no executable binary at $BINARY (pass --binary PATH or --version vX.Y.Z)"
  fi
  if [ -n "$EXPECT_SHA256" ]; then
    local actual
    actual="$(sha256_of "$BINARY")"
    [ "$actual" = "$EXPECT_SHA256" ] \
      || die "checksum mismatch for $BINARY: --sha256 is $EXPECT_SHA256, the file is $actual. Nothing was changed."
  fi
}

# ---------------------------------------------------------------------------
# preflight: everything checkable before any host state changes
# ---------------------------------------------------------------------------

# 100 MiB: ample margin over the ~30-40MB static binary plus the identity
# key, early spool and state files — not a hard budget for long-term spool
# growth, which is a capacity-planning concern, not an install-time one.
MIN_FREE_KB=102400

disk_free_kb() { df -Pk "$1" 2>/dev/null | awk 'NR==2{print $4}'; }

# reachable_https URL: true if an HTTPS request to URL gets far enough to
# prove TLS and routing work — any response (even an error one) counts; only
# "could not connect/resolve/timed out" counts as unreachable. Preflight
# only; download_release's own fetch() is what verifies actual content.
reachable_https() {
  curl --proto '=https' --tlsv1.2 -sS --connect-timeout 5 --max-time 10 -o /dev/null "$1" 2>/dev/null
  local rc=$?
  [ "$rc" -ne 6 ] && [ "$rc" -ne 7 ] && [ "$rc" -ne 28 ]
}

# tcp_reachable HOST PORT: true unless a plain TCP connect to HOST:PORT
# cannot be established within the timeout. Deliberately connect-only — no
# payload sent, nothing read, no curl: the libp2p port speaks a binary
# protocol, not HTTP, and curl waiting for an HTTP response that will never
# come times out (exit 28) on a port that is in fact open — a false
# negative that would wrongly report a working server as unreachable. bash's
# own /dev/tcp proves only the thing this actually needs: the connection was
# accepted. `timeout` isn't available everywhere this script might be
# exercised from (not on the target Ubuntu hosts, where it is coreutils and
# always present — only in some non-Linux dev/test environments), so it is
# used when present and skipped, never faked, when it is not.
tcp_reachable() {
  local host="$1" port="$2"
  if command -v timeout >/dev/null 2>&1; then
    timeout 5 bash -c "exec 3<>'/dev/tcp/$host/$port'" 2>/dev/null
  else
    bash -c "exec 3<>'/dev/tcp/$host/$port'" 2>/dev/null
  fi
}

# multiaddr_host_port ADDR sets MA_HOST/MA_PORT from a /.../tcp/PORT/...
# multiaddr (ip4, ip6, dns4 or dns6). Empty MA_PORT means it did not parse —
# callers skip the check rather than fail preflight on a shape MULTIADDR_RE
# already accepted but this parser does not recognise.
multiaddr_host_port() {
  local addr="$1" i
  IFS=/ read -ra _ma_parts <<<"$addr"
  MA_HOST="${_ma_parts[2]:-}"
  MA_PORT=""
  for ((i = 0; i < ${#_ma_parts[@]}; i++)); do
    if [ "${_ma_parts[$i]}" = tcp ]; then MA_PORT="${_ma_parts[$((i + 1))]:-}"; break; fi
  done
}

# run_preflight exits with one clear line and changes nothing if this host
# cannot take the install at all. Every path it reads is $DESTDIR-relative,
# the same convention the rest of the script uses, so a staged test can seed
# or remove the exact fixture a check reads rather than this function
# special-casing STAGED itself — the only two things actually impossible to
# check hermetically (root, since STAGED never needs it; see the earlier
# `id -u` guard this would otherwise duplicate) are not checked twice here.
run_preflight() {
  local os machine

  os="$(uname -s)"
  [ "$os" = Linux ] || die "preflight: release binaries are built for Linux only (this host is $os)"
  machine="$(uname -m)"
  case "$machine" in
    x86_64 | amd64 | aarch64 | arm64) : ;;
    *) die "preflight: unsupported architecture '$machine': releases exist for x86_64 (amd64) and aarch64/arm64 only" ;;
  esac

  # ATLAS_INSTALL_SYSTEMD_MARKER is a test-only seam, the same kind
  # ATLAS_INSTALL_TEST already is: unset in real use, where this is exactly
  # /run/systemd/system.
  local systemd_marker="${ATLAS_INSTALL_SYSTEMD_MARKER:-/run/systemd/system}"
  [ -d "$systemd_marker" ] \
    || die "preflight: systemd does not appear to be running (no $systemd_marker) — atlas-agent is installed as a systemd service"

  local free
  free="$(disk_free_kb "$(dirname "$BIN_DEST")")"
  if [ -z "$free" ] || [ "$free" -lt "$MIN_FREE_KB" ]; then
    die "preflight: less than $((MIN_FREE_KB / 1024))MiB free on $(dirname "$BIN_DEST")'s filesystem (need room for the binary and its data directory)"
  fi

  if [ -n "$RELEASE_VERSION" ]; then
    reachable_https "https://github.com" \
      || die "preflight: cannot reach github.com over https (the release binary and SHA256SUMS are downloaded from there)"
  fi

  if [ "$INSTALL_CODE_MODE" -eq 1 ]; then
    reachable_https "$SERVER_URL/healthz" \
      || die "preflight: cannot reach $SERVER_URL over https"
  fi

  # The plain libp2p flow knows its server address up front, so it gets a
  # TCP check here; install-code mode only learns its address from the
  # redeem response, after the code is already spent, so it gets no
  # equivalent check at all — see the "install-code mode" section's own
  # comment on why a false negative there is worse than skipping it.
  if [ "$INSTALL_CODE_MODE" -eq 0 ] && [ "$TRANSPORT" = libp2p ] && [ -n "$LIBP2P_SERVER_ADDR" ]; then
    multiaddr_host_port "$LIBP2P_SERVER_ADDR"
    if [ -n "$MA_PORT" ]; then
      tcp_reachable "$MA_HOST" "$MA_PORT" \
        || die "preflight: cannot open a TCP connection to $MA_HOST:$MA_PORT (--server-addr)"
    fi
  fi

  # Soft check: warn, never fail. An absent docker group means container
  # inventory is simply empty, not broken (see the drop-in block below) —
  # real hosts only ("getent group" on a staged test's own host is a
  # question about the test runner, not the install being simulated).
  if [ "$STAGED" -eq 0 ] && ! getent group docker >/dev/null 2>&1; then
    echo "install.sh: warning: no 'docker' group on this host — container inventory will be empty until one exists and the service is restarted" >&2
  fi
}

# write_embedded_unit DEST writes the embedded systemd unit to DEST via a
# temp file in the same directory, then installs it with the usual
# mode/owner rules. Embedded rather than read from
# $SCRIPT_DIR/atlas-agent.service: the UI flow downloads install.sh alone,
# with no sibling file ever present, and this is the one and only code
# path that installs the unit, for every flow. atlas-agent.service in
# this directory remains the source of truth — a test extracts this exact
# heredoc body and diffs it byte-for-byte against that file, so they
# cannot drift apart.
write_embedded_unit() {
  local dest="$1" tmp
  tmp="$(mktemp "$(dirname "$dest")/.atlas-agent.service.XXXXXX")"
  cat > "$tmp" <<'ATLAS_AGENT_SERVICE_EOF'
[Unit]
Description=Atlas Agent — host observation for the Atlas control plane
Documentation=https://github.com/hexane/atlas
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
ExecStart=/usr/local/bin/atlas-agent
EnvironmentFile=/etc/atlas-agent/atlas-agent.env

# Restart unconditionally. A control-plane outage is handled inside the
# agent (spool + backoff, see internal/core/transport/remote) and must
# never cause this process to exit; Restart=always covers everything else
# — an unhandled panic, an OOM kill — without a burst limit that could
# leave a host permanently unmonitored after a bad patch of transient
# failures.
Restart=always
RestartSec=5s

# --- Identity -----------------------------------------------------------
User=atlas
Group=atlas
# Docker container inventory needs group access to /var/run/docker.sock.
# Referencing a group that does not exist on the host is a hard startup
# failure (systemd exit 216/GROUP), so this is not set here — a host with
# no 'docker' group must still start cleanly. install.sh adds it via a
# drop-in at atlas-agent.service.d/10-docker-group.conf only when the
# group actually exists.

# --- Directories ----------------------------------------------------------
# systemd creates and owns these; install.sh does not need to.
StateDirectory=atlas-agent
StateDirectoryMode=0700
ConfigurationDirectory=atlas-agent
ConfigurationDirectoryMode=0750

# --- Hardening --------------------------------------------------------
# Deny-by-default sandboxing. The only capability granted is CAP_SYS_PTRACE,
# for Ports socket-to-process attribution — see the note above that line.
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=/var/lib/atlas-agent
ProtectHome=true
PrivateTmp=true
ProtectClock=true
ProtectHostname=true
ProtectKernelLogs=true
ProtectKernelModules=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
RestrictRealtime=true
RestrictNamespaces=true
LockPersonality=true
MemoryDenyWriteExecute=true
RemoveIPC=true
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallFilter=@system-service
SystemCallErrorNumber=EPERM

# Full socket-to-process attribution on the Ports page needs CAP_SYS_PTRACE
# to read other users' /proc/[pid]/fd entries — without it, sockets owned by
# another user report as unattributed (see docs/architecture/agent-design.md
# sec 3). This is the one and only capability granted: the plugin reads
# /proc, it never calls ptrace(2), so SystemCallFilter is left untouched. It
# is also the minimum — the cross-UID /proc/[pid]/fd read is gated by the
# kernel's ptrace_may_access() check, which no narrower capability satisfies.
CapabilityBoundingSet=CAP_SYS_PTRACE
AmbientCapabilities=CAP_SYS_PTRACE

[Install]
WantedBy=multi-user.target
ATLAS_AGENT_SERVICE_EOF
  install "${OWN_ROOT[@]}" -m 0644 "$tmp" "$dest"
  rm -f "$tmp"
}

# swap_binary SRC installs SRC as $BIN_DEST atomically: a temp file in the
# same directory, then a rename over the old one.
swap_binary() {
  local tmp
  tmp="$(mktemp "$(dirname "$BIN_DEST")/.atlas-agent.XXXXXX")"
  install "${OWN_ROOT[@]}" -m 0755 "$1" "$tmp"
  mv -f "$tmp" "$BIN_DEST"
}

# service_healthy sets HEALTH_ERR and returns non-zero unless the unit stays
# active for HEALTH_SECONDS with no automatic restart. That is all it
# proves — the process did not crash-loop. It says nothing about whether
# the agent ever reached the control plane or is reporting as a node; only
# the Nodes page in the UI can confirm that.
service_healthy() {
  local before after state i
  HEALTH_ERR=""
  before="$(systemctl show -p NRestarts --value atlas-agent 2>/dev/null || true)"
  for ((i = 0; i <= HEALTH_SECONDS; i++)); do
    state="$(systemctl is-active atlas-agent 2>/dev/null || true)"
    if [ "$state" != active ]; then
      HEALTH_ERR="systemctl is-active was '$state' ${i}s after the restart"
      return 1
    fi
    [ "$i" -lt "$HEALTH_SECONDS" ] && sleep 1
  done
  after="$(systemctl show -p NRestarts --value atlas-agent 2>/dev/null || true)"
  if ! [[ "$before" =~ ^[0-9]+$ && "$after" =~ ^[0-9]+$ ]]; then
    HEALTH_ERR="could not read NRestarts (before '$before', after '$after')"
    return 1
  fi
  if [ "$after" -gt "$before" ]; then
    HEALTH_ERR="the service restarted on its own (NRestarts $before -> $after)"
    return 1
  fi
}

rollback() {
  echo "install.sh: upgrade failed: $1" >&2
  echo "==> Restoring the previous binary from $BIN_DEST.prev" >&2
  swap_binary "$BIN_DEST.prev"
  systemctl restart atlas-agent || true
  echo "    restored; atlas-agent is now '$(systemctl is-active atlas-agent 2>/dev/null || true)'" >&2
  exit 1
}

# ---------------------------------------------------------------------------
# upgrade mode: the binary and nothing else
# ---------------------------------------------------------------------------
if [ "$MODE" = upgrade ]; then
  if [ "$STAGED" -eq 1 ] && [ "${ATLAS_INSTALL_TEST:-}" != 1 ]; then
    die "--upgrade acts on the running service; DESTDIR is accepted with it only by the test suite"
  fi
  if [ "${#CONN_FLAGS[@]}" -gt 0 ]; then
    die "--upgrade replaces only the binary and takes no connection flags (got: ${CONN_FLAGS[*]}). To change the configuration, re-install with --force-env."
  fi
  [ "$FORCE_ENV" -eq 0 ] || die "--force-env is not used with --upgrade"
  [ "$START" -eq 1 ] || die "--no-start is not used with --upgrade"
  if [ -z "$RELEASE_VERSION" ] && [ "$BINARY_SET" -eq 0 ]; then
    die "--upgrade needs --version vX.Y.Z or --binary PATH"
  fi
  [ -f "$ENV_FILE" ] || die "no $ENV_FILE: there is no configured atlas-agent here to upgrade. Run a normal install first."
  [ -x "$BIN_DEST" ] || die "no installed binary at $BIN_DEST to upgrade"

  acquire_binary
  probe_version "$BINARY"
  TARGET="$GOT_VERSION"
  if [ -n "$RELEASE_VERSION" ] && [ "$TARGET" != "$RELEASE_VERSION" ]; then
    die "the downloaded binary reports $TARGET, not $RELEASE_VERSION. Nothing was installed."
  fi

  echo "==> Keeping the current binary as $BIN_DEST.prev"
  prev_tmp="$(mktemp "$(dirname "$BIN_DEST")/.atlas-agent.prev.XXXXXX")"
  install "${OWN_ROOT[@]}" -m 0755 "$BIN_DEST" "$prev_tmp"
  mv -f "$prev_tmp" "$BIN_DEST.prev"

  echo "==> Installing $TARGET to $BIN_DEST"
  swap_binary "$BINARY"

  echo "==> Restarting atlas-agent; it must stay active for ${HEALTH_SECONDS}s"
  systemctl restart atlas-agent || rollback "systemctl restart atlas-agent failed"
  service_healthy || rollback "$HEALTH_ERR"
  binary_version "$BIN_DEST" || rollback "$BIN_DEST --version failed after the upgrade"
  [ "$GOT_VERSION" = "$TARGET" ] || rollback "$BIN_DEST reports $GOT_VERSION, want $TARGET"

  echo "==> Upgraded to $TARGET; atlas-agent is active (previous binary kept at $BIN_DEST.prev)"
  exit 0
fi

# ---------------------------------------------------------------------------
# install mode
# ---------------------------------------------------------------------------
if [ -f "$ENV_FILE" ] && [ "$FORCE_ENV" -ne 1 ]; then
  if [ "${#CONN_FLAGS[@]}" -gt 0 ]; then
    die "$ENV_FILE already exists, so these flags would be ignored: ${CONN_FLAGS[*]}. Pass --force-env to rewrite it, or use --upgrade to replace only the binary."
  fi
  die "$ENV_FILE already exists. Use --upgrade to replace only the binary, or --force-env with the full set of connection flags to rewrite it."
fi
# install-code mode only, and only here: a leftover $BIN_DEST or
# $AGENT_DATA_DIR with no $ENV_FILE (the check above already refused the
# normal "already configured" case) is still not a host this run may touch
# — it did not create either path, so it must not claim, let alone delete,
# either one. This runs BEFORE INSTALL_CODE_FRESH_INSTALL is set for exactly
# that reason: that flag is cleanup()'s permission to delete $BIN_DEST and
# rm -rf $AGENT_DATA_DIR on a later failure, and it must never be granted
# over state this run did not create.
if [ "$INSTALL_CODE_MODE" -eq 1 ] && { [ -e "$BIN_DEST" ] || [ -e "$AGENT_DATA_DIR" ]; }; then
  die "$BIN_DEST or $AGENT_DATA_DIR already exists on this host with no configured $ENV_FILE — refusing to risk cloning an existing identity. Remove it by hand first if that is intended, or use --upgrade/--force-env for a normal re-install."
fi
INSTALL_CODE_FRESH_INSTALL=1
if [ "$INSTALL_CODE_MODE" -eq 0 ]; then
  [ -n "$CONTROL_PLANE_URL" ] || die "--control-plane-url is required"

  case "$TRANSPORT" in
    https)
      if [ -n "$LIBP2P_SERVER_ADDR$LIBP2P_RELAY_ADDR$LIBP2P_SERVER_PEER_ID" ]; then
        die "--server-addr / --relay-addr / --server-peer-id require --transport libp2p"
      fi
      if [ -z "$CA_BUNDLE" ] && [ "$INSECURE_BOOTSTRAP" -ne 1 ]; then
        echo "install.sh: pass --ca-bundle PATH for a verified bootstrap, or" >&2
        echo "            --insecure-bootstrap to trust the certificate presented on first contact" >&2
        exit 1
      fi
      ;;
    libp2p)
      [ -z "$TOKEN" ] || die "--token is not used on the libp2p transport (Peer ID authorizes the agent)"
      [ -z "$CA_BUNDLE" ] || die "--ca-bundle is not used on the libp2p transport"
      [ "$INSECURE_BOOTSTRAP" -eq 0 ] || die "--insecure-bootstrap is not used on the libp2p transport"
      if [ -n "$LIBP2P_SERVER_ADDR" ]; then
        [ -z "$LIBP2P_RELAY_ADDR$LIBP2P_SERVER_PEER_ID" ] \
          || die "use --server-addr alone, or --relay-addr with --server-peer-id, not both"
      elif [ -n "$LIBP2P_RELAY_ADDR" ] && [ -n "$LIBP2P_SERVER_PEER_ID" ]; then
        :
      else
        die "libp2p needs --server-addr, or both --relay-addr and --server-peer-id"
      fi
      ;;
    *)
      die "unknown --transport '$TRANSPORT' (https or libp2p)"
      ;;
  esac
fi
# install-code mode supplies none of the above itself: --server-url is only
# the https endpoint to redeem against, and TRANSPORT/CONTROL_PLANE_URL/
# LIBP2P_SERVER_ADDR are set further down, from the redeem response, once it
# succeeds.

if [ -n "$RELATIONSHIP" ]; then
  printf '%s' "$RELATIONSHIP" | grep -Eq '^[A-Za-z0-9_-]+$' \
    || die "invalid --relationship id '$RELATIONSHIP' (only letters, digits, '_' and '-')"
  [ "$RELATIONSHIP" != "default" ] || die "'default' is reserved for the implicit relationship"
fi

run_preflight

acquire_binary
if [ -n "$RELEASE_VERSION" ]; then
  # The atlas user must exist to run the verified download unprivileged.
  ensure_atlas_user
  probe_version "$BINARY"
  [ "$GOT_VERSION" = "$RELEASE_VERSION" ] \
    || die "the downloaded binary reports $GOT_VERSION, not $RELEASE_VERSION. Nothing was installed."
fi

echo "==> Installing binary to $BIN_DEST"
install -d "$(dirname "$BIN_DEST")"
install "${OWN_ROOT[@]}" -m 0755 "$BINARY" "$BIN_DEST"

ensure_atlas_user

if [ "$STAGED" -eq 0 ] && getent group docker >/dev/null; then
  echo "==> Granting the 'docker' group via a systemd drop-in (container inventory)"
  # Not SupplementaryGroups= in the base unit: a host with no 'docker' group
  # would then fail to start at all (systemd exit 216/GROUP) rather than just
  # running without container inventory. A drop-in is written only when the
  # group actually exists on this host.
  install -d -m 0755 "$DROPIN_DIR"
  printf '[Service]\nSupplementaryGroups=docker\n' > "$DROPIN_DIR/10-docker-group.conf"
elif [ "$STAGED" -eq 0 ]; then
  rm -f "$DROPIN_DIR/10-docker-group.conf" 2>/dev/null || true
fi

if [ "$INSTALL_CODE_MODE" -eq 1 ]; then
  echo "==> Generating agent identity"
  # atlas:atlas, not root:atlas — see OWN_ATLAS_DATA's comment above. The
  # same user owns this directory at generation time and at service runtime
  # (ExecStart runs as User=atlas/Group=atlas), so the key stays readable.
  install -d "${OWN_ATLAS_DATA[@]}" -m 0700 "$AGENT_DATA_DIR"
  PEER_ID="$(agent_peer_id "$AGENT_DATA_DIR")" || die "could not generate the agent identity"
  printf '%s' "$PEER_ID" | grep -Eq '^(12D3Koo|Qm)' || die "atlas-agent peer-id produced an unexpected value"

  echo "==> Redeeming install code"
  resp_file="$(mktemp)"
  redeem_body="$(printf '{"code":"%s","peer_id":"%s"}' "$INSTALL_CODE" "$PEER_ID")"
  http_code="$(printf '%s' "$redeem_body" | curl --proto '=https' --tlsv1.2 -sS \
    --connect-timeout 5 --max-time 15 --max-filesize 65536 \
    -o "$resp_file" -w '%{http_code}' \
    -H 'Content-Type: application/json' --data-binary @- \
    "$SERVER_URL/api/v1/agent/install/redeem")" || http_code=0
  # The code is consumed the instant it crosses the wire above; it has no
  # further use in this process and must not sit in memory any longer than
  # it has to.
  unset INSTALL_CODE ATLAS_INSTALL_CODE redeem_body

  if [ "$http_code" = "401" ]; then
    rm -f "$resp_file"
    die "the install code was rejected: invalid, expired, or already used. Generate a new one in the Atlas UI and try again."
  elif [ "$http_code" != "200" ]; then
    rm -f "$resp_file"
    die "install code redeem failed (HTTP $http_code). Nothing was installed."
  fi

  # From here on, the code is spent on the server regardless of what this
  # script does next: the cleanup() trap below prints the "already
  # consumed" warning for ANY non-zero exit from this point on, not just
  # one routed through an explicit call here — set -e, a signal, anything.
  CODE_REDEEMED=1
  [ "$(wc -c <"$resp_file")" -le 65536 ] || die "redeem response exceeded the size limit"
  json_parses "$resp_file" || die "redeem response did not parse as JSON"
  NODE_ID="$(json_get "$resp_file" node_id)"
  ENVIRONMENT="$(json_get "$resp_file" environment)"
  LIBP2P_SERVER_ADDR="$(json_get "$resp_file" server_addr)"
  rm -f "$resp_file"
  printf '%s' "$NODE_ID" | grep -Eq '^[0-9a-f]{32}$' || die "redeem response node_id is not well-formed"
  printf '%s' "$ENVIRONMENT" | grep -Eq "$ENVIRONMENT_RE" || die "redeem response environment is not well-formed"
  printf '%s' "$LIBP2P_SERVER_ADDR" | grep -Eq "$MULTIADDR_RE" || die "redeem response server_addr is not well-formed"
  case "$LIBP2P_SERVER_ADDR" in
    */p2p/*) ;;
    *) die "redeem response server_addr carries no peer id" ;;
  esac

  # Deliberately no TCP reachability check here: the address is only known
  # after the code is spent, so a false negative (a server that accepts the
  # connection but, being libp2p, never answers back the way a probe
  # expects) would burn a still-valid code for nothing. service_healthy
  # below, after the unit starts, is the real check for "does this address
  # actually work" — a failure there still rolls everything back.

  CONTROL_PLANE_URL="$SERVER_URL"
  TRANSPORT="libp2p"
  echo "    redeemed: node $NODE_ID, environment $ENVIRONMENT"
fi

echo "==> Writing $ENV_FILE"
[ -d "$CONF_DIR" ] || CONF_DIR_CREATED=1
install -d "${OWN_ATLAS[@]}" -m 0750 "$CONF_DIR"
# An existing env file without --force-env was refused above, before
# anything was written.
{
  if [ -n "$RELATIONSHIP" ]; then
    PFX="$(relationship_prefix "$RELATIONSHIP")"
    {
      echo "ATLAS_AGENT_RELATIONSHIPS=$RELATIONSHIP"
      echo "${PFX}CONTROL_PLANE_URL=$CONTROL_PLANE_URL"
      echo "${PFX}TRANSPORT=$TRANSPORT"
      [ -n "$ENVIRONMENT" ] && echo "${PFX}ENVIRONMENT=$ENVIRONMENT"
      if [ "$TRANSPORT" = "libp2p" ]; then
        [ -n "$LIBP2P_SERVER_ADDR" ]    && echo "${PFX}LIBP2P_SERVER_ADDR=$LIBP2P_SERVER_ADDR"
        [ -n "$LIBP2P_RELAY_ADDR" ]     && echo "${PFX}LIBP2P_RELAY_ADDR=$LIBP2P_RELAY_ADDR"
        [ -n "$LIBP2P_SERVER_PEER_ID" ] && echo "${PFX}LIBP2P_SERVER_PEER_ID=$LIBP2P_SERVER_PEER_ID"
      else
        [ -n "$TOKEN" ]                 && echo "${PFX}TOKEN=$TOKEN"
        [ -n "$CA_BUNDLE" ]             && echo "${PFX}CA_BUNDLE=$CA_BUNDLE"
        [ "$INSECURE_BOOTSTRAP" -eq 1 ] && echo "${PFX}INSECURE_BOOTSTRAP=true"
      fi
      [ "$AGENTOPS_CONTAINER_LOGS_DISABLED" -eq 1 ] && echo "${PFX}AGENTOPS_CONTAINER_LOGS_DISABLED=true"
    } > "$ENV_FILE"
  else
    {
      echo "ATLAS_AGENT_CONTROL_PLANE_URL=$CONTROL_PLANE_URL"
      echo "ATLAS_AGENT_ENVIRONMENT=$ENVIRONMENT"
      [ -n "$NODE_ID" ] && echo "ATLAS_AGENT_NODE_ID=$NODE_ID"
      # TRANSPORT defaults to "https"; a bare --transport libp2p (no
      # --relationship) previously validated but was never actually written
      # here, leaving the agent to fall back to its own https default. Fixed
      # for both that case and install-code mode, which always sets it.
      if [ "$TRANSPORT" = "libp2p" ]; then
        echo "ATLAS_AGENT_TRANSPORT=libp2p"
        [ -n "$LIBP2P_SERVER_ADDR" ]    && echo "ATLAS_AGENT_LIBP2P_SERVER_ADDR=$LIBP2P_SERVER_ADDR"
        [ -n "$LIBP2P_RELAY_ADDR" ]     && echo "ATLAS_AGENT_LIBP2P_RELAY_ADDR=$LIBP2P_RELAY_ADDR"
        [ -n "$LIBP2P_SERVER_PEER_ID" ] && echo "ATLAS_AGENT_LIBP2P_SERVER_PEER_ID=$LIBP2P_SERVER_PEER_ID"
      else
        echo "ATLAS_AGENT_TOKEN=$TOKEN"
        [ -n "$CA_BUNDLE" ] && echo "ATLAS_AGENT_CA_BUNDLE=$CA_BUNDLE"
        [ "$INSECURE_BOOTSTRAP" -eq 1 ] && echo "ATLAS_AGENT_INSECURE_BOOTSTRAP=true"
      fi
    } > "$ENV_FILE"
  fi
  [ "$STAGED" -eq 0 ] && chown root:atlas "$ENV_FILE"
  chmod 0640 "$ENV_FILE"
}

echo "==> Installing systemd unit"
install -d "$(dirname "$UNIT_DEST")"
write_embedded_unit "$UNIT_DEST"
if [ "$STAGED" -eq 1 ] && { [ "$INSTALL_CODE_MODE" -eq 0 ] || [ "${ATLAS_INSTALL_TEST:-}" != 1 ]; }; then
  # A plain staged install (no systemd on the test host) is already a
  # complete, successful install-code run too — env file and unit on disk is
  # exactly what a real run would have at this point, absent the actual
  # systemd actions below. ATLAS_INSTALL_TEST=1 is the same escape hatch
  # --upgrade's staged tests already use, to additionally exercise the
  # start/health-check path below against the stub systemctl.
  INSTALL_CODE_COMMITTED=1
  echo "==> Staged install complete under $DESTDIR (no systemd actions taken)."
  exit 0
fi
systemctl daemon-reload
systemctl enable atlas-agent >/dev/null
INSTALL_CODE_SERVICE_TOUCHED=1

if [ "$INSTALL_CODE_MODE" -eq 1 ]; then
  if [ "$START" -eq 1 ]; then
    echo "==> Starting atlas-agent; it must stay active for ${HEALTH_SECONDS}s"
    systemctl restart atlas-agent || die "systemctl restart atlas-agent failed"
    # service_healthy only proves the process stayed up for ${HEALTH_SECONDS}s
    # with no automatic restart — not that it reached the control plane, let
    # alone that the node is reporting. See the final success message below.
    service_healthy || die "$HEALTH_ERR"
  else
    echo "==> Service enabled, not started (--no-start). Start with:"
    echo "      systemctl start atlas-agent"
  fi
  INSTALL_CODE_COMMITTED=1
  echo "==> atlas-agent is installed and the service is running (node $NODE_ID, environment $ENVIRONMENT)."
  echo "    Confirm on the Nodes page in the Atlas UI that this node shows as reporting."
elif [ "$START" -eq 1 ]; then
  if [ "$TRANSPORT" = "https" ] && [ -z "$TOKEN" ] && [ ! -f "$DATA_DIR/agent-cert.pem" ]; then
    echo "==> No --token given and no certificate on disk yet."
    echo "    Edit $ENV_FILE, set ATLAS_AGENT_TOKEN, then:"
    echo "      systemctl start atlas-agent"
    exit 0
  fi
  echo "==> Starting atlas-agent"
  systemctl restart atlas-agent
  sleep 2
  systemctl --no-pager status atlas-agent || true
else
  echo "==> Service enabled, not started (--no-start). Start with:"
  echo "      systemctl start atlas-agent"
fi
