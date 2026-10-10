#!/system/bin/sh
# Cleanup on module uninstall.
# Stops the daemons and removes PID files.
set -e

# Locate the module root. KSU/Magisk export MODPATH/MODDIR; do not rely on
# ${0%/*} because the installer may source us (then $0 is "sh").
MODDIR="${MODPATH:-${MODDIR:-}}"
if [ -z "$MODDIR" ] || [ ! -f "$MODDIR/module.prop" ]; then
    MODDIR="$(pwd)"
fi

if [ -x "$MODDIR/wg-user" ]; then
    "$MODDIR/wg-user" stop 2>/dev/null || true
fi
rm -f "$MODDIR/run/wireproxy.pid"
rm -f "$MODDIR/run/wstunnel.pid"
rm -f "$MODDIR/run/watcher.pid"
exit 0
