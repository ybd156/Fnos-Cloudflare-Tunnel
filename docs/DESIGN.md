# Cloudflare Tunnel for fnOS — 设计说明

本文记录应用的运行形态、关键不变量，以及历代版本修掉的缺陷，供后续维护参考。
包边界与导出接口的权威定义在 [ARCHITECTURE.md](ARCHITECTURE.md)。

## 运行形态

```
飞牛 OS 应用中心
  └── /var/apps/cloudflared/
      ├── target/            <- app/ 载荷（只读，升级会覆盖）
      │   ├── bin/cloudflared  <- 官方 cloudflared 二进制
      │   ├── bin/fncfd        <- 本服务，单个静态 Go 二进制
      │   ├── www/             <- 前端静态资源（index.html + js/ 模块）
      │   └── app.sock         <- 统一网关接入的 Unix socket
      ├── var/               <- $PKGVAR，持久数据
      │   ├── config.json        隧道定义 + 全局设置（0600）
      │   ├── .cloudflared/      cert.pem 与 <uuid>.json 凭据（0700）
      │   ├── tunnels/<id>/config.yml
      │   └── logs/<id>.log
      └── cmd/, wizard/, config/
```

生命周期脚本（`cmd/main`）只负责进程的启停与状态码；所有业务逻辑都在 Go 服务里。

**没有运行时依赖。** 1.4.1 及更早版本是一个 Node 服务，manifest 声明
`install_dep_apps = nodejs_v24`，靠 `cmd/_lib` 的 `find_node()` 去
`/var/apps/nodejs_v2*/target/bin/node` 逐个探测。1.4.2 起服务是 `bin/fncfd`，
包体自包含，manifest 不再声明任何依赖应用，也不再需要探测解释器。

## 后端模块

| 包 | 职责 |
| --- | --- |
| `internal/paths` | 从 `TRIM_*` 推导全部路径与资源上限；`Paths.EnsureDirs()` 是目录创建的唯一入口 |
| `internal/fsx` | 原子写文件、静默删除/改权限、私有文件写入 |
| `internal/applog` | 服务日志与 `0640` 的每隧道日志、轮转、有界尾部读取 |
| `internal/config` | `config.json` 的类型、默认值、规范化、入参校验与合并读写；向导设置合并 |
| `internal/cf` | 调用 cloudflared 二进制：一次性命令、argv/config.yml 生成、账户流程、自更新 |
| `internal/dnsroute` | 本地管理隧道的 CNAME 路由检查与修复 |
| `internal/runner` | 每隧道一个 cloudflared 子进程的监督、重启退避与运行时记录 |
| `internal/web` | 静态资源与 REST API，任何请求都不得把 panic 抛到进程外 |

依赖严格单向：`paths ← fsx ← applog`、`paths ← config ← cf ← dnsroute ← runner ← web`。
`config` 不 import 任何上层包 —— 旧实现里 `store.js` 为了一个域名正则反向
import `dns.js`，正是这次拆分的动因之一。

## 关键不变量

1. **`EnsureDirs()` 必须在任何持久化之前成功。** 它是目录创建的唯一入口；一旦它是空操作，
   `config.json`、`config.yml`、`cert.pem` 与所有日志都会写进不存在的目录。
2. **运行时记录归 `runner` 所有。** 外部只能通过 `Manager` 的方法访问，不得直接操作注册表
   —— 删除隧道时还要顺带清理自动重启定时器。
3. **凭据文件只按可证明的路径解析。** 显式路径，或 `<uuid>.json`。**不允许**“目录里第一个
   `<uuid>.json`”这种兜底：账户下有多个隧道时会静默跑起另一个隧道，症状是域名指向错误的源站，
   极难自查。只填名称时先经账户列表把名称解析成 UUID。
4. **URL 解码只允许经过受控路径。** 畸形百分号编码不得让进程退出；Go 的 `net/http` 在解析
   请求行时就会回 400，处理函数另有 panic 兜底。
5. **cloudflared 的子命令参数要放在子命令之后。** `--output` 是 `list` / `create` 的子命令
   选项，写在 `tunnel` 之后会被当作未知的隧道级参数而失败。
6. **Token 永不离开服务端。** 只通过 `TUNNEL_TOKEN` 环境变量传给子进程（不用 argv，因为
   `/proc/<pid>/cmdline` 对同机用户可读），API 只返回 `hasToken` 与掩码预览。
