#!/usr/bin/env bash
# Cross-compile wireproxy for Android (linux) on 4 architectures.
# Produces bin/<arch>/wireproxy as a static binary.
# Requires: Go 1.21+, internet access on first run.
#
# Approach: a throwaway module that uses `go get` to populate go.mod with
# the wireproxy transitive deps, then `go build` for each target.
# (We cannot use `go install` for cross-compile because Go refuses to
# install cross-compiled binaries when GOBIN is set — including the
# GOPATH-derived default.)
set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"

WIREPROXY_VERSION="${WIREPROXY_VERSION:-v1.1.3}"
GOPROXY="${GOPROXY:-https://goproxy.cn,direct}"

# Go 1.21+ supports GOOS=linux GOARCH=arm64/arm/amd64/386.
TARGETS=(
    "arm64-v8a:arm64"
    "armeabi-v7a:arm"
    "x86_64:amd64"
    "x86:386"
)

OUT_DIR="$ROOT_DIR/bin"
mkdir -p "$OUT_DIR"

WORK_DIR="$(mktemp -d)"
trap 'rm -rf "$WORK_DIR"' EXIT
cd "$WORK_DIR"

cat > go.mod <<EOF
module wguser-build

go 1.21
EOF

echo "==> fetching wireproxy ${WIREPROXY_VERSION} via ${GOPROXY}..."
GOPROXY="$GOPROXY" go get "github.com/windtf/wireproxy/cmd/wireproxy@${WIREPROXY_VERSION}"

for entry in "${TARGETS[@]}"; do
    arch_dir="${entry%%:*}"
    goarch="${entry##*:}"
    target="$OUT_DIR/$arch_dir/wireproxy"
    mkdir -p "$OUT_DIR/$arch_dir"

    echo "==> building $arch_dir (GOARCH=$goarch)..."
    GOPROXY="$GOPROXY" GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 \
        go build -trimpath -ldflags '-s -w' \
        -o "$target" \
        github.com/windtf/wireproxy/cmd/wireproxy

    chmod +x "$target"
done

echo "==> done. binaries:"
find "$OUT_DIR" -type f -name wireproxy -exec ls -lh {} \;
