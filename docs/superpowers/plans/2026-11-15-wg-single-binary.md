# wg-user 0.3.0 单二进制重构 — 实现计划

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 把模块里的 `wireproxy` + `wstunnel` 两个二进制换成一个由本仓库 Go 源码构建的 `bin/<arch>/wg-user`，传输层（UDP-over-WebSocket）内置，配置文件合并为单个 `etc/wireproxy.conf`。

**Architecture:** `cmd/wg-user` 用 `github.com/windtf/wireproxy` 作为库构建 netstack TUN + wireguard-go device，但把 `conn.Bind` 换成 `internal/wgbind`，后者把每个 WG 数据报直接写成一帧 WebSocket（`internal/wstunnel`）。`[Transport] Mode=off` 时退回 `conn.NewDefaultBind()`，行为与 0.2.0 的 wireproxy 一致。VPS 侧继续跑官方 `wstunnel server` v11.0.0，不改。

**Tech Stack:** Go 1.26（无 cgo）、`github.com/windtf/wireproxy` v1.1.3、`github.com/gorilla/websocket`、`github.com/go-ini/ini`、gvisor netstack（随 wireproxy 引入）、POSIX sh（mksh）、Preact + TypeScript（WebUI，不改技术栈）。

**Spec:** `docs/superpowers/specs/2026-11-15-wg-single-binary-design.md`

## Global Constraints

- Go 版本 `1.26.0`；模块路径 `github.com/loficore/wg-user`。
- 依赖钉死：`github.com/windtf/wireproxy v1.1.3`、`github.com/gorilla/websocket v1.5.3`、`github.com/go-ini/ini v1.67.0`。不新增其它运行时依赖。
- 交叉编译：`GOOS=linux CGO_ENABLED=0 go build -trimpath -ldflags '-s -w'`，架构映射 `arm64→arm64-v8a`、`arm→armeabi-v7a`、`amd64→x86_64`、`386→x86`。
- 配置键与默认值（逐字）：`Mode` ∈ {`off`,`wstunnel`}，缺 section 等于 `off`；`Path` 默认 `v1`；`Keepalive` 默认 `25` 秒，`0` 表示关闭；`Insecure` 默认 `false`。
- WS 帧上限 `65536` 字节；**一帧 = 一个 UDP 数据报**，绝不合并。
- JWT claims 必须是 `{"id":<uuid>,"p":{"Udp":{"timeout":null}},"r":<host>,"rp":<port>}`（服务端 serde 结构，`timeout:null` = `timeout_sec=0`）。
- `Mode = wstunnel` 时只允许恰好一个 `[Peer]`；`[Peer] Endpoint` 必须存在且为 `host:port`。
- CLI 退出码：`0` 正常、`1` 运行时错误、`2` 用法/配置错误。日志写 stderr。
- 所有 shell 脚本是 POSIX sh（Android `/system/bin/sh` 是 mksh）：不用 bash 数组/`local`/`[[ ]]`。`customize.sh` 里禁止 `set -e`（它被 sourced）。
- 版本 `0.3.0`，且 `module.prop:version` 必须等于 `webui/package.json:version`。
- 仓库是 jj colocated：每个任务用 `jj describe -m "<type>: <scope>"` 落提交，然后 `jj new` 开始下一个任务；不要用裸 `git commit`。

## Review Focus

1. **服务端 `--restrict-to` 与 `[Peer] Endpoint` 不一致**：JWT 里的 `r`/`rp` 是服务端唯一可投递目标，写错时表现为握手正常但没有一个回包。由 Task 9 的 `TestInterop_TargetOutsideRestrictIsDropped` 覆盖。
2. **UDP 空闲超时**：JWT 的 `p.Udp.timeout` 若写成数字，服务端会在该秒数后静默拆流，WG 看起来"通了又断"。由 Task 2 的 claims 形状 golden 测试锁定为 `null`。
3. **WG 包与 WS 帧边界错配**：把 `bufs` 合并进一帧会让服务端把它当成一个巨型 UDP 包发给 wg0。由 Task 5 的 `TestBind_SendOneFramePerBuffer` 覆盖。
4. **重连期间并发写 WS**：gorilla 禁止并发写，重连 + keepalive 组合最容易踩。由 Task 4 在 `-race` 下跑 `TestRun_ReconnectsAfterServerClose` 与 `TestRun_SendsPing` 覆盖。
5. **迁移把 basic-auth 凭据泄漏成可跟踪文件**：`etc/tunnel.conf.migrated` 可能含 `username`/`password`，必须继续被 `.gitignore` 覆盖且保持 `0600`。由 Task 12 的 `install_test.sh`（权限断言）与 Task 12 的 `.gitignore` 改动覆盖。

---

## Task 1: Go 模块与配置加载

**Files:**
- Create: `go.mod`, `go.sum`
- Create: `internal/config/config.go`
- Test: `internal/config/config_test.go`
- Create: `internal/config/testdata/minimal.conf`, `internal/config/testdata/transport-wstunnel.conf`, `internal/config/testdata/transport-bad-mode.conf`

**Interfaces:**
- Consumes: `wireproxy.ParseConfig(path) (*wireproxy.Configuration, error)`；`wireproxy.Configuration{Device *DeviceConfig; Routines []RoutineSpawner; Resolve *ResolveConfig}`；`DeviceConfig.Peers []PeerConfig`，`PeerConfig.Endpoint *string`。
- Produces:
  ```go
  package config

  const (ModeOff = "off"; ModeWstunnel = "wstunnel")

  type Transport struct {
      Mode, URL, Path, Username, Password string
      Insecure bool
      Keepalive time.Duration
  }

  type Config struct {
      Path      string
      WireProxy *wireproxy.Configuration
      Transport Transport
  }

  func Load(path string) (*Config, error)
  ```

- [ ] **Step 1: 写失败的测试**

