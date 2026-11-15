#!/bin/sh
# test/shell/install_test.sh — customize.sh: one binary per arch, config
# seeding, and the old etc/tunnel.conf -> [Transport] migration.
#
# Builds throwaway module roots under $TMPDIR and *sources* customize.sh the
# way KernelSU/Magisk do, so the test needs no root, no network, no Go
# toolchain, never writes outside $TMPDIR and never touches /data/adb.
#
# A getprop stub is put first on PATH: common/abi.sh asks for
# ro.product.cpu.abi, and without an answer the dev box yields "unknown" and
# select_binary fails.
set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
TMP=$(mktemp -d "${TMPDIR:-/tmp}/wg-user-install.XXXXXX")
STUB="$TMP/stub"

cleanup() {
    # Background stubs started by the uninstall case; they must not outlive the
    # suite even when an assertion fails first.
    for _p in ${_wpid:-} ${_spid:-}; do
        kill -TERM "$_p" 2>/dev/null || true
    done
    rm -rf "$TMP"
}
trap cleanup EXIT HUP INT TERM

fail() {
    printf 'FAIL: %s\n' "$*" >&2
    exit 1
}
pass() { printf 'ok: %s\n' "$*"; }

# ---------- assertions ----------

# file_mode <path> — octal permission bits (GNU stat, BSD stat fallback).
file_mode() {
    stat -c '%a' "$1" 2>/dev/null && return 0
    stat -f '%Lp' "$1"
}

assert_exists() {
    [ -e "$1" ] || fail "expected $1 to exist"
}

assert_absent() {
    [ ! -e "$1" ] || fail "expected $1 to be gone"
}

assert_exec() {
    assert_exists "$1"
    [ -x "$1" ] || fail "expected $1 to be executable"
}

assert_mode() {
    _got=$(file_mode "$1")
    [ "$_got" = "$2" ] || fail "expected mode $2 on $1, got $_got"
}

assert_contains() {
    grep -F -- "$1" "$2" >/dev/null 2>&1 || fail "expected '$1' in $2:
$(cat "$2" 2>/dev/null)"
}

assert_not_contains() {
    if grep -F -- "$1" "$2" >/dev/null 2>&1; then
        fail "unexpected '$1' in $2:
$(cat "$2")"
    fi
}

# assert_line <file> <exact line> — the line must be present verbatim.
assert_line() {
    grep -F -x -- "$1" "$2" >/dev/null 2>&1 || fail "expected line '$1' in $2:
$(cat "$2")"
}

assert_count() {
    _got=$(grep -F -c -- "$1" "$2" 2>/dev/null || true)
    [ "${_got:-0}" = "$3" ] || fail "expected $3 occurrences of '$1' in $2, got ${_got:-0}:
$(cat "$2")"
}

# ---------- fake module root ----------

printf '== building getprop stub (ro.product.cpu.abi = arm64-v8a)\n'
mkdir -p "$STUB"
cat > "$STUB/getprop" <<'STUB_EOF'
#!/bin/sh
# The only property common/abi.sh reads is ro.product.cpu.abi.
case "${1:-}" in
    ro.product.cpu.abi) echo arm64-v8a ;;
    *) : ;;
esac
STUB_EOF
chmod 0755 "$STUB/getprop"
PATH="$STUB:$PATH"
export PATH

# new_module <dir> — minimal extracted-module tree, as the installer leaves it.
new_module() {
    _m=$1
    mkdir -p "$_m/common" "$_m/etc" "$_m/bin/arm64-v8a" "$_m/run" "$_m/log"
    printf 'id=wg-user\nname=wg-user\nversion=0.0.0\nversionCode=1\n' > "$_m/module.prop"
    cp "$ROOT/common/util.sh" "$ROOT/common/abi.sh" "$_m/common/"
    cp "$ROOT/customize.sh" "$_m/customize.sh"
    cp "$ROOT/etc/wireproxy.conf.example" "$_m/etc/"
    cat > "$_m/bin/arm64-v8a/wg-user" <<'STUB_EOF'
#!/bin/sh
# Placeholder for the Go daemon: customize.sh only copies and chmods it.
exit 0
STUB_EOF
    chmod 0755 "$_m/bin/arm64-v8a/wg-user"
}

