#!/system/bin/sh
# Magisk install-time hook (also used by KSU when install.sh exists).
# Selects the right wireproxy + wstunnel binaries for the device's ABI.

set -e

trace() { printf 'wg-user install: %s\n' "$*" >&2; }

# ---- Diagnostic dump: print everything we know about our context ----
trace "--- diagnostics ---"
trace "  \$0='$0'"
trace "  MODPATH='${MODPATH:-}'"
trace "  MODDIR='${MODDIR:-}'"
trace "  pwd='$(pwd)'"
trace "  /proc/self/exe='$(readlink /proc/self/exe 2>/dev/null)'"
ppid=$(awk '/^PPid:/{print $2}' /proc/self/status 2>/dev/null)
trace "  PPid=$ppid"
if [ -n "$ppid" ] && [ -r "/proc/$ppid/cmdline" ]; then
    ppid_cmdline=$(tr '\0' ' ' < "/proc/$ppid/cmdline" 2>/dev/null)
    trace "  /proc/$ppid/cmdline='$ppid_cmdline'"
fi
trace "  /proc/self/fd/ entries:"
for fd in /proc/self/fd/*; do
    fd_target="$(readlink "$fd" 2>/dev/null)"
    [ -n "$fd_target" ] && trace "    $fd -> $fd_target"
done
trace "---"

# Resolve fd target (handles both absolute and relative paths from readlink).
resolve_fd_target() {
    fd_path="$1"
    target="$(readlink "$fd_path" 2>/dev/null)" || return 1
    case "$target" in
        /*) printf '%s\n' "$target"; return 0 ;;
        *)  abs_dir="$(CDPATH= cd -- "$(dirname -- "$target")" 2>/dev/null && pwd 2>/dev/null)" || return 1
            printf '%s/%s\n' "$abs_dir" "$(basename -- "$target")"
            return 0 ;;
    esac
}

# is_script_name: matches install.sh, customize.sh, uninstall.sh
is_script_name() {
    case "$1" in
        */install.sh) return 0 ;;
        */customize.sh) return 0 ;;
        */uninstall.sh) return 0 ;;
    esac
    return 1
}

try_moddir() {
    cand="$1"
    [ -n "$cand" ] || return 1
    [ -f "$cand/module.prop" ] || { trace "  $cand: no module.prop"; return 1; }
    [ -f "$cand/common/util.sh" ] || { trace "  $cand: no common/util.sh"; return 1; }
    MODDIR="$cand"
    return 0
}

MODDIR=""

# 1. $0 if absolute.
case "$0" in
    /*) try_moddir "$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)" && trace "resolved via \$0" ;;
esac

# 2. Env vars.
[ -z "$MODDIR" ] && for cand in "${MODPATH:-}" "${MODDIR:-}"; do
    [ -n "$cand" ] || continue
    try_moddir "$cand" && trace "resolved via env $cand"
done

# 3. /proc/self/fd scan.
[ -z "$MODDIR" ] && for fd in /proc/self/fd/*; do
    target="$(resolve_fd_target "$fd")" || continue
    is_script_name "$target" || continue
    try_moddir "$(dirname -- "$target")" && trace "resolved via /proc/self/fd $fd"
done
[ -z "$MODDIR" ] && trace "/proc/self/fd scan: no match"

# 4. /proc/<ppid>/fd scan (parent KSU process may still hold the fd).
[ -z "$MODDIR" ] && [ -n "$ppid" ] && [ -d "/proc/$ppid/fd" ] && {
    trace "scanning /proc/$ppid/fd"
    for fd in "/proc/$ppid/fd"/*; do
        target="$(resolve_fd_target "$fd")" || continue
        is_script_name "$target" || continue
        try_moddir "$(dirname -- "$target")" && trace "resolved via /proc/$ppid/fd $fd"
    done
    [ -z "$MODDIR" ] && trace "/proc/$ppid/fd scan: no match"
}

# 5. Brute force: find module.prop under /data/adb/.
[ -z "$MODDIR" ] && trace "falling back to filesystem search under /data/adb"
[ -z "$MODDIR" ] && for cand in /data/adb/modules/wg-user /data/adb/modules_update/wg-user; do
    try_moddir "$cand" && trace "resolved via /data/adb search: $cand"
done

# 6. pwd (last resort).
[ -z "$MODDIR" ] && try_moddir "$(pwd)" && trace "resolved via pwd"

if [ -z "$MODDIR" ]; then
    trace "FATAL: cannot locate module root after all 6 strategies"
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
