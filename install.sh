#!/system/bin/sh
# Magisk install-time hook: select the right wireproxy + wstunnel
# binaries for the device's ABI. (KSU calls install.sh; Magisk calls
# install.sh; the bodies are identical apart from the module-root logic.)
set -e

# Locate the module root.
#
# KSU and Magisk export MODPATH (and/or MODDIR) pointing at the module dir
# during install. Do NOT use ${0%/*}: some installers source this script
# (`. ./install.sh`), in which case $0 is the shell name ("sh") and the
# glob strips nothing. Fall back to PWD, which the installer sets to the
# module dir before running us.
MODDIR="${MODPATH:-${MODDIR:-}}"
if [ -z "$MODDIR" ] || [ ! -f "$MODDIR/module.prop" ]; then
    MODDIR="$(pwd)"
fi
if [ ! -f "$MODDIR/module.prop" ]; then
    echo "wg-user: install.sh: cannot locate module root" >&2
    echo "  MODPATH='${MODPATH:-}' MODDIR='${MODDIR:-}' pwd='$(pwd)'" >&2
    exit 1
fi
cd "$MODDIR"

. "$MODDIR/common/util.sh"
. "$MODDIR/common/abi.sh"
select_wireproxy_binary "$MODDIR" "$MODDIR/bin/wireproxy"
select_wstunnel_binary "$MODDIR" "$MODDIR/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log_info "Installed binaries for $(getprop ro.product.cpu.abi)"
