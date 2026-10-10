# wg-user KSU Module — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a KSU module that auto-starts a userspace WireGuard client and exposes it as a local SOCKS5/HTTP proxy, with multi-arch binaries, multi-root compatibility, hot config reload, an optional wstunnel obfuscation layer for hostile mobile networks, and a KSU WebUI.

**Architecture:** A KSU module wrapping the upstream `windtf/wireproxy` Go binary. wireproxy embeds `wireguard-go` + `gvisor/netstack` in-process, exposes SOCKS5/HTTP, and creates no TUN device — so it coexists cleanly with FlClash's TUN. An optional `erebe/wstunnel` (Rust) binary wraps the WG UDP path in WebSocket-over-TLS on port 443 to defeat carrier UDP throttling. Multi-arch (arm64-v8a, armeabi-v7a, x86_64, x86) via cross-compile (Go: trivial; Rust: NDK clang wrappers). Multi-root (KSU/Magisk/APatch) via shared shell library. Hot reload via mtime polling watcher. WebUI in plain HTML+JS using KSU's `ksu` global.

**Tech Stack:**
- Shell (POSIX sh + bash) for module scripts
- Go 1.26+ (cross-compile, CGO_ENABLED=0) for `wireproxy` binary
- Rust 1.9x + NDK 26.x clang wrappers for `wstunnel` binary
- HTML/CSS/vanilla JS for WebUI
- `make` (GNU make) for build orchestration
- `wireproxy` v1.1.3 from `github.com/windtf/wireproxy`
- `wstunnel` v11.0.0 from `github.com/erebe/wstunnel`
- `wireguard-go` v0.0.0-20250521... (transitive dep of wireproxy)
- `gvisor/netstack` v0.0.0-20250503... (transitive dep of wireproxy)

**Spec:** `docs/ARCHITECTURE.md` (in this repo). This plan is the breakdown; the spec is the "why". Read both before implementing any task.

---

## Global Constraints

- **POSIX shell**: All `*.sh` files must be POSIX-compatible (no bashisms). Use `#!/system/bin/sh` shebang. Test with `sh -n` and `dash -n` (if dash is installed).
- **Static binary**: `wireproxy` is built with `CGO_ENABLED=0`. No dynamic linking. Verify with `file bin/<arch>/wireproxy` (should be `statically linked`).
- **Rust binary**: `wstunnel` is built with the workspace `release` profile (`lto=fat, panic=abort, strip=symbols`). It IS dynamically linked to Android's `libc`/`libdl` (interpreter `/system/bin/linker64`), which is expected for Rust Android targets — do NOT flag this as a problem. Verify arch with `file bin/<arch>/wstunnel`.
- **Module dir resolution**: Runtime scripts must use `SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)`, never rely on `$MODDIR` / `$MODPATH` being set.
- **Logging**: All user-facing logs go through `common/util.sh::log_info` / `log_warn` / `log_err`. Each line is timestamped.
- **No secrets in repo**: `etc/wireproxy.conf` is committed with PLACEHOLDER keys, never real keys. `etc/wireproxy.conf.example` documents the format only. Same for `etc/tunnel.conf`.
- **Pinned upstream versions**: `scripts/build.sh` pins `WIREPROXY_VERSION=v1.1.3`; `scripts/cargo-cross.sh` pins `WSTUNNEL_VERSION=v11.0.0`. Do not auto-upgrade without a plan task.
- **Multi-arch** (4 targets): `arm64-v8a` (GOARCH=arm64 / Rust `aarch64-linux-android`), `armeabi-v7a` (GOARCH=arm / `armv7-linux-androideabi`), `x86_64` (GOARCH=amd64 / `x86_64-linux-android`), `x86` (GOARCH=386 / `i686-linux-android`). GOOS=linux for Go; Rust targets are Android-native.
- **Multi-root**: Runtime must work under KSU (`KSU_API` set), Magisk (`MAGISK_VER` set), and APatch (`APATCH` set). Use `${0%/*}` for module path.
- **No TUN device**: The module must never create `/dev/net/tun`. Verified by grep: no `tun.CreateTUN`, no `ip tuntap add`, no `ip link add wg0`.
- **Startup order**: if `etc/tunnel.conf` has `mode = wstunnel`, wstunnel MUST be listening on `local-udp` before wireproxy starts. Enforced in `wg-user start` (retry loop, fail loudly).
- **WebUI no-keyboard-jacking**: The WebUI must never log or transmit the WG private key to any external server. Local `ksu.exec` and `ksu.readAssets` only.
- **Tunnel is optional**: with `mode = off`, the module behaves exactly as if wstunnel did not exist. wireproxy's `Endpoint` is then the real WG server. No wstunnel process is started and no binary is required to be present.

---

## Review Focus

These inputs/conditions are not directly exercised by any task's tests but are
most likely to bite a real user. Each is pinned to a task below:

1. **What if `wireproxy.conf` references an env var that is not set?** — Startup must fail with a clear, log-writable error. (Pinned in Task 5.)
2. **What if the user installs on Magisk but only `customize.sh` exists?** — Magisk calls `install.sh` instead. Both must exist and both must work. (Pinned in Task 1.)
3. **What if two instances of wireproxy start accidentally (race)?** — `wg-user start` must use lockfile + PID existence check. (Pinned in Task 4.)
4. **What if the WG endpoint hostname cannot be resolved?** — wireproxy will log a DNS error; our wrapper must NOT silently restart. Watcher should not reload on log-file change. (Pinned in Task 5.)
5. **What if the user edits the config with a syntax error and the watcher restarts?** — `wireproxy -n` configtest must be run before restart; on failure, leave the OLD process running and log the error. (Pinned in Task 5 + Task 6.)
6. **What if `mode = wstunnel` but `bin/<arch>/wstunnel` is missing?** — `wg-user start` must fail with a clear message and NOT start wireproxy (which would then fail to reach a non-listening endpoint). (Pinned in Task 3.)
7. **What if wstunnel is enabled but its wss server is unreachable?** — wireproxy would see no WG handshake. `wg-user status` should report tunnel state separately (`running (pid)`, `tunnel: down`). (Pinned in Task 3 + Task 7.)
8. **What if wstunnel is enabled but not yet bound when wireproxy starts?** — `wg-user start` retries the local UDP connect up to 5 times before failing. (Pinned in Task 3.)

---

## File Structure (overview)

```
/var/lofibass_ssd/code/ksu/wg-user/
├── module.prop                       # T1: KSU/Magisk metadata
├── customize.sh                      # T1: KSU install hook
├── install.sh                        # T1: Magisk install hook
├── sepolicy.rule                     # T1: empty with header comment
├── service.sh                        # T3: KSU autostart + reload watcher
├── wg-user                           # T3: control CLI (POSIX sh)
├── common/
│   ├── util.sh                       # T3: logging, env detection, paths
│   └── abi.sh                        # T2: ABI detection + binary selection
├── bin/                              # T2: populated by build
│   ├── arm64-v8a/wireproxy
│   ├── armeabi-v7a/wireproxy
│   ├── x86_64/wireproxy
│   ├── x86/wireproxy
│   ├── arm64-v8a/wstunnel
│   ├── armeabi-v7a/wstunnel
│   ├── x86_64/wstunnel
│   └── x86/wstunnel
├── etc/
│   ├── wireproxy.conf                # T5: user WG config (placeholder)
│   ├── wireproxy.conf.example        # T5: documented template
│   ├── tunnel.conf                   # T5: tunnel mode + wstunnel settings
│   └── tunnel.conf.example           # T5: documented template
├── webroot/                          # T7: KSU WebUI
│   ├── module.prop                   # T7: WebUI metadata
│   ├── index.html                    # T7
│   ├── app.js                        # T7
│   └── style.css                     # T7
├── scripts/
│   ├── build.sh                      # T2: cross-compile wireproxy (Go)
│   └── cargo-cross.sh                # T2: cross-compile wstunnel (Rust)
├── Makefile                          # T2: build/package/verify
├── docs/
│   ├── INSTALL.md                    # T8
│   ├── CONFIG.md                     # T8
│   ├── TUNNEL-SERVER.md              # T8: VPS-side wstunnel setup
│   ├── ARCHITECTURE.md               # (already written)
│   └── CHANGELOG.md                  # T8
├── .gitignore                        # T1
├── LICENSE                           # T1
├── README.md                         # T1
└── docs/superpowers/plans/
    └── 2026-10-10-wg-user-module.md  # this file
```

**Naming:** `wg-user` is the project's product name (what users see in module list).
The control script is also called `wg-user` for symmetry. The control script is
invoked as `$MODDIR/wg-user <subcommand>` from `service.sh` and from terminal.

---

## Task 1: Repo skeleton & module metadata

