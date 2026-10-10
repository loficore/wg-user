#!/system/bin/sh
# wg-user install-time hook (KernelSU / Magisk "modern" customize.sh path).
#
# IMPORTANT — do NOT add an install.sh to this module. Both KernelSU and
# Magisk treat the presence of install.sh as a "legacy module":
#
#   if is_legacy_script; then                     # install.sh exists
#       unzip -oj "$ZIPFILE" module.prop install.sh uninstall.sh \
#                'common/*' -d $TMPDIR            # ^ -j FLATTENS common/abi.sh
#       . $TMPDIR/install.sh                      # runs from /dev/tmp
#   else                                          # customize.sh exists
#       unzip -o "$ZIPFILE" -x 'META-INF/*' -d $MODPATH   # extracts EVERYTHING
#       . $MODPATH/customize.sh                   # runs from the module root
#   fi
#
# The legacy path never extracts bin/, etc/, or webroot/. This module needs
# all of those, so it uses the modern customize.sh path exclusively.
#
# In the modern path the following are guaranteed:
#   - MODPATH is exported by the installer (e.g. /data/adb/modules_update/<id>)
#   - every file from the zip is already at $MODPATH/<path>
#   - we are sourced, not exec'd, so errexit would propagate to the parent
#
# CRITICAL: do NOT use `set -e` here. Sourced scripts share the parent
# shell; enabling errexit would cause the parent installer to abort on
# the first non-zero command (e.g. its own `rmdir -p $MODPATH` against
# the non-empty module dir). Verified by simulation: case A (with set -e)
# exits 1 after a non-zero rmdir; case B (without) exits 0. Handle errors
# explicitly with explicit checks instead.

trace() { printf 'wg-user install: %s\n' "$*" >&2; }

# Resolve the module root. In the modern path the installer exports
# MODPATH. (When sourced, $0 is the parent shell's name — NOT the script
# path — so the $0 absolute-path branch is just a defensive fallback.)
MODPATH="${MODPATH:-}"
if [ -z "$MODPATH" ] || [ ! -f "$MODPATH/module.prop" ]; then
    case "$0" in
        /*)
            cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
            [ -f "$cand/module.prop" ] && MODPATH="$cand"
            ;;
    esac
fi
[ -n "$MODPATH" ] || MODPATH="$(pwd)"

if [ ! -f "$MODPATH/module.prop" ]; then
    trace "FATAL: cannot locate module root"
    trace "  MODPATH='${MODPATH:-}' \$0='$0' pwd='$(pwd)'"
    # `exit` from a sourced script terminates the parent installer; that
    # is the right behaviour for an unrecoverable error.
    exit 1
fi

cd "$MODPATH"
trace "module root: $MODPATH"

. "$MODPATH/common/util.sh"
. "$MODPATH/common/abi.sh"

# Wireproxy is required; failure is fatal.
if ! select_wireproxy_binary "$MODPATH" "$MODPATH/bin/wireproxy"; then
    log_err "failed to select wireproxy binary"
    exit 1
fi

# Wstunnel is optional (only needed if the user enables tunnel mode).
select_wstunnel_binary "$MODPATH" "$MODPATH/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"

log_info "Installed binaries for $(getprop ro.product.cpu.abi)"
