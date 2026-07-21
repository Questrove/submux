# submux-agent 用户态运行面设计

> 状态：本设计取代 `v0.2.0` 的主机级 root Agent 与自动代理集成。新实现的安全边界是：Agent 以安装它的普通用户运行，只管理自己的数据目录和 Mihomo 子进程；其他软件的代理配置由 Web 页面说明，用户自行确认和执行。

## 背景与结论

submux 负责来源、节点库、模板和不可变输出订阅。可选的 submux-agent 负责在一台主机上保存自己的 Mihomo 配置订阅、运行用户态 Mihomo，并把不含凭据的状态回报给控制面。运行实例既可以直接使用平台已经发布的 Mihomo 输出订阅，也可以使用独立的外部 HTTPS 订阅。

旧设计让 Agent 以 root/LocalSystem 运行，以便创建系统账户、system service，并直接修改 Docker Engine 或 Docker Desktop 配置。即使这些操作具有固定 schema、预览和回滚，它们仍然扩大了控制面被攻破后的主机影响范围，也会和用户已有的配置管理工具争夺所有权。

新设计不再自动修改任何其他程序的配置：

```text
浏览器
  │ HTTPS
  ▼
submux 控制面
  ├─ 来源、节点、模板与输出订阅
  ├─ Agent 配对、实际状态、一次性任务与审计
  ├─ 订阅地址的一次性内存转交，不持久化地址
  └─ “代理设置指南”：只生成步骤和可复制命令
          ▲
          │ Agent 主动建立 HTTPS/WSS 出站连接
          ▼
submux-agent（普通用户）
  ├─ 当前用户目录中的设备身份、核心与多个配置订阅
  ├─ 当前用户权限的 Mihomo 子进程
  ├─ 配置校验、原子部署、last-good 与运行观测
  └─ 不写 Docker、Git、包管理器、系统代理或其他服务配置
          │ 回环地址上的受保护 API
          ▼
       Mihomo
```

## 设计目标

1. Agent 在 Linux 和 Windows 上均不要求 root、sudo、Administrator 或 LocalSystem。
2. Agent 只修改当前用户拥有的固定数据目录，并只启动自己下载且校验过的 Mihomo。
3. 保留精确版本安装、升级、回滚、配置校验、原子部署、last-good、运行状态和观测能力。
4. Agent 不开放入站管理端口；设备身份仍通过 HTTPS/WSS 出站连接证明。
5. 通过独立 Web 页面说明常见软件如何使用代理，提供可检查、可复制、可撤销的命令或图形界面步骤。
6. 旧版 `desired_integrations`、Docker preview stream 和本机 Docker 写 API 已删除。

## 非目标

- Agent 不修改 Docker Engine `daemon.json`、systemd drop-in、Docker Desktop 设置、`~/.docker/config.json`。
- Agent 不执行 `git config`、`npm config`、`pip config`，不编辑 APT/DNF 配置，也不修改 Windows 系统代理。
- Agent 不提供任意 Shell、argv、文件路径、通用下载 URL、服务名或“代用户执行指南命令”的接口。唯一可由用户提供的下载地址是 HTTPS Mihomo 配置订阅，且只能通过专用的一次性字段交给 Agent。
- Agent 不配置 TUN、REDIRECT、TPROXY、路由表或防火墙。
- Agent 不接管用户已启动的 Clash Verge、Mihomo 或其他代理程序。
- Agent 不保证退出登录后继续运行；这是用户会话管理策略，Linux lingering 等能力必须由主机管理员独立决定。
- v1 仍不管理 sing-box 运行时。

## 权限与所有权边界

### Agent 可以拥有

- 设备私钥与本地 bbolt 数据库；
- Agent 下载并验证的 Mihomo 当前/上一版本；
- Agent 本机保存的多个平台或外部 HTTPS 配置订阅及其已下载配置；
- 当前使用订阅经过验证的 current/last-good 配置；
- Mihomo 工作目录、短期日志和本地 IPC；
- Agent 自己的 systemd user unit 或当前用户启动项。

### Agent 不可以拥有

