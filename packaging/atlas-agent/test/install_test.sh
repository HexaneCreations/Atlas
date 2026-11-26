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
# prints a fixed, format-valid Peer ID.
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
  # preflight's systemd check: ATLAS_INSTALL_SYSTEMD_MARKER substitutes for
  # /run/systemd/system under test, the same test-only-seam precedent
  # ATLAS_INSTALL_TEST already sets — kept outside $DESTDIR entirely so it
  # never shows up in a snapshot("$DESTDIR") comparison. Present by default
  # (systemd "running"); a test can rm -rf it to simulate the opposite.
  export ATLAS_INSTALL_SYSTEMD_MARKER="$STUB_STATE/systemd-marker"
  mkdir -p "$ATLAS_INSTALL_SYSTEMD_MARKER"

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
  https://github.com)
    # preflight's GitHub reachability probe. Default: reachable; a test can
    # touch $STUB_STATE/github_unreachable to simulate the opposite.
    [ -f "$STUB_STATE/github_unreachable" ] && exit 7
    exit 0
    ;;
  */healthz)
    # preflight's --server-url reachability probe.
    [ -f "$STUB_STATE/server_unreachable" ] && exit 7
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
  # Also dumps its own environment on every call, overwriting the previous
  # capture — uname is the first external command install.sh runs (in
  # run_preflight's arch check), right after the ATLAS_INSTALL_CODE unset,
  # so this is what a test checks to prove no child process inherits it.
  cat >"$STUBS/uname" <<'EOF'
#!/bin/sh
env >"$STUB_STATE/child-env.log" 2>/dev/null || true
case "$1" in -s) echo Linux ;; -m) echo "$STUB_ARCH" ;; *) echo Linux ;; esac
EOF
  # preflight's disk-space check. Default: plenty free; a test can write a
  # small number (KB) to $STUB_STATE/df_kb to simulate a full disk.
  cat >"$STUBS/df" <<'EOF'
#!/bin/sh
kb=10000000
[ -f "$STUB_STATE/df_kb" ] && kb="$(cat "$STUB_STATE/df_kb")"
echo "Filesystem 1K-blocks Used Available Use% Mounted"
echo "stub 100000000 0 $kb 0% /"
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
# single-file installer: no dependence on a sibling atlas-agent.service —
# the UI flow downloads install.sh alone.
# ---------------------------------------------------------------------------

setup "the embedded systemd unit is byte-identical to atlas-agent.service"
sed -n "/<<.ATLAS_AGENT_SERVICE_EOF.\$/,/^ATLAS_AGENT_SERVICE_EOF\$/p" "$WORK/install.sh" | sed '1d;$d' >"$SANDBOX/extracted.service"
cmp -s "$SANDBOX/extracted.service" "$HERE/../atlas-agent.service" \
  || fail "embedded unit differs from atlas-agent.service: $(diff "$SANDBOX/extracted.service" "$HERE/../atlas-agent.service" || true)"
teardown

setup "install.sh works copied alone, with no sibling atlas-agent.service"
ALONE_DIR="$SANDBOX/alone"
mkdir -p "$ALONE_DIR"
cp "$INSTALL_SH" "$ALONE_DIR/install.sh"
chmod 0755 "$ALONE_DIR/install.sh"
fake_binary "$SANDBOX/standalone-binary/atlas-agent" v1.3.0
OUT="$SANDBOX/out.txt"
set +e
bash "$ALONE_DIR/install.sh" "${https_flags[@]}" --binary "$SANDBOX/standalone-binary/atlas-agent" >"$OUT" 2>&1
RC=$?
set -e
expect_rc_zero
[ -f "$DESTDIR/etc/systemd/system/atlas-agent.service" ] || fail "unit not installed from a standalone install.sh"
teardown

# ---------------------------------------------------------------------------
# install-code mode
# ---------------------------------------------------------------------------
install_code_artifacts_gone() {
  [ ! -e "$DESTDIR/usr/local/bin/atlas-agent" ] || fail "binary not rolled back"
  [ ! -e "$DESTDIR/etc/atlas-agent/atlas-agent.env" ] || fail "env file not rolled back"
  [ ! -e "$DESTDIR/etc/systemd/system/atlas-agent.service" ] || fail "unit not rolled back"
  [ ! -d "$DESTDIR/var/lib/atlas-agent" ] || fail "agent data dir not rolled back"
  # Only meaningful for a failure that reached env-file-writing (this run
  # created /etc/atlas-agent itself); harmlessly already-true otherwise.
  [ ! -d "$DESTDIR/etc/atlas-agent" ] || fail "config directory this run created was not rolled back"
}

