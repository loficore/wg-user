# wg-user

> Userspace WireGuard client exposed as a SOCKS5/HTTP proxy.
> A KernelSU / Magisk / APatch module.

**Why?** When you already run FlClash in TUN mode but want a separate
WireGuard tunnel for some apps (e.g., intranet), this module gives you
a local SOCKS5 endpoint that routes through WG — without creating a TUN
device and without interfering with FlClash's TUN. On lossy mobile
networks, an optional wstunnel layer wraps WG in WebSocket-over-TLS to
defeat carrier UDP throttling.

## Quick start (after installing the module)

1. Edit the WG config:
   ```sh
   adb shell su -c "vi /data/adb/modules/wg-user/etc/wireproxy.conf"
   ```
2. Reload:
   ```sh
   adb shell su -c "wg-user reload"
   ```
3. Point your client at `socks5://127.0.0.1:1080`.

## Documentation

- Architecture: [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)
- Install: [`docs/INSTALL.md`](docs/INSTALL.md)
- Config reference: [`docs/CONFIG.md`](docs/CONFIG.md)
- Tunnel server (VPS): [`docs/TUNNEL-SERVER.md`](docs/TUNNEL-SERVER.md)
- Changelog: [`docs/CHANGELOG.md`](docs/CHANGELOG.md)
