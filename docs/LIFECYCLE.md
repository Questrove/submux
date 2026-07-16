# 机场订阅生命周期

## 边界

机场生命周期元数据没有统一、完整的标准。submux 同时观察：

1. HTTP `Subscription-Userinfo` 响应头中的 `upload`、`download`、`total`、`expire`。
2. 机场伪装成代理条目的流量、到期和公告信息节点。
3. 管理员对信息节点分类与可信度的显式覆盖。

`Subscription-Userinfo` 是客户端生态中的事实约定，不视为强制协议。Clash for Windows 的历史文档描述了该响应头；subconverter 的示例配置同时提供从 `剩余流量`、`Bandwidth`、`过期时间`、`到期时间` 和 `Smart Access expire` 等节点名提取 userinfo 的规则：

- [Clash for Windows subscription-userinfo](https://docs.cfw.lbyczf.com/contents/urlscheme.html#subscription-userinfo)
- [subconverter pref.example.toml](https://github.com/tindy2013/subconverter/blob/master/pref.example.toml)

因此解析器只把完整、锚定的格式判为高置信度。包含“流量”“时间”等模糊词语的真实节点不会被自动排除。

## 状态模型

生命周期分为两个正交维度：

| 维度 | 状态 |
|---|---|
| 权益 | `unknown`、`active`、`expiring`、`expired`、`quota_exhausted` |
| 新鲜度 | `never_refreshed`、`fresh`、`stale`、`refresh_error` |

Source 的 `enabled` 仍只表达管理员意图。系统不会因为到期而修改它，保证续费刷新后可以自动恢复。

## 元数据优先级

字段按下列优先级合并：

1. `Subscription-Userinfo` Header。
2. 高置信度信息节点名称。
3. 上一次成功观察值，缺少新观察时标为 stale。

Header 与节点名冲突时保留 Header，并把冲突暴露给管理 API。流量字段允许 `1%` 误差，容差最低 `100 MiB`、最高 `1 GiB`，避免把节点名称取整或短暂刷新时间差误报为冲突。到期节点只提供日期时，与 Header 的精确时间落在同一 UTC 日期即视为一致，并保留 Header 的精确时间。`expire=0` 或 Header 缺失不会静默清除以前已知的到期时间；旧值保留并标为 stale。

生态约定没有统一定义 `total=0`。实际机场会用它表达不限流量或未提供限额，因此 submux 不把零总量推导为“剩余 0”：页面显示“不限流量”，strict 也不会据此判定流量耗尽。显式 `total=0` 会清除以前观察到的有限剩余量，且优先于节点名称中的流量提示。

信息节点产生的到期或流量耗尽默认只用于展示和预警。只有 Source 开启 `trust_node_notices` 后才可以触发 strict 执法。

## 信息节点

当前高置信度格式包括：

- `剩余流量：12.5 GB`
- `流量剩余 12 GiB`
- `Traffic Remaining: 12 GB`
- `剩余流量：12 GB | 总流量：100 GB`
- `Bandwidth: 88 GB/100 GB`
- `到期时间：2026-08-01`
- `过期时间：2026-08-01 12:30:00`
- `Expire: 2026/08/01`
- `套餐到期：长期有效`（同类表达还包括永久有效、永不过期、无限期和不过期）

单位 B、KB、MB、GB、TB、KiB、MiB、GiB、TiB 都按 1024 进制换算。无时区的纯日期按 UTC 当日 `23:59:59` 处理；“长期有效”类信息记录为无到期时间。由于节点名缺少可信时区，默认不允许它直接触发 strict。

官网、客服、公告、Website、Support 等前缀只产生中置信度候选，不会自动排除。管理员可把订阅条目强制分类为 `proxy` 或 `notice`；覆盖按节点语义 fingerprint 跨刷新保留。

## 到期策略

### continuity（默认）

- 过期或流量耗尽节点仍可用于输出订阅。
- 编译产物记录 warning。
- 公开订阅继续返回 `200` 和 last-good。
- `X-Submux-Degraded` 只返回通用生命周期警告，不泄露机场名称。

### strict

- 仅排除可信元数据判定为 expired 或 quota_exhausted 的来源节点。
- 有替代节点时发布不含过期来源的新产物。
- 必需插槽为空时保留旧产物供审计，但设置 `blocked_reason`，公开订阅返回 `503`，不会继续下发旧节点。
- 续费刷新后自动重新编译并解除 blocked。

输出订阅自身的 `expires_at` 独立于机场生命周期，到期仍返回 `410`。

## 刷新与扫描

- 网络错误和 HTTP 5xx 最多重试三次，错误文本不得包含订阅 URL 或 Token。
- 刷新失败保留上一次节点和元数据，只更新 `last_error`。
- 仅返回信息节点时更新生命周期观察，但保留上一次代理节点快照。
- 服务启动和每小时执行生命周期扫描，处理没有恰好发生网络刷新的时间型状态转换。
- 状态转换写入 `lifecycle_events`，同一状态不会重复产生事件。

## 数据安全

上游原始正文仍不落库。数据库只保存规范化代理条目、用于审计的信息条目、结构化生命周期元数据和状态转换事件。实时测试由 `SUBMUX_LIVE_URLS` 门控，私有 URL 不进入测试夹具或日志。
