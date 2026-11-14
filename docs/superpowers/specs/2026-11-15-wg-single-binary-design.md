# wg-user 0.3.0 — 单二进制重构设计

> **状态:** 已批准的设计。实现计划见后续 `docs/superpowers/plans/` 下的计划文件。
> **日期:** 2026-11-15

## 1. 背景

0.2.0 的模块里有两个第三方二进制，每个架构合计 20.8 MB：

| 二进制 | 语言 | 大小/arch | 职责 |
|---|---|---|---|
| `bin/<arch>/wireproxy` | Go | 9.6 MB | 用户态 WireGuard（wireguard-go + gvisor netstack）+ SOCKS5/HTTP 入站 |
| `bin/<arch>/wstunnel` | Rust | 11.2 MB | WG 的 UDP 路径包进 WebSocket-over-TLS，绕过运营商对 UDP 的 QoS |

两个进程之间靠本地 UDP 环回互联：`wstunnel client -L udp://127.0.0.1:51820:127.0.0.1:51820` 监听本地端口，wireproxy 的 `[Peer] Endpoint` 指向这个本地端口。这带来三个副作用：

1. 模块 zip 34.8 MB，其中 11.2 MB/arch 是一个只能以"外部进程"方式使用的 Rust 二进制；
2. 构建需要 Rust 工具链 + Android NDK + 12 个 clang wrapper，PR CI 跑不动，只有 release 才跑；
3. `service.sh` 必须遵守"先起 wstunnel、等它绑定、再起 wireproxy"的顺序，还要处理重启时的端口竞争；配置被拆成两个文件（`wireproxy.conf` + `tunnel.conf`），WebUI 也要两张卡。

**wstunnel 的功能是必需的**（实测所在区域 UDP QoS 严重，且 WG 服务端在 natfrp 的 TCP 隧道后面，无法直连 UDP），所以本设计不删除这个能力，只删除"独立二进制"和"第二个配置文件"这两件事：把它作为传输层实现进同一个 Go 进程。

## 2. 决策摘要

1. **单二进制**：模块只发 `bin/<arch>/wg-user`，由本仓库 Go 源码构建，`wireproxy` 以上游库的形式链接（`github.com/windtf/wireproxy` v1.1.3，其根包即 `package wireproxy`，导出 `ParseConfig` / `CreateIPCRequest` / `VirtualTun` / `RoutineSpawner`）。
2. **传输层内置**：自己实现 wstunnel v11 的客户端协议（UDP-over-WebSocket），**服务端不变**，继续跑官方 `wstunnel server`，版本钉死 v11.0.0。
3. **配置合一**：删除 `etc/tunnel.conf`，传输配置写进 `etc/wireproxy.conf` 的独立 `[Transport]` section；`[Peer] Endpoint` 恢复为"真正的 WG 服务器地址"，不再是本地环回占位。
4. **CLI 收敛**：删除 `wg-user tunnel ...` 子命名空间；一个进程、一个 pid 文件、一个日志、一个状态文件。

预期结果（已实测）：模块 zip 34.8 MB → **14.1 MB**。

## 3. 数据流

改造前（0.2.0，两进程）：

```
SOCKS5 client
  → wireproxy: gvisor netstack → wireguard-go
  → UDP sendto 127.0.0.1:51820          ← [Peer] Endpoint（本地占位）
  → wstunnel client（独立进程，监听同一个本地端口）
  → WSS → natfrp TCP 隧道 → wstunnel server
  → UDP 127.0.0.1:51820                 ← tunnel.conf 的 remote-udp（真正的投递目标）
  → wg0
```

改造后（0.3.0，单进程）：

```
SOCKS5 client
  → gvisor netstack → wireguard-go
  → 自定义 conn.Bind（无本地 socket）
  → WS binary frame（一帧 = 一个 WG 数据报）
  → WSS → natfrp TCP 隧道 → wstunnel server（不变）
  → UDP <[Peer] Endpoint>               ← 必须是服务端 --restrict-to 的目标
  → wg0
```

`Mode = off` 时走 `conn.NewDefaultBind()`，wireguard-go 直接 UDP 发往 `[Peer] Endpoint`，与 0.2.0 的 wireproxy 行为一致。

**收益**：无本地监听端口、无启动顺序依赖、少一次内核拷贝；故障面从"两个进程 + 一个本地端口"变成"一个进程 + 一条 WS 连接"。