setup "install-code: rejects a malformed code"
run_install --install-code 'not-a-code' --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "install.sh: --install-code has the wrong format (expected atlas_install_ followed by 64 lowercase hex characters)"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "install-code: a rejected code never appears in the output"
BADCODE="atlas_install_BADMARKERXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX"
run_install --install-code "$BADCODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "has the wrong format"
! grep -qF "$BADCODE" "$OUT" || fail "the rejected code appears verbatim in the output"
! grep -qF "BADMARKER" "$OUT" || fail "the rejected code's content appears in the output"
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
expect_output "the install code has already been consumed on the server; the pre-assigned node is burned"
install_code_artifacts_gone
teardown

setup "install-code: service unhealthy after redeem burns the code and rolls back"
publish v1.2.3 amd64 CRASHES
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
export ATLAS_INSTALL_TEST=1
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "is-active was 'activating'"
expect_output "the install code has already been consumed on the server; the pre-assigned node is burned"
install_code_artifacts_gone
grep -qx 'stop atlas-agent' "$STUB_STATE/systemctl.log" || fail "service not stopped during rollback"
grep -qx 'disable atlas-agent' "$STUB_STATE/systemctl.log" || fail "service not disabled during rollback"
teardown

setup "install-code: never removes a pre-existing /etc/atlas-agent on rollback"
publish v1.2.3 amd64 CRASHES
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
export ATLAS_INSTALL_TEST=1
install -d -m 0750 "$DESTDIR/etc/atlas-agent"   # pre-existing, otherwise empty
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
[ -d "$DESTDIR/etc/atlas-agent" ] || fail "a pre-existing config directory was removed on rollback"
teardown

setup "install-code: an unexpected failure after redeem still burns the code (no explicit die call site)"
publish v1.2.3 amd64
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
mkdir -p "$DESTDIR/etc"
touch "$DESTDIR/etc/atlas-agent"  # a plain file where install -d expects a directory; fails regardless of privilege
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "the install code has already been consumed on the server; the pre-assigned node is burned"
rm -f "$DESTDIR/etc/atlas-agent"
teardown

setup "install-code: a child process does not inherit ATLAS_INSTALL_CODE"
publish v1.2.3 amd64
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
export ATLAS_INSTALL_CODE="$TEST_INSTALL_CODE"
run_install --server-url "$TEST_SERVER_URL" --version v1.2.3
unset ATLAS_INSTALL_CODE
expect_rc_zero
[ -f "$STUB_STATE/child-env.log" ] || fail "no child process environment was captured"
! grep -q '^ATLAS_INSTALL_CODE=' "$STUB_STATE/child-env.log" || fail "a child process inherited ATLAS_INSTALL_CODE"
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

setup "install-code: refuses when the data directory already exists with an identity key, and leaves it intact"
install -d -m 0700 "$DESTDIR/var/lib/atlas-agent"
head -c 64 /dev/urandom >"$DESTDIR/var/lib/atlas-agent/p2p-identity.key"
before="$(snapshot "$DESTDIR")"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "already exists on this host with no configured"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "the pre-existing data directory changed"
teardown

setup "install-code: refuses when the binary already exists, and leaves it intact"
fake_binary "$DESTDIR/usr/local/bin/atlas-agent" v1.0.0
before="$(snapshot "$DESTDIR")"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "already exists on this host with no configured"
[ "$(snapshot "$DESTDIR")" = "$before" ] || fail "the pre-existing binary changed"
teardown

