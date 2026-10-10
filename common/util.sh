#!/system/bin/sh
# Shared shell utilities for wg-user.
# Source: `. common/util.sh` from any script in the module.

# log_ts — echo an ISO-8601-ish timestamp
log_ts() {
    # Busybox date is the only date we have on most Android; fall back to nothing.
    date '+%Y-%m-%dT%H:%M:%S%z' 2>/dev/null || echo "0000-00-00T00:00:00+0000"
}

log_info() { printf '%s INFO  wg-user: %s\n' "$(log_ts)" "$*" >&2; }
log_warn() { printf '%s WARN  wg-user: %s\n' "$(log_ts)" "$*" >&2; }
log_err()  { printf '%s ERROR wg-user: %s\n' "$(log_ts)" "$*" >&2; }

# module_dir — echo the absolute directory of the module, regardless of how
# the calling script was invoked.
module_dir() {
    # ${0%/*} is the directory of the calling script. We resolve "." to abs.
    script_path="$0"
    case "$script_path" in
        /*) ;;
        *)  script_path="$(pwd)/$script_path" ;;
    esac
    dir="$(dirname "$script_path")"
    case "$dir" in
        .)  echo "$(pwd)" ;;
        *)  echo "$dir" ;;
    esac
}

# is_running <pidfile> — returns 0 if pidfile is present and the process is alive.
is_running() {
    pidfile="$1"
    [ -f "$pidfile" ] || return 1
    pid="$(cat "$pidfile" 2>/dev/null)" || return 1
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null
}

# root_dir — return the module's runtime root (parent of bin/, common/, etc.)
root_dir() {
    # $0 might be .../wg-user or .../bin/wireproxy; we want the module root.
    _myself="$0"
    _dir=$(CDPATH= cd -- "$(dirname -- "$_myself")" && pwd)
    # If called from .../bin/<something>, go up two levels.
    case "$_dir" in
        */bin) echo "${_dir%/bin}" ;;
        *)     echo "$_dir" ;;
    esac
}