- `/etc`、`/usr`、`/var/lib`、`Program Files`、`ProgramData` 等系统级目录；
- root/LocalSystem 服务、系统用户、系统级 Docker unit；
- 其他程序的用户配置，即使文件位于当前用户目录；
- 用户 Shell profile、注册表系统代理或通用环境变量持久化位置。

控制面被攻破时，攻击者最多能调用协议允许的用户态 Mihomo 管理动作；不能借 Agent 写入其他程序的启动配置或系统文件。Agent 进程漏洞仍等同于当前用户会话失守，但不自动等同于整台主机 root 失守。

## 默认路径

### Linux

遵循 XDG 目录；未设置时使用：

```text
~/.local/state/submux-agent/agent.db
~/.local/share/submux-agent/mihomo-core/
~/.local/share/submux-agent/mihomo-config/
~/.local/share/submux-agent/mihomo-runtime/
${XDG_RUNTIME_DIR}/submux-agent/agent.sock
```

如果没有 `XDG_RUNTIME_DIR`，Socket 放到 `~/.local/state/submux-agent/run/agent.sock`。目录拒绝符号链接祖先，设备身份和状态目录权限为当前用户专用。

### Windows

```text
%LOCALAPPDATA%\submux-agent\agent.db
%LOCALAPPDATA%\submux-agent\mihomo-core\
%LOCALAPPDATA%\submux-agent\mihomo-config\
%LOCALAPPDATA%\submux-agent\mihomo-runtime\
\\.\pipe\submux-agent
```

命名管道 DACL 在运行时绑定当前用户 SID，不再绑定 LocalSystem 与 Administrators。

## Mihomo 生命周期

Agent 仍从代码内置的 `MetaCubeX/mihomo` 官方 Release 来源解析准确资产：

1. 控制面只下发准确版本和 stable/alpha 渠道，不传下载 URL。
2. 每个实例可以选择直连，或填写一个 HTTP/SOCKS5 代理供 Agent 下载 Release 元数据和文件。代理地址不能包含账号、密码、路径、查询参数或 fragment；它不作用于 Agent 与控制面的 HTTPS/WSS 连接。
3. Agent 下载资产并验证 Release SHA-256，执行 `mihomo -v` 核对准确版本。
4. staging、current、previous 和 failed 都位于 Agent 用户数据目录，在同一文件系统原子切换。
5. Mihomo 作为 Agent 的同权限子进程运行；Agent 正常退出时停止子进程，Linux 使用 parent-death signal、Windows 使用 `KILL_ON_JOB_CLOSE` Job Object，防止 Agent 异常退出后遗留进程。显式启动或重启成功后，Agent 在本地记录下次启动时恢复 Mihomo；显式停止或卸载成功后清除该记录。恢复时立即尝试一次，失败后按 2、4、8、16 秒退避，最多尝试五次；每次仍执行完整运行验证，全部失败后保持停止并上报错误。
6. 升级时先完成下载、摘要校验、解压和版本核对，再停止当前子进程并切换已验证目录；失败则恢复。
7. Agent 只向自己生成的配置注入回环 `external-controller` 和本机 secret。

Mihomo 不再注册 systemd system unit 或 Windows SCM 服务。它也不拥有 `CAP_NET_ADMIN`，因此默认服务器模板不得依赖 TUN、透明代理或低位端口。

内置 `Mihomo Linux 服务器` 模板只在 `127.0.0.1:7890` 提供 mixed 代理，关闭 LAN 暴露、TUN、透明代理和进程匹配。应用是否使用该代理由用户按“代理设置指南”自行配置；Agent 不修改应用或系统配置。

## 配置订阅、部署与运行观测

每个 Agent 在本机维护最多 32 个配置订阅。浏览器可以为当前实例添加、修改、刷新、切换和删除订阅。添加时可以选择平台中已启用、已发布的 Mihomo 输出订阅，也可以填写返回完整 Mihomo YAML 的外部 HTTPS 地址；不解析 Base64 分享链接列表。外部地址的默认下载器直接连接公网，拒绝回环、私网、链路本地和 CGNAT 目标，避免把远程控制任务变成内网探测接口。

