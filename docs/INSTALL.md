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

`make webui` is opt-in (rebuilds the Preact+Tailwind WebUI; needs Node 18+).

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

The bundled `webroot/app.js` auto-detects the KSU WebView API style
(callback vs Promise). If you have an old KSU where both fail, the
buttons will show "ksu.exec unavailable" in the console — the CLI
(`wg-user ...`) is always available as a fallback.

## Uninstall

- KSU / Magisk / APatch: disable the module from the root manager.
  `uninstall.sh` runs automatically and stops the daemons.
