#!/system/bin/sh
# KernelSU install-time hook: select the right wireproxy + wstunnel
# binaries for the device's ABI.
set -e

MODPATH="${MODPATH:-${0%/*}}"
. "$MODPATH/common/util.sh"
. "$MODPATH/common/abi.sh"
select_wireproxy_binary "$MODPATH" "$MODPATH/bin/wireproxy"
# wstunnel is optional; if the user did not build it (no obfuscation
# layer), don't fail. Missing binary is logged but tolerated.
select_wstunnel_binary "$MODPATH" "$MODPATH/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log "Installed binaries for $(getprop ro.product.cpu.abi)"
