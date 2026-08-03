# Submux Runtime 本机 IPC

本文定义 GUI、TUI、CLI 与 Submux Runtime 之间的唯一管理接口。当前已经实现 Unix Socket、Windows Named Pipe、对端身份校验、Snapshot、事件流、一次性内容上传、分层候选配置预览、托管资源、本机高级覆盖、三类来源的添加、刷新、切换与删除、持久化运行操作、审计、秘密读取、便携备份与整体恢复、诊断包和 Tauri GUI 桥接。

## 传输

协议使用带版本前缀的 HTTP/JSON，但不监听 TCP：

| 系统 | 外部管理传输 | 对端身份 |
|---|---|---|
| Linux | Unix Domain Socket | `SO_PEERCRED` |
| macOS | Unix Domain Socket | `LOCAL_PEERCRED` |
| Windows | Named Pipe | 客户端进程 token 与 SID |

第一版不提供回环 TCP、远程 Named Pipe、WebSocket 或浏览器直连作为后备路径。Windows Named Pipe 必须设置 `PIPE_REJECT_REMOTE_CLIENTS`。

所有请求都必须：

- 使用 `/v1` 路径前缀；
- 携带唯一请求 ID；
- 通过 `X-Submux-Client-Type` 和 `X-Submux-Client-Version` 指定客户端类型与版本；
- 使用严格 JSON，拒绝未知字段、重复字段和不符合类型的值；
- 受固定请求头、正文、字符串、数组和嵌套深度上限约束；
- 在响应和日志中脱敏凭据。

## 操作系统授权

Runtime 不建立第二套账号或会话。

### Linux

- Runtime 以无登录权限的 `submux-runtime` 专用账户运行。
- root 始终可以管理。
- `submux-runtime` 操作员组成员可以连接管理 Socket。
- 服务器安装默认只允许 root；桌面安装可以在用户明确确认后把当前用户加入操作员组。
- 状态目录只允许 Runtime 账户和 root 读取，操作员通过 IPC 访问。

服务账户与操作员组使用相同名称，但不是相同权限：状态目录和文件固定为 `0700`/`0600` 且 Runtime 使用 `umask 0077`，只有管理 Socket 使用操作员组和 `0660`。安装与升级测试必须验证新增组成员不能直接读取状态文件。

### Windows

- Runtime 服务使用虚拟账户 `NT SERVICE\SubmuxRuntime`。
- 本地组 `Submux Runtime Operators` 的成员可以管理。
- 管理 Pipe 只允许 Runtime 服务 SID、LocalSystem、已提升的 Administrators 和操作员组。
- 桌面安装可以直接把执行安装的用户 SID 写入 Pipe ACL，并同时加入操作员组，使首次使用不依赖重新登录。
- 服务器安装默认只允许已提升的 Administrators，除非安装时明确指定操作员。

### macOS

- Runtime 作为 LaunchDaemon，以 `_submux-runtime` 专用账户运行。
- `submux-runtime-operators` 组成员可以连接管理 Socket。
- root 可以通过 `sudo` 管理；普通管理员不会仅因属于 admin 组而自动获得非提权访问。
- 桌面安装可以在确认后把当前用户加入操作员组；服务器式安装默认只允许 root。

所有获授权的操作员权限相同。只读角色、Runtime 密码和 per-client token 不属于第一版。

## 接口

### 读取快照

```http
GET /v1/snapshot
```

Snapshot 至少包含：

- 单调递增的 `revision`；
- Runtime 版本、服务状态和当前故障；
- Mihomo 安装版本、期望状态 `desired_state`、实际状态 `state` 和崩溃恢复状态 `recovery`；
- 当前十分钟窗口内已使用的自动重启次数、下次重试时间和脱敏故障；
- 当前运行方式及脱敏的网络设置；
- 配置来源摘要、当前来源和最近刷新结果；
- 托管资源的类型、摘要和总量，本机高级覆盖的摘要，以及流量策略的当前选择、字段来源和实际应用值；
- 当前运行操作、排队数量和最近一个终态运行操作的标识；
- 产品与核心更新状态；
- 最近一次整体恢复和机器设置待确认状态；
- 当前上传、下载速度，活动连接数，以及本次 Mihomo 运行的累计上传、下载量；
- 最新事件游标。

