# Changelog

All notable changes to this project are documented here.

## [0.3.1] — 2026-11-15

### Fixed

- **Hostnames work on Android again.** The 0.3.0 client is a static
  `CGO_ENABLED=0` Go binary, so its hostname lookups use Go's pure resolver.
  Android 10+ has no `/etc/resolv.conf` and leaves `net.dns1…4` empty, so the
  resolver fell back to `[::1]:53` and **every hostname lookup failed**. A
  `[Transport] URL` such as `wss://frp-pen.com:57968` left the transport in
  `reconnecting` forever, and `[Peer] Endpoint = vpn.example.com:51820` made
  `wg-user configtest` exit 2 with
  `lookup … on [::1]:53: read udp …: connection refused`. The same configs
  with a numeric IP always worked, which is why 0.2.0 (local libc
  `getaddrinfo`) was unaffected.

  Hostnames are now resolved by `internal/resolve`: the Go resolver first, then
  a `/system/bin/ping -c 1 -W 2 <host>` fallback that parses the address out of
  `PING <host> (<addr>)` and ignores ping's exit status (ICMP is often
  blocked). Successful answers are cached for 5 minutes. This covers both the
  WebSocket dial (the TCP connection goes to the resolved IP while TLS SNI and
  the `Host` header keep the name) and `[Peer] Endpoint`, which is now resolved
  before wireproxy parses the config.

### Notes

- **A numeric IP is still the most robust choice** in `[Transport] URL` and
  `[Peer] Endpoint`. If DNS on the phone is unreliable, put the IP in the URL
  and keep the hostname in the certificate — SNI is unaffected because only the
  dialled address is substituted.
- No breaking changes: existing configs, keys, scripts and the CLI are
  unchanged. A hostname that cannot be resolved now fails the config load with
  an error that names the peer and host, instead of a silent reconnect loop.

## [0.3.0] — 2026-11-15

The single-binary release: the wstunnel client is now Go code in this
repository, the module ships one binary per ABI, and the transport
configuration moved into `etc/wireproxy.conf`.

### Breaking changes

- **`bin/<arch>/wg-user` is the only binary.** The separate
  `bin/<arch>/wireproxy` and `bin/<arch>/wstunnel` executables are gone;
  wireproxy is linked as a Go library and the wstunnel client is
  implemented in-process (`internal/wstunnel`, UDP-over-WebSocket,
  protocol-compatible with the official wstunnel **server** v11.0.0).
  There is one process, one pid file (`run/wg-user.pid`) and one log
  (`log/wg-user.log`). `run/wireproxy.pid`, `run/wstunnel.pid`,
  `log/wireproxy.log` and `log/wstunnel.log` no longer exist.
- **`etc/tunnel.conf` is gone.** The transport is configured by an
  optional `[Transport]` section in `etc/wireproxy.conf` (`Mode`, `URL`,
  `Path`, `Keepalive`, `Username`, `Password`, `Insecure`). An existing
  `tunnel.conf` is migrated automatically — see below. A missing
  `[Transport]` section means `Mode = off`.
- **`[Peer] Endpoint` changed meaning in wstunnel mode.** With
  `Mode = off` it is the WireGuard server's UDP address, as before. With
  `Mode = wstunnel` it is the address the **server** delivers to (what
  `remote-udp` used to be), normally `127.0.0.1:51820`; the public server
  address moves to `[Transport] URL`. The old `local-udp` key has no
  replacement: there is no local UDP listener any more.
- **The wstunnel server must be started with `--websocket-mask-frame`.**
  The Go client masks every WebSocket frame, as RFC 6455 requires of a
  client, and wstunnel v11 only unmasks incoming frames when that flag is
  set (it defaults to off). Without it the connection, upgrade, path and
  auth all succeed and **the data plane silently fails** — the server
  forwards still-masked payloads to UDP, so WireGuard never handshakes and
  nothing is logged as an error. 0.2.0 did not need the flag because both
  sides were the official binaries with masking off. Minimal command
  (certificate flags omitted; see `docs/TUNNEL-SERVER.md` for the full one):

  ```bash
  wstunnel server wss://0.0.0.0:443 \
      --restrict-to 127.0.0.1:51820 \
      --websocket-mask-frame
  ```

  See `docs/TUNNEL-SERVER.md`. **If the tunnel stops working after
  upgrading this module, check that flag on the VPS first.**
