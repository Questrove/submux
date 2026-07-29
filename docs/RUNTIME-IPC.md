# Submux Runtime 本机 IPC

本文定义 GUI、TUI、CLI 与 Submux Runtime 之间的唯一管理接口。当前已经实现 Unix Socket、Windows Named Pipe、对端身份校验和只读 Snapshot；修改操作、事件流和 GUI 桥接仍按本文继续开发。

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
- 指定受支持的协议版本和客户端版本；
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
- Mihomo 安装版本、运行状态和崩溃恢复状态；
- 当前运行方式及脱敏的网络设置；
- 配置来源摘要、当前来源和最近刷新结果；
- 当前或排队中的运行操作；
- 产品与核心更新状态；
- 最新事件游标。

普通 Snapshot 不包含完整来源地址、凭据、配置正文、Mihomo secret 或可直接调用的特权进程信息。

### 上传导入内容

```http
POST /v1/imports
```

本机配置副本、托管资源、备份和离线包由客户端打开后把字节流上传给 Runtime，不能把客户端文件路径交给 Runtime。请求声明内容类型、大小和客户端计算的 SHA-256；Runtime 在硬上限内重新计算摘要，保存到固定暂存区并返回短期 `content_id`。

`content_id` 绑定 Runtime 安装实例和上传者 OS 身份，默认三十分钟过期，只能被一次后续 Action 消费。暂存内容不能执行、不能被 Mihomo 引用，也不能传给特权进程；Action 完成、失败或过期后清理。运行中导入的离线更新包只有通过 Runtime 校验并变成受信任 bundle ID 后，平台安装器才能接收该 ID。

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

响应是持续的 JSON 事件流。每个事件包含递增 cursor、类型、时间、相关 Operation ID 和产生后的 Snapshot revision。事件只是提示客户端增量更新界面，Snapshot 仍是权威状态。

Runtime 保留最近 10000 个事件。游标过期时返回 `cursor_expired` 和当前最早游标，客户端重新读取 Snapshot 后再订阅，不能猜测丢失状态。

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

GUI、Runtime 和特权网络进程作为同一个产品版本安装。服务升级后，仍在运行的旧 GUI 必须识别版本不匹配，提示重启自身，不能继续提交修改。CLI 与 TUI 位于 Runtime 二进制中，不会产生独立安装版本。

客户端发送未知 Action 或高于服务端能力的协议特性时，Runtime 返回明确的 `unsupported`，不能尽力猜测执行。

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

- Linux/macOS 只允许 Runtime 服务账户和 root；
- Windows 只允许 Runtime 服务 SID 与 LocalSystem；
- GUI、TUI、CLI 和其他操作员不能直接连接；
- 消息只包含版本化的固定网络操作和经过 Runtime 验证的对象 ID；
- 不接受配置来源地址、凭据、任意路径、命令、脚本、argv、防火墙片段或服务名；
- 每个操作都绑定 Runtime 生成的网络所有权记录，清理只能撤销 Runtime 创建且所有权匹配的状态。

特权进程每次启动生成新的随机 epoch。Runtime 建立内部连接时校验服务端进程身份，双方为该连接协商随机 nonce；每条修改消息携带 epoch、连接 nonce、单调序号、Operation ID 和短截止时间。特权进程持久化已经提交的序号与结果，拒绝旧 epoch、重复序号、过期消息和 Operation ID 不匹配的重放。重新连接只能查询已有结果或用新序号提交新的明确操作，不能重放旧字节流。

允许的概念性操作包括准备或删除 TUN、应用或撤销 Runtime 路由、应用或恢复 DNS、启停 Linux 网关规则，以及平台需要时启动经过验证的 Mihomo。每个平台的实际操作见 [RUNTIME-NETWORK.md](RUNTIME-NETWORK.md)。
