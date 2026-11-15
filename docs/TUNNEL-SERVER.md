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

- **Handshake works, no traffic.** Check `--websocket-mask-frame` first,
  then compare `[Peer] Endpoint` with `--restrict-to`. Both mistakes look
  identical from the phone: `transport_state=connected`, no WireGuard
  handshake, nothing in the client log beyond the upgrade.
- **`connection refused` / timeout on the phone.** The server is not
  running or TCP 443 is blocked. From another host:
  `curl -vI https://your-server.example.com` should return a TLS banner.
- **HTTP 404 on the upgrade.** `[Transport] Path` and
  `--restrict-http-upgrade-path-prefix` disagree. Both default to `v1`.
- **TLS error in `log/wg-user.log`** (`x509: certificate signed by unknown
  authority`, hostname mismatch). The client does not retry certificate
  errors; it exits 1. Fix the certificate, use the certificate's hostname
  in `URL`, or — for a self-signed server you control — set
  `Insecure = true`.
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
