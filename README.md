# submux

submux 是一个 **Mihomo / sing-box 配置编排服务**。它从机场订阅和手工输入中建立统一节点库，再把用户选择的节点、配置模板和规则方案编译成固定引擎、固定策略、可分享的输出订阅。

submux 控制面本身不运行代理内核，也不沿用机场的策略组、规则或 DNS 配置。机场只提供节点；入口和运行方式由模板决定，分流规则由规则方案决定。需要管理本机或服务器 Mihomo 时，可另行安装可选的 `submux-agent`。

## 产品工作流

```text
机场订阅 ─刷新─┐
              ├─> 规范化节点库 ─选择节点─┐
手工分享链接 ─┘                           ├─> 输出订阅 ─> /sub/{token}
完整配置模板 ─> 不可变模板版本 ───────────┘
规则方案 ─────> MetaCubeX 规则目录 ───────┘
```

1. 添加机场订阅来源；手工分享链接可直接导入，系统自动归入内置“自建节点”分组。
2. 在统一节点库中查看和整理节点，维护分类、标签和启用状态。
3. 选择平台预置或自行维护的 Mihomo / sing-box 模板版本。
4. 在规则方案中设置直连、主代理、流媒体代理和拦截规则。
5. 创建输出订阅，选择模板、规则方案和节点，保存后获得独立链接。

v4 直接采用输出订阅保存有序节点选择的模型，不保留旧 NodeSet 或节点配置 Profile。规则方案只负责 Mihomo 分流，不保存节点。

## 主要能力

- 机场订阅定时刷新，支持 Mihomo YAML、明文分享链接和 Base64 分享链接订阅。
- 平台资源代理与 Agent 资源代理互相独立；机场来源可在直连发生网络错误后尝试平台资源代理，并记录两次请求的结果。
- 识别 `Subscription-Userinfo` 与伪装成节点的剩余流量/到期信息，提供到期预警、状态事件和自动恢复。
- 手工导入 VLESS、VMess、Trojan、Shadowsocks、Hysteria2 节点，无需预先创建来源。
- 节点语义指纹去重；机场改名或唯一同名节点更新 IP、端口及其他连接参数时仍保留标签、启用状态、节点 ID 和订阅选择。
- 输出订阅直接保存每个模板插槽的有序节点选择；控制台支持搜索、来源/协议过滤、批量选择、拖放和排序。
- 内置 MetaCubeX `meta-rules-dat` 的完整 geosite/geoip 目录快照；规则方案按需选择分类，只有启用的 `.mrs` provider 才会写入 Mihomo 配置。
- MetaCubeX 目录可以从 GitHub 手工刷新；已有规则方案固定原提交，只有用户确认更新后才切换版本。
- 规则方案可以被多个 Mihomo 输出订阅共用，支持有序规则、指定域名/IP 规则、直连、主代理、独立流媒体代理和拦截。
- Mihomo YAML 与 sing-box JSON 双编译器；无法无损转换时整体失败，不静默丢字段或节点。
- 模板版本发布后不可变；输出订阅固定某个版本，不会随模板更新发生隐式变化。
- 每个输出订阅拥有独立 token、启用状态、可选到期时间和预编译产物。
- 一般编译失败保留该订阅的 last-good 产物，并通过 `X-Submux-Degraded` 暴露错误；strict 生命周期阻断时旧产物只供审计，公开链接返回 503。
- 机场到期默认 continuity 保持连续性；可为单个来源启用 strict，排除过期节点并在无替代节点时阻断输出订阅。
- 内置两套可版本化 Mihomo 模板：IPv4-only `Mihomo 桌面 TUN`，以及仅监听回环、供 rootless Agent 使用的 `Mihomo Linux 服务器`。
- Go 单二进制、内嵌控制台、bbolt 单文件存储，无 CGO 依赖。

协议边界和依据见 [docs/PROTOCOLS.md](docs/PROTOCOLS.md)，机场状态见 [docs/LIFECYCLE.md](docs/LIFECYCLE.md)，节点身份、领域模型与发布语义见 [docs/DESIGN.md](docs/DESIGN.md)。可选 Mihomo 运行面的实现、权限边界和验收基线见 [docs/AGENT.md](docs/AGENT.md)。

## 构建与运行

```sh
CGO_ENABLED=0 go build -o submux ./cmd/submux
SUBMUX_DB=submux.db ./submux
```

默认监听 `127.0.0.1:8080`。首次打开 <http://127.0.0.1:8080> 设置管理员密码，然后按“来源 → 节点库 → 模板 → 规则 → 输出订阅”流程创建链接。

