#!/system/bin/sh
# KernelSU install-time hook (also runs when install.sh is absent).
# Selects the right wireproxy + wstunnel binaries for the device's ABI.

set -e

trace() { printf 'wg-user install: %s\n' "$*" >&2; }

# Same 4-tier detection as install.sh. See that file for the full rationale.
MODPATH=""

case "$0" in
    /*)
        cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODPATH="$cand"; trace "resolved via \$0: $cand"
        fi
        ;;
esac

if [ -z "$MODPATH" ]; then
    for cand in "${MODPATH:-}" "${MODDIR:-}"; do
        [ -n "$cand" ] || continue
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODPATH="$cand"; trace "resolved via env: $cand"; break
        fi
    done
fi

if [ -z "$MODPATH" ]; then
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
                if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
                    MODPATH="$cand"
                    trace "resolved via /proc/self/fd scan: $cand (from $target)"
                    break
                fi
                ;;
        esac
    done
fi

if [ -z "$MODPATH" ]; then
    cand="$(pwd)"
    if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
        MODPATH="$cand"; trace "resolved via pwd: $cand"
    fi
fi

if [ -z "$MODPATH" ]; then
    trace "FATAL: cannot locate module root"
    trace "  \$0='$0'  MODPATH='${MODPATH:-}'  MODDIR='${MODDIR:-}'  pwd='$(pwd)'"
    exit 1
fi

cd "$MODPATH"
trace "cd-ed into $MODPATH"

. "$MODPATH/common/util.sh"
. "$MODPATH/common/abi.sh"
select_wireproxy_binary "$MODPATH" "$MODPATH/bin/wireproxy"
select_wstunnel_binary "$MODPATH" "$MODPATH/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log_info "Installed binaries for $(getprop ro.product.cpu.abi)"