7. **隧道 id 必须能安全地当路径片段用。** 它会被拼进 `tunnels/<id>/config.yml` 与
   `logs/<id>.log`，所以规范化时只接受 `[A-Za-z0-9_-]{1,64}`，其余一律换成新 id。
   API 本身总会覆盖 id，这条守的是**磁盘上的** `config.json`（可能被手工编辑或损坏）。
8. **日志只读有界的尾部窗口，且只解码真正读到的字节。** 两个日志接口都以 `MaxTailBytes`
   为窗口；日志每 30 秒轮转一次，文件可能在 `stat` 与 `read` 之间缩小。
9. **前端轮询渲染必须幂等。** 只在生成的 HTML 真正变化时才写 DOM；日志正文不进入该 HTML
   而是就地更新。否则每 3 秒一次的重建会清掉滚动位置、焦点与正在阅读的日志。
10. **未知隧道 id 不得在监督器里留下记录。** logs / stop / restart 三条路由先校验隧道存在
    并返回 404，与其余 `:id` 路由一致，否则一个无需登录的请求就能让注册表无限增长。
11. **`config.json` 有两个写者。** 应用中心设置页由 `cmd/config_callback` 这个独立进程改写，
    服务进程则把配置读进内存。保存必须合并而不是整体覆盖：settings 谁后写谁生效，tunnels
    以服务进程为准。
12. **并发访问必须加锁。** 旧实现靠 Node 单线程，Go 下 HTTP 处理与进程监督是并发的，
    `config.Store` 与 `runner` 的运行时记录都在锁内读写。

## 1.3.1 修复的缺陷

| # | 位置 | 缺陷 | 影响 |
| --- | --- | --- | --- |
| A | `lib/paths.js` | 用了 `fs` 但没 `require('fs')` | `ensureDirs()` 是空操作，`$PKGVAR` 从未创建；首次安装后保存隧道、写 config.yml、登录、写日志全部失败 |
| B | `lib/dns.js` | 用了 `path` 但没 `require('path')` | `resolveTunnelUuid()` 抛 `ReferenceError`；「修复 DNS 路由」返回 500 |
| C | `lib/http.js` | 用了 `runtime` 但没引入注册表 | `DELETE /api/tunnels/:id` 抛 `ReferenceError` 返回 500（界面仍提示成功） |
| D | `lib/cloudflared.js` | 只给名称时猜凭据文件 | 多隧道账户下静默跑错隧道 |
| E | `lib/cloudflared.js` | `--output` 放在子命令之前 | 账户页列不出、也建不了隧道 |
| F | `lib/http.js` | 静态目录穿越用裸前缀比较 | `www-evil` 这类同前缀兄弟目录被当作 www 内部 |
| G | `lib/http.js` | 账户接口直接 `decodeURIComponent` | 畸形编码返回 500（与历史崩溃同一类） |

A–C 这一类的缺陷在 Go 下由编译器直接拦住：未 import 的包无法使用，因此
`tools/lint-requires.mjs` 被删除。

## 1.4.0 修复的缺陷与改动

| # | 位置 | 缺陷 | 影响 |
| --- | --- | --- | --- |
| L | `lib/log.js` | `readTailLines()` 忽略 `readSync` 的返回值 | 日志轮转后文件变小，缓冲区里未初始化的堆内存被解码成日志正文 |
| M | `lib/store.js` | 隧道 id 直接取自 `config.json` | 含 `..` 或路径分隔符的 id 会把 config.yml / 日志写到数据目录之外 |
| N | `lib/http.js` | 应用日志接口按 2MB 上限读窗口 | 每 3 秒一次轮询都拉 2MB 进内存并解码 |
| O | `www/*` | 轮询每 3 秒重建整个列表 | 展开的日志框被反复拉回顶部 |
| P | `www/app.js` | 选择器错误不会报错 | 处理器绑到 `null` 上，按钮静默失效；由 UI 选择器 lint 在构建时拦住 |

## 1.4.1 修复的缺陷

| # | 位置 | 缺陷 | 影响 |
| --- | --- | --- | --- |
| Q | `lib/store.js` | 保存时整体写回内存对象 | 设置页改完的项，只要再去网页界面编辑隧道就被旧值覆盖（两个页面都提示成功） |
| R | `lib/http.js` | `GET /api/tunnels/:id/logs` 未先确认隧道存在 | 任意字符串都会在监督器的 Map 里留下永久记录，内存单调上涨 |
| S | `tools/verify-fpk.py` | manifest 从不与源码比对；逐字节比对只有 13 个文件 | 版本号、changelog、`cmd/*`、`wizard/*` 全错也能绿灯 |
| T | `cmd/uninstall_callback` | 未清理 `server.log.1`、`VERSION`、`cloudflared.version` | 「删除数据」的卸载仍留下日志与标记文件 |
| U | `build.sh` | 前两步已处理 node 缺失，语法检查仍裸调 `node` | 在它专门兜底的主机上必然以 `command not found` 失败 |

