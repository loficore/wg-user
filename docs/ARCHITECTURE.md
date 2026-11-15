# wg-user — Architecture & Design

> **Status:** 0.3.0 design (single binary). The approved design document is
> `docs/superpowers/specs/2026-11-15-wg-single-binary-design.md`.

## 1. Goal

Build a KSU module that:

1. Runs an **automatic background WireGuard client** at boot (`late_start service`).
2. Exposes that WireGuard tunnel as a **userspace SOCKS5 / HTTP proxy** on `127.0.0.1`.
3. **Creates no TUN device** — pure userspace, no kernel WireGuard needed.
4. Is **multi-root compatible** (KernelSU, Magisk, APatch).
5. Supports **multi-architecture** (arm64-v8a, armeabi-v7a, x86_64, x86).
6. Supports **hot config reload** (file watcher + restart).
7. Provides a **KSU WebUI** for status, control, and config editing.
8. Ships an **optional obfuscation transport** for the WG UDP path: WG
   traffic wrapped in WebSocket-over-TLS (default port 443) to defeat
   carrier UDP throttling / blocking common on mobile networks in China.

0.3.0 adds one structural constraint to those goals: **everything is one
Go binary and one process**. SOCKS5/HTTP (wireproxy, linked as a library),
WireGuard, and the WebSocket transport all live in `bin/<arch>/wg-user`.

## 2. Why a SOCKS5/HTTP proxy, not a TUN device

The user runs **FlClash** in TUN mode on the device. FlClash captures all app traffic
and routes it via its own ruleset. We want intranet traffic to be tunneled through
a separate WireGuard link — without interfering with FlClash's `tun0`.

Two paths were considered:

| Approach | Pros | Cons | Verdict |
|---|---|---|---|
| TUN + wg0 + redsocks | High perf, kernel WG | Needs TUN, may collide with FlClash tun0, requires extra SELinux rules | ✗ |
| SOCKS5 from in-process wireguard-go + gvisor netstack | No TUN, no kernel WG dependency, no FlClash collision | Slightly lower perf; no SOCKS5 UDP (yet) | ✓ |

**Chosen approach:** the second. We use **`wireproxy` v1.1.3** (ISC) as a
Go library: it embeds `wireguard-go` and `gvisor/netstack`, exposes a fake
TUN in-process, and speaks SOCKS5/HTTP to local clients.

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
                 │  wg-user: gvisor netstack + wireguard-go │
                 │  one process, one binary (UID 0)         │
                 │  ↑ NOT in FlClash VPN allowed-UIDs       │
                 └────────────────┬─────────────────────────┘
                                  │ WireGuard UDP datagram
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  conn.Bind selected by [Transport] Mode  │
                 │                                          │
                 │  off:      kernel UDP socket             │
                 │            → [Peer] Endpoint             │
                 │                                          │
                 │  wstunnel: in-process client            │
                 │            no local socket; each datagram│
                 │            is one WS binary frame        │
                 └────────────────┬─────────────────────────┘
                                  │ (Mode = wstunnel) TCP 443, TLS
                                  ▼
                 ┌─────────────────────────────────────────┐
                 │  wstunnel server v11.0.0 on the VPS      │
                 │  --websocket-mask-frame (required)       │
                 │  forwards to its --restrict-to target    │
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

