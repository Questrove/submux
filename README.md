# submux

submux 是一个 **Mihomo / sing-box 配置编排服务**。它从机场来源和手工输入中建立统一节点库，再把用户选择的节点、配置模板和规则方案编译成固定引擎、固定策略、可分享的输出订阅。

submux 控制面本身不运行代理内核，也不沿用机场的策略组、规则或 DNS 配置。机场只提供节点；入口和运行方式由模板决定，分流规则由规则方案决定。需要管理本机或服务器 Mihomo 时，可以另行安装完全独立、只接受本机 IPC 管理的 Submux Runtime。

## 产品工作流

```text
机场来源 ─刷新─┐
              ├─> 规范化节点库 ─选择节点─┐
手工分享链接 ─┘                           ├─> 输出订阅 ─> /sub/{token}
完整配置模板 ─> 不可变模板版本 ───────────┘
规则方案 ─────> MetaCubeX 规则目录 ───────┘
```

1. 添加机场来源；手工分享链接可直接导入，系统自动归入内置“自建节点”分组。
2. 在统一节点库中查看和整理节点，维护分类、标签和启用状态。
3. 选择平台预置或自行维护的 Mihomo / sing-box 模板版本。
4. 在规则方案中设置直连、主代理、流媒体代理和拦截规则。
5. 创建输出订阅，选择模板、规则方案和节点，保存后获得独立链接。

v4 直接采用输出订阅保存有序节点选择的模型，不保留旧 NodeSet 或节点配置 Profile。规则方案只负责 Mihomo 分流，不保存节点。

## 主要能力

- 机场来源定时刷新，来源内容可以是 Mihomo YAML、明文分享链接或 Base64 分享链接列表。
- 平台资源代理只属于 submux 控制面；机场来源可在直连发生网络错误后尝试平台资源代理，并记录两次请求的结果。Submux Runtime 独立保存自己的下载线路。
- 识别 `Subscription-Userinfo` 与伪装成节点的剩余流量/到期信息，提供到期预警、状态事件和自动恢复。
- 手工导入 VLESS、VMess、Trojan、Shadowsocks、Hysteria2 节点，无需预先创建来源。
- 节点语义指纹去重；机场改名或唯一同名节点更新 IP、端口及其他连接参数时仍保留标签、启用状态、节点 ID 和输出订阅选择。
- 输出订阅直接保存每个模板插槽的有序节点选择；控制台支持搜索、来源/协议过滤、批量选择、拖放和排序。
- 内置 MetaCubeX `meta-rules-dat` 的完整 geosite/geoip 目录快照；规则方案按需选择分类，只有启用的 `.mrs` provider 才会写入 Mihomo 配置。
- MetaCubeX 目录可以从 GitHub 手工刷新；已有规则方案固定原提交，只有用户确认更新后才切换版本。
- 规则方案可以被多个 Mihomo 输出订阅共用，支持有序规则、指定域名/IP 规则、直连、主代理、独立流媒体代理和拦截。
- Mihomo YAML 与 sing-box JSON 双编译器；无法无损转换时整体失败，不静默丢字段或节点。
- 模板版本发布后不可变；输出订阅固定某个版本，不会随模板更新发生隐式变化。
- 每个输出订阅拥有独立 token、启用状态、可选到期时间和预编译产物。
- 一般编译失败保留该输出订阅的最近可用产物，并通过 `X-Submux-Degraded` 暴露错误；strict 生命周期阻断时旧产物只供审计，公开链接返回 503。
- 机场到期默认 continuity 保持连续性；可为单个来源启用 strict，排除过期节点并在无替代节点时阻断输出订阅。
- 内置两套可版本化 Mihomo 模板：IPv4-only `Mihomo 桌面 TUN` 与仅监听回环的 `Mihomo Linux 服务器`。这里的 IPv4-only 是直接使用模板时的默认值；Submux Runtime 会覆盖这些运行字段，并按本机设置默认接管 IPv4 与 IPv6。
- 设置页统一维护共享 `fake-ip-filter`；fake-ip 模板在模板专用条目前稳定合并并去重，redir-host 模板不应用。
- Go 单二进制、内嵌控制台、bbolt 单文件存储，无 CGO 依赖。

协议边界和依据见 [docs/PROTOCOLS.md](docs/PROTOCOLS.md)，机场状态见 [docs/LIFECYCLE.md](docs/LIFECYCLE.md)，节点身份、领域模型与发布语义见 [docs/DESIGN.md](docs/DESIGN.md)。Submux Runtime 的目标架构、IPC、网络权限与发行方式分别见 [docs/RUNTIME.md](docs/RUNTIME.md)、[docs/RUNTIME-IPC.md](docs/RUNTIME-IPC.md)、[docs/RUNTIME-NETWORK.md](docs/RUNTIME-NETWORK.md) 和 [docs/RUNTIME-DISTRIBUTION.md](docs/RUNTIME-DISTRIBUTION.md)。

## 构建与运行

```sh
CGO_ENABLED=0 go build -o submux ./cmd/submux
SUBMUX_DB=submux.db ./submux
```

