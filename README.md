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

```
[apps] → [FlClash TUN] → [rules: intranet → wg-intranet]
       → [SOCKS5 127.0.0.1:1080] → [wireproxy] → [WG peer] → [intranet]
```

wireproxy embeds `wireguard-go` + `gvisor/netstack` in one process,
exposes a SOCKS5 server, and bridges SOCKS5 TCP streams to the WG tunnel
through a fake TUN device in memory. No kernel TUN is touched.

For mobile networks with UDP throttling, an **optional** wstunnel layer
wraps the WG UDP path in WebSocket-over-TLS on port 443, which is
indistinguishable from normal HTTPS to carrier DPI.

```
[apps] → [FlClash TUN] → [SOCKS5 127.0.0.1:1080] → [wireproxy] → [wstunnel] → [WSS :443] → [wstunnel] → [WG :51820] → [intranet]
```

## Install

See [`docs/INSTALL.md`](docs/INSTALL.md).

## Configure

- `etc/wireproxy.conf` — WG keys, peer, SOCKS5/HTTP bind.
- `etc/tunnel.conf` — obfuscation transport (optional, wstunnel).

See [`docs/CONFIG.md`](docs/CONFIG.md) and the `*.example` files for
the full reference.

## WebUI

The module ships a Preact + TypeScript + Tailwind WebUI (bilingual:
English / 中文, switchable in the header). Open the module in your root
manager's Modules list. Status, controls, log, and config editor are all
available from the WebUI; the CLI (`wg-user ...`) is always the source
of truth for the API surface.

## Build from source

```bash
make build       # cross-compile wireproxy (Go) + wstunnel (Rust) for 4 archs
make webui       # rebuild the WebUI (pnpm + tsc + esbuild + tailwind)
make package     # zip for distribution
make package-only  # re-zip without the slow cross-compiles
```

Requires:
- Go 1.21+ (for wireproxy)
- Rust 1.7x+ + rustup + 4 Android targets + Android NDK r26+ (for wstunnel)
- Node 26+ and pnpm 11+ (for `make webui`; versions are pinned in
  `.tool-versions` for asdf and `webui/packageManager`)
- `zip` (or the build falls back to `tar.gz`)

The Go build uses `GOPROXY=https://goproxy.cn,direct` by default — override
with `GOPROXY=... make build` if you need a different proxy.

## CI and releases

Pull requests run `.github/workflows/ci.yml`:

| Job | What it checks |
|---|---|
| `scripts` | `sh -n` on every script, NDK wrappers executable, no real `etc/*.conf` tracked |
| `webui` | `pnpm install --frozen-lockfile` → `tsc --noEmit` → esbuild → headless smoke test, then fails if `webroot/` is stale |
| `build-go` | cross-compiles wireproxy for all four arches |

The Rust cross-compile is too slow for PRs, so it only runs on release.

Pushing a `v*` tag runs `.github/workflows/release.yml`, which builds
wireproxy and wstunnel for all four arches, builds the WebUI, zips the
module and publishes `wg-user-<version>.zip` plus `SHA256SUMS` to a
GitHub Release. The tag must equal `v$(version)` from `module.prop`.

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

Push the branch and the tag **separately**: a single `git push` that
carries both refs only delivers the branch event, so CI runs and the
release silently does not.

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

ISC. See [`LICENSE`](LICENSE). Bundled wireproxy is also ISC.
Bundled wstunnel is also ISC.