外部订阅 URL 可能包含 token，因此控制面不能把它写进 Job、审计或数据库。浏览器先把 URL 放入控制面内存中的短时一次性槽位，持久化 Job 只记录随机 `secret_ref`。只有绑定该实例设备私钥的 Agent 可以读取，而且读取一次后立即删除；未读取的值十分钟后过期。平台订阅的 Job 只记录平台订阅 ID，Agent 通过设备认证接口读取当前已发布产物；该接口只接受具备订阅管理能力的设备，且只返回已启用、未过期、未阻断的 Mihomo 产物。Agent 把来源标识和下载到的配置保存在权限为当前用户专用的本地 bbolt 数据库中。心跳只上报订阅 ID、名称、来源说明、平台订阅 ID、revision、更新时间、流量、到期时间、错误和当前使用标记，不包含外部 URL 或配置正文。

Agent：

- 添加订阅时先下载并检查 YAML 顶层结构，但不自动切换 Mihomo；
- 切换订阅时校验 SHA-256 和目标 Mihomo 原生配置；应用前若 Mihomo 已停止，临时激活验证后恢复停止，只有原本运行的实例才保持运行；
- 刷新非当前订阅时只更新本地缓存；刷新当前订阅时下载、校验并应用新配置；
- 修改当前订阅地址时下载并应用新配置，失败则保持或恢复 last-good；
- 在用户目录 staging 后切换 current；
- 失败时保持或恢复 last-good；
- 通过回环控制 API 读取策略组、延迟、连接、流量、内存、配置、规则和 Mihomo 日志；
- 在浏览器按需请求时转发经过脱敏的 Agent 本地日志；
- 除 Agent 启动时按本地记录恢复 Mihomo 外，只在显式任务中测速、选择节点、关闭连接或重启核心。

浏览器不直接连接 Mihomo，API secret 不离开 Agent 主机。

运行实例页面采用任务工作区：左侧切换实例，右侧分为概览、代理、连接、日志、配置和活动。页面顶部固定显示两张实际状态卡片。Agent 卡片显示在线状态、版本、最后心跳、运行时长和平台；Mihomo 卡片显示安装/运行状态、核心版本、代理监听端口、已应用配置 revision 和当前 Agent 资源代理。所有内容均来自最近一次心跳，不根据页面表单推断，也不展示控制面的期望值。

安装、升级、卸载、启动、停止、重启、回滚、订阅管理和修改 Agent 资源代理均为一次性任务。提交后，页面立即显示“等待 Agent 接收”，随后按 Agent 心跳展示订阅下载、Release 解析、下载字节数、摘要校验、解压、版本核对、配置部署、启动和运行验证等阶段。Mihomo 进程启动后，Agent 会在限定时间内轮询控制接口和代理端口，避免把正常初始化期间的首次连接拒绝误判为启动失败。任务结束后保留成功结果或经过脱敏的失败原因；单纯保存表单不能改变 Agent 或 Mihomo。成功的启动、停止、重启和卸载任务只在 Agent 本地更新下次启动是否恢复 Mihomo，不在控制面保存运行期望。控制面不允许同一实例并行执行两个未结束任务。

浏览器为当前实例建立独立状态 WebSocket；它只负责通知浏览器重新读取实例详情，任务领取和状态上报仍通过签名 HTTPS 请求完成。WebSocket 中断不会让 Agent 重复任务，也不会把页面编辑内容当成实际状态。

概览进入时只在 Mihomo 正在运行时订阅流量、内存、连接和策略组。日志页默认订阅 Agent 日志，也允许切换到 Mihomo 日志；两者都只在用户打开日志页时建立临时流。配置与规则只在用户点击读取时获取。实时数字使用稳定宽度，策略组以可搜索卡片呈现，连接可按目标、进程和链路筛选；移动端保留相同能力并改为纵向布局。

## 协议收敛