**Files:**
- Create: `/.gitignore`
- Create: `/LICENSE` (ISC, matches wireproxy's license)
- Create: `/README.md` (skeleton, expanded in T8)
- Create: `/module.prop`
- Create: `/sepolicy.rule`
- Create: `/uninstall.sh`

**Interfaces:** None (this task has no upstream dependencies — it's the foundation).
Note: `customize.sh` and `install.sh` are created in Task 2 because they
source `common/abi.sh` which doesn't exist until then.

**Step 1.1 — Write `.gitignore`**

Content (these files should never be committed):
```
# Build artifacts
bin/*/wireproxy
dist/
src/

# User config
etc/wireproxy.conf

# Editor
*.swp
.vscode/
.idea/
```

**Step 1.2 — Write `LICENSE` (ISC)**

```
ISC License

Copyright (c) 2026 wg-user contributors

Permission to use, copy, modify, and/or distribute this software for any
purpose with or without fee is hereby granted, provided that the above
copyright notice and this permission notice appear in all copies.

THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
```

**Step 1.3 — Write `module.prop`**

KSU/Magisk module metadata. All three roots read this file.

```ini
id=wg-user
name=wg-user — Userspace WireGuard Proxy
version=0.1.0
versionCode=1
author=wg-user contributors
description=Auto-starts a userspace WireGuard client and exposes it as a SOCKS5/HTTP proxy on 127.0.0.1. No TUN device. Multi-arch, multi-root (KSU/Magisk/APatch). WebUI included.
updateJson=
```

**Step 1.4 — Write `sepolicy.rule`**

Empty file with a comment so future contributors know where to add rules.
Most KSU module daemons run in init context with sufficient SELinux privileges
and need no additional rules.

```
# wg-user: SELinux policy rules
# Currently empty. wireproxy runs entirely in userspace (no /dev/net/tun access)
# and uses standard UDP sockets, which are permitted by init context.
#
# If you encounter SELinux denials in logcat for the wireproxy process,
# add the minimal `allow <domain> <target>:<class> { <perms> };` lines here.
```

**Step 1.5 — Write `uninstall.sh`**

```sh
#!/system/bin/sh
# Cleanup on module uninstall
# Stops the daemon, removes PID file and the binary symlink.
MODDIR="${0%/*}"
"$MODDIR/wg-user" stop 2>/dev/null || true
rm -f "$MODDIR/run/wireproxy.pid"
exit 0
```

**Step 1.6 — Write a minimal `README.md` skeleton**

```markdown
# wg-user

> Userspace WireGuard client exposed as a SOCKS5/HTTP proxy.
> A KernelSU / Magisk / APatch module.

**Why?** When you already run FlClash in TUN mode but want a separate
WireGuard tunnel for some apps (e.g., intranet), this module gives you
a local SOCKS5 endpoint that routes through WG — without creating a TUN
device and without interfering with FlClash's TUN.

## Quick start (after installing the module)

1. Edit the WG config:
   ```sh
   adb shell su -c "vi /data/adb/modules/wg-user/etc/wireproxy.conf"
   ```
2. Reload:
   ```sh
   adb shell su -c "wg-user reload"
   ```
3. Point your client at `socks5://127.0.0.1:1080`.

## Documentation

- Architecture: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- Install: [`docs/INSTALL.md`](docs/INSTALL.md)
- Config reference: [`docs/CONFIG.md`](docs/CONFIG.md)
- Changelog: [`docs/CHANGELOG.md`](docs/CHANGELOG.md)
```

**Step 1.7 — Verify the metadata is parseable**

Run: `sh -n uninstall.sh && sh -n service.sh 2>/dev/null; true`
Expected: no output (exit 0). service.sh may not exist yet — that's fine.

Run: `cat module.prop` and visually confirm all 7 fields are present.

**Step 1.8 — Commit**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "feat(skeleton): repo skeleton, module metadata, install/uninstall hooks"
```

---

## Task 2: Build system — cross-compile wireproxy and wstunnel

**Files:**
- Create: `/scripts/build.sh` (Go pipeline: wireproxy)
- Create: `/scripts/cargo-cross.sh` (Rust pipeline: wstunnel)
- Create: `/scripts/cargo-wrappers/{aarch64-linux-android,armv7-linux-androideabi,x86_64-linux-android,i686-linux-android}-{clang,clang++,ar}` (12 wrapper scripts total)
- Create: `/common/abi.sh` (select wireproxy + wstunnel at install time)
- Create: `/Makefile` (orchestrates both pipelines)

**Interfaces (consumed by later tasks):**
- `common/abi.sh::select_wireproxy_binary <src_dir> <dst_path>` — copies the
  correct `bin/<arch>/wireproxy` to `dst_path` based on `ro.product.cpu.abi`.
- `common/abi.sh::select_wstunnel_binary <src_dir> <dst_path>` — same but for
  the wstunnel binary. Allowed to fail silently if not bundled.
- `common/abi.sh::get_device_arch` — echoes one of `arm64-v8a|armeabi-v7a|x86_64|x86`.
- `Makefile` targets: `build`, `clean`, `package`, `verify`.

**Step 2.1 — Write `customize.sh` and `install.sh`**

These reference `common/abi.sh` (also in this task) and `common/util.sh`
(next task); they can be written in either order as long as the task as a
whole is atomic. We put them at the top of Task 2 to keep the dependency
chain honest.

`customize.sh` (KSU install-time hook):

```sh
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
```

`install.sh` (Magisk install-time hook): same content, replace `$MODPATH`
with `$MODDIR`.

**Step 2.1.5 — Write NDK clang wrappers in `scripts/cargo-wrappers/`**

These wrappers translate the un-versioned names (e.g.
`aarch64-linux-android-clang`) that cargo's `cc-rs` crate requests into the
versioned NDK binaries (e.g. `aarch64-linux-android26-clang`).

For each of the 4 targets × 3 binaries (clang, clang++, ar), write a
3-line shell script. Default NDK path: `$HOME/Android/Sdk/ndk/26.3.11579264`.
Users with a non-default NDK must symlink their NDK there OR edit the
12 wrapper files.

Example `scripts/cargo-wrappers/aarch64-linux-android-clang`:

```sh
#!/bin/sh
# NDK clang wrapper for aarch64-linux-android
# Override by setting NDK env var before running cargo.
NDK="${NDK:-$HOME/Android/Sdk/ndk/26.3.11579264}"
exec "$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin/aarch64-linux-android26-clang" "$@"
```

Generate all 12 by a small generator script `scripts/cargo-wrappers/gen.sh`
(documented but not auto-run; wrappers are committed directly).

**Step 2.1.6 — Commit the wrappers**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
chmod +x scripts/cargo-wrappers/*
git add scripts/cargo-wrappers/
jj describe -m "feat(build): NDK clang wrappers for Rust Android targets"
```

**Step 2.2 — Write `scripts/build.sh`** (wireproxy — Go)

Cross-compile wireproxy for the 4 Android architectures. Use the Go module
proxy that works in CN (goproxy.cn).

```bash
#!/usr/bin/env bash
# Cross-compile wireproxy for Android (linux) on 4 architectures.
# Produces bin/<arch>/wireproxy as a static binary.
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

require github.com/windtf/wireproxy/cmd/wireproxy ${WIREPROXY_VERSION}
EOF

echo "==> fetching wireproxy ${WIREPROXY_VERSION} via ${GOPROXY}..."
GOPROXY="$GOPROXY" go mod download github.com/windtf/wireproxy/cmd/wireproxy

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
```

**Step 2.2.5 — Run the wireproxy build to verify it works**

Run: `GOPROXY=https://goproxy.cn,direct bash scripts/build.sh`
Expected: 4 binaries built, all ~9MB, all `statically linked` per `file`.

Verify with: `file bin/*/wireproxy`
Expected output (4 lines): each `ELF 64-bit LSB executable, ARM aarch64`
(or `x86-64` / `80386`), `statically linked`.

If a target fails (e.g. arm build error), check Go version (`go version` ≥ 1.21).

**Step 2.3 — Write `scripts/cargo-cross.sh`** (wstunnel — Rust)

The full Rust cross-compile pipeline. Uses the wrapper scripts created in
Step 2.1.5. Pins `WSTUNNEL_VERSION=v11.0.0`. Reads `ANDROID_NDK_HOME` env
var; defaults to `$HOME/Android/Sdk/ndk/26.3.11579264`. Installs missing
rust Android targets on first run.

```bash
#!/usr/bin/env bash
# Cross-compile wstunnel (Rust) for Android (4 architectures).
# Produces bin/<arch>/wstunnel.
# Requires: rustup + cargo, Android NDK r26+, internet (first run).
set -euo pipefail

SCRIPT_DIR="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(CDPATH= cd -- "$SCRIPT_DIR/.." && pwd)"

WSTUNNEL_VERSION="${WSTUNNEL_VERSION:-v11.0.0}"
NDK="${ANDROID_NDK_HOME:-$HOME/Android/Sdk/ndk/26.3.11579264}"
WRAPPERS="$ROOT_DIR/scripts/cargo-wrappers"

if [ ! -x "$WRAPPERS/aarch64-linux-android-clang" ]; then
    echo "FATAL: $WRAPPERS/aarch64-linux-android-clang not executable" >&2
    exit 1
fi
if [ ! -d "$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin" ]; then
    echo "FATAL: NDK not found at $NDK. Set ANDROID_NDK_HOME." >&2
    exit 1
fi

# (target, arch_dir)
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
[ ${#NEED[@]} -gt 0 ] && rustup target add "${NEED[@]}"

# Build a side workspace that pulls wstunnel-cli from upstream and uses
# the wrappers for all linkers.
BUILD="$(mktemp -d)"
trap 'rm -rf "$BUILD"' EXIT
cd "$BUILD"

cat > Cargo.toml <<EOF
[package]
name = "wstunnel-fetch"
version = "0.0.1"
edition = "2021"

[dependencies]
wstunnel-cli = { git = "https://github.com/erebe/wstunnel", tag = "${WSTUNNEL_VERSION}", default-features = false }
EOF

mkdir -p .cargo
{
    echo "[net]"
    echo "git-fetch-with-cli = true"
    for entry in "${TARGETS[@]}"; do
        t="${entry%%:*}"
        echo ""
        echo "[target.${t}]"
        echo "linker = \"${WRAPPERS}/${t}-clang\""
        echo "ar = \"${WRAPPERS}/${t}-ar\""
    done
} > .cargo/config.toml

# Build for each target.
OUT="$ROOT_DIR/bin"
for entry in "${TARGETS[@]}"; do
    t="${entry%%:*}"
    ad="$(echo "$entry" | cut -d: -f2)"
    echo "==> building wstunnel for $ad ($t)..."
    cargo build --release --target "$t" -p wstunnel-cli 2>&1 \
        | sed "s|^|  [$ad] |"
    src="$BUILD/target/$t/release/wstunnel"
    dst="$OUT/$ad/wstunnel"
    mkdir -p "$OUT/$ad"
    cp "$src" "$dst"
    chmod +x "$dst"
done

echo "==> done."
find "$OUT" -type f -name wstunnel -exec ls -lh {} \;
```

**Step 2.4 — Run the wstunnel build to verify it works**

Run: `ANDROID_NDK_HOME=$HOME/Android/Sdk/ndk/26.3.11579264 bash scripts/cargo-cross.sh`
Expected: 4 wstunnel binaries built in `bin/<arch>/wstunnel`, each ~10-12 MB.

Verify with: `file bin/*/wstunnel`
Expected: 4 lines, each `ELF 64-bit LSB executable, ARM aarch64` (or
`x86-64` / `80386`), `dynamically linked, interpreter /system/bin/linker*`.
Dynamic linking to Android's libc/libdl is expected for Rust Android
targets and is fine — the Android linker is always at that path.

**Step 2.5 — Write `common/abi.sh`**

This file lives in two places conceptually:
- In the source repo: `common/abi.sh` (committed)
- In the installed module: `$MODDIR/common/abi.sh` (same content)

It must be POSIX sh, since Android's `/system/bin/sh` is mksh, not bash.

```sh
#!/system/bin/sh
# ABI detection and wireproxy binary selection.
# Source: `. common/abi.sh` from customize.sh / install.sh / wg-user.

# get_device_arch — echo one of: arm64-v8a, armeabi-v7a, x86_64, x86
get_device_arch() {
    abi="$(getprop ro.product.cpu.abi 2>/dev/null || echo unknown)"
    case "$abi" in
        arm64-v8a) echo arm64-v8a ;;
        arm64-v8a*) echo arm64-v8a ;;
        armeabi-v7a) echo armeabi-v7a ;;
        armeabi-v7a*) echo armeabi-v7a ;;
        x86_64) echo x86_64 ;;
        x86_64*) echo x86_64 ;;
        x86) echo x86 ;;
        x86*) echo x86 ;;
        armv7*|armv8*) echo armeabi-v7a ;;  # rare fallbacks
        *) echo "unsupported:$abi" >&2; return 1 ;;
    esac
}

# select_wireproxy_binary <src_dir> <dst_path>
#   src_dir: $MODPATH (contains bin/<arch>/wireproxy)
#   dst_path: where the running binary should live (e.g. $MODPATH/bin/wireproxy)
select_wireproxy_binary() {
    src_dir="$1"
    dst_path="$2"
    arch="$(get_device_arch)" || {
        echo "wg-user: cannot detect device ABI" >&2
        return 1
    }
    src_path="$src_dir/bin/$arch/wireproxy"
    if [ ! -f "$src_path" ]; then
        echo "wg-user: no wireproxy for arch '$arch' at $src_path" >&2
        return 1
    fi
    mkdir -p "$(dirname "$dst_path")"
    cp -f "$src_path" "$dst_path"
    chmod 0755 "$dst_path"
    echo "wg-user: wireproxy: $arch"
}

# select_wstunnel_binary <src_dir> <dst_path>
#   Same as select_wireproxy_binary, but for wstunnel. May fail if the
#   user did not bundle the wstunnel binary (i.e. chose to disable
#   obfuscation at build time). Callers should `|| true` if optional.
select_wstunnel_binary() {
    src_dir="$1"
    dst_path="$2"
    arch="$(get_device_arch)" || return 1
    src_path="$src_dir/bin/$arch/wstunnel"
    if [ ! -f "$src_path" ]; then
        echo "wg-user: no wstunnel for arch '$arch' at $src_path" >&2
        return 1
    fi
    mkdir -p "$(dirname "$dst_path")"
    cp -f "$src_path" "$dst_path"
    chmod 0755 "$dst_path"
    echo "wg-user: wstunnel: $arch"
}
```

**Step 2.6 — Write `Makefile`** (extended for dual pipelines)

```make
# wg-user build orchestration
# Use: make build / make clean / make package / make verify

SHELL := /bin/bash
ROOT_DIR := $(shell pwd)
SCRIPTS  := $(ROOT_DIR)/scripts
BIN      := $(ROOT_DIR)/bin
DIST     := $(ROOT_DIR)/dist
VERSION  := $(shell grep '^version=' $(ROOT_DIR)/module.prop | cut -d= -f2)

ARCHS := arm64-v8a armeabi-v7a x86_64 x86
WPBINS := $(addprefix $(BIN)/,$(addsuffix /wireproxy,$(ARCHS)))
TNBINS := $(addprefix $(BIN)/,$(addsuffix /wstunnel,$(ARCHS)))

.PHONY: build build-go build-rust clean package verify help

help:
	@echo "make build      - cross-compile wireproxy (Go) + wstunnel (Rust)"
	@echo "make build-go   - only wireproxy (faster, ~3 min)"
	@echo "make build-rust - only wstunnel (~5 min; needs NDK + rustup targets)"
	@echo "make clean      - remove build artifacts"
	@echo "make package    - zip the module for distribution (after build)"
	@echo "make verify     - run syntax/static checks and configtest"

build: build-go build-rust

build-go: $(WPBINS)

build-rust: $(TNBINS)

$(BIN)/%/wireproxy:
	GOPROXY=$${GOPROXY:-https://goproxy.cn,direct} \
	WIREPROXY_VERSION=$${WIREPROXY_VERSION:-v1.1.3} \
	    bash $(SCRIPTS)/build.sh

$(BIN)/%/wstunnel:
	ANDROID_NDK_HOME=$${ANDROID_NDK_HOME:-$$HOME/Android/Sdk/ndk/26.3.11579264} \
	WSTUNNEL_VERSION=$${WSTUNNEL_VERSION:-v11.0.0} \
	    bash $(SCRIPTS)/cargo-cross.sh

clean:
	rm -rf $(BIN)/*/wireproxy $(BIN)/*/wstunnel
	rm -rf $(DIST)

package: build
	@echo "==> packaging wg-user v$(VERSION)..."
	@mkdir -p $(DIST)
	@cd $(ROOT_DIR) && zip -r $(DIST)/wg-user-$(VERSION).zip \
	    module.prop customize.sh install.sh uninstall.sh sepolicy.rule \
	    service.sh wg-user common/ etc/ webroot/ docs/ \
	    bin/arm64-v8a bin/armeabi-v7a bin/x86_64 bin/x86 \
	    scripts/cargo-wrappers/ \
	    README.md LICENSE 2>/dev/null || \
	  (echo "zip not found, trying tar:" && tar czf $(DIST)/wg-user-$(VERSION).tar.gz \
	    module.prop customize.sh install.sh uninstall.sh sepolicy.rule \
	    service.sh wg-user common/ etc/ webroot/ docs/ \
	    bin/arm64-v8a bin/armeabi-v7a bin/x86_64 bin/x86 \
	    scripts/cargo-wrappers/ \
	    README.md LICENSE)
	@ls -lh $(DIST)

verify:
	@echo "==> sh -n syntax checks..."
	@for f in customize.sh install.sh uninstall.sh service.sh wg-user scripts/*.sh common/*.sh scripts/cargo-wrappers/*; do \
	    case "$$f" in \
	        scripts/cargo-wrappers/*) continue ;; \
	    esac; \
	    sh -n "$$f" || { echo "syntax error in $$f"; exit 1; }; \
	    echo "  ok: $$f"; \
	done
	@if [ -x $(BIN)/x86_64/wireproxy ]; then \
	    echo "==> wireproxy -n configtest..."; \
	    if [ -f etc/wireproxy.conf ]; then \
	        $(BIN)/x86_64/wireproxy -n -c etc/wireproxy.conf || \
	            { echo "configtest failed (this is expected if PLACEHOLDER keys)"; }; \
	    fi; \
	fi
```

**Step 2.7 — Verify with `make build` then `file` checks**

Run: `make build`
Expected: 4 wireproxy binaries (~9 MB each, statically linked) AND
4 wstunnel binaries (~10-12 MB each, dynamically linked via Android
linker). Last lines of output should be `find ... -exec ls -lh`.

Run: `file bin/*/wireproxy`
Expected: 4 lines, all `statically linked`, correct architectures.

Run: `file bin/*/wstunnel`
Expected: 4 lines, all `dynamically linked, interpreter /system/bin/linker*`,
correct architectures.

Run: `make verify`
Expected: 4-7 `ok: ...` lines, no syntax errors.

**Step 2.8 — Commit**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "feat(build): dual build pipeline, wireproxy (Go) + wstunnel (Rust)"
```

---

## Task 3: Shell utilities & control script

**Files:**
- Create: `/common/util.sh`
- Create: `/wg-user` (POSIX sh script with subcommands)

**Interfaces (consumed by later tasks):**
- `common/util.sh::log_info|log_warn|log_err` — log a timestamped line to stderr.
- `common/util.sh::module_dir` — echoes the absolute path of the module's runtime dir.
- `common/util.sh::is_running <pidfile>` — returns 0 if the pidfile points to a live process.
- `wg-user <start|stop|restart|reload|status|log> ...` — see Step 3.2 for full API.

**Step 3.1 — Write `common/util.sh`**

```sh
#!/system/bin/sh
# Shared shell utilities for wg-user.
# Source: `. common/util.sh` from any script in the module.

# log_ts — echo an ISO-8601-ish timestamp
log_ts() {
    # Busybox date is the only date we have on most Android; fall back to nothing.
    date '+%Y-%m-%dT%H:%M:%S%z' 2>/dev/null || echo "0000-00-00T00:00:00+0000"
}

log_info() { printf '%s INFO  wg-user: %s\n' "$(log_ts)" "$*" >&2; }
log_warn() { printf '%s WARN  wg-user: %s\n' "$(log_ts)" "$*" >&2; }
log_err()  { printf '%s ERROR wg-user: %s\n' "$(log_ts)" "$*" >&2; }

# module_dir — echo the absolute directory of the module, regardless of how
# the calling script was invoked.
module_dir() {
    # ${0%/*} is the directory of the calling script. We resolve "." to abs.
    script_path="$0"
    case "$script_path" in
        /*) ;;
        *)  script_path="$(pwd)/$script_path" ;;
    esac
    dir="$(dirname "$script_path")"
    case "$dir" in
        .)  echo "$(pwd)" ;;
        *)  echo "$dir" ;;
    esac
}

# is_running <pidfile> — returns 0 if pidfile is present and the process is alive.
is_running() {
    pidfile="$1"
    [ -f "$pidfile" ] || return 1
    pid="$(cat "$pidfile" 2>/dev/null)" || return 1
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null
}

# root_dir — return the module's runtime root (parent of bin/, common/, etc.)
root_dir() {
    # $0 might be .../wg-user or .../bin/wireproxy; we want the module root.
    _myself="$0"
    _dir=$(CDPATH= cd -- "$(dirname -- "$_myself")" && pwd)
    # If called from .../bin/<something>, go up two levels.
    case "$_dir" in
        */bin) echo "${_dir%/bin}" ;;
        *)     echo "$_dir" ;;
    esac
}
```

**Step 3.2 — Write `wg-user` (control CLI)**

```sh
#!/system/bin/sh
# wg-user — control script for the WireGuard userspace proxy module.
#
# Top-level subcommands:
#   start                  — start wstunnel (if enabled) then wireproxy
#   stop                   — stop wireproxy then wstunnel
#   restart                — stop + start
#   reload                 — configtest; on success, stop+start
#   status                 — print status of wireproxy + tunnel
#   log [daemon] [N]       — tail N lines of wireproxy.log or wstunnel.log
#   save-config <path>     — copy file to wireproxy.conf; triggers reload
#   version                — module version
#
#   tunnel start|stop|status|restart   — sub-namespace for wstunnel
#
# All subcommands exit 0 on success, 1 on error.

set -eu

SELF="$0"
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
. "$ROOT/common/util.sh"
. "$ROOT/common/abi.sh"

WPPIDFILE="$ROOT/run/wireproxy.pid"
TNPIDFILE="$ROOT/run/wstunnel.pid"
WPLOG="$ROOT/log/wireproxy.log"
TNLOG="$ROOT/log/wstunnel.log"
WPCONF="$ROOT/etc/wireproxy.conf"
TNCONF="$ROOT/etc/tunnel.conf"
BINWP="$ROOT/bin/wireproxy"
BINTN="$ROOT/bin/wstunnel"
mkdir -p "$ROOT/run" "$ROOT/log"

# ---------- tunnel config helpers ----------

tunnel_enabled() {
    [ -f "$TNCONF" ] || return 1
    grep -Eq '^[[:space:]]*mode[[:space:]]*=[[:space:]]*(wstunnel)' "$TNCONF"
}

tunnel_local_addr() {
    awk -F'[[:space:]]*=[[:space:]]*' \
        '/^[[:space:]]*local-udp[[:space:]]*=/{print $2; exit}' "$TNCONF" 2>/dev/null
}

tunnel_remote_url() {
    awk -F'[[:space:]]*=[[:space:]]*' \
        '/^[[:space:]]*remote[[:space:]]*=/{print $2; exit}' "$TNCONF" 2>/dev/null
}

tunnel_auth() {
    u=$(awk -F'[[:space:]]*=[[:space:]]*' '/^[[:space:]]*username[[:space:]]*=/{print $2; exit}' "$TNCONF" 2>/dev/null)
    p=$(awk -F'[[:space:]]*=[[:space:]]*' '/^[[:space:]]*password[[:space:]]*=/{print $2; exit}' "$TNCONF" 2>/dev/null)
    if [ -n "$u" ] && [ -n "$p" ]; then echo "$u:$p"; fi
}

# ---------- tunnel lifecycle ----------

cmd_tunnel_start() {
    if ! tunnel_enabled; then
        log_info "tunnel disabled in $TNCONF (mode = off)"
        return 0
    fi
    if [ ! -x "$BINTN" ]; then
        log_err "tunnel enabled but wstunnel binary missing: $BINTN"
        return 1
    fi
    if is_running "$TNPIDFILE"; then
        log_info "tunnel already running (pid $(cat "$TNPIDFILE"))"
        return 0
    fi
    local_addr=$(tunnel_local_addr); local_addr=${local_addr:-127.0.0.1:51820}
    remote_url=$(tunnel_remote_url)
    if [ -z "$remote_url" ]; then
        log_err "$TNCONF: missing 'remote' URL (e.g. wss://server:443)"
        return 1
    fi
    auth=$(tunnel_auth)

    log_info "starting wstunnel: $local_addr <-> $remote_url"
    # shellcheck disable=SC2086
    nohup "$BINTN" client \
        -L "udp://$local_addr" \
        -F "$remote_url" \
        ${auth:+--ws-auth "$auth"} \
        > "$TNLOG" 2>&1 &
    pid=$!
    echo "$pid" > "$TNPIDFILE"

    # Wait for the local UDP listener to be reachable. /dev/udp is bash/ksh
    # only; on busybox it's missing — fall back to a short sleep loop and
    # check the process is still alive.
    i=0
    while [ $i -lt 25 ]; do
        sleep 0.2
        if ! is_running "$TNPIDFILE"; then break; fi
        i=$((i + 1))
    done
    if ! is_running "$TNPIDFILE"; then
        log_err "wstunnel exited immediately. Last log:"
        tail -n 20 "$TNLOG" >&2
        return 1
    fi
    log_info "tunnel started (pid $pid) on udp/$local_addr"
}

cmd_tunnel_stop() {
    if ! is_running "$TNPIDFILE"; then
        rm -f "$TNPIDFILE"
        log_info "tunnel not running"
        return 0
    fi
    pid=$(cat "$TNPIDFILE")
    log_info "stopping wstunnel (pid $pid)..."
    kill -INT "$pid" 2>/dev/null || true
    i=0
    while [ $i -lt 30 ] && kill -0 "$pid" 2>/dev/null; do
        sleep 0.1
        i=$((i + 1))
    done
    kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null || true
    rm -f "$TNPIDFILE"
    log_info "tunnel stopped"
}

cmd_tunnel_status() {
    if ! tunnel_enabled; then
        echo "tunnel: disabled"
        return 0
    fi
    if is_running "$TNPIDFILE"; then
        echo "tunnel: running (pid $(cat "$TNPIDFILE"))"
    else
        echo "tunnel: stopped"
        return 1
    fi
}

# ---------- wireproxy lifecycle ----------

cmd_start() {
    # Order matters: tunnel first, then wireproxy.
    cmd_tunnel_start || { log_err "tunnel failed to start; aborting"; return 1; }

    if is_running "$WPPIDFILE"; then
        log_info "wireproxy already running (pid $(cat "$WPPIDFILE"))"
        return 0
    fi
    if [ ! -x "$BINWP" ]; then
        log_err "binary not installed: $BINWP. Run 'customize.sh' or 'install.sh'."
        return 1
    fi
    if [ ! -f "$WPCONF" ]; then
        log_err "config not found: $WPCONF"
        return 1
    fi

    log_info "starting wireproxy..."
    nohup "$BINWP" -c "$WPCONF" > "$WPLOG" 2>&1 &
    pid=$!
    echo "$pid" > "$WPPIDFILE"

    if [ -e "/proc/$pid/oom_score_adj" ]; then
        echo -1000 > "/proc/$pid/oom_score_adj" 2>/dev/null \
            && log_info "set oom_score_adj=-1000" \
            || log_warn "could not set oom_score_adj (non-fatal)"
    fi

    sleep 1
    if ! is_running "$WPPIDFILE"; then
        log_err "wireproxy exited immediately. Last log:"
        tail -n 20 "$WPLOG" >&2
        return 1
    fi
    log_info "wireproxy started (pid $pid). Logs: $WPLOG"
}

cmd_stop() {
    if is_running "$WPPIDFILE"; then
        pid=$(cat "$WPPIDFILE")
        log_info "stopping wireproxy (pid $pid)..."
        kill -INT "$pid" 2>/dev/null || true
        i=0
        while [ $i -lt 50 ] && kill -0 "$pid" 2>/dev/null; do
            sleep 0.1
            i=$((i + 1))
        done
        kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null || true
        rm -f "$WPPIDFILE"
        log_info "wireproxy stopped"
    else
        rm -f "$WPPIDFILE"
        log_info "wireproxy not running"
    fi
    # Stop the tunnel after wireproxy so the WG handshake errors don't pile up.
    cmd_tunnel_stop
}

cmd_restart() {
    cmd_stop
    cmd_start
}

cmd_reload() {
    if [ ! -f "$WPCONF" ]; then
        log_err "no config at $WPCONF"
        return 1
    fi
    log_info "configtest $WPCONF"
    if ! "$BINWP" -n -c "$WPCONF"; then
        log_err "configtest FAILED; not reloading. See $WPLOG for details."
        return 1
    fi
    log_info "config OK; restarting"
    cmd_restart
}

cmd_status() {
    rc=0
    if is_running "$WPPIDFILE"; then
        pid=$(cat "$WPPIDFILE")
        log_info "wireproxy: running (pid $pid)"
        [ -f "$WPLOG" ] && log_info "last log: $(tail -n1 "$WPLOG")"
    else
        log_info "wireproxy: stopped"
        rc=1
    fi
    cmd_tunnel_status || rc=$?
    return $rc
}

cmd_log() {
    target="${1:-wireproxy}"
    n="${2:-50}"
    case "$target" in
        wireproxy|wp) f="$WPLOG" ;;
        tunnel|tn)     f="$TNLOG" ;;
        *) log_err "log: unknown target '$target' (use 'wireproxy' or 'tunnel')"; return 1 ;;
    esac
    [ -f "$f" ] || { log_err "no log: $f"; return 1; }
    tail -n "$n" "$f"
}