`Mode = off` (the default) stops at the kernel socket: WG datagrams go
straight to `[Peer] Endpoint`, exactly like 0.2.0's wireproxy. `Mode =
wstunnel` replaces that socket with an in-process WebSocket connection.

**Why FlClash and wg-user don't interfere:**

- FlClash's VpnService installs UID-scoped routing rules. UID 0 (root daemon) is
  not in any application's allowed-UID set, so FlClash's tun0 does NOT see
  wg-user's outbound UDP or TCP.
- FlClash dials `127.0.0.1:1080`. The SOCKS5 connection is local; it never hits
  FlClash's TUN. From FlClash's perspective, this is a regular local socket.
- wg-user creates no TUN, so in `off` mode the kernel sees only its outbound UDP
  socket to the WG peer. Routing is by the kernel's main table → physical
  interface.
- In `wstunnel` mode the only socket is an outbound TCP connection on port 443;
  even if FlClash's VpnService captured UID 0, TCP 443 is normally DIRECT in
  FlClash's default rules (and the user can add an explicit rule for it).

## 4. Components

| Component | File(s) | Role |
|---|---|---|
| Module metadata | `module.prop`, `customize.sh`, `sepolicy.rule` | KSU / Magisk / APatch installation hooks |
| Autostart | `service.sh` | Invoked at `late_start`; starts the daemon, then the reload watcher |
| Control CLI | `wg-user` (POSIX sh) | start / stop / restart / reload / status / log / save-config / version |
| Shared shell lib | `common/util.sh`, `common/abi.sh` | Logging, ABI detection, `select_binary` (each script resolves its own path inline) |
| Daemon | `bin/wg-user` (~9 MB per arch) | WireGuard + gvisor netstack + SOCKS5/HTTP + the transport, in one process |
| Config | `etc/wireproxy.conf`, `etc/wireproxy.conf.example` | WG keys, peer, inbounds, `[Transport]` |
| Hot reload | watcher written by `service.sh` | Polls `etc/wireproxy.conf` mtime; restarts on change |
| WebUI | `webroot/index.html`, `app.js`, `style.css` | Status / control / log / config editor |
| Build | `Makefile`, `scripts/build.sh` | Cross-compile `wg-user` for 4 architectures |

Go packages (`cmd/wg-user`, `internal/…`):

| Package | Role |
|---|---|
| `internal/config` | Reads `etc/wireproxy.conf`: `wireproxy.ParseConfig` for the sections it owns, plus the `[Transport]` section parsed here |
| `internal/wstunnel` | UDP-over-WebSocket client (dial, JWT, reconnect, keepalive) |
| `internal/wgbind` | `conn.Bind` implementation: one `Send` = one WS frame, no local socket |
| `internal/device` | netstack TUN + wireguard-go device + `wireproxy.VirtualTun` (port of upstream `StartWireguard`, with the bind injected) |
| `internal/state` | Atomic `run/state` writer/reader |

## 5. Multi-root compatibility

| Root | Detection | `service.sh` invocation | Path |
|---|---|---|---|
| KernelSU | `$KSU_API` non-empty | `service.sh` is run by KSU at `late_start service` | `$MODPATH` |
| Magisk | `$MAGISK_VER` non-empty | `service.sh` is run by Magisk at `service.d` | `$MODDIR` |
| APatch | `$APATCH` non-empty | `service.sh` is run by APatch | `$MODPATH` |

All three root solutions use the same path: `/data/adb/modules/wg-user/`. Each
shipped script resolves its own directory from `$0` with `dirname` (`wg-user`,
`service.sh`, `uninstall.sh`). `service.sh` falls back to the fixed module path
when `$0` is relative, `uninstall.sh` to `$MODPATH` and then the current
directory (`$MODDIR` is its own near-empty variable, not a root-manager one),
and `customize.sh` prefers `$MODPATH` because the installer sources it.

`customize.sh` is the only install-time hook (KernelSU / Magisk "modern" path
and APatch). It calls `common/abi.sh::select_binary` to copy
`bin/<arch>/wg-user` to `bin/wg-user` for the device's ABI, restores
executable bits (the installer extracts everything as 0644), seeds
`etc/wireproxy.conf`, and runs the `tunnel.conf` migration described in §7.4.
Without `bin/wg-user` the install aborts: there is no second binary to fall
back to.

## 6. Runtime model: one daemon, one supervisor loop

`bin/wg-user run -c etc/wireproxy.conf` (started by `wg-user start`) is the
only long-lived process. Its shape:

1. **Signals.** `SIGINT`/`SIGTERM` cancel the root context.
2. **Config.** `config.Load` parses `etc/wireproxy.conf` and validates
   `[Transport]` before anything is brought up. A bad file exits 2.
3. **Transport selection.** `off` → `conn.NewDefaultBind()`; `wstunnel` →
   one `wstunnel.Client` plus `wgbind.New(client)`. The bind is fixed at
   device construction, so a mode change needs a restart — which is what the
   watcher does.
4. **Device.** `device.Create` builds the netstack TUN, the wireguard-go
   device and `wireproxy.VirtualTun`, then calls `dev.Up()`.
5. **Inbounds.** `cfg.WireProxy.Routines` (SOCKS5, HTTP, …) are spawned as
   goroutines, one per configured inbound.
6. **Transport supervisor.** In `wstunnel` mode `client.Run` loops:
   `connecting` → dial (10 s timeout) → `connected` → wait for the connection
   to end or a keepalive tick → back off → retry. Backoff starts at 500 ms,
   doubles, caps at 30 s, with ±20% jitter. A TLS certificate error is
   **permanent**: no retry, the process logs it and exits 1.
7. **State poller.** Once a second, `client.State()` is compared with the
   last written value; `run/state` is rewritten only on a change.
8. **Shutdown.** After the context ends (or the supervisor fails):
   `Dev.Close()` → stop the supervisor → `client.Close()` → wait for `Run`
   → close the poller → remove `run/state` → exit 0 (or 1 on a transport
   error). The order matters: the device's receive routines must stop before
   the transport they read from disappears.

Goroutines in steady state: one per inbound routine, wireguard-go's own
device routines, the transport supervisor + its reader, and the state poller.
There is no second process, no local UDP listener, and no start-order
dependency between components.

Exit codes: `0` clean, `1` runtime/transport error, `2` usage or config
error.

The shell CLI keeps the process boundary thin:

| Subcommand | Behaviour |
|---|---|
| `start` | `nohup bin/wg-user run -c etc/wireproxy.conf > log/wg-user.log 2>&1 &`, writes `run/wg-user.pid` (temp + rename), sets `oom_score_adj=-1000`, re-checks liveness after 1 s |
| `stop` | SIGTERM, poll up to 20 s, then SIGKILL; refuses to signal a pid whose `/proc/<pid>/cmdline` is not this module's daemon |
| `restart` | stop + start |
| `reload` | `bin/wg-user configtest -c etc/wireproxy.conf`; restart only if it exits 0 |
| `status` | pid + `transport: <detail> (<state>)`; `run/state` is only trusted when its `pid` matches the live pid |
| `log [N]` | `tail -n N log/wg-user.log` (default 50) |
| `save-config <path>` | `configtest -c <path>`; only on success copy to `etc/wireproxy.conf` (0600). Does not restart — the reload watcher applies the file within ~5 s |
| `version` | `version=` from `module.prop` |

## 7. Transport layer (in-process)

### 7.1 Protocol

The client speaks the wstunnel v11 UDP-over-WebSocket protocol (`internal/wstunnel`),
so the server side stays the unmodified official binary:

- **Upgrade URL**: `{ws|wss}://<host>:<port>/<Path>/events`, `Path` default
  `v1`. Any path in the configured `URL` is replaced, not appended to.
