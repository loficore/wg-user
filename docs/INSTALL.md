# Installing wg-user

wg-user works under KernelSU, Magisk, and APatch. The install path is the
same (`/data/adb/modules/wg-user/`); only the install-time hook differs.

## Pre-built (recommended for most users)

1. Download the latest `wg-user-<version>.zip` from Releases (~15 MB).
2. Flash via your root manager:
   - KSU: KSU app → Modules → Install from storage → pick the zip.
   - Magisk: Magisk app → Modules → Install from storage → pick the zip.
   - APatch: APatch app → Modules → Install → pick the zip.
3. Reboot. (KSU's `service.sh` runs at `late_start service` — after boot.)

The module contains one binary per ABI (`bin/<arch>/wg-user`). There is no
second process and no second binary to install.

## Upgrading from 0.2.0

Flash the 0.3.0 zip over the installed module. Nothing else is required on
the phone: `customize.sh` keeps your `etc/wireproxy.conf`, folds an
existing `etc/tunnel.conf` into its `[Transport]` section, and renames the
old file to `etc/tunnel.conf.migrated` (0600).

What to check in the install log:

- `migrated …/etc/tunnel.conf into etc/wireproxy.conf ([Transport] Mode = …)`
  — the normal path. `[Peer] Endpoint` was rewritten to the old
  `remote-udp` target (default `127.0.0.1:51820`), which is what the
  server should deliver to.
- `… is neither 'off' nor 'wstunnel'; migrating as 'off'`, or
  `… is not ws:// or wss:// …; migrating as 'off'` — the transport was
  disabled. In that case `[Peer] Endpoint` must be changed to the real
  WireGuard server address before the tunnel can work.
- `has no 'insecure' value … so Insecure = true is written` — 0.2.0 did
  not verify certificates when the key was absent, and the migration
  preserves that. Set `Insecure = false` in `etc/wireproxy.conf` if your
  server has a trusted certificate.

**Server-side check (0.3.0 only).** The official `wstunnel server` on the
VPS must be started with `--websocket-mask-frame`. 0.2.0 did not need it;
without it the 0.3.0 client's handshake succeeds and no traffic flows. The
full command is in [`TUNNEL-SERVER.md`](TUNNEL-SERVER.md#4-run-the-wstunnel-server).

### Where the old config went

`/data/adb/modules/wg-user/etc/tunnel.conf.migrated`. It is kept (not
deleted) so you can read the old values, and so you can roll back.

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

## Build from source

```bash
git clone https://<your-fork>/wg-user
cd wg-user
make build      # cross-compile bin/<arch>/wg-user for 4 arches
make package    # build + WebUI + dist/wg-user-<version>.zip
```

`make build` needs:

- **Go 1.26.x** (`.tool-versions` pins 1.26.5; `go.mod` declares
  `go 1.26.0`) — the only compiler. There is no second toolchain and no
  Android SDK component to install: `scripts/build.sh` runs
  `CGO_ENABLED=0 GOOS=linux GOARCH=… go build` and nothing else.

`make build` expects an x86_64 Linux build host: `test/shell/build_test.sh`
executes the amd64 binary. On another host, `ARCH=<arch> scripts/build.sh`
cross-compiles one target without that check.

`make webui` rebuilds the WebUI (Preact + TypeScript + Tailwind via
pnpm/esbuild). It is part of `make package`; needs Node 26+ (pinned in
`.tool-versions`) and pnpm 11+ (pinned in `webui/package.json`'s
`packageManager`). `make package-only` re-zips without the
cross-compiles. `make test` runs `go vet` + `go test`; `make interop`
round-trips datagrams through the pinned official wstunnel v11.0.0 server
(the target prints the download commands when the binary is missing).

Then flash the zip as above.

## Post-install

The zip ships only `etc/wireproxy.conf.example`. A real config contains
your WireGuard private key, so it is never packaged. On install,
`customize.sh` keeps an existing `etc/wireproxy.conf` (upgrade), migrates
an existing `etc/tunnel.conf` into it, or copies the example into place
(fresh install); either way you must fill in your own keys.

1. Edit `etc/wireproxy.conf` with your real WG keys/endpoint. On device:
   ```sh
   adb shell su -c "vi /data/adb/modules/wg-user/etc/wireproxy.conf"
   ```
2. Reload (or just edit the file — the watcher will pick it up):
   ```sh
   adb shell su -c "/data/adb/modules/wg-user/wg-user reload"
   ```
3. Check status:
   ```sh
   adb shell su -c "/data/adb/modules/wg-user/wg-user status"
   # wg-user: running (pid …)
   # transport: udp (off)      # Mode = off
   # transport: wss://… (connected)   # Mode = wstunnel
   ```
   The `# ` at the start of those lines is this snippet's comment marker, not
   part of the output, and the real lines also start with a
   `<timestamp> INFO  wg-user: ` prefix; both are elided here for readability.
4. Point FlClash at `socks5://127.0.0.1:1080`. Add rules for the traffic
   you want to route through the WG tunnel.

The module directory is not on `PATH`; use the absolute path (or
`su -c "cd /data/adb/modules/wg-user && ./wg-user status"`).

### Optional: enable the wstunnel transport

For mobile networks with heavy UDP throttling. Set up the VPS first
([`TUNNEL-SERVER.md`](TUNNEL-SERVER.md)), then on the phone:

```ini
# etc/wireproxy.conf
[Peer]
# What the SERVER forwards to, not the VPS's public address.
Endpoint = 127.0.0.1:51820

[Transport]
Mode     = wstunnel
URL      = wss://your-server.example.com:443
# true only for a self-signed server.
Insecure = false
```

```sh
adb shell su -c "/data/adb/modules/wg-user/wg-user reload"
```

`[Peer] Endpoint` semantics and every `[Transport]` key:
[`CONFIG.md`](CONFIG.md).

### Manual testing checklist

After install, verify each of these:

- [ ] `wg-user status` shows "running"
- [ ] With `Mode = wstunnel`, the transport line shows `(connected)`
- [ ] `wg-user log 50` shows the wireguard-go handshake log
- [ ] Traffic through `socks5://127.0.0.1:1080` reaches an intranet host
      (from a phone shell with `curl`:
      `curl --socks5-hostname 127.0.0.1:1080 http://10.66.0.1/`)
- [ ] `ps -A | grep wg-user` shows one daemon process (its own `grep` line
      aside)
- [ ] With `Mode = wstunnel`, `netstat -lunp 2>/dev/null | grep 51820` shows
      no local listener (the loopback UDP hop is gone)
- [ ] Airplane mode for 10 s and back: `transport_state` returns to
      `connected` within ~30 s and traffic resumes
- [ ] `wg-user stop` + `wg-user start` works
- [ ] Editing `etc/wireproxy.conf` triggers an auto-restart within ~6 s
- [ ] KSU WebUI shows the status as green, log lines visible, save-config
      works, no tunnel card

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
  `uninstall.sh` runs automatically: it stops the reload watcher, stops the
  daemon, and removes `run/wg-user.pid`, `run/watcher.pid` and `run/state`.
- `etc/tunnel.conf.migrated` is not touched by the uninstall hook, but the
  whole module directory goes away with the module. Copy that file
  elsewhere **before** uninstalling if you may want the 0.2.0 rollback
  described above.