## 4. 代码布局

新增 Go 源码（模块路径 `github.com/loficore/wg-user`）：

```
go.mod / go.sum                 依赖钉死：wireproxy v1.1.3、gorilla/websocket、go-ini
cmd/wg-user/main.go             CLI：run（默认）/ configtest / version
internal/config/config.go       读 wireproxy.conf：wireproxy.ParseConfig + 我们自己的 [Transport]
internal/wstunnel/client.go     UDP-over-WebSocket 客户端（握手、JWT、重连、保活）
internal/wstunnel/jwt.go        JWT claims 生成（与服务端 serde 结构对齐）
internal/wgbind/bind.go         conn.Bind 实现，把 WG 报文交给 wstunnel.Conn
internal/device/device.go       netstack TUN + device + VirtualTun 组装（移植自上游 StartWireguard）
internal/state/state.go         run/state 原子写入（供 wg-user status / WebUI 读取）
```

`internal/device` 是上游 `wireproxy.StartWireguard()` 的移植（约 60 行），唯一差别是用我们自己的 `conn.Bind` 而不是 `conn.NewDefaultBind()`。上游 `VirtualTun` 的所有字段都是导出的，因此不需要 fork wireproxy。

## 5. 传输层协议规格（wstunnel v11 互通）

以下全部从 `github.com/erebe/wstunnel` v11 源码核实（`wstunnel/src/tunnel/transport/websocket.rs`、`jwt.rs`、`protocols/udp/server.rs`）：

### 5.1 连接

- 目标：`{ws|wss}://<host>:<port>/<Path>/events`，`Path` 默认 `v1`。
- HTTP/1.1 upgrade，请求头：
  - `Host: <url 的 host[:port]>`
  - `Upgrade: websocket`、`Connection: upgrade`
  - `Sec-WebSocket-Version: 13`
  - `Sec-WebSocket-Key: <16 字节随机数的 base64>`
  - `Sec-WebSocket-Protocol: v1, authorization.bearer.<jwt>`
  - 若配置了 `Username`/`Password`：`Authorization: Basic <base64(user:pass)>`
- TLS：`Insecure = true` → `tls.Config{InsecureSkipVerify: true}`；否则按 URL 主机名做正常校验。

### 5.2 JWT

服务端对 `Sec-WebSocket-Protocol` 里的 token 用 `jsonwebtoken::dangerous::insecure_decode` **不验签**，只取 claims。我们仍生成结构合法的 HS256 token（用每进程随机的 32 字节密钥签名），以便兼容未来可能加验签的服务端。

Header：`{"alg":"HS256","typ":"JWT"}`

Claims：

```json
{
  "id": "<uuid4>",
  "p": { "Udp": { "timeout": null } },
  "r": "<[Peer] Endpoint 的 host>",
  "rp": "<[Peer] Endpoint 的 port>"
}
```

- `p` 必须精确匹配 Rust serde 的外部标签枚举 `LocalProtocol::Udp { timeout: Option<Duration> }`；`timeout: null` 等价于客户端 `timeout_sec=0`（服务端不因空闲关闭 UDP 流）。
- `r`/`rp` 是服务端要投递到的地址，必须落在服务端 `--restrict-to` 允许范围内。
- `r` 取 `wireproxy.ParseConfig` 解析后的 `Peer.Endpoint`（主机名会被本地解析成 IP），因此 wstunnel 模式下 Endpoint 必须是可解析的 `host:port`。

### 5.3 数据帧

- **一个 WS binary frame 恰好承载一个 UDP 数据报**，双向一致（服务端把每帧 payload 直接 `sendto`，反向亦然）。
- 帧上限 65536 字节（与 wstunnel `MAX_PACKET_LENGTH` 一致）；超过则断开并重连。
- 控制帧：收到 `Ping` 立即回 `Pong`；按 `Keepalive` 秒主动发 `Ping`；收到 `Close` 视为断开。
- 同一时刻只保持一条 WS 连接。重连后服务端会为新的 WS 建立新的上游 UDP 流（新的源端口），WireGuard 通过重新握手 + 服务端对端漫游自动恢复，无需服务端改动。

### 5.4 实现约束