```go
func TestLoad_MissingTransportSectionDefaultsToOff(t *testing.T) {
	c, err := Load("testdata/minimal.conf")
	if err != nil { t.Fatalf("unexpected error: %v", err) }
	if c.Transport.Mode != ModeOff { t.Fatalf("mode = %q, want off", c.Transport.Mode) }
	if c.WireProxy == nil || len(c.WireProxy.Device.Peers) != 1 { t.Fatal("wireproxy config not parsed") }
}

func TestLoad_WstunnelParsesEveryKey(t *testing.T) {
	c, err := Load("testdata/transport-wstunnel.conf")
	// want: Mode=wstunnel, URL=wss://example.com:443, Path=wstunnel, Insecure=true,
	//       Username=u, Password=p, Keepalive=30*time.Second
}

func TestLoad_RejectsUnknownMode(t *testing.T) {
	// testdata/transport-bad-mode.conf has Mode=quic; want error containing "unsupported transport mode"
}

func TestLoad_WstunnelRequiresURL(t *testing.T)       { /* want error containing "[Transport] URL is required" */ }
func TestLoad_WstunnelRequiresEndpoint(t *testing.T)   { /* want error containing "[Peer] Endpoint is required" */ }
func TestLoad_WstunnelRejectsMultiplePeers(t *testing.T) { /* want error containing "exactly one [Peer]" */ }
func TestLoad_IgnoresLegacyTunnelKeys(t *testing.T)    { /* local-udp/remote-udp in [Transport] must not error */ }
```

`testdata/minimal.conf` 用固定的合法 32 字节 base64 密钥（例如 `PrivateKey = yAnz5TF+lXXJte14tji3zlMNq+hd2rYUIgJBgB3fBmk=`、`PublicKey = xTIBA5rboUvnH4htodjb6e697QjLERt1NAB4mZqp8Dg=`），`Address = 10.66.0.11/32`、`MTU = 1280`、`Endpoint = 127.0.0.1:51820`、`AllowedIPs = 10.66.0.0/24`、`[Socks5] BindAddress = 127.0.0.1:1080`。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/config -run TestLoad -v`
Expected: FAIL（包不存在 / `Load` 未定义）

- [ ] **Step 3: 实现 `Load`**

先 `go mod init github.com/loficore/wg-user`，再 `go get github.com/windtf/wireproxy@v1.1.3 github.com/gorilla/websocket@v1.5.3 github.com/go-ini/ini@v1.67.0`（`go.mod` 的 `go` 行必须是 `1.26.0`，与 wireproxy v1.1.3 一致）。

然后 `ini.Load(path)` 拿 `[Transport]` section 的原始键值，再 `wireproxy.ParseConfig(path)`。校验顺序固定：`Mode` 合法 → `wstunnel` 时 `URL` 非空且 scheme 为 `ws`/`wss` → `Path` 缺省 `v1` → `Keepalive` 缺省 25、负数报错 → `Endpoint` 存在 → `len(Peers)==1`。所有错误用 `fmt.Errorf("config %s: ...", path)` 包一层。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/config -v`
Expected: PASS（7 个用例）

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(config): single-file config loader with [Transport] section"
jj new
```

---

## Task 2: wstunnel JWT claims

**Files:**
- Create: `internal/wstunnel/jwt.go`
- Test: `internal/wstunnel/jwt_test.go`

**Interfaces:**
- Produces: `func BuildToken(target string) (string, error)`，`target` 形如 `127.0.0.1:51820`；返回三段式 HS256 token。

- [ ] **Step 1: 写失败的测试**

```go
func TestBuildToken_HasThreeHS256Segments(t *testing.T) {
	tok, err := BuildToken("127.0.0.1:51820")
	// header 段 base64url 解码后必须等于 `{"alg":"HS256","typ":"JWT"}`
	// token 必须是三段，用 "." 分隔
}

func TestBuildToken_ClaimShapeMatchesServerSerde(t *testing.T) {
	tok, _ := BuildToken("127.0.0.1:51820")
	// payload 段解码后用 map[string]json.RawMessage 断言：
	//   "p" 原始字节 == `{"Udp":{"timeout":null}}`
	//   "r" == `"127.0.0.1"`, "rp" == `51820`
	//   "id" 非空且形如 8-4-4-4-12 的十六进制
}