cmd_save_config() {
    src="$1"
    [ -n "$src" ] && [ -f "$src" ] || { log_err "save-config: file not found: $src"; return 1; }
    cp -f "$src" "$WPCONF"
    log_info "saved config to $WPCONF"
    cmd_reload
}

cmd_version() {
    grep '^version=' "$ROOT/module.prop" | cut -d= -f2
}

usage() {
    sed -n '2,/^$/p' "$0" | head -n 40 | sed 's/^# \{0,1\}//'
    return 1
}

# ---------- dispatcher ----------

# Handle the "tunnel" sub-namespace explicitly so the dispatcher stays flat.
if [ "${1:-}" = "tunnel" ]; then
    shift
    case "${1:-}" in
        start)   cmd_tunnel_start ;;
        stop)    cmd_tunnel_stop ;;
        status)  cmd_tunnel_status ;;
        restart) cmd_tunnel_stop; cmd_tunnel_start ;;
        *)       log_err "tunnel: unknown subcommand: $1"; usage; exit 1 ;;
    esac
    exit $?
fi

case "${1:-}" in
    start)        cmd_start ;;
    stop)         cmd_stop ;;
    restart)      cmd_restart ;;
    reload)       cmd_reload ;;
    status)       cmd_status ;;
    log)          shift; cmd_log "$@" ;;
    save-config)  shift; cmd_save_config "$@" ;;
    version)      cmd_version ;;
    ""|-h|--help|help) usage ;;
    *) log_err "unknown subcommand: $1"; usage; exit 1 ;;