控制面不再下发核心安装状态、核心版本、运行状态或 Agent 资源代理的期望值，也没有 generation。Agent 每次同步先观测本地 Mihomo，再领取尚未结束的一次性任务，执行后再次观测并发送心跳。没有任务时，日常同步过程不能安装、启停、升级、部署配置或改变 Agent 资源代理；唯一例外是 Agent 进程启动时可以根据本机保存的运行意图恢复 Mihomo，且不会持续对账或无限重试。

运行实例的订阅记录和配置正文保存在 Agent 本机，由固定结构的一次性任务管理。平台订阅通过只读、设备认证的已发布产物端点提供给 Agent；外部订阅仍通过一次性地址槽位传递。控制面和 Agent 不包含运行实例 binding、desired 或 `update_subscription` 任务。数据库升级会删除开发阶段遗留的 binding、desired、部署记录、集成状态和对应旧任务，不保留兼容读取路径。

### 一次性任务

```text
add_runtime_subscription(name, secret_ref | platform_subscription_id)
edit_runtime_subscription(id, name, optional_secret_ref | optional_platform_subscription_id)
delete_runtime_subscription(id)
refresh_runtime_subscription(id)
activate_runtime_subscription(id)
configure_resource_proxy(mode, url)
list_core_versions(channel)
install_core(channel, exact_version)
uninstall_core
start_core
stop_core
restart_core
rollback_core
test_proxy_delay
select_proxy
close_connection
```

每个任务都有唯一 Job ID、请求 ID、截止时间、固定参数结构和所需 capability。Agent 本地保存已领取任务，终态任务不会重放；进程在无法确认执行结果的时刻退出时，任务标记为 `outcome_unknown`，必须由用户观察实际状态后重新发起新的任务。运行中的操作心跳包含 Job ID、任务类型、阶段、已完成字节数、总字节数和脱敏错误，页面据此关联进度和结果。

`list_core_versions` 由 Agent 查询内置的 MetaCubeX/mihomo 官方 GitHub Releases，只返回适用于当前系统和架构且具备 SHA-256 摘要的资源版本。查询与核心下载共用 Agent 当前保存的资源代理；控制端和浏览器不直接请求 GitHub，也不能为任务指定其他仓库或下载地址。该代理只影响 Agent 下载自身所需的固定官方资源，不接管订阅、Mihomo 运行流量或其他程序的网络设置。

协议不存在 integration enable/disable 任务。Capabilities 不再声明：

```text
integration.docker_daemon
integration.docker_desktop
```

运行流只允许 `proxies`、`configs`、`rules`、`connections`、`traffic`、`memory`、`logs` 和 `agent_logs`；`docker_preview` 与 `docker_desktop_preview` 被拒绝。Agent 日志帧包含本次 Agent 进程的 `stream_id` 和单调递增的 `sequence`，页面在重新订阅本地日志尾部时据此忽略已经显示的帧。

## 本机 CLI

```text
submux-agent enroll --server https://submux.example.com
submux-agent serve
submux-agent status | doctor | logs
submux-agent mihomo install --version vX.Y.Z
submux-agent mihomo status | restart | rollback
submux-agent subscription status | rollback
submux-agent proxy env bash|powershell
submux-agent proxy shell
submux-agent unenroll [--force-local] [--yes]
```

`proxy env` 只打印环境变量文本，`proxy shell` 只为新建子 Shell 设置进程环境；两者都不写 Shell profile。Docker 与 Docker Desktop CLI 子命令已移除，旧路径返回 404 或 unknown command。

## 代理设置指南页面

Web 控制台新增一级页面“代理设置指南”。它不依赖 Agent，用户也可以为其他代理服务器填写连接信息。

### 输入

- 代理主机；
- HTTP/mixed 端口；
- SOCKS5 端口；
- no-proxy 列表。

主机名和端口先做严格前端校验，防止把任意文本插入可复制的 Shell 命令。页面必须明确提示：容器、虚拟机和远程设备中的 `127.0.0.1` 不是宿主机。

### 首批指南