func TestBuildToken_RejectsTargetWithoutPort(t *testing.T) // want error
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/wstunnel -run TestBuildToken -v`
Expected: FAIL（`BuildToken` 未定义）

- [ ] **Step 3: 实现 `BuildToken`**

`crypto/rand` 生成 16 字节作为 UUID v4（设置 version/variant 位）与 32 字节 HMAC 密钥（包级 `var tokenKey = mustRandomKey()`）。claims 用固定字段顺序的结构体，**注意 `p` 需要一层包装**：`type claims struct { ID string \`json:"id"\`; P protocol \`json:"p"\`; R string \`json:"r"\`; RP uint16 \`json:"rp"\` }`、`type protocol struct { UDP udp \`json:"Udp"\` }`、`type udp struct { Timeout *int \`json:"timeout"\` }` 且 `Timeout: nil`。签名：`hmac.New(sha256.New, tokenKey)`。编码用 `base64.RawURLEncoding`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/wstunnel -run TestBuildToken -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(wstunnel): JWT claims matching the v11 server's serde shape"
jj new
```

---

## Task 3: WebSocket 数据报连接（单连接）

**Files:**
- Create: `internal/wstunnel/client.go`
- Test: `internal/wstunnel/client_test.go`

**Interfaces:**
- Consumes: `BuildToken(target string) (string, error)`（Task 2）。
- Produces:
  ```go
  type Config struct {
      URL, Path, Target, Username, Password string
      Insecure bool
      Keepalive time.Duration
      BackoffMin, BackoffMax time.Duration // 测试可缩短；0 时用 500ms / 30s
  }
  func New(cfg Config) *Client
  func (c *Client) Send(pkt []byte) error                          // 未连接时丢弃并返回 nil
  func (c *Client) Recv(buf []byte, stop <-chan struct{}) (int, error) // stop 关闭或 Client 关闭后返回 net.ErrClosed
  func (c *Client) Close() error
  func (c *Client) State() string             // "connecting" | "connected" | "reconnecting"
  ```

- [ ] **Step 1: 写测试辅助与失败测试**

测试文件里用 `httptest.NewServer` + `gorilla.Upgrader` 做假服务端，记录每次 upgrade 的 `Sec-WebSocket-Protocol` 头，并把收到的 binary frame 原样回显。

```go
func TestDialOnce_SendsTunnelJWT(t *testing.T) {
	// header 必须以 "v1, authorization.bearer." 开头；token 解码后 r/rp == target
}

func TestSendRecv_PreservesDatagramBoundaries(t *testing.T) {
	// 依次 Send 长度 1 / 100 / 1300 的包，Recv 必须得到同样三个长度与内容
}

func TestSend_WhenNotConnectedDropsWithoutError(t *testing.T) // Send 返回 nil，且服务端没收到包
func TestRecv_AfterCloseReturnsErrClosed(t *testing.T)        // errors.Is(err, net.ErrClosed)
func TestDialOnce_FailsOnNonUpgradeResponse(t *testing.T)     // 服务端返回 200 -> 错误
func TestRecv_RejectsOversizeFrame(t *testing.T)              // 服务端发 70000 字节 -> 错误且连接被关闭
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/wstunnel -run 'TestDialOnce|TestSend|TestRecv' -v`
Expected: FAIL（`New` 未定义）

- [ ] **Step 3: 实现 `Client` 的单连接部分**

`dialOnce(ctx)`：按 `Config` 组 URL（`/<Path>/events`，`Path` 为空时用 `v1`），把 `Sec-WebSocket-Protocol` 手工写进 `http.Header`，`Authorization: Basic` 仅在用户名与密码都非空时设置，`websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure}}`，`ws.SetReadLimit(65536)`。`Send` 在互斥锁内检查连接并 `WriteMessage(BinaryMessage, pkt)`；`Recv` 从 `inbox chan []byte`（容量 256）取一帧并 `copy` 进调用方 buffer，buffer 不够大时返回错误。`Close` 关闭连接与 inbox，`Recv` 返回 `net.ErrClosed`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/wstunnel -run 'TestDialOnce|TestSend|TestRecv' -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(wstunnel): websocket datagram client (single connection)"
jj new
```

---

## Task 4: 重连、退避与保活

**Files:**
- Modify: `internal/wstunnel/client.go`
- Test: `internal/wstunnel/supervisor_test.go`

**Interfaces:**
- Produces: `func (c *Client) Run(ctx context.Context) error`（阻塞直到 `ctx` 取消；内部维护唯一连接）。

- [ ] **Step 1: 写失败的测试**

```go
func TestRun_ReconnectsAfterServerClose(t *testing.T) {
	// 假服务端第 1 次升级后回显一个包便关闭连接；
	// 断言：upgrades >= 2，且最终 State() == "connected"
}

func TestRun_SendsPingWhenKeepaliveElapses(t *testing.T) {
	// Keepalive=50ms；假服务端置 PingHandler 计数，500ms 内计数 >= 1
}

func TestRun_DropsPacketsWhileReconnecting(t *testing.T) {
	// 关闭后立即 Send 若干次：不阻塞、不 panic、返回 nil
}

func TestRun_StopsOnContextCancel(t *testing.T) // Run 在 1s 内返回，State() == "reconnecting" 或 "connecting"

func TestRun_ReturnsFatalOnCertificateError(t *testing.T) {
	// 服务端用自签证书且 Insecure=false；Run 必须直接返回错误（不无限重连），
	// 错误链里能 errors.As 到 x509.UnknownAuthorityError 或 tls.CertificateVerificationError
}
```

这三个并发用例必须用 `go test -race ./internal/wstunnel` 跑。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test -race ./internal/wstunnel -run TestRun -v`
Expected: FAIL（`Run` 未定义）

- [ ] **Step 3: 实现 `Run`**

循环：置状态 `connecting` → **用有界 context 调 `dialOnce`**（`context.WithTimeout(ctx, 10*time.Second)`，因为 `dialOnce` 自身没有握手超时）→ 成功后置 `connected` → 启动 ping ticker（`Keepalive > 0`，ping 走与数据帧相同的 `writeFrame` 路径，因此同样受写超时约束）→ 等待"本代连接结束或 ctx 取消" → 置 `reconnecting` → 退避（`BackoffMin` 起，×2，封顶 `BackoffMax`，±20% 抖动；一次成功连接后重置为 `BackoffMin`）→ 重试。

**读协程的所有权**：`dialOnce` 已经启动并独占本连接的读协程（gorilla 禁止同一连接上并发 `ReadMessage`）。`Run` **绝不能**再启动读协程；它只等待本代连接结束 —— 在 `dialOnce` 返回后于 `mu` 下快照本代的错误通道（`readErrCh`），然后 `select` 该通道与 `ctx.Done()`。通道在本代连接退役时关闭（由 `failConn` 完成），所以"连接结束"就是它可读。

`ctx` 取消时关闭连接并返回 `nil`。

**永久性错误不重试**：`dialOnce` 返回的 TLS 证书校验错误（`errors.As` 到 `*tls.CertificateVerificationError`、`x509.UnknownAuthorityError`、`x509.HostnameError`、`x509.CertificateInvalidError`）表示配置/证书问题，`Run` 立即把它返回给调用方（`cmd` 打印后以退出码 1 结束）。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test -race ./internal/wstunnel -v`
Expected: PASS（含 Task 3 的用例）

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(wstunnel): reconnect with backoff, keepalive pings"
jj new
```

---

## Task 5: `conn.Bind` 适配层

**Files:**
- Create: `internal/wgbind/bind.go`
- Test: `internal/wgbind/bind_test.go`

**Interfaces:**
- Consumes: `conn.Bind`（`Open/Close/SetMark/Send/ParseEndpoint/BatchSize`），`conn.ReceiveFunc`，`conn.Endpoint`。

**wireguard-go 的真实生命周期（务必按此实现）：** `device.BindUpdate()` 是 `bind.Close()` → `net.stopping.Wait()` → `bind.Open()`，而 `Up()` 开头就调 `BindUpdate()` —— 所以**首次 `Up()` 之前就会先 `Close()` 一次**。`Close` 因此必须只终止"当前代"、立刻唤醒阻塞的 `ReceiveFunc`（否则 `stopping.Wait()` 永久卡死），并且让随后的 `Open()` 重新可用；**绝不能关闭 transport**（transport 由 CLI 关闭）。
- Produces:
  ```go
  package wgbind
  type Transport interface {
      Send(pkt []byte) error
      // stop 关闭时返回 net.ErrClosed，用于让 Close 唤醒阻塞的接收而不关闭 transport。
      Recv(buf []byte, stop <-chan struct{}) (int, error)
  }
  func New(t Transport) *Bind
  ```

- [ ] **Step 1: 写失败的测试**

```go
func TestBind_SendOneFramePerBuffer(t *testing.T) {
	// fake transport 记录每次 Send 的字节；Send([][]byte{p1,p2,p3}) 后必须有 3 次调用且内容一一对应
}