esac
```

**Step 3.3 — Smoke test the script on the dev machine**

The script is POSIX sh and uses only tools available on Android's mksh, so
we can partially test it on the dev machine (Linux) by stubbing the binary.

Run: `sh -n wg-user && sh -n common/util.sh && sh -n common/abi.sh`
Expected: no output, exit 0.

Run: `bash -n wg-user` (extra safety)
Expected: no output, exit 0.

For end-to-end, we'll need to flash to a real device; document this in
the test plan in `docs/INSTALL.md` (Task 8).

**Step 3.4 — Commit**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "feat(control): POSIX-sh wg-user CLI + shared utils"
```

---

## Task 4: `service.sh` — autostart + hot reload watcher

**Files:**
- Create: `/service.sh`

**Interfaces (consumed by all roots):**
- Invoked by KSU/Magisk/APatch at `late_start service` / `service.d` time.
- Sources `common/util.sh` and `wg-user`.
- Starts wireproxy, then runs a background reload watcher.
- On module disable, the watcher's parent (this script) should also exit
  cleanly. Implementation: use a trap on TERM/INT, kill the watcher, then
  optionally stop wireproxy. (Most KSU frameworks just kill the script;
  the wireproxy process needs to be reaped on next boot.)

**Step 4.1 — Write `service.sh`**

