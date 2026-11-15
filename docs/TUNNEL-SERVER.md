# VPS setup for the wstunnel transport

This document is **server-side only**: how to run the official
`wstunnel server` and the co-located WireGuard daemon on your VPS. The
phone side is `etc/wireproxy.conf`'s `[Transport]` section — see
[`CONFIG.md`](CONFIG.md).

The 0.3.0 client is an in-process implementation of the wstunnel v11
UDP-over-WebSocket protocol. The server stays the **official binary,
pinned at v11.0.0**. Newer server versions may change the private
handshake or frame format; upgrade only after the repository's interop
test (`make interop`) passes against the new binary.

> **Hard requirement:** start the server with
> `--websocket-mask-frame`. It defaults to off, and without it the
> handshake and auth succeed while **no datagram gets through**. See
> [§4](#4-run-the-wstunnel-server).

## Assumptions

- VPS running a modern Linux (Ubuntu 22.04+ / Debian 12+ / AlmaLinux 9+).
- You have root access on the VPS.
- You have a real domain or stable IP pointing at the VPS.
- The standard WireGuard kernel module is available (or you have
  `wireguard-go` ready).
- A TLS certificate (Let's Encrypt or self-signed).
- TCP 443 (or your chosen port) is reachable from the phone.

## 1. Install the WireGuard server

If not already present:

```bash
# Debian / Ubuntu
apt update && apt install -y wireguard

# Alma / RHEL
dnf install -y epel-release elrepo-release
dnf install -y kmod-wireguard wireguard-tools
```

Create or copy your server's WireGuard config. Important: it must
listen on `127.0.0.1` only, NOT on the public interface. The wstunnel
server will forward to it.

```ini
# /etc/wireguard/wg0.conf
[Interface]
Address    = 10.66.0.1/24
ListenPort = 51820
PrivateKey = <SERVER_PRIVATE_KEY>

[Peer]
PublicKey  = <CLIENT_PUBLIC_KEY>
AllowedIPs = 10.66.0.11/32
```

Bring it up:

```bash
systemctl enable --now wg-quick@wg0
```

## 2. Install wstunnel v11.0.0

The release asset for Linux x86_64 is pinned here together with its
SHA-256:

```bash
curl -fsSL -o /tmp/wstunnel.tar.gz \
  https://github.com/erebe/wstunnel/releases/download/v11.0.0/wstunnel_11.0.0_linux_amd64.tar.gz
echo '9708a99717b5a951453c2ff7c14c25d3418d02ca7fcb96fdb382a8f2083bab5e  /tmp/wstunnel.tar.gz' \
  | sha256sum -c -
tar -xzf /tmp/wstunnel.tar.gz -C /usr/local/bin wstunnel
chmod +x /usr/local/bin/wstunnel
wstunnel --version   # expect 11.0.0
```

For another architecture, substitute the asset name; keep the pinned
version. Do not run `latest`.

## 3. TLS certificate

Use Let's Encrypt (recommended — then the phone can keep
`Insecure = false`):

```bash
apt install -y certbot
certbot certonly --standalone -d your-server.example.com
# certificates land in /etc/letsencrypt/live/your-server.example.com/
```

Or a self-signed certificate (testing, or a lab you fully control):

```bash
mkdir -p /etc/wstunnel
openssl req -x509 -newkey rsa:2048 -days 365 -nodes \
    -keyout /etc/wstunnel/server.key \
    -out /etc/wstunnel/server.crt \
    -subj "/CN=your-server.example.com"
```

A self-signed certificate only works if the phone is told not to verify
it: `Insecure = true` in `[Transport]`. Prefer a trusted certificate;
`Insecure = true` also disables the transport's only server
authentication.

If you created a certificate (either option above), pass both files to the
server: **without `--tls-certificate` / `--tls-private-key`, wstunnel
serves its embedded self-signed certificate.** That is fine for a first
test (with `Insecure = true` on the phone) but never for a real
deployment.

| Choice | `--tls-certificate` | `--tls-private-key` | Phone |
|---|---|---|---|
| Let's Encrypt | `/etc/letsencrypt/live/<domain>/fullchain.pem` | `/etc/letsencrypt/live/<domain>/privkey.pem` | `Insecure = false` |
| Self-signed | `/etc/wstunnel/server.crt` | `/etc/wstunnel/server.key` | `Insecure = true` |
| Embedded default | (omit) | (omit) | `Insecure = true` |

The certificate's SAN must contain the hostname in the phone's
`[Transport] URL`; `--standalone` needs TCP 80 open for the HTTP-01
challenge. wstunnel reloads both files when they change, so a renewed
Let's Encrypt certificate needs no restart.

## 4. Run the wstunnel server

For a Let's Encrypt certificate (recommended, §3) the full command is:

```bash
wstunnel server wss://0.0.0.0:443 \
    --tls-certificate /etc/letsencrypt/live/your-server.example.com/fullchain.pem \
    --tls-private-key /etc/letsencrypt/live/your-server.example.com/privkey.pem \
    --restrict-to 127.0.0.1:51820 \
    --websocket-mask-frame
```

- `wss://0.0.0.0:443` — listen for TLS WebSocket upgrades.
- `--tls-certificate` / `--tls-private-key` — the pair the phone
  validates. Omit both and wstunnel uses its embedded self-signed
  certificate instead.
- `--restrict-to 127.0.0.1:51820` — the only destination a tunnel may
  reach. It must contain the phone's `[Peer] Endpoint`; in wstunnel mode
  that is the address *the server* dials, normally the local WireGuard
  listener `127.0.0.1:51820`.
- `--websocket-mask-frame` — accept RFC 6455 masked client frames. The Go
  client always masks, and this flag defaults to **off**.

Without the certificate pair (embedded self-signed certificate, phone must
set `Insecure = true`), the command is just:

```bash
wstunnel server wss://0.0.0.0:443 \
    --restrict-to 127.0.0.1:51820 \
    --websocket-mask-frame
```

Optionally pin the upgrade path so the server rejects everything else:

```bash
wstunnel server wss://0.0.0.0:443 \
    --restrict-to 127.0.0.1:51820 \
    --restrict-http-upgrade-path-prefix v1 \
    --websocket-mask-frame
```

If you set it, the phone's `[Transport] Path` must match (`v1` is the
default on both sides); a mismatch rejects the upgrade with 404.

