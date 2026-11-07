#!/bin/bash
#
# Simulated-install test for the cloudflared fnOS lifecycle scripts.
#
# The .fpk cannot be installed without root, so this reproduces the layout the
# app centre creates under /var/apps/<appname>/ inside a scratch directory and
# drives every cmd/ script with the TRIM_* variables the app centre would set.
# It exercises the whole install -> start -> status -> config -> uninstall path
# for real, rather than only syntax-checking the scripts.
#
# The service is a static Go binary (target/bin/fncfd). The suite compiles it
# from ./cmd/fncfd so it always tests the current source, and it replaces the
# 38MB upstream cloudflared with a stub -- the lifecycle scripts only need it to
# exist, be executable, answer --version, and stay alive so the token-delivery
# assertions can inspect its argv and environment.
#
# This suite drives /proc, ps and Unix sockets, so it only runs on Linux (the
# NAS). Everywhere else it prints SKIP and exits 0.
#
# Run it under setsid so that any process-group signal stays inside the test:
#
#   setsid ./tools/test-lifecycle.sh
#
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$ROOT/cloudflared"
# The service binds a Unix socket inside the payload directory, and AF_UNIX
# caps sun_path at 108 bytes. A CI checkout lives under
# /home/runner/work/<repo>/<repo>/, which made the socket path 113 bytes: bind
# failed, app.sock never appeared, and every API assertion after "main start"
# failed with no hint about why. Keep the scratch tree short; override with
# FNCFD_LIFECYCLE_SCRATCH when you need a specific location.
SCRATCH="${FNCFD_LIFECYCLE_SCRATCH:-$(mktemp -d "${TMPDIR:-/tmp}/fncfd-lc.XXXXXX")}"
APPS="$SCRATCH/apps/cloudflared"

SOCKET_PATH="$APPS/target/app.sock"
if [ "${#SOCKET_PATH}" -gt 100 ]; then
    printf '%s\n' "ERROR: the payload path is ${#SOCKET_PATH} bytes:" >&2
    printf '%s\n' "  $SOCKET_PATH" >&2
    printf '%s\n' "AF_UNIX allows 108 bytes for sun_path, so the service cannot bind there." >&2
    printf '%s\n' "Point FNCFD_LIFECYCLE_SCRATCH at a short directory and re-run." >&2
    exit 1
fi

PASS=0
FAIL=0

ok() {
    printf '  \033[32mPASS\033[0m %s\n' "$*"
    PASS=$((PASS + 1))
}
bad() {
    printf '  \033[31mFAIL\033[0m %s\n' "$*"
    FAIL=$((FAIL + 1))
}
check() { # check <description> <expected> <actual>
    if [ "$2" = "$3" ]; then ok "$1 ($3)"; else bad "$1 (expected [$2], got [$3])"; fi
}
section() { printf '\n\033[1;36m==>\033[0m %s\n' "$*"; }

if [ "$(uname -s)" != "Linux" ]; then
    printf '\n\033[33mSKIP\033[0m this suite needs Linux (/proc, ps, unix sockets); it runs on the NAS.\n'
    exit 0
fi

# ------------------------------------------------------------------ environment
# Mirror what the app centre exports before invoking cmd/*.
export TRIM_APPNAME=cloudflared
export TRIM_APPDEST="$APPS/target"
export TRIM_PKGVAR="$SCRATCH/var"
export TRIM_PKGTMP="$SCRATCH/tmp"
export TRIM_PKGETC="$SCRATCH/etc"
export TRIM_PKGHOME="$SCRATCH/home"
export TRIM_APPDEST_VOL="$SCRATCH"
export TRIM_TEMP_LOGFILE="$SCRATCH/install.log"
export TRIM_SERVICE_PORT=0
export TRIM_APPVER=1.4.2

setup_install_root() {
    rm -rf "$SCRATCH"
    mkdir -p "$APPS" "$TRIM_PKGVAR" "$TRIM_PKGTMP" "$TRIM_PKGETC" "$TRIM_PKGHOME"
    mkdir -p "$APPS/target/bin"
    cp -a "$SRC/app/." "$APPS/target/"
    cp -a "$SRC/cmd" "$APPS/cmd"
    cp -a "$SRC/config" "$APPS/config"
    cp -a "$SRC/wizard" "$APPS/wizard"
    cp "$SRC/manifest" "$APPS/manifest"
    : >"$TRIM_TEMP_LOGFILE"
}