当前正式版本为 [`v1.0.1`](https://github.com/Questrove/submux/releases/tag/v1.0.1)。安装器固定准确版本并验证 Release 的 SHA-256 清单：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --version v1.0.1
```

安装 systemd 服务：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --version v1.0.1 --service
```

## 可选 Mihomo Agent

`submux-agent` 与控制面分别发布和安装。Agent 以当前用户权限运行，只管理自己用户目录中的 Mihomo 二进制、配置和子进程；它不会修改 Docker、Git、包管理器或系统代理配置。Agent 只建立到控制面的 HTTPS/WSS 出站连接，不开放远程管理端口；Mihomo 控制 API 和本地管理 IPC 只绑定回环或当前用户可访问的 Socket/命名管道。

Linux amd64/arm64：

```sh
AGENT_VERSION='v1.0.1'
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install-agent.sh | bash -s -- --version "$AGENT_VERSION" --service
~/.local/bin/submux-agent enroll --server https://submux.example.com --code '<控制台生成的一次性配对码>'
```

只有 root 登录入口的 Linux 服务器可直接复制控制台生成的“一键接入”命令：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/bootstrap-agent.sh | sudo bash -s -- \
  --version "$AGENT_VERSION" --server https://submux.example.com --code '<一次性配对码>'
```

引导脚本的 root 权限只用于创建无 sudo 权限的 `submuxagent` 专用用户并启用 systemd user lingering；安装、配对、Agent 和 Mihomo 进程均以该普通用户运行。源码开发版还可使用 root 所有的本地二进制包，避免从 Release 下载不存在的版本。

Windows amd64/arm64（普通 PowerShell）：

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/Questrove/submux/main/scripts/install-agent.ps1 -OutFile .\install-agent.ps1
$AgentVersion = 'v1.0.1'
.\install-agent.ps1 -Version $AgentVersion -Service
& "$env:LOCALAPPDATA\Programs\Submux\submux-agent.exe" enroll --server https://submux.example.com --code '<控制台生成的一次性配对码>'
```

`v0.2.0` 的 Agent 使用旧的 root/Administrator 模型，不能直接原地升级到 `v1.0.1`。升级前请按 [Agent 迁移步骤](docs/AGENT.md#从-v020-迁移) 停止并卸载旧服务，再以普通用户安装和配对新 Agent。

常用本机恢复入口：

```text
submux-agent status | doctor | logs
submux-agent service start|stop|status
submux-agent mihomo status|restart|rollback
submux-agent subscription status|rollback
submux-agent unenroll [--force-local] [--yes]
```

这些本机管理命令不需要 Linux root 或 Windows 管理员权限。控制台只显示 Agent 上报的实际状态，安装、启停、回滚、管理配置订阅和修改 Agent 资源代理都作为一次性任务执行，不会在 Agent 重新连接后按旧表单内容持续对账。显式启动或重启 Mihomo 成功后，Agent 会在本地记录下次启动时恢复运行；显式停止或卸载会清除该记录。恢复失败时按 2、4、8、16 秒退避，最多尝试五次。每个 Agent 可以保存并切换多个 Mihomo 配置订阅：既可以直接选择平台已经发布的 Mihomo 订阅，也可以填写独立的 HTTPS 地址。外部订阅地址只保存在 Agent 本机；平台订阅通过设备认证接口读取。Linux 的 `--service` 安装 systemd user unit；无人值守服务器若需要用户退出后继续运行，应由主机管理员明确决定是否为该用户启用 lingering。

Agent 接入后，可以在运行实例的“配置”页设置独立的 HTTP 或 SOCKS5 Agent 资源代理，例如 SSH 转发到本机的 `socks5://127.0.0.1:1080`。该地址只用于读取 Mihomo 官方版本并下载核心，不会代理 Agent 与控制面的连接，也不会修改系统或其他软件的代理设置。页面会实时显示 Agent 接收、下载、校验、部署、启动和验证进度；日志页可在 Agent 日志和 Mihomo 日志之间切换。

终端代理只作用于新 Shell：Linux 使用 `eval "$(submux-agent proxy env bash)"`，PowerShell 使用 `submux-agent proxy env powershell | Invoke-Expression`。Web 控制台的“代理设置指南”会按终端、Git、APT/DNF、npm/pnpm/Yarn、pip、Docker Engine/Desktop、systemd 服务和 Windows 系统代理生成步骤或可复制命令；只有用户亲自执行后才会修改对应软件。

## 配置

| 项 | 默认值 | 说明 |
|---|---:|---|
| `SUBMUX_DB` | `submux.db` | bbolt 数据文件路径 |
| `listen_addr` | `127.0.0.1:8080` | 监听地址（数据库设置，重启生效） |
| `base_url` | 空 | 控制台生成输出订阅外部链接时使用 |
| `fetch_interval_sec` | `10800` | 机场刷新间隔，范围 60–604800 秒 |
| 平台资源代理 | 直连 | 在设置页配置，只供规则目录刷新和已明确启用回退的机场来源使用 |

## 反向代理

输出订阅 token 相当于访问凭据；对外提供订阅必须使用 HTTPS。

```nginx
map $http_upgrade $connection_upgrade {
    default upgrade;
    ''      close;
}

server {
    listen 443 ssl;
    server_name sub.example.com;
    ssl_certificate     /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;
    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $connection_upgrade;
        proxy_read_timeout 1800s;
    }
}
```

然后把控制台 `base_url` 设置为 `https://sub.example.com`。Agent 更新提示和运行数据使用 WebSocket，反向代理必须保留上面的 Upgrade 头；配对、设备认证和任务状态仍走 HTTPS。

## 安全边界

- 管理密码使用 bcrypt，登录会话使用 HMAC 签名的 `HttpOnly` / `SameSite` Cookie。
- 管理 API 需要会话；公开端点只能通过 192-bit 随机订阅 token 读取已发布产物。
- 上游只接受 HTTP(S)，响应上限 10 MiB；原始订阅不会入库。
- 模板发布、规则方案、输出订阅保存和节点转换均采用严格校验；失败不会覆盖 last-good。
- sing-box 转换仅接受文档中明确支持且可保持语义的字段。
- Agent 协议只允许固定类型化操作；设备私钥和 Mihomo secret 只保存在 Agent 本机，核心二进制只从内置的 MetaCubeX/mihomo 官方 Release 坐标下载并校验 GitHub 提供的 SHA-256 摘要。
