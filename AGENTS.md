# AGENTS.md

给 AI 编码 agent 的仓库说明书。**先读这一页再动手**；`docs/ARCHITECTURE.md` 讲包边界与接口契约，
`docs/DESIGN.md` 讲运行形态与历代缺陷，本页只讲「怎么在这个仓库里安全地改东西」。

---

## 1. 这是什么

飞牛 OS（fnOS）的 Cloudflare Tunnel 应用：一个 Go 单静态二进制后端 + 原生 JS 前端，打成 `.fpk` 由应用中心安装。
**没有任何运行时依赖**（不要引入 Node/Python/Java）。

产物三件：

| 产物 | 位置 | 来源 |
| --- | --- | --- |
| 服务二进制 `fncfd` | `cloudflared/app/bin/fncfd` | `tools/build` 交叉编译（linux/amd64, CGO 关） |
| 官方 `cloudflared` | `cloudflared/app/bin/cloudflared` | `tools/fetchcf` 按 pin 下载 + 校验 SHA-256 |
| 安装包 | `dist/cloudflared.fpk` | `fnpack build` + 权限归一化 + 逐字节复核 |

两个二进制都在 `.gitignore` 里，**不要提交**。

---

## 2. 一分钟地图

| 路径 | 职责 | 改之前要知道 |
| --- | --- | --- |
| `cmd/fncfd/` | 服务入口：监听、单实例锁、PID、自动启动、`config-set` 子命令 | 服务同时监听 unix socket（网关）与可选 TCP（调试） |
| `internal/paths/` | `TRIM_*` → 全部路径与资源上限 | 路径只有一个来源，别在别处拼 |
| `internal/fsx/` | 原子写、静默删、改权限 | 写文件一律走这里 |
| `internal/applog/` | 服务日志 + 每隧道日志 + 轮转 + 有界尾部读取 | 轮转期间文件会缩小，读要按实际字节数 |
| `internal/config/` | `config.json`：类型、默认值、校验、合并读写、向导合并 | 合并语义见 §4.8 |
| `internal/cf/` | 调 cloudflared：exec、argv、config.yml、账户、自更新 | Token 只经 `TUNNEL_TOKEN` 环境变量 |
| `internal/dnsroute/` | 本地管理隧道的 CNAME 检查与修复 | DoH 失败要回退系统解析器 |
| `internal/runner/` | 每隧道一个子进程的监督、退避重启、源站探测 | 运行时记录有锁 |
| `internal/web/` | 静态资源 + REST API | 未知 id 一律 404 |
| `cloudflared/` | fnpack 工程目录（目录名必须等于 appname） | `manifest` 是版本唯一来源 |
| `cloudflared/cmd/` | 生命周期脚本（`install_*` / `upgrade_*` / `uninstall_*` / `main` / `config_*` + `_lib`） | 必须 `100755`，见 §4.5 |
| `tools/` | 构建与校验工具，全是 Go | 不要写 .py/.mjs |
| `.github/workflows/` | `build.yml`（测试/打包/发版）、`pin.yml`（每周跟进 cloudflared） | 发版只走 tag |

依赖方向：`paths` 最底层，`web` 最上层；`internal/` 内部严格单向，不许成环。

---

## 3. 命令

```bash
# 构建与校验（薄封装，转调 go run ./tools/build）
./build.sh                # 图标 → 交叉编译 → 权限归一化 → 各 lint → fnpack → 复核
./build.sh --no-pack      # 到校验为止，不打包（没有 fnpack 的机器用这个）
./build.sh --no-icons     # 跳过图标生成

# 测试
go vet ./...
go test ./...
go test -race ./...                     # 需要 cgo（CI 上有；本地要 Linux 环境）
setsid bash tools/test-lifecycle.sh     # 需要 Linux；模拟安装→启动→API→设置→卸载

# 单项工具
go run ./tools/lintui                   # 前端选择器契约（#id 可解析、class 有样式、import 存在）
go run ./tools/lintshell                # 生命周期脚本静态契约
go run ./tools/validatejson             # 全部 JSON 配置
go run ./tools/fetchcf -check           # 本地 cloudflared 是否等于 pin
go run ./tools/fetchcf -latest          # 把 pin 跟到上游最新版（下载+验 ELF+算哈希+改 pin）
go run ./tools/fpk verify dist/cloudflared.fpk   # 产物逐字节复核
```

**构建宿主只支持 Linux**（CI 的 ubuntu runner，或任意 Linux/WSL）：`chmod`、`bash -n`、`fnpack` 都是 Linux 语义。

---

## 4. 硬约束（违反会静默出错或直接发坏包）

1. **只用标准库。** `go.mod` 没有第三方依赖，构建不联网拉包。加依赖等于让 NAS 上无法构建。
2. **`cloudflared/manifest` 的 `version` 必须等于 git tag**（`v1.4.10` ↔ `version = 1.4.10`）。
   CI 会断言，不一致直接红。
3. **服务二进制必须 `CGO_ENABLED=0 GOOS=linux GOARCH=amd64`**，否则装到 NAS 上跑不起来。
4. **归档权限位必须归一化。** `fnpack` 从宿主 `stat()` 取模式且不校验；模式错了，
   `cmd/main` 与两个二进制到了 NAS 不可执行，而打包仍打印成功。`tools/fpk normalize` 负责改写，别绕过。
