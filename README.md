# Cloudflare Tunnel for fnOS（飞牛 OS）

把 Cloudflare Tunnel（内网穿透）带到飞牛 OS 的应用。内置官方 `cloudflared`
客户端，可在**不开放任何路由器端口**的前提下，把 NAS 上的服务安全发布到公网，
并自动获得 HTTPS 证书。

## 功能

| 隧道类型 | 说明 | 是否需要 Cloudflare 账户 |
| --- | --- | --- |
| **Token 隧道** | 粘贴 Zero Trust 控制台生成的隧道 Token，由云端远程下发配置，适合生产环境 | 需要 |
| **快速隧道** | 一键获得 `*.trycloudflare.com` 临时公网地址 | 不需要 |
| **本地管理隧道** | 在应用内完成账户授权，创建/删除隧道、配置 ingress 路由、自动添加 DNS 记录 | 需要 |
| **自定义配置** | 直接使用您自己编写的 `config.yml` | 视配置而定 |

其他特性：

- **没有任何运行时依赖**：服务是单个静态 Go 二进制，装完即用，不需要 Node/Python/Java
- 每个隧道一个独立 `cloudflared` 进程，互不影响
- 异常退出后按指数退避自动重启（5 秒起，最长 60 秒，最多 10 次；稳定运行 120 秒后重置计数）
- 实时日志查看（环形缓冲 600 行，同时落盘）
- 一键启停 / 重启 / 编辑 / 删除
- 隧道 Token 只保存在服务端（`0600` 权限），**永不回传浏览器**，界面仅显示掩码预览
- 界面支持浅色 / 深色模式自动切换
- 可一键从 GitHub 更新 `cloudflared` 二进制

## 目录结构

```
fnos-cloudflared/
├── go.mod                    # Go 模块（只用标准库，无第三方依赖）
├── cmd/fncfd/                # 服务入口：监听、自动启动、优雅关停
├── internal/                 # 后端包，依赖严格单向
│   ├── paths/                #   TRIM_* → 全部路径与资源上限
│   ├── fsx/                  #   原子写、静默删除/改权限
│   ├── applog/               #   服务日志 + 每隧道日志 + 轮转 + 有界尾部读取
│   ├── config/               #   config.json：类型、规范化、校验、合并读写
│   ├── cf/                   #   调用 cloudflared：exec、argv/config.yml、账户、自更新
│   ├── dnsroute/             #   本地管理隧道的 CNAME 路由检查与修复
│   ├── runner/               #   每隧道一个子进程的监督与退避重启
│   └── web/                  #   静态资源 + REST API
├── cloudflared/              # fnpack 工程目录（目录名必须等于 appname）
│   ├── manifest              # 应用元信息（无 install_dep_apps）
│   ├── ICON.PNG / ICON_256.PNG
│   ├── app/                  # 应用载荷，安装后落到 /var/apps/cloudflared/target/
│   │   ├── bin/cloudflared   # 官方 cloudflared 二进制（不随仓库分发，见下）
│   │   ├── bin/fncfd         # 本服务二进制（构建产物）
│   │   ├── www/              # 前端：index.html + js/ 六个 ES 模块 + style.css
│   │   └── ui/config         # 应用入口与图标配置
│   ├── cmd/                  # 生命周期脚本（含共用库 _lib）
│   ├── config/               # privilege + resource
│   └── wizard/               # 安装 / 升级 / 卸载 / 设置向导
├── tools/                    # 构建与校验工具，全部 Go
│   ├── build/                #   唯一的构建流水线（build.sh/ps1 都转调它）
│   ├── fetchcf/              #   下载固定版本 cloudflared 并校验 SHA-256
│   ├── icons/                #   纯标准库图标生成器
│   ├── fpk/                  #   归档权限归一化 + 产物校验
│   ├── validatejson/         #   JSON 配置校验
│   ├── lintui/               #   UI 选择器契约检查
│   ├── lintshell/            #   生命周期脚本静态检查
│   └── test-lifecycle.sh     #   模拟安装：真实跑一遍生命周期脚本（需 Linux）
├── docs/
│   ├── ARCHITECTURE.md       # 包边界、导出接口契约、工程规则
│   └── DESIGN.md             # 运行形态、关键不变量、历代缺陷记录
├── build.sh                  # 薄封装：定位 go 后转调 tools/build
├── build.ps1                 # 同上（Windows）
└── dist/cloudflared.fpk      # 构建产物（本地产物，不随仓库分发）
```

