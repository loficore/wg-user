#!/system/bin/sh
# ABI detection and wg-user binary selection.
# Source: `. common/abi.sh` from customize.sh / service.sh.
# POSIX sh — Android's /system/bin/sh is mksh, not bash.

# get_device_arch — echo one of: arm64-v8a, armeabi-v7a, x86_64, x86
get_device_arch() {
    abi="$(getprop ro.product.cpu.abi 2>/dev/null || echo unknown)"
    case "$abi" in
        arm64-v8a)  echo arm64-v8a  ;;
        arm64-v8a*) echo arm64-v8a  ;;
        armeabi-v7a)  echo armeabi-v7a  ;;
        armeabi-v7a*) echo armeabi-v7a  ;;
        x86_64)   echo x86_64   ;;
        x86_64*)  echo x86_64   ;;
        x86)      echo x86      ;;
        x86*)     echo x86      ;;
        armv7*|armv8*) echo armeabi-v7a ;;  # rare fallbacks
        *) echo "unsupported:$abi" >&2; return 1 ;;
    esac
}

# select_binary <src_dir> <dst_path>
#   src_dir: $MODPATH (contains bin/<arch>/wg-user)
#   dst_path: where the running binary should live (e.g. $MODPATH/bin/wg-user)
select_binary() {
    src_dir="$1"
    dst_path="$2"
    arch="$(get_device_arch)" || {
        echo "wg-user: cannot detect device ABI" >&2
        return 1
    }
    src_path="$src_dir/bin/$arch/wg-user"
    if [ ! -f "$src_path" ]; then
        echo "wg-user: no wg-user binary for arch '$arch' at $src_path" >&2
        return 1
    fi
    mkdir -p "$(dirname "$dst_path")"
    cp -f "$src_path" "$dst_path"
    chmod 0755 "$dst_path"
    echo "wg-user: binary: $arch"
}
