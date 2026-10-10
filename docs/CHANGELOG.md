# Changelog

All notable changes to this project are documented here.

## [0.2.0] — 2026-11-14

The WebUI release: it now actually works on KernelSU 3.x.

### Fixed
- **WebUI / KernelSU 3.x bridge.** `ksu.readAssets` was removed from the
  manager years ago, and `ksu.exec(cmd)` in 3.3.1 returns *only the last
  line* of output, so every card showed a fragment (`Load error`, empty
  log, or a green status with no text). The bridge now uses the
  `exec(cmd, optionsJson, callbackName)` overload (verified against the
  decompiled `WebViewInterface` of `me.weishu.kernelsu` v3.3.1), which
  delivers the complete stdout/stderr. File reads use
  `cat /data/adb/modules/wg-user/...`; a 30 s watchdog turns a dead
  bridge into a visible error instead of a permanent "checking…".
- **WebUI invoked bare `wg-user`.** The module directory is not on
  `PATH`, so every command exited 127 with empty stdout. All commands now
  use absolute paths and append `2>&1`.
- **WebUI status detection** parses the CLI text
  (`wireproxy: running`) instead of guessing from exit codes, and the
  tunnel card distinguishes `running` / `stopped` / `disabled`.
- **`service.sh` exec bit** is lost when KernelSU extracts the module
  (files land as 0644), so the module never started. `customize.sh` now
  restores modes explicitly (`fix_mode`).
- **wstunnel v11 CLI**: positional server URL, `-L 'udp://BIND:PORT:HOST:PORT?timeout_sec=0'`,
  `--http-upgrade-credentials` for basic auth, `remote-udp` config key,
  working `insecure` (certificate verification off).
- **Tunnel bind race** right after a restart: `spawn_tunnel()` retries once.
- Example config narrows `AllowedIPs` to the WireGuard subnet
  (`10.66.0.0/24`) so the module does not become a default route.

### Added
- **i18n**: English and Chinese catalogues with a header switch,
  persisted in `localStorage`; missing translations are a compile error.
- **TypeScript** sources (`webui/src/*.ts(x)`) with `pnpm run typecheck`
  (`tsc --noEmit`, strict) in front of the esbuild bundle.
- **Headless smoke test** (`webui/test/smoke.mjs`) that runs the built
  bundle against a fake `ksu` bridge in both languages.
- **pnpm** as the package manager (`pnpm-lock.yaml`), Node 26 pinned in
  `.tool-versions` for asdf and `webui/packageManager`.
- `make package-only` — re-zip the module without the slow cross-compiles.

### Security
- The distributed zip no longer contains `etc/wireproxy.conf` or
  `etc/tunnel.conf` — a real config carries the WireGuard private key and
  the server address. Only the `*.example` files ship. `customize.sh`
  keeps an existing config across reinstalls and otherwise seeds one from
  the example (with a warning), so a fresh install still boots.

### Changed
- `Makefile: webui` uses pnpm and runs the typecheck + smoke-test gate.
- WebUI save-config stages through a dotfile in the module directory
  instead of `/sdcard`, so it no longer depends on shared storage.
- Removed the stale `webroot/module.prop` (nothing reads it).
- `.gitignore`: anchor the `src/` rule as `/src/` — it was accidentally
  ignoring `webui/src/`, so the WebUI source had never been committed.

## [0.1.0] — 2026-10-10

### Added
- Initial release.
- Multi-arch: arm64-v8a, armeabi-v7a, x86_64, x86.
- Multi-root: KernelSU, Magisk, APatch.
- SOCKS5 inbound on `127.0.0.1:1080` (configurable).
- Optional HTTP inbound.
- Hot config reload via mtime watcher (wireproxy.conf + tunnel.conf).
- KSU WebUI: Preact + Tailwind, status / controls / log / config editor
  (incl. tunnel config).
- `wg-user` POSIX-sh control CLI with `tunnel` subnamespace.
- Build pipeline:
  - `scripts/build.sh` — cross-compile wireproxy (Go) for 4 archs.
  - `scripts/cargo-cross.sh` — cross-compile wstunnel (Rust) for 4 archs.
  - 12 NDK clang wrappers under `scripts/cargo-wrappers/`.
  - `webui/` (Preact + Tailwind) → `webroot/` build via esbuild.
- **Optional obfuscation transport** via wstunnel v11.0.0
  (WebSocket-over-TLS on 443). Disabled by default; enable in
  `etc/tunnel.conf`.

### Fixed
- `install.sh` / `customize.sh` / `uninstall.sh`: resolve the module root
  from `$MODPATH` / `$MODDIR`, falling back to `$PWD`, instead of
  `${0%/*}`. The installer sources these scripts (`. ./install.sh`), so
  `$0` is the shell name (`sh`) and `${0%/*}` returned `sh`, making the
  `. "$sh/common/util.sh"` source line fail with
  "can't open 'sh/common/util.sh'". (Reported by the first device install.)
- `install.sh` / `customize.sh`: use `log_info`, not `log` — the shared
  `common/util.sh` defines `log_info` / `log_warn` / `log_err`, and an
  undefined `log` aborted the install (with `set -e`) at the last line.

### Known limitations
- SOCKS5 UDP ASSOCIATE not supported (upstream wireproxy limitation).
- WebUI save-config uses base64 staging via `/sdcard` (one extra disk
  write). Could be avoided with a KSU `ksu.writeAssets` API if it
  becomes available.
- wstunnel adds ~5-15% CPU + latency overhead vs raw WG. Acceptable for
  mobile networks; users on stable WiFi may prefer `mode = off`.
- wstunnel server requires manual VPS setup (documented in
  `docs/TUNNEL-SERVER.md`).
- The Rust build (wstunnel) downloads ~500 MB of crates on first run;
  subsequent builds use the local cargo cache.
