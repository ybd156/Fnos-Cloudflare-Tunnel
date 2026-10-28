# 架构与接口契约

本文件是重构后的**权威设计说明**：包边界、每个包的导出接口、以及不可破坏的不变量。
并行开发时以本文件的签名为准，实现方不得擅自改签名；确需改动先改这里。

## 为什么换掉 Node

1.4.1 的后端是一个 Node 服务，manifest 里声明 `install_dep_apps = nodejs_v24`。
这带来两个问题：

- **依赖名不存在于官方文档。** `FPK-SPEC.md` §12 已记录：官方只列出
  `nodejs_v22` / `python312` / `java-21-openjdk`，`nodejs_v24` 未被收录。依赖解析
  失败时应用装不上，或装上后 `cmd/main` 找不到 `node`。
- **依赖应用的 bin 不自动进 PATH。** 旧 `cmd/_lib` 因此要硬编码探测
  `/var/apps/nodejs_v2*/target/bin/node` 等多个候选路径，脆弱且随版本漂移。

改成 **单个静态 Go 二进制**（`app/bin/fncfd`）后，包体自包含，manifest 不再声明
任何运行时依赖，也没有"探测解释器"这一步。开发与构建工具链同样用 Go，仓库里
不再需要 Node。

前端仍然是**浏览器里的原生 JS**（无框架、无构建步骤），与 Node 无关。

## 目录结构

```
cmd/fncfd/main.go            进程入口：监听、自动启动、优雅关停
internal/paths/              TRIM_* → 全部路径与资源上限（Paths 值，依赖注入）
internal/fsx/                原子写、静默删除/改权限
internal/applog/             服务日志 + 每隧道日志 + 轮转 + 有界尾部读取
internal/config/             config.json：类型、默认值、规范化、校验、合并读写
internal/cf/                 cloudflared 相关：exec 封装、argv/config.yml、账户、自更新
internal/dnsroute/           本地管理隧道的 CNAME 路由检查与修复
internal/runner/             每隧道一个子进程的监督、退避重启、运行时记录
internal/web/                HTTP 层：静态资源 + REST API
tools/                       构建与校验工具（全部 Go）
cloudflared/                 fnpack 工程目录（载荷、cmd/*、wizard/*、config/*）
```

依赖方向严格单向，不得出现环：

```
paths ← fsx ← applog
paths ← config ← cf ← dnsroute ← runner ← web ← cmd/fncfd
```

`config` **不得** import `cf`/`dnsroute`/`runner`/`web`。旧实现里
`store.js` 为了一个域名正则去 import `dns.js`（状态层反向依赖路由层），是本次
拆分的直接动因之一。

## 工程规则

- **只用标准库**，`go.mod` 不出现任何第三方 require。理由：NAS 上无法 `go mod
  download`，且项目本来就以零依赖为卖点。
- `go 1.22`。`gofmt` 干净，`go vet ./...` 无告警。
- 用户可见的错误信息一律中文，且与旧实现**逐字一致**（测试按原文断言）。
- 时间戳格式与旧实现一致：服务日志 `2026-10-28T05:42:49.842Z`（UTC，毫秒），
  隧道日志行前缀 `05:42:49`。
- 任何请求处理路径都不得 panic 到进程外；`web` 层有 recover 兜底。

## 接口契约

### internal/paths

```go
type Paths struct { /* AppName, AppDest, PkgVar, WWW, CFBin, ServerBin, SocketPath,
                       GatewayPrefix, CFHome, CFDir, CertFile, TunnelsDir, LogsDir,
                       ConfigFile, AppLog, CFVersionFile, PIDFile, TCPHost, TCPPort */ }
func FromEnv() Paths
func (p Paths) EnsureDirs() error
func (p Paths) TunnelConfig(id string) string
func (p Paths) TunnelLog(id string) string
func (p Paths) CredentialFile(uuid string) string
```