func TestBind_BatchSizeIsOne(t *testing.T) // b.BatchSize() == 1

func TestBind_ReceiveCopiesAtOffset(t *testing.T) {
	// fake transport 返回 "hello"；断言 sizes[0]==5 且 packets[0][:5]=="hello"
	// 且 eps[0] 非 nil（pinned wireguard-go 的 ReceiveFunc 没有 offset 参数）
}

func TestBind_CloseUnblocksReceive(t *testing.T) // Close 后 ReceiveFunc 返回 net.ErrClosed
func TestBind_CloseThenOpenWorksAgain(t *testing.T) // wireguard-go 的 Close→Open 生命周期：第二轮 Open 的 ReceiveFunc 仍能收包
func TestBind_TransientErrorsAreRetried(t *testing.T) // Transport.Recv 连续返回错误时 ReceiveFunc 不返回错误，且在重试
func TestBind_CloseDoesNotCloseTransport(t *testing.T) // 关闭后 transport 仍可 Send（生命周期归 CLI）

func TestBind_ParseEndpointKeepsString(t *testing.T) // ep.DstToString() == "127.0.0.1:51820"
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/wgbind -v`
Expected: FAIL（`New` 未定义）

- [ ] **Step 3: 实现 `Bind`**

`Open` 忽略端口、建立新一代（自己的 stop 通道）并返回单个 `ReceiveFunc`；`ReceiveFunc` 循环调 `Transport.Recv(packets[0], stop)`，成功则写 `sizes[0]`/`eps[0]` 返回 1，`net.ErrClosed` 直接返回，其它错误按 50ms 重试（绝不上抛，否则 wireguard-go 接收协程会永久退出）；`Send` 对每个 buf 各调一次 `Transport.Send`；`SetMark` 返回 nil；`ParseEndpoint` 返回只保存原字符串的 endpoint；`Close` 只 `close` 当前代的 stop 通道（幂等），不碰 transport；`Close` 之后 `Open` 必须能再次工作。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/wgbind -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(wgbind): wireguard-go conn.Bind over the websocket transport"
jj new
```

---

## Task 6: device 组装

**Files:**
- Create: `internal/device/device.go`
- Test: `internal/device/device_test.go`

**Interfaces:**
- Consumes: `wireproxy.CreateIPCRequest(conf *DeviceConfig) (*DeviceSetting, error)`（`DeviceSetting{IpcRequest string; DNS, DeviceAddr []netip.Addr; MTU int}`）；`netstack.CreateNetTUN(addrs, dns []netip.Addr, mtu int) (tun.Device, *netstack.Net, error)`；`device.NewDevice(tun, bind, logger)`；`wireproxy.VirtualTun{Tnet *netstack.Net; Dev *device.Device; Conf *DeviceConfig; ResolveConfig *ResolveConfig; SystemDNS bool; PingRecord map[string]uint64; PingRecordLock *sync.Mutex; ...}`。
- Produces: `func Create(conf *wireproxy.Configuration, bind conn.Bind, logLevel int) (*wireproxy.VirtualTun, error)`。

- [ ] **Step 1: 写失败的测试**

```go
func TestCreate_ReturnsUsableVirtualTun(t *testing.T) {
	cfg, err := config.Load("../config/testdata/minimal.conf")
	vt, err := Create(cfg.WireProxy, conn.NewDefaultBind(), device.LogLevelSilent)
	// 断言 vt != nil, vt.Tnet != nil, vt.Dev != nil, vt.SystemDNS == true（minimal.conf 无 DNS）
	// 另用一个带 DNS 的 fixture（internal/device/testdata/dns.conf）断言 SystemDNS == false
	// cleanup: vt.Dev.Close()
}

func TestCreate_SetsResolveStrategyForV4Only(t *testing.T) {
	// minimal.conf 只有 v4 地址且 Resolve 缺省；断言 vt.ResolveConfig.ResolveStrategy == "ipv4"
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/device -v`
Expected: FAIL（`Create` 未定义）

- [ ] **Step 3: 实现 `Create`**