- WebSocket 库：`github.com/gorilla/websocket`。握手头 `Sec-WebSocket-Protocol` 由我们手工设置到请求头（该值不是标准子协议列表，不能走 `Dialer.Subprotocols`）。
- 单读协程 + 单写协程：写操作（数据帧 + Ping/Pong）全部经一个 channel 交给写协程，避免并发写。
- 接收侧零拷贝：`NextReader()` + `io.CopyBuffer`/`io.ReadFull` 直接写进 wireguard-go 提供的 buffer。

## 6. `conn.Bind` 设计

接口（wireguard-go `conn.Bind`）：`Open`、`Close`、`SetMark`、`Send`、`ParseEndpoint`、`BatchSize`。

- `Send(bufs, ep)`：对每个 buf 调用一次 `transport.Send`（一帧一包，不合并——合并会被服务端当成单个 UDP 数据报）。未连接时丢弃并返回 `nil`，同时以 1 次/秒的速率限制写 debug 日志，避免握手重试把日志刷爆。
- `Open(port)`：启动接收协程，返回单个 `ReceiveFunc`；忽略 `port`（无本地 socket），返回 `actualPort = 0`。
- `ReceiveFunc(bufs, sizes, offset)`：阻塞直到收到一帧，把 payload 拷进 `bufs[0][offset:]`，写 `sizes[0]`，返回 `1`；`Close()` 之后返回 `net.ErrClosed`。
- `ParseEndpoint(s)`：返回携带原字符串的 endpoint（`DstToString` 原样返回），仅用于日志与 IPC 兼容。
- `BatchSize()`：返回 1。
- `SetMark`：no-op（root 守护进程，不走 SO_MARK）。

配置校验：`Mode = wstunnel` 时只允许恰好一个 `[Peer]`（单条传输只能对应一个对端）；`Mode = off` 保留上游行为。

## 7. 配置规格

### 7.1 文件

`etc/wireproxy.conf` 是唯一配置文件。`[Interface]` / `[Peer]` / `[Socks5]` / `[HTTP]` / `[Resolve]` / `[TCPClientTunnel]` 等 section 的语义与 0.2.0 完全一致（仍由上游 `wireproxy.ParseConfig` 解析）。新增由我们解析的 `[Transport]`：

```ini
[Interface]
Address    = 10.66.0.11/32
PrivateKey = <base64>
DNS        = 10.66.0.1
MTU        = 1280

[Peer]
PublicKey  = <base64>
Endpoint   = 127.0.0.1:51820
AllowedIPs = 10.66.0.0/24

[Socks5]
BindAddress = 127.0.0.1:1080

[Transport]
Mode      = wstunnel
URL       = wss://frp-pen.com:57968
Insecure  = true
Path      = v1
Keepalive = 25
# Username = user
# Password = pass
```

| 键 | 必填 | 默认 | 说明 |
|---|---|---|---|
| `Mode` | 否 | `off` | `off` \| `wstunnel`；其它值 → 配置错误 |
| `URL` | `Mode=wstunnel` 时必填 | — | `wss://`（TLS）或 `ws://`（明文，仅测试用） |
| `Path` | 否 | `v1` | WS upgrade 前缀，`/<Path>/events` |
| `Insecure` | 否 | `false` | 跳过 TLS 证书校验 |
| `Username` / `Password` | 否 | — | 两者同时存在才发送 `Authorization: Basic` |
| `Keepalive` | 否 | `25` | WS Ping 间隔（秒）；`0` 关闭 |

缺少 `[Transport]` section 等价于 `Mode = off`，与 0.2.0 的 `wireproxy.conf` 向后兼容。上游 parser 只按已知 section 名查找，未知 section 被忽略，因此不需要 fork。

### 7.2 `[Peer] Endpoint` 的语义

| 模式 | 含义 | 由谁使用 |
|---|---|---|
| `off` | 客户端直接发送 WG UDP 的目标 | 内核（`conn.NewDefaultBind`） |
| `wstunnel` | **服务端**投递 WG UDP 的目标（= 服务端 `--restrict-to`） | 我们的传输层（JWT `r`/`rp`） |

`Endpoint` 在任何模式下都必须存在且是 `host:port`。0.2.0 里 `tunnel.conf` 的 `local-udp` 与 `remote-udp` 两个键都被删除：前者随本地端口一起消失，后者被 `Endpoint` 取代（两者在当前部署里本来就是同一个值）。

## 8. CLI 与运行时状态

### 8.1 二进制 CLI

