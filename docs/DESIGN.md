# submux v4 设计

## 定位

submux 是配置编排器，不是订阅文件合并器，也不运行代理内核。上游机场只拥有节点连接信息；策略组、入口和运行场景由平台模板拥有，域名/IP 分流及其 DNS 策略由规则方案拥有。

核心约束：

1. 不继承机场策略，不做全局覆盖。
2. 不根据 User-Agent 猜输出；一个输出订阅永远只对应一个引擎。
3. 模板版本不可变，输出订阅显式固定版本。
4. 任何有损转换都必须失败，不能部分输出。
5. 客户端请求只读预编译产物，不拉上游、不现场拼装。

## 领域模型

| 模型 | 作用 | 关键字段 |
|---|---|---|
| Source | 节点来源 | `kind=subscription/manual`、URL、UA、标签、启用状态、生命周期策略、刷新网络方式 |
| NodeRecord | 统一节点库记录 | 来源、原始名称、协议、规范化配置、语义指纹、配置版本、标签、启用状态、proxy/notice 角色 |
| Template | 模板目录元数据 | 引擎、场景、描述、当前版本 |
| TemplateVersion | 不可变完整配置 | 完整 YAML/JSON、目标内核版本、自动识别的节点组、校验和 |
| RuleProfile | Mihomo 规则方案 | 固定规则目录版本、有序规则选择、自定义规则、处理方式、兜底方式 |
| OutputSubscription | 可发布配置实例 | 固定模板版本、规则方案、每个插槽的有序节点 ID、固定引擎、独立 token、到期时间 |
| SubscriptionArtifact | 预编译产物 | body、Content-Type、revision、last-success、last-error、warnings、blocked reason |

SourceCache 保存带 provenance 的生命周期元数据；高置信度 notice 不可被输出订阅选择，但保留规范化记录用于审计和人工分类。生命周期状态、信息节点格式与执法策略见 [LIFECYCLE.md](LIFECYCLE.md)。

节点组是模板发布时根据标准名称自动生成的内部数据，不由页面另行编辑：

```json
{"key":"primary","target":"PROXY","mode":"replace","required":true}
```

- Mihomo 必须提供 `PROXY` 主代理组，可以提供 `MEDIA` 流媒体组。
- sing-box 必须提供 `PROXY` 或 `AUTO` 主代理 outbound，可以提供 `MEDIA` 流媒体 outbound。
- `append` 保留模板静态成员后追加节点；`replace` 用节点结果替换目标成员。
- `required` 插槽未绑定或解析为空时，编译失败。

## 数据流

### 来源刷新

```text
直连 HTTP(S) 下载（10 MiB 上限）
  -> HTTPS 来源按设置在网络连接失败时尝试平台资源代理
  -> 解析订阅
  -> 规范化 NodeRecord
  -> 按连接语义计算 fingerprint
  -> 单事务替换该来源节点 + 更新刷新元数据
  -> 重编译所有启用的输出订阅
```

数据库不保存上游原文，也不保存第二份序列化节点快照。刷新时先按 fingerprint 精确关联；连接配置变化时，仅对来源内新旧快照中名称都唯一的条目按名称关联并沿用 Node ID、标签、启用状态和订阅选择，同时递增配置版本。重名歧义不猜测，按删除和新增处理。名称始终采用上游当前值；消失的节点从该来源快照删除。单来源失败只更新 `last_error`，上一份规范化节点快照保持不变。

新建机场来源会立即执行一次同步刷新，使节点和生命周期状态在创建响应返回时可用。首次刷新失败仍保留来源并记录 `last_error`，API 返回刷新结果供控制台明确提示；手工来源不执行网络刷新。

机场刷新默认只直连。单个 HTTPS 来源可以开启“直连失败后尝试平台资源代理”：只有超时、拒绝连接、连接重置等网络错误会自动回退；HTTP 状态错误、证书校验失败、内容超限或订阅解析失败不会切换线路。页面也允许在失败后显式通过平台资源代理重试一次。缓存分别记录成功线路、直连错误和代理错误，失败不会覆盖上一份节点快照。直连客户端明确忽略 submux 进程的 `HTTP_PROXY`、`HTTPS_PROXY` 环境变量。

平台资源代理在设置页独立保存，支持直连、自定义 HTTP 和 SOCKS5。它只供控制面读取 MetaCubeX 目录，以及为明确开启回退或手工重试的机场来源下载订阅；不会修改系统设置，也不影响 Agent、Mihomo 或其他程序。

