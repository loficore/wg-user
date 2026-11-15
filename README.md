# wg-user

[![CI](https://github.com/loficore/wg-user/actions/workflows/ci.yml/badge.svg)](https://github.com/loficore/wg-user/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/loficore/wg-user)](https://github.com/loficore/wg-user/releases/latest)

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

One Go binary, one process, one config file:

```
[apps] → [FlClash TUN] → [rules: intranet → wg-intranet]
       → [SOCKS5 127.0.0.1:1080] → [wg-user: gvisor netstack + wireguard-go]
       → [Transport] → [WG peer] → [intranet]
```

`wg-user` links [wireproxy](https://github.com/windtf/wireproxy) v1.1.3 as
a Go library: `wireguard-go` + `gvisor/netstack` + the SOCKS5/HTTP
servers all run in this one process, bridged through an in-memory TUN.
No kernel TUN is touched.

`[Transport]` picks how the WireGuard UDP datagrams leave the process:

- **`Mode = off`** (default) — the kernel sends them straight to
  `[Peer] Endpoint`.
- **`Mode = wstunnel`** — an in-process client
  (`internal/wstunnel`) wraps each datagram in one WebSocket binary
  frame over TLS/443, indistinguishable from normal HTTPS to carrier
  DPI. It speaks the wstunnel v11 UDP-over-WebSocket protocol, so the
  **server side is the unmodified official `wstunnel server`** (pinned
  at v11.0.0).

```
[apps] → [FlClash TUN] → [SOCKS5 127.0.0.1:1080] → [wg-user]
       → [WS frames over WSS :443] → [wstunnel server] → [WG :51820] → [intranet]
```

There is no local UDP listener and no second process any more, so no
start-order dependency and no port race. The old `etc/tunnel.conf` is
gone; the transport is a `[Transport]` section in `etc/wireproxy.conf`,
and the installer migrates an existing `tunnel.conf` automatically.

> **Server requirement:** the VPS-side `wstunnel server` must be started
> with `--websocket-mask-frame`. Without it the handshake and auth
> succeed but no datagram ever gets through. See
> [`docs/TUNNEL-SERVER.md`](docs/TUNNEL-SERVER.md).

## Install

See [`docs/INSTALL.md`](docs/INSTALL.md). The module zip is ~15 MB
compressed: four `bin/<arch>/wg-user` binaries, ~9 MB each uncompressed.

## Configure

`etc/wireproxy.conf` is the only config file:

```ini
[Peer]
PublicKey  = <base64>
# With Mode = off this is the WireGuard server's UDP address; see the table below.
Endpoint   = your-server.example.com:51820
AllowedIPs = 10.66.0.0/24

[Transport]
# off (the default) or wstunnel.
Mode = off
# URL = wss://your-server.example.com:443
# Insecure = true
```

`[Peer] Endpoint` means two different things depending on the mode:

| Mode | `Endpoint` is | Used by |
|---|---|---|
| `off` | the WireGuard server's UDP address, dialled from the phone | the kernel |
| `wstunnel` | the address the **server** delivers to (must fall inside its `--restrict-to`), normally `127.0.0.1:51820` | the in-process transport |

See [`docs/CONFIG.md`](docs/CONFIG.md) and
[`etc/wireproxy.conf.example`](etc/wireproxy.conf.example) for the full
reference, and [`docs/TUNNEL-SERVER.md`](docs/TUNNEL-SERVER.md) for the
VPS side.

## CLI

`/data/adb/modules/wg-user/wg-user <subcommand>` (the module directory is
not on `PATH`):

| Subcommand | What it does |
|---|---|
| `start` | Start the daemon in the background (`run/wg-user.pid`, log to `log/wg-user.log`). No-op if already running. |
| `stop` | SIGTERM, wait up to 20 s, then SIGKILL. |
| `restart` | Stop + start. |
| `reload` | `wg-user configtest` on `etc/wireproxy.conf`; restart only if it passes. |
| `status` | Print pid, the last log line, and the transport line from `run/state`; exit 1 when stopped. |
| `log [N]` | Tail N lines of `log/wg-user.log` (default 50). |
| `save-config <path>` | Validate `<path>` with `configtest`, then copy it to `etc/wireproxy.conf` (0600). Restart happens via the reload watcher (within ~5 s), not here; an invalid file is rejected and the live config is left untouched. |
| `version` | Print the module version from `module.prop`. |

The binary itself has three subcommands: `run` (the default), `configtest`
and `version`. `wg-user tunnel ...` no longer exists.

The daemon writes `run/state` on every transport change:

```
pid=1234
transport=wstunnel
transport_state=connected
transport_detail=wss://your-server.example.com:443
updated=1763000000
```

`transport_state` is one of `connected`, `connecting`, `reconnecting`,
`closed`; with `Mode = off` it is `off`.

## WebUI

The module ships a Preact + TypeScript + Tailwind WebUI (bilingual:
English / 中文, switchable in the header). Open the module in your root
manager's Modules list. Status, controls, log, and config editor are all
available from the WebUI; the CLI (`wg-user ...`) is always the source
of truth for the API surface.

## Build from source

```bash
make build       # cross-compile bin/<arch>/wg-user for 4 arches
make webui       # rebuild the WebUI (pnpm + tsc + esbuild + tailwind)
make package     # build + webui + zip for distribution
make package-only  # re-zip without the cross-compiles
make test        # go vet + go test
make interop     # protocol test against the real wstunnel v11.0.0 server
make verify      # shell syntax + CLI/install tests
```

Requires:

- **Go 1.26.x** (`.tool-versions` pins 1.26.5; `go.mod` declares
  `go 1.26.0`). That is the only compiler: the transport client is Go
  source in this repository, and nothing is built but `wg-user`.
- Node 26+ (pinned in `.tool-versions`) and pnpm 11+ (pinned in
  `webui/package.json`'s `packageManager`) for `make webui`.
- `zip` (or the build falls back to `tar.gz`).

`make build` expects an x86_64 Linux build host: `test/shell/build_test.sh`
executes `bin/x86_64/wg-user` to check the amd64 build. On another host,
`ARCH=<arch> scripts/build.sh` cross-compiles one target without that check.

The Go build uses `GOPROXY=https://goproxy.cn,direct` by default — override
with `GOPROXY=... make build` if you need a different proxy.

`make interop` needs the pinned official server binary at
`bin/interop/wstunnel` (or `WSTUNNEL_SERVER_BIN`); the target prints the
exact download and sha256 commands when it is missing.

## CI and releases

Pull requests run `.github/workflows/ci.yml`:

| Job | What it checks |
|---|---|
| `scripts` | `make verify`; no real config (`etc/wireproxy.conf` / `etc/tunnel.conf*`) is tracked; `module.prop` and `webui/package.json` versions agree |
| `webui` | `pnpm install --frozen-lockfile` → `tsc --noEmit` → esbuild → headless smoke test, then fails if `webroot/` is stale |
| `build-go` | `go vet` + `go test`, the wstunnel v11.0.0 interop gate (binary pinned by sha256), then cross-compiles all four arches |

Pushing a `v*` tag runs `.github/workflows/release.yml`, which cross-compiles
the four binaries, builds the WebUI, zips the module and publishes
`wg-user-<version>.zip` plus `SHA256SUMS` to a GitHub Release. The tag must
equal `v$(version)` from `module.prop`.

Cut a release:

```bash
V=$(sed -n 's/^version=//p' module.prop)

# after bumping module.prop (version + versionCode) and webui/package.json:
make webui                      # rebuild the committed bundle
jj describe -m "release: $V"
jj bookmark set main -r @
jj git push --bookmark main     # -> CI run on main
jj tag set "v$V" -r main
jj git push --tag "v$V"         # -> Release run
```

Run the two pushes in the order shown: `jj git push --bookmark main` updates
the branch, then `jj git push --tag "v$V"` sends the tag. Each command pushes
only the refs named on its command line, and re-running either is a no-op when
nothing changed.

`workflow_dispatch` on the release workflow runs the same build without
creating a release — useful for verifying the toolchain.

## Contributing

`main` is protected:

- direct pushes are rejected (including for the owner);
- a pull request only becomes mergeable once all three CI jobs are green
  **and** the branch is up to date with `main`;
- force-pushes and branch deletion are disabled.

```bash
jj new main                        # start from main
# ...edit...
jj bookmark create my-change -r @
jj git push --bookmark my-change
gh pr create --base main --head my-change --fill
```

Tags are not covered by branch protection, so pushing a release tag
directly is still the release trigger.

## License

ISC. See [`LICENSE`](LICENSE). The linked wireproxy library is also ISC;
the in-process transport is protocol-compatible with wstunnel v11 (ISC).
No third-party binaries are bundled.
