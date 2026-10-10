# VPS-side setup for the obfuscation transport

This document explains how to set up the wstunnel server and the
co-located WireGuard daemon on your VPS. The result is a reachable
endpoint (port 443, TLS) that forwards to the local WireGuard UDP listener.

## Assumptions

- VPS running a modern Linux (Ubuntu 22.04+ / Debian 12+ / AlmaLinux 9+).
- You have root access on the VPS.
- You have a real domain or stable IP pointing at the VPS.
- The standard WireGuard kernel module is available (or you have
  `wireguard-go` ready).
- A TLS cert (Let's Encrypt or self-signed).

## 1. Install WireGuard server

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
Address    = 10.0.0.1/24
ListenPort = 51820
PrivateKey = <SERVER_PRIVATE_KEY>

[Peer]
PublicKey = <CLIENT_PUBLIC_KEY>
AllowedIPs = 10.0.0.2/32
```

Bring it up:

```bash
systemctl enable --now wg-quick@wg0
```

## 2. Install wstunnel

```bash
# x86_64 Linux static binary (adjust version/arch)
curl -L -o /usr/local/bin/wstunnel \
    https://github.com/erebe/wstunnel/releases/download/v11.0.0/wstunnel-v11.0.0-x86_64-unknown-linux-gnu.tar.gz \
    | tar -xz -C /usr/local/bin wstunnel
chmod +x /usr/local/bin/wstunnel
```

(or use your distro's package manager if it ships wstunnel.)

## 3. TLS certificate

Use Let's Encrypt (recommended):

```bash
apt install -y certbot
certbot certonly --standalone -d your-server.com
# certificates land in /etc/letsencrypt/live/your-server.com/
```

Or a self-signed cert (testing only):

```bash
mkdir -p /etc/wstunnel
openssl req -x509 -newkey rsa:2048 -days 365 -nodes \
    -keyout /etc/wstunnel/server.key \
    -out /etc/wstunnel/server.crt \
    -subj "/CN=your-server.com"
```

## 4. Run wstunnel server

Create `/etc/systemd/system/wstunnel.service`:

```ini
[Unit]
Description=wstunnel server (for wg-user)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/wstunnel server \
    wss://0.0.0.0:443 \
    --restrict-to 127.0.0.1:51820
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
systemctl daemon-reload
systemctl enable --now wstunnel
```

## 5. (Optional) Firewall lockdown

Make sure the public UDP/51820 is not reachable. iptables:

```bash
iptables -A INPUT -p udp --dport 51820 -i lo -j ACCEPT
iptables -A INPUT -p udp --dport 51820 -j DROP
```

This allows wstunnel → 127.0.0.1:51820 but drops any public UDP/51820
attempts (so the WireGuard daemon is invisible to the public).

## 6. Configure the Android side

On the phone, in `etc/wireproxy.conf`:

```ini
[Peer]
Endpoint = 127.0.0.1:51820
```

In `etc/tunnel.conf`:

```ini
mode = wstunnel

[wstunnel]
local-udp = 127.0.0.1:51820
remote    = wss://your-server.com:443
# username = youruser
# password = yourpassword
```

`wg-user reload` and you should see in `log/wstunnel.log`:

```
[INFO] wstunnel started
```

And in `log/wireproxy.log`:

```
[INFO] handshake complete
```

## 7. Troubleshooting

- **`wstunnel: connection refused` from the client** — check the server is
  running and port 443 is open. `curl -vI https://your-server.com` from
  a host outside the VPS should return a server banner.
- **Handshake fails after 30 s** — wstunnel may be sending to a wrong
  remote. Run `wg show` on the server and verify the peer entry exists.
- **Handshake succeeds but traffic doesn't flow** — AllowedIPs in the
  server's `[Peer]` block must include the client's `10.0.0.2/32` (or
  whatever the client is using).
- **Carrier still throttles** — wstunnel defaults to TCP 443. If
  throttling persists, switch the client remote to `wqs://...` (WebSocket
  over QUIC) which uses UDP/443 instead.
