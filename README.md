# submux

submux 是一个 **Mihomo / sing-box 配置编排服务**。它从机场订阅和手工输入中建立统一节点库，再把用户直接选择的节点注入平台维护的完整配置模板，生成固定引擎、固定策略、可分享的输出订阅。

submux 控制面本身不运行代理内核，也不沿用机场的策略组、规则或 DNS 配置。机场只提供节点；最终行为完全由你在平台上的模板决定。需要管理本机或服务器 Mihomo 时，可另行安装可选的 `submux-agent`。

## 产品工作流

```text
机场订阅 ─刷新─┐
              ├─> 规范化节点库 ─选择节点─┐
手工分享链接 ─┘                           ├─> 输出订阅 ─> /sub/{token}
完整配置模板 ─> 不可变模板版本 ───────────┘
```

1. 添加机场订阅来源；手工分享链接可直接导入，系统自动归入内置“自建节点”分组。
2. 在统一节点库中查看和整理节点，维护分类、标签和启用状态。
3. 选择平台预置或自行维护的 Mihomo / sing-box 模板版本。
4. 创建输出订阅，把左侧节点拖到模板插槽的右侧已选列表，保存后获得独立链接。

v4 直接采用最终产品模型，不保留 NodeSet/Profile 中间层。升级时旧输出配置会被清理，但来源、节点、模板和生命周期数据保留。

## 主要能力

- 机场订阅定时刷新，支持 Mihomo YAML、明文分享链接和 Base64 分享链接订阅。
- 识别 `Subscription-Userinfo` 与伪装成节点的剩余流量/到期信息，提供到期预警、状态事件和自动恢复。
- 手工导入 VLESS、VMess、Trojan、Shadowsocks、Hysteria2 节点，无需预先创建来源。
- 节点语义指纹去重；机场改名或唯一同名节点更新 IP、端口及其他连接参数时仍保留标签、启用状态、节点 ID 和订阅选择。
- 输出订阅直接保存每个模板插槽的有序节点选择；控制台支持搜索、来源/协议过滤、批量选择、拖放和排序。
- Mihomo YAML 与 sing-box JSON 双编译器；无法无损转换时整体失败，不静默丢字段或节点。
- 模板版本发布后不可变；输出订阅固定某个版本，不会随模板更新发生隐式变化。
- 每个输出订阅拥有独立 token、启用状态、可选到期时间和预编译产物。
- 一般编译失败保留该订阅的 last-good 产物，并通过 `X-Submux-Degraded` 暴露错误；strict 生命周期阻断时旧产物只供审计，公开链接返回 503。
- 机场到期默认 continuity 保持连续性；可为单个来源启用 strict，排除过期节点并在无替代节点时阻断输出订阅。
- 内置六套可版本化模板：推荐的 IPv4-only Mihomo 桌面 TUN、轻量 Mihomo 桌面系统代理、Mihomo Linux 网关、Mihomo 服务器 Sidecar，以及 sing-box 桌面/服务器。
- Go 单二进制、内嵌控制台、bbolt 单文件存储，无 CGO 依赖。

协议边界和依据见 [docs/PROTOCOLS.md](docs/PROTOCOLS.md)，机场状态见 [docs/LIFECYCLE.md](docs/LIFECYCLE.md)，节点身份、领域模型与发布语义见 [docs/DESIGN.md](docs/DESIGN.md)。可选 Mihomo 运行面的实现、权限边界和验收基线见 [docs/AGENT.md](docs/AGENT.md)。

## 构建与运行

```sh
CGO_ENABLED=0 go build -o submux ./cmd/submux
SUBMUX_DB=submux.db ./submux
```

默认监听 `127.0.0.1:8080`。首次打开 <http://127.0.0.1:8080> 设置管理员密码，然后按“来源 → 节点库 → 模板 → 输出订阅”流程创建链接。

当前正式版本为 [`v0.2.0`](https://github.com/Questrove/submux/releases/tag/v0.2.0)。安装器固定准确版本并验证 Release 的 SHA-256 清单：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --version v0.2.0
```

安装 systemd 服务：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --version v0.2.0 --service
```

## 可选 Mihomo Agent

`submux-agent` 与控制面分别发布和安装。Agent 只建立到控制面的 HTTPS/WSS 出站连接，不开放远程管理端口；Mihomo 控制 API 和本地管理 IPC 只绑定回环或本机 Socket/命名管道。

Linux amd64/arm64：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install-agent.sh | bash -s -- --version v0.2.0 --service
sudo submux-agent enroll --server https://submux.example.com --code '<控制台生成的一次性配对码>'
```

Windows amd64/arm64（管理员 PowerShell）：

```powershell
Invoke-WebRequest https://raw.githubusercontent.com/Questrove/submux/main/scripts/install-agent.ps1 -OutFile .\install-agent.ps1
.\install-agent.ps1 -Version v0.2.0 -Service
& "$env:ProgramFiles\Submux\submux-agent.exe" enroll --server https://submux.example.com --code '<控制台生成的一次性配对码>'
```

常用本机恢复入口：

```text
submux-agent status | doctor | logs
submux-agent service start|stop|status
submux-agent mihomo status|restart|rollback
submux-agent subscription status|check|update|rollback
submux-agent unenroll [--force-local] [--yes]
```

这些本机管理命令需要 Linux root 或 Windows 管理员权限。本机回滚属于控制面不可用时的应急恢复；Agent 重新连上控制面后仍会按当前 desired generation 对账，因此需要长期固定旧版本时也应同步修改控制台期望状态。

终端代理只作用于新 Shell：Linux 使用 `eval "$(submux-agent proxy env bash)"`，PowerShell 使用 `submux-agent proxy env powershell | Invoke-Expression`。Linux Docker Engine 与 Windows Docker Desktop 集成必须先预览，再在 Web 控制台确认当前文件 hash；发生外部修改时进入 `conflict`，不会以旧备份覆盖。

## 配置

| 项 | 默认值 | 说明 |
|---|---:|---|
| `SUBMUX_DB` | `submux.db` | bbolt 数据文件路径 |
| `listen_addr` | `127.0.0.1:8080` | 监听地址（数据库设置，重启生效） |
| `base_url` | 空 | 控制台生成输出订阅外部链接时使用 |
| `fetch_interval_sec` | `10800` | 机场刷新间隔，范围 60–604800 秒 |

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

然后把控制台 `base_url` 设置为 `https://sub.example.com`。Agent 更新提示和运行数据使用 WebSocket，反向代理必须保留上面的 Upgrade 头；配对、设备认证和 artifact 下载仍走 HTTPS。

## 安全边界

- 管理密码使用 bcrypt，登录会话使用 HMAC 签名的 `HttpOnly` / `SameSite` Cookie。
- 管理 API 需要会话；公开端点只能通过 192-bit 随机订阅 token 读取已发布产物。
- 上游只接受 HTTP(S)，响应上限 10 MiB；原始订阅不会入库。
- 模板发布、输出订阅保存和节点转换均采用严格校验；失败不会覆盖 last-good。
- sing-box 转换仅接受文档中明确支持且可保持语义的字段。
- Agent 协议只允许固定类型化操作；设备私钥和 Mihomo secret 只保存在 Agent 本机，核心二进制只从内置的 MetaCubeX/mihomo 官方 Release 坐标下载并校验 GitHub 提供的 SHA-256 摘要。