- **`wg-user tunnel …` is removed.** The CLI is now
  `start|stop|restart|reload|status|log [N]|save-config <path>|version`;
  `status` reports the transport from `run/state` and `log` no longer
  takes a target.
- **WebTransport (`wts://`) URLs are no longer supported.** Only `ws://`
  and `wss://` are accepted by the client, and the installer degrades an
  old `wts://` remote to `Mode = off`.

### Automatic migration (run once, by `customize.sh`)

On the first install/upgrade that finds an `etc/tunnel.conf`,
`customize.sh` inserts a `[Transport]` section into `etc/wireproxy.conf`
(before the first `[Peer]`, or at end of file if there is none) and
renames the old file to `etc/tunnel.conf.migrated` (0600; it is kept, not
deleted). If `[Transport]` already exists, nothing is rewritten; the old
file is still parked.

- `mode` → `Mode`, `remote` → `URL`, `username` → `Username`,
  `password` → `Password`; `local-udp` is dropped and `remote-udp`
  becomes `[Peer] Endpoint` in wstunnel mode (default
  `127.0.0.1:51820`).
- **`insecure` is translated by 0.2.0's rule, not as a boolean.** 0.2.0
  only verified the server certificate when the value started with
  `false`, `no` or `0`; every other value — including a missing key —
  meant "do not verify". The migration maps `false*|no*|0*` to
  `Insecure = false` and everything else, **including an absent
  `insecure` key, to `Insecure = true`**, with a warning in the install
  log. This preserves the old behaviour instead of breaking an existing
  self-signed deployment. `Insecure` is ineffective in `Mode = off`
  (there is no TLS), so the key is only written for `Mode = wstunnel`.
  For a self-signed server, the two options are: keep
  `Insecure = true` explicitly, or switch to a trusted certificate and set
  `Insecure = false`.
- **Invalid old configs degrade to `Mode = off` with a warning**, never to
  an unparsable file: a `mode` other than `off`/`wstunnel`, a `remote`
  that is not `ws://`/`wss://`, or `mode = wstunnel` without a `remote`.
  In `off` mode `[Peer] Endpoint` must then be the real WireGuard server
  address; the old value was the local listener, and the warning says so.
- Trailing comments are stripped from `mode`, `remote`, `remote-udp` and
  `insecure` before use. Credentials are copied verbatim.
- A failed rewrite aborts the install, leaving the working 0.2.0 module
  installed.
- Fresh installs seed `etc/wireproxy.conf` from the example with
  `Mode = off`, so `Insecure` is not set and keeps its default `false`:
  a new install verifies the server certificate.

### Rolling back to 0.2.0

1. Stop the 0.3.0 daemon and its reload watcher:
   `/data/adb/modules/wg-user/wg-user stop` (or disable the module in the
   root manager), then reboot — the reload watcher lives until reboot. If
   you must avoid a reboot, kill the pid in `run/watcher.pid` only after
   confirming `/proc/<pid>/cmdline` mentions `wg-user-reload` (a stale pid
   can name an unrelated process); the watcher re-runs `wg-user reload` 5 s
   after any `etc/wireproxy.conf` edit and would restart the 0.3.0 daemon
   mid-rollback.
2. Restore the old transport config:
   `mv /data/adb/modules/wg-user/etc/tunnel.conf.migrated /data/adb/modules/wg-user/etc/tunnel.conf`
3. Remove the `[Transport]` section the installer added to
   `/data/adb/modules/wg-user/etc/wireproxy.conf` (cleanup: 0.2.0 ignores
   that section).