- **Headers**: `Upgrade: websocket`, `Connection: upgrade`,
  `Sec-WebSocket-Version: 13`, `Sec-WebSocket-Key`, and
  `Sec-WebSocket-Protocol: v1, authorization.bearer.<jwt>`. With both
  `Username` and `Password` set, `Authorization: Basic …` is added.
- **JWT**: HS256, signed with a per-process random key. The server does not
  verify the signature; it reads the claims: `id`, `p = {"Udp":{"timeout":null}}`,
  `r` = `[Peer] Endpoint` host, `rp` = its port. Those two are what the
  server is allowed to deliver to.
- **Frames**: one WS binary frame is exactly one UDP datagram, in both
  directions. Frames over 64 KiB are treated as a protocol error and the
  connection is dropped.
- **Keepalive**: `Ping` every `Keepalive` seconds (0 disables); incoming
  `Ping` is answered with `Pong`; `Close` ends the connection.
- **One connection at a time.** Reconnects produce a new server-side UDP
  source port; WireGuard re-handshakes and the server roams to the new port,
  so nothing server-side needs to change.

### 7.2 The mask flag is a server requirement

The client masks every frame, as RFC 6455 requires of a client (gorilla does
this unconditionally). The official server only unmasks incoming frames when
it was started with `--websocket-mask-frame`, which defaults to **off**.

