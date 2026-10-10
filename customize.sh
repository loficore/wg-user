#!/system/bin/sh
# KernelSU install-time hook (also runs when install.sh is absent).
# Selects the right wireproxy + wstunnel binaries for the device's ABI.

set -e

# ---------------------------------------------------------------------------
# Locate the module root. See install.sh for the full reasoning. The four
# fallback strategies are: $0 if absolute, $MODPATH/$MODDIR, /proc/self/fd
# scan, and finally $PWD.
# ---------------------------------------------------------------------------

MODPATH=""

# 1. Try $0 if it is an absolute path.
case "$0" in
    /*)
        cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODPATH="$cand"
        fi
        ;;
esac

# 2. Try the well-known env vars.
if [ -z "$MODPATH" ]; then
    for cand in "${MODPATH:-}" "${MODDIR:-}"; do
        [ -n "$cand" ] || continue
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODPATH="$cand"
            break
        fi
    done
fi

# 3. /proc/self/fd scan.
if [ -z "$MODPATH" ]; then
    for fd in /proc/self/fd/*; do
        target="$(readlink "$fd" 2>/dev/null)" || continue
        case "$target" in
            */install.sh|*/customize.sh|*/uninstall.sh)
                cand="$(dirname "$target")"
                if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
                    MODPATH="$cand"
                    break
                fi
                ;;
        esac
    done
fi

# 4. $PWD.
if [ -z "$MODPATH" ]; then
    cand="$(pwd)"
    if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
        MODPATH="$cand"
    fi
fi

if [ -z "$MODPATH" ]; then
    echo "wg-user: customize.sh: cannot locate module root" >&2
    echo "  Tried \$0='$0' MODPATH='${MODPATH:-}' MODDIR='${MODDIR:-}' pwd='$(pwd)'" >&2
    echo "  /proc/self/fd scan did not find a module.prop sibling." >&2
    exit 1
fi

cd "$MODPATH"

. "$MODPATH/common/util.sh"
. "$MODPATH/common/abi.sh"
select_wireproxy_binary "$MODPATH" "$MODPATH/bin/wireproxy"
# wstunnel is optional; if the user did not build it (no obfuscation
# layer), don't fail. Missing binary is logged but tolerated.
select_wstunnel_binary "$MODPATH" "$MODPATH/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log_info "Installed binaries for $(getprop ro.product.cpu.abi)"