4. Set `[Peer] Endpoint` only when the old `tunnel.conf` says
   `mode = wstunnel`. The migration rewrote it to the old `remote-udp`
   (the server-side delivery target), while 0.2.0 dials the local
   listener itself: use the loopback address and port of the old
   `local-udp` — usually `127.0.0.1:51820`; use `127.0.0.1` even if
   `local-udp` was `0.0.0.0`. With `mode = off` the migration left
   `Endpoint` alone (it is the real WireGuard server), so change nothing.
   Also a no-op when `local-udp` and `remote-udp` were the same address.
5. Flash the 0.2.0 zip over the module and reboot.

Restoring `tunnel.conf` alone is not enough in wstunnel mode: 0.2.0 binds
`local-udp` itself, so `Endpoint` must point at that loopback listener
again, and the 0.3.0 binary must be replaced (0.2.0 ignores `[Transport]`,
so removing it is cleanup). In off mode restoring `tunnel.conf` and the
0.2.0 binary is all that changes.

### Added

- In-process wstunnel client (`internal/wstunnel`): one WS binary frame
  per WireGuard datagram, JWT auth claims, basic auth, keepalive pings,
  reconnect with 0.5 s → 30 s exponential backoff (±20% jitter), and a
  hard failure on TLS certificate errors instead of an endless retry.
- `internal/wgbind`, a `conn.Bind` over the transport: no local socket, no
  port, no start-order dependency, one fewer kernel copy.
- `[Transport] URL` / `Path` / `Keepalive` / `Username` / `Password` /
  `Insecure` knobs; `Path` defaults to `v1`, `Keepalive` to 25 s.
- `run/state` transport snapshot consumed by `wg-user status` and the
  WebUI: `pid`, `transport`, `transport_state`
  (`connected|connecting|reconnecting|closed`, or `off`), `transport_detail`,
  `updated`.
- Interop test (`make interop`, run in CI) that round-trips datagrams
  through the official, sha256-pinned wstunnel v11.0.0 server started with
  `--websocket-mask-frame`.
- `make build-go` (CI compatibility alias for `make build`) and
  `test/shell/build_test.sh`, which asserts all four binaries exist and
  the amd64 one runs.

### Changed

- **Module zip 34.8 MB → 14.8 MB** (14.15 MiB): four `bin/<arch>/wg-user`
  binaries at roughly 9 MB each, instead of a ~9 MB Go binary plus an
  ~11 MB third-party binary per ABI.
- One Go cross-compile replaces the second build pipeline; CI now runs the
  full build and the interop test on every pull request, not only on
  release.
- `service.sh` only starts the daemon and the `etc/wireproxy.conf` content
  watcher; the reload path is `wg-user reload` after `configtest`.
- `common/abi.sh::select_binary` replaces
  `select_wireproxy_binary` and installs `bin/<arch>/wg-user` as
  `bin/wg-user`.
- The WebUI has one config editor card and a transport line on the status
  card; the tunnel card is gone.

### Fixed

- The local UDP loopback hop and its restart race (wstunnel had to bind
  127.0.0.1:51820 before wireproxy started, and a restart could collide
  with the old socket) no longer exist.
- A dead transport is visible in `wg-user status` instead of requiring a
  log read; `run/state` is only trusted when its `pid` matches the live
  daemon.
- `wg-user save-config` no longer restarts the daemon itself: it validates
  the staged file with `configtest` and writes it, and the reload watcher
  performs the one restart ~5 s later. Every WebUI save used to stop+start
  the daemon twice, and two overlapping restarts could leave a dead pid in
  `run/wg-user.pid`. A file that fails `configtest` is now rejected before
  it replaces the live config.
- The installer warns when a 0.2.x `wireproxy.conf` has more than one
  `[Peer]` and `Mode = wstunnel`, instead of migrating "successfully" into
  a config the daemon refuses on every boot (it requires exactly one peer).

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