移植上游 `wireproxy.StartWireguard()` 的流程，唯一差别是使用传入的 `bind`；并保留上游对 `ResolveStrategy == "auto"` 的 v4/v6 判定。在文件头注明"derived from wireproxy v1.1.3 (ISC)"。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/device -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(device): assemble netstack device with a custom bind"
jj new
```

---

## Task 7: 运行状态文件

**Files:**
- Create: `internal/state/state.go`
- Test: `internal/state/state_test.go`

**Interfaces:**
- Produces:
  ```go
  type State struct {
      PID            int
      Transport      string // "off" | "wstunnel"
      TransportState string // "connected" | "connecting" | "reconnecting"
      TransportDetail string
      Updated        time.Time
  }
  func Write(path string, s State) error
  func Read(path string) (State, error)
  ```

- [ ] **Step 1: 写失败的测试**

```go
func TestWriteRead_RoundTrips(t *testing.T) // 全部字段相等（时间按 Unix 秒比较）
func TestWrite_IsAtomic(t *testing.T)       // 目录里不残留 "*.tmp"
func TestWrite_UsesMode0644(t *testing.T)
func TestRead_MissingFileReturnsError(t *testing.T)
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/state -v`
Expected: FAIL

- [ ] **Step 3: 实现**

`Write` 写 `<path>.tmp` → `os.Chmod(0644)` → `os.Rename`。格式严格为 `key=value` 每行一个：`pid`、`transport`、`transport_state`、`transport_detail`、`updated`（Unix 秒）。`Read` 逐行解析，未知键忽略，缺 `pid` 报错。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/state -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(state): atomic run/state file for status and webui"
jj new
```

---

## Task 8: CLI 入口

**Files:**
- Create: `cmd/wg-user/main.go`
- Test: `cmd/wg-user/main_test.go`

**Interfaces:**
- Consumes: `config.Load`（Task 1）、`wstunnel.New/Run/Send/Recv/State/Close`（Task 3/4）、`wgbind.New`（Task 5）、`device.Create`（Task 6）、`state.Write`（Task 7）。
- Produces: `func realMain(ctx context.Context, args []string, stdout, stderr io.Writer) int`（`main` 只做 `signal.NotifyContext` + `os.Exit`）。

- [ ] **Step 1: 写失败的测试**

```go
func TestRealMain_ConfigtestOK(t *testing.T) // args ["configtest","-c","../../internal/config/testdata/minimal.conf"] -> 0，stdout 含 "Config OK"
func TestRealMain_ConfigtestBadConfig(t *testing.T) // -> 2，stderr 含 "unsupported transport mode"
func TestRealMain_UnknownSubcommand(t *testing.T)   // -> 2，stderr 含 "unknown subcommand"
func TestRealMain_MissingConfig(t *testing.T)       // -> 2
func TestRealMain_RunStopsOnContextCancel(t *testing.T) // Mode=off + cancel 后 2s 内返回 0
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/wg-user -v`
Expected: FAIL（`realMain` 未定义）

- [ ] **Step 3: 实现**

子命令：空参或首个参数以 `-` 开头 → `run`；`-c` 缺省 `<exe>/../etc/wireproxy.conf`；`-v` 提高日志级别（`device.LogLevelVerbose`，否则 `LogLevelError`）。`version` 打印包级 `var version = "dev"`（构建时用 `-ldflags -X main.version=$(VERSION)` 覆盖）。

`run` 流程：`config.Load` → 若 `Mode==wstunnel` 用 `wstunnel.New(Config{URL, Path, Target: *peer.Endpoint, Insecure, Username, Password, Keepalive})` + `wgbind.New(client)` 并 `go client.Run(ctx)`，否则 `conn.NewDefaultBind()` → `device.Create` → 对每个 `cfg.WireProxy.Routines` 执行 `go r.SpawnRoutine(vt)` → `os.MkdirAll(filepath.Dir(statePath), 0755)` 后写一次 `state.Write(<config dir>/../run/state, ...)`，并起一个 1 秒轮询 goroutine 在传输状态变化时重写 → `<-ctx.Done()` → 关 device、关 client、删除 state 文件、返回 0。

`client.Run` 提前返回（TLS 证书错误等永久性错误）时：打印错误到 stderr，关闭 device，返回退出码 1（不要当成正常退出）。

日志前缀统一 `wg-user: `，写 stderr。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/wg-user -v`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(cli): wg-user run/configtest/version entrypoint"
jj new
```

---

## Task 9: 与真实 wstunnel 服务端的互通测试

**Files:**
- Create: `internal/wstunnel/interop_test.go`（`//go:build interop`）
- Modify: `Makefile`（新增 `interop` 目标）

**Interfaces:**
- Consumes: `wstunnel.New`、`Run`、`Send`、`Recv`（Task 3/4）；环境变量 `WSTUNNEL_SERVER_BIN`（官方服务端二进制路径）。
- Produces: `make interop` 目标。

- [ ] **Step 1: 写测试**

```go
//go:build interop

func TestInterop_UDPRoundTripThroughRealServer(t *testing.T) {
	// 1. 起 UDP echo 服务（127.0.0.1:0）
	// 2. 选空闲 TCP 端口，exec WSTUNNEL_SERVER_BIN server wss://127.0.0.1:<port> --restrict-to 127.0.0.1:<echo>
	// 3. client = New(Config{URL:"wss://127.0.0.1:<port>", Target:"127.0.0.1:<echo>", Insecure:true}); go client.Run(ctx)
	// 4. 等 State()=="connected"（10s 超时）
	// 5. 发 10 个随机长度（1..1200）的包，逐个校验回包内容完全相同
}

func TestInterop_TargetOutsideRestrictIsDropped(t *testing.T) {
	// Target 指向 restrict-to 之外的端口；2s 内收不到任何回包
}
```

服务端二进制缺失时 `t.Skip("WSTUNNEL_SERVER_BIN not set")`。

- [ ] **Step 2: 下载服务端并跑测试确认失败**

