#!/usr/bin/env bash
# Staged (DESTDIR) tests for uninstall.sh. Plain bash, no root, no network —
# DESTDIR mirrors install.sh's own staged-install convention (test-only; see
# uninstall.sh's own comment).
#
#   bash packaging/atlas-agent/test/uninstall_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
UNINSTALL_SH="$HERE/../uninstall.sh"

PASS=0
FAIL=0
CURRENT=""

fail() { echo "  FAIL  $CURRENT: $*"; FAIL=$((FAIL + 1)); CASE_OK=0; }

setup() {
  CURRENT="$1"
  CASE_OK=1
  SANDBOX="$(mktemp -d)"
  export DESTDIR="$SANDBOX/root"
}

teardown() {
  rm -rf "$SANDBOX"
  if [ "$CASE_OK" -eq 1 ]; then echo "  ok    $CURRENT"; PASS=$((PASS + 1)); fi
}

run_uninstall() {
  OUT="$SANDBOX/out.txt"
  set +e
  bash "$UNINSTALL_SH" "$@" >"$OUT" 2>&1
  RC=$?
  set -e
}

expect_rc_zero() { [ "$RC" -eq 0 ] || fail "exit $RC; output: $(cat "$OUT")"; }

# seed_full_install: everything a real install leaves behind, including the
# docker-group drop-in and a kept .prev binary from a past --upgrade.
seed_full_install() {
  local d="$DESTDIR"
  mkdir -p "$d/usr/local/bin" "$d/etc/systemd/system/atlas-agent.service.d" \
    "$d/var/lib/atlas-agent" "$d/etc/atlas-agent"
  printf 'binary\n' >"$d/usr/local/bin/atlas-agent"
  printf 'prev-binary\n' >"$d/usr/local/bin/atlas-agent.prev"
  printf '[Unit]\n' >"$d/etc/systemd/system/atlas-agent.service"
  printf '[Service]\nSupplementaryGroups=docker\n' >"$d/etc/systemd/system/atlas-agent.service.d/10-docker-group.conf"
  printf 'key\n' >"$d/var/lib/atlas-agent/p2p-identity.key"
  printf 'env\n' >"$d/etc/atlas-agent/atlas-agent.env"
}

echo "==> uninstall.sh staged tests"

# ---------------------------------------------------------------------------
setup "removes the drop-in file and its now-empty directory"
seed_full_install
run_uninstall
expect_rc_zero
[ ! -e "$DESTDIR/etc/systemd/system/atlas-agent.service.d/10-docker-group.conf" ] || fail "drop-in file not removed"
[ ! -d "$DESTDIR/etc/systemd/system/atlas-agent.service.d" ] || fail "drop-in directory not removed"
teardown

setup "does not remove a drop-in directory holding an unrelated file"
seed_full_install
printf 'unrelated\n' >"$DESTDIR/etc/systemd/system/atlas-agent.service.d/99-something-else.conf"
run_uninstall
expect_rc_zero
[ -d "$DESTDIR/etc/systemd/system/atlas-agent.service.d" ] || fail "non-empty drop-in directory was removed"
teardown

setup "removes the .prev binary backup"
seed_full_install
run_uninstall
expect_rc_zero
[ ! -e "$DESTDIR/usr/local/bin/atlas-agent.prev" ] || fail ".prev binary not removed"
teardown

setup "removes the unit and the binary"
seed_full_install
run_uninstall
expect_rc_zero
[ ! -e "$DESTDIR/etc/systemd/system/atlas-agent.service" ] || fail "unit not removed"
[ ! -e "$DESTDIR/usr/local/bin/atlas-agent" ] || fail "binary not removed"
teardown

setup "leaves /var/lib/atlas-agent and /etc/atlas-agent in place without --purge"
seed_full_install
run_uninstall
expect_rc_zero
[ -f "$DESTDIR/var/lib/atlas-agent/p2p-identity.key" ] || fail "data directory content was removed without --purge"
[ -f "$DESTDIR/etc/atlas-agent/atlas-agent.env" ] || fail "config directory content was removed without --purge"
teardown

setup "--purge removes data and config too"
seed_full_install
run_uninstall --purge
expect_rc_zero
[ ! -d "$DESTDIR/var/lib/atlas-agent" ] || fail "data directory not purged"
[ ! -d "$DESTDIR/etc/atlas-agent" ] || fail "config directory not purged"
teardown

# ---------------------------------------------------------------------------
# idempotency
# ---------------------------------------------------------------------------
setup "idempotent: running twice in a row exits cleanly"
seed_full_install
run_uninstall
expect_rc_zero
run_uninstall
expect_rc_zero
teardown

setup "idempotent: --purge twice in a row exits cleanly"
seed_full_install
run_uninstall --purge
expect_rc_zero
run_uninstall --purge
expect_rc_zero
teardown

setup "idempotent: exits cleanly when nothing is installed at all"
run_uninstall
expect_rc_zero
teardown

setup "idempotent: --purge exits cleanly when nothing is installed at all"
run_uninstall --purge
expect_rc_zero
teardown

echo
echo "==> $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