```
wg-user run        [-c FILE] [-v]     # 默认子命令，等价于 wg-user -c FILE
wg-user configtest [-c FILE]          # 只校验配置并退出；成功打印 "Config OK"
wg-user version
```

- 第一个参数为空或以 `-` 开头时视为 `run`，`-c` 两种位置都接受。
- `-c` 缺省为 `<二进制目录>/../etc/wireproxy.conf`。
- 退出码：`0` 正常，`1` 运行时错误，`2` 用法/配置错误。
- 日志输出到 stderr，带时间戳，由 shell 重定向到 `log/wg-user.log`。

### 8.2 状态文件

守护进程在每次传输状态变化时原子写入（`run/state.tmp` → `rename`）：

```
pid=1234
transport=wstunnel
transport_state=connected      # connected | connecting | reconnecting
transport_detail=wss://frp-pen.com:57968
updated=1763000000
```

`Mode = off` 时 `transport=off`、`transport_state=connected`（UDP 无连接状态）。

### 8.3 shell CLI（`wg-user`）

删除整个 `tunnel` 子命名空间。其余子命令语义不变，实现简化：

| 子命令 | 0.3.0 行为 |
|---|---|
| `start` | `nohup bin/wg-user run -c etc/wireproxy.conf > log/wg-user.log 2>&1 &`，写 `run/wg-user.pid`，设 `oom_score_adj=-1000`，1 秒后确认存活 |
| `stop` | 对 pid 发 SIGINT，等待 ≤5 秒，必要时 SIGTERM |
| `restart` | stop + start |
| `reload` | `bin/wg-user configtest` 通过后 restart |
| `status` | `wg-user: running (pid N)` + `transport: <detail> (<state>)`；未运行则 `wg-user: stopped` 且退出码 1 |
| `log [N]` | `tail -n N log/wg-user.log`（默认 50） |
| `save-config <path>` | 写入 `etc/wireproxy.conf` 后 reload |
| `version` | 打印 `module.prop` 的 `version` |

`status` 以 `run/wg-user.pid` 为准并校验进程存活；进程已死时忽略 `run/state` 的残留内容（被 SIGKILL 时不会清理）。

删除：`run/wireproxy.pid`、`run/wstunnel.pid`、`log/wireproxy.log`、`log/wstunnel.log`、`common/abi.sh::select_wireproxy_binary`（改为 `select_binary`，指向 `bin/wg-user`）。

`service.sh` 只保留：启动一次 → 启动单文件 mtime 观察器（`etc/wireproxy.conf`）→ 返回。

### 8.4 WebUI

- 删除 tunnel 卡及其 i18n 词条。
- 状态卡：进程状态 + `transport: wss://... (connected)`。
- 配置编辑器仍编辑 `etc/wireproxy.conf`，保存 → `wg-user save-config`。
- 冒烟测试（`webui/test/smoke.mjs`）的假桥接输出同步更新。

## 9. 可靠性与错误处理

| 场景 | 行为 |
|---|---|
| WS 连接失败/断开 | 指数退避重连：0.5s 起，×2，上限 30s，±20% 抖动；状态写 `reconnecting` 并写日志 |
| 重连成功 | 状态写 `connected`；WireGuard 自行重新握手，数据面恢复（服务端对端漫游到新的 UDP 源端口） |
| 未连接时 WG 发包 | 丢弃 + 1 次/秒 debug 日志；不阻塞、不返回错误给 wireguard-go |
| WS 帧超过 65536 字节 | 视为协议错误，断开并重连 |
| TLS 证书错误 | 不重连（配置错误），进程退出码 1 并打印原因 |
| 配置非法 | `run` 与 `configtest` 都立即失败，打印行号/键名；`run` 退出码 2 |
| SIGINT / SIGTERM | 停止 SOCKS5/HTTP 监听 → 关闭 device → 关闭 WS → 删除 `run/state` → 退出 0 |
| 本地 UDP 端口 | 不再存在；无端口冲突、无启动顺序依赖 |

## 10. 构建、CI、打包

### 10.1 构建