普通 Snapshot 不包含完整来源地址、凭据、配置正文、Mihomo secret 或可直接调用的特权进程信息。

### 读取流量历史

```http
GET /v1/traffic/history?since={RFC3339Nano}&limit={1..1024}
GET /v1/traffic/history?after={cursor}&limit={1..1024}
```

首次读取按 `since` 请求最近 1、5 或 15 分钟的样本，后续使用 `after` 只读取更大的游标；两者不能同时出现。响应最多返回 1024 个样本，同时给出当前流量状态、最早和最新游标。游标已经早于内存保留范围时，响应设置 `reset_required` 并返回当前有界窗口，客户端据此替换旧曲线。

Runtime 每秒从 Mihomo 本机控制接口读取累计上传、下载量和活动连接数，在内存中计算当前速度，只保留最近 15 分钟。读取短暂失败时保留最后一次确认的累计值并标记不可用；Mihomo 停止、重启或计数器回退时插入 `discontinuity` 样本，恢复采样后把 Snapshot 累计值切换到新的 Mihomo 运行。Runtime 重启后历史自然清空。流量样本和累计值不写入状态数据库、日志或备份，也不计算跨 Mihomo 运行的长期流量。

### 读取活动连接

```http
GET /v1/connections?target={text}&process={text}&rule={text}&node={text}&page={n}&page_size={1..100}
```

该只读接口返回当前活动连接的有界分页结果。每条连接包含稳定连接 ID、来源、目标、协议、入站类型、进程名称、匹配规则及内容、出站链、开始时间、持续时间和上传、下载量。`target`、`process`、`rule`、`node` 均为不区分大小写的包含筛选，留空表示不限；默认每页 20 条，最多 100 条，完整响应不得超过 1 MiB。响应中的 `available` 表示本次 Mihomo 采集是否成功，`observed_at` 表示这些连接最后一次成功采集的时间。

Runtime 与流量采集共用 Mihomo 本机控制连接。客户端只调用 Runtime 本机 IPC，不直接连接 Mihomo，也不会取得 Mihomo secret。Mihomo 读取短暂失败时，Runtime 保留最后一次成功的连接结果并将 `available` 设为 `false`；客户端应只把连接区域标记为过期，使用最长五秒的退避间隔继续重试，其他页面和区域继续可用。接口不返回进程路径，连接明细不进入 Snapshot、状态数据库、日志或备份。

### 读取最终规则

```http
GET /v1/rules?content={text}&type={text}&target={text}
```

该只读接口从 `current/config.yaml` 读取当前运行配置的最终规则，并用同一配置集中的 `source.yaml` 判断非 Runtime 规则来自配置来源还是本机高级覆盖。响应明确标记 `view: "applied"`，同时返回配置摘要，以及每条规则的最终顺序、类型、匹配条件、目标、来源和完整规则内容。`content`、`type` 和 `target` 都是可选的、不区分大小写的包含筛选，每项最多 256 个字符。接口不提供编辑、重排或提交能力。

候选配置预览响应中的 `rules` 使用相同结构，但标记为 `view: "candidate"`。候选规则在配置来源、本机高级覆盖、本机流量策略和 Runtime 保留规则全部合并后生成，因此顺序就是尚未应用候选的最终顺序。客户端必须明确显示当前运行配置与尚未应用候选配置的区别，不能把候选规则表述为已经生效。

Runtime 添加的健康检查规则标记为 `runtime`；其余规则标记为 `source`、`advanced_override`，无法从旧配置集确认时标记为 `current_configuration`。规则内容不写入 Snapshot、事件、审计或备份索引。活动连接详情可以用返回的 `rule` 和 `rule_payload` 作为类型和内容筛选，跳转到当前运行规则中的命中项。规则只能通过 Runtime 配置来源、本机高级覆盖或既有控制面流程修改。

关闭活动连接使用运行操作，不增加客户端可直接调用的 Mihomo 控制接口。`connection.close` 只接受稳定连接 ID 和可选的脱敏目标说明；连接已经消失时按幂等成功记录。连接分页结果还包含由当前筛选范围内全部稳定连接 ID 计算的 `scope_token`。`connection.close_scope` 只接受目标、进程、规则和节点筛选、界面显示并经操作员确认的连接数量、该范围令牌，以及 `confirm: true`。Runtime 执行前重新计算令牌；匹配成员或数量发生变化时拒绝操作并要求刷新后再次确认，因此确认后新出现的连接不会被关闭。令牌核验后、实际关闭前消失的连接仍按幂等成功记录。成功、失败、取消和结果未知都由同一套 Operation 状态与审计记录呈现。

