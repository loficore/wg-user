#!/bin/sh
# test/shell/cli_test.sh — the shell CLI is a single-process supervisor:
# start / stop / restart / reload / status / log / save-config / version, with
# no "tunnel" namespace and no second process.
#
# The test builds a throwaway module root under $TMPDIR and stubs bin/wg-user,
# so it needs no Go toolchain, no real binary and no network, and never touches
# /data/adb. Every CLI call runs with the fake root as the working directory and
# a relative $0, exactly how service.sh and the WebUI invoke it.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/wg-user-cli.XXXXXX")
MOD="$TMP/module"

cleanup() {
    # The stub daemon and the decoy both outlive the scripts that started them.
    # Kill them by the pids this script recorded, never via run/wg-user.pid:
    # that file deliberately holds a foreign pid in the stale-pidfile case.
    for p in ${daemon_pid:-} ${decoy:-}; do
        kill -KILL "$p" 2>/dev/null || true
    done
    rm -rf "$TMP"
}
trap cleanup EXIT HUP INT TERM

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}

# must_contain <needle> <haystack>
must_contain() {
    printf '%s\n' "$2" | grep -F -- "$1" >/dev/null || fail "missing '$1' in output:
$2"
}

# must_not_contain <needle> <haystack>
must_not_contain() {
    if printf '%s\n' "$2" | grep -F -- "$1" >/dev/null; then
        fail "unexpected '$1' in output:
$2"
    fi
}

# cli <args...> — run wg-user from the fake module root. Prints combined
# stdout+stderr; the caller checks the exit status.
cli() {
    ( cd "$MOD" && sh wg-user "$@" 2>&1 )
}

# ---------- fake module root ----------

printf '== building fake module root\n'
mkdir -p "$MOD/bin" "$MOD/common" "$MOD/etc" "$MOD/run" "$MOD/log"
cp "$ROOT/wg-user" "$MOD/wg-user"
cp "$ROOT/common/util.sh" "$ROOT/common/abi.sh" "$MOD/common/"
printf 'id=wg-user\nname=wg-user\nversion=0.9.9\nversionCode=1\n' > "$MOD/module.prop"
printf '[Interface]\nAddress = 10.0.0.2/32\n' > "$MOD/etc/wireproxy.conf"

cat > "$MOD/bin/wg-user" <<'STUB'
#!/bin/sh
# Stand-in for the Go daemon. `run` writes a fixed log, then stays up: that is
# all the CLI's start/stop/status/log paths care about. Every invocation is
# appended to run/stub-calls so the tests can assert which subcommands ran (and
# how often) without timing races.
calls="$(dirname -- "$0")/../run/stub-calls"
case "${1:-run}" in
    run)
        printf '%s\n' "$*" >> "$calls"
        i=0
        while [ "$i" -lt 10 ]; do
            printf 'stub log line %s\n' "$i"
            i=$((i + 1))
        done
        while :; do sleep 1; done
        ;;
    configtest)
        printf '%s\n' "$*" >> "$calls"
        # The staged file is named by -c; a config containing INVALID stands in
        # for the Go parser rejecting it.
        staged=""
        while [ "$#" -gt 0 ]; do
            if [ "$1" = "-c" ]; then shift; staged="${1:-}"; fi
            shift
        done
        if [ -n "$staged" ] && grep -q INVALID "$staged" 2>/dev/null; then
            printf 'configtest: %s: unknown section\n' "$staged" >&2
            exit 2
        fi
        printf 'Config OK\n'
        ;;
    version)
        printf '%s\n' "$*" >> "$calls"
        printf 'wg-user 0.9.9\n'
        ;;
    *)
        printf 'wg-user: unknown subcommand\n' >&2
        exit 2
        ;;
esac
STUB
chmod 0755 "$MOD/bin/wg-user"

# ---------- status when stopped ----------

printf '== status (stopped)\n'
rc=0
out=$(cli status) || rc=$?
[ "$rc" -eq 1 ] || fail "status (stopped): exit $rc, want 1
$out"
must_contain 'wg-user: stopped' "$out"
must_not_contain 'tunnel:' "$out"