5. **脚本必须 `100755`。** 在会丢执行位的环境（Windows 检出、某些挂载）里 git 会把它记成
   `100644`，Linux 上 `./build.sh` 就是 `Permission denied`（exit 126）。改完跑
   `git update-index --chmod=+x <file>` 确认。
6. **AF_UNIX 路径 ≤ 108 字节。** 服务在载荷目录里绑 `app.sock`；CI 的
   `/home/runner/work/<repo>/<repo>/` 会让路径超限、bind 失败、后面所有 API 断言变成空响应。
   测试工作目录必须短（`tools/test-lifecycle.sh` 已用 `mktemp -d`，可用 `FNCFD_LIFECYCLE_SCRATCH` 覆盖）。
7. **Token 不进命令行、不回浏览器。** 启动隧道用 `TUNNEL_TOKEN` 环境变量；API 只返回
   `hasToken` 与 `tokenPreview`。Linux 上任何本地用户都能读 `/proc/<pid>/cmdline`。
8. **`config.json` 合并语义**：`settings` 谁后写谁生效（本进程改过就以内存为准，否则以磁盘为准），
   `tunnels` 始终以服务进程为准；未知顶层键原样保留。设置页回调与网页端不能互相抹掉。
9. **未知隧道 id 的每条 `:id` 路由都返回 404，且不得创建运行时记录。** 否则一个无需登录的请求
   就能让监督器的 Map 无限增长。
10. **cloudflared 的 pin 只能经 `tools/fetchcf -latest` 更新**（版本与 SHA-256 同时改）。
    只改版本不改哈希，`tools/fpk verify` 会失败——这正是它存在的意义。
11. **`.gitattributes` 强制 LF。** CRLF 的 shell 脚本在 NAS 上报 `\r: command not found`。
12. **不要手工往 Release 上传本地打的包。** 包不是字节级可复现的（`app.tgz` 的 md5 写进 manifest，
    tar 时间戳每次变），手工上传会让产物与提交失去对应关系。发版只推 tag。

---

## 5. 代码约定

- **错误信息面向 NAS 用户**：中文、说清「发生了什么 + 下一步做什么」，不要裸露 `err.Error()` 之外的东西。
- **每个包一个职责**，跨包只经导出的窄接口；不要为了省一个函数反向 import。
- **状态要加锁**：配置与运行时记录都有并发访问（HTTP 处理器 + 监督器 + 回调子进程）。
- **前端 6 个 ES 模块**（`core/api/render/modal/views/app`）：无框架、无构建步骤。
  新增选择器必须同时在 `index.html` 有对应元素、`style.css` 有对应规则，否则 `tools/lintui` 红。
- **测试先证明能失败**：新增回归用例时，把缺陷改回去确认它变红，再恢复。
  仓库里已有两次教训（窗口大于文件导致用例永远命中不了目标代码；断言取行数而对窗口大小不敏感）。

---

## 6. 改一个功能的标准流程

1. 先 `git log --oneline -10` 看同类改动是怎么做的。
2. 改代码 → `go vet ./... && go test ./...`。
3. 涉及前端 → `go run ./tools/lintui`；涉及生命周期脚本 → `go run ./tools/lintshell` 且确认执行位。
4. 涉及打包/清单 → `./build.sh --no-pack`（会跑完除 fnpack 外的全部检查）。
5. 有 fnpack 时 `./build.sh`，再 `go run ./tools/fpk verify dist/cloudflared.fpk`。
6. 用户可见的改动：改 `cloudflared/manifest` 的 `version`（patch +1）与 `changelog`（写在最前面），
   并在 README 的对应章节补一句。
7. 提交信息用 `fix(1.4.x):` / `feat(1.4.x):` / `ci:` / `docs:` 前缀，正文说清「为什么」。

---

## 7. 发布

```bash
sed -i 's/^version = .*/version = 1.4.11/' cloudflared/manifest   # 版本唯一来源
# 写 changelog（同一个文件，应用中心会显示）
git commit -am 'chore: 1.4.11'
git tag -a v1.4.11 -m '1.4.11'
git push origin main --tags        # 推 tag 触发 CI：测试 → 打包 → 发 Release
```

CI 会：跑测试（含只能在 Linux 跑的模拟安装套件）→ 跟进 cloudflared 最新版 → 打包 →
断言 tag 与 manifest 版本一致 → 上传 `cloudflared-<版本>.fpk` + `.sha256` 到 Release。

---

## 8. 不要做的事

- 不要引入第三方 Go 模块、Node、Python。
- 不要把 `cloudflared` 或 `fncfd` 提交进仓库（40MB / 构建产物）。
- 不要手改 `tools/internal/cfpin/cfpin.go` 的哈希去「让它过」——用 `fetchcf -latest`。
- 不要在 `internal/` 里反向依赖（例如状态层 import 路由层）。
- 不要用 `cat >` 盲写 `config.json`（会丢字段与权限），走 `config` 包。
- 不要在日志/响应里回显完整 Token。
- 不要把 `.fpk` 手工上传到 Release，也不要跳过 `tools/fpk verify`。
