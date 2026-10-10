# wg-user — Architecture & Design

> **Status:** Approved design. Implementation follows `docs/superpowers/plans/2026-10-10-wg-user-module.md`.

## 1. Goal

Build a KSU module that:

1. Runs an **automatic background WireGuard client** at boot (`late_start service`).
2. Exposes that WireGuard tunnel as a **userspace SOCKS5 / HTTP proxy** on `127.0.0.1`.
3. **Creates no TUN device** — pure userspace, no kernel WireGuard needed.
4. Is **multi-root compatible** (KernelSU, Magisk, APatch).
5. Supports **multi-architecture** (arm64-v8a, armeabi-v7a, x86_64, x86).
6. Supports **hot config reload** (file watcher + graceful restart).
7. Provides a **KSU WebUI** for status, control, and config editing.
8. **Optional obfuscation transport** for the WG UDP path: wraps WG traffic in
   WebSocket-over-TLS (default port 443) to defeat carrier UDP throttling /
   blocking common on mobile networks in China.

## 2. Why a SOCKS5/HTTP proxy, not a TUN device

The user runs **FlClash** in TUN mode on the device. FlClash captures all app traffic
and routes it via its own ruleset. We want intranet traffic to be tunneled through
a separate WireGuard link — without interfering with FlClash's `tun0`.

Two paths were considered:

| Approach | Pros | Cons | Verdict |
|---|---|---|---|
| TUN + wg0 + redsocks | High perf, kernel WG | Needs TUN, may collide with FlClash tun0, requires extra SELinux rules | ✗ |
| SOCKS5 from in-process wireguard-go + gvisor netstack | No TUN, no kernel WG dependency, no FlClash collision | Slightly lower perf; no SOCKS5 UDP (yet) | ✓ |

**Chosen approach:** the second. We use **`wireproxy`** (Go, MIT-style) which
embeds `wireguard-go` and `gvisor/netstack` in one binary, exposes a fake TUN
in-process, and speaks SOCKS5/HTTP to local clients.

## 3. Data flow

```
                 ┌─────────────────────────────────────────┐
                 │  User-installed apps (any UID)            │
                 └────────────────┬─────────────────────────┘
                                  │ outbound TCP
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  FlClash (TUN mode, VpnService)          │
                 │  rules:                                  │
                 │    IP-CIDR,10.0.0.0/8  → wg-intranet     │
                 │    IP-CIDR,172.16.0.0/12 → wg-intranet   │
                 │    IP-CIDR,192.168.0.0/16 → wg-intranet  │
                 │    MATCH               → DIRECT          │
                 └────────────────┬─────────────────────────┘
                                  │ SOCKS5 CONNECT 10.0.0.x:443
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  127.0.0.1:1080 (SOCKS5)                 │
                 │  ↑ wireproxy (root daemon, UID 0)        │
                 │  ↑ NOT in FlClash VPN allowed-UIDs       │
                 └────────────────┬─────────────────────────┘
                                  │ TCP, gvisor netstack
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  in-process "fake TUN" (channel)         │
                 │  in-process wireguard-go                 │
                 │  encrypts with WG keys                   │
                 └────────────────┬─────────────────────────┘
                                  │ UDP to 127.0.0.1:51820   (or remote if no tunnel)
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  [OPTIONAL] wstunnel (root daemon)       │
                 │  listens 127.0.0.1:51820, forwards via   │
                 │  WebSocket-over-TLS to remote server     │
                 │  on port 443 (indistinguishable from     │
                 │  normal HTTPS to carrier DPI)            │
                 └────────────────┬─────────────────────────┘
                                  │ TCP 443 over public internet
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  wstunnel server on VPS (your-server:443)│
                 │  forwards to 127.0.0.1:51820 of VPS      │
                 └────────────────┬─────────────────────────┘
                                  │ UDP
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  Real WireGuard daemon on VPS:51820      │
                 │  decrypts, routes to intranet            │
                 └────────────────┬─────────────────────────┘
                                  │
                                  ▼
                          Intranet resource
```

**Why FlClash and wireproxy don't interfere:**

- FlClash's VpnService installs UID-scoped routing rules. UID 0 (root daemon) is
  not in any application's allowed-UID set, so FlClash's tun0 does NOT see
  wireproxy's outbound UDP packets.
- FlClash dials `127.0.0.1:1080`. The SOCKS5 connection is local; it never hits
  FlClash's TUN. From FlClash's perspective, this is a regular local socket.