### 上传导入内容

```http
POST /v1/imports
```

本机配置副本、托管资源、备份和离线包由客户端打开后把字节流上传给 Runtime，不能把客户端文件路径交给 Runtime。请求声明内容类型、大小和客户端计算的 SHA-256；Runtime 在硬上限内重新计算摘要，保存到固定暂存区并返回短期 `content_id`。

`content_id` 绑定 Runtime 安装实例和上传者 OS 身份，默认三十分钟过期，只能被一次后续 Action 消费。暂存内容不能执行、不能被 Mihomo 引用，也不能传给特权进程；Action 完成、失败或过期后清理。运行中导入的离线更新包只有通过 Runtime 校验并变成受信任 bundle ID 后，平台安装器才能接收该 ID。

远程来源的 URL、凭据和兼容设置使用 `application/vnd.submux.runtime-source+json` 上传。客户端随后只能把返回的 `content_id` 交给固定的 `source.add_remote` Action；URL 和凭据不会进入 Action、Operation、Snapshot 或事件。Runtime 消费暂存内容后自行规范化目标、执行地址策略检查、下载、生成候选配置并用当前 Mihomo 精确版本校验。

托管资源使用 `application/vnd.submux.managed-resource` 上传，随后由 `resource.add` 声明固定资源类型和安全名称。高级覆盖使用 YAML 上传，随后由 `override.set` 消费。两者都不能把客户端文件路径提交给 Runtime。

### 预览候选配置

```http
POST /v1/candidates/preview
Content-Type: application/json
```

请求必须且只能选择当前调用者尚未消费的配置 `content_id`，或已经保存的 `source_id`；还可以提供尚未消费的高级覆盖 `content_id` 来预览保存前结果，并用可选的 `traffic_policy` 预览跟随来源、规则、全局或直连的选择。Runtime 依次合并来源、本机高级覆盖和 Runtime 保留设置，再用准备运行的 Mihomo 精确版本完成静态校验。响应返回经过结构化脱敏的最终候选配置、摘要、显式代理监听、引用的托管资源、有效流量策略、结构化最终规则，以及每个字段的来源和替换状态。预览不消费导入内容、不创建运行操作、不保存流量策略、不切换当前配置，也不启动 Mihomo。响应始终使用 `Cache-Control: no-store`。

### 读取本机高级覆盖

```http
GET /v1/advanced-override?reveal=1
```

该接口只向已经通过操作系统身份校验、明确传入 `reveal=1` 的 Runtime 操作员返回当前 YAML 正文和摘要，并始终禁止缓存。CLI 使用 `override get --reveal`；TUI 与 GUI 在读取前要求二次确认。读取会留下只含操作者、客户端、时间和对象标识的审计记录，不记录 YAML 正文。修改仍必须上传 YAML，再创建 `override.set` Operation。

### 创建运行操作

```http
POST /v1/operations
Content-Type: application/json
```

请求外形固定为：

```json
{
  "request_id": "01J...",
  "if_revision": 42,
  "action": {
    "kind": "source.refresh",
    "params": {
      "source_id": "src_..."
    }
  }
}
```

`request_id` 按“Runtime 安装实例 ID + 调用者 OS 身份 + request_id”在本机数据库中去重，并至少保留到对应 Operation 被清理。客户端因连接中断重试同一请求时，Runtime 返回原 Operation，不再执行第二次；其他调用者重复使用同一 ID 时返回冲突。`if_revision` 与当前 Snapshot revision 不一致时返回冲突和最新 revision。

Action 使用固定 `kind` 和严格参数结构，不能承载 Shell、argv、任意环境变量、任意文件路径、下载 URL 转发或系统服务名。文件导入只提交已经上传的 `content_id`；任何特权操作都不能接收客户端提供的路径。

配置来源使用以下 Action：