```sh
#!/system/bin/sh
# wg-user autostart + hot reload.
# Invoked by KSU/Magisk/APatch at late_start. Do NOT block the boot path —
# fork everything.

set -eu

# Resolve module dir regardless of which root solution invoked us.
SELF="${0:-/data/adb/modules/wg-user/service.sh}"
case "$SELF" in
    /*) ;;
    *)  SELF="/data/adb/modules/wg-user/service.sh" ;;
esac
ROOT="$(CDPATH= cd -- "$(dirname -- "$SELF")" && pwd)"

. "$ROOT/common/util.sh"
. "$ROOT/common/abi.sh"

# Sanity: ensure the binaries are in place. customize.sh / install.sh
# should have run; if not (e.g. partial install), pick them now.
if [ ! -x "$ROOT/bin/wireproxy" ]; then
    log_warn "wireproxy missing; running ABI selection"
    select_wireproxy_binary "$ROOT" "$ROOT/bin/wireproxy" || {
        log_err "no wireproxy binary available; service.sh exiting"
        exit 1
    }
fi
# wstunnel is optional — only needed if tunnel.conf enables it.
if [ ! -x "$ROOT/bin/wstunnel" ]; then
    log_warn "wstunnel not bundled; tunnel mode will be unavailable"
fi

# Start the daemon (this internally starts wstunnel first if enabled,
# then wireproxy — see wg-user).
"$ROOT/wg-user" start

# Start the reload watcher in the background. Watches BOTH wireproxy.conf
# AND tunnel.conf; reload handles the right restart order.
WATCHER="$ROOT/wg-user-reload"
cat > "$WATCHER" <<'WATCHER_EOF'
#!/system/bin/sh
set -eu
ROOT="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
. "$ROOT/common/util.sh"

WPCONF="$ROOT/etc/wireproxy.conf"
TNCONF="$ROOT/etc/tunnel.conf"
WPLOG="$ROOT/log/wireproxy.log"
TNLOG="$ROOT/log/wstunnel.log"
INTERVAL="${WGU_RELOAD_INTERVAL:-5}"

# Initialize mtimes.
wp_mtime=0
tn_mtime=0
[ -f "$WPCONF" ] && wp_mtime=$(stat -c %Y "$WPCONF" 2>/dev/null || stat -f %m "$WPCONF" 2>/dev/null || echo 0)
[ -f "$TNCONF" ] && tn_mtime=$(stat -c %Y "$TNCONF" 2>/dev/null || stat -f %m "$TNCONF" 2>/dev/null || echo 0)

log_info "reload-watcher started (interval ${INTERVAL}s); watching $WPCONF and $TNCONF"

while :; do
    sleep "$INTERVAL" 2>/dev/null || sleep 5

    cur_wp=0; cur_tn=0
    [ -f "$WPCONF" ] && cur_wp=$(stat -c %Y "$WPCONF" 2>/dev/null || stat -f %m "$WPCONF" 2>/dev/null || echo 0)
    [ -f "$TNCONF" ] && cur_tn=$(stat -c %Y "$TNCONF" 2>/dev/null || stat -f %m "$TNCONF" 2>/dev/null || echo 0)

    wp_changed=0; tn_changed=0
    [ "$cur_wp" != "$wp_mtime" ] && [ "$cur_wp" != "0" ] && wp_changed=1
    [ "$cur_tn" != "$tn_mtime" ] && [ "$cur_tn" != "0" ] && tn_changed=1

    if [ $wp_changed -eq 0 ] && [ $tn_changed -eq 0 ]; then
        continue
    fi

    log_info "config change: wireproxy=$wp_changed tunnel=$tn_changed"
    wp_mtime=$cur_wp
    tn_mtime=$cur_tn
    sleep 1  # debounce

    # `wg-user reload` configtests wireproxy.conf, then restarts both
    # in the right order (tunnel first if enabled, then wireproxy).
    "$ROOT/wg-user" reload || log_warn "reload failed; see $WPLOG or $TNLOG"
done
WATCHER_EOF
chmod 0755 "$WATCHER"

nohup "$WATCHER" > "$ROOT/log/watcher.log" 2>&1 &
watcher_pid=$!
echo "$watcher_pid" > "$ROOT/run/watcher.pid"
log_info "watcher started (pid $watcher_pid)"

# Hand off; service.sh returns immediately so the root framework isn't blocked.
exit 0
```

**Step 4.2 — Verify scripts are POSIX-compatible**

Run: `sh -n service.sh && sh -n common/util.sh && sh -n common/abi.sh && sh -n wg-user`
Expected: no output, exit 0.

Run: `bash -n service.sh && bash -n wg-user`
Expected: no output, exit 0.

Run: `command -v dash && dash -n service.sh`  (if dash is installed)
Expected: no output, exit 0. (POSIX check.)

If dash is not installed, document the dash absence in a comment; do not
block on it.

**Step 4.3 — Commit**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "feat(autostart): service.sh with hot-reload watcher"
```

---

## Task 5: Default configuration

**Files:**
- Create: `/etc/wireproxy.conf` (placeholder, real values for user to fill)
- Create: `/etc/wireproxy.conf.example` (fully documented template)

**Step 5.1 — Write `etc/wireproxy.conf`**

A working-looking but inert default. The user must replace keys/endpoint.

> **When wstunnel is enabled**, `[Peer] Endpoint` must be `127.0.0.1:51820`
> (the local-udp from `tunnel.conf`), not the real server. Both files must
> be kept consistent. See `docs/CONFIG.md` for details.

```ini
# wg-user — runtime config (wireproxy v1.1.3 format)
# Edit me. After saving, the watcher (or `wg-user reload`) will restart wireproxy.
# The values below are PLACEHOLDERS — replace with your real WG setup.
#
# Reference: https://github.com/windtf/wireproxy (Sample config file section)

[Interface]
# The /32 (IPv4) and /128 (IPv6) address of this client inside the WG network.
# Do NOT use /24 — wireproxy requires /32.
Address = 10.0.0.2/32
# Optional MTU; default is fine for most.
# MTU = 1420
PrivateKey = REPLACE_WITH_YOUR_WG_PRIVATE_KEY_BASE64
# DNS resolvable through the tunnel; only used by the daemon (not by clients).
DNS = 10.0.0.1

[Peer]
PublicKey = REPLACE_WITH_PEER_PUBLIC_KEY_BASE64
# Optional but recommended; speeds up roaming.
# PresharedKey =
# The remote WG endpoint: hostname or IP + UDP port.
Endpoint = wireguard.example.com:51820
# Keep the NAT mapping alive on mobile networks.
PersistentKeepalive = 25
# 0.0.0.0/0 routes ALL traffic through the tunnel; for split-tunneling,
# narrow this to specific subnets (e.g. 10.0.0.0/8).
# Note: with SOCKS5 inbound, AllowedIPs controls which destinations the
# SOCKS5 clients are allowed to reach — it is a per-Peer allowlist, not a
# global route.
AllowedIPs = 0.0.0.0/0

# The SOCKS5 proxy that local apps (e.g. FlClash) will connect to.
[Socks5]
BindAddress = 127.0.0.1:1080
# Optional authentication:
# Username =
# Password =

# Optional HTTP proxy on a different port (uncomment to enable):
# [HTTP]
# BindAddress = 127.0.0.1:8080
```

**Step 5.2 — Write `etc/wireproxy.conf.example`**

A copy of the above plus an extended comment block at the top. Note that
the wireproxy INI format supports `$VAR` references — a user who writes
`PrivateKey = $MY_WG_KEY` and forgets to export `MY_WG_KEY` will get a
startup failure; `wireproxy -n` catches it. Document this in CONFIG.md.

```ini
# =============================================================================
# wg-user / wireproxy configuration reference
# =============================================================================
# This file follows the wireproxy INI format. The required sections are
# [Interface] and [Peer]; at least one of [Socks5], [HTTP], or [TCPClientTunnel]
# must be present to actually accept traffic.
#
# All string values may reference environment variables: $MY_VAR.
# Use $$ to escape a literal $ (e.g. for a password containing $).
#
# Most fields are case-insensitive. Section names are case-sensitive.
# =============================================================================

# === [Interface] — this client ===
[Interface]
# Required. The IPv4 (/32) and/or IPv6 (/128) address inside the WG network.
Address = 10.0.0.2/32
# Required. Base64-encoded private key for this client. Keep this SECRET.
# Generate with: wg genkey | tee privatekey | wg pubkey > publickey
PrivateKey = <base64>
# Optional. DNS servers reachable through the tunnel. Used by wireproxy for
# the WG endpoint hostname (e.g. if Endpoint is a hostname).
DNS = 10.0.0.1
# Optional. Default 1420.
# MTU = 1420

# === [Peer] — the remote WG server ===
[Peer]
# Required. Base64-encoded public key of the remote peer.
PublicKey = <base64>
# Required. host:port of the remote WireGuard UDP listener.
Endpoint = wireguard.example.com:51820
# Optional. Pre-shared key (extra layer of post-quantum-ready symmetric key).
# PresharedKey =
# Optional. Default 0 (off). 25 is a sensible value on mobile networks.
PersistentKeepalive = 25
# Optional but important. AllowedIPs is the allowlist of CIDRs the SOCKS5
# clients can reach THROUGH this peer. To allow all: 0.0.0.0/0, ::/0
AllowedIPs = 0.0.0.0/0

# === One or more of these inbound sections ===
[Socks5]
BindAddress = 127.0.0.1:1080
# Optional. If set, SOCKS5 auth is required.
# Username = user
# Password = pass

# [HTTP]
# BindAddress = 127.0.0.1:8080
# Username = user
# Password = pass
# CertFile = /path/to/cert.pem
# KeyFile  = /path/to/key.pem

# [TCPClientTunnel] — listen on a local port, forward to remote through WG.
# BindAddress = 127.0.0.1:3389
# Target      = remotehost:3389
```

**Step 5.3 — Verify with `wireproxy -n` (configtest)**

Run: `bin/x86_64/wireproxy -n -c etc/wireproxy.conf`
Expected: The binary will try to parse; with PLACEHOLDER keys (base64-shaped
strings) the configtest will likely fail because they're not valid base64
keys of correct length. This is OK — what we're verifying is that the
binary can read and parse the file structure. If it complains about
"configtest failed: <reason>", the failure should be about keys, not about
malformed INI.

If the binary errors out before parsing, fix the INI syntax.

**Step 5.4 — Write `etc/tunnel.conf`** (default: disabled)

```ini
# wg-user obfuscation transport configuration.
#
# Set mode = wstunnel to wrap the WireGuard UDP path in a WebSocket-over-TLS
# tunnel on port 443. This defeats carrier UDP throttling on mobile networks.
# When mode = off, wireproxy talks to the real WG endpoint directly.
#
# IMPORTANT: when enabling wstunnel, also change wireproxy.conf's
# [Peer] Endpoint to 127.0.0.1:<local-udp below>. wstunnel then forwards
# from that local UDP port to the real server over TLS/443.

mode = off

[wstunnel]
# Local UDP listener that wireproxy will dial.
# Must match [Peer] Endpoint in wireproxy.conf.
local-udp = 127.0.0.1:51820

# Remote wstunnel server (the VPS). Use wss:// for WebSocket-over-TLS (443).
# Example: remote = wss://your-server.com:443
remote =

# Optional HTTP basic auth, if your wstunnel server requires it.
# username =
# password =

# Optional: when true, skip TLS certificate verification.
# Only for self-signed test certs. Never use in production.
# insecure = false
```

**Step 5.5 — Write `etc/tunnel.conf.example`**

A copy of the above with additional examples:

```ini
# =============================================================================
# wg-user obfuscation transport — configuration reference
# =============================================================================
# wstunnel docs: https://github.com/erebe/wstunnel
#
# Recommended VPS setup (see docs/TUNNEL-SERVER.md):
#   wstunnel server wss://0.0.0.0:443 --restrict-to 127.0.0.1:51820
# (with a normal wireguard daemon on 127.0.0.1:51820 of the VPS)
# =============================================================================

# mode must be "off" or "wstunnel". Any other value is treated as off.
mode = off

[wstunnel]
# Local UDP listener. wireproxy's [Peer] Endpoint must equal this.
local-udp = 127.0.0.1:51820

# Remote server. Options:
#   wss://host:443   WebSocket over TLS (recommended, HTTPS-disguised)
#   ws://host:port   plain WebSocket (only inside a trusted LAN)
#   wqs://host:443   WebSocket over QUIC (better on very lossy networks)
# remote = wss://your-server.com:443
remote =

