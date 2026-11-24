#!/usr/bin/env bash
# Staged (DESTDIR) tests for install.sh. Plain bash, no root, no network:
# curl, systemctl, uname, id, useradd and sleep are stubs on PATH.
#
#   bash packaging/atlas-agent/test/install_test.sh
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_SH="$HERE/../install.sh"

PASS=0
FAIL=0
CURRENT=""

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
  else shasum -a 256 "$1" | awk '{print $1}'; fi
}

# snapshot DIR: every file with its hash, and every directory, sorted.
snapshot() {
  [ -d "$1" ] || { echo "<absent>"; return; }
  (cd "$1" && find . -type d | sort && find . -type f | sort | while read -r f; do
    printf '%s %s\n' "$(sha256_of "$f")" "$f"
  done)
}

fail() { echo "  FAIL  $CURRENT: $*"; FAIL=$((FAIL + 1)); CASE_OK=0; }

# fake_binary PATH VERSION [MARKER]: a stand-in agent whose --version prints
# the same shape as internal/platform/build.Info.String, and whose peer-id
# prints a fixed, format-valid Peer ID (install-code mode tests only).
fake_binary() {
  mkdir -p "$(dirname "$1")"
  cat >"$1" <<EOF
#!/bin/sh
# ${3:-healthy}
if [ "\$1" = --version ]; then
  echo "atlas $2 (abcdef0) built 2026-01-01T00:00:00Z with go1.25 for linux/amd64"
elif [ "\$1" = peer-id ]; then
  echo "12D3KooWTestFixturePeerIdForInstallerUnitTestsXX"
fi
EOF
  chmod 0755 "$1"
}

# setup: fresh sandbox with stubs; exports DESTDIR, STUB_STATE, FIXTURES.
setup() {
  CURRENT="$1"
  CASE_OK=1
  SANDBOX="$(mktemp -d)"
  export DESTDIR="$SANDBOX/root"
  export STUB_STATE="$SANDBOX/state"
  export FIXTURES="$SANDBOX/releases"
  export STUB_ARCH="x86_64"
  STUBS="$SANDBOX/bin"
  mkdir -p "$STUB_STATE" "$FIXTURES" "$STUBS"
  echo 0 >"$STUB_STATE/nrestarts"
  unset ATLAS_INSTALL_TEST

  cat >"$STUBS/curl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$STUB_STATE/curl.log"
out=""; url=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift 2 ;;
    --proto) [ "$2" = "=https" ] || exit 99; shift 2 ;;
    *) url="$1"; shift ;;
  esac
done
case "$url" in
  */api/v1/agent/install/redeem)
    # Drain the piped JSON body (install.sh never reads curl's stdout for
    # this call except via -w, below) and never let it touch disk.
    cat >/dev/null
    if [ -f "$STUB_STATE/redeem_response" ]; then cp "$STUB_STATE/redeem_response" "$out"; else : >"$out"; fi
    if [ -f "$STUB_STATE/redeem_status" ]; then cat "$STUB_STATE/redeem_status"; else printf '200'; fi
    exit 0
    ;;
esac
prefix="https://github.com/HexaneCreations/Atlas/releases/download/"
case "$url" in "$prefix"*) ;; *) exit 98 ;; esac
src="$FIXTURES/${url#"$prefix"}"
[ -f "$src" ] || exit 22
cp "$src" "$out"
EOF
  cat >"$STUBS/systemctl" <<'EOF'
#!/usr/bin/env bash
echo "$*" >>"$STUB_STATE/systemctl.log"
bin="$DESTDIR/usr/local/bin/atlas-agent"
case "$1" in
  is-active)
    if grep -q CRASHES "$bin" 2>/dev/null; then echo activating; exit 3; fi
    echo active ;;
  show)
    n="$(cat "$STUB_STATE/nrestarts")"
    if grep -q FLAPS "$bin" 2>/dev/null; then n=$((n + 1)); echo "$n" >"$STUB_STATE/nrestarts"; fi
    echo "$n" ;;
  *) : ;;
esac
EOF
  cat >"$STUBS/uname" <<'EOF'