| 软件/场景 | 交付形式 | 必须说明的恢复边界 |
|---|---|---|
| Bash/Zsh/PowerShell | 当前会话环境变量与清除命令 | 关闭会话自动失效 |
| curl/wget | 单次命令 | 不保存全局配置 |
| Git | `git config` 设置/查看/清除 | 不影响 SSH remote |
| APT | 单次 `-o` 与手工配置片段 | 独立文件由用户删除 |
| DNF/YUM | `--setopt` 与配置项 | 只编辑 `[main]` |
| npm/pnpm/Yarn | 工具自身 config set/delete | 只操作实际使用的工具 |
| pip | `--proxy` 与 config set/unset | 优先单次参数 |
| Docker Engine | systemd drop-in 步骤 | 重启影响和手工撤销 |
| Docker 构建/容器 | `~/.docker/config.json` 合并片段 | 不覆盖 auths；只影响新容器 |
| Docker Desktop | 图形界面步骤 | 不写 ProgramData/daemon.json |
| 指定 systemd 服务 | 手工 drop-in 片段 | 只重启明确 unit |
| Windows 系统/WinHTTP | 图形步骤与显式 netsh 命令 | 两套代理语义不同 |

每段命令都带复制按钮，复制后的提示要求用户再次检查主机、端口和作用范围。页面只在浏览器中生成文本，不向 Agent 或后端提交执行请求。

## 安装与自启动

### Linux

默认安装到 `~/.local/bin`。`--service` 只创建 `~/.config/systemd/user/submux-agent.service`，使用 `systemctl --user`，不调用 sudo。首次配对后启动用户 unit。Mihomo 不创建独立 unit；如果上次显式操作要求保持运行，由 Agent 启动后按有限退避策略恢复。

user unit 只启用当前 systemd user manager 可执行的加固项。`ProtectKernelModules` 与 `ProtectKernelLogs` 需要系统服务能力，在无特权 user unit 中会导致 `218/CAPABILITIES`，因此不得写入 Agent 的用户服务。配对完成后安装器等待本地 Socket 就绪，再报告成功。

只有 root 登录入口的服务器可使用 `scripts/bootstrap-agent.sh` 一键接入。引导脚本创建或复用 `submuxagent` 专用用户、启用 lingering，再以该用户调用普通安装器并完成配对；它不得创建 system unit、授予 sudo 权限或让 Agent/Mihomo 保留 root 身份。控制台的配对卡片生成准确版本、控制面地址和短时配对码组成的可复制命令。开发构建使用 root 所有且 SHA-256 固定的本地 bundle，禁止静默退回旧稳定版 Agent。

无人值守服务器若需要退出登录后继续运行，`loginctl enable-linger <user>` 是主机管理员的独立选择；安装器不得自动执行。

### Windows

默认安装到 `%LOCALAPPDATA%\Programs\Submux`。`-Service` 为向后兼容保留参数名，但只创建当前用户 Startup 项，不创建 Windows Service。配对后用户可以运行 `submux-agent serve`，或下次登录时由启动项运行。

## 从 v0.2.0 迁移

旧版 root Agent 不自动就地转成用户态，因为设备私钥、运行状态和文件所有权跨越了信任边界。迁移必须显式执行：

1. 记录旧实例使用的订阅地址和准确 Mihomo 版本，并撤销旧实例。
2. 停止并卸载旧 `submux-agent.service` 与 `mihomo.service`；确认没有需要恢复的旧 Docker 集成。
3. 旧版曾修改 Docker 配置时，由管理员根据旧备份和当前真实配置手工恢复，不能由新 Agent 猜测覆盖。
4. 使用普通专用用户安装新 Agent并生成新设备身份。
5. 创建新实例，依次执行安装核心、添加配置订阅、切换订阅和启动核心；每一步确认实际状态与任务结果后再继续。
6. 验证新进程、Socket、核心与配置路径均属于普通用户，且控制面 capabilities 不含 integration。
7. 观察稳定后再由管理员清理 `/var/lib/submux-agent`、旧 system unit 和旧系统账户。

迁移期间不得让旧 root Agent 与新用户 Agent同时管理同一监听端口。

## 安全规则

