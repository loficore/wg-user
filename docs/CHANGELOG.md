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
