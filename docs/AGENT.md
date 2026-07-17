# submux-agent 运行面设计与实现

> 状态：P0–P5 的仓库实现与 `v0.2.0` 正式 Release 已完成（2026-07-17）。本文同时保留产品决策、威胁模型和验收基线；干净 Linux/Windows 主机验证仍是发布后验收门禁，不改变 submux v4 的编排语义。

## 实现状态

| 阶段 | 已交付 |
|---|---|
| P0 | 可复现多平台 Release、SHA-256 清单、控制面事务型安装/升级/回滚/卸载、Sidecar 模板、runtime contract 与风险提示 |
| P1 | v1 严格协议、一次性配对、Ed25519 设备认证、HTTPS/WSS 出站通道、desired generation、持久 Job、部署/集成/审计数据闭环和运行实例 Web 控制台 |
| P2 | Linux root Agent、低权限 Mihomo systemd 服务、官方精确版本与摘要校验、staging/last-good/rejected revision、本地 Unix Socket 与恢复 CLI |
| P3 | 配置、规则、内存、策略组、单节点延迟、显式选择、连接、流量与日志的按需中继；限速、脱敏与选择恢复 |
| P4 | 当前终端/代理子 Shell，以及 Linux Docker daemon 的预览、确认、语义合并、冲突检测、验证、停用和恢复 |
| P5 | Windows 服务、命名管道 ACL、低权限 Mihomo SCM 服务、真实配置端口冲突保护、PowerShell 安装器和 Docker Desktop Business Settings Management 适配器 |

仓库测试覆盖协议拒绝、幂等对账、Job 去重与 `outcome_unknown`、配置/核心失败回滚、选择保持、Docker 冲突恢复、审计链、安装器生命周期及平台交叉编译。容器构建/运行环境代理、rootless Docker、macOS 和通用文件编辑仍明确不在 v1 范围内。

## 背景与结论

submux 当前负责把机场和手工节点整理成节点库，再用不可变模板版本生成 Mihomo / sing-box 完整配置。服务器还存在另一类需求：安装和更新代理内核、部署配置、查看运行状态，以及为终端、Docker 等明确指定代理。

这些能力不应直接放进现有 `submux` 进程。最终采用控制面与运行面分离的架构：

```text
浏览器
  │ HTTPS
  ▼
submux（控制面）
  ├─ 来源、节点库、模板、输出订阅
  ├─ 实例、期望状态、任务与审计
  └─ 已发布的不可变配置产物
          ▲
          │ Agent 主动建立的加密连接
          ▼
submux-agent（运行面，每台主机一个）
  ├─ Mihomo 安装、升级、回滚与服务管理
  ├─ 配置校验、原子部署与 last-good
  ├─ 延迟、选择、流量、连接和日志
  └─ 终端、Docker 等显式代理集成
          │ 回环地址上的受保护 API
          ▼
       Mihomo
```

同一仓库构建两个独立二进制：

```text
cmd/submux/
cmd/submux-agent/
internal/agentproto/
internal/hostops/
internal/mihomo/
internal/integration/
```

submux 继续可以单独使用；只有需要管理本机代理运行时的主机才安装 Agent。

## 设计目标

1. 从一个 Submux 控制台管理本机和多台服务器上的 Mihomo。
2. 安全安装、更新、固定版本和回滚 Mihomo；稳定版为默认渠道，Alpha 必须显式启用。
3. 把某个 Mihomo 输出订阅绑定到运行实例，并在配置变化后校验、原子部署和失败回滚。
4. 展示 Mihomo 版本、服务状态、策略组、节点延迟、流量、连接与日志，并只在用户明确操作时切换节点。
5. 内置服务器模板默认不使用透明代理，通过终端环境变量、Docker daemon 等可审计适配器满足服务器下载需求；自定义模板仍可使用 Mihomo 的其他能力。
6. 所有系统修改都支持预览、确认、验证、停用和恢复，不留下无法解释的配置漂移。
7. Agent 不需要开放入站管理端口，适合 NAT 后的本机和服务器。

## 非目标

- Agent 不主动生成或修改 TUN、REDIRECT、TPROXY、路由表和防火墙配置，也不因远程部署自动提升 Mihomo 的系统权限；用户在自定义模板中明确配置的 Mihomo 能力不由 Agent 做产品级禁止。
- Agent 不解析机场订阅，不复制 submux 的来源、节点库和编译器逻辑。
- Agent v1 不管理 sing-box 运行时；submux 仍可正常生成 sing-box 输出订阅。
- 不提供任意远程 Shell、脚本执行或通用文件编辑接口。
- 不允许通过 Mihomo API 临时修改规则后把运行状态当成配置来源。
- 不承诺自动代理所有程序；只有被明确启用的终端或应用集成会使用代理。
- v1 不接管 Clash Verge 等桌面客户端已经启动的内核，避免两个管理器争用进程、端口和配置。连接外部内核可作为后续独立模式设计。

## 组件职责

| 组件 | 拥有的数据与职责 | 明确不负责 |
|---|---|---|
| `submux` | 来源、节点、模板、输出订阅、实例期望状态、任务、部署记录和审计 | 启动内核、修改主机系统文件 |
| `submux-agent` | 作为主机级 root 信任组件管理主机能力、Mihomo 进程、本地 current/last-good、运行状态、固定系统操作和集成适配器 | 拉取和解析机场订阅、生成策略、保存通用配置版本历史 |
| Mihomo | 实际代理、策略匹配、节点选择、实时流量/连接/日志 | 持久化产品配置意图 |