# ---------- the tunnel namespace is gone ----------

printf '== tunnel status is an unknown subcommand\n'
rc=0
out=$(cli tunnel status) || rc=$?
[ "$rc" -eq 2 ] || fail "tunnel status: exit $rc, want 2
$out"
must_contain 'unknown subcommand' "$out"

# ---------- start ----------

printf '== start\n'
rc=0
out=$(cli start) || rc=$?
[ "$rc" -eq 0 ] || fail "start: exit $rc, want 0
$out"
[ -f "$MOD/run/wg-user.pid" ] || fail "start: no run/wg-user.pid"
pid=$(cat "$MOD/run/wg-user.pid")
daemon_pid=$pid
kill -0 "$pid" 2>/dev/null || fail "start: pid $pid is not alive"
[ -f "$MOD/log/wg-user.log" ] || fail "start: no log/wg-user.log"

# ---------- status while running, before the daemon wrote run/state ----------

printf '== status (running, no state file)\n'
rc=0
out=$(cli status) || rc=$?
[ "$rc" -eq 0 ] || fail "status (running): exit $rc, want 0
$out"
must_contain "wg-user: running (pid $pid)" "$out"
must_contain 'transport: unknown' "$out"

# ---------- status reads run/state ----------

printf '== status (running, state file written)\n'
printf 'pid=%s\ntransport=wstunnel\ntransport_state=connected\ntransport_detail=wss://example.com:443\nupdated=1763000000\n' \
    "$pid" > "$MOD/run/state"
rc=0
out=$(cli status) || rc=$?
[ "$rc" -eq 0 ] || fail "status (with state): exit $rc, want 0
$out"
must_contain "wg-user: running (pid $pid)" "$out"
must_contain 'transport: wss://example.com:443 (connected)' "$out"

# ---------- log ----------

printf '== log 5\n'
rc=0
out=$(cli log 5) || rc=$?
[ "$rc" -eq 0 ] || fail "log 5: exit $rc, want 0
$out"
lines=$(printf '%s\n' "$out" | wc -l | tr -d ' ')
[ "$lines" -eq 5 ] || fail "log 5: printed $lines lines, want 5
$out"
first=$(printf '%s\n' "$out" | head -n 1)
last=$(printf '%s\n' "$out" | tail -n 1)
[ "$first" = 'stub log line 5' ] || fail "log 5: first line '$first', want 'stub log line 5'"
[ "$last" = 'stub log line 9' ] || fail "log 5: last line '$last', want 'stub log line 9'"

# ---------- stop ----------

printf '== stop\n'
rc=0
out=$(cli stop) || rc=$?
[ "$rc" -eq 0 ] || fail "stop: exit $rc, want 0
$out"
[ ! -f "$MOD/run/wg-user.pid" ] || fail "stop: run/wg-user.pid still present"
! kill -0 "$pid" 2>/dev/null || fail "stop: pid $pid still alive"

# ---------- status ignores a stale state file once the process is gone ----------

printf '== status (stopped, stale state file)\n'
rc=0
out=$(cli status) || rc=$?
[ "$rc" -eq 1 ] || fail "status (stopped again): exit $rc, want 1
$out"
must_contain 'wg-user: stopped' "$out"
must_not_contain 'transport:' "$out"
must_not_contain 'tunnel:' "$out"

# ---------- a stale pidfile must not name an unrelated live process ----------
# pidfiles in /data/adb outlive crashes and unclean reboots; after PID reuse the
# pid can belong to anything. Trusting it would report "already running" (so the
# module never comes up) or SIGTERM a stranger as root.

printf '== stale pidfile (pid reused by another process)\n'
# The decoy is deliberately the weakest case: a live stranger that even carries
# our binary's path in its argv (at argv[3], not argv[0]). A substring match
# adopts it; only an argv-position check rejects it. Launched through a
# subshell so this shell does not track it as a job (a "Killed" notice would
# pollute the output).
( sh -c 'while :; do sleep 1; done' "$MOD/bin/wg-user" & printf '%s\n' "$!" > "$TMP/decoy.pid" )
decoy=$(cat "$TMP/decoy.pid")

