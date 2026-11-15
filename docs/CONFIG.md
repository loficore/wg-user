# Config reference

All runtime configuration lives in **one file**,
`/data/adb/modules/wg-user/etc/wireproxy.conf`:

- `wireproxy.conf` — WireGuard keys, peer, SOCKS5/HTTP bind, and the
  optional `[Transport]` section. **Edit this.**
- `wireproxy.conf.example` — annotated template (do not edit; for
  reference).

`etc/tunnel.conf` does not exist in 0.3.0. An existing one is migrated
into `[Transport]` at install time and renamed
`etc/tunnel.conf.migrated` (see [Migration](#migration-from-020) below).

The file uses the **wireproxy INI format** — see
[`etc/wireproxy.conf.example`](../etc/wireproxy.conf.example) for a
commented template, and [the upstream README](https://github.com/windtf/wireproxy)
for the sections wireproxy owns (`[Interface]`, `[Peer]`, `[Socks5]`,
`[HTTP]`, `[Resolve]`, `[TCPClientTunnel]`, …). This document covers the
whole file at a summary level and `[Transport]` in full.

## Minimal config

```ini
[Interface]
Address    = 10.66.0.11/32
PrivateKey = <base64>
DNS        = 10.66.0.1
MTU        = 1280

[Peer]
PublicKey  = <base64>
Endpoint   = 203.0.113.10:51820
AllowedIPs = 10.66.0.0/24

[Socks5]
BindAddress = 127.0.0.1:1080

[Transport]
Mode = off
```

`[Interface]` and `[Peer]` are required; at least one inbound (`[Socks5]`,
`[HTTP]`, `[TCPClientTunnel]`, …) must be present to accept traffic.
`AllowedIPs` is the allowlist of destinations the proxy may reach through
the peer, not a routing table.

## `[Peer] Endpoint` has two meanings

`Endpoint` is required in every mode and must be `host:port`. A hostname is
resolved when the config is loaded, so it must be resolvable on the phone.
What the address *means* depends on `[Transport] Mode`:

| Mode | `endpoint` is | Consumed by |
|---|---|---|
| `off` | the WireGuard server's UDP address — datagrams are sent there from the phone | the kernel (`conn.NewDefaultBind`) |
| `wstunnel` | the address the **server** delivers datagrams to, inside its `--restrict-to` set — normally `127.0.0.1:51820` (the server's own WireGuard listener) | the in-process transport (JWT `r`/`rp`) |

In other words: with `off` you write your server's public address; with
`wstunnel` you write what *the server* should reach, and the public address
goes into `[Transport] URL` instead. Pointing `Endpoint` at
`127.0.0.1:51820` in `off` mode sends WireGuard traffic to the phone
itself — the classic post-migration mistake, which the installer warns
about.

`Mode = wstunnel` also requires exactly one `[Peer]`: one transport carries
one peer.

## `[Transport]`

Optional. A missing section is identical to `Mode = off`,
so a 0.2.0 `wireproxy.conf` still parses. Section and key names are
matched case-insensitively (`[transport]`, `mode = …`), but the examples
use the canonical spelling. `Mode` values are lowercased before
comparison.

| Key | Required | Default | Meaning |
|---|---|---|---|
| `Mode` | no | `off` | `off` or `wstunnel`. Anything else is a config error (exit 2). |
| `URL` | yes when `Mode = wstunnel` | — | wstunnel server base URL. Scheme must be `wss://` (TLS) or `ws://` (plaintext, testing only). Any path in the URL is ignored. |
| `Path` | no | `v1` | Upgrade path prefix: the client requests `/<Path>/events`. Leading/trailing slashes are trimmed; an empty value means `v1`. |
| `Keepalive` | no | `25` | Seconds between WebSocket pings. `0` disables them; a negative or non-integer value is a config error. |
| `Username` | no | — | HTTP basic-auth user for the upgrade request. |
| `Password` | no | — | HTTP basic-auth password. `Authorization: Basic` is sent only when **both** are set. |
| `Insecure` | no | `false` | `true` skips TLS certificate verification (self-signed servers). Accepted spellings are the INI booleans: `true/false`, `yes/no`, `on/off`, `1/0`, `t/f`, `y/n`. |

### Minimal wstunnel example

```ini
[Peer]
PublicKey  = <base64>
# What the SERVER forwards to, not the VPS's public address.
Endpoint   = 127.0.0.1:51820
AllowedIPs = 10.66.0.0/24

[Transport]
Mode      = wstunnel
URL       = wss://your-server.example.com:443
# Must match the server's --restrict-http-upgrade-path-prefix.
Path      = v1
Keepalive = 25
# Only if the server requires basic auth; send both or neither.
# Username = user
# Password = pass
Insecure  = false
```

Match `Path` to the server's `--restrict-http-upgrade-path-prefix` when it
sets one; the default on both sides is `v1`.

### `Insecure` and certificates

- `Insecure = false` (default) verifies the certificate against the system
  trust store and the URL's hostname. Use a real certificate (Let's
  Encrypt) and keep this.
- `Insecure = true` accepts any certificate. Only for a self-signed server
  you control.

`Insecure = true` on an untrusted network removes the only authentication
the transport has. Prefer a trusted certificate.

### Values must not carry trailing comments

The INI parser treats `#` and `;` as the start of a comment **anywhere in a
value**, not just after whitespace. So:

```ini
URL      = wss://example.com:443   # production   ← parses as wss://example.com:443
Password = p#ss                                  ← parses as p
```

A trailing comment on `URL`/`Mode` happens to work, but do not rely on it:
anything after `#` or `;` is silently dropped, which matters for
`Password` (and for any value that legitimately contains those
characters). Keep comments on their own lines.

## Migration from 0.2.0

`customize.sh` migrates once, on the first install/upgrade that finds an
`etc/tunnel.conf`. It writes a `[Transport]` section into
`etc/wireproxy.conf` and keeps the old file as
`etc/tunnel.conf.migrated` (0600).

| Old `tunnel.conf` key | New |
|---|---|
| `mode` | `[Transport] Mode` |
| `remote` | `[Transport] URL` |
| `username` / `password` | `[Transport] Username` / `Password` |
| `insecure` | `[Transport] Insecure` (0.2.0's rule, see below) |
| `remote-udp` | `[Peer] Endpoint`, wstunnel mode only (default `127.0.0.1:51820`) |
| `local-udp` | dropped — no local listener any more |

Behaviour that can surprise you:

- **`Insecure` may flip to `true`.** 0.2.0 verified the certificate only
  when `insecure` started with `false`, `no` or `0`. A file that *omitted*
  `insecure` was therefore unverified, and the migration writes
  `Insecure = true` for it (with an install-log warning). Set
  `Insecure = false` explicitly to start verifying.
- **Invalid configurations degrade to `Mode = off`**, with a warning: a
  `mode` other than `off`/`wstunnel`, a `remote` that is not `ws://` or
  `wss://` (e.g. the old `wts://`), or `mode = wstunnel` with no `remote`.
  In `off` mode `[Peer] Endpoint` must then be the real WireGuard server
  address; the old value was the local listener and is left as-is.
- A `[Transport]` section that already exists stops the migration; the old
  file is still parked as `.migrated`.
- If the rewrite fails, the install aborts and the 0.2.0 module stays
  installed.

Rollback to 0.2.0:

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

## Reaching only the WireGuard subnet

`[Peer] AllowedIPs` is the allowlist of destinations SOCKS5 clients may
reach through the peer. For a "VPN-only" setup (no NAT on the server,
no internet egress), narrow it:

```ini
[Peer]
AllowedIPs = 10.66.0.0/24
```

Anything outside it is never put on the tunnel. Reaching the server's LAN
(`192.168.1.0/24`) or the internet would additionally need NAT or a static
route on the server side.

## Hot reload

The module ships a background watcher (started by `service.sh`) that
polls `etc/wireproxy.conf`'s mtime every 5 seconds. On change it waits 1 s,
then runs `wg-user reload`. `reload` first runs `wg-user configtest`; if the
new file is invalid the running daemon is left alone and the error is
logged. If it passes, the daemon is restarted.

The watcher is the only thing that restarts the daemon for a config edit.
`wg-user save-config` (used by the WebUI editor) validates the staged file
and writes it; it does **not** reload, so one save is one restart, not two.
`wg-user reload` and `wg-user restart` are the manual equivalents.

A `[Transport]` change is just a config change: the daemon rebuilds the
transport from the new file on restart.

You can trigger reload manually: `wg-user reload`.

`$WGU_RELOAD_INTERVAL` (env var) overrides the 5-second poll interval.

## Subcommands

| Subcommand | What it does |
|---|---|
| `wg-user start` | Start the daemon. No-op if already running. |
| `wg-user stop` | SIGTERM, wait up to 20 s, then SIGKILL. |
| `wg-user restart` | Stop + start. |
| `wg-user reload` | Configtest + restart (only if configtest passes). |
| `wg-user status` | Print pid, last log line, and the transport line from `run/state`. Exit 1 when stopped. |
| `wg-user log [N]` | Tail N lines of `log/wg-user.log` (default 50). |
| `wg-user save-config <path>` | Validate `<path>` with `configtest`, then copy it to `etc/wireproxy.conf` (0600). The reload watcher applies it within ~5 s; `save-config` does not restart the daemon itself. A file that fails `configtest` is rejected and the live config is kept. |
| `wg-user version` | Print the module version. |

The binary itself: `bin/wg-user run [-c FILE] [-v]` (the default
subcommand), `configtest [-c FILE]`, and `version`. Exit codes: `0` OK,
`1` runtime error, `2` usage/config error.

## Logs and state

- `log/wg-user.log` — the daemon's stdout/stderr (the shell redirects it;
  `-v` adds wireguard-go's verbose lines).
- `log/watcher.log` — the reload watcher.
- `run/wg-user.pid` — the daemon's pid.
- `run/watcher.pid` — the watcher's pid.
- `run/state` — transport snapshot, read by `wg-user status` and the WebUI:

  ```
  pid=1234
  transport=wstunnel
  transport_state=connected
  transport_detail=wss://your-server.example.com:443
  updated=1763000000
  ```

  `transport` is `off` or `wstunnel`; `transport_state` is `connected`,
  `connecting`, `reconnecting` or `closed`, and `off` when the transport is
  disabled. The daemon removes the file on a clean exit.

## Tuning

- `Keepalive` trades radio wakeups for how quickly a dead connection is
  noticed. `25` is a reasonable mobile default; `0` disables pings (and
  makes half-open TCP connections hang until the kernel notices).
- Narrowing `AllowedIPs` keeps the proxy from becoming a default route.
- `[Transport] Mode = off` is the cheapest path: one kernel UDP socket and
  no TLS.