#!/bin/sh
case "$1" in -s) echo Linux ;; -m) echo "$STUB_ARCH" ;; *) echo Linux ;; esac
EOF
  # shellcheck disable=SC2016 # the stub's own $1, expanded when it runs
  printf '#!/bin/sh\n[ "$1" = -u ] && echo 1000 && exit 0\necho "uid=1000"\n' >"$STUBS/id"
  printf '#!/bin/sh\necho "useradd must not run in a staged install" >&2\nexit 1\n' >"$STUBS/useradd"
  printf '#!/bin/sh\nexit 0\n' >"$STUBS/sleep"
  chmod 0755 "$STUBS"/*
  export PATH="$STUBS:$ORIG_PATH"

  # A working tree to install from, next to a copy of install.sh.
  WORK="$SANDBOX/pkg"
  mkdir -p "$WORK"
  cp "$INSTALL_SH" "$HERE/../atlas-agent.service" "$WORK/"
}

teardown() {
  export PATH="$ORIG_PATH"
  rm -rf "$SANDBOX"
  if [ "$CASE_OK" -eq 1 ]; then echo "  ok    $CURRENT"; PASS=$((PASS + 1)); fi
}

# publish VERSION ARCH [MARKER] [SUMS=good|bad|none]: a fake release.
publish() {
  local dir="$FIXTURES/$1" asset="atlas-agent-linux-$2"
  fake_binary "$dir/$asset" "$1" "${3:-healthy}"
  case "${4:-good}" in
    good) printf '%s  %s\n' "$(sha256_of "$dir/$asset")" "$asset" >"$dir/SHA256SUMS" ;;
    bad) printf '%s  %s\n' "$(printf '0%.0s' {1..64})" "$asset" >"$dir/SHA256SUMS" ;;
    none) : ;;
  esac
}

run_install() {
  OUT="$SANDBOX/out.txt"
  set +e
  bash "$WORK/install.sh" "$@" >"$OUT" 2>&1
  RC=$?
  set -e
}

# run_install_stdin CODE ARGS...: like run_install, but feeds CODE on stdin
# instead of passing it any other way — for "--install-code -" tests.
run_install_stdin() {
  local code="$1"; shift
  OUT="$SANDBOX/out.txt"
  set +e
  printf '%s\n' "$code" | bash "$WORK/install.sh" "$@" >"$OUT" 2>&1
  RC=$?
  set -e
}

https_flags=(--control-plane-url https://cp.example.com:8443 --token atlas_enroll_0123abcd --insecure-bootstrap)

# A format-valid install code and server url, for install-code mode tests.
TEST_INSTALL_CODE="atlas_install_$(printf '0123456789abcdef%.0s' {1..4})"
TEST_SERVER_URL="https://cp.example.com"

# redeem_ok NODE_ID ENVIRONMENT SERVER_ADDR: makes the curl stub's redeem
# endpoint answer 200 with this body. redeem_status CODE overrides the HTTP
# status for the next redeem call (default 200 when no file is written).
redeem_ok() {
  printf '{"node_id":"%s","environment":"%s","server_addr":"%s"}\n' "$1" "$2" "$3" >"$STUB_STATE/redeem_response"
}
redeem_status() { printf '%s' "$1" >"$STUB_STATE/redeem_status"; }

TEST_NODE_ID="0123456789abcdef0123456789abcdef"
TEST_SERVER_ADDR="/ip4/203.0.113.10/tcp/4102/p2p/12D3KooWD3eckifWpRn9wQpMG9R9hX3sD158z7EqHWmweQAJU5SA"

# seed_install: an existing install the upgrade must leave alone.
seed_install() {
  local d="$DESTDIR"
  fake_binary "$d/usr/local/bin/atlas-agent" v1.0.0
  mkdir -p "$d/etc/atlas-agent" "$d/etc/systemd/system/atlas-agent.service.d" \
    "$d/var/lib/atlas-agent/relationships/prod/spool" "$d/var/lib/atlas-agent/spool"
  printf 'ATLAS_AGENT_RELATIONSHIPS=prod\nATLAS_AGENT_RELATIONSHIP_PROD_TRANSPORT=libp2p\n' >"$d/etc/atlas-agent/atlas-agent.env"
  printf '[Unit]\n# hand-edited unit\n' >"$d/etc/systemd/system/atlas-agent.service"
  printf '[Service]\nSupplementaryGroups=docker\n' >"$d/etc/systemd/system/atlas-agent.service.d/10-docker-group.conf"
  head -c 64 /dev/urandom >"$d/var/lib/atlas-agent/p2p-identity.key"
  printf '0123456789abcdef0123456789abcdef\n' >"$d/var/lib/atlas-agent/node-id"
  printf '{"transport":"libp2p"}\n' >"$d/var/lib/atlas-agent/relationships/prod/relationship.json"
  printf 'cert\n' >"$d/var/lib/atlas-agent/agent-cert.pem"
  printf 'key\n' >"$d/var/lib/atlas-agent/agent-key.pem"
  printf 'segment\n' >"$d/var/lib/atlas-agent/relationships/prod/spool/000001.seg"
  printf 'segment\n' >"$d/var/lib/atlas-agent/spool/000001.seg"
}

# preserved: snapshot of everything except the binary and its .prev.
preserved() {
  snapshot "$DESTDIR" | grep -v -e 'usr/local/bin/atlas-agent' -e '/usr/local/bin/.atlas-agent'
}

expect_rc_nonzero() { [ "$RC" -ne 0 ] || fail "expected a non-zero exit; output: $(cat "$OUT")"; }
expect_rc_zero() { [ "$RC" -eq 0 ] || fail "exit $RC; output: $(cat "$OUT")"; }
expect_output() { grep -qF -- "$1" "$OUT" || fail "output lacks '$1': $(cat "$OUT")"; }
same_file() { cmp -s "$1" "$2" || fail "$3"; }

ORIG_PATH="$PATH"
echo "==> install.sh staged tests"

# ---------------------------------------------------------------------------
setup "install with --binary"
fake_binary "$WORK/atlas-agent" v0.9.0
run_install "${https_flags[@]}" --environment production
expect_rc_zero
same_file "$WORK/atlas-agent" "$DESTDIR/usr/local/bin/atlas-agent" "binary not installed verbatim"
grep -qx 'ATLAS_AGENT_CONTROL_PLANE_URL=https://cp.example.com:8443' "$DESTDIR/etc/atlas-agent/atlas-agent.env" \
  || fail "env file lacks the control-plane URL"
grep -qx 'ATLAS_AGENT_ENVIRONMENT=production' "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the environment"
[ -f "$DESTDIR/etc/systemd/system/atlas-agent.service" ] || fail "unit not installed"
[ ! -f "$STUB_STATE/curl.log" ] || fail "--binary must not download anything"
teardown

# ---------------------------------------------------------------------------
setup "--version happy path (amd64)"
publish v1.2.3 amd64
run_install "${https_flags[@]}" --version v1.2.3
expect_rc_zero
same_file "$FIXTURES/v1.2.3/atlas-agent-linux-amd64" "$DESTDIR/usr/local/bin/atlas-agent" "installed binary is not the release asset"
grep -q -- "--proto =https --tlsv1.2 -fsSL" "$STUB_STATE/curl.log" || fail "curl not called with --proto =https --tlsv1.2 -fsSL"
expect_output "verified"
teardown

setup "--version picks arm64 on aarch64"
export STUB_ARCH=aarch64
publish v1.2.3 arm64
run_install "${https_flags[@]}" --version v1.2.3
expect_rc_zero
same_file "$FIXTURES/v1.2.3/atlas-agent-linux-arm64" "$DESTDIR/usr/local/bin/atlas-agent" "arm64 asset not installed"
teardown

setup "--version refuses an unsupported architecture"
export STUB_ARCH=riscv64
publish v1.2.3 amd64
run_install "${https_flags[@]}" --version v1.2.3
expect_rc_nonzero
expect_output "unsupported architecture 'riscv64'"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "--version with --sha256 matching installs"
publish v1.2.3 amd64
run_install "${https_flags[@]}" --version v1.2.3 --sha256 "$(sha256_of "$FIXTURES/v1.2.3/atlas-agent-linux-amd64" | tr a-f A-F)"
expect_rc_zero
teardown

# ---------------------------------------------------------------------------
setup "SHA256SUMS mismatch aborts with no change"
publish v1.2.3 amd64 healthy bad
run_install "${https_flags[@]}" --version v1.2.3
expect_rc_nonzero
expect_output "checksum mismatch"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "missing SHA256SUMS aborts with no change"
publish v1.2.3 amd64 healthy none
run_install "${https_flags[@]}" --version v1.2.3
expect_rc_nonzero
expect_output "could not download SHA256SUMS"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "--sha256 mismatch aborts with no change"
publish v1.2.3 amd64
run_install "${https_flags[@]}" --version v1.2.3 --sha256 "$(printf 'a%.0s' {1..64})"
expect_rc_nonzero
expect_output "--sha256 is"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "release reporting another version aborts with no change"
publish v1.2.3 amd64
fake_binary "$FIXTURES/v1.2.3/atlas-agent-linux-amd64" v9.9.9
printf '%s  atlas-agent-linux-amd64\n' "$(sha256_of "$FIXTURES/v1.2.3/atlas-agent-linux-amd64")" >"$FIXTURES/v1.2.3/SHA256SUMS"
run_install "${https_flags[@]}" --version v1.2.3
expect_rc_nonzero
expect_output "reports v9.9.9, not v1.2.3"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

# ---------------------------------------------------------------------------
setup "--upgrade keeps every preserved file byte-identical"
seed_install
cp "$DESTDIR/usr/local/bin/atlas-agent" "$SANDBOX/old-binary"
publish v1.2.3 amd64
before="$(preserved)"
export ATLAS_INSTALL_TEST=1
run_install --upgrade --version v1.2.3
expect_rc_zero
[ "$(preserved)" = "$before" ] || fail "a preserved file changed: $(diff <(echo "$before") <(preserved) || true)"
same_file "$FIXTURES/v1.2.3/atlas-agent-linux-amd64" "$DESTDIR/usr/local/bin/atlas-agent" "new binary not installed"
same_file "$SANDBOX/old-binary" "$DESTDIR/usr/local/bin/atlas-agent.prev" "previous binary not kept as .prev"
grep -qx 'restart atlas-agent' "$STUB_STATE/systemctl.log" || fail "service not restarted"
! grep -q -e daemon-reload -e enable "$STUB_STATE/systemctl.log" || fail "upgrade touched the unit (daemon-reload/enable)"
for leftover in "$DESTDIR"/usr/local/bin/.atlas-agent*; do
  [ -e "$leftover" ] && fail "temp file left behind: $leftover"
done
expect_output "Upgraded to v1.2.3"
teardown

setup "--upgrade rolls back when the service does not stay active"
seed_install
cp "$DESTDIR/usr/local/bin/atlas-agent" "$SANDBOX/old-binary"
publish v1.2.3 amd64 CRASHES
before="$(preserved)"
export ATLAS_INSTALL_TEST=1
run_install --upgrade --version v1.2.3
expect_rc_nonzero
expect_output "is-active was 'activating'"
same_file "$SANDBOX/old-binary" "$DESTDIR/usr/local/bin/atlas-agent" "previous binary not restored"
[ "$(grep -cx 'restart atlas-agent' "$STUB_STATE/systemctl.log")" -eq 2 ] || fail "want a restart for the upgrade and one for the rollback"
[ "$(preserved)" = "$before" ] || fail "a preserved file changed during the rollback"
teardown

setup "--upgrade rolls back when NRestarts increases"
seed_install
cp "$DESTDIR/usr/local/bin/atlas-agent" "$SANDBOX/old-binary"
publish v1.2.3 amd64 FLAPS
export ATLAS_INSTALL_TEST=1
run_install --upgrade --version v1.2.3
expect_rc_nonzero
expect_output "restarted on its own"
same_file "$SANDBOX/old-binary" "$DESTDIR/usr/local/bin/atlas-agent" "previous binary not restored"
teardown

setup "--upgrade with --binary"
seed_install
fake_binary "$WORK/atlas-agent" v1.3.0
export ATLAS_INSTALL_TEST=1
run_install --upgrade --binary "$WORK/atlas-agent"
expect_rc_zero
same_file "$WORK/atlas-agent" "$DESTDIR/usr/local/bin/atlas-agent" "--binary not installed"
teardown

setup "--upgrade refuses without an env file"
fake_binary "$DESTDIR/usr/local/bin/atlas-agent" v1.0.0
publish v1.2.3 amd64
before="$(snapshot "$DESTDIR")"
export ATLAS_INSTALL_TEST=1
run_install --upgrade --version v1.2.3
expect_rc_nonzero
expect_output "no configured atlas-agent here to upgrade"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

setup "--upgrade refuses connection flags"
seed_install
publish v1.2.3 amd64
before="$(snapshot "$DESTDIR")"
export ATLAS_INSTALL_TEST=1
run_install --upgrade --version v1.2.3 --control-plane-url https://other.example.com
expect_rc_nonzero
expect_output "takes no connection flags (got: --control-plane-url)"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

setup "--upgrade under DESTDIR refuses without ATLAS_INSTALL_TEST=1"
seed_install
publish v1.2.3 amd64
before="$(snapshot "$DESTDIR")"
run_install --upgrade --version v1.2.3
expect_rc_nonzero
expect_output "only by the test suite"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

setup "ATLAS_INSTALL_TEST=1 does nothing for a real (non-DESTDIR) install"
publish v1.2.3 amd64
export ATLAS_INSTALL_TEST=1
DESTDIR="" run_install --upgrade --version v1.2.3
expect_rc_nonzero
expect_output "must run as root"
teardown

# ---------------------------------------------------------------------------
setup "re-run with an existing env file and new flags fails loudly"
seed_install
fake_binary "$WORK/atlas-agent" v1.3.0
before="$(snapshot "$DESTDIR")"
run_install --transport libp2p --relationship staging --control-plane-url https://cp.example.com \
  --server-addr /ip4/203.0.113.7/tcp/4102/p2p/12D3KooWAG8Ks6fnAuXcq5CzjmFHkrhJc3kRxy1YGh8BTUtAFaf
expect_rc_nonzero
expect_output "would be ignored: --transport --relationship --control-plane-url --server-addr"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

setup "re-run with an existing env file and no flags points at --upgrade"
seed_install
fake_binary "$WORK/atlas-agent" v1.3.0
before="$(snapshot "$DESTDIR")"
run_install
expect_rc_nonzero
expect_output "Use --upgrade"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

# ---------------------------------------------------------------------------
peer="12D3KooWAG8Ks6fnAuXcq5CzjmFHkrhJc3kRxy1YGh8BTUtAFaf"
relay="/ip4/203.0.113.7/tcp/4103/p2p/$peer"
injections=(
  "url newline|--control-plane-url|https://cp.example.com"$'\n'"ATLAS_AGENT_TOKEN=x"
  "url carriage return|--control-plane-url|https://cp.example.com"$'\r'
  "url quote|--control-plane-url|https://cp.example.com\"x"
  "url space|--control-plane-url|https://cp.example.com x"
  "url hash|--control-plane-url|https://cp.example.com#x"
  "url backslash|--control-plane-url|https://cp.example.com\\x"
  "url not https|--control-plane-url|http://cp.example.com"
  "environment newline|--environment|prod"$'\n'"ATLAS_AGENT_LOG_LEVEL=debug"
  "environment quote|--environment|prod'x"
  "environment space|--environment|prod x"
  "relationship space|--relationship|prod x"
  "relationship quote|--relationship|prod\"x"
  "server-peer-id newline|--server-peer-id|$peer"$'\n'"X=1"
  "server-peer-id space|--server-peer-id|$peer x"
  "relay-addr space|--relay-addr|$relay x"
  "relay-addr quote|--relay-addr|$relay\""
  "relay-addr hash|--relay-addr|$relay#x"
  "token newline|--token|atlas_enroll_ab"$'\n'"X=1"
  "token space|--token|atlas_enroll ab"
  "ca-bundle space|--ca-bundle|/etc/atlas ca.pem"
)
for entry in "${injections[@]}"; do
  name="${entry%%|*}"; rest="${entry#*|}"; flag="${rest%%|*}"; value="${rest#*|}"
  setup "rejects injection: $name"
  fake_binary "$WORK/atlas-agent" v1.3.0
  case "$flag" in
    --relationship | --server-peer-id | --relay-addr)
      args=(--transport libp2p --control-plane-url https://cp.example.com --relay-addr "$relay" --server-peer-id "$peer" --relationship prod)
      ;;
    *) args=("${https_flags[@]}") ;;
  esac
  args+=("$flag" "$value")
  run_install "${args[@]}"
  expect_rc_nonzero
  grep -qE -- "^install.sh: $flag (contains characters that are not allowed|must not contain a newline)" "$OUT" \
    || fail "not rejected by the $flag validator: $(cat "$OUT")"
  [ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written for $flag"
  teardown
done

# ---------------------------------------------------------------------------
# install-code mode
# ---------------------------------------------------------------------------
install_code_artifacts_gone() {
  [ ! -e "$DESTDIR/usr/local/bin/atlas-agent" ] || fail "binary not rolled back"
  [ ! -e "$DESTDIR/etc/atlas-agent/atlas-agent.env" ] || fail "env file not rolled back"
  [ ! -e "$DESTDIR/etc/systemd/system/atlas-agent.service" ] || fail "unit not rolled back"
  [ ! -d "$DESTDIR/var/lib/atlas-agent" ] || fail "agent data dir not rolled back"
}

setup "install-code: rejects a malformed code"
run_install --install-code 'not-a-code' --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "install.sh: --install-code contains characters that are not allowed"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "install-code: --server-url must be https"
run_install --install-code "$TEST_INSTALL_CODE" --server-url 'http://cp.example.com' --version v1.2.3
expect_rc_nonzero
expect_output "install.sh: --server-url contains characters that are not allowed"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "install-code: --install-code and --server-url are required together (code only)"
run_install --install-code "$TEST_INSTALL_CODE" --version v1.2.3
expect_rc_nonzero
expect_output "must be given together"
teardown

setup "install-code: --install-code and --server-url are required together (url only)"
run_install --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "must be given together"
teardown

setup "install-code: happy path redeems against a stub server"
publish v1.2.3 amd64
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_zero
grep -qx "ATLAS_AGENT_CONTROL_PLANE_URL=$TEST_SERVER_URL" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the control-plane URL"
grep -qx "ATLAS_AGENT_ENVIRONMENT=production" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the environment"
grep -qx "ATLAS_AGENT_NODE_ID=$TEST_NODE_ID" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the node id"
grep -qx "ATLAS_AGENT_TRANSPORT=libp2p" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the transport"
grep -qx "ATLAS_AGENT_LIBP2P_SERVER_ADDR=$TEST_SERVER_ADDR" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the server address"
[ -f "$DESTDIR/etc/systemd/system/atlas-agent.service" ] || fail "unit not installed"
! grep -rF "$TEST_INSTALL_CODE" "$SANDBOX" >/dev/null 2>&1 || fail "the install code appears somewhere under $SANDBOX"
teardown

setup "install-code: a 401 from redeem gives a clear message and changes nothing"
publish v1.2.3 amd64
redeem_status 401
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "the install code was rejected: invalid, expired, or already used"
install_code_artifacts_gone
! grep -rF "$TEST_INSTALL_CODE" "$SANDBOX" >/dev/null 2>&1 || fail "the install code appears somewhere under $SANDBOX"
teardown

setup "install-code: a malformed redeem response burns the code and rolls back"
publish v1.2.3 amd64
printf '{"node_id":"not-hex","environment":"production","server_addr":"%s"}\n' "$TEST_SERVER_ADDR" >"$STUB_STATE/redeem_response"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "the install code was already redeemed on the server; this host has been rolled back"
install_code_artifacts_gone
teardown

setup "install-code: service unhealthy after redeem burns the code and rolls back"
publish v1.2.3 amd64 CRASHES
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
export ATLAS_INSTALL_TEST=1
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "is-active was 'activating'"
expect_output "the install code was already redeemed on the server; this host has been rolled back"
install_code_artifacts_gone
teardown

setup "install-code: --install-code - reads the code from stdin"
publish v1.2.3 amd64
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
run_install_stdin "$TEST_INSTALL_CODE" --install-code - --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_zero
grep -qx "ATLAS_AGENT_NODE_ID=$TEST_NODE_ID" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the node id"
teardown

setup "install-code: ATLAS_INSTALL_CODE environment variable works without the flag"
publish v1.2.3 amd64
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
export ATLAS_INSTALL_CODE="$TEST_INSTALL_CODE"
run_install --server-url "$TEST_SERVER_URL" --version v1.2.3
unset ATLAS_INSTALL_CODE
expect_rc_zero
grep -qx "ATLAS_AGENT_NODE_ID=$TEST_NODE_ID" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the node id"
teardown

setup "install-code: refuses an already configured host, to prevent cloning an identity"
seed_install
before="$(snapshot "$DESTDIR")"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "already exists. Use --upgrade"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

setup "install-code: --force-env is refused together with --install-code"
seed_install
before="$(snapshot "$DESTDIR")"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3 --force-env
expect_rc_nonzero
expect_output "--force-env is not used with --install-code"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

setup "install-code: --upgrade is refused together with --install-code"
seed_install
before="$(snapshot "$DESTDIR")"
run_install --upgrade --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "--install-code is not used with --upgrade"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "something changed"
teardown

setup "install-code: connection flags are refused alongside --install-code"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3 --environment staging
expect_rc_nonzero
expect_output "does not take connection flags"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

echo
echo "==> $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
