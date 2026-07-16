# submux v4 设计

## 定位

submux 是配置编排器，不是订阅文件合并器，也不运行代理内核。上游机场只拥有节点连接信息；DNS、路由、策略组、入口和运行场景都由平台模板拥有。

核心约束：

1. 不继承机场策略，不做全局覆盖。
2. 不根据 User-Agent 猜输出；一个输出订阅永远只对应一个引擎。
3. 模板版本不可变，输出订阅显式固定版本。
4. 任何有损转换都必须失败，不能部分输出。
5. 客户端请求只读预编译产物，不拉上游、不现场拼装。

## 领域模型

| 模型 | 作用 | 关键字段 |
|---|---|---|
| Source | 节点来源 | `kind=subscription/manual`、URL、UA、标签、启用状态、生命周期策略 |
| NodeRecord | 统一节点库记录 | 来源、原始名称、协议、规范化配置、语义指纹、配置版本、标签、启用状态、proxy/notice 角色 |
| Template | 模板目录元数据 | 引擎、场景、描述、当前版本 |
| TemplateVersion | 不可变完整配置 | 完整 YAML/JSON、目标内核版本、插槽、校验和 |
| OutputSubscription | 可发布配置实例 | 固定模板版本、每个插槽的有序节点 ID、固定引擎、独立 token、到期时间 |
| SubscriptionArtifact | 预编译产物 | body、Content-Type、revision、last-success、last-error、warnings、blocked reason |

SourceCache 保存带 provenance 的生命周期元数据；高置信度 notice 不可被输出订阅选择，但保留规范化记录用于审计和人工分类。生命周期状态、信息节点格式与执法策略见 [LIFECYCLE.md](LIFECYCLE.md)。

模板插槽只声明节点注入目标：

```json
{"key":"primary","target":"PROXY","mode":"replace","required":true}
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
  -> 重编译所有启用的输出订阅
```

数据库不保存上游原文，也不保存第二份序列化节点快照。刷新时先按 fingerprint 精确关联；连接配置变化时，仅对来源内新旧快照中名称都唯一的条目按名称关联并沿用 Node ID、标签、启用状态和订阅选择，同时递增配置版本。重名歧义不猜测，按删除和新增处理。名称始终采用上游当前值；消失的节点从该来源快照删除。单来源失败只更新 `last_error`，上一份规范化节点快照保持不变。

新建机场来源会立即执行一次同步刷新，使节点和生命周期状态在创建响应返回时可用。首次刷新失败仍保留来源并记录 `last_error`，API 返回刷新结果供控制台明确提示；手工来源不执行网络刷新。

手工节点导入不要求用户先创建来源。未指定 `source_id` 时，存储层会自动创建或复用内置“自建节点”手工分组；该分组始终启用，不能通过管理 API 修改或删除。`source_id` 仍作为内部兼容参数保留。

### 节点选择解析

输出订阅为模板的每个插槽保存一个有序节点 ID 列表。解析时按用户在右侧已选列表中的顺序处理，并依次应用：

1. 节点仍存在且不是信息节点；
2. 来源和节点处于启用状态；
3. strict 生命周期过滤；
4. 跨插槽按 fingerprint 去重，同时保留首次出现顺序。

多插槽选择相同 fingerprint 时，输出订阅中只生成一个节点实体。生成名称固定使用“来源名称 + 上游节点名称”，冲突时追加节点 ID。节点在来源刷新后消失、停用或被 strict 排除时会产生 warning；必需插槽为空则保留 last-good。

### 模板发布

发布前会解析完整模板、校验插槽目标与基础引用。每次发布创建新的 TemplateVersion 并更新 Template 的 `current_version_id`，旧版本不修改。已有输出订阅仍固定旧版本；升级需要显式编辑订阅。

### 输出订阅编译与发布

```text
OutputSubscription + TemplateVersion + bindings
  -> 按顺序解析每个插槽的已选节点
  -> 全局节点去重与确定性命名
  -> 引擎编译器注入节点与插槽成员
  -> 严格引用/协议字段校验
  -> SHA-256(最终产物) 作为 revision
  -> 原子替换该订阅 artifact
```

