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

# Sanity: ensure the binaries are in place. customize.sh / install.sh
# should have run; if not (e.g. partial install), pick them now.
if [ ! -x "$ROOT/bin/wireproxy" ]; then
    log_warn "wireproxy missing; running ABI selection"
    select_wireproxy_binary "$ROOT" "$ROOT/bin/wireproxy" || {
        log_err "no wireproxy binary available; service.sh exiting"
        exit 1
    }
fi
# wstunnel is optional — only needed if tunnel.conf enables it.
if [ ! -x "$ROOT/bin/wstunnel" ]; then
    log_warn "wstunnel not bundled; tunnel mode will be unavailable"
fi

# Start the daemon (this internally starts wstunnel first if enabled,
# then wireproxy — see wg-user).
"$ROOT/wg-user" start

# Start the reload watcher in the background. Watches BOTH wireproxy.conf
# AND tunnel.conf; reload handles the right restart order.
WATCHER="$ROOT/wg-user-reload"
cat > "$WATCHER" <<'WATCHER_EOF'
#!/system/bin/sh
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
. "$ROOT/common/util.sh"

WPCONF="$ROOT/etc/wireproxy.conf"
TNCONF="$ROOT/etc/tunnel.conf"
WPLOG="$ROOT/log/wireproxy.log"
TNLOG="$ROOT/log/wstunnel.log"
INTERVAL="${WGU_RELOAD_INTERVAL:-5}"

# Initialize mtimes.
wp_mtime=0
tn_mtime=0
[ -f "$WPCONF" ] && wp_mtime=$(stat -c %Y "$WPCONF" 2>/dev/null || stat -f %m "$WPCONF" 2>/dev/null || echo 0)
[ -f "$TNCONF" ] && tn_mtime=$(stat -c %Y "$TNCONF" 2>/dev/null || stat -f %m "$TNCONF" 2>/dev/null || echo 0)

log_info "reload-watcher started (interval ${INTERVAL}s); watching $WPCONF and $TNCONF"

while :; do
    sleep "$INTERVAL" 2>/dev/null || sleep 5

    cur_wp=0; cur_tn=0
    [ -f "$WPCONF" ] && cur_wp=$(stat -c %Y "$WPCONF" 2>/dev/null || stat -f %m "$WPCONF" 2>/dev/null || echo 0)
    [ -f "$TNCONF" ] && cur_tn=$(stat -c %Y "$TNCONF" 2>/dev/null || stat -f %m "$TNCONF" 2>/dev/null || echo 0)

    wp_changed=0; tn_changed=0
    [ "$cur_wp" != "$wp_mtime" ] && [ "$cur_wp" != "0" ] && wp_changed=1
    [ "$cur_tn" != "$tn_mtime" ] && [ "$cur_tn" != "0" ] && tn_changed=1

    if [ $wp_changed -eq 0 ] && [ $tn_changed -eq 0 ]; then
        continue
    fi

    log_info "config change: wireproxy=$wp_changed tunnel=$tn_changed"
    wp_mtime=$cur_wp
    tn_mtime=$cur_tn
    sleep 1  # debounce

    # `wg-user reload` configtests wireproxy.conf, then restarts both
    # in the right order (tunnel first if enabled, then wireproxy).
    "$ROOT/wg-user" reload || log_warn "reload failed; see $WPLOG or $TNLOG"
done
WATCHER_EOF
chmod 0755 "$WATCHER"

nohup "$WATCHER" > "$ROOT/log/watcher.log" 2>&1 &
watcher_pid=$!
echo "$watcher_pid" > "$ROOT/run/watcher.pid"
log_info "watcher started (pid $watcher_pid)"

# Hand off; service.sh returns immediately so the root framework isn't blocked.
exit 0