# The stub stands in for the upstream binary. It must answer --version (the
# install callback checks it) and stay alive for a tunnel start, so the token
# delivery assertions have a process to inspect.
make_cloudflared_stub() {
    cat >"$APPS/target/bin/cloudflared" <<'STUB'
#!/bin/bash
# Test stub for the upstream cloudflared binary.
for arg in "$@"; do
    case "$arg" in
    --version | version)
        echo "cloudflared version 2026.9.3 (stub)"
        exit 0
        ;;
    list)
        echo "[]"
        exit 0
        ;;
    esac
done
trap 'kill "$child" 2>/dev/null; exit 0' TERM INT
sleep 60 &
child=$!
wait "$child"
STUB
    chmod 755 "$APPS/target/bin/cloudflared"
}

# The service is the Go binary. Compile it from source so the suite always
# tests the working tree; fall back to the binary the build pipeline produced
# when this host has no Go toolchain (the NAS installs a static binary, so it
# has no reason to).
build_server_binary() {
    local out="$APPS/target/bin/fncfd"
    if command -v go >/dev/null 2>&1; then
        if ! (cd "$ROOT" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$out" ./cmd/fncfd); then
            echo "FATAL: go build ./cmd/fncfd failed" >&2
            exit 1
        fi
    elif [ -x "$SRC/app/bin/fncfd" ]; then
        cp "$SRC/app/bin/fncfd" "$out"
    else
        echo "FATAL: neither a Go toolchain nor a built app/bin/fncfd is available" >&2
        exit 1
    fi
    chmod 755 "$out"
}

CMD="$APPS/cmd"
run() {
    local script="$1"
    shift
    bash "$CMD/$script" "$@"
}

cleanup() {
    bash "$CMD/main" stop >/dev/null 2>&1
    rm -rf "$SCRATCH"
}
trap cleanup EXIT

# ------------------------------------------------------------------- the tests
setup_install_root
make_cloudflared_stub
build_server_binary

section "payload and lifecycle wiring"
if "$APPS/target/bin/fncfd" --version >/dev/null 2>&1; then
    ok "the service binary runs ($("$APPS/target/bin/fncfd" --version 2>/dev/null | head -n 1))"
else
    bad "the service binary does not run"
fi
if grep -q 'bin/fncfd' "$CMD/main"; then
    ok "cmd/main launches the service binary directly"
else
    bad "cmd/main does not reference bin/fncfd"
fi
if grep -Eq 'find_node|TRIM_NODE_BIN|nodejs_v' "$CMD"/*; then
    bad "a lifecycle script still probes an interpreter runtime"
else
    ok "no lifecycle script probes a runtime (the payload is self-contained)"
fi

section "install_init"
run install_init
check "install_init exit code" 0 $?

section "install_callback with wizard_auto_start=true"
export wizard_auto_start=true
run install_callback
check "install_callback exit code" 0 $?
[ -f "$TRIM_PKGVAR/VERSION" ] && ok "VERSION written" || bad "VERSION missing"
[ -d "$TRIM_PKGVAR/.cloudflared" ] && ok "credential dir created" || bad "credential dir missing"
check "credential dir mode" 700 "$(stat -c %a "$TRIM_PKGVAR/.cloudflared" 2>/dev/null)"
[ -x "$APPS/target/bin/cloudflared" ] && ok "cloudflared is executable" || bad "cloudflared not executable"
[ -f "$TRIM_PKGVAR/config.json" ] && ok "config.json created via config_callback" || bad "config.json missing"

section "main status before start"
run main status
check "status reports not-running" 3 $?

section "main start"
run main start
check "start exit code" 0 $?
[ -S "$TRIM_APPDEST/app.sock" ] && ok "app.sock created" || bad "app.sock missing"
check "app.sock mode" 660 "$(stat -c %a "$TRIM_APPDEST/app.sock" 2>/dev/null)"
check "pid file mode" 644 "$(stat -c %a "$TRIM_PKGVAR/server.pid" 2>/dev/null)"

section "main status after start"
run main status
check "status reports running" 0 $?

section "main start is idempotent"
BEFORE="$(cat "$TRIM_PKGVAR/server.pid")"
run main start
check "second start exit code" 0 $?
check "same process kept" "$BEFORE" "$(cat "$TRIM_PKGVAR/server.pid")"

section "gateway API over the unix socket"
META="$(curl -s --max-time 5 --unix-socket "$TRIM_APPDEST/app.sock" http://localhost/api/meta)"
if printf '%s' "$META" | grep -q '"app":"cloudflared"'; then
    ok "GET /api/meta answered"
    printf '       %s\n' "$META"
else
    bad "GET /api/meta returned: $META"
fi

section "gateway prefix handling"
PREFIXED="$(curl -s --max-time 5 --unix-socket "$TRIM_APPDEST/app.sock" http://localhost/app/cloudflared/ | grep -o '__CF_BASE__ = "[^"]*"')"
check "prefixed request injects prefix" '__CF_BASE__ = "/app/cloudflared"' "$PREFIXED"
ROOTED="$(curl -s --max-time 5 --unix-socket "$TRIM_APPDEST/app.sock" http://localhost/ | grep -o '__CF_BASE__ = "[^"]*"')"
check "unprefixed request injects empty" '__CF_BASE__ = ""' "$ROOTED"

section "main restart"
run main restart
check "restart exit code" 0 $?
run main status
check "status running after restart" 0 $?

section "config_callback merges without losing tunnels or tokens"
# Write a config.json that already has a tunnel with a token, then let the
# wizard answer change the settings. The tunnel and its token must survive: the
# settings page and the web UI are two writers of the same file.
cat >"$TRIM_PKGVAR/config.json" <<'JSON'
{
  "version": 1,
  "settings": {
    "protocol": "auto",
    "logLevel": "info",
    "metricsPort": 0,
    "autoStart": true,
    "autoRestart": true
  },
  "tunnels": [
    {
      "id": "keepme",
      "name": "Keep Me",
      "type": "token",
      "enabled": true,
      "autoStart": true,
      "autoDns": true,
      "token": "secret-token"
    }
  ]
}
JSON
chmod 600 "$TRIM_PKGVAR/config.json"
export wizard_log_level=debug wizard_protocol=http2 wizard_auto_start=false wizard_auto_restart=false
run config_callback
check "config_callback exit code" 0 $?
grep -q '"logLevel": "debug"' "$TRIM_PKGVAR/config.json" && ok "settings merged: logLevel" || bad "logLevel not merged"
grep -q '"protocol": "http2"' "$TRIM_PKGVAR/config.json" && ok "settings merged: protocol" || bad "protocol not merged"
grep -q '"autoStart": false' "$TRIM_PKGVAR/config.json" && ok "settings merged: autoStart" || bad "autoStart not merged"
grep -q '"autoRestart": false' "$TRIM_PKGVAR/config.json" && ok "settings merged: autoRestart" || bad "autoRestart not merged"
grep -q '"id": "keepme"' "$TRIM_PKGVAR/config.json" && ok "tunnel definition preserved" || bad "tunnel definition lost"
grep -q '"token": "secret-token"' "$TRIM_PKGVAR/config.json" && ok "tunnel token preserved" || bad "tunnel token lost"
check "config.json mode" 600 "$(stat -c %a "$TRIM_PKGVAR/config.json" 2>/dev/null)"

section "config_callback rejects out-of-enum values"
export wizard_log_level="../../etc/passwd" wizard_protocol="bogus"
run config_callback
check "config_callback exit code" 0 $?
grep -q '"logLevel": "debug"' "$TRIM_PKGVAR/config.json" && ok "invalid logLevel ignored, previous kept" || bad "invalid logLevel was written"
grep -q '"protocol": "http2"' "$TRIM_PKGVAR/config.json" && ok "invalid protocol ignored, previous kept" || bad "invalid protocol was written"
unset wizard_log_level wizard_protocol

section "a token tunnel keeps the token out of the process command line"
# A token passed as an argv flag would be readable by any local user through
# /proc/<pid>/cmdline, so the server must hand it over via TUNNEL_TOKEN.
TOKEN="$(printf '%s' '{"a":"00000000000000000000000000000000","t":"00000000-0000-0000-0000-000000000000","s":"SECRETTOKENMARKER"}' | base64 | tr -d '\n')"
CREATED="$(curl -s --max-time 5 --unix-socket "$TRIM_APPDEST/app.sock" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"argv-test\",\"type\":\"token\",\"token\":\"$TOKEN\"}" \
    http://localhost/api/tunnels)"
TID="$(printf '%s' "$CREATED" | grep -o '"id":"[^"]*"' | head -n 1 | cut -d'"' -f4)"
if [ -n "$TID" ]; then ok "token tunnel created ($TID)"; else bad "could not create tunnel: $CREATED"; fi

# autoStart 默认开启：创建响应里就该是 starting，而不是 stopped（否则用户保存后
# 看到「已停止」，会以为没生效）。
case "$CREATED" in
*'"status":"starting"'*) ok "auto-start: tunnel starts immediately on create" ;;
*) bad "created tunnel did not auto-start: $CREATED" ;;
esac

LIST="$(curl -s --max-time 5 --unix-socket "$TRIM_APPDEST/app.sock" http://localhost/api/tunnels)"
case "$LIST" in
*"$TOKEN"*) bad "the raw token is returned by the list API" ;;
*) ok "list API returns no token plaintext" ;;
esac
case "$LIST" in
*'"hasToken":true'*) ok "list API reports hasToken" ;;
*) bad "list API did not report hasToken: $LIST" ;;
esac

curl -s --max-time 5 --unix-socket "$TRIM_APPDEST/app.sock" \
    -X POST "http://localhost/api/tunnels/$TID/start" >/dev/null

# cloudflared may exit quickly with a bogus token, so sample the process as soon
# as it appears rather than after a fixed sleep.
CFPID=""
CMDLINE=""
ENVIRON=""
for _ in $(seq 1 50); do
    # 只认本测试自己起的进程：安装目录前缀必须落在 fakeinstall 下。
    # 真机上用户的 cloudflared（/vol1/@appcenter/...）也在跑，按名字抓会抓错。
    CFPID="$(ps -eo pid=,args= | awk -v d="$TRIM_APPDEST" '$0 ~ d && /bin\/cloudflared/ {print $1}' | head -1)"
    if [ -n "$CFPID" ]; then
        CMDLINE="$(tr '\0' ' ' <"/proc/$CFPID/cmdline" 2>/dev/null)"
        ENVIRON="$(tr '\0' '\n' <"/proc/$CFPID/environ" 2>/dev/null)"
        [ -n "$CMDLINE" ] && break
    fi
    sleep 0.1
done

if [ -n "$CFPID" ]; then
    ok "captured the cloudflared process (pid $CFPID)"
    case "$CMDLINE" in
    *SECRETTOKENMARKER*) bad "token is visible in argv: $CMDLINE" ;;
    *) ok "argv is token-free: ${CMDLINE#*cloudflared }" ;;
    esac
    if printf '%s' "$ENVIRON" | grep -q "^TUNNEL_TOKEN=$TOKEN$"; then
        ok "token delivered through the TUNNEL_TOKEN environment variable"
    else
        bad "TUNNEL_TOKEN not found in the child environment"
    fi
else
    bad "no cloudflared process appeared"
fi

curl -s --max-time 5 --unix-socket "$TRIM_APPDEST/app.sock" \
    -X DELETE "http://localhost/api/tunnels/$TID" >/dev/null

section "main stop"
run main stop
check "stop exit code" 0 $?
run main status
check "status reports not-running" 3 $?
[ -S "$TRIM_APPDEST/app.sock" ] && bad "app.sock left behind" || ok "app.sock removed"

section "uninstall keeping data (wizard_remove_data=false)"
run uninstall_init
check "uninstall_init exit code" 0 $?
export wizard_remove_data=false
run uninstall_callback
check "uninstall_callback exit code" 0 $?
[ -f "$TRIM_PKGVAR/config.json" ] && ok "config.json preserved" || bad "config.json was deleted"
[ -d "$TRIM_PKGVAR/.cloudflared" ] && ok "credentials preserved" || bad "credentials were deleted"

section "uninstall removing data (wizard_remove_data=true)"
export wizard_remove_data=true
run uninstall_callback
check "uninstall_callback exit code" 0 $?
[ -f "$TRIM_PKGVAR/config.json" ] && bad "config.json still present" || ok "config.json removed"
[ -d "$TRIM_PKGVAR/.cloudflared" ] && bad "credentials still present" || ok "credentials removed"

section "start fails loudly when the payload is missing"
setup_install_root
make_cloudflared_stub
export TRIM_APPDEST="$SCRATCH/empty-target"
mkdir -p "$TRIM_APPDEST"
run main start
RC=$?
if [ "$RC" -ne 0 ]; then ok "start fails without the service binary (rc=$RC)"; else bad "start succeeded without the service binary"; fi
if grep -q '应用文件缺失' "$TRIM_TEMP_LOGFILE" 2>/dev/null; then
    ok "error surfaced through TRIM_TEMP_LOGFILE"
else
    bad "TRIM_TEMP_LOGFILE did not get the error: $(cat "$TRIM_TEMP_LOGFILE" 2>/dev/null)"
fi
run main status
check "status reports not-running after failure" 3 $?

# ------------------------------------------------------------------- teardown
trap - EXIT
cleanup

printf '\n\033[1m%s passed, %s failed\033[0m\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