手工节点导入不要求用户先创建来源。未指定 `source_id` 时，存储层会自动创建或复用内置“自建节点”手工分组；该分组始终启用，不能通过管理 API 修改或删除。`source_id` 仍作为内部兼容参数保留。

### 节点选择解析

输出订阅为模板的每个插槽保存一个有序节点 ID 列表。解析时按用户在右侧已选列表中的顺序处理，并依次应用：

1. 节点仍存在且不是信息节点；
2. 来源和节点处于启用状态；
3. strict 生命周期过滤；
4. 跨插槽按 fingerprint 去重，同时保留首次出现顺序。

多插槽选择相同 fingerprint 时，输出订阅中只生成一个节点实体。生成名称固定使用“来源名称 + 上游节点名称”，冲突时追加节点 ID。节点在来源刷新后消失、停用或被 strict 排除时会产生 warning；必需插槽为空则保留 last-good。

### 模板发布

发布前会解析完整模板，根据标准代理组识别节点注入位置，并校验基础引用。页面只提交完整配置和目标内核版本，不再提交节点插槽 JSON。每次发布创建新的 TemplateVersion 并更新 Template 的 `current_version_id`，旧版本不修改。已有输出订阅仍固定旧版本；升级需要显式编辑订阅。

### 规则方案

项目随版本携带 MetaCubeX `meta-rules-dat` 的 geosite/geoip 文件目录和对应提交号，不携带规则正文。管理员可以在规则页面手工通过 GitHub API 刷新目录，请求使用当前平台资源代理。新目录必须包含完整的 geosite/geoip 树和内置方案所需规则，校验通过后才成为可用版本；请求失败、返回截断或内容不完整时继续使用上一次成功版本。请求保存 ETag、Last-Modified 和 GitHub 速率限制信息。当前不接入 GitHub 登录，公开 REST API 通常按出口 IP 限制为每小时 60 次请求，页面显示响应中的剩余次数和重置时间。

目录中的规则均可搜索和添加。规则方案固定创建或上次手工更新时的目录提交，只保存启用项、顺序和 `direct`、`proxy`、`media`、`reject` 处理方式。刷新目录不会改变已有方案；用户点击“更新规则版本”后，平台先确认新目录仍包含方案选择的全部规则，再更新固定提交并重新编译相关订阅。自定义 `DOMAIN`、`DOMAIN-SUFFIX`、`DOMAIN-KEYWORD`、`DOMAIN-WILDCARD`、`IP-CIDR` 和 `IP-CIDR6` 规则始终排在规则库之前。

编译 Mihomo 时只为启用项生成 `rule-provider`，URL 固定到目录快照对应的上游提交，格式为 MRS，下载更新统一设置 `proxy: PROXY`。域名 provider 同时生成 `nameserver-policy`：直连使用模板的 `direct-nameserver`，主代理和流媒体分别通过 `PROXY`、`MEDIA` 查询，拦截规则返回空结果。模板不再保存另一份 `rule-providers` 或 `rules`。

内置“常用规则”包含私有地址、国内外地区分流、微软、Apple、Steam、开发服务和常见流媒体；广告过滤保留在目录中但默认不启用。内置方案的自定义规则默认为空，管理员添加的域名、网段和处理方式只保存在自己的规则方案中。宽泛集合之前必须放置对应的具体集合，例如 `microsoft@cn` 和 `onedrive` 在 `microsoft` 之前，`apple-cn` 和 Apple Music 在 `apple` 之前。

### 输出订阅编译与发布

