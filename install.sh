#!/system/bin/sh
# Magisk install-time hook (also used by KSU when install.sh exists).
# Selects the right wireproxy + wstunnel binaries for the device's ABI.

set -e

# ---------------------------------------------------------------------------
# Verbose stderr helper that works even BEFORE common/util.sh is sourced.
# We use this to trace which fallback resolved the module root, so the KSU
# install log tells us exactly what happened.
# ---------------------------------------------------------------------------
trace() { printf 'wg-user install: %s\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# Locate the module root. The root manager (KSU / Magisk) can invoke this
# script in several ways:
#
#   1. sh -c '. /abs/path/install.sh'    (sourced, $0='sh')
#   2. sh /abs/path/install.sh           (exec'd, $0=absolute)
#   3. . /abs/path/install.sh            (sourced, $0 may be anything)
#   4. . install.sh                      (sourced, $0 inherited, pwd has the file)
#
# $0 is unreliable in case (1)/(3)/(4); in case (1) the script's actual
# on-disk path is available via /proc/self/fd/<N> (the shell keeps the
# source file open as an fd). In case (2), $0 itself is the answer.
# Env vars (MODPATH, MODDIR) cover cases where the manager exports them.
# ---------------------------------------------------------------------------

MODDIR=""

# Resolve a possibly-relative path under /proc/self/fd.
# readlink on Android's mksh may not follow the symlink; fall back to
# manual resolution via cd + pwd if readlink gives us a relative path.
resolve_fd_target() {
    fd_path="$1"
    target="$(readlink "$fd_path" 2>/dev/null)" || return 1
    case "$target" in
        /*) echo "$target"; return 0 ;;
        *)  # relative — resolve against current dir
            dir_part="$(dirname -- "$target")"
            base_part="$(basename -- "$target")"
            abs_dir="$(CDPATH= cd -- "$dir_part" 2>/dev/null && pwd 2>/dev/null)" || return 1
            echo "$abs_dir/$base_part"
            return 0
            ;;
    esac
}

# 1. $0 if absolute.
case "$0" in
    /*)
        cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODDIR="$cand"
            trace "resolved via \$0: $cand"
        else
            trace "\$0 absolute but $cand lacks module.prop/common/util.sh"
        fi
        ;;
    *)
        trace "\$0 not absolute ($0); trying env"
        ;;
esac

# 2. Well-known env vars.
if [ -z "$MODDIR" ]; then
    for cand in "${MODPATH:-}" "${MODDIR:-}"; do
        [ -n "$cand" ] || continue
        if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
            MODDIR="$cand"
            trace "resolved via env: $cand"
            break
        else
            trace "env $cand: missing module.prop or common/util.sh"
        fi
    done
fi

# 3. /proc/self/fd scan — the killer feature for sourced scripts.
#    The shell keeps the source file open as an fd. /proc/self/fd lists
#    all open fds; readlink on each gives the on-disk path of the file.
if [ -z "$MODDIR" ]; then
    trace "falling back to /proc/self/fd scan"
    found_via_fd=0
    for fd in /proc/self/fd/*; do
        target="$(resolve_fd_target "$fd")" || continue
        case "$target" in
            */install.sh|*/customize.sh|*/uninstall.sh)
                cand="$(dirname -- "$target")"
                if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
                    MODDIR="$cand"
                    found_via_fd=1
                    trace "resolved via /proc/self/fd scan: $cand (from $target)"
                    break
                fi
                ;;
        esac
    done
    [ "$found_via_fd" -eq 0 ] && trace "/proc/self/fd scan: no match"
fi

# 4. Current working directory (only correct when manager did
#    `cd module_dir; . install.sh` — fragile).
if [ -z "$MODDIR" ]; then
    cand="$(pwd)"
    if [ -f "$cand/module.prop" ] && [ -f "$cand/common/util.sh" ]; then
        MODDIR="$cand"
        trace "resolved via pwd: $cand"
    else
        trace "pwd $cand: missing module.prop or common/util.sh"
    fi
fi

if [ -z "$MODDIR" ]; then
    trace "FATAL: cannot locate module root"
    trace "  \$0='$0'  MODPATH='${MODPATH:-}'  MODDIR='${MODDIR:-}'  pwd='$(pwd)'"
    trace "  /proc/self/fd scan did not find a module.prop sibling."
    exit 1
fi

cd "$MODDIR"
trace "cd-ed into $MODDIR"

. "$MODDIR/common/util.sh"
. "$MODDIR/common/abi.sh"
select_wireproxy_binary "$MODDIR" "$MODDIR/bin/wireproxy"
select_wstunnel_binary "$MODDIR" "$MODDIR/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log_info "Installed binaries for $(getprop ro.product.cpu.abi)"