保存输出订阅时先 Preview，成功后才持久化并发布。来源或节点变化会自动重编译启用的订阅。一般重编译失败时只更新该 artifact 的 `last_error`，原 body、revision 与 last-success 不变；公开链接继续返回 last-good，并附带 `X-Submux-Degraded`。strict 生命周期过滤导致必需插槽为空时会额外设置 `blocked_reason`，旧 body 只供审计，公开链接返回 503。

### 订阅请求

`GET /sub/{token}` 只执行：token 索引查找、启用/到期校验、生命周期阻断校验、读取 artifact、返回固定 Content-Type。Mihomo 返回 YAML，sing-box 返回 JSON；User-Agent 不参与决策。无产物或 strict 生命周期阻断返回 503，输出订阅自身过期返回 410，未知或禁用 token 返回 401。

## 存储

bbolt bucket：

| bucket | 内容 |
|---|---|
| `settings` | 管理凭据、session secret、base URL、刷新设置、预置模板标记 |
| `sources` | Source |
| `source_cache` | 流量元数据、刷新时间与错误，不含订阅原文 |
| `nodes` | NodeRecord |
| `templates` | Template |
| `template_versions` | TemplateVersion |
| `subscriptions` | OutputSubscription |
| `subscription_artifacts` | SubscriptionArtifact，以订阅 ID 为 key |
| `token_index` | 独立 token 到订阅 ID 的索引 |
| `lifecycle_events` | 机场权益状态转换事件 |
| `meta` | schema version |

v1 数据库首次打开时执行破坏式迁移：旧 Source 归类为 subscription；删除全局 output token、override 与全局 last-good；清除 source cache 中的原文。v2 升级到 v3 时补齐来源生命周期默认值。v3 升级到 v4 时保留来源、节点、模板、设置和生命周期历史，清理尚未发布产品中的 NodeSet/Profile 输出状态，并启用统一输出订阅模型。

## 模块边界

| 包 | 职责 |
|---|---|
| `internal/parse` | 上游 YAML、明文/Base64 分享链接解析 |
| `internal/node` | NodeRecord 构造、指纹、显示名、手工导入 |
| `internal/lifecycle` | Header/信息节点解析、元数据合并、状态计算与 strict 判定 |
| `internal/source` | HTTP 拉取、有限重试、刷新与生命周期调度、原子提交 |
| `internal/store` | v4 领域持久化、有序节点选择与 token 索引 |
| `internal/compiler` | 模板校验、节点选择解析、Mihomo/sing-box 编译、last-good |
| `internal/server` | 鉴权、管理 API、固定引擎订阅端点 |
| `web` | 来源、节点库、模板、输出订阅四阶段控制台 |

旧的 `merge`、`override` 与 `output` 包已经删除，避免新旧抽象并存。

## 删除约束

- 含有已选节点的 Source 不可删除。
- 被输出订阅选择的手工 Node 不可删除。
- 任一版本被输出订阅使用的 Template 不可删除。
- 机场节点不能逐条删除或修改连接配置；需要停用节点或刷新/删除来源。
- 手工节点可替换连接内容，元数据可独立修改。

## 平台预置模板

首次运行安装五套普通、可继续发布新版本的模板；已有 v2 模板目录升级时只补装新的桌面 TUN 模板，不修改已有输出订阅固定的版本：

当前产品尚未发布，内置模板目录迁移可原地修正已有版本内容；正式发布后恢复严格的版本不可变约束。

- Mihomo 桌面 TUN（推荐）：IPv4-only、mixed TUN、严格路由、fake-ip DNS、MRS 国内外分流、单层 `PROXY` 手动选择组，并排除 `192.0.2.0/24` 与 `198.51.100.0/24` 两个 WireGuard 网段。
- Mihomo 桌面：本机 mixed-port、fake-ip DNS、单层 `PROXY` 手动选择组，作为系统代理兼容模式。
- Mihomo 网关：LAN、TUN、DNS 劫持、自动路由与单层 `PROXY` 手动选择组。
- sing-box 桌面：本机 mixed 入口、系统代理、1.14 DNS server 格式。
- sing-box 服务器：回环 mixed sidecar、低日志级别与持久 DNS cache。

这里的“服务器”是服务器作为代理客户端/sidecar 使用，不是生成 VLESS/Trojan 服务端部署配置。