- `source.add_remote` 只接受来源草稿的 `content_id`；
- `source.add_imported` 只接受 YAML 的 `content_id` 和不含控制字符的 `source_name`；
- `source.refresh` 只接受 `source_id` 和可选的一次性 `direct` 或 `mihomo` 线路；
- `source.apply` 只接受当前来源的 `source_id`；
- `source.switch` 接受目标 `source_id`、可选的一次性下载线路，以及显式的 `use_cached`；
- `source.delete` 接受 `source_id`；停止后删除当前来源还必须显式提交 `confirm`。

本机配置层当前使用以下 Action：

- `resource.add` 只接受资源内容的 `content_id`、固定 `resource_kind` 和安全 `resource_name`；
- `override.set` 只接受 YAML 的 `content_id`；
- `traffic_policy.set` 只接受 `traffic_policy`，值为 `follow_source`、`rule`、`global` 或 `direct`；
- `backup.restore` 只接受备份的 `content_id` 和 `confirm: true`。

流量策略默认是 `follow_source`。明确选择 `rule`、`global` 或 `direct` 后，Runtime 把它作为本机运行设置持久化，并在配置来源和本机高级覆盖之后写入候选，因此该字段拥有最终优先级；改回 `follow_source` 会删除持久设置。`global` 候选必须包含至少一个具名策略组。`traffic_policy.set` 在保存前使用当前来源、本机高级覆盖和已安装的 Mihomo 精确版本重新生成并校验候选；保存成功只改变本机运行设置，不立即应用配置或重启 Mihomo。

产品更新使用固定的稳定频道和以下 Action：

- `product.check` 不接受参数，只刷新固定 TUF 仓库的元数据，不下载程序；
- `product.update` 只接受 `product_plan_...`、`trust: "tuf"` 和 `confirm: true`；
- `product.rollback` 只接受 `confirm: true`。

配置来源、客户端和 Action 都不能提交更新 URL、仓库、频道、平台安装命令或文件路径。

添加和刷新只保存通过校验的不可变来源版本，不切换运行配置，也不启动 Mihomo。`source.apply` 应用当前来源的最近有效版本；Mihomo 原先停止时仍保持停止，原先运行时必须完成健康检查后才提交。

`source.switch` 对远程来源先执行刷新。刷新失败时默认终止；只有 `use_cached: true` 才能使用该目标来源最近一次通过校验的缓存。Runtime 完成目标配置应用和健康检查后才提交当前来源，失败则保留旧当前来源、旧最近可用配置和旧运行状态。操作结果返回 `source_id`、`previous_source_id` 和 `used_cached_source`，三个客户端据此显示相同结果。每个来源使用独立的 Mihomo 数据目录，策略选择、provider 缓存和兼容的 fake-IP 状态不会跨来源混用。

`source.delete` 删除非当前来源时不需要额外确认。当前来源在 Mihomo 运行时不能删除；停止后必须提交 `confirm: true`。当前来源刷新失败只更新该来源的失败记录和下一次刷新时间，不会自动切换到其他来源。

响应在副作用开始前返回已经持久化的 Operation：

```json
{
  "operation": {
    "id": "op_...",
    "request_id": "01J...",
    "state": "queued",
    "stage": "accepted"
  }
}
```

### 查询运行操作

```http
GET /v1/operations/{id}
```

Operation 状态只有：

- `queued`
- `running`
- `succeeded`
- `failed`
- `cancelled`
- `outcome_unknown`

操作记录包含阶段、进度、脱敏错误、调用者 OS 身份、客户端类型和版本。终态操作不可重放。

如果 Runtime 在无法确认某个外部副作用是否完成时崩溃，恢复后将 Operation 标记为 `outcome_unknown`。客户端必须重新读取 Snapshot 并由操作员决定下一步，Runtime 不能盲目重试。

### 取消运行操作

```http
POST /v1/operations/{id}/cancel
```

取消请求也必须携带新的 `request_id` 和客户端依据的 `if_revision`，并按普通修改请求持久化和去重。所有 Runtime 操作员权限相同，因此任一操作员都可以取消其他操作员发起但仍可取消的 Operation；界面必须显示原发起者和取消者。

取消只在操作仍排队，或者执行器明确声明当前阶段尚未产生系统副作用时生效。进入提交、程序替换、配置切换或网络修改阶段后返回 `not_cancellable`，但操作仍可被观察。取消与开始副作用发生竞态时，以已经持久化的阶段转换为准。