常量：`MaxLogLines` `MaxBodyBytes` `MaxAppLogBytes` `MaxTunnelLogBytes`
`MaxTailBytes` `LogRotateCheckMS` `RestartBaseMS` `RestartMaxMS`
`RestartMaxAttempts` `RestartStableMS`。

### internal/applog

```go
type Logger struct { Out, Err io.Writer; /* ... */ }
func New(appLog string) *Logger
func (l *Logger) Info/Warn/Error(parts ...any)
func (l *Logger) AppendPrivate(file, line string, maxBytes int64)
func (l *Logger) RotateAppLogNow()
func (l *Logger) Close()
func ReadTailLines(file string, maxBytes int64, maxLines int) []string
```

### internal/config

```go
type Settings, Ingress, Tunnel, Config   // json 字段名即磁盘契约，不得改
func DefaultSettings() Settings
func DefaultConfig() Config
func NewID() string
func MaskSecret(secret string) string
func ValidTunnelName(name string) bool
func NormalizeTunnel(raw rawTunnel) Tunnel
func MergeTunnel(existing *Tunnel, body map[string]json.RawMessage) (Tunnel, error)
func ValidateTunnel(t Tunnel) []string
var SafeIDRE, UUIDRE, DNSHostnameRE
func (t Tunnel) Clone() Tunnel
func (c Config) Clone() Config
type Store struct{ /* ... */ }
func NewStore(path string, log *applog.Logger) *Store
func (s *Store) Load()
func (s *Store) Save() error
func (s *Store) Snapshot() Config
func (s *Store) Settings() Settings
func (s *Store) SetSettings(next Settings)
func (s *Store) Tunnels() []Tunnel
func (s *Store) Tunnel(id string) (Tunnel, bool)
func (s *Store) Add(t Tunnel)
func (s *Store) Replace(t Tunnel) bool
func (s *Store) Update(id string, fn func(*Tunnel)) bool
func (s *Store) Delete(id string) bool
```

### internal/cf

所有返回 `error` 的函数，`error.Error()` 就是可以直接回给界面的中文消息。

```go
type Runner struct{ /* ... */ }
func New(p paths.Paths, log *applog.Logger) *Runner

type ExecOptions struct { Timeout time.Duration; Env map[string]string; Dir string }
type ExecResult struct { Code int; Stdout, Stderr, Err string }

func (r *Runner) Env(extra map[string]string) []string
func (r *Runner) Exec(args []string, opts ExecOptions) ExecResult   // 从不返回错误
func (r *Runner) Version(refresh bool) string                       // 二进制缺失时返回 ""
func SplitArgs(s string) []string
func GlobalArgs(s config.Settings) []string
func BuildConfigYAML(t config.Tunnel, credentialsFile string) string
func (r *Runner) BuildArgs(t config.Tunnel, s config.Settings) (args []string, env map[string]string, err error)
func (r *Runner) ResolveCredentials(t config.Tunnel) string         // 无法证明时返回 ""
func (r *Runner) WriteTunnelConfig(t config.Tunnel, cred string) (string, error)
func (r *Runner) ResolveTunnelIDByName(name string) string

type LoginStatus struct {
    LoggedIn  bool   `json:"loggedIn"`
    Status    string `json:"status"`
    URL       string `json:"url"`
    Error     string `json:"error"`
    StartedAt string `json:"startedAt"`
    CertPath  string `json:"certPath"`
}
func (r *Runner) LoginStart() error
func (r *Runner) LoginStatus() LoginStatus
func (r *Runner) Logout()

type AccountTunnel struct {
    ID          string `json:"id"`
    Name        string `json:"name"`
    CreatedAt   string `json:"createdAt"`
    Connections int    `json:"connections"`
}
func (r *Runner) ListTunnels() ([]AccountTunnel, error)
func (r *Runner) CreateTunnel(name string) (raw json.RawMessage, output string, err error)
func (r *Runner) DeleteTunnel(name string) error
func (r *Runner) RouteDNS(name, hostname string) error

func (r *Runner) AppVersion() string
func (r *Runner) UpdateBinary() (version string, err error)
```