## 从 1.4.1 升级

1.4.2 把后端从 Node 服务换成了 Go 静态二进制：

- manifest 不再声明 `install_dep_apps`，**不再需要 `nodejs_v24`**（这个依赖名本来也不在
  官方依赖文档里，只有 `nodejs_v22`）。升级后可以卸载它。
- `config.json` 的字段名、默认值与合并语义完全沿用，**隧道不需要重新配置**。
- 账户凭据 `cert.pem` 与各隧道凭据原地保留。

## 构建

需要 Go ≥ 1.22；打包还需要官方 `fnpack`（本项目已在 `/usr/local/bin/fnpack`，
版本 1.2.4，下载地址 <https://static2.fnnas.com/fnpack/>）。**不再需要 Node 或 Python。**

```bash
./build.sh              # 重新生成图标 → 校验 → 交叉编译 → 打包
./build.sh --no-icons   # 跳过图标生成
./build.sh --no-pack    # 只跑全部校验，不打 .fpk（没有 fnpack 的机器用这个）
```

Windows 上用等价封装：

```powershell
pwsh -File build.ps1
pwsh -File build.ps1 -NoIcons -Fnpack C:\tools\fnpack-1.2.4.exe
```

> **在 Windows 上建议改用 WSL 构建。** 目标平台是 Linux，在 WSL 里构建能省掉
> 整类 Windows 特有的麻烦：`chmod` 有真实的权限位（归档不需要再归一化，实测
> Linux 上修正 0 个、Windows 上要修正 23+23 个）、`bash -n` 能真跑、`fnpack`
> 用官方 Linux 版、也不受 `%TEMP%` 不可写的影响。
>
> ```bash
> wsl -e bash -lc 'cd ~/fncfd-build && ./build.sh'
> ```
>
> 官方 Linux 版 fnpack：`https://static2.fnnas.com/fnpack/fnpack-1.2.3-linux-amd64`。
> Windows 路径仍然受支持（`build.ps1` 会自动把临时目录指到 `work/tmp`），只是
> 多一道权限位修正。

两个脚本都只是**薄封装**：解析自己的参数、定位 `go` 与 `fnpack`，然后转调
`go run ./tools/build`。流水线只存在一份，不会像 1.4.1 那样两份脚本各自漂移。

流水线依次完成：

1. 用 `tools/fetchcf` 确保存在固定版本的 `cloudflared` 二进制并校验 SHA-256
2. 用 `tools/icons` 生成 6 个尺寸的图标
3. **交叉编译服务二进制**：`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath
   -ldflags="-s -w" -o cloudflared/app/bin/fncfd ./cmd/fncfd`（约 7.8MB 静态二进制）
4. 归一化权限位
5. `bash -n` 校验全部生命周期脚本
6. `go vet` + `go test` 静态检查
7. `tools/lintui` 检查前端选择器契约（扫描 `www` 下全部 `.js`）
8. `tools/lintshell` 静态检查生命周期脚本
9. `tools/validatejson` 校验全部 JSON 配置
10. 检查必需文件是否齐全
11. 调用 `fnpack build` 产出 `dist/cloudflared.fpk`
12. `tools/fpk` 修正**归档内**的权限位并复核产物与源码一致，打印大小与 SHA-256

> **关于 `app/bin/cloudflared`。** 这个 38MB 的二进制**不在仓库里**（见 `.gitignore`）：
> 它是 Cloudflare 官方发布的构建产物，本仓库没有任何人修改它，提交进 Git 只会让每次
> 克隆都拖下 38MB，并让它永久留在提交历史中。取而代之的是 `tools/fetchcf`：它按固定
> 版本下载官方 release，**校验 SHA-256 后**才落盘。`app/bin/fncfd` 同理是构建产物。
> 打包好的 `.fpk`（其中当然包含这两个二进制）作为 GitHub Release 附件发布。