- wireproxy creates no TUN, so the kernel sees only its outbound UDP socket to
  the WG peer. Routing is by the kernel's main table → physical interface.
- wstunnel is a separate process. Its outbound TCP is on port 443; even if
  FlClash's VpnService captured UID 0, TCP 443 is normally DIRECT in FlClash's
  default rules (and the user can add an explicit rule for it).

## 4. Components

| Component | File(s) | Role |
|---|---|---|
| Module metadata | `module.prop`, `customize.sh`, `sepolicy.rule` | KSU / Magisk / APatch installation hooks |
| Autostart | `service.sh` | Invoked at `late_start`; starts wstunnel (if enabled) then wireproxy; runs hot-reload watcher |
| Control CLI | `wg-user` (POSIX sh) | start / stop / restart / reload / status / log + `tunnel` subnamespace |
| Shared shell lib | `common/util.sh`, `common/abi.sh` | Logging, path resolution, env detection, arch detection |
| WireGuard client | `bin/wireproxy` (multi-arch, ~9 MB) | In-process wireguard-go + gvisor netstack + SOCKS5 server |
| Obfuscation transport | `bin/wstunnel` (multi-arch, ~11 MB) | Optional WebSocket-over-TLS wrapper for the WG UDP path |
| WireGuard config | `etc/wireproxy.conf`, `etc/wireproxy.conf.example` | WG keys, peer, SOCKS5 bind |
| Tunnel config | `etc/tunnel.conf`, `etc/tunnel.conf.example` | `mode = off \| wstunnel` + remote URL, auth |
| Hot reload | watcher in `service.sh` | Polls `etc/wireproxy.conf` AND `etc/tunnel.conf` mtime; restarts on change |
| WebUI | `webroot/index.html`, `app.js`, `style.css` | Status / control / log / config editor; tunnel card |
| Build (Go) | `Makefile`, `scripts/build.sh` | Cross-compile wireproxy for 4 architectures (Go, trivial) |
| Build (Rust) | `scripts/cargo-cross.sh` | Cross-compile wstunnel for 4 architectures (Rust + NDK clang wrappers) |

## 5. Multi-root compatibility

| Root | Detection | `service.sh` invocation | Path |
|---|---|---|---|
| KernelSU | `$KSU_API` non-empty | `service.sh` is run by KSU at `late_start service` | `$MODPATH` |
| Magisk | `$MAGISK_VER` non-empty | `service.sh` is run by Magisk at `service.d` | `$MODDIR` |
| APatch | `$APATCH` non-empty | `service.sh` is run by APatch | `$MODPATH` |

All three root solutions use the same path: `/data/adb/modules/wg-user/`. The
module's runtime code resolves its own directory from `${0%/*}` to be robust
against env-var differences.

`customize.sh` is KSU-specific (runs at install time, can read device ABI
and copy the right binary). `install.sh` is Magisk-specific (same purpose, but
called by `module_install` from the Magisk framework). Both delegate to
`common/abi.sh::select_wireproxy_binary` so the logic lives in one place.

## 6. Hot reload design

wireproxy does not implement SIGHUP-based config reload (it accepts only
SIGINT/SIGQUIT for graceful shutdown). To get hot reload, we wrap the daemon
with a small watcher that:

1. Records the mtime of `etc/wireproxy.conf` and `etc/tunnel.conf` on startup.
2. Every 5 seconds (configurable), polls both files' mtime.
3. If either changed, runs `wireproxy -n -c <config>` to validate the new
   wireproxy config, and (if the tunnel config changed) also configtest the
   tunnel by re-parsing `etc/tunnel.conf`.
4. On configtest pass, sends SIGINT (graceful) to the current wstunnel (if
   running) and wireproxy; waits for exit; re-execs `wg-user start` which
   restarts them in the right order (wstunnel first, then wireproxy).

The watcher runs as a background POSIX shell loop inside `service.sh`. It is
killed on `wg-user stop` to ensure clean shutdown.

Reload ordering is critical: **wstunnel must be up before wireproxy starts**,
because wireproxy's WG endpoint may point at wstunnel's local UDP listener.
`service.sh` enforces this by starting wstunnel first, waiting for its
listener to bind (sleep 0.3 + retry connect), then starting wireproxy.

## 7. Obfuscation transport (optional)