### systemd unit

```ini
# /etc/systemd/system/wstunnel.service
[Unit]
Description=wstunnel server (for wg-user)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
# Omit both TLS flags to use the embedded self-signed certificate
# (the phone then needs Insecure = true).
ExecStart=/usr/local/bin/wstunnel server \
    wss://0.0.0.0:443 \
    --tls-certificate /etc/letsencrypt/live/your-server.example.com/fullchain.pem \
    --tls-private-key /etc/letsencrypt/live/your-server.example.com/privkey.pem \
    --restrict-to 127.0.0.1:51820 \
    --websocket-mask-frame
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now wstunnel
```

### Why the mask flag is not optional

A WebSocket client must mask every frame it sends (RFC 6455), and the Go
client does. The official server only unmasks incoming frames when
`--websocket-mask-frame` is set; with the flag absent it forwards the
still-masked payload to UDP. Every datagram then reaches WireGuard XORed
with the 4-byte mask key.

Nothing fails visibly: TCP connects, the upgrade returns 101, the path
and JWT are accepted, the server logs a normal session. WireGuard simply
never handshakes. This is the single most confusing failure mode of the
0.3.0 upgrade.

0.2.0 did **not** need the flag: both sides were the official binaries and
neither masked. After upgrading the module, check the VPS command first.

## 5. Configure the phone (summary)

In `etc/wireproxy.conf`:

```ini
[Peer]
PublicKey  = <base64>
# What the SERVER should reach; must be inside --restrict-to.
Endpoint   = 127.0.0.1:51820
AllowedIPs = 10.66.0.0/24

[Transport]
Mode     = wstunnel
URL      = wss://your-server.example.com:443
Path     = v1
# true only for a self-signed server.
Insecure = false
```

`Endpoint` must be inside `--restrict-to`. Full reference:
[`CONFIG.md`](CONFIG.md).

## 6. Firewall lockdown

Make sure the public UDP/51820 is not reachable. iptables:

```bash
iptables -A INPUT -p udp --dport 51820 -i lo -j ACCEPT
iptables -A INPUT -p udp --dport 51820 -j DROP
```

This allows wstunnel → `127.0.0.1:51820` but drops any public UDP/51820
attempt, so the WireGuard daemon is invisible to the public. Add the
equivalent rule for your firewall manager of choice, and make sure TCP
443 is open.

Also give the WireGuard interface a way out if the clients need more than
the tunnel subnet: `net.ipv4.ip_forward=1` plus NAT, or a static route.

## 7. Verify the data plane

A working transport is a chain; check it in this order.

1. **Server is listening:**

   ```bash
   ss -ltnp | grep ':443'
   journalctl -u wstunnel -f        # shows the upgrade when the phone connects
   ```

2. **Phone reports a live transport:**

   ```sh
   adb shell su -c "/data/adb/modules/wg-user/wg-user status"
   # wg-user: running (pid …)
   # transport: wss://your-server.example.com:443 (connected)
   ```

   `run/state` carries the same snapshot (`transport_state=connected`).

3. **WireGuard handshakes** — on the VPS:

   ```bash
   wg show
   # latest handshake: 12 seconds ago
   # transfer: 4.21 KiB received, 3.88 KiB sent
   ```

   A `connected` WebSocket with **no** handshake is the mask-flag symptom
   (see §4). A handshake with no transfer is usually `AllowedIPs` on
   either side.

4. **Traffic flows end to end** — through the phone's SOCKS5 port:

   ```sh
   # on the phone, from a shell that has curl (e.g. Termux)
   curl --socks5-hostname 127.0.0.1:1080 http://10.66.0.1/
   ```

   Any intranet TCP service works; a `200`/`401`/`404` from it proves the
   datagrams reached the far side. Keep an eye on `transfer:` in
   `wg show` while it runs. The module itself does not ship `curl` — use a
   terminal app, a static binary, or route an app through
   `socks5://127.0.0.1:1080` and exercise it instead.