- `go.mod` 固定 `go 1.26`（wireproxy v1.1.3 的 go.mod 即 `go 1.26.0`）；新增依赖：`github.com/gorilla/websocket`、`github.com/go-ini/ini`（已随 wireproxy 间接引入，改为直接依赖）。
- `scripts/build.sh`：对 4 个架构执行 `GOOS=linux GOARCH=<arm64|arm|amd64|386> CGO_ENABLED=0 go build -trimpath -ldflags '-s -w' -o bin/<arch>/wg-user ./cmd/wg-user`。
- 删除：`scripts/cargo-cross.sh`、`scripts/cargo-wrappers/`（12 个 wrapper）、`bin/*/wstunnel`、`bin/*/wireproxy`。
- `.gitignore`：`bin/*/wireproxy`、`bin/*/wstunnel` → `bin/*/wg-user`；`etc/tunnel.conf` → `etc/tunnel.conf*`（迁移后的 `.migrated` 可能含 basic-auth 凭据，仍必须忽略）。
- `.tool-versions`：新增 Go 1.26 的固定版本（与 nodejs 并列）。

### 10.2 CI（保持三个必需检查）

| Job | 内容 |
|---|---|
| `scripts` | `sh -n` 全部脚本；`make verify`；确认 `etc/` 下没有任何被跟踪的真实配置（`wireproxy.conf`、`tunnel.conf`、`tunnel.conf.migrated`）；`module.prop` 与 `webui/package.json` 版本一致 |
| `build-go` | `go vet ./...` + `go test ./...` + 4 架构交叉编译 + **互通测试**（下载钉死版本的 `wstunnel` 服务端二进制并校验 sha256，然后跑 `go test -tags interop ./internal/wstunnel`） |
| `webui` | 不变（`pnpm run check` + `webroot/` 是否过期） |

Release workflow：删除 Rust 构建步骤；`make package` 产出 `wg-user-<version>.zip` + `SHA256SUMS`。

### 10.3 打包清单

`PKG_FILES` 去掉 `etc/tunnel.conf.example`；`bin/<arch>` 现在只含 `wg-user`。实测 zip 14.1 MB。

### 10.4 版本与文档

- `module.prop`：`version=0.3.0`、`versionCode` +1；`webui/package.json` 同步（CI 校验两者一致）。
- `README.md`：数据流图、构建要求（去掉 Rust/NDK）、zip 体积、CLI 说明。
- `docs/ARCHITECTURE.md`：按本设计重写 §2/§3/§4/§7（传输层内置）、删除"两个 build pipeline"。
- `docs/CONFIG.md`：单一配置文件 + `[Transport]` 参考表 + `Endpoint` 双语义说明。
- `docs/TUNNEL-SERVER.md`：只保留 VPS 侧服务端搭建，写明服务端版本钉死 v11.0.0 与互通测试要求。
- `docs/INSTALL.md`：删除 Rust 工具链依赖。
- `docs/CHANGELOG.md`：新增 0.3.0 段，标注破坏性变更（`tunnel.conf` 删除、`wg-user tunnel` 删除）与自动迁移。
- `uninstall.sh`：pid 文件列表改为 `run/wg-user.pid`、`run/watcher.pid`，并清理 `run/state`。

## 11. 测试计划

### 11.1 单元（`go test ./...`）

- `internal/config`：无 `[Transport]` → `off`；`off`/`wstunnel` 解析；未知 `Mode` 报错；`wstunnel` 缺 `URL`、缺 `Endpoint`、多 `[Peer]` 报错；旧的 `local-udp`/`remote-udp` 出现时被忽略且不报错（迁移期的宽容）。
- `internal/wstunnel/jwt`：claims 与 golden JSON 逐字节一致（`{"id":...,"p":{"Udp":{"timeout":null}},"r":...,"rp":...}`），token 是合法三段式 HS256。
- `internal/wstunnel`：帧编解码；`Send` 在未连接时丢弃且不阻塞；`ReceiveFunc` 按帧边界交付；超长帧触发断开。
- `internal/wgbind`：`Send` 对每个 buf 各发一帧（不合并）；`BatchSize()==1`；`Close` 后 `ReceiveFunc` 返回 `net.ErrClosed`。
- `internal/state`：原子写入内容与格式。

### 11.2 互通测试（`//go:build interop`，CI 必跑）

启动官方 `wstunnel server wss://127.0.0.1:<port> --restrict-to 127.0.0.1:<echo>`（二进制路径来自 `WSTUNNEL_SERVER_BIN`），配一个 UDP echo 服务；我们的客户端发送 10 个随机长度的数据报并逐个校验往返。这是"协议没漂移"的锁。