Run:
```bash
mkdir -p bin/interop && curl -fsSL -o /tmp/wstunnel.tar.gz \
  https://github.com/erebe/wstunnel/releases/download/v11.0.0/wstunnel_11.0.0_linux_amd64.tar.gz \
  && tar -xzf /tmp/wstunnel.tar.gz -C bin/interop wstunnel && chmod +x bin/interop/wstunnel
WSTUNNEL_SERVER_BIN=$PWD/bin/interop/wstunnel go test -tags interop ./internal/wstunnel -run Interop -v
```
Expected: FAIL（`make interop` 目标未定义前先看到测试本身跑不通或连接失败）

- [ ] **Step 3: 加 `Makefile` 目标并实现到通过**

```make
interop:
	@WSTUNNEL_SERVER_BIN=$${WSTUNNEL_SERVER_BIN:-$(BIN)/interop/wstunnel} \
	    go test -tags interop ./internal/wstunnel -run Interop -v
```

若互通失败，先比对 `Sec-WebSocket-Protocol`、路径（`/v1/events`）、claims 三处，再改实现（不要改测试期望的协议常量）。同时把 `bin/interop/` 加入 `.gitignore`（服务端二进制不入库）。

- [ ] **Step 4: 跑测试确认通过**

Run: `make interop`
Expected: PASS（两个用例）

- [ ] **Step 5: 提交**

```bash
jj describe -m "test(wstunnel): interop against the real v11 server"
jj new
```

---

## Task 10: 构建流水线

**Files:**
- Modify: `scripts/build.sh`, `Makefile`, `.gitignore`, `.tool-versions`
- Delete: `scripts/cargo-cross.sh`, `scripts/cargo-wrappers/`
- Test: `test/shell/build_test.sh`

**Interfaces:**
- Produces: `bin/<arch>/wg-user`（4 个架构）；`make build`、`make build-go` 等价。

- [ ] **Step 1: 写失败的测试**

```sh
#!/bin/sh
# test/shell/build_test.sh — 1) 4 个架构产物存在且不是脚本 2) amd64 产物能在本机跑
set -eu
ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd)
for a in arm64-v8a armeabi-v7a x86_64 x86; do
    [ -x "$ROOT/bin/$a/wg-user" ] || { echo "missing bin/$a/wg-user"; exit 1; }
done
"$ROOT/bin/x86_64/wg-user" version >/dev/null || { echo "amd64 binary does not run"; exit 1; }
```

- [ ] **Step 2: 跑测试确认失败**

Run: `sh test/shell/build_test.sh`
Expected: FAIL（`missing bin/arm64-v8a/wg-user`）

- [ ] **Step 3: 重写 `scripts/build.sh`、改 `Makefile`/`.gitignore`/`.tool-versions`、删 Rust 资产**

`scripts/build.sh` 保留现有 `TARGETS` 映射与 `GOPROXY` 默认，循环体改为 `go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o "$OUT_DIR/$arch_dir/wg-user" ./cmd/wg-user`（`VERSION` 从 `module.prop` 读，与现有 `Makefile` 的 `VERSION` 变量同源），且必须先 `go mod download`（在 `ROOT_DIR` 下，不再建临时模块）。`Makefile` 目标分工（CI 依赖这个划分）：`build: build-go`；`WPBINS/TNBINS` 换成 `GOWBINS := $(addprefix $(BIN)/,$(addsuffix /wg-user,$(ARCHS)))`；`test:` = `go vet ./... && go test ./...`；`verify:` = 所有脚本 `sh -n` + `sh test/shell/cli_test.sh` + `sh test/shell/install_test.sh`（不依赖 Go 工具链与已构建产物）；`build-go` 末尾加 `sh test/shell/build_test.sh`。`.gitignore` 用 `bin/*/wg-user` 替换两行旧规则，`etc/tunnel.conf` 改成 `etc/tunnel.conf*`。`.tool-versions` 增加一行 `golang 1.26.5`（asdf 只接受精确版本；`go.mod` 的 `1.26.0` 是语言下限，钉最旧的补丁级会把所有人拽回旧工具链）。`clean:` 改为 `rm -rf $(BIN)/*/wg-user $(BIN)/interop $(DIST)`，并且**不**调用 `webui-clean`——`webroot/{app.js,index.html,style.css}` 是已跟踪、随包发布的产物，`clean` 删掉它们会在 jj 自动快照里变成删除记录。`package`/`package-only` 归档前先校验四个 `$(GOWBINS)` 都是可执行普通文件，否则报错退出（`zip` 对缺失条目返回 0，会静默产出空 `bin/<arch>` 的包）。`PKG_FILES` 里删除 `etc/tunnel.conf.example`（文件本体由 Task 12 删除）。

- [ ] **Step 4: 跑测试确认通过**

Run: `make clean && make build && sh test/shell/build_test.sh && ls -l bin/*/wg-user`
Expected: PASS，四个产物各约 8–9 MB

- [ ] **Step 5: 提交**

```bash
jj describe -m "build: single go binary per arch, drop the rust pipeline"
jj new
```

---

## Task 11: shell CLI 与 service.sh

**Files:**
- Modify: `wg-user`, `common/abi.sh`, `service.sh`
- Test: `test/shell/cli_test.sh`

**Interfaces:**
- Consumes: `bin/wg-user`（Task 8 的 CLI：`run [-c FILE] [-v]`、`configtest [-c FILE]`、`version`）。
- Produces: `wg-user start|stop|restart|reload|status|log [N]|save-config <path>|version`；无 `tunnel` 子命令。

- [ ] **Step 1: 写失败的测试**

`test/shell/cli_test.sh` 构造一个假模块根（`module.prop`、`etc/wireproxy.conf`、`common/`、`bin/wg-user` 用 stub 脚本：`run` 时 `sleep 300`，`configtest` 时 `exit 0`，`version` 时 `echo 0.3.0`），所有命令都在该目录内以 `sh wg-user ...` 执行（`$0` 是相对路径，脚本据此解析模块根），然后断言：