Without the flag: the TCP connection, the upgrade, the path and the JWT all
succeed, and the server forwards the still-masked payload to UDP — every
datagram arrives XORed with the 4-byte mask key. WireGuard never handshakes,
and no error is logged anywhere. This is why the flag is called out in
`docs/TUNNEL-SERVER.md`, in the CHANGELOG upgrade notes, and hard-coded in
the interop test's server command.

The flag only affects client→server frames; the server does not mask its own
replies, so the Go client accepts them.

### 7.3 State file contract

`internal/state` owns `run/state`, a `key=value` file the shell CLI and the
WebUI read. Written atomically (`run/state.tmp` → `chmod 0644` → `rename`),
removed on clean exit, and rewritten only when the transport state changes:

```
pid=1234
transport=wstunnel
transport_state=connected
transport_detail=wss://your-server.example.com:443
updated=1763000000
```

| Key | Values |
|---|---|
| `pid` | the daemon's pid; readers require it |
| `transport` | `off` or `wstunnel` (the `[Transport] Mode`) |
| `transport_state` | `connected` \| `connecting` \| `reconnecting` \| `closed`; `off` only when `transport=off` |
| `transport_detail` | free-form, normally the wstunnel URL or `udp`; never contains a newline |
| `updated` | Unix seconds |

`Write` rejects a `transport_detail` with a newline (it would forge an extra
key). `Read` ignores unknown keys and lines without `=`, so the format can
grow. `wg-user status` only trusts the file when `pid` matches the live
daemon; a file left behind by a crash is ignored, and the daemon removes it
on a normal exit. The WebUI renders `closed` and `off` as "not enabled".

### 7.4 Installer migration

`customize.sh` folds a 0.2.x `etc/tunnel.conf` into `etc/wireproxy.conf` once,
on the first install/upgrade that finds one:

| Old (`tunnel.conf`) | New (`wireproxy.conf`) |
|---|---|
| `mode = off` / `wstunnel` | `[Transport] Mode` |
| `remote = wss://…` | `[Transport] URL` |
| `username`, `password` | `Username`, `Password` |
| `insecure` | `Insecure`, translated by 0.2.0's rule (below) |
| `remote-udp` | `[Peer] Endpoint` (wstunnel mode only; default `127.0.0.1:51820`) |
| `local-udp` | dropped — there is no local listener any more |

Rules and failure modes:

- **Skip if already migrated.** If `etc/wireproxy.conf` already contains a
  `[Transport]` section, nothing is rewritten and the old file is still
  parked.
- **Unknown `mode`** (anything other than `off`/`wstunnel`) degrades to
  `Mode = off` with a warning: 0.2.0 treated it as off, but 0.3.0's parser
  rejects unknown modes, and rejecting the file would brick the daemon.
- **Non-`ws://`/`wss://` URL** (for example `wts://`, WebTransport) degrades
  to `Mode = off` with a warning. `Mode = wstunnel` without a URL does the
  same. The warning says that in `off` mode `[Peer] Endpoint` must be the
  real WireGuard server: the old value was the local listener.
- **`insecure` translation.** 0.2.0 only verified the certificate when the
  value started with `false`, `no` or `0`; every other value — and a missing
  key — meant "do not verify". 0.3.0's `Insecure` defaults to `false`
  (verify), so the migration maps `false*|no*|0*` → `Insecure = false` and
  everything else, including an absent key → `Insecure = true`, with a
  warning when the key was absent or empty. That preserves the old
  behaviour instead of silently breaking a self-signed deployment.
- **Trailing comments** are stripped from `mode`, `remote`, `remote-udp`
  and `insecure` before use (the old reader kept the rest of the line).
  Credentials are copied verbatim.
- **The old file is kept**, never deleted: renamed to
  `etc/tunnel.conf.migrated` (0600) when it lives in the new module
  directory, or copied to `etc/tunnel.conf.migrated` when it belongs to the
  currently installed module (that directory is replaced at the next boot).
  This is the rollback path.