### 11.3 设备端验收清单（手动）

1. 覆盖安装 0.3.0 → `wg-user status` 显示 running + `transport: wstunnel ... (connected)`。
2. `curl --socks5-hostname 127.0.0.1:1080 http://10.66.0.1/`（或内网任意 TCP 服务）通。
3. 开飞行模式 10 秒再关 → 30 秒内恢复 `connected`，`curl` 再次可用。
4. 改 `etc/wireproxy.conf` 的 `[Transport] URL` → 观察器 5 秒内触发 reload，状态回到 connected。
5. WebUI：中英文切换、状态卡、日志卡、配置保存均正常；无 tunnel 卡。
6. `ps` 中只有一个 `wg-user` 进程；`netstat` 中不存在本地 `51820` UDP 监听。

## 12. 迁移与回滚

### 12.1 自动迁移（`customize.sh`）

升级安装时，若存在 `etc/tunnel.conf` 且 `etc/wireproxy.conf` 里没有 `[Transport]`：

1. 按映射追加 section：`mode=wstunnel→Mode`、`remote→URL`、`insecure→Insecure`、`username→Username`、`password→Password`；`local-udp`/`remote-udp` 丢弃。
2. 打印一条日志，说明 `[Peer] Endpoint` 现在的语义（服务端投递目标，应等于原 `remote-udp`）。
3. 旧文件改名为 `etc/tunnel.conf.migrated` 保留。

全新安装：只从 `*.example` 播种 `etc/wireproxy.conf`（含 `[Transport]` 段，`Mode = off`）。

### 12.2 回滚

保留 0.2.0 的 release zip 与 `etc/tunnel.conf.migrated`。回滚步骤：改名 `tunnel.conf.migrated → tunnel.conf`，重装 0.2.0。`[Transport]` section 会被 0.2.0 的 wireproxy 忽略，因此同一个 `wireproxy.conf` 在回滚后仍可读。

## 13. 风险

| 风险 | 影响 | 缓解 |
|---|---|---|
| wstunnel 服务端升级改了私有握手/帧格式 | 传输全断 | VPS 版本钉死 v11.0.0；CI 互通测试；升级服务端前必须先过该测试；文档写明 |
| 自定义 `conn.Bind` 与 wireguard-go 语义不符 | 握手失败、数据面不通 | 互通测试覆盖真实 UDP 往返；`Mode = off` 作为对照路径；设备端验收清单 |
| WSTUNNEL_SERVER_BIN 在 CI 下载失败 | CI 红 | 钉死版本 + sha256；失败明确报错，避免静默跳过 |
| wireproxy 上游 API 变化 | 编译失败 | `go.mod` 钉死 v1.1.3；升级时由 CI 编译 + 单测把关 |
| Go 1.26 工具链要求 | 本地/CI 构建失败 | `.tool-versions` 与 CI `setup-go` 固定 1.26 |
| 重连后服务端 UDP 源端口变化 | 短暂丢包 | WG 重新握手 + 服务端对端漫游；验收清单第 3 项验证 |
| 单二进制体积仍偏大（8.7 MB/arch） | 下载体积 | 已在预期内（zip 14.1 MB）；如需更小，另议"只发 arm64/armv7"方案（本设计不含） |

## 14. 非目标（YAGNI）

- 自己实现 wstunnel **服务端**（B′ 方案，留待协议漂移成为真实问题后再做）。
- SOCKS5 UDP ASSOCIATE（上游 wireproxy 不支持）。
- 多 Peer / 多传输并行。
- 只发 arm 架构（体积优化，单独决策）。
- QUIC/WebTransport 传输（`wqs://`）。
- 进程内配置热重载（继续用 shell mtime 观察器 + restart）。

## 15. 验收标准

1. `make package` 产出的 zip ≤ 15 MB，且每个 `bin/<arch>/` 下只有 `wg-user` 一个文件。
2. `make verify` 通过，`go vet ./...`、`go test ./...`、互通测试全绿。
3. 设备端验收清单 6 项全部通过。
4. 仓库中不再有 `etc/tunnel.conf*`、`scripts/cargo-*`、`bin/*/wstunnel`、`bin/*/wireproxy` 的引用（文档中的历史说明除外）。
5. WebUI 中英文完整，无 tunnel 卡，`webroot/` 与源码一致。
