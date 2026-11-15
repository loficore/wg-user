#!/system/bin/sh
# wg-user autostart + hot reload.
# Invoked by KSU/Magisk/APatch at late_start. Do NOT block the boot path —
# fork everything.

set -eu

# Resolve module dir regardless of which root solution invoked us.
SELF="${0:-/data/adb/modules/wg-user/service.sh}"
case "$SELF" in
    /*) ;;
    *)  SELF="/data/adb/modules/wg-user/service.sh" ;;
esac
ROOT="$(CDPATH= cd -- "$(dirname -- "$SELF")" && pwd)"

. "$ROOT/common/util.sh"
. "$ROOT/common/abi.sh"

mkdir -p "$ROOT/run" "$ROOT/log"

# Sanity: ensure the binary is in place. customize.sh / install.sh should have
# run; if not (e.g. partial install), pick it now.
if [ ! -x "$ROOT/bin/wg-user" ]; then
    log_warn "bin/wg-user missing; running ABI selection"
    select_binary "$ROOT" "$ROOT/bin/wg-user" || {
        log_err "no wg-user binary available; service.sh exiting"
        exit 1
    }
fi

# Start the single daemon. A failure (bad config, port in use) is logged but not
# fatal: the reload watcher below retries after the next config edit, which is
# how a boot-time typo gets fixed without a reboot.
"$ROOT/wg-user" start || log_warn "wg-user failed to start; waiting for a config change"

# Start the reload watcher in the background. etc/wireproxy.conf is the only
# config the module has now; a transport change is an edit to that file.
WATCHER="$ROOT/wg-user-reload"
cat > "$WATCHER" <<'WATCHER_EOF'
#!/system/bin/sh
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
. "$ROOT/common/util.sh"

CONF="$ROOT/etc/wireproxy.conf"
LOG="$ROOT/log/wg-user.log"
INTERVAL="${WGU_RELOAD_INTERVAL:-5}"

# sig <file> — a content signature, not a timestamp. The watcher is the only
# restart path, and `stat -c %Y` has one-second granularity: two saves inside
# the same second leave the mtime unchanged, so the second write would never be
# applied while the UI promises "within ~5 s". cksum is POSIX (toybox and
# busybox ship it) and prints "<CRC> <bytes>"; including the length makes a
# collision need both a CRC and a length collision.
sig() {
    cksum < "$1" 2>/dev/null | awk '{print $1 "-" $2}'
}

conf_sig=""
if [ -f "$CONF" ]; then conf_sig=$(sig "$CONF" || true); fi

log_info "reload-watcher started (interval ${INTERVAL}s); watching $CONF (cksum)"

while :; do
    sleep "$INTERVAL" 2>/dev/null || sleep 5

    cur=""
    if [ -f "$CONF" ]; then cur=$(sig "$CONF" || true); fi

    [ "$cur" != "$conf_sig" ] || continue
    [ -n "$cur" ] || continue

    log_info "config change detected; reloading"
    conf_sig=$cur
    sleep 1  # debounce

    # `wg-user reload` configtests wireproxy.conf, then restarts the daemon.
    "$ROOT/wg-user" reload || log_warn "reload failed; see $LOG"
done
WATCHER_EOF
chmod 0755 "$WATCHER"

nohup "$WATCHER" > "$ROOT/log/watcher.log" 2>&1 &
watcher_pid=$!
echo "$watcher_pid" > "$ROOT/run/watcher.pid"
log_info "watcher started (pid $watcher_pid)"

# Hand off; service.sh returns immediately so the root framework isn't blocked.
exit 0