# Optional basic auth. Both username and password must be set to enable.
# username = myuser
# password = mypassword

# Optional: skip certificate verification (self-signed test only).
# insecure = false
```

**Step 5.6 — Verify with `wireproxy -n` (configtest)**

Run: `bin/x86_64/wireproxy -n -c etc/wireproxy.conf`
Expected: the binary will parse the file structure. With PLACEHOLDER keys,
it should fail with a key-format error (not a syntax error).

Run: `sh -n wg-user` to verify the tunnel-config parser is syntactically valid.

**Step 5.7 — Commit**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "feat(config): wireproxy config + tunnel config (wstunnel)"
```

---

## Task 6: Run-level smoke test

**Files:** None new. This task exercises Tasks 1–5 on the dev machine.

**Step 6.1 — Stand up a local mock environment**

Since we have no Android device/emulator, we simulate the module layout in
a temp dir and run `wg-user` against a mock `wireproxy` that just sleeps.

```bash
SANDBOX=$(mktemp -d)
trap "rm -rf $SANDBOX" EXIT
mkdir -p "$SANDBOX"/{bin,common,etc,run,log,webroot,scripts}
cp -r common/* "$SANDBOX/common/"
cp wg-user "$SANDBOX/"
cp module.prop "$SANDBOX/"

# Mock wireproxy: pretends to start, sleeps forever.
cat > "$SANDBOX/bin/wireproxy" <<'EOF'
#!/bin/sh
echo "mock wireproxy running (pid $$) with: $@"
exec sleep 3600
EOF
chmod +x "$SANDBOX/bin/wireproxy"

# Mock wstunnel: pretends to start, sleeps forever.
cat > "$SANDBOX/bin/wstunnel" <<'EOF'
#!/bin/sh
echo "mock wstunnel running (pid $$) with: $@"
exec sleep 3600
EOF
chmod +x "$SANDBOX/bin/wstunnel"

# Mock configs.
cp etc/wireproxy.conf "$SANDBOX/etc/"
cp etc/tunnel.conf "$SANDBOX/etc/"

cd "$SANDBOX"
```

**Step 6.2 — Run `start / status / stop`**

Run: `./wg-user start`
Expected: stderr line "starting wireproxy..." and "started (pid ...)".

Run: `./wg-user status`
Expected: stderr line "running (pid ...)".

Run: `./wg-user stop`
Expected: stderr line "stopping..." and "stopped".

Run: `./wg-user status`
Expected: stderr line "not running" and exit 1.

**Step 6.2.1 — Test race-condition handling**

While wireproxy is running, run `./wg-user start` again. Expected: log
"already running" and exit 0 (not a duplicate process). This pins the
Review Focus item about race conditions.

**Step 6.2.2 — Test tunnel mode**

Edit `$SANDBOX/etc/tunnel.conf` to enable the tunnel: replace `mode = off`
with `mode = wstunnel` and set `remote = wss://127.0.0.1:65535` (any
non-empty value is fine since wstunnel is mocked). Then:

Run: `./wg-user start`
Expected: stderr line "starting wstunnel: 127.0.0.1:51820 <-> wss://..." and
"tunnel started (pid ...)" followed by "starting wireproxy..." and
"wireproxy started (pid ...)".

Run: `./wg-user status`
Expected: "wireproxy: running" AND "tunnel: running" lines.

Run: `./wg-user tunnel status`
Expected: "tunnel: running".

Run: `./wg-user tunnel stop`
Expected: "stopping wstunnel" / "tunnel stopped".

Run: `./wg-user status`
Expected: "wireproxy: running" AND "tunnel: stopped" lines.

Run: `./wg-user stop`
Expected: "stopping wireproxy" / "wireproxy stopped" / "tunnel stopped".

**Step 6.2.3 — Test tunnel mode auto-disabled path**

Edit `$SANDBOX/etc/tunnel.conf` back to `mode = off`. Run:
`./wg-user start`. Expected: NO "starting wstunnel" line; only
"tunnel disabled" followed by the wireproxy start. This pins the
Review Focus item about tunnel mode being optional.

Clean up: `./wg-user stop`

**Step 6.3 — Test reload behavior**

Run: `./wg-user start; sleep 0.5; touch etc/wireproxy.conf; sleep 0.5; ./wg-user status`
The status should still show "running" but the PID should differ (it restarted).
If the PID is the same, the reload path is broken — investigate.

**Step 6.3.1 — Test reload on invalid config**

Run: `./wg-user start; sleep 0.5; echo "garbage [[[" > etc/wireproxy.conf; sleep 0.5; ./wg-user status`
Expected: status still shows "running" with the **old PID** (wireproxy -n
rejected the garbage and the running daemon was not touched). Log file
should contain a "configtest FAILED" line. This pins the Review Focus
item about bad config not killing the live process.

**Step 6.3.2 — Test reload on tunnel.conf change**

With the tunnel enabled and both daemons running, touch
`$SANDBOX/etc/tunnel.conf`. Expected: the watcher (running from
`service.sh`, not `wg-user` directly) detects the change and calls
`wg-user reload`, which restarts both. Since this sandbox doesn't run
`service.sh`, instead test the direct path:

Run: `./wg-user start; wp_pid=$(cat run/wireproxy.pid); tn_pid=$(cat run/wstunnel.pid); touch etc/tunnel.conf; ./wg-user reload; sleep 0.5; ./wg-user status`
Expected: new wireproxy PID != `$wp_pid`, new wstunnel PID != `$tn_pid`,
status shows both running.

Clean up: `./wg-user stop`

**Step 6.4 — Verify the sandbox cleanup**

Run: `rm -rf "$SANDBOX"`
Expected: clean removal. The trap handles this automatically.

**Step 6.5 — Commit any fixes found in Tasks 1–7**

If the smoke test revealed bugs, fix them, then:
```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "fix: address issues from smoke test"
```

---

## Task 7: KSU WebUI

**Files:**
- Create: `/webroot/module.prop`
- Create: `/webroot/index.html`
- Create: `/webroot/app.js`
- Create: `/webroot/style.css`

**Interfaces (consumed by users via KSU WebView):**
- Page loads in KSU WebView with the `ksu` global available.
- Buttons call `ksu.exec("wg-user <subcommand>")`.
- The log viewer uses `ksu.exec("wg-user log 50")`.
- The config editor uses `ksu.readAssets("etc/wireproxy.conf")` to load and
  `ksu.exec("wg-user save-config /sdcard/Download/wg-user.conf")` to save.

**Step 7.1 — Write `webroot/module.prop`**

KSU WebUI metadata (separate from the root `module.prop`):

```ini
id=wg-user
name=wg-user
version=0.1.0
versionCode=1
author=wg-user contributors
description=Userspace WireGuard proxy module
```

**Step 7.2 — Write `webroot/index.html`**

```html
<!doctype html>
<html lang="en">
<head>
    <meta charset="utf-8">
    <title>wg-user</title>
    <link rel="stylesheet" href="style.css">
</head>
<body>
    <header>
        <h1>wg-user</h1>
        <p class="subtitle">Userspace WireGuard Proxy</p>
    </header>

    <section class="card">
        <h2>Status</h2>
        <div id="status">
            <span class="dot" id="status-dot"></span>
            <span id="status-text">checking…</span>
        </div>
        <div id="meta" class="muted"></div>
    </section>

    <section class="card">
        <h2>Controls</h2>
        <div class="row">
            <button id="btn-start">Start</button>
            <button id="btn-stop">Stop</button>
            <button id="btn-restart">Restart</button>
            <button id="btn-reload">Reload</button>
        </div>
    </section>

    <section class="card">
        <h2>Tunnel <small class="muted">(wstunnel, optional)</small></h2>
        <div class="row">
            <span class="dot" id="tunnel-dot"></span>
            <span id="tunnel-text">checking…</span>
        </div>
        <div class="row" style="margin-top: 8px;">
            <button id="btn-tunnel-start">Start</button>
            <button id="btn-tunnel-stop">Stop</button>
            <button id="btn-tunnel-restart">Restart</button>
        </div>
        <details style="margin-top: 10px;">
            <summary>Config (etc/tunnel.conf)</summary>
            <textarea id="tunnel-editor" spellcheck="false" rows="6"></textarea>
            <div class="row">
                <button id="btn-load-tunnel">Load</button>
                <button id="btn-save-tunnel" class="primary">Save & Reload</button>
            </div>
            <div id="tunnel-save-status" class="muted"></div>
        </details>
    </section>

    <section class="card">
        <h2>Config <small class="muted">(etc/wireproxy.conf)</small></h2>
        <textarea id="config-editor" spellcheck="false" rows="20"></textarea>
        <div class="row">
            <button id="btn-load-config">Load</button>
            <button id="btn-save-config" class="primary">Save & Reload</button>
        </div>
        <div id="config-status" class="muted"></div>
    </section>

    <section class="card">
        <h2>Log <small class="muted">(last 50 lines)</small></h2>
        <pre id="log-view" class="log"></pre>
        <div class="row">
            <button id="btn-refresh-log">Refresh</button>
            <button id="btn-clear-log">Clear</button>
        </div>
    </section>

    <script src="app.js"></script>
</body>
</html>
```

**Step 7.3 — Write `webroot/style.css`**

```css
:root {
    --bg: #0f1115;
    --card: #181b22;
    --fg: #e6e8ee;
    --muted: #8b93a7;
    --accent: #5b8cff;
    --ok: #3ecf8e;
    --warn: #ffb454;
    --err: #ff6b6b;
    --border: #262a33;
}
* { box-sizing: border-box; }
body {
    margin: 0;
    font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
    background: var(--bg);
    color: var(--fg);
    padding: 16px;
}
header h1 { margin: 0; font-size: 22px; }
.subtitle { margin: 0 0 16px; color: var(--muted); font-size: 13px; }
.card {
    background: var(--card);
    border: 1px solid var(--border);
    border-radius: 8px;
    padding: 14px 16px;
    margin-bottom: 14px;
}
.card h2 { margin: 0 0 10px; font-size: 15px; }
.card h2 small { font-weight: normal; }
.row { display: flex; gap: 8px; flex-wrap: wrap; margin-top: 10px; }
button {
    background: #2a2f3a;
    color: var(--fg);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 7px 14px;
    font-size: 13px;
    cursor: pointer;
}
button:hover { background: #353b48; }
button.primary { background: var(--accent); border-color: var(--accent); }
button:disabled { opacity: 0.5; cursor: not-allowed; }
.dot {
    display: inline-block;
    width: 10px; height: 10px;
    border-radius: 50%;
    background: var(--muted);
    margin-right: 6px;
    vertical-align: middle;
}
.dot.ok { background: var(--ok); box-shadow: 0 0 6px var(--ok); }
.dot.err { background: var(--err); }
.muted { color: var(--muted); font-size: 12px; }
textarea#config-editor {
    width: 100%;
    background: #0a0c11;
    color: var(--fg);
    border: 1px solid var(--border);
    border-radius: 6px;
    font-family: "SF Mono", Menlo, Consolas, monospace;
    font-size: 12px;
    padding: 10px;
    resize: vertical;
}
pre.log {
    background: #0a0c11;
    color: var(--fg);
    border: 1px solid var(--border);
    border-radius: 6px;
    padding: 10px;
    font-family: "SF Mono", Menlo, Consolas, monospace;
    font-size: 11px;
    line-height: 1.4;
    max-height: 360px;
    overflow: auto;
    white-space: pre-wrap;
    word-break: break-all;
}
```