1. Agent 与注入的 Mihomo 控制 API 不监听 `0.0.0.0`。
2. 设备私钥、Mihomo secret、订阅 token 不写日志或任务结果。
3. 远程协议不接受任意命令、argv、文件路径、通用下载 URL 或服务名。Mihomo 核心下载目标仍固定为代码内置的官方 Release 坐标；外部配置订阅 URL 只允许 HTTPS，并通过实例绑定、短时、一次性且不落库的专用通道转交。平台订阅只允许读取平台已经发布的 Mihomo 产物。
4. 所有 Agent 管理路径固定在当前用户数据根下，拒绝符号链接/reparse link。
5. 二进制和 artifact 均校验摘要，版本切换和配置部署使用同文件系统原子替换。
6. Job ID 去重；终态任务不重放，`outcome_unknown` 不自动重放。没有任务时，日常心跳和同步只能观测状态；Agent 启动时按本地运行意图进行的有限次数恢复不重放 Job，也不形成持续对账。
7. Web 指南永远不调用执行 API；复制命令不等于执行授权。
8. 任何需要 sudo/管理员权限的指南步骤由用户在目标主机亲自执行，Agent 不代办。

## 验收标准

1. 普通 Linux/Windows 用户可以 enroll、serve、安装/运行/升级/回滚用户态 Mihomo。
2. Agent 运行时没有 root/Administrator 检查，不创建系统用户、system service 或 SCM service。
3. 仓库 Agent 路径不写 `/etc/docker`、Docker Desktop settings、Git/npm/pip/包管理器配置。
4. 旧 desired、binding 和 integrations 字段不存在；配置订阅任务可以选择平台已发布的 Mihomo 输出订阅或独立外部地址；没有任务时日常同步不改变 Mihomo，Agent 启动恢复只读取本地意图并最多尝试五次。
5. Capabilities 与 WebSocket stream allowlist 不包含任何 Docker integration。
6. Web 控制台有独立代理指南，覆盖首批常见软件，输入变化会同步生成命令，复制前后无后端副作用。
7. 容器回环地址、Docker daemon 与容器代理区别、systemd/Windows 权限影响均有明确提示。
8. 旧订阅、模板、实例观测和 Mihomo 运行控制不因移除集成写入能力而回归。
9. Linux installer 生命周期测试证明默认路径和 user unit 不需要 sudo；Windows 脚本不要求管理员。
10. `go test ./...`、`go vet ./...`、多平台交叉编译和安装器 smoke test 通过。
11. 网页可以通过一次性任务为单个实例设置直连或自定义 HTTP/SOCKS5 Agent 资源代理；它不影响控制面连接，Agent 在下载期间持续上报阶段和字节进度。
12. Agent 接入后，网页顶部无需手工刷新即可看到 Agent 与 Mihomo 的实际状态；任何配置修改都生成一次性任务，并持续显示等待、执行阶段、进度与最终结果。
13. 同一 Agent 可以保存、修改和切换多个平台或外部 HTTPS Mihomo 配置订阅；外部订阅 URL 和 Agent 下载的外部配置正文不出现在控制端数据库、任务结果、审计、心跳或日志中。平台配置正文仍只保存在原有发布产物和 Agent 本机状态中。

## 已确定的产品决策

- Agent 是当前用户信任组件，不是主机 root 信任组件。
- Agent 仍可管理自己下载的 Mihomo，但不能管理其他代理程序。
- 页面只展示 Agent 上报的实际状态，不保存或展示需要自动对账的核心期望状态。
- 所有远程修改行为都是一次性任务，任务结束后不会继续维持页面曾填写的值；核心启动、停止、重启和卸载成功后，只在 Agent 本地记录下次启动是否恢复 Mihomo。
- “如何配置应用代理”属于产品页面，“替用户修改应用配置”不属于 Agent 能力。
- 心跳协议不包含 integrations 字段，也没有修改其他应用配置的执行通道。
- Linux systemd user unit 与 Windows current-user startup 可以自动运行 Agent，但不扩大其 OS 权限。
- 更高权限的 TUN、透明代理、系统服务和 Docker daemon 修改必须由用户在 Agent 之外自行管理。
