# submux v2 设计

## 定位

submux 是配置编排器，不是订阅文件合并器，也不运行代理内核。上游机场只拥有节点连接信息；DNS、路由、策略组、入口和运行场景都由平台模板拥有。

核心约束：

1. 不继承机场策略，不做全局覆盖。
2. 不根据 User-Agent 猜输出；一个 Profile 永远只对应一个引擎。
3. 模板版本不可变，Profile 显式固定版本。
4. 任何有损转换都必须失败，不能部分输出。
5. 客户端请求只读预编译产物，不拉上游、不现场拼装。

## 领域模型

| 模型 | 作用 | 关键字段 |
|---|---|---|
| Source | 节点来源 | `kind=subscription/manual`、URL、UA、标签、启用状态、生命周期策略 |
| NodeRecord | 统一节点库记录 | 来源、协议、规范化配置、语义指纹、Alias、标签、启用状态、proxy/notice 角色 |
| NodeSet | 可复用的节点选择器 | 来源 ID、节点 ID、排除 ID、协议、名称、标签 |
| Template | 模板目录元数据 | 引擎、场景、描述、当前版本 |
| TemplateVersion | 不可变完整配置 | 完整 YAML/JSON、目标内核版本、插槽、校验和 |
| Profile | 可发布配置实例 | 固定模板版本、插槽绑定、固定引擎、独立 token、到期时间 |
| ProfileArtifact | 预编译产物 | body、Content-Type、revision、last-success、last-error、warnings、blocked reason |

SourceCache 保存带 provenance 的生命周期元数据；高置信度 notice 不参与 NodeSet 和编译，但保留规范化记录用于审计和人工分类。生命周期状态、信息节点格式与执法策略见 [LIFECYCLE.md](LIFECYCLE.md)。

模板插槽只声明节点注入目标：

```json
{"key":"primary","target":"AUTO","mode":"replace","required":true}
```

- Mihomo 的 `target` 必须是现有 `proxy-groups[].name`。
- sing-box 的 `target` 必须是现有 `selector` 或 `urltest` outbound tag。
- `append` 保留模板静态成员后追加节点；`replace` 用节点结果替换目标成员。
- `required` 插槽未绑定或解析为空时，编译失败。

## 数据流

### 来源刷新

```text
HTTP(S) 下载（10 MiB 上限）
  -> 解析订阅
  -> 规范化 NodeRecord
  -> 按连接语义计算 fingerprint
  -> 单事务替换该来源节点 + 更新刷新元数据
  -> 重编译所有启用 Profile
```

数据库不保存上游原文，也不保存第二份序列化节点快照。刷新时相同 fingerprint 沿用 Node ID、Alias、标签和启用状态；消失的节点从该来源快照删除。单来源失败只更新 `last_error`，上一份规范化节点快照保持不变。

### NodeSet 解析

来源选择与显式节点选择取并集；两者都为空表示全部启用节点。随后依次应用：

1. 来源和节点启用状态；
2. 显式排除 ID；
3. 协议过滤；
4. 显示名称包含过滤；
5. 必须同时具备的标签过滤。

多插槽或多来源得到相同 fingerprint 时，Profile 中只生成一个节点实体。名称按稳定顺序生成；未设置 Alias 时带来源前缀，冲突时追加节点 ID。

### 模板发布

发布前会解析完整模板、校验插槽目标与基础引用。每次发布创建新的 TemplateVersion 并更新 Template 的 `current_version_id`，旧版本不修改。已有 Profile 仍固定旧版本；升级需要显式修改 Profile。

### Profile 编译与发布

```text
Profile + TemplateVersion + bindings
  -> 解析每个 NodeSet
  -> 全局节点去重与确定性命名
  -> 引擎编译器注入节点与插槽成员
  -> 严格引用/协议字段校验
  -> SHA-256(最终产物) 作为 revision
  -> 原子替换该 Profile artifact
```

保存 Profile 时先 Preview，成功后才持久化并发布。来源或节点变化会自动重编译启用的 Profile。一般重编译失败时只更新该 artifact 的 `last_error`，原 body、revision 与 last-success 不变；公开链接继续返回 last-good，并附带 `X-Submux-Degraded`。strict 生命周期过滤导致必需插槽为空时会额外设置 `blocked_reason`，旧 body 只供审计，公开链接返回 503。

### 订阅请求

`GET /sub/{profile-token}` 只执行：token 索引查找、启用/到期校验、生命周期阻断校验、读取 artifact、返回固定 Content-Type。Mihomo 返回 YAML，sing-box 返回 JSON；User-Agent 不参与决策。无产物或 strict 生命周期阻断返回 503，Profile 自身过期返回 410，未知或禁用 token 返回 401。

## 存储

bbolt bucket：

| bucket | 内容 |
|---|---|
| `settings` | 管理凭据、session secret、base URL、刷新间隔、预置模板标记 |
| `sources` | Source |
| `source_cache` | 流量元数据、刷新时间与错误，不含订阅原文 |
| `nodes` | NodeRecord |
| `node_sets` | NodeSet |
| `templates` | Template |
| `template_versions` | TemplateVersion |
| `profiles` | Profile |
| `profile_artifacts` | ProfileArtifact，以 profile ID 为 key |
| `token_index` | 独立 token 到 profile ID 的索引 |
| `lifecycle_events` | 机场权益状态转换事件 |
| `meta` | schema version |

v1 数据库首次打开时执行破坏式迁移：旧 Source 归类为 subscription；删除全局 output token、override 与全局 last-good；清除 source cache 中的原文。v2 升级到 v3 时，现有机场默认使用 continuity 和提前 7 天预警，不删除节点或 Profile。旧来源可在刷新后进入统一节点库并补齐生命周期元数据。

## 模块边界

| 包 | 职责 |
|---|---|
| `internal/parse` | 上游 YAML、明文/Base64 分享链接解析 |
| `internal/node` | NodeRecord 构造、指纹、显示名、手工导入 |
| `internal/lifecycle` | Header/信息节点解析、元数据合并、状态计算与 strict 判定 |
| `internal/source` | HTTP 拉取、有限重试、刷新与生命周期调度、原子提交 |
| `internal/store` | v2 领域持久化与 token 索引 |
| `internal/compiler` | 模板校验、NodeSet 解析、Mihomo/sing-box 编译、last-good |
| `internal/server` | 鉴权、管理 API、固定 Profile 订阅端点 |
| `web` | 五步配置编排控制台 |

旧的 `merge`、`override` 与 `output` 包已经删除，避免新旧抽象并存。

## 删除约束

- 被 NodeSet 显式引用的 Source 或手工 Node 不可删除。
- 被 Profile 绑定的 NodeSet 不可删除。
- 任一版本被 Profile 使用的 Template 不可删除。
- 机场节点不能逐条删除或修改连接配置；需要停用节点或刷新/删除来源。
- 手工节点可替换连接内容，元数据可独立修改。

## 平台预置模板

首次运行安装四套普通、可继续发布新版本的模板：

- Mihomo 桌面：本机 mixed-port、fake-ip DNS、自动测速。
- Mihomo 网关：LAN、TUN、DNS 劫持与自动路由。
- sing-box 桌面：本机 mixed 入口、系统代理、1.14 DNS server 格式。
- sing-box 服务器：回环 mixed sidecar、低日志级别与持久 DNS cache。

这里的“服务器”是服务器作为代理客户端/sidecar 使用，不是生成 VLESS/Trojan 服务端部署配置。
