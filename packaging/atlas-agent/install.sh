#!/usr/bin/env bash
# Installs atlas-agent as a systemd service, or verifies an existing install.
#
# Usage:
#   install.sh --control-plane-url URL --token TOKEN [options]     # https
#   install.sh --transport libp2p --relationship NAME \            # libp2p
#     --control-plane-url URL --server-addr MULTIADDR [options]
#   install.sh --verify [--relationship NAME] [--data-dir DIR] [options]
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
#   --agentops-container-logs-disabled
#                            Set ..._AGENTOPS_CONTAINER_LOGS_DISABLED=true
#   --no-start              Enable the service but do not start it
#   --force-env             Overwrite an existing atlas-agent.env
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

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --control-plane-url) CONTROL_PLANE_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --environment) ENVIRONMENT="$2"; shift 2 ;;
    --ca-bundle) CA_BUNDLE="$2"; shift 2 ;;
    --insecure-bootstrap) INSECURE_BOOTSTRAP=1; shift ;;
    --transport) TRANSPORT="$2"; shift 2 ;;
    --relationship) RELATIONSHIP="$2"; shift 2 ;;
    --server-addr) LIBP2P_SERVER_ADDR="$2"; shift 2 ;;
    --relay-addr) LIBP2P_RELAY_ADDR="$2"; shift 2 ;;
    --server-peer-id) LIBP2P_SERVER_PEER_ID="$2"; shift 2 ;;
    --agentops-container-logs-disabled) AGENTOPS_CONTAINER_LOGS_DISABLED=1; shift ;;
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
# DATA_DIR is fixed by the unit's StateDirectory= for a real install; the
# --data-dir flag only redirects where --verify looks.

# ---------------------------------------------------------------------------
# verify mode
# ---------------------------------------------------------------------------
if [ "$MODE" = verify ]; then
  [ "$BINARY" = "$SCRIPT_DIR/atlas-agent" ] && BINARY="$BIN_DEST"
  fails=0
  pass() { printf '  ok    %s\n' "$1"; }
  fail() { printf '  FAIL  %s\n' "$1" >&2; fails=$((fails + 1)); }
  warn() { printf '  warn  %s\n' "$1"; }   # advisory only — does not fail --verify

  json_get() {
    # json_get FILE KEY  ->  prints the string value of a top-level "KEY"
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
# install mode
# ---------------------------------------------------------------------------
# STAGED is true when DESTDIR is set: write the tree under the prefix, own
# files as the calling user, and touch nothing system-wide (no useradd, no
# systemctl). A real install (DESTDIR empty) needs root and does all of it.
STAGED=0
[ -n "$DESTDIR" ] && STAGED=1
OWN_ROOT=(-o root -g root)
OWN_ATLAS=(-o root -g atlas)
if [ "$STAGED" -eq 1 ]; then OWN_ROOT=(); OWN_ATLAS=(); fi

if [ "$STAGED" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
  die "must run as root (try sudo), or set DESTDIR to stage the install"
fi
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

if [ -n "$RELATIONSHIP" ]; then
  printf '%s' "$RELATIONSHIP" | grep -Eq '^[A-Za-z0-9_-]+$' \
    || die "invalid --relationship id '$RELATIONSHIP' (only letters, digits, '_' and '-')"
  [ "$RELATIONSHIP" != "default" ] || die "'default' is reserved for the implicit relationship"
fi

if [ ! -x "$BINARY" ]; then
  die "no executable binary at $BINARY (pass --binary PATH)"
fi

echo "==> Installing binary to $BIN_DEST"
install -d "$(dirname "$BIN_DEST")"
install "${OWN_ROOT[@]}" -m 0755 "$BINARY" "$BIN_DEST"

if [ "$STAGED" -eq 0 ]; then
  echo "==> Creating system user 'atlas'"
  if ! getent passwd atlas >/dev/null; then
    useradd --system --no-create-home --home-dir /var/lib/atlas-agent \
      --shell /usr/sbin/nologin atlas
  fi
fi

DROPIN_DIR="$DESTDIR/etc/systemd/system/atlas-agent.service.d"
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

echo "==> Writing $ENV_FILE"
install -d "${OWN_ATLAS[@]}" -m 0750 "$CONF_DIR"
if [ -f "$ENV_FILE" ] && [ "$FORCE_ENV" -ne 1 ]; then
  echo "    $ENV_FILE already exists; leaving it as-is (--force-env to overwrite)"
else
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
      echo "ATLAS_AGENT_TOKEN=$TOKEN"
      echo "ATLAS_AGENT_ENVIRONMENT=$ENVIRONMENT"
      [ -n "$CA_BUNDLE" ] && echo "ATLAS_AGENT_CA_BUNDLE=$CA_BUNDLE"
      [ "$INSECURE_BOOTSTRAP" -eq 1 ] && echo "ATLAS_AGENT_INSECURE_BOOTSTRAP=true"
    } > "$ENV_FILE"
  fi
  [ "$STAGED" -eq 0 ] && chown root:atlas "$ENV_FILE"
  chmod 0640 "$ENV_FILE"
fi

echo "==> Installing systemd unit"
install -d "$(dirname "$UNIT_DEST")"
install "${OWN_ROOT[@]}" -m 0644 "$SCRIPT_DIR/atlas-agent.service" "$UNIT_DEST"
if [ "$STAGED" -eq 1 ]; then
  echo "==> Staged install complete under $DESTDIR (no systemd actions taken)."
  exit 0
fi
systemctl daemon-reload
systemctl enable atlas-agent >/dev/null

if [ "$START" -eq 1 ]; then
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
