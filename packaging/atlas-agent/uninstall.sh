#!/usr/bin/env bash
# Removes the atlas-agent systemd service.
#
# Usage:
#   uninstall.sh            # stop, disable, remove binary and unit;
#                            # leaves /var/lib/atlas-agent and /etc/atlas-agent
#   uninstall.sh --purge    # also remove data, config, and the 'atlas' user
set -euo pipefail

PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

if [ "$(id -u)" -ne 0 ]; then
  echo "uninstall.sh: must run as root (try sudo)" >&2
  exit 1
fi

echo "==> Stopping atlas-agent"
systemctl stop atlas-agent 2>/dev/null || true
systemctl disable atlas-agent 2>/dev/null || true

echo "==> Removing systemd unit"
rm -f /etc/systemd/system/atlas-agent.service
systemctl daemon-reload

echo "==> Removing binary"
rm -f /usr/local/bin/atlas-agent

if [ "$PURGE" -eq 1 ]; then
  echo "==> Purging data, config, and the 'atlas' user"
  rm -rf /var/lib/atlas-agent /etc/atlas-agent
  getent passwd atlas >/dev/null && userdel atlas 2>/dev/null || true
else
  echo "==> Leaving /var/lib/atlas-agent and /etc/atlas-agent in place"
  echo "    (certificate, spool, and config survive a reinstall)"
  echo "    Re-run with --purge to remove them."
fi