**Step 7.4 — Write `webroot/app.js`**

```javascript
// wg-user WebUI controller. Uses the KSU-provided `ksu` global.
// API: ksu.exec(cmd) → string; ksu.spawn(cmd); ksu.readAssets(name) → string.

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => Array.from(document.querySelectorAll(sel));

function setStatus(state, text) {
    const dot = $("#status-dot");
    const txt = $("#status-text");
    dot.classList.remove("ok", "err");
    if (state === "ok") dot.classList.add("ok");
    if (state === "err") dot.classList.add("err");
    txt.textContent = text;
}

async function refreshStatus() {
    try {
        const out = await ksu.exec("wg-user status");
        setStatus("ok", "Running");
        $("#meta").textContent = out.split("\n").filter(Boolean).join(" · ");
    } catch (e) {
        setStatus("err", "Stopped");
        $("#meta").textContent = "";
    }
}

async function refreshLog() {
    try {
        const text = await ksu.exec("wg-user log 50");
        $("#log-view").textContent = text || "(no log)";
    } catch (e) {
        $("#log-view").textContent = "Error: " + e;
    }
}

async function loadConfig() {
    try {
        const text = await ksu.readAssets("etc/wireproxy.conf");
        $("#config-editor").value = text;
        $("#config-status").textContent = "Loaded.";
    } catch (e) {
        $("#config-status").textContent = "Load error: " + e;
    }
}

async function saveConfig() {
    const text = $("#config-editor").value;
    // KSU WebUI cannot write directly into module dir; stage via a temp
    // file in /sdcard, then have wg-user copy it.
    const stage = "/sdcard/wg-user.conf.tmp";
    try {
        // We don't have a direct file-write API in older KSU; fall back
        // to writing via shell. This is best-effort.
        const b64 = btoa(unescape(encodeURIComponent(text)));
        await ksu.exec(
            `echo '${b64}' | base64 -d > ${stage} && wg-user save-config ${stage} && rm ${stage}`
        );
        $("#config-status").textContent = "Saved & reloaded.";
        await refreshStatus();
        await refreshLog();
    } catch (e) {
        $("#config-status").textContent = "Save error: " + e;
    }
}

async function runCmd(name, sub) {
    const btn = $(`#btn-${name}`);
    btn.disabled = true;
    try {
        await ksu.exec(`wg-user ${sub}`);
    } catch (e) {
        console.error(e);
    } finally {
        btn.disabled = false;
        await refreshStatus();
        await refreshLog();
    }
}

window.addEventListener("DOMContentLoaded", () => {
    $$("button").forEach((b) => b.addEventListener("click", () => {}));
    $("#btn-start").onclick = () => runCmd("start", "start");
    $("#btn-stop").onclick = () => runCmd("stop", "stop");
    $("#btn-restart").onclick = () => runCmd("restart", "restart");
    $("#btn-reload").onclick = () => runCmd("reload", "reload");
    $("#btn-refresh-log").onclick = refreshLog;
    $("#btn-clear-log").onclick = () => { $("#log-view").textContent = ""; };
    $("#btn-load-config").onclick = loadConfig;
    $("#btn-save-config").onclick = saveConfig;

    refreshStatus();
    refreshLog();
    loadConfig();
});
```

**Step 7.5 — Extend `webroot/app.js` with tunnel subcommand handlers**

Add the following to `app.js` before the `DOMContentLoaded` handler. The
tunnel config editor uses the same `/sdcard` base64 staging pattern as the
wireproxy config editor.

```javascript
// ----- Tunnel card -----
async function refreshTunnelStatus() {
    try {
        const out = await ksu.exec("wg-user tunnel status");
        const dot = $("#tunnel-dot");
        const txt = $("#tunnel-text");
        dot.classList.remove("ok", "err");
        if (out.includes("running")) dot.classList.add("ok");
        else if (out.includes("stopped")) dot.classList.add("err");
        txt.textContent = out.trim();
    } catch (e) {
        // tunnel disabled → no error
        $("#tunnel-text").textContent = "disabled";
        $("#tunnel-dot").classList.remove("ok", "err");
    }
}

async function tunnelCmd(sub) {
    try { await ksu.exec(`wg-user tunnel ${sub}`); }
    catch (e) { console.error(e); }
    finally {
        await refreshTunnelStatus();
        await refreshStatus();
        await refreshLog();
    }
}

async function loadTunnelConfig() {
    try {
        const text = await ksu.readAssets("etc/tunnel.conf");
        $("#tunnel-editor").value = text;
        $("#tunnel-save-status").textContent = "Loaded.";
    } catch (e) {
        $("#tunnel-save-status").textContent = "Load error: " + e;
    }
}

async function saveTunnelConfig() {
    const text = $("#tunnel-editor").value;
    const stage = "/sdcard/wg-user-tunnel.conf.tmp";
    try {
        const b64 = btoa(unescape(encodeURIComponent(text)));
        await ksu.exec(
            `echo '${b64}' | base64 -d > ${stage} && ` +
            `cp ${stage} /data/adb/modules/wg-user/etc/tunnel.conf && ` +
            `rm ${stage} && wg-user reload`
        );
        $("#tunnel-save-status").textContent = "Saved & reloaded.";
        await refreshTunnelStatus();
        await refreshStatus();
    } catch (e) {
        $("#tunnel-save-status").textContent = "Save error: " + e;
    }
}
```

And add to the `DOMContentLoaded` handler:

```javascript
$("#btn-tunnel-start").onclick = () => tunnelCmd("start");
$("#btn-tunnel-stop").onclick = () => tunnelCmd("stop");
$("#btn-tunnel-restart").onclick = () => tunnelCmd("restart");
$("#btn-load-tunnel").onclick = loadTunnelConfig;
$("#btn-save-tunnel").onclick = saveTunnelConfig;
refreshTunnelStatus();
loadTunnelConfig();
```

**Step 7.6 — Static-check the WebUI**

Run: `node -c webroot/app.js 2>/dev/null || echo "no node; manual check"`
Or just `cat webroot/app.js` and visually confirm the file parses.

Visually inspect `webroot/index.html` for unclosed tags.

**Step 7.7 — Verify KSU WebUI API compatibility**

The `app.js` uses `ksu.exec(cmd)` and `ksu.readAssets(name)`. These are
documented in KSU's WebView API but the exact method names have varied
across KSU versions:

| KSU version | `exec` API | `readAssets` API |
|---|---|---|
| 10900+ | `ksu.exec(cmd): Promise<string>` | `ksu.readAssets(name): Promise<string>` |
| Older | `ksu.exec(cmd, callback)` | `ksu.readAssets(name, callback)` |

If the user is on an older KSU, `app.js` needs to wrap these in
`new Promise((resolve, reject) => ksu.exec(cmd, resolve))`. Document this
in `docs/INSTALL.md`. (The `Makefile` cannot detect the user's KSU version
at build time, so we just pin to the modern API and note the fallback.)

**Step 7.8 — Commit**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "feat(webui): tunnel card with wstunnel status/controls/config"
```

---

## Task 8: Documentation & packaging

**Files:**
- Create: `/docs/INSTALL.md`
- Create: `/docs/CONFIG.md`
- Modify: `/README.md` (replace Task 1 skeleton with full version)
- Create: `/docs/CHANGELOG.md`

**Step 8.1 — Write `docs/INSTALL.md`**

```markdown
# Installing wg-user

wg-user works under KernelSU, Magisk, and APatch. The install path is the
same (`/data/adb/modules/wg-user/`); only the install-time hook differs.

## Pre-built (recommended for most users)

1. Download the latest `wg-user-<version>.zip` from Releases.
2. Flash via your root manager:
   - KSU: KSU app → Modules → Install from storage → pick the zip.
   - Magisk: Magisk app → Modules → Install from storage → pick the zip.
   - APatch: APatch app → Modules → Install → pick the zip.
3. Reboot. (KSU's `service.sh` runs at `late_start service` — after boot.)

## Build from source

```bash
git clone https://<your-fork>/wg-user
cd wg-user
make build      # cross-compile wireproxy for 4 archs
make package    # produce dist/wg-user-<version>.zip
```

Then flash the zip as above.

## Post-install

1. Edit `etc/wireproxy.conf` with your real WG keys/endpoint. On device:
   ```sh
   adb shell su -c "vi /data/adb/modules/wg-user/etc/wireproxy.conf"
   ```
2. Reload (or just edit the file — the watcher will pick it up):
   ```sh
   adb shell su -c "wg-user reload"
   ```
3. Check status:
   ```sh
   adb shell su -c "wg-user status"
   ```
4. Point FlClash at `socks5://127.0.0.1:1080`. Add rules for the traffic
   you want to route through the WG tunnel.

## Manual testing checklist

After install, verify each of these:

- [ ] `wg-user status` shows "running"
- [ ] `wg-user log 50` shows the wireguard-go handshake log
- [ ] `adb shell su -c "echo > /dev/tcp/127.0.0.1/1080"` succeeds (port open)
- [ ] From a host with adb access, `curl --socks5-hostname 127.0.0.1:1080 https://ifconfig.me`
      shows the WG server's public IP, not your phone's
- [ ] Killing wireproxy (`wg-user stop`) and starting again (`wg-user start`) works
- [ ] Editing `etc/wireproxy.conf` triggers an auto-restart within 6 seconds
- [ ] KSU WebUI shows the status as green, log lines visible, save-config works

## KSU WebUI compatibility note

The bundled `webroot/app.js` uses the modern Promise-based KSU WebView API
(`ksu.exec`, `ksu.readAssets`). If you are on KSU < 10900, edit
`webroot/app.js` to use the callback form:

```js
function exec(cmd) {
    return new Promise((resolve, reject) => {
        ksu.exec(cmd, (out, err) => err ? reject(err) : resolve(out));
    });
}
```
```

**Step 8.2 — Write `docs/CONFIG.md`**

```markdown
# Config reference

