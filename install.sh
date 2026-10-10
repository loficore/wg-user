#!/system/bin/sh
# Magisk install-time hook: select the right wireproxy + wstunnel
# binaries for the device's ABI. (KSU calls customize.sh; Magisk calls
# install.sh; the bodies are identical except for the env-var name.)
set -e

MODDIR="${MODDIR:-${0%/*}}"
. "$MODDIR/common/util.sh"
. "$MODDIR/common/abi.sh"
select_wireproxy_binary "$MODDIR" "$MODDIR/bin/wireproxy"
select_wstunnel_binary "$MODDIR" "$MODDIR/bin/wstunnel" 2>/dev/null \
    || log_warn "wstunnel not bundled; tunnel mode will be unavailable"
log "Installed binaries for $(getprop ro.product.cpu.abi)"
