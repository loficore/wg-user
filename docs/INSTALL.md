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
make build      # cross-compile wireproxy (Go) + wstunnel (Rust) for 4 archs
make package    # produce dist/wg-user-<version>.zip
```

`make build` needs:
- Go 1.21+ (for wireproxy)
- Rust 1.7x+ with rustup + the 4 Android targets (for wstunnel)
- Android NDK r26+ (NDK clang wrappers in `scripts/cargo-wrappers/`)

`make webui` rebuilds the WebUI (Preact + TypeScript + Tailwind via
pnpm/esbuild). It is part of `make package`; needs Node 26+ and pnpm 11+
(pinned in `.tool-versions`). `make package-only` re-zips without the slow
cross-compiles.

Then flash the zip as above.

## Post-install

The zip ships only `etc/wireproxy.conf.example` and `etc/tunnel.conf.example`.
A real config contains your WireGuard private key, so it is never packaged.
On install, `customize.sh` keeps an existing `etc/*.conf` (upgrade) or copies
the example into place (fresh install); either way you must fill in your own
keys.

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

### Optional: enable wstunnel obfuscation

For mobile networks with heavy UDP throttling. See `docs/TUNNEL-SERVER.md`
for the VPS-side setup, then on the phone:

```sh
adb shell su -c "vi /data/adb/modules/wg-user/etc/tunnel.conf"
# set: mode = wstunnel
# set: remote = wss://your-server.com:443
# also change etc/wireproxy.conf's [Peer] Endpoint to 127.0.0.1:51820
adb shell su -c "wg-user reload"
```

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

Verified against KernelSU manager **v3.3.1** (`me.weishu.kernelsu`). The
bridge tries, in order: the 3-argument `exec(cmd, '{}', cb)` (complete
stdout/stderr — the one KSU 3.x needs), the synchronous one-argument
`exec(cmd)` (older builds, last line only), and the very old
`exec(cmd, cb)` form. If none answer within 30 s the card shows
`ksu bridge timeout` instead of hanging on "checking…".

The CLI (`wg-user ...`) is always available as a fallback.

## Uninstall

- KSU / Magisk / APatch: disable the module from the root manager.
  `uninstall.sh` runs automatically and stops the daemons.