```text
OutputSubscription + TemplateVersion + RuleProfile + bindings
  -> 按顺序解析每个插槽的已选节点
  -> 全局节点去重与确定性命名
  -> 引擎编译器注入节点与插槽成员
  -> Mihomo 编译器按规则顺序注入 provider、rules 与 DNS policy
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
| `rule_profiles` | RuleProfile |
| `rule_catalog_snapshots` | 按提交号保存已校验的 MetaCubeX 规则目录 |
| `subscriptions` | OutputSubscription |
| `subscription_artifacts` | SubscriptionArtifact，以订阅 ID 为 key |
| `token_index` | 独立 token 到订阅 ID 的索引 |
| `lifecycle_events` | 机场权益状态转换事件 |
| `meta` | schema version |

v1 数据库首次打开时执行破坏式迁移：旧 Source 归类为 subscription；删除全局 output token、override 与全局 last-good；清除 source cache 中的原文。v2 升级到 v3 时补齐来源生命周期默认值。v3 升级到 v4 时保留来源、节点、模板、设置和生命周期历史，清理尚未发布产品中的 NodeSet/Profile 输出状态，并启用统一输出订阅模型。v6 升级到 v7 时增加规则方案存储；已有 Mihomo 输出订阅在启动时绑定内置“常用规则”并重新编译。v7 升级到 v8 时把开发阶段的 Mihomo 下载代理字段和任务改名为 Agent 资源代理。

## 模块边界

| 包 | 职责 |
|---|---|
| `internal/parse` | 上游 YAML、明文/Base64 分享链接解析 |
| `internal/node` | NodeRecord 构造、指纹、显示名、手工导入 |
| `internal/lifecycle` | Header/信息节点解析、元数据合并、状态计算与 strict 判定 |
| `internal/source` | 机场直连拉取、按条件使用平台代理、刷新与生命周期调度、原子提交 |
| `internal/resourceproxy` | 平台资源代理校验、保存和独立 HTTP 客户端 |
| `internal/store` | v4 领域持久化、有序节点选择与 token 索引 |
| `internal/rulecatalog` | MetaCubeX 规则目录快照、常用规则元数据与默认方案 |
| `internal/compiler` | 模板校验、节点选择解析、Mihomo/sing-box 编译、last-good |
| `internal/server` | 鉴权、管理 API、固定引擎订阅端点 |
| `web` | 来源、节点库、模板、规则方案和输出订阅控制台 |

旧的 `merge`、`override` 与 `output` 包已经删除，避免新旧抽象并存。

## 删除约束

- 含有已选节点的 Source 不可删除。
- 被输出订阅选择的手工 Node 不可删除。
- 任一版本被输出订阅使用的 Template 不可删除。
- 被输出订阅使用的 RuleProfile 不可删除；内置“常用规则”可以修改但不能删除。
- 机场节点不能逐条删除或修改连接配置；需要停用节点或刷新/删除来源。
- 手工节点可替换连接内容，元数据可独立修改。

## 平台预置模板

首次运行安装两套可继续发布新版本的 Mihomo 模板：

当前产品尚未发布，内置模板目录迁移可原地修正已有版本内容；正式发布后恢复严格的版本不可变约束。

- Mihomo 桌面 TUN：IPv4-only、mixed TUN、严格路由和 fake-ip DNS；内置模板不预设任何用户网络的路由排除项。
- Mihomo Linux 服务器：只向本机应用提供回环 mixed 代理，使用 redir-host 内部 DNS，不启用 TUN、透明代理、系统路由或进程匹配。

两个模板都提供必填 `PROXY` 节点槽位和可选 `MEDIA` 节点槽位。`MEDIA` 默认包含 `PROXY` 作为回退，用户选择流媒体节点后追加到该策略组。通用规则由规则方案注入，自定义域名和网段只有在管理员明确添加后才进入编译结果。

Linux 服务器模板遵循 Mihomo 官方配置边界：`allow-lan: false` 且 mixed 入口绑定 `127.0.0.1`；显式代理场景使用 redir-host 而非 fake-ip/DNS 劫持；MRS 只用于 domain/ipcidr provider；控制 API 地址和 secret 由 Agent 在部署时注入。依据见 [General configuration](https://wiki.metacubex.one/en/config/general/)、[DNS configuration](https://wiki.metacubex.one/en/config/dns/)、[Route Rules](https://wiki.metacubex.one/en/config/rules/) 与 [Rule-Providers](https://wiki.metacubex.one/en/config/rule-providers/)。官方 systemd 示例包含 TUN/透明代理所需的广泛 capabilities，但本项目的服务器 sidecar 不使用这些能力，始终保持普通用户权限。

旧版预置模板在升级时会被移除；若某个旧模板版本仍被输出订阅引用，则仅保留为隐藏的 `retired` 记录，不能再用于新建订阅。`Mihomo 桌面 TUN（推荐）` 与 `Mihomo 服务器 Sidecar（推荐）` 会原地改名并升级到当前模板，保留模板 ID 和现有订阅引用。用户自行创建的模板不受目录迁移影响。
