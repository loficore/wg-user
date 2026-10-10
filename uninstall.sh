#!/system/bin/sh
# Cleanup on module uninstall.

set -e

# Locate the module root (same fallback strategy as install.sh).
MODDIR=""

case "$0" in
    /*)
        cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
        if [ -f "$cand/module.prop" ]; then MODDIR="$cand"; fi
        ;;
esac

if [ -z "$MODDIR" ]; then
    for cand in "${MODPATH:-}" "${MODDIR:-}"; do
        [ -n "$cand" ] || continue
        if [ -f "$cand/module.prop" ]; then MODDIR="$cand"; break; fi
    done
fi

if [ -z "$MODDIR" ]; then
    for fd in /proc/self/fd/*; do
        target="$(readlink "$fd" 2>/dev/null)" || continue
        case "$target" in
            */install.sh|*/customize.sh|*/uninstall.sh)
                cand="$(dirname "$target")"
                if [ -f "$cand/module.prop" ]; then MODDIR="$cand"; break; fi
                ;;
        esac
    done
fi

if [ -z "$MODDIR" ]; then
    cand="$(pwd)"
    [ -f "$cand/module.prop" ] && MODDIR="$cand"
fi

# Stop the daemons; do not abort on failure (they may not be running).
if [ -n "$MODDIR" ] && [ -x "$MODDIR/wg-user" ]; then
    "$MODDIR/wg-user" stop 2>/dev/null || true
fi
# Best-effort cleanup regardless of whether we found the root.
rm -f "${MODDIR:-.}/run/wireproxy.pid" "${MODDIR:-.}/run/wstunnel.pid" "${MODDIR:-.}/run/watcher.pid"
exit 0