Mobile carriers (especially in CN) commonly throttle or block UDP traffic on
mobile networks, which destroys WireGuard's default UDP transport. To fix this,
the module ships an **optional** obfuscation layer: **wstunnel** (Rust,
github.com/erebe/wstunnel, v11.0.0+).

### How it integrates

The user edits `etc/wireproxy.conf` so the WG endpoint is **local**:

```ini
[Peer]
Endpoint = 127.0.0.1:51820      # was: 1.2.3.4:51820 (the real server)
```

And enables the tunnel in `etc/tunnel.conf`:

```ini
mode = wstunnel

[wstunnel]
local-udp = 127.0.0.1:51820
remote    = wss://your-server.com:443
# auth = user:password            # optional basic auth
```

At startup, `service.sh`:

1. Reads `etc/tunnel.conf`. If `mode = off`, skips the wstunnel step entirely.
2. Otherwise, runs `bin/wstunnel client ...` which:
   - Listens on UDP `local-udp` (127.0.0.1:51820)
   - Forwards every packet through a WebSocket-over-TLS tunnel to `remote`
   - On the server, `wstunnel server` accepts the WSS, forwards to its local
     UDP 51820 (where the real WireGuard daemon listens)
3. After wstunnel is listening, starts `bin/wireproxy`, whose WG handshake
   now flows: `wireguard-go` → UDP 127.0.0.1:51820 → wstunnel → WSS → server
   → wg daemon.

### Why wstunnel

- **HTTPS-on-443 indistinguishable from real web traffic** to carrier DPI
- **Single binary, both client and server** (one tool on the VPS too)
- **Small** (11 MB stripped per arch, vs 50 MB for gost) — important for the
  KSU module download
- **Active development** (v11.x in 2026); v6.0 was the last Go release and is
  far too old to use
- The cross-compile cost (Rust + NDK clang wrapper) is one-time, baked into
  `scripts/cargo-cross.sh`; users do not interact with it

### Why not other tools

| Tool | Why not |
|---|---|
| `udp2raw` | Requires raw socket + iptables on the server; "fake TCP" can be detected by advanced DPI |
| `gost` (Go) | Same Go toolchain simplicity, but 4.5× larger binary (200 MB zip vs 45 MB) |
| `sing-box` | More general-purpose; would replace wireproxy entirely — bigger refactor |
| Native WG obfuscation | Not available; would require patching wg-go |

### Server setup (out of module scope)

Documented in `docs/TUNNEL-SERVER.md`. One-command VPS setup:

```bash
# On the VPS (x86_64 Linux)
wstunnel server wss://0.0.0.0:443 --restrict-to 127.0.0.1:51820
# (and have a normal wireguard daemon listening on 127.0.0.1:51820)
```

## 8. WebUI design

KSU exposes a WebView in which modules under `webroot/` are loaded. The page
communicates with the daemon via the `ksu` global object. On KernelSU 3.x
(the versions in the wild today) the bridge is:

- `ksu.exec(cmd)` — synchronous, but returns **only the last line** of output
- `ksu.exec(cmd, optionsJson, callbackName)` — async; calls
  `window[callbackName](exitCode, stdout, stderr)` with the **complete**
  output. This is the overload the WebUI uses.
- `ksu.spawn(cmd, ...)` — run async, returns a process handle
- `ksu.toast(msg)` — a native toast
- `ksu.moduleInfo` — module metadata

`ksu.readAssets()` no longer exists; module files are read with
`cat /data/adb/modules/wg-user/<file>` through `ksu.exec`.

UI sections:

- **Status**: green/red dot plus a one-line summary, parsed from the text
  of `wg-user status`.
- **Controls**: Start / Stop / Restart / Reload buttons →
  `ksu.exec('/data/adb/modules/wg-user/wg-user <cmd>')`
- **Logs**: tail the log file via `ksu.exec('wg-user log 50')`
- **Config editor**: `<textarea>` populated via
  `cat /data/adb/modules/wg-user/etc/wireproxy.conf`, saved via
  `wg-user save-config`. On save, the CLI reloads the daemon.

The WebUI is **optional** — the module works without it. The CLI (`wg-user`)
is the source of truth for the API surface; the WebUI is a thin shim.

Sources live in `webui/` (Preact + TypeScript + Tailwind, bundled by
esbuild, `pnpm run check` runs `tsc --noEmit` first). Strings are
internationalised through `webui/src/i18n/` — `en.ts` is the source of
truth for the key set, every other catalogue is typed against it, so a
missing translation breaks the build.

