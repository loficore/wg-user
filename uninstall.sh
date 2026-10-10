#!/system/bin/sh
# wg-user uninstall hook (KernelSU / Magisk).
#
# KernelSU runs this via:
#   busybox sh /data/adb/modules/<id>/uninstall.sh
# with CWD set to the script's parent (the module root). Magisk behaves
# similarly. So $0 is the absolute script path and CWD is the module root;
# either source is reliable.
set -e

trace() { printf 'wg-user uninstall: %s\n' "$*" >&2; }

MODDIR=""
case "$0" in
    /*)
        cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
        [ -f "$cand/module.prop" ] && MODDIR="$cand"
        ;;
esac
if [ -z "$MODDIR" ]; then
    for cand in "${MODPATH:-}" "${MODDIR:-}" "$(pwd)"; do
        [ -n "$cand" ] || continue
        if [ -f "$cand/module.prop" ]; then MODDIR="$cand"; break; fi
    done
fi

trace "module root: ${MODDIR:-<not found>}"

if [ -n "$MODDIR" ] && [ -x "$MODDIR/wg-user" ]; then
    "$MODDIR/wg-user" stop 2>/dev/null || true
fi
rm -f "${MODDIR:-.}/run/wireproxy.pid" "${MODDIR:-.}/run/wstunnel.pid" "${MODDIR:-.}/run/watcher.pid"
exit 0