# (a) status must not claim the stranger is our daemon.
printf '%s\n' "$decoy" > "$MOD/run/wg-user.pid"
rc=0
out=$(cli status) || rc=$?
[ "$rc" -eq 1 ] || fail "status (stale pidfile): exit $rc, want 1
$out"
must_contain 'wg-user: stopped' "$out"
must_not_contain 'transport:' "$out"

# (b) stop must not signal it. The pidfile still names the stranger here, so
# this is the assertion that bites when cmd_stop trusts the file blindly.
rc=0
out=$(cli stop) || rc=$?
[ "$rc" -eq 0 ] || fail "stop (stale pidfile): exit $rc, want 0
$out"
[ ! -f "$MOD/run/wg-user.pid" ] || fail "stop (stale pidfile): run/wg-user.pid still present"
kill -0 "$decoy" 2>/dev/null \
    || fail "stop signalled an unrelated process (pid $decoy is gone)"

# (c) start must not believe "already running": put the stale pid back and
# require a fresh daemon under a different, live pid.
printf '%s\n' "$decoy" > "$MOD/run/wg-user.pid"
rc=0
out=$(cli start) || rc=$?
[ "$rc" -eq 0 ] || fail "start (stale pidfile): exit $rc, want 0
$out"
must_not_contain 'already running' "$out"
[ -f "$MOD/run/wg-user.pid" ] || fail "start (stale pidfile): no run/wg-user.pid"
pid=$(cat "$MOD/run/wg-user.pid")
daemon_pid=$pid
[ "$pid" != "$decoy" ] || fail "start (stale pidfile): pidfile still names the decoy pid $decoy"
kill -0 "$pid" 2>/dev/null || fail "start (stale pidfile): pid $pid is not alive"

# ---------- run/state's pid must match the pidfile's ----------
# The state file carries a pid for exactly this reason: another daemon's
# transport must never be reported as ours.

printf '== status (state pid does not match the pidfile)\n'
printf 'pid=%s\ntransport=wstunnel\ntransport_state=connected\ntransport_detail=wss://other.example:443\nupdated=1763000000\n' \
    "$decoy" > "$MOD/run/state"
rc=0
out=$(cli status) || rc=$?
[ "$rc" -eq 0 ] || fail "status (state pid mismatch): exit $rc, want 0
$out"
must_contain "wg-user: running (pid $pid)" "$out"
must_contain 'transport: unknown' "$out"
must_not_contain 'wss://other.example:443' "$out"

rc=0
out=$(cli stop) || rc=$?
[ "$rc" -eq 0 ] || fail "stop (after stale pidfile): exit $rc, want 0
$out"
kill -0 "$decoy" 2>/dev/null \
    || fail "stop signalled the unrelated process (pid $decoy is gone)"
must_not_contain 'wg-user not running' "$out"

# ---------- save-config validates + copies; the watcher owns the restart ----------
# service.sh's watcher restarts the daemon whenever etc/wireproxy.conf's mtime
# changes. save-config must not also stop+start: that is two restarts per WebUI
# save (and two concurrent restarts can leave a dead pid in run/wg-user.pid).

printf '== save-config\n'
calls="$MOD/run/stub-calls"
rc=0
out=$(cli start) || rc=$?
[ "$rc" -eq 0 ] || fail "start (before save-config): exit $rc, want 0
$out"
daemon_pid=$(cat "$MOD/run/wg-user.pid")
runs_before=$(grep -c '^run' "$calls" 2>/dev/null || true)

printf '[Interface]\nAddress = 10.0.0.9/32\n' > "$TMP/new.conf"
rc=0
out=$(cli save-config "$TMP/new.conf") || rc=$?
[ "$rc" -eq 0 ] || fail "save-config: exit $rc, want 0
$out"
cmp -s "$TMP/new.conf" "$MOD/etc/wireproxy.conf" \
    || fail "save-config: etc/wireproxy.conf was not replaced"