# ---------------------------------------------------------------------------
# install-code mode: leftover-state regression tests
#
# A real host hit this: install.sh created /var/lib/atlas-agent as
# root:atlas 0700 (copied from $CONF_DIR's ownership, which is deliberately
# root-owned/group-readable), then tried to generate the identity as the
# atlas USER — whose membership in the atlas GROUP is irrelevant, since mode
# 0700 grants the group nothing at all. `atlas-agent peer-id` got EACCES,
# the whole install died, and the empty drop-in directory it also created
# was left behind (only the file inside it was ever removed). None of this
# showed up above because STAGED mode skips chown entirely (OWN_ATLAS_DATA
# is `()`) and never reaches the docker-group block (`[ "$STAGED" -eq 0 ]`),
# so the staged suite never actually exercised either ownership bug.
# ---------------------------------------------------------------------------

setup "install-code: cleanup removes the empty drop-in directory, not just the file"
publish v1.2.3 amd64
# Simulates a drop-in left by an earlier attempt (real installs only reach
# this branch when a 'docker' group exists; staged tests never do — see the
# section comment above) — the trap must still clean it up regardless of
# who created it.
install -d -m 0755 "$DESTDIR/etc/systemd/system/atlas-agent.service.d"
printf '[Service]\nSupplementaryGroups=docker\n' >"$DESTDIR/etc/systemd/system/atlas-agent.service.d/10-docker-group.conf"
printf '{"node_id":"not-hex","environment":"production","server_addr":"%s"}\n' "$TEST_SERVER_ADDR" >"$STUB_STATE/redeem_response"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
[ ! -e "$DESTDIR/etc/systemd/system/atlas-agent.service.d/10-docker-group.conf" ] || fail "drop-in file not removed"
[ ! -d "$DESTDIR/etc/systemd/system/atlas-agent.service.d" ] || fail "now-empty drop-in directory left behind"
teardown

setup "install-code: a retry succeeds on a host left in exactly that state"
publish v1.2.3 amd64
redeem_ok "$TEST_NODE_ID" production "$TEST_SERVER_ADDR"
# The leftover from a previous failed attempt: an empty drop-in directory
# and nothing else (no binary, no env file, no unit — see the bug report).
install -d -m 0755 "$DESTDIR/etc/systemd/system/atlas-agent.service.d"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_zero
grep -qx "ATLAS_AGENT_NODE_ID=$TEST_NODE_ID" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the node id"
grep -qx "ATLAS_AGENT_TRANSPORT=libp2p" "$DESTDIR/etc/atlas-agent/atlas-agent.env" || fail "env file lacks the transport"
[ -f "$DESTDIR/etc/systemd/system/atlas-agent.service" ] || fail "unit not installed"
teardown

# ---------------------------------------------------------------------------
# preflight: hard checks, each in isolation, before any host mutation.
# See run_preflight in install.sh.
# ---------------------------------------------------------------------------

peer="12D3KooWAG8Ks6fnAuXcq5CzjmFHkrhJc3kRxy1YGh8BTUtAFaf"

setup "preflight: refuses an unsupported architecture even with --binary (not just --version)"
fake_binary "$WORK/atlas-agent" v1.3.0
export STUB_ARCH=riscv64
run_install "${https_flags[@]}"
expect_rc_nonzero
expect_output "preflight: unsupported architecture 'riscv64'"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "preflight: refuses when systemd is not running"
fake_binary "$WORK/atlas-agent" v1.3.0
rm -rf "$ATLAS_INSTALL_SYSTEMD_MARKER"
run_install "${https_flags[@]}"
expect_rc_nonzero
expect_output "preflight: systemd does not appear to be running"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "preflight: refuses when free disk space is too low"
fake_binary "$WORK/atlas-agent" v1.3.0
echo 1024 >"$STUB_STATE/df_kb"
run_install "${https_flags[@]}"
expect_rc_nonzero
expect_output "preflight: less than 100MiB free"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "preflight: refuses when github.com is unreachable (--version)"
publish v1.2.3 amd64
: >"$STUB_STATE/github_unreachable"
run_install "${https_flags[@]}" --version v1.2.3
expect_rc_nonzero
expect_output "preflight: cannot reach github.com"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "preflight: refuses when --server-url is unreachable"
publish v1.2.3 amd64
: >"$STUB_STATE/server_unreachable"
run_install --install-code "$TEST_INSTALL_CODE" --server-url "$TEST_SERVER_URL" --version v1.2.3
expect_rc_nonzero
expect_output "preflight: cannot reach $TEST_SERVER_URL"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