配置策略始终由模板拥有。规则、DNS、代理组或节点选择范围发生持久变更时，必须发布模板版本或修改输出订阅，再由 Agent 部署新产物。Mihomo 运行 API 只用于观测、显式节点选择、连接操作和受控重载。

## 运行实例与本机 Agent

“运行实例”表示一台已注册的 Agent 主机，不等同于输出订阅。一个输出订阅可以部署给多个实例；一个实例在 v1 同一时刻只绑定一个 Mihomo 输出订阅。

### 同机模式

submux 和 Agent 在同一台机器时：

- Linux 使用 `/run/submux-agent/agent.sock`；
- Windows 使用 `\\.\pipe\submux-agent`；
- macOS 使用受权限保护的 Unix Socket；
- submux 通过本地 IPC 发现 Agent，管理员仍需在控制台确认一次，不进行无提示接管；
- Socket 或命名管道只允许 Agent 服务账户、submux 服务账户和本机管理员访问。

控制台把它显示为“本机”，但权限模型与远程实例相同。

### 远程模式

submux 位于服务器而 Agent 位于其他主机时，Agent 主动连接控制面：

1. 管理员在“运行实例 → 添加实例”生成一次性配对码。
2. 主机执行 `submux-agent enroll --server https://submux.example.com` 并输入配对码。
3. Agent 在本机生成设备密钥，私钥永不离开主机。
4. 控制面校验短时配对码，记录设备公钥并签发受限设备身份。
5. Agent 通过 HTTPS/WSS 建立出站连接，使用随机挑战签名证明设备身份。

配对码只保存摘要、只能使用一次并在短时间后失效。反向代理只需转发 HTTPS 和 WebSocket，不需要向 Agent 主机开放端口。实例可以在控制台撤销；撤销后连接、产物下载和任务领取立即失效。

这里的设备身份是应用层身份，避免要求常见 HTTPS 反向代理透传客户端证书；未来可以额外支持 mTLS，但不作为 v1 部署前提。

## 控制入口

### Web 控制台

新增一级导航“运行实例”，实例详情包含：

- 概览：在线状态、主机系统、Agent/Mihomo 版本、运行时间和最近错误；
- 配置：绑定的输出订阅、远端 revision、已应用 revision、可用更新、自动更新开关、检查间隔和本机 last-good；
- 代理：策略组、当前节点、逐节点延迟测试和手动选择；
- 运行：实时流量、连接、内存和日志；
- 集成：终端、Docker daemon、Docker 构建/容器等适配器；
- 操作：安装、启动、停止、重启、升级、固定版本和 Mihomo 核心回滚；
- 审计：哪个操作入口在何时触发了什么请求、目标实例、结果和 revision 变更摘要。

浏览器不直接连接 Mihomo。所有操作先经过 submux 鉴权：安装版本、运行状态和集成等持久目标更新实例期望状态；手动更新配置、重启、测速、选择节点、关闭连接和诊断等瞬时操作生成类型化一次性任务。配置自动更新由 Agent 按绑定的内部订阅端点和检查间隔执行。Agent 只对账期望状态、检查配置订阅或执行协议允许的一次性任务，再调用本机 API。

### 本地 CLI

CLI 用于首次注册、当前终端代理和控制面不可用时的恢复：

```text
submux-agent enroll --server https://submux.example.com
submux-agent status
submux-agent doctor
submux-agent logs

submux-agent mihomo install
submux-agent mihomo status
submux-agent mihomo restart
submux-agent mihomo rollback

submux-agent subscription status
submux-agent subscription check
submux-agent subscription update

submux-agent proxy env bash
submux-agent proxy env powershell
submux-agent proxy shell
submux-agent proxy docker status
submux-agent proxy docker enable --preview
submux-agent proxy docker disable
```

本地 CLI 只通过 Socket/命名管道访问 Agent，不复用远程管理 API。Agent 服务本身以 root 运行，安装该服务即表示本机管理员授予它执行固定主机操作的权限，不对每次操作重复触发 `sudo`。本地和远程入口最终调用同一组类型化内部操作；本地 Socket 还必须校验对端身份。任何入口都不能传入任意命令、argv、绝对路径、下载 URL 或 systemd unit 名称。

## 期望状态与任务模型

控制面保存“希望实例变成什么样”，Agent 定期上报“主机现在是什么样”。连接中断后恢复时按 generation 和 revision 对账，而不是盲目重放历史操作。

### 核心状态

| 对象 | 关键状态 |
|---|---|
| Instance | `enrolling`、`online`、`offline`、`degraded`、`revoked` |
| MihomoRuntime | `not_installed`、`stopped`、`starting`、`running`、`failed` |
| Deployment | `pending`、`validating`、`applying`、`active`、`rolled_back`、`failed` |
| Job | `queued`、`accepted`、`running`、`succeeded`、`failed`、`outcome_unknown`、`expired`、`cancelled` |
| Integration | `disabled`、`previewed`、`applying`、`active`、`conflict`、`failed` |

心跳上报 Agent 版本、OS/架构、能力列表、Mihomo 版本/状态、订阅 remote/applied/rejected revision、更新时间、代理监听状态和集成摘要。默认不上传完整系统配置、订阅正文、环境变量或长期日志。

### 期望状态对账

以下持久目标不作为一次性任务重放：