# (a) the staged file itself was validated, not the live config.
grep -F -x -- "configtest -c $TMP/new.conf" "$calls" >/dev/null \
    || fail "save-config did not configtest the staged file:
$(cat "$calls")"
# (b) no restart from save-config: same daemon, same pid, no new `run`.
[ -f "$MOD/run/wg-user.pid" ] || fail "save-config: daemon pidfile disappeared"
[ "$(cat "$MOD/run/wg-user.pid")" = "$daemon_pid" ] \
    || fail "save-config restarted the daemon (pid ${daemon_pid} -> $(cat "$MOD/run/wg-user.pid"))"
runs_after=$(grep -c '^run' "$calls" 2>/dev/null || true)
[ "$runs_after" = "$runs_before" ] \
    || fail "save-config started the daemon $((runs_after - runs_before)) time(s); the watcher owns that restart"
must_contain 'within ~5s' "$out"

# (c) a config that fails configtest is rejected before it lands on disk.
printf '== save-config rejects an invalid config\n'
cp -f "$MOD/etc/wireproxy.conf" "$TMP/before-bad.conf"
printf '[Interface]\nINVALID\n' > "$TMP/bad.conf"
rc=0
out=$(cli save-config "$TMP/bad.conf") || rc=$?
[ "$rc" -ne 0 ] || fail "save-config accepted a config that failed configtest
$out"
cmp -s "$TMP/before-bad.conf" "$MOD/etc/wireproxy.conf" \
    || fail "save-config replaced the live config with an invalid file"
must_contain 'configtest FAILED' "$out"
[ "$(cat "$MOD/run/wg-user.pid")" = "$daemon_pid" ] \
    || fail "save-config restarted the daemon after a failed configtest"

rc=0
out=$(cli stop) || rc=$?
[ "$rc" -eq 0 ] || fail "stop (after save-config): exit $rc, want 0
$out"

# ---------- version ----------

printf '== version\n'
rc=0
out=$(cli version) || rc=$?
[ "$rc" -eq 0 ] || fail "version: exit $rc, want 0
$out"
must_contain '0.9.9' "$out"

# ---------- the SIGTERM window is long enough for a graceful shutdown ----------
# Asserted from the source, not by sleeping: the Go daemon may spend up to 10s
# finishing an in-flight UDP write before it exits.

printf '== stop timeout\n'
timeout=$(sed -n 's/^STOP_TIMEOUT=//p' "$MOD/wg-user")
case "$timeout" in
    ''|*[!0-9]*)
        fail "wg-user: no numeric STOP_TIMEOUT assignment (got '$timeout')"
        ;;
esac
[ "$timeout" -ge 20 ] \
    || fail "wg-user: STOP_TIMEOUT=$timeout, want >= 20 (a 10s UDP flush must fit)"

# ---------- common/abi.sh selects the single binary ----------

printf '== select_binary\n'
mkdir -p "$TMP/fakebin" "$MOD/bin/arm64-v8a"
printf '#!/bin/sh\necho arm64-v8a\n' > "$TMP/fakebin/getprop"
chmod 0755 "$TMP/fakebin/getprop"
cp "$MOD/bin/wg-user" "$MOD/bin/arm64-v8a/wg-user"
PATH="$TMP/fakebin:$PATH"
export PATH
rc=0
out=$(sh -c '. "$1/common/abi.sh" && select_binary "$1" "$2"' _ "$MOD" "$TMP/selected" 2>&1) || rc=$?
[ "$rc" -eq 0 ] || fail "select_binary: exit $rc, want 0
$out"
[ -x "$TMP/selected" ] || fail "select_binary: $TMP/selected missing or not executable"
cmp -s "$MOD/bin/arm64-v8a/wg-user" "$TMP/selected" \
    || fail "select_binary: copied the wrong file"
must_contain 'arm64-v8a' "$out"

printf 'PASS: cli_test.sh\n'