```sh
sh wg-user status        # 退出码 1，输出含 "wg-user: stopped"，且不含 "tunnel:"
sh wg-user tunnel status # 退出码 2，输出含 "unknown subcommand"
sh wg-user start         # 退出码 0，run/wg-user.pid 存在
sh wg-user status        # 退出码 0，输出含 "wg-user: running (pid " 和 "transport: "
sh wg-user log 5         # 输出 log/wg-user.log 的最后 5 行（stub 写入的固定内容）
sh wg-user stop          # 退出码 0，run/wg-user.pid 不存在
```

- [ ] **Step 2: 跑测试确认失败**

Run: `sh test/shell/cli_test.sh`
Expected: FAIL（status 仍在找 `wireproxy.pid`/输出 `wireproxy:`）

- [ ] **Step 3: 重写三个脚本**

`wg-user`：删除 `tunnel` 命名空间、`spawn_tunnel`/`tunnel_*` 函数、`TNPIDFILE`/`TNLOG`/`TNCONF`；路径改为 `run/wg-user.pid`、`log/wg-user.log`、`bin/wg-user`；`start` 用 `nohup "$BIN" run -c "$CONF" > "$LOG" 2>&1 &` + `oom_score_adj` + `sleep 1` + `is_running`；`reload` 先 `"$BIN" configtest -c "$CONF"`；`status` 先按 pid 判活，再从 `run/state` 读 `transport`/`transport_state`/`transport_detail`（进程不存活时忽略状态文件；状态文件缺失时打印 `transport: unknown`）；`log` 只接受可选行数。`common/abi.sh`：`select_wireproxy_binary`/`select_wstunnel_binary` 合并为 `select_binary <src_dir> <dst_path>`（找 `bin/<arch>/wg-user`）。`service.sh`：启动一次 + 观察器只盯 `etc/wireproxy.conf`。

- [ ] **Step 4: 跑测试确认通过**

Run: `sh test/shell/cli_test.sh && sh -n wg-user service.sh common/*.sh`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(cli): single-process start/stop/status, drop the tunnel namespace"
jj new
```

---

## Task 12: 安装器、卸载器与配置迁移

**Files:**
- Modify: `customize.sh`, `uninstall.sh`, `etc/wireproxy.conf.example`
- Delete: `etc/tunnel.conf.example`
- Test: `test/shell/install_test.sh`

**Interfaces:**
- Consumes: `select_binary`（Task 11）、`log_info`/`log_warn`（`common/util.sh`）。
- Produces: 安装期自动迁移，旧 `etc/tunnel.conf` → `etc/wireproxy.conf` 的 `[Transport]`。

- [ ] **Step 1: 写失败的测试**

`test/shell/install_test.sh` 三组断言：

```sh
# A. 全新安装：无 etc/*.conf -> 播种 wireproxy.conf，权限 0600，含 "[Transport]"
# B. 升级迁移：预置 etc/tunnel.conf（mode=wstunnel, remote=wss://x:443, username=u, password=p,
#    local-udp=127.0.0.1:51820）与不含 [Transport] 的 etc/wireproxy.conf
#    -> wireproxy.conf 出现 [Transport]，Mode=wstunnel / URL=wss://x:443 / Username=u / Password=p
#    -> 不含 "local-udp"；tunnel.conf 消失；tunnel.conf.migrated 存在且权限 0600
# C. 幂等：对 B 的结果再跑一次 customize -> [Transport] 只出现一次
```

用 `MODPATH=<tmp> sh -c '. <tmp>/customize.sh'` 调用（脚本被 sourced，与真实安装一致）。测试必须把一个 `getprop` stub（`echo arm64-v8a`）放到 `PATH` 最前，否则开发机上 `get_device_arch` 得到 unknown，`select_binary` 会失败。

- [ ] **Step 2: 跑测试确认失败**

Run: `sh test/shell/install_test.sh`
Expected: FAIL（仍在调用 `select_wireproxy_binary`/`seed_config tunnel.conf`）

- [ ] **Step 3: 改安装器与示例配置**

`customize.sh`：调用 `select_binary "$MODPATH" "$MODPATH/bin/wg-user"`；`seed_config` 只处理 `wireproxy.conf`；新增 `migrate_tunnel_conf()`：当 `etc/tunnel.conf` 存在且 `wireproxy.conf` 里没有 `[Transport]` 时，用 `awk` 读出 `mode/remote/insecure/username/password` 并追加 `[Transport]` 段（键名首字母大写；空值不写），然后 `mv etc/tunnel.conf etc/tunnel.conf.migrated` 并 `chmod 0600`；`fix_mode` 的循环加入 `bin/wg-user` 由 `chmod -R 0755 bin` 覆盖。`uninstall.sh`：pid 清理改为 `run/wg-user.pid`、`run/watcher.pid`，并 `rm -f run/state`。`etc/wireproxy.conf.example` 追加 `[Transport]` 段（`Mode = off` 注释示例）。

- [ ] **Step 4: 跑测试确认通过**

Run: `sh test/shell/install_test.sh && sh -n customize.sh uninstall.sh`
Expected: PASS

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(install): migrate tunnel.conf into [Transport], ship one binary"
jj new
```

---

## Task 13: WebUI 去掉 tunnel 卡

**Files:**
- Modify: `webui/src/App.tsx`, `webui/src/api.ts`, `webui/src/i18n/en.ts`, `webui/src/i18n/zh.ts`, `webui/test/smoke.mjs`
- Modify（生成物）: `webroot/index.html`, `webroot/app.js`, `webroot/style.css`

**Interfaces:**
- Consumes: `wg-user status` 的新输出（`wg-user: running (pid N)` + `transport: <detail> (<state>)`），`wg-user log 50`，`wg-user save-config`。
- Produces: 单一配置卡 + 状态卡（含传输状态）。

- [ ] **Step 1: 改测试与类型**

