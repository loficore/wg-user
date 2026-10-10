#!/system/bin/sh
# Cleanup on module uninstall
# Stops the daemon, removes PID file and the binary symlink.
MODDIR="${0%/*}"
"$MODDIR/wg-user" stop 2>/dev/null || true
rm -f "$MODDIR/run/wireproxy.pid"
rm -f "$MODDIR/run/wstunnel.pid"
exit 0