### internal/dnsroute

```go
type Failure struct { Hostname string `json:"hostname"`; Error string `json:"error"` }
type Result struct {
    OK        bool      `json:"ok"`
    Routed    []string  `json:"routed"`
    AlreadyOK []string  `json:"alreadyOk"`
    Failed    []Failure `json:"failed"`
    Skipped   bool      `json:"skipped"`
    Message   string    `json:"message"`
}
type Resolver struct{ /* ... */ }
func New(p paths.Paths, log *applog.Logger, runner *cf.Runner) *Resolver
func (r *Resolver) Ensure(t config.Tunnel) Result
func (r *Resolver) Hostnames(t config.Tunnel) []string
func (r *Resolver) UUID(t config.Tunnel) string
func (r *Resolver) Remember(name, hostname, target string)
func (r *Resolver) CacheFresh(name, hostname string) bool
func (r *Resolver) LookupCNAME(hostname string) (string, error)
```

### internal/runner

```go
type View struct { /* json 字段与旧 tunnelView 一致：id,name,type,enabled,autoStart,
                     autoDns,target,quickHostname,tunnelName,tunnelId,
                     credentialsFile,configFile,ingress,catchAll,extraArgs,note,
                     hasToken,tokenPreview,status,pid,url,error,startedAt,stoppedAt,
                     lastExitCode,lastExitAt,attempts,dnsStatus,dnsResults,
                     dnsMessage,dnsRunning */ }
type Manager struct{ /* ... */ }
func New(p paths.Paths, log *applog.Logger, store *config.Store,
         runner *cf.Runner, dns *dnsroute.Resolver) *Manager
func (m *Manager) Start(id string) error
func (m *Manager) Stop(id string)
func (m *Manager) StopAll()
func (m *Manager) Forget(id string)
func (m *Manager) Running(id string) bool            // 是否有存活子进程（PUT 判断要不要重启）
func (m *Manager) View(t config.Tunnel) View
func (m *Manager) Logs(id string, limit int) []string
func (m *Manager) ApplyLocalConfig(t config.Tunnel)
func (m *Manager) SetDNSResult(id string, res dnsroute.Result)
func (m *Manager) RunDNSRoutes(id string, force bool)
func (m *Manager) WaitIdle(timeout time.Duration)   // 关停时等子进程真正退出
```

## 不变量（回归测试必须覆盖）

1. **`EnsureDirs()` 必须先于任何持久化成功**，且真的创建目录。
2. **运行时记录归 `runner` 所有**；删除隧道要顺带清掉重启定时器。
3. **凭据文件只按可证明路径解析**：显式路径或 `<uuid>.json`，绝不"目录里第一个"。
4. **URL 解码只经 `safeDecodePath`**；畸形百分号编码返回 400 而不是崩进程。
5. **cloudflared 子命令参数必须在子命令之后**（`tunnel list --output json`）。
6. **Token 永不离开服务端**：只经 `TUNNEL_TOKEN` 环境变量传给子进程，API 只回
   `hasToken` 与掩码。
7. **隧道 id 必须能安全当路径片段**：只接受 `[A-Za-z0-9_-]{1,64}`。
8. **日志只读有界尾部**，且只解码真正读到的字节。
9. **前端轮询渲染幂等**：内容不变就不碰 DOM，日志正文不内联进列表 HTML。
10. **未知隧道 id 不得在监督器里留下记录**：logs/stop/restart 先校验存在性。
11. **设置页与 `config_callback` 双写者互不覆盖**：settings 后写者赢，tunnels 以本进程为准。

## 测试

```bash
go test ./...        # 全部单元测试，纯逻辑，任何平台都能跑
go vet ./...         # 静态检查（原先的 lint-requires 由编译器取代）
```

`tools/test-lifecycle.sh` 仍是唯一需要 Linux 的套件（真实跑一遍生命周期脚本），
它在 NAS 上执行；Windows 上跳过。