`webui/test/smoke.mjs` 的假桥接改为返回新的 `status` 文本，并断言渲染结果里含 `transport`、不含 `tunnel`（中英两种语言）。`en.ts`/`zh.ts` 删除 `tunnel.*` 全部键（`tunnel.title/optional/start/stop/restart/config/load/save/loaded/saved/loadError/saveError/unknown`），新增 `transport.label`、`transport.connected`、`transport.reconnecting`、`transport.off`、`transport.unknown`。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd webui && pnpm run check`
Expected: FAIL（`App.tsx` 仍引用已删除的 i18n 键）

- [ ] **Step 3: 改 `App.tsx` / `api.ts`**

删除 tunnel 区块与 `tunnel start/stop` 调用；状态卡解析两行输出（进程行 + `transport:` 行），把 `connected|reconnecting|off` 映射到对应 i18n 文案；日志仍调 `wg-user log 50`。

- [ ] **Step 4: 跑测试并重建产物**

Run: `cd webui && pnpm run check && cd .. && make webui && git diff --stat webroot/`
Expected: PASS；`webroot/` 有更新

- [ ] **Step 5: 提交**

```bash
jj describe -m "feat(webui): one config card, transport status instead of tunnel controls"
jj new
```

---

## Task 14: CI 与 release 工作流

**Files:**
- Modify: `.github/workflows/ci.yml`, `.github/workflows/release.yml`

**Interfaces:**
- Consumes: `make verify`、`make interop`、`scripts/build.sh`（Task 10）、`webui`（Task 13）。
- Produces: 三个必需检查（`scripts`、`build-go`、`webui`）内容更新；release 不再构建 Rust。

- [ ] **Step 1: 改 CI**

`build-go` job 增加 `actions/setup-go`（`go-version: '1.26.x'`，取该系列最新补丁），步骤顺序：`make test` → 下载钉死版本的 wstunnel 服务端（`wstunnel_11.0.0_linux_amd64.tar.gz` + `sha256sum -c` + `chmod +x`）→ `make interop` → `make build`（末尾自带 `sh test/shell/build_test.sh`）。`scripts` job：`make verify`（只需 shell，不需要 Go）；"No real configs are tracked" 检查改为 `etc/wireproxy.conf`、`etc/tunnel.conf*` 都不能被跟踪；删除 NDK wrapper 可执行性检查（wrapper 已删）。

- [ ] **Step 2: 跑校验**

Run:
```bash
python3 -c "import yaml;yaml.safe_load(open('.github/workflows/ci.yml'));yaml.safe_load(open('.github/workflows/release.yml'))" && make verify
```
Expected: 无输出（YAML 合法）+ `make verify` PASS

- [ ] **Step 3: 改 release 工作流**

删除 Rust 交叉编译步骤与 NDK 依赖；保留 `make package` + `SHA256SUMS` 上传；`workflow_dispatch` 仍可干跑。

- [ ] **Step 4: 提交**

```bash
jj describe -m "ci: go-only build with interop gate, drop the rust job"
jj new
```

---

## Task 15: 文档与版本号

**Files:**
- Modify: `README.md`, `docs/ARCHITECTURE.md`, `docs/CONFIG.md`, `docs/TUNNEL-SERVER.md`, `docs/INSTALL.md`, `docs/CHANGELOG.md`, `module.prop`, `webui/package.json`

**Interfaces:**
- Consumes: 前 14 个任务的最终行为。
- Produces: 0.3.0 的可发布状态。

- [ ] **Step 1: 升版本**

`module.prop`：`version=0.3.0`、`versionCode` 在 0.2.0 基础上 +1；`webui/package.json` 的 `version` 同步为 `0.3.0`。

- [ ] **Step 2: 更新文档**

按 spec §10.4 的清单逐条改：README（数据流、构建要求去掉 Rust/NDK、zip 体积、CLI）、ARCHITECTURE（单进程、传输层内置）、CONFIG（单文件 + `[Transport]` 表 + Endpoint 双语义）、TUNNEL-SERVER（仅 VPS 侧，写明服务端版本钉死 v11.0.0 与互通测试要求）、INSTALL（无 Rust）、CHANGELOG（0.3.0 破坏性变更 + 自动迁移说明 + 回滚步骤：把 `etc/tunnel.conf.migrated` 改回 `tunnel.conf` 并重装 0.2.0）。

- [ ] **Step 3: 验证文档与实现一致**

Run:
```bash
node -p "require('./webui/package.json').version"   # 0.3.0
sed -n 's/^version=//p' module.prop                 # 0.3.0
grep -rn "tunnel.conf\|wstunnel client\|cargo-cross\|select_wireproxy_binary" --include='*.md' --include='*.sh' --include='Makefile' . | grep -v CHANGELOG
```
Expected: 两个版本号都是 `0.3.0`；最后一条 grep 无输出（CHANGELOG 的历史记录除外）

- [ ] **Step 4: 打最终包并核对体积**

Run: `make clean && make package && ls -l dist/`
Expected: `wg-user-0.3.0.zip` ≤ 15 MB（实测预期约 14 MB）

- [ ] **Step 5: 提交**

```bash
jj describe -m "docs: 0.3.0 — single binary, [Transport] config, no rust toolchain"
jj new
```

---

## 最终整分支验收（所有任务完成后）

1. `make clean && make package` → zip ≤ 15 MB，`unzip -l` 里每个 `bin/<arch>/` 只有 `wg-user`。
2. `make verify`、`make interop`、`cd webui && pnpm run check` 全绿。
3. `grep -rn "bin/wireproxy\|bin/wstunnel\|cargo-wrappers\|etc/tunnel.conf" --exclude-dir=.git --exclude-dir=dist .` 只命中 CHANGELOG 与 `.migrated` 相关说明。
4. 设备端按 spec §11.3 的 6 项清单手动验收（安装、SOCKS5 连通、飞行模式重连、热重载、WebUI 双语、单进程 + 无本地 51820 监听）。