# run_install <dir> [path] — source customize.sh exactly like the installer
# does. The optional second argument replaces PATH for this run only.
run_install() {
    ( cd "$1" && MODPATH="$1" PATH="${2:-$PATH}" sh -c '. "$MODPATH/customize.sh"' )
}

# upgrade_module <dir> — a module root that also carries an 0.2.x
# etc/wireproxy.conf: no [Transport], Endpoint at the old local-udp.
upgrade_module() {
    new_module "$1"
    cat > "$1/etc/wireproxy.conf" <<'CONF_EOF'
[Interface]
Address = 10.0.0.2/32
PrivateKey = <old-key>

[Peer]
PublicKey = <old-pub>
Endpoint = 127.0.0.1:51820
AllowedIPs = 0.0.0.0/0
CONF_EOF
    chmod 0600 "$1/etc/wireproxy.conf"
}

# A PATH stub that fails only customize.sh's [Transport] writer (whose awk
# program mentions emit_transport), so a migration can be made to fail while
# old_conf_value and strip_comment still work. Deterministic under any uid —
# unlike making etc/ read-only, which root would defeat.
REAL_AWK=$(command -v awk)
STUB_FAIL="$TMP/stub-fail-awk"
mkdir -p "$STUB_FAIL"
cat > "$STUB_FAIL/awk" <<STUB_EOF
#!/bin/sh
case "\${1:-}" in
    *emit_transport*) exit 1 ;;
esac
exec "$REAL_AWK" "\$@"
STUB_EOF
chmod 0755 "$STUB_FAIL/awk"

# ---------- A. fresh install ----------

printf '== A. fresh install seeds etc/wireproxy.conf from the example\n'
mod="$TMP/a"
new_module "$mod"
assert_absent "$mod/etc/wireproxy.conf"

if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on a fresh install:
$out"
fi

assert_exists "$mod/etc/wireproxy.conf"
assert_mode "$mod/etc/wireproxy.conf" 600
assert_contains '[Interface]' "$mod/etc/wireproxy.conf"
assert_contains '[Transport]' "$mod/etc/wireproxy.conf"
# The seeded example must stay parseable: an unknown/absent Mode would fail
# configtest on every fresh install.
assert_line 'Mode = off' "$mod/etc/wireproxy.conf"
# select_binary must have installed the single wg-user binary for this ABI.
assert_exec "$mod/bin/wg-user"
pass "fresh install"

# ---------- B. upgrade: old tunnel.conf migrates ----------

printf '== B. upgrade: old etc/tunnel.conf folds into [Transport]\n'
mod="$TMP/b"
new_module "$mod"
# An 0.2.x wireproxy.conf: no [Transport], Endpoint points at the old local
# UDP listener (tunnel.conf's local-udp).
cat > "$mod/etc/wireproxy.conf" <<'CONF_EOF'
[Interface]
Address = 10.0.0.2/32
PrivateKey = <old-key>

[Peer]
PublicKey = <old-pub>
Endpoint = 127.0.0.1:51820
AllowedIPs = 0.0.0.0/0
CONF_EOF
chmod 0644 "$mod/etc/wireproxy.conf"
# An 0.2.x tunnel.conf: flat mode at top level, the rest under [wstunnel].
# remote-udp is the SERVER-side delivery target and differs from local-udp, so
# the migrated [Peer] Endpoint must be rewritten to it.
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
# old flat transport config
mode = wstunnel

[wstunnel]
local-udp = 127.0.0.1:51820
remote-udp = 127.0.0.1:51999
remote = wss://x:443
username = u
password = p
insecure = true
CONF_EOF
chmod 0600 "$mod/etc/tunnel.conf"