> **为什么需要归档权限归一化。** `fnpack` 的权限位取自宿主文件系统的 `stat()`。Linux 上
> 第 4 步的 `chmod` 已经生效；但 Windows 没有可执行位，于是每个文件都变成 `0666`、
> 目录变成 `0777` —— `cmd/main` 和两个二进制到了 NAS 上是**不可执行**的，应用根本起不来。
> `tools/fpk` 把两层 tar 的权限位改写回 Linux 的正确值，并同步修正 manifest 里的
> `checksum`（它是新 `app.tgz` 的 MD5）。

> **为什么还要复核产物。** `fnpack build` 只要必需文件存在就打印 "Packing successfully"，
> **不会**检查归档内容是否等于当前源码树。所以从旧源码树打包、或漏掉权限修正，都会"成功"。
> `tools/fpk` 因此重新打开产物逐项断言：容器结构、manifest 与源码逐字节相同且版本一致、
> checksum 自洽、**遍历源码树自动发现的每个文件都与源码逐字节相同**、权限位、以及
> 1.3.1 / 1.4.0 / 1.4.1 各缺陷的回归探针。包内 `cloudflared` 二进制的哈希也会与固定值比对。

> 构建**不是**字节级可复现的：`.fpk` 内 `app.tgz` 的 md5 会被写入 manifest 的
> `checksum` 字段，而 tar 时间戳每次构建都会变化。

## 测试

```bash
go test ./...           # 全部单元测试（纯逻辑，任何平台可跑）
go vet ./...            # 静态检查
go run ./tools/lintui   # 前端选择器契约
go run ./tools/lintshell# 生命周期脚本静态契约
```

竞态检测需要 cgo（Windows 上没有 gcc），生命周期套件需要 Linux 语义
（`/proc`、`ps`、`curl`、unix socket）。在 Windows 上开发时用 WSL 跑这两项：

```bash
# 竞态检测
wsl -e bash -lc 'cd /mnt/d/.../fnos-cloudflared && \
  PATH=$HOME/go-toolchain/go/bin:$PATH CGO_ENABLED=1 go test -race ./...'

# 模拟安装（先复制到 WSL 原生盘：/mnt/d 是 drvfs，chmod 语义不完整）
wsl -e bash -lc 'mkdir -p ~/fncfd-test && cd ~/fncfd-test && \
  cp -a /mnt/d/.../fnos-cloudflared/{go.mod,cmd,internal,cloudflared,tools} . && \
  setsid bash tools/test-lifecycle.sh'
```

安装 `.fpk` 需要 root，因此项目自带一个**模拟安装**测试，在临时目录里复现应用中心
创建 `/var/apps/cloudflared/` 的布局，并用应用中心会导出的 `TRIM_*` 环境变量真实地
跑一遍全部生命周期脚本：

```bash
setsid ./tools/test-lifecycle.sh
```

覆盖内容：`install_init`/`install_callback` → `main start|status|restart|stop`
（含幂等性）→ 通过 `app.sock` 访问网关 API 与前缀注入 → `config_callback`
的读-改-写合并（验证隧道与 Token 不会被清掉、非法枚举值被拒绝）→
`uninstall_callback` 的保留/删除两种分支 → 载荷缺失时向 `$TRIM_TEMP_LOGFILE`
报错，以及**Token 不出现在子进程命令行、而是通过 `TUNNEL_TOKEN` 传递**这条安全回归。

用 `setsid` 运行是为了把测试放进独立会话，避免脚本里的进程组信号影响到外层 shell。

### 后端测试覆盖了什么

`go test ./...` 覆盖的都是**曾经真实让功能失效**的缺陷：

- 数据目录确实被创建；配置写入后能原样读回（`Store.Load` 曾把整份文档反序列化进
  `Settings`，导致每次重启后全局设置静默回到默认值）
- 配置双写者语义：外部 `config_callback` 写入的设置不会被隧道编辑回滚，本进程的写入优先
- 隧道 id 的路径安全：含 `..`、路径分隔符或非法字符的 id 一律换成新 id
- Token 三条规则（空串=不修改、`null`=清空、缺省=保留）与「响应里不出现完整 Token」
- 凭据解析绝不猜：只存了名称的隧道返回空，而不是挑一个 `<uuid>.json` 顶替
- `config.yml` 生成与 argv 顺序（`--output` 必须在 `list`/`create` 之后）
- 请求体上限返回 413、非 JSON 返回 400、畸形百分号编码返回 4xx 且进程存活
- 静态目录穿越与 `www-evil` 这类同前缀兄弟目录
- 未知隧道 id 的每条 `:id` 路由都返回 404，且不会在监督器里留下记录
- 日志尾部读取不返回窗口边界残行、不返回未初始化字节