```text
desired_core_installed
desired_core_version
desired_runtime_state = running | stopped
desired_integrations
```

安装、升级和核心回滚都表现为准确 `desired_core_version` 的变化；启动和停止表现为 `desired_runtime_state` 的变化；集成确认后更新对应适配器的 desired config revision。每次期望状态修改增加 generation，Agent 串行对账并上报 observed generation。重复对账同一 generation 必须幂等，Agent 已达到目标时只上报状态，不重复执行系统操作。

配置更新不进入 desired state。Agent 像订阅客户端一样检查绑定的 submux 内部配置订阅，只关心远端当前 revision 与本机 applied revision；不补放中间 revision，也不请求历史配置。控制面解析稳定版、Alpha、版本约束或核心回滚选择后，必须向 Agent 下发准确核心版本，Agent 不自行猜测“最新版”。

### 一次性类型化任务

只保留不能自然表达为持久目标的瞬时操作，任务类型由协议版本固定，例如：

```text
update_subscription
restart_core
test_proxy_delay
select_proxy
close_connection
collect_diagnostics
```

每个任务包含唯一 ID、协议版本、目标实例、参数 schema、截止时间、`actor_type`、请求关联 ID 和审计原因。Agent 必须拒绝未知任务类型、未知字段、尚未开始便已过期的任务和能力不匹配任务。协议中永远不存在 `exec`、`shell`、任意 argv、任意文件路径、任意下载 URL 或任意服务名称字段；连接 ID、策略组和节点名称等业务标识只能映射到 Agent 当前观测到的对象。

Agent 在执行一次性任务前，必须把 Job ID 和 `running` 状态持久写入本地有界任务日志，执行结束后再持久记录结构化结果。重复收到同一 Job ID 时只返回已保存状态或结果，不重新执行。如果 Agent 在外部副作用完成与结果落盘之间崩溃，重启后把该任务标记为 `outcome_unknown`，不得自动重放；用户需要显式创建新的 Job。`update_subscription` 始终获取并尝试应用远端当前 revision，不接受任意 URL 或历史 revision。观察性低风险任务也使用新的 Job ID 重试，不能把旧任务静默变回 `queued`。

v1 每台 Agent 使用一个主机修改队列。期望状态对账、订阅配置更新以及重启、节点选择、关闭连接等会改变运行状态的任务串行执行；状态读取、订阅检查、日志/流量流和不改变状态的查询可以并发。截止时间只阻止尚未开始的任务；任务进入 `running` 后不以超时为由假装撤销已经发生的外部副作用。

任务结果只返回结构化状态和经过脱敏的错误，不返回订阅 URL、token、Mihomo API secret 或完整敏感文件。

## Mihomo 生命周期管理

### 安装与升级

- Agent 根据内置的 `MetaCubeX/mihomo` 官方仓库、准确版本和 OS/架构自行确定 Release 产物，不接受控制面传入下载 URL 或摘要；
- 默认跟踪正式 Release，Alpha 作为显式的预发布渠道；
- 下载后验证官方发布资产摘要，并检查解压后二进制报告的版本与目标版本一致，再原子替换二进制；
- 保留当前和上一个可运行版本，升级失败自动恢复；
- 支持固定准确版本，自动升级不得跨越管理员设置的约束；
- v1 只安装 Agent 自己管理的 Mihomo，不覆盖未知来源的现有二进制。