## 1.4.2 运行时换成 Go

1.4.2 把后端从 Node 服务改成单个静态 Go 二进制，manifest 去掉
`install_dep_apps`。这不是修缺陷，但顺带解决了三个结构性问题：

| # | 位置 | 问题 | 影响 |
| --- | --- | --- | --- |
| V | `manifest` | `install_dep_apps = nodejs_v24` 不是文档化的依赖名（只有 `nodejs_v22`） | 依赖解析失败时应用装不上；装上后 `cmd/main` 也找不到 `node` |
| W | `cmd/_lib` | `find_node()` 硬编码探测多个候选路径 | 随依赖应用版本漂移，属于结构性脆弱 |
| X | `cmd/config_callback` | 用一段内联 Node 脚本改 `config.json` | 为了改一个 JSON 文件而需要整个运行时 |

现在 `cmd/config_callback` 改为调用 `fncfd config-set`，合并语义不变；服务二进制在
`app/bin/fncfd`，`cmd/main` 直接 exec 它。

### 迁移时修掉的一个新缺陷

`Store.Load()` 最初把整份文档反序列化进 `Settings` 结构体。`Settings` 的字段都在
`settings` 子对象下，顶层没有任何匹配键，于是**每次重启后所有全局设置都静默回到默认值**。
由 `TestStoreSaveLoadRoundTrip` 发现（它断言写入的 `protocol`/`logLevel`/`metricsPort`
能往返），现改为先取出 `settings` 原始片段再解码。写在这里是因为它说明了一件事：
迁移不是纯机械翻译，配置的读写路径必须逐字段验证。

## 测试

| 测试 | 命令 | 依赖 |
| --- | --- | --- |
| 全部单元测试 | `go test ./...` | 仅 Go |
| 竞态检测 | `CGO_ENABLED=1 go test -race ./...` | 需 cgo（Windows 上需 gcc；建议在 Linux 上跑） |
| 静态检查 | `go vet ./...` | 仅 Go |
| UI 选择器契约 | `go run ./tools/lintui` | 仅 Go |
| 生命周期脚本静态契约 | `go run ./tools/lintshell` | 仅 Go |
| JSON 配置校验 | `go run ./tools/validatejson ...` | 仅 Go |
| 模拟安装 | `setsid ./tools/test-lifecycle.sh` | 需 Linux + `/proc`、`curl` |

后端测试覆盖的关键回归：

- 配置双写者语义（外部 `config_callback` 的写入不被隧道编辑回滚；本进程的写入优先）
- 隧道 id 的路径安全（`../`、路径分隔符、超长、非法字符一律换新 id）
- Token 三条规则（空串=不修改、`null`=清空、缺省=保留）与「响应里不出现完整 Token」
- 凭据解析绝不猜（只有名称时返回空，而不是挑一个 `<uuid>.json`）
- `config.yml` 生成与 argv 顺序（`--output` 在子命令之后、Token 只进 `TUNNEL_TOKEN`）
- 请求体上限（413）、非 JSON（400）、畸形百分号编码（4xx 且进程存活）
- 静态目录穿越与 `www-evil` 同前缀兄弟目录
- 未知隧道 id 的所有 `:id` 路由返回 404，且不改动配置
- 日志尾部读取不返回窗口边界残行、不返回未初始化字节

> 新增回归测试时必须验证它**真的能失败**：把缺陷改回去，确认测试变红，再恢复。
> 本项目已有两次教训：一个测试因为窗口大于文件而永远命中不了目标代码，另一个因为断言取的是
> 行数而对窗口大小完全不敏感。写不出失败用例的测试等于没有测试。

### 前端测试的取舍

1.4.1 有一个基于 Node + 最小 DOM 桩的 UI 行为回归套件（42 项）。去掉 Node 后，在没有 JS
引擎的 Go 测试里无法执行浏览器代码，因此**该套件没有等价迁移**：现在前端只有静态契约检查
（`go run ./tools/lintui`：每个 `#id` 可解析、每个 class 有样式、`index.html` 的 id 唯一、
模块 import 目标存在）。

这是本次重构明确接受的覆盖损失 —— 它换来的是仓库彻底不依赖 Node。拆模块期间的行为等价性
由一次性的搬迁比对验证（与旧文件做行多重集对比，并逐项驱动轮询与渲染断言），未入库。