if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on an upgrade:
$out"
fi

conf="$mod/etc/wireproxy.conf"
assert_count '[Transport]' "$conf" 1
assert_line 'Mode = wstunnel' "$conf"
assert_line 'URL = wss://x:443' "$conf"
assert_line 'Username = u' "$conf"
assert_line 'Password = p' "$conf"
assert_line 'Insecure = true' "$conf"
assert_not_contains 'local-udp' "$conf"
# Endpoint changed meaning: server-side delivery target, i.e. remote-udp.
assert_line 'Endpoint = 127.0.0.1:51999' "$conf"
assert_not_contains '127.0.0.1:51820' "$conf"
assert_count 'Endpoint' "$conf" 1
assert_mode "$conf" 600
# The old file is kept for the user's comments, just parked.
assert_absent "$mod/etc/tunnel.conf"
assert_exists "$mod/etc/tunnel.conf.migrated"
assert_mode "$mod/etc/tunnel.conf.migrated" 600
assert_contains 'mode = wstunnel' "$mod/etc/tunnel.conf.migrated"
pass "migration"

# ---------- C. idempotency ----------

printf '== C. second run adds no second [Transport]\n'
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on a re-install:
$out"
fi
assert_count '[Transport]' "$conf" 1
assert_line 'Endpoint = 127.0.0.1:51999' "$conf"
assert_mode "$conf" 600
assert_exists "$mod/etc/tunnel.conf.migrated"
assert_absent "$mod/etc/tunnel.conf"
pass "idempotent re-install"

# A tunnel.conf can also reappear (restored from a backup, or the copy in an
# old module dir). The source is parked, but [Transport] must still not be
# appended a second time.
printf '== C2. a reappearing tunnel.conf is parked, not appended again\n'
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://y:443
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero when etc/tunnel.conf reappeared:
$out"
fi
assert_count '[Transport]' "$conf" 1
# The existing section wins; the stale file is not merged into it.
assert_line 'URL = wss://x:443' "$conf"
assert_not_contains 'wss://y:443' "$conf"
assert_absent "$mod/etc/tunnel.conf"
assert_exists "$mod/etc/tunnel.conf.migrated"
pass "existing [Transport] kept"

# ---------- D. unknown mode ----------

printf '== D. mode = on (0.2.x: off) migrates as Mode = off\n'
mod="$TMP/d"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
# 0.2.x documented "any other value is treated as off".
mode = on

[wstunnel]
local-udp = 127.0.0.1:51820
remote-udp = 127.0.0.1:51999
remote = wss://x:443
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on mode = on:
$out"
fi
assert_line 'Mode = off' "$mod/etc/wireproxy.conf"
assert_not_contains 'Mode = on' "$mod/etc/wireproxy.conf"
# off mode: Endpoint stays the real server address, remote-udp is inert.
assert_line 'Endpoint = 127.0.0.1:51820' "$mod/etc/wireproxy.conf"
# No TLS is in play, so no Insecure key (and no advice to set one).
assert_not_contains 'Insecure =' "$mod/etc/wireproxy.conf"
printf '%s\n' "$out" | grep -F 'Endpoint must be the real WireGuard server address' >/dev/null \
    || fail "expected the unknown-mode warning to explain the Endpoint requirement:
$out"
pass "unknown mode becomes off"

# ---------- E. unsupported URL scheme ----------

printf '== E. wts:// remote is dropped and Mode falls back to off\n'
mod="$TMP/e"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wts://host:443
remote-udp = 127.0.0.1:51999
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on a wts:// remote:
$out"
fi
assert_line 'Mode = off' "$mod/etc/wireproxy.conf"
assert_not_contains 'URL =' "$mod/etc/wireproxy.conf"
assert_not_contains 'wts://' "$mod/etc/wireproxy.conf"
assert_line 'Endpoint = 127.0.0.1:51820' "$mod/etc/wireproxy.conf"
assert_not_contains 'Insecure =' "$mod/etc/wireproxy.conf"
printf '%s\n' "$out" | grep -F 'Endpoint must be the real WireGuard server address' >/dev/null \
    || fail "expected the bad-scheme warning to explain the Endpoint requirement:
$out"
pass "unsupported URL scheme"

# ---------- F. remote-udp with a trailing comment ----------

printf '== F. remote-udp with a trailing comment yields a clean Endpoint\n'
mod="$TMP/f"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
remote-udp = 127.0.0.1:51999  # server side
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on a commented remote-udp:
$out"
fi
assert_line 'Endpoint = 127.0.0.1:51999' "$mod/etc/wireproxy.conf"
assert_not_contains 'server side' "$mod/etc/wireproxy.conf"
pass "trailing comment stripped"

# ---------- G. existing lowercase [transport] ----------

printf '== G. an existing lowercase [transport] is not duplicated\n'
mod="$TMP/g"
upgrade_module "$mod"
cat >> "$mod/etc/wireproxy.conf" <<'CONF_EOF'

[transport]
mode = wstunnel
url = wss://y:443
CONF_EOF
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero with a lowercase [transport] present:
$out"
fi
# go-ini matches section names case-insensitively and takes the first one, so
# inserting [Transport] above [transport] would shadow the user's settings.
assert_count '[transport]' "$mod/etc/wireproxy.conf" 1
assert_not_contains '[Transport]' "$mod/etc/wireproxy.conf"
assert_line 'url = wss://y:443' "$mod/etc/wireproxy.conf"
assert_not_contains 'wss://x:443' "$mod/etc/wireproxy.conf"
assert_absent "$mod/etc/tunnel.conf"
pass "case-insensitive [Transport] guard"

# ---------- H. failed migration aborts the install ----------

printf '== H. a failed migration is fatal\n'
mod="$TMP/h"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
CONF_EOF
if out=$(run_install "$mod" "$STUB_FAIL:$PATH" 2>&1); then
    fail "customize.sh exited 0 although the [Transport] writer failed:
$out"
fi
printf '%s\n' "$out" | grep -F 'aborting the install' >/dev/null \
    || fail "expected the aborted install to say why:
$out"
# Nothing was rewritten, the old file is untouched, no temp file leaked.
assert_not_contains '[Transport]' "$mod/etc/wireproxy.conf"
assert_exists "$mod/etc/tunnel.conf"
leftovers=$(ls "$mod"/etc/*.migrating.* 2>/dev/null || true)
[ -z "$leftovers" ] || fail "migration temp file leaked: $leftovers"
pass "failed migration aborts"

# ---------- I. uninstall order: watcher before daemon ----------

# A 5 s reload watcher can call `wg-user reload` and start the daemon again, so
# uninstall.sh must TERM the watcher before delegating to `wg-user stop`.
# Waiting for a real reload race is not portable or fast, so the order is read
# off `sh -x`, which is emitted synchronously in execution order.
printf '== I. uninstall stops the reload watcher before the daemon\n'
mod="$TMP/i"
new_module "$mod"
cp "$ROOT/uninstall.sh" "$mod/uninstall.sh"
cat > "$mod/wg-user" <<'STUB_EOF'
#!/bin/sh
# uninstall.sh delegates the daemon stop to this script.
printf '%s\n' "$*" > "$(dirname -- "$0")/stop.called"
STUB_EOF
chmod 0755 "$mod/wg-user"
cat > "$mod/wg-user-reload" <<'STUB_EOF'
#!/bin/sh
# Stand-in for service.sh's watcher: exits as soon as it is TERMed.
trap 'exit 0' TERM
while :; do :; done
STUB_EOF
chmod 0755 "$mod/wg-user-reload"
"$mod/wg-user-reload" &
_wpid=$!
printf '%s\n' "$_wpid" > "$mod/run/watcher.pid"

if ! out=$(sh -x "$mod/uninstall.sh" 2>&1); then
    fail "uninstall.sh exited non-zero:
$out"
fi
xtrace=$(printf '%s\n' "$out" | sed 's/^[+ ]*//')
kill_line=$(printf '%s\n' "$xtrace" | grep -n 'kill -TERM' | head -n 1 | cut -d: -f1)
stop_line=$(printf '%s\n' "$xtrace" | grep -n 'wg-user stop' | head -n 1 | cut -d: -f1)
[ -n "$kill_line" ] || fail "uninstall.sh never TERMed the watcher:
$out"
[ -n "$stop_line" ] || fail "uninstall.sh never called 'wg-user stop':
$out"
[ "$kill_line" -lt "$stop_line" ] \
    || fail "the watcher must be TERMed before 'wg-user stop' (trace lines $kill_line vs $stop_line)"
assert_exists "$mod/stop.called"
assert_contains 'stop' "$mod/stop.called"
assert_absent "$mod/run/watcher.pid"
wait "$_wpid" 2>/dev/null || true
pass "watcher stopped before the daemon"

# Same again with a stale pidfile that names a stranger: it must not be
# signalled, but the pidfile is still cleaned up and the uninstall still exits 0.
printf '== I2. a watcher.pid naming a stranger is removed, not signalled\n'
mod="$TMP/i2"
new_module "$mod"
cp "$ROOT/uninstall.sh" "$mod/uninstall.sh"
sleep 30 &
_spid=$!
printf '%s\n' "$_spid" > "$mod/run/watcher.pid"
if ! out=$(sh -x "$mod/uninstall.sh" 2>&1); then
    fail "uninstall.sh exited non-zero on a stale stranger pid:
$out"
fi
kill -0 "$_spid" 2>/dev/null \
    || fail "uninstall.sh signalled a process that is not its watcher"
assert_absent "$mod/run/watcher.pid"
assert_absent "$mod/run/wg-user.pid"
kill -TERM "$_spid" 2>/dev/null || true
wait "$_spid" 2>/dev/null || true
pass "stranger pid left alone"

# ---------- J. no insecure key: 0.2.x did not verify ----------

# 0.2.x only passed --tls-verify-certificate for insecure=false; wstunnel's
# default is verification OFF. internal/config defaults Insecure to false =
# verify, so an absent key must migrate to true or self-signed upgrades break.
printf '== J. a missing insecure key migrates to Insecure = true\n'
mod="$TMP/j"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
remote-udp = 127.0.0.1:51999
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero without an insecure key:
$out"
fi
assert_line 'Mode = wstunnel' "$mod/etc/wireproxy.conf"
assert_line 'Insecure = true' "$mod/etc/wireproxy.conf"
printf '%s\n' "$out" | grep -F '0.2.0 did not verify' >/dev/null \
    || fail "expected the migration to say why verification stays off:
$out"
pass "missing insecure becomes true"

# ---------- K. explicit insecure = false stays false ----------

printf '== K. insecure = false migrates to Insecure = false\n'
mod="$TMP/k"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
insecure = false
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on insecure = false:
$out"
fi
assert_line 'Insecure = false' "$mod/etc/wireproxy.conf"
assert_not_contains 'Insecure = true' "$mod/etc/wireproxy.conf"
pass "explicit insecure = false"

# ---------- M. insecure = off is not "verification on" ----------

# 0.2.x only treated false|no|0 as verification ON; "off" fell through to "no
# flag" = verification OFF. Mapping "off" to Insecure = false would silently
# turn verification on for exactly the users this migration protects.
printf '== M. insecure = off stays Insecure = true\n'
mod="$TMP/m"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
insecure = off
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on insecure = off:
$out"
fi
assert_line 'Mode = wstunnel' "$mod/etc/wireproxy.conf"
assert_line 'Insecure = true' "$mod/etc/wireproxy.conf"
assert_not_contains 'Insecure = false' "$mod/etc/wireproxy.conf"
pass "insecure = off stays true"

# ---------- L. wstunnel without a remote URL degrades to off ----------

printf '== L. mode = wstunnel with no remote URL migrates as off\n'
mod="$TMP/l"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote-udp = 127.0.0.1:51999
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero when wstunnel had no URL:
$out"
fi
assert_line 'Mode = off' "$mod/etc/wireproxy.conf"
assert_not_contains 'Mode = wstunnel' "$mod/etc/wireproxy.conf"
assert_not_contains 'URL =' "$mod/etc/wireproxy.conf"
# off mode: Endpoint stays the real server address from the old file.
assert_line 'Endpoint = 127.0.0.1:51820' "$mod/etc/wireproxy.conf"
printf '%s\n' "$out" | grep -F 'Endpoint must be the real WireGuard server address' >/dev/null \
    || fail "expected the degraded transport to explain the Endpoint requirement:
$out"
pass "wstunnel without URL becomes off"

# ---------- N. multi-[Peer] wstunnel upgrade warns ----------

# internal/config rejects wstunnel mode unless there is exactly one [Peer]. The
# migration rewrites Endpoint in every peer block and cannot know which peer the
# user wants, so a two-peer 0.2.x file would migrate "successfully" and then
# exit 2 on every boot with no install-time signal. The installer must warn, and
# still complete the migration.
printf '== N. multi-[Peer] wstunnel upgrade warns\n'
mod="$TMP/n"
upgrade_module "$mod"
cat >> "$mod/etc/wireproxy.conf" <<'CONF_EOF'

[peer]
PublicKey = <old-pub-2>
Endpoint = 127.0.0.1:51820
AllowedIPs = 10.66.0.0/24
CONF_EOF
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
remote-udp = 127.0.0.1:51999
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on a two-peer upgrade:
$out"
fi
printf '%s\n' "$out" | grep -F '2 [Peer] sections' >/dev/null \
    || fail "expected the warning to count both [Peer] sections (case-insensitively):
$out"
printf '%s\n' "$out" | grep -F 'wstunnel mode supports exactly one peer' >/dev/null \
    || fail "expected the warning to say wstunnel mode supports one peer:
$out"
printf '%s\n' "$out" | grep -F 'refuse to start' >/dev/null \
    || fail "expected the warning to say the daemon will refuse the config:
$out"
# The migration still completes: [Transport] is written once and Endpoint is
# rewritten in both blocks (the daemon, not the installer, rejects the file).
assert_line 'Mode = wstunnel' "$mod/etc/wireproxy.conf"
assert_count 'Endpoint = 127.0.0.1:51999' "$mod/etc/wireproxy.conf" 2
assert_count '[Transport]' "$mod/etc/wireproxy.conf" 1
assert_absent "$mod/etc/tunnel.conf"
assert_exists "$mod/etc/tunnel.conf.migrated"
pass "multi-peer warning"

# A single-peer upgrade must stay quiet: the warning is only for the case the
# daemon cannot accept.
printf '== N2. a single [Peer] wstunnel upgrade does not warn\n'
mod="$TMP/n2"
upgrade_module "$mod"
cat > "$mod/etc/tunnel.conf" <<'CONF_EOF'
mode = wstunnel

[wstunnel]
remote = wss://x:443
remote-udp = 127.0.0.1:51999
CONF_EOF
if ! out=$(run_install "$mod" 2>&1); then
    fail "customize.sh exited non-zero on a single-peer upgrade:
$out"
fi
if printf '%s\n' "$out" | grep -F 'supports exactly one peer' >/dev/null; then
    fail "single-peer upgrade warned about peer count:
$out"
fi
assert_line 'Endpoint = 127.0.0.1:51999' "$mod/etc/wireproxy.conf"
pass "single peer stays quiet"

printf 'PASS: install_test\n'
