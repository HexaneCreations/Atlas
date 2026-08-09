#!/usr/bin/env bash
# Installs atlas-agent as a systemd service.
#
# Usage:
#   install.sh --control-plane-url URL --token TOKEN [options]
#   install.sh --control-plane-url URL   # install without starting; edit
#                                         # the env file and start manually
#
# Options:
#   --binary PATH          Agent binary to install (default: ./atlas-agent
#                           next to this script)
#   --control-plane-url URL
#   --token TOKEN           Enrollment token (see `atlas-server enroll-token`)
#   --environment NAME      Operator-assigned environment tag
#   --ca-bundle PATH        CA certificate to pin instead of trust-on-first-use
#   --no-start              Enable the service but do not start it
#   --force-env             Overwrite an existing /etc/atlas-agent/atlas-agent.env
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

BINARY="$SCRIPT_DIR/atlas-agent"
CONTROL_PLANE_URL=""
TOKEN=""
ENVIRONMENT=""
CA_BUNDLE=""
START=1
FORCE_ENV=0

while [ $# -gt 0 ]; do
  case "$1" in
    --binary) BINARY="$2"; shift 2 ;;
    --control-plane-url) CONTROL_PLANE_URL="$2"; shift 2 ;;
    --token) TOKEN="$2"; shift 2 ;;
    --environment) ENVIRONMENT="$2"; shift 2 ;;
    --ca-bundle) CA_BUNDLE="$2"; shift 2 ;;
    --no-start) START=0; shift ;;
    --force-env) FORCE_ENV=1; shift ;;
    -h|--help) grep '^#' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "install.sh: unknown option $1" >&2; exit 1 ;;
  esac
done

if [ "$(id -u)" -ne 0 ]; then
  echo "install.sh: must run as root (try sudo)" >&2
  exit 1
fi

if [ -z "$CONTROL_PLANE_URL" ]; then
  echo "install.sh: --control-plane-url is required" >&2
  exit 1
fi

if [ ! -x "$BINARY" ]; then
  echo "install.sh: no executable binary at $BINARY (pass --binary PATH)" >&2
  exit 1
fi

BIN_DEST=/usr/local/bin/atlas-agent
CONF_DIR=/etc/atlas-agent
ENV_FILE="$CONF_DIR/atlas-agent.env"
UNIT_DEST=/etc/systemd/system/atlas-agent.service
DATA_DIR=/var/lib/atlas-agent

echo "==> Installing binary to $BIN_DEST"
install -o root -g root -m 0755 "$BINARY" "$BIN_DEST"

echo "==> Creating system user 'atlas'"
if ! getent passwd atlas >/dev/null; then
  useradd --system --no-create-home --home-dir "$DATA_DIR" \
    --shell /usr/sbin/nologin atlas
fi

DROPIN_DIR=/etc/systemd/system/atlas-agent.service.d
if getent group docker >/dev/null; then
  echo "==> Granting the 'docker' group via a systemd drop-in (container inventory)"
  # Not SupplementaryGroups= in the base unit: a host with no 'docker' group
  # would then fail to start at all (systemd exit 216/GROUP) rather than just
  # running without container inventory. A drop-in is written only when the
  # group actually exists on this host.
  install -d -m 0755 "$DROPIN_DIR"
  printf '[Service]\nSupplementaryGroups=docker\n' > "$DROPIN_DIR/10-docker-group.conf"
else
  rm -f "$DROPIN_DIR/10-docker-group.conf" 2>/dev/null || true
fi

echo "==> Writing $CONF_DIR/atlas-agent.env"
install -d -o root -g atlas -m 0750 "$CONF_DIR"
if [ -f "$ENV_FILE" ] && [ "$FORCE_ENV" -ne 1 ]; then
  echo "    $ENV_FILE already exists; leaving it as-is (--force-env to overwrite)"
else
  {
    echo "ATLAS_AGENT_CONTROL_PLANE_URL=$CONTROL_PLANE_URL"
    echo "ATLAS_AGENT_TOKEN=$TOKEN"
    echo "ATLAS_AGENT_ENVIRONMENT=$ENVIRONMENT"
    [ -n "$CA_BUNDLE" ] && echo "ATLAS_AGENT_CA_BUNDLE=$CA_BUNDLE"
  } > "$ENV_FILE"
  chown root:atlas "$ENV_FILE"
  chmod 0640 "$ENV_FILE"
fi

echo "==> Installing systemd unit"
install -o root -g root -m 0644 "$SCRIPT_DIR/atlas-agent.service" "$UNIT_DEST"
systemctl daemon-reload
systemctl enable atlas-agent >/dev/null

if [ "$START" -eq 1 ]; then
  if [ -z "$TOKEN" ] && [ ! -f "$DATA_DIR/agent-cert.pem" ]; then
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