The runtime config file is `/data/adb/modules/wg-user/etc/wireproxy.conf`.
It uses the **wireproxy INI format** — see
[`etc/wireproxy.conf.example`](../etc/wireproxy.conf.example) for a
commented template, and [the upstream README](https://github.com/windtf/wireproxy)
for the full spec.

## Hot reload

The module ships a background watcher that polls the config mtime every
5 seconds. On change, it:

1. Runs `wireproxy -n -c <config>` (configtest).
2. If configtest fails: keeps the old daemon running and logs the error.
3. If configtest passes: graceful stop (SIGINT) + restart.

You can trigger reload manually: `wg-user reload`.

## Subcommands

| Subcommand | What it does |
|---|---|
| `wg-user start` | Start the daemon. No-op if already running. |
| `wg-user stop` | Stop the daemon. No-op if not running. |
| `wg-user restart` | Stop + start. |
| `wg-user reload` | Configtest + restart (only if configtest passes). |
| `wg-user status` | Print "running" or "not running" + last log line. |
| `wg-user log [N]` | Tail the last N lines (default 50) of the log. |
| `wg-user save-config <path>` | Copy file to `etc/wireproxy.conf`, then reload. |
| `wg-user version` | Print module version. |

## Environment variables

`$WGU_RELOAD_INTERVAL` — override the 5-second watcher poll interval.

## Logs

- `log/wireproxy.log` — the wireproxy daemon's stdout/stderr (verbose by default).
- `log/watcher.log` — the config-watcher's log.

To reduce wireproxy verbosity, replace its invocation in `wg-user` with
`wg-user start -- -s` (i.e. add `-s`/`--silent`). Not exposed as a config
field to keep the wireproxy config file format-untouched.
```

**Step 8.2.1 — Write `docs/TUNNEL-SERVER.md`**

The VPS-side setup guide. The wstunnel server runs on the same VPS as
the real WireGuard daemon; the two must cooperate.

```markdown
# VPS-side setup for the obfuscation transport

This document explains how to set up the wstunnel server and the
co-located WireGuard daemon on your VPS. The result is a reachable
endpoint (port 443, TLS) that forwards to the local WireGuard UDP listener.

## Assumptions

- VPS running a modern Linux (Ubuntu 22.04+ / Debian 12+ / AlmaLinux 9+).
- You have root access on the VPS.
- You have a real domain or stable IP pointing at the VPS.
- The standard WireGuard kernel module is available (or you have
  `wireguard-go` ready).
- A TLS cert (Let's Encrypt or self-signed).

## 1. Install WireGuard server

If not already present:

```bash
# Debian / Ubuntu
apt update && apt install -y wireguard

# Alma / RHEL
dnf install -y epel-release elrepo-release
dnf install -y kmod-wireguard wireguard-tools
```

Create or copy your server's WireGuard config. Important: it must
listen on `127.0.0.1` only, NOT on the public interface. The wstunnel
server will forward to it.

```ini
# /etc/wireguard/wg0.conf
[Interface]
Address    = 10.0.0.1/24
ListenPort = 51820
PrivateKey = <SERVER_PRIVATE_KEY>

[Peer]
PublicKey = <CLIENT_PUBLIC_KEY>
AllowedIPs = 10.0.0.2/32
```

Bring it up:

```bash
systemctl enable --now wg-quick@wg0
```

## 2. Install wstunnel

```bash
# x86_64 Linux static binary (adjust version/arch)
curl -L -o /usr/local/bin/wstunnel \
    https://github.com/erebe/wstunnel/releases/download/v11.0.0/wstunnel-v11.0.0-x86_64-unknown-linux-gnu.tar.gz \
    | tar -xz -C /usr/local/bin wstunnel
chmod +x /usr/local/bin/wstunnel
```

(or use your distro's package manager if it ships wstunnel.)

## 3. TLS certificate

Use Let's Encrypt (recommended):

```bash
apt install -y certbot
certbot certonly --standalone -d your-server.com
# certificates land in /etc/letsencrypt/live/your-server.com/
```

Or a self-signed cert (testing only):

```bash
mkdir -p /etc/wstunnel
openssl req -x509 -newkey rsa:2048 -days 365 -nodes \
    -keyout /etc/wstunnel/server.key \
    -out /etc/wstunnel/server.crt \
    -subj "/CN=your-server.com"
```

## 4. Run wstunnel server

Create `/etc/systemd/system/wstunnel.service`:

```ini
[Unit]
Description=wstunnel server (for wg-user)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/wstunnel server \
    wss://0.0.0.0:443 \
    --restrict-to 127.0.0.1:51820
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
systemctl daemon-reload
systemctl enable --now wstunnel
```

## 5. (Optional) Firewall lockdown

Make sure the public UDP/51820 is not reachable. iptables:

```bash
iptables -A INPUT -p udp --dport 51820 -i lo -j ACCEPT
iptables -A INPUT -p udp --dport 51820 -j DROP
```

This allows wstunnel → 127.0.0.1:51820 but drops any public UDP/51820
attempts (so the WireGuard daemon is invisible to the public).

## 6. Configure the Android side

On the phone, in `etc/wireproxy.conf`:

```ini
[Peer]
Endpoint = 127.0.0.1:51820
```

In `etc/tunnel.conf`:

```ini
mode = wstunnel

[wstunnel]
local-udp = 127.0.0.1:51820
remote    = wss://your-server.com:443
# username = youruser
# password = yourpassword
```

`wg-user reload` and you should see in `log/wstunnel.log`:

```
[INFO] wstunnel started
```

And in `log/wireproxy.log`:

```
[INFO] handshake complete
```

## 7. Troubleshooting

- **`wstunnel: connection refused` from the client** — check the server is
  running and port 443 is open. `curl -vI https://your-server.com` from
  a host outside the VPS should return a server banner.
- **Handshake fails after 30 s** — wstunnel may be sending to a wrong
  remote. Run `wg show` on the server and verify the peer entry exists.
- **Handshake succeeds but traffic doesn't flow** — AllowedIPs in the
  server's `[Peer]` block must include the client's `10.0.0.2/32` (or
  whatever the client is using).
- **Carrier still throttles** — wstunnel defaults to TCP 443. If
  throttling persists, switch the client remote to `wqs://...` (WebSocket
  over QUIC) which uses UDP/443 instead.
```

**Step 8.3 — Update `README.md`** (extended for tunnel)

Replace the Task 1 skeleton with the full version:

```markdown
# wg-user

> Userspace WireGuard client exposed as a SOCKS5/HTTP proxy.
> A KernelSU / Magisk / APatch module.

## What it does

`wg-user` runs a userspace WireGuard client on your rooted Android device
and exposes it as a local SOCKS5 (or HTTP) proxy. It creates **no TUN
device** — the entire data path is in userspace — so it coexists with
other VPN apps (e.g. FlClash) without conflict.

Typical use: you have a WireGuard peer in your corporate intranet, and
you want some apps on your phone to reach intranet hosts while leaving
the rest of your traffic on the regular internet (or your usual VPN).

## How it works

```
[apps] → [FlClash TUN] → [rules: intranet → wg-intranet]
       → [SOCKS5 127.0.0.1:1080] → [wireproxy] → [WG peer] → [intranet]
```

wireproxy embeds `wireguard-go` + `gvisor/netstack` in one process,
exposes a SOCKS5 server, and bridges SOCKS5 TCP streams to the WG tunnel
through a fake TUN device in memory. No kernel TUN is touched.

## Install

See [`docs/INSTALL.md`](docs/INSTALL.md).

## Configure

Edit `etc/wireproxy.conf`. See [`docs/CONFIG.md`](docs/CONFIG.md) and
[`etc/wireproxy.conf.example`](etc/wireproxy.conf.example).

## WebUI

The module ships a KSU WebUI. Open the module in KSU's Modules list.

## Build from source

```bash
make build      # cross-compile wireproxy
make package    # zip for distribution
```

Requires Go 1.21+ and `zip`. Uses `GOPROXY=https://goproxy.cn,direct` by
default — override with `GOPROXY=... make build` if you need a different
proxy.

## License

ISC. See [`LICENSE`](LICENSE). Bundled wireproxy is also ISC.
```

**Step 8.4 — Write `docs/CHANGELOG.md`**

```markdown
# Changelog

All notable changes to this project are documented here.

## [0.1.0] — 2026-10-10

### Added
- Initial release.
- Multi-arch: arm64-v8a, armeabi-v7a, x86_64, x86.
- Multi-root: KernelSU, Magisk, APatch.
- SOCKS5 inbound on `127.0.0.1:1080` (configurable).
- Optional HTTP inbound.
- Hot config reload via mtime watcher (wireproxy.conf + tunnel.conf).
- KSU WebUI: status, controls, log, config editor (incl. tunnel config).
- `wg-user` POSIX-sh control CLI with `tunnel` subnamespace.
- **Optional obfuscation transport** via wstunnel v11.0.0 (WebSocket-over-TLS
  on 443). Disabled by default; enable in `etc/tunnel.conf`.

### Known limitations
- SOCKS5 UDP ASSOCIATE not supported (upstream wireproxy limitation).
- WebUI save-config uses base64 staging via `/sdcard` (one extra disk write).
- wstunnel adds ~5-15% CPU + latency overhead vs raw WG. Acceptable for
  mobile networks; users on stable WiFi may prefer `mode = off`.
- wstunnel server requires manual VPS setup (documented in
  `docs/TUNNEL-SERVER.md`).
```

**Step 8.5 — Final `make package`**

Run: `make build && make package`
Expected: `dist/wg-user-0.1.0.zip` (or `.tar.gz` if zip is missing) of size
~30–40 MB.

Run: `unzip -l dist/wg-user-0.1.0.zip | head -30`
Expected: file list including `module.prop`, `wg-user`, `service.sh`,
`common/`, `bin/arm64-v8a/wireproxy`, etc.

**Step 8.6 — Commit**

```bash
cd /var/lofibass_ssd/code/ksu/wg-user
jj describe -m "docs: install, config, README, changelog, packaging"
```

---

## Done criteria

- [ ] `make build` produces 4 wireproxy binaries (statically linked) AND
      4 wstunnel binaries (dynamically linked, Android linker)
- [ ] `make verify` shows 0 syntax errors across all `.sh` files
- [ ] `make package` produces a flashable zip
- [ ] `wg-user start / stop / restart / reload / status / log` work in the smoke test (Task 6)
- [ ] `wg-user tunnel start / stop / status` work in the smoke test
- [ ] All shell scripts are POSIX-compliant (pass `sh -n` and `dash -n`)
- [ ] No file in the repo references `/dev/net/tun`, `tun.CreateTUN`, or `wg-quick`
- [ ] No real keys in `etc/wireproxy.conf` or `etc/tunnel.conf`; documented in CONFIG.md
- [ ] `etc/wireproxy.conf.example` and `etc/tunnel.conf.example` are fully commented
- [ ] `docs/INSTALL.md` covers KSU + Magisk + APatch
- [ ] `docs/TUNNEL-SERVER.md` provides a copy-pasteable VPS setup
- [ ] `docs/ARCHITECTURE.md` (this plan's spec) matches what was built
- [ ] WebUI opens in KSU WebView and shows both wireproxy + tunnel status
      correctly (manual on device)
- [ ] Tunnel mode can be toggled at runtime by changing `mode` in `tunnel.conf`
      and running `wg-user reload` (verified in T6)
- [ ] All 12 NDK clang wrappers in `scripts/cargo-wrappers/` are committed
      with executable bit set
