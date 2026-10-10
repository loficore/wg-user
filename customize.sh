#!/system/bin/sh
# wg-user install-time hook (KernelSU / Magisk, "modern" customize.sh path).
#
# IMPORTANT — do NOT add an install.sh to this module. Both KernelSU and
# Magisk treat the PRESENCE of install.sh as a "legacy module":
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
#   - $0 is the absolute path to this script  ($MODPATH/customize.sh)
#   - $MODPATH is the module root              (/data/adb/modules_update/<id>)
#   - every file from the zip is already at    $MODPATH/<path>
#   - we are sourced, not exec'd, so `set -e` still applies (fine).
set -e

trace() { printf 'wg-user install: %s\n' "$*" >&2; }

# Resolve the module root. In the modern path $0 is already absolute; prefer
# it (a script can never lie about its own path when executed with one).
MODPATH="${MODPATH:-}"
case "$0" in
    /*)
        cand="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
        [ -f "$cand/module.prop" ] && MODPATH="$cand"
        ;;
esac
# Fall back to PWD — the modern installer cd's nowhere, but some future
# variant might.
if [ -z "$MODPATH" ] || [ ! -f "$MODPATH/module.prop" ]; then
    MODPATH="$(pwd)"
fi
if [ ! -f "$MODPATH/module.prop" ]; then
    trace "FATAL: cannot locate module root"
    trace "  \$0='$0' MODPATH='${MODPATH:-}' pwd='$(pwd)'"
    exit 1
fi

cd "$MODPATH"
trace "module root: $MODPATH"

. "$MODPATH/common/util.sh"
. "$MODPATH/common/abi.sh"

select_wireproxy_binary "$MODPATH" "$MODPATH/bin/wireproxy"
select_wstunnel_binary "$MODPATH" "$MODPATH/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log_info "Installed binaries for $(getprop ro.product.cpu.abi)"
