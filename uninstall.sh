#!/system/bin/sh
# Cleanup on module uninstall.

set -e

trace() { printf 'wg-user uninstall: %s\n' "$*" >&2; }

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
    resolve_fd_target() {
        fd_path="$1"
        target="$(readlink "$fd_path" 2>/dev/null)" || return 1
        case "$target" in
            /*) echo "$target"; return 0 ;;
            *)  abs_dir="$(CDPATH= cd -- "$(dirname -- "$target")" 2>/dev/null && pwd 2>/dev/null)" || return 1
                echo "$abs_dir/$(basename -- "$target")"
                return 0 ;;
        esac
    }
    for fd in /proc/self/fd/*; do
        target="$(resolve_fd_target "$fd")" || continue
        case "$target" in
            */install.sh|*/customize.sh|*/uninstall.sh)
                cand="$(dirname -- "$target")"
                if [ -f "$cand/module.prop" ]; then MODDIR="$cand"; break; fi
                ;;
        esac
    done
fi

if [ -z "$MODDIR" ]; then
    cand="$(pwd)"
    [ -f "$cand/module.prop" ] && MODDIR="$cand"
fi

trace "MODDIR=${MODDIR:-<not found>}"

if [ -n "$MODDIR" ] && [ -x "$MODDIR/wg-user" ]; then
    "$MODDIR/wg-user" stop 2>/dev/null || true
fi
rm -f "${MODDIR:-.}/run/wireproxy.pid" "${MODDIR:-.}/run/wstunnel.pid" "${MODDIR:-.}/run/watcher.pid"
exit 0
