#!/usr/bin/env bash
# Removes the atlas-agent systemd service.
#
# Usage:
#   uninstall.sh            # stop, disable, remove binary and unit;
#                            # leaves /var/lib/atlas-agent and /etc/atlas-agent
#   uninstall.sh --purge    # also remove data, config, and the 'atlas' user
#
# Idempotent: safe to run twice in a row, and safe to run when nothing is
# installed at all — every removal is already-gone-is-fine, and nothing
# here fails just because there was nothing to undo.
set -euo pipefail

PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

# DESTDIR mirrors install.sh's own staged-install convention — test-only:
# empty (the default) is a real uninstall; set it to stage removals under a
# prefix with no root and no systemd actions, so this can be exercised
# without needing a real host.
DESTDIR="${DESTDIR:-}"
STAGED=0
[ -n "$DESTDIR" ] && STAGED=1

if [ "$STAGED" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
  echo "uninstall.sh: must run as root (try sudo), or set DESTDIR to stage" >&2
  exit 1
fi

if [ "$STAGED" -eq 0 ]; then
  echo "==> Stopping atlas-agent"
  systemctl stop atlas-agent 2>/dev/null || true
  systemctl disable atlas-agent 2>/dev/null || true
fi

echo "==> Removing systemd unit"
rm -f "$DESTDIR/etc/systemd/system/atlas-agent.service"
# The drop-in install.sh adds when a 'docker' group exists (see its own
# comment) — both the file and, since rmdir only ever succeeds on an empty
# directory, the now-empty directory itself. A manually added drop-in of
# some other name would leave the directory non-empty, and rmdir simply
# no-ops rather than remove it.
rm -f "$DESTDIR/etc/systemd/system/atlas-agent.service.d/10-docker-group.conf"
rmdir "$DESTDIR/etc/systemd/system/atlas-agent.service.d" 2>/dev/null || true
[ "$STAGED" -eq 0 ] && systemctl daemon-reload

echo "==> Removing binary"
rm -f "$DESTDIR/usr/local/bin/atlas-agent"
# --upgrade's kept-previous-version backup; stale once the binary it was a
# backup for is gone.
rm -f "$DESTDIR/usr/local/bin/atlas-agent.prev"

if [ "$PURGE" -eq 1 ]; then
  echo "==> Purging data, config, and the 'atlas' user"
  rm -rf "$DESTDIR/var/lib/atlas-agent" "$DESTDIR/etc/atlas-agent"
  if [ "$STAGED" -eq 0 ]; then
    getent passwd atlas >/dev/null && userdel atlas 2>/dev/null || true
  fi
else
  echo "==> Leaving /var/lib/atlas-agent and /etc/atlas-agent in place"
  echo "    (certificate, spool, and config survive a reinstall)"
  echo "    Re-run with --purge to remove them."
fi
