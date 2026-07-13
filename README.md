# submux

submux 是一个 **Mihomo / sing-box 配置编排服务**。它从机场订阅和手工输入中建立统一节点库，再把 NodeSet 注入平台维护的完整配置模板，生成固定引擎、固定策略、可分享的 Profile 订阅链接。

submux 不运行代理内核，也不沿用机场的策略组、规则或 DNS 配置。机场只提供节点；最终行为完全由你在平台上的模板决定。

## v2 工作流

```text
机场订阅 ─刷新─┐
              ├─> 规范化节点库 ─> NodeSet ─┐
手工分享链接 ─┘                            ├─> Profile ─> /sub/{profile-token}
完整配置模板 ─> 不可变模板版本 ────────────┘
```

1. 添加机场订阅来源，或创建手工来源并导入节点。
2. 用来源、指定节点、协议、名称与标签组成可复用 NodeSet。
3. 选择平台预置或自行维护的 Mihomo / sing-box 模板版本。
4. 把模板插槽绑定到 NodeSet，创建 Profile。
5. 把该 Profile 的独立订阅链接交给对应内核。

这是破坏式 v2：旧的全局订阅 token、Merge Override、按 User-Agent 猜输出格式和 Base64 输出均已删除，旧 `/sub/{token}` 不兼容。

## 主要能力

- 机场订阅定时刷新，支持 Mihomo YAML、明文分享链接和 Base64 分享链接订阅。
- 手工导入 VLESS、VMess、Trojan、Shadowsocks、Hysteria2 节点。
- 节点语义指纹去重；机场改名后仍保留本地 Alias、标签、启用状态和节点 ID。
- NodeSet 动态选择来源与节点，并支持协议、名称、标签过滤和显式排除。
- Mihomo YAML 与 sing-box JSON 双编译器；无法无损转换时整体失败，不静默丢字段或节点。
- 模板版本发布后不可变；Profile 固定某个版本，不会随模板更新发生隐式变化。
- 每个 Profile 独立 token、启用状态、可选到期时间和预编译产物。
- 编译失败保留该 Profile 的 last-good 产物，并通过 `X-Submux-Degraded` 暴露错误。
- 首次启动预置 Mihomo 桌面/网关、sing-box 桌面/服务器四套模板。
- Go 单二进制、内嵌控制台、bbolt 单文件存储，无 CGO 依赖。

协议边界和依据见 [docs/PROTOCOLS.md](docs/PROTOCOLS.md)，领域模型与发布语义见 [docs/DESIGN.md](docs/DESIGN.md)。

## 构建与运行

```sh
CGO_ENABLED=0 go build -o submux ./cmd/submux
SUBMUX_DB=submux.db ./submux
```

默认监听 `127.0.0.1:8080`。首次打开 <http://127.0.0.1:8080> 设置管理员密码，然后按控制台的五步工作流创建 Profile。

Linux / macOS 安装：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash
```

安装 systemd 服务：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh | bash -s -- --service
```

## 配置

| 项 | 默认值 | 说明 |
|---|---:|---|
| `SUBMUX_DB` | `submux.db` | bbolt 数据文件路径 |
| `listen_addr` | `127.0.0.1:8080` | 监听地址（数据库设置，重启生效） |
| `base_url` | 空 | 控制台生成 Profile 外部链接时使用 |
| `fetch_interval_sec` | `10800` | 机场刷新间隔，范围 60–604800 秒 |

## 反向代理

Profile token 相当于访问凭据；对外提供订阅必须使用 HTTPS。

```nginx
server {
    listen 443 ssl;
    server_name sub.example.com;
    ssl_certificate     /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;
    location / { proxy_pass http://127.0.0.1:8080; }
}
```

然后把控制台 `base_url` 设置为 `https://sub.example.com`。

## 安全边界

- 管理密码使用 bcrypt，登录会话使用 HMAC 签名的 `HttpOnly` / `SameSite` Cookie。
- 管理 API 需要会话；公开端点只能通过 192-bit 随机 Profile token 读取已发布产物。
- 上游只接受 HTTP(S)，响应上限 10 MiB；原始订阅不会入库。
- 模板发布、Profile 保存和节点转换均采用严格校验；失败不会覆盖 last-good。
- sing-box 转换仅接受文档中明确支持且可保持语义的字段。