### 观察事件

```http
GET /v1/events?after={cursor}
```

成功响应的内容类型是 `application/x-ndjson`，连接保持打开，每行是一个完整 JSON 事件。每个事件包含递增 cursor、类型、时间、相关 Operation ID 和产生后的 Snapshot revision。事件只是提示客户端增量更新界面，Snapshot 仍是权威状态；连接中断时客户端可以从最后一个完整事件的 cursor 重新订阅。

Runtime 保留最近 10000 个事件。游标过期时返回 HTTP 410、`cursor_expired` 和当前最早连续游标，客户端重新读取 Snapshot 后使用 `latest_event_cursor` 再订阅，不能猜测丢失状态。为了保留结果未知或当前故障相关的审计记录，数据库中可能保留更早的离散事件；这些事件不会被当成可连续回放的历史。

## 修改串行化

Runtime 是机器状态的唯一写入者：

1. 允许多个客户端并行读取 Snapshot 和事件；
2. 同一时刻只执行一个修改 Operation；
3. 使用有界队列，满时返回 `busy`；
4. 长操作不依赖发起客户端保持连接；
5. 所有客户端观察同一 Operation；
6. 不采用跨客户端最后写入覆盖。

读取类 Action 不创建 Operation。可能改变来源、配置、核心、运行状态、网络或磁盘状态的行为必须创建 Operation。

## 版本兼容

路径中的主版本定义不兼容协议。`/v1` 内只允许增加可选响应字段和新的 Action kind，不能改变既有字段含义。

GUI、Runtime 和特权网络进程作为同一个产品版本安装。服务升级后，仍在运行的旧 GUI 必须识别版本不匹配，提示重启自身，不能继续提交修改。Runtime 会对上传、创建运行操作和取消操作再次检查客户端产品版本；版本不一致时返回 `protocol_unsupported`。只读快照和候选配置预览仍可用于显示故障信息。CLI 与 TUI 位于 Runtime 二进制中，不会产生独立安装版本。

客户端发送未知 Action 或高于服务端能力的协议特性时，Runtime 返回明确的 `unsupported`，不能尽力猜测执行。

## 来源秘密与诊断包

读取来源原始 URL 使用专用接口：

```http
POST /v1/sources/reveal-url
Content-Type: application/json

{"source_id":"src_...","confirm":true}
```

请求必须包含明确确认。响应只返回所选来源的原始 URL，设置 `Cache-Control: no-store`；Runtime 审计调用者、客户端、请求 ID、时间和来源 ID，不记录 URL。

诊断包分为预览和生成两步：

```http
POST /v1/diagnostics/preview
POST /v1/diagnostics/create
```

默认内容包括脱敏后的 Snapshot、运行操作、事件和审计。`include_raw_config`、`include_full_logs`、`include_network_info` 分别选择原始配置、完整日志和本机网络信息；选择任一敏感项时，生成请求还必须设置 `confirm_sensitive`。客户端先调用预览接口显示文件名、大小和敏感标记，再调用生成接口。Runtime 只把 ZIP 写入本机状态目录下的固定诊断目录，返回文件名、大小和 SHA-256，不接受上传目标，也没有上传接口。

### 便携备份与整体恢复

备份和恢复使用以下接口：

```http
POST /v1/backups/preview
POST /v1/backups/export
POST /v1/backups/restore/preview
POST /v1/imports
POST /v1/operations
```

Runtime 产品更新使用以下接口：

```http
POST /v1/imports
POST /v1/product/updates/preview
POST /v1/operations
```

在线预览提交 `source: "online_tuf"` 和可选的精确稳定版本，只刷新固定仓库的 TUF 元数据。离线预览先以 `application/vnd.submux.runtime-product-update+zip` 上传有大小和 SHA-256 约束的完整 TUF 包，再提交 `source: "offline_tuf"` 与返回的 `content_id`。两条路径使用同一内置 Root、最高已接受元数据版本和目标校验器。预览返回发行说明、数据库与 IPC 兼容范围、磁盘空间、网络中断说明、计划有效期和是否已持有目标字节；预览本身不安装。计划绑定调用者 OS 身份，消费一次后失效。