## 8. Troubleshooting

**What the phone reports (0.3.0).** `internal/wstunnel` has no logger. While
the transport is retrying, `wg-user status` prints the state and says nothing
about the cause:

```
wg-user: running (pid 1234)
last log: <the daemon's last log line>
transport: wss://your-server.example.com:443 (reconnecting)
```

`last log:` stays free of transport errors in 0.3.0: the error text is *not*
written to `log/wg-user.log` while the client retries. A line appears there
only when a failure is permanent and ends the process (`wg-user: …`, exit 1).
So do not wait for a message that will never be printed: use the checks below.

- **`transport: <URL> (reconnecting)`, no error text.** That means the
  WebSocket upgrade is not completing. In order:
  1. Read the **server's** `wstunnel` log — it is the side that knows why
     (bad path, blocked upgrade, refused forward).
  2. Confirm the server was started with `--websocket-mask-frame`; without
     it the client retries forever.
  3. **Check DNS on the phone.** 0.3.0 could not resolve a hostname at all
     on Android (its Go resolver had no nameserver) and reported it exactly
     this way, as a silent `reconnecting`. 0.3.1 resolves names with a
     `ping` fallback, but if DNS is broken the state still shows
     `reconnecting` with no reason. Run `ping -c 1 your-server.example.com`
     on the phone: it must print `PING … (203.0.113.10)`. No address means
     the phone's DNS is down — put the **IP** in `[Transport] URL` (the
     certificate must then cover that IP, or use `Insecure = true` for a
     server you control) or fix the phone's connectivity.
  4. Run `wg-user configtest -c /data/adb/modules/wg-user/etc/wireproxy.conf`
     on the phone. It catches a malformed `URL`/`Path` locally, and since
     0.3.1 it resolves `[Peer] Endpoint` names too, so it also reports a
     hostname the phone cannot resolve.
  5. Prove reachability outside the module: from another host,
     `curl -i https://your-server.example.com/` (a TLS answer, any status,
     means DNS and TCP 443 work), and if you have the client binary,
     `wstunnel client -L 'udp://127.0.0.1:51820:127.0.0.1:51820?timeout_sec=0'
     wss://your-server.example.com:443` reproduces the handshake with
     error output.
- **Handshake works, no traffic.** Check `--websocket-mask-frame` first,
  then compare `[Peer] Endpoint` with `--restrict-to`. Both mistakes look
  identical from the phone: `transport: wss://… (connected)` in
  `wg-user status`, no WireGuard handshake, nothing in the client log beyond
  the upgrade.
- **The daemon is alive but nothing happens: `connecting` forever, restarts
  change nothing.** The process may be frozen by Android rather than broken.
  Every process started from an app inherits that app's cgroup
  (`/uid_<uid>/pid_<pid>`), and the framework freezes it when the app is
  backgrounded — the WebUI runs inside the KernelSU app, so a restart from
  there used to hand the daemon a freezable cgroup. The daemon then stays
  alive with a valid pid file and a stale `run/state`, its SOCKS listener
  keeps a backlog and the transport never leaves `connecting`. Check:

  ```bash
  pid=$(cat /data/adb/modules/wg-user/run/wg-user.pid)
  awk -F: '$1 == "0" { print $3 }' "/proc/$pid/cgroup"
  ```

  `/uid_…/pid_…` means frozen; `/` is the root cgroup and healthy. 0.3.2 and
  later move themselves to `/` at startup, and `wg-user status` warns when it
  sees an app cgroup, so this only affects older installs or a hand-started
  binary. On such a process, `wg-user restart` is not enough if it is started
  from the same app: move the running pid once with
  `echo "$pid" > /sys/fs/cgroup/cgroup.procs` (root), or start the daemon
  from `service.sh` / a root shell outside the app.
- **`connection refused` / timeout on the phone.** The server is not
  running or TCP 443 is blocked. From another host:
  `curl -vI https://your-server.example.com` should return a TLS banner.
- **HTTP 404 on the upgrade.** `[Transport] Path` and
  `--restrict-http-upgrade-path-prefix` disagree. Both default to `v1`.
- **A permanent TLS/config failure.** A certificate error (hostname
  mismatch, unknown authority) that does not retry ends the process: the
  daemon logs `wg-user: …` to `log/wg-user.log` and exits 1. Fix the
  certificate, use the certificate's hostname in `URL`, or — for a
  self-signed server you control — set `Insecure = true`.
- **The server refuses to forward.** Check the `wstunnel` log for a
  `--restrict-to` rejection: the requested target must be one of the
  allowed addresses.
- **`wg show` shows no peer.** The server's `[Peer] PublicKey` must be the
  phone's client public key, and its `AllowedIPs` must include the
  phone's tunnel address (`10.66.0.11/32`).
- **Traffic flows, then stops after a while.** `Keepalive = 0` in
  `[Transport]` means no pings; a NAT or firewall can drop the idle
  mapping. Use the default `25`.
- **Everything works, then dies after a server upgrade.** The protocol is
  pinned for a reason. Roll the server back to v11.0.0 and open an issue
  with the interop test output.
