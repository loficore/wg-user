#!/usr/bin/env bash
# Cross-compile wstunnel (Rust) for Android (4 architectures).
# Produces bin/<arch>/wstunnel.
# Requires: rustup + cargo, Android NDK r26+, internet (first run).
#
# Approach: clone the upstream wstunnel repo at $WSTUNNEL_VERSION, then
# `cargo build` for each of the 4 Android targets with the NDK clang
# wrappers (in scripts/cargo-wrappers/) as the linker/ar. This avoids
# the complexity of a dummy wrapper package and is exactly how upstream
# releases are built.
set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"

WSTUNNEL_VERSION="${WSTUNNEL_VERSION:-v11.0.0}"
NDK="${ANDROID_NDK_HOME:-$HOME/Android/Sdk/ndk/26.3.11579264}"
WRAPPERS="$ROOT_DIR/scripts/cargo-wrappers"

if [ ! -x "$WRAPPERS/aarch64-linux-android-clang" ]; then
    echo "FATAL: $WRAPPERS/aarch64-linux-android-clang not executable" >&2
    echo "Run: chmod +x scripts/cargo-wrappers/*  (or re-run this script via 'make build-rust')" >&2
    exit 1
fi
if [ ! -d "$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin" ]; then
    echo "FATAL: NDK not found at $NDK. Set ANDROID_NDK_HOME." >&2
    exit 1
fi

# (rust_target, arch_dir)
TARGETS=(
    "aarch64-linux-android:arm64-v8a"
    "armv7-linux-androideabi:armeabi-v7a"
    "x86_64-linux-android:x86_64"
    "i686-linux-android:x86"
)

# Install missing Android rust targets.
NEED=()
for entry in "${TARGETS[@]}"; do
    t="${entry%%:*}"
    rustup target list --installed | grep -qx "$t" || NEED+=("$t")
done
if [ ${#NEED[@]} -gt 0 ]; then
    echo "==> installing rust targets: ${NEED[*]}"
    rustup target add "${NEED[@]}"
fi

# Clone the wstunnel source.
SRC="$(mktemp -d)"
trap 'rm -rf "$SRC"' EXIT
echo "==> cloning wstunnel ${WSTUNNEL_VERSION}..."
git clone --depth 1 --branch "${WSTUNNEL_VERSION}" \
    https://github.com/erebe/wstunnel "$SRC/wstunnel"
cd "$SRC/wstunnel"

# Generate a .cargo/config.toml that points each target at our wrappers.
# Two things need configuration:
#   1. The Rust linker (read from [target.X] linker = ...)
#   2. The C compiler used by cc-rs (read from CC_<target> env via [env])
# Without (2), crates like aws-lc-sys fail to find "aarch64-linux-android-clang".
mkdir -p .cargo
{
    echo "[net]"
    echo "git-fetch-with-cli = true"
    for entry in "${TARGETS[@]}"; do
        t="${entry%%:*}"
        # env key form: dashes -> underscores
        env_key="CC_$(echo "$t" | tr '-' '_')"
        echo ""
        echo "[target.${t}]"
        echo "linker = \"${WRAPPERS}/${t}-clang\""
        echo "ar = \"${WRAPPERS}/${t}-ar\""
    done
    echo ""
    echo "[env]"
    for entry in "${TARGETS[@]}"; do
        t="${entry%%:*}"
        env_key="CC_$(echo "$t" | tr '-' '_')"
        env_key_cxx="CXX_$(echo "$t" | tr '-' '_')"
        env_key_ar="AR_$(echo "$t" | tr '-' '_')"
        echo "${env_key} = \"${WRAPPERS}/${t}-clang\""
        echo "${env_key_cxx} = \"${WRAPPERS}/${t}-clang++\""
        echo "${env_key_ar} = \"${WRAPPERS}/${t}-ar\""
    done
} > .cargo/config.toml

OUT="$ROOT_DIR/bin"
for entry in "${TARGETS[@]}"; do
    t="${entry%%:*}"
    ad="$(echo "$entry" | cut -d: -f2)"
    echo "==> building wstunnel for $ad ($t)..."
    cargo build --release --target "$t" -p wstunnel-cli 2>&1 \
        | sed "s|^|  [$ad] |"
    src="$SRC/wstunnel/target/$t/release/wstunnel"
    dst="$OUT/$ad/wstunnel"
    mkdir -p "$OUT/$ad"
    cp "$src" "$dst"
    chmod +x "$dst"
done

echo "==> done."
find "$OUT" -type f -name wstunnel -exec ls -lh {} \;