### 前端测试的取舍

1.4.1 有一个基于 Node + 最小 DOM 桩的 UI 行为套件（42 项断言）。去掉 Node 之后，在没有
JS 引擎的 Go 测试里无法执行浏览器代码，因此该套件**没有等价迁移**：现在前端由
`tools/lintui` 做静态契约检查 —— `app.js`（现在是 `www/js/*.js`）查询的每个 `#id` 都能
解析到元素、每个用到的类名在 `style.css` 里有规则、`index.html` 的 id 不重复、模块
`import` 的目标都存在。这类错误**不会报错**（处理器绑到 `null`，按钮就是没反应），
所以值得在构建时拦住。

## 安装

应用需要 root 权限安装，请在**飞牛 OS 的「应用中心」中手动安装**：

1. 从本仓库的 [Releases](https://github.com/ybd156/fnos-cloudflared/releases)
   下载最新版 `cloudflared-<版本>.fpk`（`dist/` 只是本地构建目录，不在仓库中）
2. 打开「应用中心」→ 右上角「手动安装」→ 选择该 `.fpk`
3. 安装向导中确认是否「服务启动时自动运行已启用的隧道」
4. 安装过程**不会**再安装任何依赖应用

也可以在 NAS 的 root shell 中执行：

```bash
appcenter-cli install-fpk /path/to/cloudflared.fpk
```

安装完成后，飞牛 OS 桌面上会出现 **Cloudflare Tunnel** 图标。

## 使用

### 快速隧道（最快上手，无需账户）

1. 打开应用 → 「隧道」→「新建隧道」
2. 类型选「快速隧道」，填写本地服务地址，例如 `http://127.0.0.1:5000`
3. 保存后点「启动」，几秒后卡片上会出现 `https://xxx-xxx.trycloudflare.com` 地址

该地址每次重启都会变化，仅适合临时测试。

### Token 隧道（推荐用于生产）

1. 登录 [Cloudflare Zero Trust 控制台](https://one.dash.cloudflare.com/)
2. `Networks` → `Tunnels` → `Create a tunnel` → 选择 `Cloudflared`
3. 给隧道命名后，复制页面上给出的 **Token**（`eyJhIjoi...` 那一长串）
4. 在同一页面的 `Public Hostname` 标签中配置域名 → 服务地址（如 `http://localhost:5000`）
5. 回到本应用 → 「新建隧道」→ 类型选「Token 隧道」→ 粘贴 Token → 保存 → 启动

配置完全由 Cloudflare 云端下发，本机无需再维护 ingress 规则。

### 本地管理隧道（在应用内管理账户与路由）

1. 「账户」标签页 → 点「登录 Cloudflare」
2. 页面会给出一个授权链接，在浏览器中打开并授权您的域名
3. 授权完成后回到应用，即可：
   - 查看账户下已有的隧道
   - 创建 / 删除隧道
   - 为隧道配置 ingress 规则（主机名 → 本地服务）
   - 一键为规则添加 DNS 路由记录

凭据保存在 `$PKGVAR/.cloudflared/cert.pem`，权限为 `0700` 目录内的私有文件。

### 自定义 config.yml

在「新建隧道」中选择「自定义配置」，填入您自己编写的 `config.yml` 内容，
或指定已有的 `config.yml` 路径，应用会以 `--config` 方式启动。

## 数据与安全

| 路径 | 内容 | 权限 |
| --- | --- | --- |
| `$PKGVAR/config.json` | 隧道定义、设置、Token | `0600` |
| `$PKGVAR/.cloudflared/cert.pem` | Cloudflare 账户凭据 | 目录 `0700` |
| `$PKGVAR/tunnels/<id>/config.yml` | 每个本地管理隧道的配置 | `0750` |
| `$PKGVAR/logs/<id>.log` | 每个隧道的 cloudflared 日志 | `0640` |
| `$PKGVAR/server.log` | 应用自身日志（自动轮转） | `0640` |
| `$PKGVAR/main.log` | 生命周期脚本日志 | — |

其中 `$PKGVAR` 即 `/vol1/@appdata/cloudflared`。

安全设计：

- 应用以 `run-as: package` 模式运行，使用专属系统用户 `cloudflared`，不使用 root
- 管理界面**只通过 fnOS 统一网关**（`app.sock`）暴露，访问前由飞牛 OS 校验登录态，
  不对局域网开放任何端口
- 隧道 Token 在 API 中只返回 `hasToken` 与掩码预览（如 `eyJhIjoi…Zm9v`），
  明文永不离开服务端
- 启动隧道时 Token 通过 `TUNNEL_TOKEN` **环境变量**交给 `cloudflared`，而不是
  命令行参数——Linux 上任何本地用户都能读取 `/proc/<pid>/cmdline`，用参数传递
  等于把凭据泄露给同机的其他用户
- 日志文件（`server.log`、`logs/<id>.log`）以 `0640` 创建
- 网关会注入 `X-Trim-Userid` / `X-Trim-Username` / `X-Trim-Isadmin` 请求头

## 常见问题

**Q：升级到 1.4.2 后还需要 nodejs_v24 吗？**
A：不需要。服务已经是静态二进制，manifest 不再声明任何依赖应用，可以把它卸载。
旧版本会去 `/var/apps/nodejs_v2*/target/bin/node` 探测解释器，现在这一步已经删除。

**Q：启动失败，提示「应用文件缺失或不可执行」？**
A：说明载荷里的 `bin/fncfd` 或 `bin/cloudflared` 没到位或没有可执行位。重新安装；
若自行打包，确认走了 `./build.sh`（它会交叉编译二进制并归一化归档权限位）。

**Q：隧道一直处于「启动中」？**
A：打开该隧道的日志查看 cloudflared 输出。若 UDP 7844 被网络阻断，请在
「设置」中把传输协议改为 `http2`。

**Q：如何更新 cloudflared？**
A：「设置」→「更新 cloudflared 二进制」，会从 GitHub 拉取最新版并原子替换。

**Q：本地管理隧道启动时报「无法确定隧道的 UUID」？**
A：说明这条隧道只保存了名称、没有隧道 ID。请到「账户」页确认已登录，然后点该隧道
的「编辑」重新从下拉框选择一次并保存 —— 保存时会把 UUID 一并记下，之后
`cloudflared` 就能用 `<UUID>.json` 精确定位凭据文件。应用**不会**再去猜凭据文件：
账户下有多个隧道时，「随便挑一个」会让隧道看起来正常运行、实际却指向了另一个源站。

**Q：状态码含义？**
A：`cmd/main status` 返回 `0` = 运行中，`3` = 未运行，`1` = 失败。

## 卸载

在应用中心卸载即可。卸载向导会询问是否**同时删除隧道配置、Token 与账户凭据**；
默认保留，便于重新安装后继续使用。Cloudflare 控制台中已创建的隧道不会被删除。

## 技术说明

- 后端：Go，**只用标准库**（`net/http` / `os` / `os/exec` / `encoding/json` /
  `archive/tar` 等），没有任何第三方模块，因此安装与构建都不需要联网拉包
- 服务是**单个静态二进制**（`CGO_ENABLED=0`，linux/amd64，strip 后约 7.8MB）
- 前端：原生 JavaScript，拆成 6 个 ES 模块（`www/js/`），无框架、无构建步骤
- 网关前缀由后端在返回 HTML 时把 `%%CF_BASE%%` 占位符替换为实际前缀，
  因此同一套代码在「网关访问」与「本地直连」两种入口下都能正确解析资源路径
- 生命周期脚本参考 `fn-deepseek-harness` 的健壮性写法：用 `/proc/<pid>/stat`
  识别僵尸进程、用 `/proc/<pid>/cmdline` 校验进程身份以避免误杀 PID 复用进程
- 构建工具同样是 Go，`build.sh` 与 `build.ps1` 只做薄封装

更细的设计与缺陷史见 [`docs/DESIGN.md`](docs/DESIGN.md)，
包边界与接口契约见 [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md)。