Mihomo 官方同时发布正式版本和 `Prerelease-Alpha`，因此 UI 必须把渠道与准确版本分开显示，不能把 Alpha 伪装成“最新版”。发布来源见 [MetaCubeX/mihomo Releases](https://github.com/MetaCubeX/mihomo/releases)。

v1 将 GitHub HTTPS、`MetaCubeX/mihomo` 官方仓库及其 Release 发布权限视为 Mihomo 二进制的信任根。SHA-256 用于检测下载损坏、资产错配和内容不一致，不承诺抵御官方仓库、发布账号或 Release 工作流本身失陷。控制面只下发准确版本；稳定版、Alpha、版本约束和显式回滚由控制面解析，Agent 只从内置官方来源取得对应资产。独立签名、构建 provenance 和固定发布公钥可在威胁模型需要时后续增加，不作为 v1 前提。

### 服务运行

Linux 使用独立的 `mihomo.service`。内置服务器模板不启用 TUN、透明端口或低位端口，因此默认 unit 不申请 `CAP_NET_ADMIN` 等网络管理能力。自定义配置可以使用其他 Mihomo 能力，但如果当前服务权限不足，Agent 只报告能力缺口，不通过远程部署自动提权；额外权限必须由本机管理员显式预览和授予。Mihomo 官方给出了 systemd 运行、重载和日志查询方式，Agent 应生成更小权限的专用 unit，而不是照搬包含大量 capability 的通用示例：[Mihomo systemd 文档](https://wiki.metacubex.one/en/startup/service/)。

内置“Mihomo 服务器 Sidecar”模板的默认运行约束：

- mixed 代理端口只监听 `127.0.0.1`；
- Agent 注入的外部控制 API 只监听回环地址；
- API secret 由 Agent 在本机生成，不进入公开输出订阅；
- Agent 不向浏览器或控制面回传 API secret；
- 工作目录、配置目录和日志目录使用独立服务账户及最小权限。

### Agent 兼容配置

输出订阅仍是不可变基础产物。部署时 Agent 只允许应用一个可展示差异的“本地管理覆盖”，字段限定为：

- `external-controller: 127.0.0.1:<本地端口>`；
- `secret: <本机随机密钥>`；

这两个字段由 Agent 拥有；基础 artifact 中已有同名字段时，effective config 使用 Agent 的值，并在部署差异中明确展示。覆盖后的 effective config 与基础 artifact 分别记录 SHA-256。Agent 不得借此修改节点、策略组、规则、DNS、TUN、代理监听地址、其他控制端点、CORS 或外部 UI。自定义模板中的非回环监听、透明代理和其他控制端点只产生风险提示，不作为配置合法性的产品级否决条件。

模板版本需要增加显式 `runtime_contract=mihomo-agent/v1`。该契约只定义 Agent 接管所需的最小机制：

- 引擎为 Mihomo；
- 配置能通过目标 Mihomo 版本的原生校验；
- 模板所有者接受 Agent 覆盖 `external-controller` 和 `secret`；
- 策略组若需要在控制台切换节点，应提供可显式选择的 `select` 组；
- 通过模板正常校验后才能绑定 Agent。

平台应新增推荐的“Mihomo 服务器 Sidecar”内置模板。现有 Mihomo 桌面 TUN、网关和其他自定义模板可以绑定 Agent，但 UI 必须展示监听范围、透明代理、额外控制端点和所需主机权限等风险或能力提示；提示不代替 Mihomo 原生校验，也不静默修改配置。

### 配置部署

```text
Agent 使用设备身份检查绑定的内部订阅端点
  -> ETag/revision 未变化：结束
  -> 有新 revision 且自动更新关闭：只上报 update_available
  -> 自动更新开启或用户显式 update_subscription
  -> 下载远端当前 artifact 并校验 SHA-256/revision
  -> 生成限定字段的本地管理覆盖
  -> 写入本机 staging 目录
  -> 用目标 Mihomo 版本校验候选配置
  -> 校验失败：删除 staging，current 不变，记录 rejected revision
  -> 校验成功：把当前 current（已验证配置）保存为本机 last-good
  -> 原子切换 current 到候选配置
  -> 重载/重启 Mihomo
  -> 调用 /version、/configs 和代理端口进行运行验证
  -> 成功：标记 active
  -> 失败：恢复本机 last-good、重新启动并验证，上报 degraded
```

控制面为每个 RuntimeBinding 提供稳定的设备认证内部订阅端点，例如 `/api/agent/bindings/{binding-id}/artifact`。Agent 保存控制面身份和 binding ID，不使用也不保存公开 `/sub/{token}`，不接受远程任务传入任意配置 URL。端点支持 `HEAD` 或条件请求，使用 `ETag`/revision 表示当前完整 Mihomo artifact；WSS 通知只用于提示 Agent 立即检查，定时轮询是通知丢失时的可靠兜底。

自动更新默认关闭。关闭时 Agent 定期检查并上报 `remote_revision` 与 `update_available`，只有 Web 或本地 CLI 显式触发 `update_subscription` 才下载并应用远端当前配置；开启时发现新的远端 revision 后自动更新。自动更新只跟随已绑定输出订阅实际发布的新 artifact，不替用户修改输出订阅绑定、模板版本或 Mihomo 发布渠道。

控制面只保存输出订阅当前成功生成的 artifact；新 revision 成功生成后可以覆盖旧 artifact，不提供按历史 revision 下载或重新部署的版本库。Agent 只获取远端当前 revision，不补放中间 revision。部署记录只保存 revision、hash、状态和脱敏结果，不保存历史配置正文或承诺生成完整历史差异。

Agent 本机只维护 `current`、`last-good` 和临时 `staging`。候选配置在 staging 阶段失败时 current 完全不动；候选通过静态校验后，当前已验证配置才成为新的 last-good，然后原子切换 current。切换后的运行验证失败时只恢复本机 last-good，不向控制面请求旧配置。相同 `rejected_revision` 不自动重复尝试；只有远端出现新 revision 或用户显式重试才再次更新。首次安装没有 last-good 且运行验证失败时，Agent 停止 Mihomo、保留诊断状态并上报 degraded，不循环重启。

last-good 只用于当前更新失败时自动恢复，以及控制面不可用时的本机紧急恢复，不构成通用配置版本历史；Agent 重装或本地状态丢失后，旧 last-good 无需由控制面恢复。配置一般优先重载；涉及监听端口或服务参数时允许重启。每次更新都记录远端 revision、effective hash、Mihomo 版本、验证结果和本机恢复目标。

## Mihomo 面板能力

Mihomo 官方 API 提供版本、配置、日志、流量、连接、策略组、代理选择和延迟测试等接口，Agent 只封装所需子集：[Mihomo API](https://wiki.metacubex.one/en/api/)。

### 延迟与节点选择

- 延迟测试必须由用户发起，首版不做后台持续测速或稳定性评分；
- 使用单节点 `/proxies/{name}/delay`，避免组级测试的额外状态语义；
- 测试结果只更新观测值，不修改当前节点；
- 只有 `select_proxy` 明确任务才能切换 `select` 策略组；
- UI 不根据最低延迟自动选择，也不在刷新页面时触发测试；
- 部署新配置后尽量按节点确定性名称恢复旧选择，节点不存在时使用模板默认项并明确提示。

Mihomo 官方说明组级 delay 操作可能清除自动组的固定选择，因此 Agent 不把组级健康检查当成无副作用的测速接口。平台内置 Agent 模板使用手动 `select` 组，不创建会自动切换的 `url-test` 组。

### 规则修改

控制台可以展示当前规则和匹配信息，但持久修改遵循：

```text
编辑模板草稿 -> 发布新模板版本 -> 输出订阅显式升级 -> Agent 部署新 revision
```

不把 Mihomo `/rules/disable` 等临时运行状态反向写成模板，避免重启后丢失或产生双重事实来源。

### 实时数据

日志、流量和连接使用按需流式转发：没有浏览器订阅时 Agent 不持续向控制面上传。默认只保留短期聚合状态；长期日志和流量历史不进入 v1。连接关闭属于显式任务并进入审计。

## 系统代理集成

每个适配器统一执行：

```text
检测 -> 预览差异 -> 用户确认 -> 备份 -> 应用 -> 验证
                                         └失败->恢复备份
```

停用时只撤销 Agent 拥有的字段。如果目标文件在应用后被外部修改，Agent 标记 `conflict` 并要求人工确认，不使用旧备份覆盖用户的新配置。

### 终端代理

默认只影响当前新建终端：

```bash
eval "$(submux-agent proxy env bash)"
```

```powershell
submux-agent proxy env powershell | Invoke-Expression
```

输出设置 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY` 和 `NO_PROXY`，值指向本机 Mihomo 代理端口。命令不能改变已经打开的其他 SSH/PowerShell 会话。`proxy shell` 可以启动一个带代理变量的子 Shell；写入用户 profile 的持久模式以后单独实现并要求预览。

### Docker daemon

Linux Docker Engine 适配器对 `/etc/docker/daemon.json` 做语义合并，配置 daemon 自身访问镜像仓库时使用 Mihomo。它必须：

- 保留文件中的其他 Docker 设置；
- 默认把 localhost、回环和常用私网加入 `no-proxy`；
- 检测 rootful/rootless Docker 并采用对应路径；
- 在确认页明确说明重启 Docker 会影响正在执行的操作；
- 应用后重载/重启 daemon，并用配置状态和一次显式测试验证；
- 停用时恢复此前的 `proxies` 值，而不是删除用户原有代理。

Docker 官方推荐在 `daemon.json` 配置 daemon 代理，修改后需要重启；Docker Desktop 会忽略这里的代理项，因此 Windows/macOS Desktop 必须作为不同适配器处理：[Docker daemon proxy](https://docs.docker.com/engine/daemon/proxy/)。

### Docker 构建与容器

这是与 daemon 拉取镜像不同的适配器。它可按用户修改 `~/.docker/config.json` 的 `proxies`，只影响之后创建的构建和容器，不影响 Docker Engine 本身或已经运行的容器。官方边界见 [Docker CLI proxy](https://docs.docker.com/engine/cli/proxy/)。

容器中的 `127.0.0.1` 指向容器自身，不是宿主机，因此该适配器绝不能把回环代理地址直接写进容器环境。实现前必须为目标 Docker 平台确定并验证容器可达的宿主机地址，同时解决监听范围、访问控制和防火墙问题；无法安全建立该通道时应明确标为不支持。首版只需解决 Docker daemon 拉取镜像，构建/容器代理可以晚于 daemon 适配器交付。

### 后续适配器

Git、APT/DNF 和指定 systemd 服务可以复用相同生命周期，但每一种都需要独立 schema、冲突检测和恢复实现；不能提供“填写文件路径和内容”的通用适配器。

## 权限与安全边界

### 主机级 root Agent

Linux v1 使用一个以 root 运行的 `submux-agent` 系统服务，不再拆分独立特权 helper。需要 root 是因为 Agent 必须安装和替换 Mihomo、管理固定的 systemd unit、更新已注册的系统集成文件并恢复备份。Mihomo 本身仍由独立低权限服务账户运行，其网络代理和核心运行面不继承 Agent 的 root 身份。

```text
submux-agent（root，主机级受信任组件）
  ├─ agentproto：出站连接、类型化任务、状态与审计
  ├─ hostops：固定 root 操作，不接受通用命令或路径
  ├─ integration：每种系统集成的独立 schema 与生命周期
  └─ mihomo client：调用回环控制 API
            │
            ▼
mihomo.service（独立低权限账户）
```

`internal/hostops` 是进程内权限边界和审计边界。它只提供安装已验证二进制、管理固定的 `mihomo.service`、通过已注册适配器更新固定文件和恢复已知备份等类型化操作。目标路径、下载来源和 systemd unit 由代码或适配器内置，不能由远程或本地请求传入。保持该接口独立可以让未来在威胁模型变化时拆出 helper，而不改变 Agent 协议。

单进程方案明确接受以下信任假设：控制面被攻破时，攻击者仍只能调用协议允许的固定操作；如果 `submux-agent` 进程本身出现可利用的代码执行漏洞，则视为主机 root 已失守。Windows 对应服务同样是主机级受信任组件，并使用明确的命名管道 ACL；具体服务账户与权限在 Windows 阶段单独确定。

### 单管理员与审计归因

v1 继续使用当前单管理员模型，不新增用户、角色或实例级 RBAC。所有通过管理员密码建立的有效会话都拥有全部未撤销实例的管理权限；系统只校验会话有效、目标实例存在且未撤销，不声称能够区分共享管理员密码背后的具体个人。

审计使用固定 `actor_type` 标识操作入口：

```text
admin_session
local_cli
agent_reconcile
system_scheduler
```

每次期望状态修改和一次性任务都生成请求关联 ID。后续 Agent 接受、执行、回报和恢复事件沿用该 ID，使审计能够关联一次操作的完整链路。审计记录时间、actor type、请求 ID、目标实例、动作、revision、结果和脱敏摘要；它回答“哪个入口触发了什么”，不回答“具体哪位自然人操作”。将来若引入多用户，再为会话增加 subject/user ID、session ID、角色和实例权限，不改变现有 actor type 与请求关联结构。

### 必须满足的安全规则

1. Agent 自身的管理入口和 Agent 注入的 Mihomo 控制 API 不监听 `0.0.0.0`；模板明确配置的其他监听由模板所有者负责，并在部署前提示风险。
2. 所有远程期望状态修改和一次性任务先经过 submux 管理员会话鉴权，并确认目标实例存在且未撤销。
3. Agent 只暴露固定、类型化的主机操作；不存在任意命令、argv、文件路径、下载 URL 或服务名称参数。
4. 设备私钥、Mihomo secret、订阅 URL/token 不写日志、不进入任务结果。
5. 二进制只从 Agent 内置的官方 Release 来源下载并验证发布摘要，artifact 校验目标 revision/hash；这些摘要用于检测损坏、错配和内容不一致，版本切换采用原子替换。
6. 系统文件路径与 unit 名称由内置适配器固定；备份设置严格权限、数量与保留期上限，并防止符号链接、路径穿越和检查/写入竞态。
7. 参数采用严格 schema；未知字段失败，不做宽松向前兼容。
8. 持久目标按 generation 幂等对账；订阅按 ETag/revision 更新且相同 rejected revision 不自动重试；一次性任务在 Agent 本地持久记录，结果不确定时标记 `outcome_unknown` 且不自动重放。
9. 控制面被攻破时，攻击者可以调用已授权的固定操作，但不能通过协议获得通用远程 Shell；Agent 进程本身被攻破则等同于主机 root 失守。
10. 本机管理员始终可以用 CLI 停止 Agent、撤销注册和回滚 last-good。

## 数据模型扩展

控制面新增以下对象；字段名在实现前可调整，但职责不得合并：

| 模型 | 关键字段 |
|---|---|
| RuntimeInstance | ID、名称、设备公钥、OS/架构、Agent 版本、能力、状态、last-seen、revoked-at |
| RuntimeBinding | 实例 ID、输出订阅 ID、runtime contract、内部端点标识、自动更新、检查间隔 |
| RuntimeDesiredState | 实例 ID、generation、核心安装目标、渠道/版本约束、准确 desired core version、desired runtime state |
| AgentJob | ID、类型、schema 版本、参数、状态、actor type、请求关联 ID、截止时间、结构化结果 |
| Deployment | artifact/effective hash、Mihomo 版本、previous revision、验证结果、状态、时间 |
| RuntimeObservation | remote/applied/rejected revision、update available、last-check/update、核心状态、当前选择、端口状态、最近错误、观测时间 |
| IntegrationState | 类型、作用域、desired/observed 状态、原文件 hash、备份引用、冲突与验证结果 |
| AuditEvent | actor type、请求关联 ID、实例、动作、对象 revision、结果和脱敏摘要 |

实时日志、连接明细和流量流不写入 bbolt；只在浏览器订阅期间中继。设备公钥可以入库，设备私钥与 Mihomo secret 只能保存在 Agent 主机。

## 平台范围

### 第一阶段：Linux

- systemd 管理 Agent 与 Mihomo；
- amd64、arm64；
- 本地 Unix Socket；
- 终端代理；
- rootful Docker Engine daemon 代理；
- 原生二进制安装，不要求 Docker 才能管理 Docker 的代理。

### 第二阶段：Windows

- Agent 作为 Windows 服务；
- 本地命名管道与 PowerShell 环境输出；
- Agent 自管 Mihomo 进程；
- 明确检测 Clash Verge/其他 Mihomo 的端口冲突并拒绝接管；
- Docker Desktop 依据其受支持配置方式单独设计，不能复用 Linux `daemon.json` 假设。

### 后续

macOS、rootless Docker、已有内核 attach 模式和其他显式应用适配器按独立里程碑推进。

## 安装与发布

`submux` 与 `submux-agent` 分别发布、分别安装：

```text
submux install                 # 控制面
submux-agent install           # 某台需要运行代理的主机
```

### 一键安装脚本审计与发布结果

当前实现见 [`scripts/install.sh`](../scripts/install.sh)。实现前的 GitHub `latest` 是 2026-06-28 发布的 [`v0.1.0`](https://github.com/Questrove/submux/releases/tag/v0.1.0)，其中只有旧版 `submux` 二进制，因此当时不能继续把 README 的一键安装命令当作当前产品的可用安装方式。

2026-07-17 已发布新的 [`v0.2.0`](https://github.com/Questrove/submux/releases/tag/v0.2.0) 正式版本并成为 `latest`。Release 包含控制面 Linux/Windows/macOS 5 个资产、Agent Linux/Windows 4 个资产及 `checksums.txt`；发布流水线成功校验两个 Linux amd64 二进制的版本输出，9 个二进制的 GitHub SHA-256 摘要也已与清单逐项一致。README 已恢复固定 `v0.2.0` 的安装命令。下表保留实现前发现的问题和修复依据：

| 优先级 | 实现前问题 | 影响 | 必须采取的修复 |
|---|---|---|---|
| 阻断 | `latest=v0.1.0`，内容早于当前 v4 | 新用户安装到已经废弃的产品模型 | 先发布包含当前 v4 的新正式版本并验证资产，再恢复 README 一键安装入口 |
| 阻断 | 下载二进制后不验证 SHA-256 | 下载损坏、资产错配或内容不一致时仍会安装并执行 | Release 生成 checksum manifest；脚本下载并验证目标资产摘要，失败时禁止替换 |
| 高 | 覆盖二进制后只执行 `systemctl enable --now submux` | 服务已在运行时不会加载新二进制，磁盘版本与实际进程不一致 | 新安装执行 enable/start；升级完成后显式 restart，并验证进程版本与健康状态 |
| 高 | systemd 分支无条件使用 `sudo` | root-only 精简系统没有 `sudo` 时安装失败 | 增加统一 `run_as_root`，root 直接执行，非 root 才检测并使用 sudo；缺少权限时给出明确错误 |
| 高 | 没有保留旧二进制和失败恢复 | 新版本无法启动时服务直接不可用 | 替换前保存上一版本；健康检查失败时恢复二进制、unit 和 last-good，然后重新启动 |
| 中 | `INSTALL_DIR` 不存在时不创建，并可能误判为需要 sudo | 自定义目录和首次安装容易失败 | 先解析并创建目录，再分别检查目录和目标文件权限 |
| 中 | 临时文件没有统一清理 trap，且在 `/tmp` 下载后直接移动到安装目录 | 失败残留文件；跨文件系统替换不具备原子性 | 注册 EXIT trap；校验后复制到安装目录内的 staging 文件，再在同一文件系统原子 rename |
| 中 | 参数只判断第一个参数是否等于 `--service`，未知参数被忽略 | 错误输入可能执行非预期安装 | 使用完整参数解析，拒绝未知参数，提供 `--help`、`--version`、`--channel` 和非交互选项 |
| 中 | 通过 `grep/head/cut` 解析 GitHub API JSON | API 格式、限流或错误正文可能造成模糊失败 | 优先使用稳定 Release 下载规则或严格 JSON 解析，并显示 HTTP/限流错误；显式版本仍可离线传入 |
| 中 | unit 描述仍是 `subscription aggregator`，缺少最小权限加固 | 与当前产品定位不符，服务安全边界不清晰 | 更新描述；设置专用账户、工作目录、UMask、写入路径和经过测试的 systemd hardening |
| 中 | 安装完成只提示启动地址，没有版本/健康校验 | 脚本成功不等于服务可用 | 输出安装版本，检查 systemd active 状态和本机健康端点；失败返回非零状态 |
| 后续 | 当前脚本只认识 `submux`，没有 Agent 资产与命令边界 | 引入 Agent 后容易把两个不同权限的服务混装 | 保持两个资产和两个 unit；允许分别安装控制面、Agent 或显式选择组合安装 |
| 后续 | 缺少卸载、升级策略和渠道语义 | 用户难以恢复，也可能误跟 Alpha | 增加 uninstall/upgrade/rollback；stable 默认，Alpha 必须显式选择并在执行前提示 |

仓库实现已修复表中所有代码项：控制面和 Agent 使用独立安装器与 unit，完整解析参数，固定准确版本和渠道，校验 Release `checksums.txt`，在安装目录内 staging 后原子替换，保留上一二进制，并在服务/健康验证失败时恢复；安装器生命周期测试覆盖校验失败、升级、回滚、恶意版本、卸载和状态保留。正式 Release 与线上资产校验已经完成；剩余外部门禁是在干净 Linux amd64/arm64 和 Windows 主机上的发行验收。README 继续要求显式 `--version`，避免安装行为随 `latest` 漂移。

submux 安装链路采用相同威胁模型：v1 信任 GitHub HTTPS、`Questrove/submux` 仓库及其 Release 发布权限。由同一 Release 工作流生成并发布的 checksum manifest 用于检测下载损坏、资产错配和内容不一致，不声称能够抵御仓库管理员账号、工作流或 Release 发布权限失陷。独立签名和构建 provenance 属于后续安全增强，不阻塞当前发布修复。

### 修复顺序

安装链路按下面顺序修复，避免“脚本改好了但仍下载旧程序”：

1. [x] 建立当前 v4 的可复现构建和 Release 工作流，生成资产、摘要及版本信息。
2. [x] 修复 `scripts/install.sh` 的权限处理、参数解析、校验、原子替换、服务重启、健康检查和回滚。
3. [ ] 在干净的 Linux amd64/arm64 环境分别测试首次安装、重复安装、升级、失败回滚、root、sudo 和自定义目录。
4. [x] 发布 `v0.2.0` 正式 Release，并确认 `releases/latest`、资产名称、摘要和实际二进制版本一致。
5. [x] 恢复 README 中固定准确版本的安装命令。
6. [x] 扩展同一发布流水线生成独立 Agent 资产，且不让高权限 Agent 安装逻辑隐式附带在控制面安装中。

修复完成的最低验收命令应覆盖：

```text
install.sh --version <stable-version>
install.sh --service
install.sh --upgrade
install.sh --rollback
install.sh --uninstall
```

重复执行安装必须幂等；升级后 `submux --version`、systemd 进程和 Release 版本必须一致；模拟 checksum 错误、服务启动失败和健康检查失败时，脚本必须返回非零并恢复上一可运行版本。

一键脚本不能在没有新 Release 的情况下指向旧产品版本。正式提供 Agent 安装前还必须完成统一发布流水线：

- 为每个平台生成明确命名的 submux/Agent 资产和校验摘要；
- 安装脚本验证摘要、创建安装目录、使用临时文件清理 trap；
- root 环境不强依赖 `sudo`；
- 升级二进制后正确重启已经运行的服务；
- systemd unit 使用准确描述、专用账户和最小权限；
- 支持 `--version`、`--channel`、卸载和回滚，不静默跟随 Alpha。

## 分阶段实施计划

### P0：发布基础修复

- 修复现有安装脚本和 stale latest Release 问题；
- 建立多平台构建、摘要、Release 与升级测试；
- 新增“Mihomo 服务器 Sidecar”模板、最小 runtime contract 校验和非阻断风险分析。

### P1：Agent 协议与实例管理

- 新增实例、绑定、任务、部署与审计模型；
- 完成一次性配对、设备身份、心跳、撤销和断线重连；
- 完成设备认证内部订阅端点、ETag/revision 条件请求、WSS 更新提示与定时检查兜底；
- 控制台实现“运行实例”基础页面；
- 固化单管理员 actor type、请求关联 ID 和完整操作链路审计，不承诺逐人归因；
- 固化协议版本、严格 schema、desired/observed generation 对账、一次性任务持久日志、`outcome_unknown` 和超时语义；
- 每台 Agent 实现单一主机修改队列，验证升级、部署、启停和集成不会并发互相覆盖。

### P2：Linux Agent 与安全部署

- 构建主机级 root `submux-agent`、本地 IPC、固定类型化 `hostops` 和 systemd 安全加固；
- 安装/固定/升级/回滚 Mihomo 正式版；
- 完成订阅检查、当前 artifact 下载、受限本地覆盖、staging 校验、原子部署、本机 last-good 和 rejected revision 抑制；
- 实现 status、doctor、logs 与本地恢复 CLI。

### P3：Mihomo 运行面板

- 接入版本、配置、策略组、逐节点延迟、显式选择、流量、连接和日志；
- 确保测速不切换节点；
- 增加按需流式转发、限速、断线和脱敏测试。

### P4：显式系统集成

- 当前终端环境和代理子 Shell；
- Linux Docker Engine daemon 代理；
- 在宿主机可达性与访问控制方案验证后，再提供 Docker 构建/容器用户配置；
- 对每个适配器完成预览、冲突检测、备份、验证、停用和回滚测试。

### P5：Windows 本机 Agent

- Windows 服务、命名管道、PowerShell CLI；
- Agent 自管 Mihomo 与端口冲突保护；
- 评估并实现 Docker Desktop 专用适配器。

每个阶段都应单独可发布。P1/P2 完成前，控制台不能展示无法实际执行的开关；P4 不能以通用远程文件编辑代替独立适配器。

## 验收标准

1. 不安装 Agent 时，现有来源、节点库、模板和输出订阅功能完全不变。
2. Agent 主机不开放入站管理端口，Agent 注入的 Mihomo API 只监听回环地址；内置服务器模板的代理入口默认只监听回环地址，自定义模板保持用户明确配置的监听行为。
3. Agent 能通过设备认证内部订阅端点检查并更新当前 artifact；有效配置部署后 applied revision 与 remote revision 一致，无效配置不改变 current，并在切换后验证失败时只恢复本机 last-good。
4. Mihomo 升级或配置重载失败后，服务能恢复到已验证版本并在控制台显示 degraded 原因。
5. 延迟测试前后手动选择保持不变，只有显式选择操作能更改节点。
6. Docker daemon 集成能保留用户原有 JSON 字段，预览准确，停用后恢复原值；外部修改时不会被旧备份覆盖。
7. 重复下发同一 desired generation 不产生额外系统操作；重复 Job ID 只返回已有状态，执行结果不确定的任务进入 `outcome_unknown` 且不自动重放；过期、撤销、未知和 schema 不匹配任务均被拒绝。
8. 审计能按 actor type 和请求关联 ID 串联操作链路，但不宣称识别共享管理员密码背后的具体个人；日志和审计中不出现机场 URL/token、设备私钥、Mihomo secret 或完整订阅正文。
9. 本机 CLI 在控制面不可用时仍能查看状态、停止服务并回滚 last-good。

## 已确定的产品决策

- 控制面与 Agent 分进程，但在同一个仓库和同一个 Web 产品中维护。
- Agent 是可选运行面，不把 submux 变成代理内核。
- 首版运行时只支持 Mihomo，稳定 Release 默认，Alpha 显式选择。
- Agent 不主动生成 TUN 或主机透明代理配置，也不因远程部署自动提权；内置服务器模板通过显式应用集成完成代理，自定义模板可以使用 Mihomo 的其他能力。
- Agent 不重新解析机场，只消费 submux 生成的完整配置。
- Agent 像订阅客户端一样通过设备认证内部端点检查和更新绑定的当前完整配置；自动更新关闭时只提示，开启或用户显式操作时才应用。
- 持久规则修改走模板版本，运行 API 不成为第二配置源。
- 测延迟不自动选节点；节点切换必须是用户明确动作。
- 远程控制只允许期望状态对账和类型化一次性任务，不提供任意命令执行。
- Linux 优先，Windows 本机 Agent 在协议稳定后实现。