预览请求用 `include_secrets` 选择完整明文备份或脱敏清单。脱敏清单不含来源地址、凭据、配置正文、私钥资源或最近可用配置，因此不能恢复。导出还必须设置 `confirm_plaintext: true`；响应使用 `application/vnd.submux.runtime-backup+zip` 字节流，携带大小、SHA-256、创建时间和是否可恢复等元数据，并设置 `Cache-Control: no-store`。客户端负责把字节写入仅当前所有者可访问的新文件，已有文件不能覆盖；输出路径不会进入 IPC。

恢复前，客户端用备份媒体类型把字节上传到 `/v1/imports`，再把返回的 `content_id` 交给恢复预览。预览显示来源、托管资源、高级覆盖、近期配置和需要重新确认的机器设置。执行时创建 `backup.restore` Operation，并要求 `confirm: true`。Runtime 先在固定备份目录创建当前完整状态的 owner-only 自动备份，再整体替换可迁移状态和近期配置目录；不合并来源、资源或原配置目录。备份中的 `current` 在重新构建安全的本机候选时转为 `previous-good`，备份中的原 `previous-good` 保留在历史目录。Mihomo 和网络接管先停止，监听、TUN、网关和网络权限保持待确认，不能跨机器自动启用。恢复后的候选配置校验失败时，Runtime 重新应用替换前的可迁移状态和配置目录，并保留自动备份供人工恢复。

## 秘密读取

来源 URL 和凭据是写入后默认隐藏的值。普通 Snapshot、事件和 Operation 只包含脱敏形式。

显式秘密读取使用专用 Action，并要求客户端再次确认：

```text
source.reveal_url
backup.export_plaintext
diagnostics.include_sensitive
```

CLI 必须使用 `--reveal` 或同等明确参数。响应设置禁止缓存标记，GUI 不把结果保存在前端持久存储。审计只记录谁执行了读取和读取对象，不记录秘密内容。

脱敏在 Runtime 的结构化字段层完成，客户端不能自行用字符串替换补救。至少把以下值视为秘密：Authorization、Cookie、代理用户名和密码、URL userinfo、query 与 fragment 中的 token、Mihomo secret、证书私钥、配置正文和本机完整文件路径。错误链、重定向地址和命令输出进入日志前使用同一规则。测试夹具必须覆盖 IPv4、带方括号的 IPv6、百分号编码 URL、重复 query key 和嵌套错误。

## GUI 桥接

Tauri WebView 不能访问 Runtime Socket、Named Pipe 或 Mihomo。调用顺序固定为：

```text
HTML/TypeScript
  -> Tauri command
  -> Rust 本机 IPC 客户端
  -> Submux Runtime
```

Rust 层只负责传输、版本协商和 OS 错误转换，不复制 Runtime 的业务校验。TUI 和 CLI 使用同一个 Go IPC 客户端模块。

## 特权网络进程的内部 IPC

`submux-runtime-net` 使用另一条仅供 Runtime 调用的本机 IPC：

- Linux/macOS 客户端只允许 Runtime 服务账户；root 是特权进程的服务端身份，不能作为客户端调用；
- Windows 客户端只允许 Runtime 服务 SID；LocalSystem 是特权进程的服务端身份，不能作为客户端调用；
- GUI、TUI、CLI 和其他操作员不能直接连接；
- 消息只包含版本化的固定网络操作和经过 Runtime 验证的对象 ID；
- 不接受配置来源地址、凭据、任意路径、命令、脚本、argv、防火墙片段或服务名；
- 每个操作都绑定 Runtime 生成的网络所有权记录，清理只能撤销 Runtime 创建且所有权匹配的状态。

特权进程每次启动生成新的随机 epoch。Runtime 建立内部连接时校验服务端进程身份，双方为该连接协商随机 nonce；每条修改消息携带 epoch、连接 nonce、单调序号、Operation ID 和短截止时间。特权进程持久化已经提交的序号与结果，拒绝旧 epoch、重复序号、过期消息和 Operation ID 不匹配的重放。重新连接只能查询已有结果或用新序号提交新的明确操作，不能重放旧字节流。

允许的概念性操作包括准备或删除 TUN、应用或撤销 Runtime 路由、应用或恢复 DNS、启停 Linux 网关规则，以及平台需要时启动经过验证的 Mihomo。每个平台的实际操作见 [RUNTIME-NETWORK.md](RUNTIME-NETWORK.md)。