- **A failed rewrite aborts the install** (exit 1) so the working 0.2.x
  module stays in place. Continuing would leave `Mode = off` with the old
  local `Endpoint`, i.e. WG traffic pointed back at the phone.

A fresh install seeds `etc/wireproxy.conf` from the example (`Mode = off`)
and never creates a `tunnel.conf`.

## 8. Hot reload design

wireproxy does not implement SIGHUP-based config reload. `service.sh`
therefore writes a small watcher (`wg-user-reload`, `run/watcher.pid`) that:

1. Records the mtime of `etc/wireproxy.conf` on startup.
2. Every 5 seconds (override with `$WGU_RELOAD_INTERVAL`) compares it.
3. On change, waits 1 s (debounce), then runs `wg-user reload`.
4. `wg-user reload` runs `bin/wg-user configtest`; on failure the running
   daemon is left alone and the error is logged. On success it restarts the
   daemon.

There is no reload path for `[Transport]` in particular: a transport change
is an edit to `etc/wireproxy.conf`, and the restart rebuilds the client from
the new file. Since the bind is chosen before the device comes up, this is
the only correct sequence.

`uninstall.sh` stops the watcher before the daemon (otherwise the watcher
could restart the daemon against a directory that is about to be deleted),
then stops the daemon through the CLI, then removes `run/wg-user.pid`,
`run/watcher.pid` and `run/state`.

## 9. WebUI design

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

- **Status**: green/red dot plus a one-line summary parsed from the text
  of `wg-user status`, including the transport line (`transport: wss://…
  (connected)`).
- **Controls**: Start / Stop / Restart / Reload buttons →
  `ksu.exec('/data/adb/modules/wg-user/wg-user <cmd>')`
- **Logs**: tail the log file via
  `ksu.exec('/data/adb/modules/wg-user/wg-user log 50')`
- **Config editor**: `<textarea>` populated via
  `cat /data/adb/modules/wg-user/etc/wireproxy.conf`, saved via
  `wg-user save-config <staged-file>`. On save, the CLI validates the file
  and writes it; the reload watcher restarts the daemon within ~5 s (so one
  save is one restart).

There is no separate tunnel card: the transport is part of the one config
file and its live state is part of the status card.

The WebUI is **optional** — the module works without it. The CLI (`wg-user`)
is the source of truth for the API surface; the WebUI is a thin shim.

Sources live in `webui/` (Preact + TypeScript + Tailwind, bundled by
esbuild, `pnpm run check` runs `tsc --noEmit` first). Strings are
internationalised through `webui/src/i18n/` — `en.ts` is the source of
truth for the key set, every other catalogue is typed against it, so a
missing translation breaks the build.

## 10. Build & distribution

One pipeline, one toolchain:

- `scripts/build.sh` cross-compiles this repository (`./cmd/wg-user`) with
  `GOOS=linux GOARCH=<arm64|arm|amd64|386> CGO_ENABLED=0 go build -trimpath
  -ldflags "-s -w -X main.version=$VERSION"` into `bin/<arch>/wg-user`
  (~9 MB each). `test/shell/build_test.sh` asserts all four exist and that
  the amd64 binary runs.
- `make webui` rebuilds the Preact bundle into `webroot/` (pnpm install,
  `tsc --noEmit`, esbuild, headless smoke test).
- `make package` = `build` + `check-bins` + `webui`, then zips the module to
  `dist/wg-user-<version>.zip` (~15 MB). `zip` missing falls back to
  `tar.gz`.
- `make package-only` re-zips without the cross-compiles.
- `make clean` removes `bin/*/wg-user`, `bin/interop` and `dist/`. It does
  **not** touch `webroot/`: the bundle is committed and shipped.
- `make verify` syntax-checks every shell script with its shebang
  interpreter, then runs the CLI and installer suites in `test/shell/`.
- `make test` = `go vet ./... && go test ./...`.
- `make interop` starts the pinned official `wstunnel` v11.0.0 server with
  `--restrict-http-upgrade-path-prefix v1 --websocket-mask-frame` and round-trips
  datagrams through it. The binary is scrubbed from `bin/interop/`; the
  target prints the download + sha256 commands when it is missing.

