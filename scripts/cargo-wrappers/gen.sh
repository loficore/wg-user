#!/usr/bin/env bash
# Regenerate the 12 NDK clang wrappers under scripts/cargo-wrappers/.
# The wrappers are committed; this script is for re-generation when the
# NDK path changes or the wrapper set needs to grow.
#
# Usage: bash scripts/cargo-wrappers/gen.sh
set -euo pipefail

NDK_DEFAULT="$HOME/Android/Sdk/ndk/26.3.11579264"
NDK_BIN_DIR_REL="toolchains/llvm/prebuilt/linux-x86_64/bin"

# rust-target : NDK-binary-prefix (with -26 suffix for clang family)
gen() {
    local rtgt="$1"
    local ndkpfx="$2"
    local name="$3"
    local ndkbin="$4"
    local f="$(dirname "$0")/${rtgt}-${name}"
    cat > "$f" <<EOF
#!/bin/sh
# NDK ${name} wrapper for ${rtgt}
# Override by setting NDK env var before running cargo.
NDK="\${NDK:-$NDK_DEFAULT}"
exec "\$NDK/${NDK_BIN_DIR_REL}/${ndkbin}" "\$@"
EOF
    chmod +x "$f"
    echo "wrote $f"
}

gen aarch64-linux-android    aarch64-linux-android26        clang   aarch64-linux-android26-clang
gen aarch64-linux-android    aarch64-linux-android26        clang++ aarch64-linux-android26-clang++
gen aarch64-linux-android    aarch64-linux-android26        ar      llvm-ar

gen armv7-linux-androideabi  armv7a-linux-androideabi26     clang   armv7a-linux-androideabi26-clang
gen armv7-linux-androideabi  armv7a-linux-androideabi26     clang++ armv7a-linux-androideabi26-clang++
gen armv7-linux-androideabi  armv7a-linux-androideabi26     ar      llvm-ar

gen x86_64-linux-android     x86_64-linux-android26        clang   x86_64-linux-android26-clang
gen x86_64-linux-android     x86_64-linux-android26        clang++ x86_64-linux-android26-clang++
gen x86_64-linux-android     x86_64-linux-android26        ar      llvm-ar

gen i686-linux-android       i686-linux-android26          clang   i686-linux-android26-clang
gen i686-linux-android       i686-linux-android26          clang++ i686-linux-android26-clang++
gen i686-linux-android       i686-linux-android26          ar      llvm-ar