## 9. Build & distribution

Two build pipelines, both triggered by `make build`:

### Pipeline 1: wireproxy (Go)

`scripts/build.sh` cross-compiles `github.com/windtf/wireproxy/cmd/wireproxy@v1.1.3`
for the 4 architectures with `CGO_ENABLED=0 GOOS=linux`. Output: `bin/<arch>/wireproxy`
(~9 MB each). Build time: ~3 min from scratch, < 30s incremental.

### Pipeline 2: wstunnel (Rust)

`scripts/cargo-cross.sh` cross-compiles `github.com/erebe/wstunnel@v11.0.0`
(`wstunnel-cli` crate) for the 4 Android targets using NDK 26.x clang wrappers.
Required Rust targets: `aarch64-linux-android`, `armv7-linux-androideabi`,
`x86_64-linux-android`, `i686-linux-android`. Output: `bin/<arch>/wstunnel`
(~11 MB each). Build time: ~5 min from scratch, < 30s incremental.

### Packaging

- `make package` — zip the module into `dist/wg-user-<version>.zip` (or
  `.tar.gz` if zip is unavailable) ready to flash via KSU/Magisk/APatch
  module installer. Final size ~80-90 MB.
- `make clean` — remove build artifacts.
- `make verify` — run `sh -n` over all scripts, cross-compile sanity, and
  `wireproxy -n` configtest on a sample config.

## 9. Out of scope (YAGNI)

- **SOCKS5 UDP ASSOCIATE**: wireproxy does not support it (TODO in upstream).
  For UDP-based intranet traffic, the user must add a fallback. This is
  acceptable for the user's stated "intranet" use case, which is mostly TCP.
  Note: the obfuscation layer (wstunnel) wraps all UDP, so WG's UDP reaches
  the server intact — the SOCKS5 UDP limitation is unrelated.
- **Multiple simultaneous tunnels**: one `[Peer]` per config; we do not chain
  or load-balance. Users who need this can use a SOCKS5→sing-box→mihomo stack.
- **Kernel WireGuard mode**: deliberately avoided to keep the module
  dependency-free and prevent TUN conflicts.
- **Automatic WG config sync from FlClash**: out of scope. The user manages
  the WG config independently; FlClash only sees the SOCKS5 endpoint.
- **Supporting multiple obfuscation transports** (udp2raw, gost, etc.):
  Only wstunnel is bundled. The config schema could be extended later to
  allow pluggable transports (`mode = udp2raw` etc.), but YAGNI for now.
- **TCP-over-TCP performance tuning**: wstunnel defaults are good enough.
  Users with severe TCP-over-TCP issues can switch to wstunnel's QUIC mode
  (`wss` → `wqs`).

## 10. Risks & mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| `landlock` syscall unavailable on Android kernel | Med | wireproxy uses `BestEffort() + IgnoreIfMissing()`; verified safe in code |
| SOCKS5 UDP needed by some intranet app | Low | Document; recommend fallback to direct or L2TP/SS for UDP |
| Wireproxy process killed by LMK | Low | `oom_score_adj = -1000` set in `wg-user start` |
| Conf mtime changes during a reload | Low | Watcher debounces by 1s before re-reading |
| Stale PID file after crash | Med | `wg-user start` overwrites PID; `start` always re-checks process exists |
| WebUI exposes keys via cleartext | Med | Document that WebUI traffic is local; user should not expose port publicly |
| Source download blocked in CN | Med | Use `GOPROXY=https://goproxy.cn,direct` (Go) and cargo's default registry (Rust) |
| wstunnel starts before bind, wireproxy fails to dial | Med | `service.sh` waits for wstunnel UDP listener to be reachable before starting wireproxy (max 5 retries, 200ms apart) |
| wstunnel's wss server unreachable on cellular (firewall) | Med | Documented; user can switch to QUIC (`wqs://`) for better fallback |
| Rust build requires network (cargo registry, source download) | Med | First build downloads ~500 MB of crates; subsequent builds use cache. Document; users in restricted networks can pre-fetch or vendor. |
| NDK clang version mismatch with rust target | Low | Pin to NDK 26.x in `scripts/cargo-cross.sh`; test on multiple NDK versions is in T8 |
| wstunnel server TLS cert self-signed | Low | Document use of Let's Encrypt or `--restrict-to` mode for testing |
