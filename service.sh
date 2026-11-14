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

# mtime <file> — epoch seconds; GNU stat, then BSD stat, then 0.
mtime() {
    stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || echo 0
}

conf_mtime=0
if [ -f "$CONF" ]; then conf_mtime=$(mtime "$CONF"); fi

log_info "reload-watcher started (interval ${INTERVAL}s); watching $CONF"

while :; do
    sleep "$INTERVAL" 2>/dev/null || sleep 5

    cur=0
    if [ -f "$CONF" ]; then cur=$(mtime "$CONF"); fi

    [ "$cur" != "$conf_mtime" ] || continue
    [ "$cur" != "0" ] || continue

    log_info "config change detected; reloading"
    conf_mtime=$cur
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
