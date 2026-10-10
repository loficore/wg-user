#!/system/bin/sh
# KernelSU install-time hook: select the right wireproxy + wstunnel
# binaries for the device's ABI.
set -e

# Locate the module root.
#
# KSU and Magisk export MODPATH (and/or MODDIR) pointing at the module dir
# during install. Do NOT use ${0%/*}: some installers source this script
# (`. ./customize.sh`), in which case $0 is the shell name ("sh") and the
# glob strips nothing. Fall back to PWD, which the installer sets to the
# module dir before running us.
MODPATH="${MODPATH:-${MODDIR:-}}"
if [ -z "$MODPATH" ] || [ ! -f "$MODPATH/module.prop" ]; then
    MODPATH="$(pwd)"
fi
if [ ! -f "$MODPATH/module.prop" ]; then
    echo "wg-user: customize.sh: cannot locate module root" >&2
    echo "  MODPATH='${MODPATH:-}' MODDIR='${MODDIR:-}' pwd='$(pwd)'" >&2
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
