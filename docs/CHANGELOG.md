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
