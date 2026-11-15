#!/usr/bin/env bash
# Cross-compile the wg-user binary for Android (linux) on 4 architectures.
# Produces bin/<arch>/wg-user as a static binary.
# Requires: Go 1.21+, internet access on first run.
#
# Set ARCH=<arch dir> (for example ARCH=x86_64) to build a single target; the
# Makefile's pattern rule does that. Unset, it builds all four.
set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"

GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"
VERSION="$(sed -n 's/^version=//p' "$ROOT_DIR/module.prop")"
[ -n "$VERSION" ] || { echo "no version= line in $ROOT_DIR/module.prop" >&2; exit 1; }

# Go 1.21+ supports GOOS=linux GOARCH=arm64/arm/amd64/386.
TARGETS=(
    "arm64-v8a:arm64"
    "armeabi-v7a:arm"
    "x86_64:amd64"
    "x86:386"
)

OUT_DIR="$ROOT_DIR/bin"
mkdir -p "$OUT_DIR"

cd "$ROOT_DIR"
echo "==> downloading modules via ${GOPROXY}..."
GOPROXY="$GOPROXY" go mod download

built=()
for entry in "${TARGETS[@]}"; do
    arch_dir="${entry%%:*}"
    goarch="${entry##*:}"

    if [ -n "${ARCH:-}" ] && [ "$ARCH" != "$arch_dir" ]; then
        continue
    fi

    target="$OUT_DIR/$arch_dir/wg-user"
    mkdir -p "$OUT_DIR/$arch_dir"

    echo "==> building $arch_dir (GOARCH=$goarch)..."
    GOPROXY="$GOPROXY" GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 \
        go build -trimpath -ldflags "-s -w -X main.version=$VERSION" \
        -o "$target" \
        ./cmd/wg-user

    chmod +x "$target"
    built+=("$target")
done

if [ ${#built[@]} -eq 0 ]; then
    echo "unknown ARCH=${ARCH:-} (want: arm64-v8a armeabi-v7a x86_64 x86)" >&2
    exit 1
fi

echo "==> done. binaries:"
ls -lh "${built[@]}"