Packaging carries `module.prop`, the shell scripts, `common/`, `webroot/`,
the four `bin/<arch>` directories, and the docs. It carries only
`etc/wireproxy.conf.example` — real configs hold the WireGuard private key.

## 11. Out of scope (YAGNI)

- **SOCKS5 UDP ASSOCIATE**: wireproxy does not support it (TODO in upstream).
  For UDP-based intranet traffic, the user must add a fallback. Note: the
  obfuscation layer (wstunnel) wraps WG's own UDP, so WG reaches the server
  intact — the SOCKS5 UDP limitation is unrelated.
- **A Go reimplementation of the wstunnel server**: the official binary
  stays, pinned at v11.0.0.
- **Multiple simultaneous transports / multiple `[Peer]` in wstunnel mode**:
  one transport targets exactly one peer (enforced by `internal/config`).
  Users who need more can use a SOCKS5→sing-box→mihomo stack.
- **Kernel WireGuard mode**: deliberately avoided to keep the module
  dependency-free and prevent TUN conflicts.
- **Automatic WG config sync from FlClash**: out of scope. The user manages
  the WG config independently; FlClash only sees the SOCKS5 endpoint.
- **Other obfuscation transports** (udp2raw, gost, …): only the wstunnel
  protocol is implemented.
- **QUIC/WebTransport (`wqs://`)**: explicit non-goal for 0.3.0; the
  installer degrades such an old URL to `Mode = off`.
- **In-process config hot reload**: the mtime watcher + restart is enough.
- **Shipping only arm architectures**: a size decision for later.

## 12. Risks & mitigations

| Risk | Likelihood | Mitigation |
|---|---|---|
| Server started without `--websocket-mask-frame` | Med | Documented as a hard requirement in `docs/TUNNEL-SERVER.md` and the CHANGELOG; the interop test always passes the flag; symptom ("handshake works, no traffic") is the first troubleshooting entry |
| wstunnel server upgrades and changes the private handshake/frame format | Low | VPS version pinned at v11.0.0; CI interop gate against that exact binary (sha256-pinned); upgrade only after the gate passes |
| The custom `conn.Bind` gets a wireguard-go semantic wrong | Low | Interop test with real datagrams; `Mode = off` as the control path; device-side acceptance checklist; `Close` is generation-scoped and recoverable |
| Config migration writes an `Endpoint` that is the phone's local listener | Med | Migration rewrites `Endpoint` to the old `remote-udp` (default `127.0.0.1:51820`) in wstunnel mode, degrades invalid configs to `off` with a warning, and aborts the install if the rewrite fails |
| Migration silently lowers TLS security | Med | `insecure` is translated by 0.2.0's prefix rule, not by boolean; an absent key becomes `Insecure = true` with a warning, and the CHANGELOG tells self-signed users how to keep or drop it |
| Upstream wireproxy API changes | Low | `go.mod` pins v1.1.3; CI compiles and unit-tests every change |
| Go 1.26 toolchain requirement | Low | `.tool-versions` pins 1.26.5; CI and release use `go-version: 1.26.x` |
| Reconnect changes the server-side UDP source port | Low | WireGuard re-handshakes; the server roams to the new port; device acceptance step 3 covers it |
| SOCKS5 UDP needed by some intranet app | Low | Document; recommend fallback to direct or L2TP/SS for UDP |
| Daemon killed by LMK | Low | `oom_score_adj = -1000` set in `wg-user start` |
| Conf mtime changes during a reload | Low | Watcher debounces by 1 s before re-reading |
| Stale PID file after a crash | Med | `running_pid` validates `/proc/<pid>/cmdline` before signalling; `run/state` is only trusted when its pid matches |
| WebUI exposes keys via cleartext | Med | Document that WebUI traffic is local; user should not expose the port publicly |
| Source download blocked in CN | Med | `GOPROXY=https://goproxy.cn,direct` is the build default |
| Single binary is still ~9 MB per arch | Med | Expected (~15 MB zip); the four-arch payload is the cost of the module supporting every ABI |