setup "preflight: refuses when the plain libp2p --server-addr is unreachable (known up front, nothing written)"
fake_binary "$WORK/atlas-agent" v1.3.0
# A real, deterministic closed port — 127.0.0.1 refuses instantly (no
# network dependency, no reliance on a timeout firing) — not a stub: the
# tcp_reachable check this exercises no longer goes through curl at all
# (see install.sh), so there is nothing left in that path to stub.
run_install --transport libp2p --control-plane-url https://cp.example.com --server-addr "/ip4/127.0.0.1/tcp/1/p2p/$peer"
expect_rc_nonzero
expect_output "preflight: cannot open a TCP connection to 127.0.0.1:1"
[ "$(snapshot "$DESTDIR")" = "<absent>" ] || fail "something was written"
teardown

# ---------------------------------------------------------------------------
# install-code mode: the real permission model
#
# Everything above runs staged and unprivileged, which is exactly why the
# root:atlas-0700 bug reached a real host undetected: OWN_ATLAS_DATA is `()`
# under STAGED, so no chown ever happens, and agent_peer_id's STAGED branch
# runs the binary directly as the test's own (non-root) user instead of
# dropping privilege to 'atlas' at all. This section exercises the actual
# syscalls against a real throwaway system user instead of a stand-in —
# needs real root, so it is skipped (not failed) everywhere else, including
# normal CI.
# ---------------------------------------------------------------------------
if [ "$(id -u)" -eq 0 ] && command -v useradd >/dev/null 2>&1 && command -v runuser >/dev/null 2>&1; then
  CURRENT="real permissions: atlas:atlas 0700 lets the atlas user create its own identity key"
  SANDBOX="$(mktemp -d)"
  # mktemp -d defaults to 0700 root-owned: left alone, the throwaway user
  # could not even traverse into $SANDBOX to reach $REALDIR, which would
  # fail every case below for the wrong reason (blocked by the parent, not
  # by $REALDIR's own mode — the thing actually under test). A world-
  # traversable sandbox with the real restriction only on $REALDIR/$BUGDIR
  # themselves is what actually isolates the claim being tested.
  chmod 0755 "$SANDBOX"
  TESTUSER="atlas-perm-test-$$"
  useradd --system --no-create-home --shell /usr/sbin/nologin "$TESTUSER"
  REALDIR="$SANDBOX/data-dir"
  install -d -o "$TESTUSER" -g "$TESTUSER" -m 0700 "$REALDIR"
  if runuser -u "$TESTUSER" -- sh -c "echo identity > '$REALDIR/p2p-identity.key'" 2>"$SANDBOX/err" \
      && [ -f "$REALDIR/p2p-identity.key" ] \
      && runuser -u "$TESTUSER" -- cat "$REALDIR/p2p-identity.key" >/dev/null 2>>"$SANDBOX/err"; then
    echo "  ok    $CURRENT"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $CURRENT: $(cat "$SANDBOX/err")"; FAIL=$((FAIL + 1))
  fi

  CURRENT="real permissions: root:atlas 0700 reproduces the exact EACCES this bug hit (regression guard)"
  BUGDIR="$SANDBOX/bug-dir"
  install -d -o root -g "$TESTUSER" -m 0700 "$BUGDIR"
  if runuser -u "$TESTUSER" -- sh -c "echo identity > '$BUGDIR/p2p-identity.key'" 2>"$SANDBOX/err2"; then
    echo "  FAIL  $CURRENT: the write unexpectedly succeeded — the bug's permission model no longer reproduces, re-check the fix"
    FAIL=$((FAIL + 1))
  elif grep -qi "permission denied" "$SANDBOX/err2"; then
    echo "  ok    $CURRENT"; PASS=$((PASS + 1))
  else
    echo "  FAIL  $CURRENT: failed for an unexpected reason: $(cat "$SANDBOX/err2")"; FAIL=$((FAIL + 1))
  fi

  userdel "$TESTUSER" 2>/dev/null || true
  rm -rf "$SANDBOX"
else
  echo "  skip  real permissions: needs root, useradd and runuser (run as root, e.g. on the target host or in CI, to exercise this)"
fi

echo
echo "==> $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