默认监听 `127.0.0.1:8080`。首次打开 <http://127.0.0.1:8080> 设置管理员密码，然后按“来源 → 节点库 → 模板 → 规则 → 输出订阅”流程创建链接。

当前正式版本为 [`v1.0.2`](https://github.com/Questrove/submux/releases/tag/v1.0.2)。安装器固定准确版本并验证 Release 的 SHA-256 清单：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --version v1.0.2
```

安装 systemd 服务：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --version v1.0.2 --service
```

## Submux Runtime

Submux Runtime 是另行安装的机器级 Mihomo 管理程序。它可以读取 submux 输出订阅，但不向 submux 注册，不建立设备身份，也没有配对、心跳、WSS 或由 submux 发起的运行操作。GUI、TUI 和 CLI 只通过 Unix Socket 或 Windows Named Pipe 管理本机 Runtime。

目标产品由低权限的 `submux-runtime`、按需执行固定网络操作的 `submux-runtime-net` 和可选的 Tauri GUI 组成。一台机器或一个网络命名空间只运行一个 Runtime，并且只管理一个 Mihomo。运行方式可以选择显式代理、TUN 或 Linux 网关；TUN 和网关异常时恢复直连。

Runtime 可以保存 submux 输出订阅、外部 HTTP(S) 完整配置和本机导入副本。来源原文之上可以应用本机高级覆盖，最后由 Runtime 强制写入监听、控制端点、TUN、路由、DNS、网关和数据路径等保留设置。来源不能扩大本机权限或引用任意文件。

Runtime 正在实现。仓库目前已有 `submux-runtime` 服务、本机 IPC、单实例锁、单写者状态库、持久化运行操作、候选配置预览、双栈回环显式代理，以及共用本机 IPC 的 Bubble Tea TUI 和 Tauri GUI；远程来源、TUN/网关模式和安装器尚未完成，因此还不能用于正式部署。新的 Runtime 采用清理后重新安装，不读取或迁移任何已移除的远程运行端状态。

开发环境中，在 Runtime 状态目录已经放置受信任 Mihomo 核心并启动服务后，可以先上传配置副本、预览并应用候选配置，再显式启动：

```bash
go run ./cmd/submux-runtime import --json ./config.yaml
go run ./cmd/submux-runtime proxy preview --content-id <content_id> --json
go run ./cmd/submux-runtime proxy apply --content-id <content_id> --wait --json
go run ./cmd/submux-runtime proxy start --wait --json
go run ./cmd/submux-runtime proxy verify --json
go run ./cmd/submux-runtime proxy stop --wait --json
```

CLI 打开文件并上传字节，Runtime 不接收客户端文件路径。首次应用只保存经过静态校验的配置，不会启动 Mihomo；`proxy start` 是单独的运行操作。运行操作通过 `operation get`、`operation wait` 和 `operation cancel` 查询、等待或取消。交互终端可以运行 `submux-runtime tui`；Tauri 2 GUI 源码位于 `desktop/submux-runtime-gui`，WebView 只调用 Rust 本机 IPC 桥接。

完整设计见：

- [Runtime 总体设计](docs/RUNTIME.md)
- [本机 IPC](docs/RUNTIME-IPC.md)
- [TUN、Linux 网关与特权边界](docs/RUNTIME-NETWORK.md)
- [安装、手动更新、离线包与发行](docs/RUNTIME-DISTRIBUTION.md)

## 配置

| 项 | 默认值 | 说明 |
|---|---:|---|
| `SUBMUX_DB` | `submux.db` | bbolt 数据文件路径 |
| `listen_addr` | `127.0.0.1:8080` | 监听地址（数据库设置，重启生效） |
| `base_url` | 空 | 控制台生成输出订阅外部链接时使用 |
| `fetch_interval_sec` | `10800` | 机场刷新间隔，范围 60–604800 秒 |
| 平台资源代理 | 直连 | 在设置页配置，只供规则目录刷新和已明确启用回退的机场来源使用 |
| 共享 `fake-ip-filter` | blacklist、空列表 | 在设置页统一配置，保存后重建已启用的 Mihomo 输出订阅 |

## 反向代理

输出订阅 token 相当于访问凭据；对外提供输出订阅必须使用 HTTPS。

```nginx
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
    }
}
```

然后把控制台 `base_url` 设置为 `https://sub.example.com`。Submux Runtime 不通过这个反向代理接受管理，也不需要为 Runtime 保留 WebSocket、设备认证或任务端点。

## 安全边界

- 管理密码使用 bcrypt，登录会话使用 HMAC 签名的 `HttpOnly` / `SameSite` Cookie。
- 管理 API 需要会话；公开端点只能通过 192-bit 随机输出订阅 token 读取已发布产物。
- 上游只接受 HTTP(S)，响应上限 10 MiB；来源原文不会入库。
- 模板发布、规则方案、输出订阅保存和节点转换均采用严格校验；失败不会覆盖最近可用产物。
- sing-box 转换仅接受文档中明确支持且可保持语义的字段。
- Submux Runtime 与控制面没有设备协议。Runtime 的管理接口只存在于本机 Socket 或 Named Pipe；Mihomo secret、配置来源凭据和系统网络权限都不进入 submux 控制面。
