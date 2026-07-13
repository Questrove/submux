# 协议兼容边界

submux 的内部主模型是 Clash/Mihomo 节点。Clash YAML 输出保留模型字段;Base64 输出是分享 URI/QR JSON 的兼容适配器,并不等于任意 Clash 节点都能无损转换。

## 设计原则

1. 协议语义以官方协议或项目文档为准。
2. 没有统一分享链接 RFC 的协议,以当前主流客户端 v2rayN 的解析/生成实现作为互操作目标。
3. 任何节点或连接选项不能在目标格式中可靠表达时,Base64 整体渲染失败并回退该格式的 last-good;不静默删节点或字段。
4. 带宽、Hysteria2 客户端模式等本地策略不写入分享链接;Hysteria2 官方明确把它们排除在 URI 之外。

## 支持矩阵

| 协议 | 分享链接输入 | Base64 输出 | 当前传输/扩展边界 |
|------|--------------|-------------|-------------------|
| VLESS | `vless://` | `vless://` | `tcp/raw`、WebSocket、gRPC;TLS、REALITY、flow、encryption、SNI、ALPN、fingerprint |
| Trojan | `trojan://` | `trojan://` | `tcp/raw`、WebSocket、gRPC;默认 TLS,支持 REALITY 兼容查询字段 |
| VMess | v2rayN QR JSON | v2rayN QR JSON | `tcp/raw`、WebSocket、gRPC、TLS;REALITY 和不能表达的传输选项拒绝转换 |
| Shadowsocks | SIP002 | SIP002 | 标准 AEAD 与 AEAD-2022;AEAD-2022 使用百分号编码的明文 userinfo;支持 `obfs-local`/Clash `obfs` 与 `v2ray-plugin` 的明确选项 |
| Hysteria2 | `hysteria2://`、`hy2://` | `hysteria2://` | 官方 auth、端口、obfs、SNI、证书跳过验证、证书 pin;输出兼容 v2rayN 的 `mport`;不输出带宽和客户端模式 |

不在表中的协议(当前包括 TUIC)或未列出的传输会触发严格转换错误。Clash YAML 源中的这些节点仍可原样进入 Clash 输出。

## UA 分发

- Clash/Mihomo/Meta 客户端返回 Clash YAML。
- v2rayN/v2rayNG/NekoBox/Hiddify 返回 Base64。
- sing-box 尚无专用适配器,与未知 UA 一样回退设置项 `default_format`(默认 Clash),避免错误猜测为 Base64。

## 规范与兼容来源

- [Hysteria2 URI Scheme](https://v2.hysteria.network/docs/developers/URI-Scheme/)
- [Shadowsocks SIP002 URI Scheme](https://shadowsocks.org/doc/sip002.html)
- [Shadowsocks SIP022 AEAD-2022](https://shadowsocks.org/doc/sip022.html)
- [Xray VLESS outbound](https://xtls.github.io/en/config/outbounds/vless.html)
- [Xray transport](https://xtls.github.io/en/config/transport.html)
- [Xray REALITY transport](https://xtls.github.io/en/config/transports/reality.html)
- [Trojan-Go share URL draft](https://p4gefau1t.github.io/trojan-go/developer/url/)
- [Mihomo Hysteria2 fields](https://wiki.metacubex.one/config/proxies/hysteria2/)
- [Mihomo VLESS fields](https://wiki.metacubex.one/config/proxies/vless/)
- [Mihomo VMess fields](https://wiki.metacubex.one/config/proxies/vmess/)
- [Mihomo Shadowsocks fields](https://wiki.metacubex.one/config/proxies/ss/)
- [v2rayN share-link formatter source](https://github.com/2dust/v2rayN/tree/master/v2rayN/ServiceLib/Handler/Fmt)
