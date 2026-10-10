# Config reference

The runtime config files live in `/data/adb/modules/wg-user/etc/`:

- `wireproxy.conf` — WireGuard keys, peer, SOCKS5/HTTP bind. **Edit this.**
- `wireproxy.conf.example` — fully commented template (do not edit; for reference).
- `tunnel.conf` — obfuscation transport (wstunnel). Optional.
- `tunnel.conf.example` — fully commented template.

`wireproxy.conf` uses the **wireproxy INI format** — see
[`etc/wireproxy.conf.example`](../etc/wireproxy.conf.example) for a
commented template, and [the upstream README](https://github.com/windtf/wireproxy)
for the full spec.

## When using the tunnel (wstunnel)

If you enable wstunnel in `tunnel.conf`, change `wireproxy.conf`'s
`[Peer] Endpoint` to `127.0.0.1:51820` (matching `tunnel.conf`'s
`local-udp`). wstunnel then forwards to the real server over TLS/443.

`tunnel.conf` keys (flat parser, first match wins, no trailing comments):

| key | default | meaning |
|---|---|---|
| `mode` | `off` | only `wstunnel` enables the tunnel |
| `local-udp` | `127.0.0.1:51820` | where wstunnel listens for wireproxy on the phone |
| `remote-udp` | `127.0.0.1:51820` | where the *server* forwards to (its own WG listener) |
| `remote` | — | `wss://host:443`, `ws://host:port` or `wts://host:443` |
| `username` / `password` | — | WebSocket upgrade basic auth (both required) |
| `insecure` | `true` | `false` enables TLS certificate verification (wstunnel's default cert is self-signed) |

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
polls the config mtime every 5 seconds. On change of either file, it:

1. Runs `wireproxy -n -c <config>` (configtest) for the new content.
2. If configtest fails: keeps the old daemons running and logs the error.
3. If configtest passes: graceful stop (SIGINT, then SIGTERM after 5s) +
   restart, in the right order (tunnel first if enabled, then wireproxy).

You can trigger reload manually: `wg-user reload`.

`$WGU_RELOAD_INTERVAL` (env var) overrides the 5-second poll interval.

## Subcommands

| Subcommand | What it does |
|---|---|
| `wg-user start` | Start wstunnel (if enabled) then wireproxy. No-op if already running. |
| `wg-user stop` | Stop wireproxy then wstunnel. |
| `wg-user restart` | Stop + start. |
| `wg-user reload` | Configtest + restart (only if configtest passes). |
| `wg-user status` | Print status of wireproxy AND tunnel. |
| `wg-user log [target] [N]` | Tail N lines. `target` = `wireproxy` (default) or `tunnel`. |
| `wg-user save-config <path>` | Copy file to `etc/wireproxy.conf`, then reload. |
| `wg-user version` | Print module version. |
| `wg-user tunnel start` | Start the wstunnel only. |
| `wg-user tunnel stop` | Stop the wstunnel only. |
| `wg-user tunnel status` | Show tunnel state (running / stopped / disabled). |
| `wg-user tunnel restart` | Stop + start wstunnel. |

## Environment variables in config

Any string value in `wireproxy.conf` may reference an environment
variable: `PrivateKey = $MY_WG_KEY`. If `$MY_WG_KEY` is unset, wireproxy
will refuse to start. Run `wg-user reload` after exporting the var.

Use `$$` to emit a literal `$` (e.g. for a password containing `$`).

## Logs

- `log/wireproxy.log` — wireproxy stdout/stderr (verbose by default; pass `-s` to silence).
- `log/wstunnel.log` — wstunnel stdout/stderr.
- `log/watcher.log` — the config-watcher's log.

## Tuning

To reduce wireproxy verbosity, edit `wg-user` to pass `-s` to wireproxy
in `cmd_start` (and any other place `wireproxy` is invoked). The
config file format is kept unchanged.

## Environment variables

`$WGU_RELOAD_INTERVAL` — override the watcher's 5-second poll interval
(set higher on a battery-constrained device).
