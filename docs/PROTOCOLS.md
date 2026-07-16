# 协议与配置兼容边界

## 原则

1. 连接语义以协议项目和目标内核官方文档为准，不根据某个机场返回样本反推字段。
2. Mihomo 与 sing-box 是两个独立目标，不假设同名字段语义相同。
3. Mihomo 模板直接接收规范化的 Mihomo 节点对象；sing-box 必须经过显式字段转换。
4. sing-box 转换遇到未知协议、未知字段、未知传输或语义不等价字段时，整个输出订阅编译失败并保留 last-good。
5. 协议支持表示“当前列出的连接字段可以保持语义”，不表示任意客户端私有扩展都受支持。

## 输入

机场或手工输入可使用：

- 完整 Mihomo/Clash YAML 的 `proxies` 数组；
- 每行一个分享链接；
- Base64 编码的分享链接列表。

严格解析的分享链接协议：

| 协议 | 输入形式 | 已解析能力 |
|---|---|---|
| VLESS | `vless://` | TCP/raw、WebSocket、gRPC、TLS、REALITY、flow、SNI、ALPN、uTLS 指纹 |
| VMess | v2rayN JSON `vmess://` | TCP/raw、WebSocket、gRPC、TLS、SNI、ALPN、cipher、alterId |
| Trojan | `trojan://` | TCP/raw、WebSocket、gRPC、TLS、REALITY、SNI、ALPN |
| Shadowsocks | SIP002 `ss://` | AEAD/AEAD-2022、`obfs-local`、`v2ray-plugin` 明确选项 |
| Hysteria2 | `hysteria2://` / `hy2://` | auth、多端口、obfs、SNI、insecure、证书 pin |

TUIC 等未列出的分享链接会被拒绝。Mihomo YAML 中的其他节点类型仍可进入节点库并用于 Mihomo 输出订阅，但进入 sing-box 输出订阅会严格失败。

订阅中的高置信度流量/到期信息条目会标为 notice，不作为代理节点送入任一编译器。识别格式、可信度与执法语义见 [LIFECYCLE.md](LIFECYCLE.md)。

## Mihomo 编译器

节点配置按原对象复制，只改成输出订阅内确定性唯一名称。编译器负责：

- 校验 `proxy-groups` 存在、组名唯一、插槽目标存在；
- 把节点添加到顶层 `proxies`；
- 按 `append/replace` 写入目标组的 `proxies`；
- 校验策略组成员与 `rules` 的目标引用存在；
- 保留未参与编译的完整模板配置。

节点本身的最终 schema 由 Mihomo 内核校验。官方文档：[代理节点通用字段](https://wiki.metacubex.one/en/config/proxies/)、[策略组](https://wiki.metacubex.one/config/proxy-groups/)、[URL-Test](https://wiki.metacubex.one/config/proxy-groups/url-test/)、[TUN](https://wiki.metacubex.one/en/config/inbound/tun/)、[DNS](https://wiki.metacubex.one/en/config/dns/)。

## sing-box 编译器

目标配置为当前 sing-box JSON 结构；平台预置模板标注为 1.14。模板中的 selector/urltest 与路由引用会严格校验。官方基础结构见 [Configuration](https://sing-box.sagernet.org/configuration/)、[Selector](https://sing-box.sagernet.org/configuration/outbound/selector/)、[URLTest](https://sing-box.sagernet.org/configuration/outbound/urltest/)、[Route Rule Action](https://sing-box.sagernet.org/configuration/route/rule_action/)。

### 映射矩阵

| Mihomo 节点 | sing-box outbound | 支持字段 |
|---|---|---|
| `vless` | `vless` | endpoint、uuid、flow、network、packet_encoding、TLS/REALITY、ws/grpc transport |
| `vmess` | `vmess` | endpoint、uuid、security、alter_id、network、packet_encoding、TLS、ws/grpc transport |
| `trojan` | `trojan` | endpoint、password、network、TLS/REALITY、ws/grpc transport |
| `ss` | `shadowsocks` | endpoint、method、password、obfs-local/v2ray-plugin 选项 |
| `hysteria2` / `hy2` | `hysteria2` | endpoint、server_ports、hop_interval、up/down Mbps、salamander/gecko obfs、password、SNI/insecure/ALPN |

通用可转换字段包括 `interface-name → bind_interface`、`routing-mark → routing_mark`、`tfo → tcp_fast_open`、`mptcp → tcp_multi_path`。对应官方结构：[Dial Fields](https://sing-box.sagernet.org/configuration/shared/dial/)、[TLS](https://sing-box.sagernet.org/configuration/shared/tls/)、[V2Ray Transport](https://sing-box.sagernet.org/configuration/shared/v2ray-transport/)。

协议字段依据：[VLESS](https://sing-box.sagernet.org/configuration/outbound/vless/)、[VMess](https://sing-box.sagernet.org/configuration/outbound/vmess/)、[Trojan](https://sing-box.sagernet.org/configuration/outbound/trojan/)、[Shadowsocks](https://sing-box.sagernet.org/configuration/outbound/shadowsocks/)、[Hysteria2](https://sing-box.sagernet.org/configuration/outbound/hysteria2/)。

### 明确拒绝的情况

- 非 ws/grpc/tcp/raw 的 V2Ray transport；或 transport option 中无等价目标字段的选项。
- VLESS `encryption` 非空且非 `none`。
- 未列出的协议或任何节点未知字段。
- `udp` 非布尔值；缺失或 `false` 会显式转换为 sing-box `network: tcp`，避免其默认同时开启 TCP/UDP。
- 非默认 `ip-version`，因为它需要订阅模板级 DNS resolver 策略，不能安全地局部猜测。
- Mihomo `dialer-proxy` 和未显式转换的 smux 扩展。
- REALITY 缺少 public key，或包含 sing-box 当前客户端 schema 无等价字段的扩展。
- Hysteria2 URI `pinSHA256`。

最后一项是刻意的语义边界：Hysteria2 官方 URI 把 `pinSHA256` 定义为服务器**证书**的 SHA-256 指纹，而 sing-box TLS 的 `certificate_public_key_sha256` 是**公钥**摘要，不能直接复制。来源：[Hysteria2 URI Scheme](https://v2.hysteria.network/docs/developers/URI-Scheme/)、[sing-box TLS](https://sing-box.sagernet.org/configuration/shared/tls/#certificate_public_key_sha256)。带该字段的节点仍可用于 Mihomo 输出订阅。

## sing-box 版本边界

平台模板使用新 DNS server 对象格式、route action 和合并后的 TUN `address` 字段，避免继续生成已经迁移的旧配置。版本变化依据：[sing-box Migration](https://sing-box.sagernet.org/migration/)。自定义模板应在 `engine_version` 中记录实际目标版本；升级内核时发布新模板版本，再显式编辑输出订阅采用新版本。
