#!/bin/sh
# test/shell/build_test.sh — every arch produced a runnable wg-user binary.
#  1) the four cross-compiled binaries exist and are executable
#  2) the amd64 build (which runs on the build host) reports the module version
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)

for a in arm64-v8a armeabi-v7a x86_64 x86; do
    [ -x "$ROOT/bin/$a/wg-user" ] || { echo "missing bin/$a/wg-user"; exit 1; }
done

version=$(sed -n 's/^version=//p' "$ROOT/module.prop")
[ -n "$version" ] || { echo "no version= in module.prop"; exit 1; }

out=$("$ROOT/bin/x86_64/wg-user" version) || { echo "amd64 binary does not run"; exit 1; }
[ "$out" = "wg-user $version" ] || {
    echo "amd64 binary reports '$out', want 'wg-user $version'"
    exit 1
}
