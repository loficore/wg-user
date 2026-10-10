#!/system/bin/sh
# Magisk install-time hook (also used by KSU when install.sh exists).
# Selects the right wireproxy + wstunnel binaries for the device's ABI.

set -e

# ---------------------------------------------------------------------------
# Locate the module root. This is the fragile part of install scripts: the
# root manager (KSU / Magisk) invokes us in one of several ways:
#   1. sh -c '. /path/to/install.sh'           (sourced, $0='sh')
#   2. sh /path/to/install.sh                  (exec'd, $0=absolute path)
#   3. . /path/to/install.sh                   (sourced, $0 may be anything)
# In case (1), $0 is useless. In case (2), $0 is the answer. In all cases,
# the script's own file is one of the open fds (readlink /proc/self/fd/*).
# We try multiple sources in order and pick the first that has module.prop
# AND common/util.sh.
# ---------------------------------------------------------------------------

MODDIR=""

# 1. Try $0 if it is an absolute path.
case "$0" in
    /*)
        cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODDIR="$cand"
        fi
        ;;
esac

# 2. Try the well-known env vars the root manager may have set.
if [ -z "$MODDIR" ]; then
    for cand in "${MODPATH:-}" "${MODDIR:-}"; do
        [ -n "$cand" ] || continue
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODDIR="$cand"
            break
        fi
    done
fi

# 3. Last resort: scan /proc/self/fd for the script's own file.
#    When sourced, the shell keeps the source file open. /proc/self/fd
#    gives us a symlink to its path on disk. (Linux/Android-specific,
#    but both environments have /proc.)
if [ -z "$MODDIR" ]; then
    for fd in /proc/self/fd/*; do
        target="$(readlink "$fd" 2>/dev/null)" || continue
        case "$target" in
            */install.sh|*/customize.sh|*/uninstall.sh)
                cand="$(dirname "$target")"
                if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
                    MODDIR="$cand"
                    break
                fi
                ;;
        esac
    done
fi

# 4. Absolute last resort: current working directory (rarely correct when
#    sourced, but if the manager did `cd module_dir; . install.sh` it
#    is correct).
if [ -z "$MODDIR" ]; then
    cand="$(pwd)"
    if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
        MODDIR="$cand"
    fi
fi

if [ -z "$MODDIR" ]; then
    echo "wg-user: install.sh: cannot locate module root" >&2
    echo "  Tried \$0='$0' MODPATH='${MODPATH:-}' MODDIR='${MODDIR:-}' pwd='$(pwd)'" >&2
    echo "  /proc/self/fd scan did not find a module.prop sibling." >&2
    exit 1
fi

cd "$MODDIR"

. "$MODDIR/common/util.sh"
. "$MODDIR/common/abi.sh"
select_wireproxy_binary "$MODDIR" "$MODDIR/bin/wireproxy"
select_wstunnel_binary "$MODDIR" "$MODDIR/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log_info "Installed binaries for $(getprop ro.product.cpu.abi)"
