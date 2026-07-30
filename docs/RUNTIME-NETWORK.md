# Submux Runtime 网络与权限设计

本文定义显式代理、TUN 和 Linux 网关三种运行方式，以及 `submux-runtime-net` 的权限边界。Linux 普通 TUN 已按本文实现。Linux 网关也已实现，并通过 network namespace 的双网卡、单臂和故障恢复测试，但在真实物理网关验收前仍标记为预览功能。Windows 和 macOS 普通 TUN 已实现预览后端；它们必须完成对应系统和架构的真实验收后才能移除预览标记。

## 共同规则

一台机器或一个网络命名空间只有一个 Submux Runtime、一个受管 Mihomo 和一种当前运行方式。TUN 或 Linux 网关运行时仍可保留一个本机 mixed 监听，但它只是辅助入口，不构成第二种运行方式。

共同约束如下：

1. GUI、TUI、CLI 和主 Runtime 永不以管理员权限运行。
2. 配置来源不能决定运行方式、监听范围、TUN、路由、DNS、防火墙或网关范围。
3. 特权网络进程只执行固定类型操作，不提供通用命令、脚本或文件写入。
4. 启用前记录系统原状态和 Runtime 创建的对象；停用时只撤销所有权匹配的对象。
5. 发现另一个全隧道程序正在拥有默认路由时，拒绝启动 TUN，不与其反复争夺路由。
6. 任一关键进程异常退出时执行故障放行，恢复直连。
7. 第一版不提供 kill switch。

网络修改必须先完成配置校验和 Mihomo 就绪检查。应用顺序遵循“先准备可用代理，再接管流量”；停用顺序遵循“先停止接管，再停止代理”。

## 显式代理

默认提供一个 mixed HTTP/SOCKS 监听：

```text
IPv4: 127.0.0.1:7890
IPv6: [::1]:7890
```

默认端口被占用时明确报错，不随机选择其他端口。操作员可以改用分离的 HTTP 和 SOCKS 端口。

监听到局域网必须满足全部条件：

- 操作员明确选择具体地址或接口；
- 必须启用认证；
- 使用 Runtime 生成的高强度凭据或用户明确设置的凭据；
- 预览中显示可访问网段和防火墙影响；
- 不允许无认证监听 `0.0.0.0` 或 `::`。

来源配置和本机高级覆盖不能创建额外监听，也不能修改认证。显式代理不修改默认路由或系统 DNS。Linux 和 Windows 的显式代理因此不需要特权网络进程；macOS 预览实现为了与 TUN 使用同一份官方 Mihomo 和固定执行对象，由 root helper 启动核心，但不会创建 TUN 或修改路由。

## 普通 TUN

默认 TUN 同时接管 IPv4、IPv6、TCP、UDP 以及 TCP/UDP 53。DoH、DoT 和 DoQ 作为普通 TUN 流量按 Mihomo 规则处理。

如果系统存在 IPv6 而 Runtime 无法建立完整 IPv6 接管，不得静默退化为 IPv4。操作员只能明确选择：

- IPv4 与 IPv6 均接管；
- IPv4 接管、IPv6 直连，并持续显示泄漏警告；
- IPv4 接管、IPv6 在 Runtime 运行期间阻断。

系统完全没有 IPv6 能力时不显示无意义警告。停用或故障时同时恢复原 IPv4、IPv6 和 DNS 状态。

### 局域网与其他隧道

默认保留系统已存在的直连网段和更具体路由，包括局域网、WireGuard、企业 VPN、虚拟机和容器网络。始终绕过：

- loopback；
- link-local；
- 广播；
- 组播。

不能把所有 RFC1918 地址无条件直连，因为远端企业网、重叠 VPN 和代理目标也可能使用私网地址。Runtime 展示发现的具体路由，操作员可以逐项选择继续绕过或纳入 TUN。

如果另一程序已经安装默认路由或全隧道策略，Runtime 拒绝启动并显示冲突所有者和相关路由。它不能定时覆盖对方设置。

## 平台权限实现

### Linux

`submux-runtime-net` 创建持久 TUN 设备，把设备所有权和必要访问权限授予低权限 Mihomo，并负责 Runtime 自有的路由、策略规则、DNS 与 nftables 对象。Mihomo 关闭自己的自动路由和自动重定向功能，不持有 `CAP_NET_ADMIN`。

每个网络对象带 Runtime 所有权标识或记录。已有对象与预期名称相同但所有权不匹配时拒绝覆盖。

### Windows

当前预览实现使用安装器预置的固定 `SubmuxRuntime` Wintun 设备。安装器必须由 LocalSystem 创建该设备，并将设备 ACL 限制为 Runtime 服务 SID，使 Mihomo 保持低权限；运行期间不接受客户端传入设备名、路径或任意命令。缺少预置设备、设备索引在预览后改变、存在其他全隧道路由，或者残留 Runtime 风格路由时，启用会被拒绝。

LocalSystem `submux-runtime-net` 通过独立的 `\\.\pipe\submux-runtime-net` 与低权限 Runtime 通信。Pipe DACL 只允许 LocalSystem 和 `NT SERVICE\SubmuxRuntime`，并由 `FILE_PIPE_REJECT_REMOTE_CLIENTS` 拒绝远程客户端。它使用 IP Helper API 创建和删除带实例派生 metric 的精确 IPv4/IPv6 路由；IPv6 阻断使用实例命名的 Windows 防火墙规则。清理只处理接口、前缀、下一跳、协议和 metric 全部匹配的对象，不删除预置 Wintun 设备或其他程序的路由。

DNS 劫持不改写 Windows 的系统 DNS 设置。预览通过 `GetAdaptersAddresses` 记录当前活动接口的 DNS 服务器，并为这些地址添加精确的 TUN 主机路由，确保局域网 DNS 不会因为原有具体路由而绕过；Mihomo 同时接管 TCP/UDP 53。由于 DNS 配置本身未被修改，停用时只需删除所有权匹配的主机路由，不存在覆盖用户后续 DNS 修改的问题。

Mihomo 使用 `auto-route: false`，Windows 配置不写入 Linux 专用的 `routing-mark`。启动后 Runtime 会检查控制 Pipe DACL；出现任何未授权允许项便拒绝声明核心就绪。该实现和界面状态保持 `preview_only`，必须通过 Windows amd64 与 arm64 的真实集成测试后才能作为稳定实现。预览标记表示发布成熟度，不会把明确执行的启用操作改成只读演练。

如果官方 Mihomo 与 Wintun 的实际限制使低权限进程无法可靠使用预创建设备，允许 LocalSystem 的 `submux-runtime-net` 按固定路径和摘要启动 Mihomo。它不能接受客户端路径或任意 argv；GUI、TUI、CLI 和主 Runtime 仍保持低权限。

需要由 LocalSystem 启动 Mihomo 时，低权限 Runtime 只能把候选文件写入暂存区。特权进程通过已打开的文件句柄读取并重新校验受信任元数据与摘要，再复制到只有 LocalSystem 可写的执行目录；启动时拒绝 reparse point，并从已经校验且锁定的文件句柄或等价安全句柄执行，避免校验后替换。

### macOS

当前预览不开发私有 Mihomo fork，也不开发 Network Extension。低权限 `_submux-runtime` LaunchDaemon 提供管理 Socket、状态和配置；独立的 root `submux-runtime-net` LaunchDaemon 只接受固定的核心、配置和数据对象 ID。管理 Socket 允许 Runtime 身份、root 和 `submux-runtime-operators`，不会因为用户属于 macOS `admin` 组而自动授权。管理目录由 Runtime 服务账户拥有、操作员组只可进入而不可写；内部 Socket 位于独立的 `/var/run/submux-runtime-privileged/runtime-net.sock`，其目录和 Socket 只向 root 与 Runtime 服务组开放。

主 Runtime 通过协议发送对象 ID 和 SHA-256，不发送核心路径、配置路径、来源 URL、操作员凭据、argv 或命令。root helper 只从 `/Library/Application Support/SubmuxRuntime` 下的固定对象位置安全打开文件，并逐级拒绝符号链接。核心和配置重新计算摘要后复制到 `/Library/Application Support/SubmuxRuntimePrivileged/core`；该目录及文件只有 root 可写。任何摘要变化、目录替换、链接、过大的对象或非固定数据对象都会使启动失败。

root helper 使用固定参数从摘要命名的只读对象启动官方 Mihomo。macOS 配置不固定 `device`，由 Mihomo 创建一个新的 `utunN`；helper 将启动前后的 utun 集合对比，只有恰好出现一个新接口才继续。Mihomo 的 `auto-route` 和 `auto-redirect` 都关闭，helper 再按已确认的快照添加两条 IPv4 `/1` 路由、按策略添加 IPv6 `/1`、DNS 服务器主机路由和操作员选中的具体路由。选中已有具体路由时先保存完整网关与接口，停用时只在 Runtime 路由仍精确匹配且目的前缀没有被第三方重新占用时恢复原路由。

预览不改写 SystemConfiguration DNS。DNS 劫持通过原 DNS 服务器的精确 utun 主机路由和 Mihomo 的 TCP/UDP 53 接管完成，清理时只删除这些精确路由。IPv6 可以代理、直连并显示泄漏警告，或在运行期间用 blackhole 路由阻断。

root helper 正常停止、租约过期或 Runtime 请求故障放行时，先撤销路由，再停止 Mihomo。helper 被 `SIGKILL`、系统睡眠唤醒、网络切换、并发创建多个 utun 以及卸载中断仍需要真实 macOS 验收；当前状态和所有接口都持续返回 `preview_only`。CI 在 macOS runner 上执行单元测试，并交叉构建 amd64 与 arm64，但这不代替 macOS 13 及以上 Intel 和 Apple Silicon 的 IPv4、IPv6、TCP、UDP、DNS、冲突、崩溃、更新和卸载测试。

## Mihomo 控制连接

Runtime 在 Linux 和 macOS 使用 Mihomo 的 `external-controller-unix`，在 Windows 使用 `external-controller-pipe`，并把 TCP `external-controller` 置空。Unix Socket 和 Windows Named Pipe 不依赖 Mihomo secret 鉴权，因此操作系统权限就是安全边界。

每次启动后、声明核心就绪前必须验证：

- Unix Socket 位于仅 Runtime 和特权进程可进入的固定目录，所有者和 mode 符合预期；
- Windows Pipe 的 DACL 只允许 Runtime 服务 SID、启动 Mihomo 的特权身份和 LocalSystem，不允许普通操作员直接连接；
- Pipe 不能被远程客户端访问；
- 端点名称和父目录不能由配置来源或本机高级覆盖修改；
- 已存在同名端点但所有者或 ACL 不匹配时拒绝启动，不能连接或覆盖。

Windows 和 macOS 的特权启动路径必须在真实系统中证明低权限 Runtime 可以连接，而其他操作员和未授权用户不能连接；未通过时该平台的 TUN 不能作为稳定功能发布。

## Linux 网关

Linux 网关用于内网设备把本机设为默认网关的场景。第一版只代理 IPv4，支持 TCP 和 UDP；UDP 默认启用，可以按机器关闭。

优先采用 TUN 加策略路由。只有真实 Linux 测试证明转发流量无法可靠进入 TUN 时，才允许在特权网络进程内部改用 TProxy。TProxy 是实现细节，不能成为配置来源或 IPC 的可编辑防火墙接口。

### 默认接管范围

默认覆盖所有实际把本机作为 IPv4 网关的内网设备，不要求逐台登记：

- 自动识别默认 WAN；
- 自动识别直接连接的内网和客户端网段；
- DHCP 新客户端无需重新配置 Runtime；
- 支持双网卡和单臂网关；
- WAN 进入的新连接不接管；
- loopback、link-local、广播和组播不接管；
- Docker、虚拟机和 VPN 网段在界面中列出，可逐项排除；
- 高级设置可以改为显式 CIDR allowlist。

发现结果必须显示具体接口、CIDR、路由来源和是否接管，不能只显示“局域网已开启”。

### TCP、UDP 与例外

TCP 默认接管。UDP 默认接管，但操作员可以关闭整个机器的普通 UDP 接管。即使 UDP 已开启，也可以按以下维度添加直连例外：

- 来源主机或 CIDR；
- 目标主机或 CIDR；
- 目标端口或端口范围；
- 上述条件的组合。

这些例外用于内网游戏服务、语音、QUIC 和其他不应经过代理的流量。例外必须在最终网络预览中展开显示。

关闭普通 UDP 不等于关闭 DNS 接管。DNS 是独立设置。

### DNS

网关默认接管内网设备发往 IPv4 TCP/UDP 53 的 DNS：

- DNS 接管可以单独关闭；
- 普通 UDP 关闭时，DNS UDP 53 仍可保持；
- 可以为内网 DNS 服务器设置直连例外；
- Runtime 和 Mihomo 自己的上游 DNS 流量必须绕过接管，防止递归；
- DoH、DoT、DoQ 作为普通流量处理；
- WAN DNAT 进入的 DNS 流量不接管。

### 对外服务与现有 NAT

Runtime 不接管从 WAN 进入的新连接，也不修改用户已有的 DNAT 或端口转发规则。已建立连接和相关回包始终绕过代理接管，保证对外提供的 TCP/UDP 服务按原路返回。

所有规则放在 Runtime 自己的 nftables 表、chain 和策略路由范围内。停用与卸载不清空系统或其他软件的规则。

### 网关主机自身流量

网关主机自己发起的流量默认直连。操作员可以独立启用“代理本机流量”，并为以下对象设置直连例外：

- UID；
- 来源或目标地址；
- 目标端口；
- Runtime、特权网络进程和 Mihomo 自身的必要上游流量。

启用本机流量不会扩大 LAN 接管范围，也不能把 WAN 入站流量变成代理流量。

### IPv6

第一版网关不代理 IPv6。默认让 IPv6 直连并持续显示警告。操作员可以选择在网关运行期间阻断转发的 IPv6；Runtime 停止或故障时必须移除该阻断。

不能把 IPv4 网关成功状态显示为“双栈已代理”。

## 故障放行

故障放行在 Runtime、Mihomo 和特权网络进程三个层次执行：

1. Mihomo 不健康时，先撤销流量接管，再尝试有限重启；
2. Runtime 主进程退出时，特权网络进程根据租约和父进程状态撤销 Runtime 网络对象；
3. 特权网络进程自身重启时，先核对持久所有权记录和系统实际状态，清理没有有效 Runtime 租约的对象；
4. 系统服务停止、产品更新和卸载都走相同清理路径；
5. 清理失败时报告仍存在的具体路由、DNS 或防火墙对象，不能声称已经恢复直连。

Runtime 只恢复自己在启用时记录的原值。系统设置已被第三方再次修改时，不用旧快照覆盖新值；应移除 Runtime 对象并报告冲突，交由操作员处理。

故障放行开始、完成、超时和发现残留时都写入持久事件并触发本机高优先级通知。清理具有平台固定的短截止时间；超过截止时间后继续由特权服务在后台按租约回收，但 Snapshot 必须保持“接管状态未知”并列出可能泄漏或仍被拦截的流量范围，不能显示“已直连”。由于第一版明确采用故障放行，任何成功清理都会导致流量和 DNS 绕过代理，界面和文档必须持续说明这一泄漏边界。

## 网络操作状态

每次启用网络接管都生成持久的所有权记录，至少包含：

- Runtime 实例固定 ID；
- 当前 Operation ID；
- 运行方式；
- 创建的 TUN、路由、策略规则、DNS 和防火墙对象；
- 修改前观测值；
- 所有权标识与内容摘要；
- 租约或父进程状态；
- 最后一次真实系统观测。

Snapshot 展示的是系统实际状态，不根据数据库中的期望值推断“已接管”或“已清理”。

安装实例 ID 和网络所有权 token 在首次安装时随机生成且永不复用。所有权记录只允许特权网络进程写入，并带完整性校验；系统对象标签包含不可预测 token，而不只使用可猜测名称。名称相同但 token、摘要或记录完整性不匹配时只报告冲突，不能删除或覆盖。

## 发布验收

### 通用 TUN

每个稳定支持的平台和架构必须在真实系统或等价虚拟机中验证：

- 安装、首次授权、启动、停止和系统重启；
- IPv4、IPv6、TCP、UDP 和 DNS；
- 局域网、既有具体路由和 VPN 共存；
- 默认路由冲突拒绝；
- Runtime、Mihomo 和特权网络进程分别崩溃；
- 产品与核心更新期间故障放行；
- 卸载和清理残留检查。

### Linux 网关

除单元测试外，至少使用 Linux network namespace 和真实双网卡或等价环境验证：

- 新客户端自动纳入默认范围；
- TCP、UDP、QUIC 和 TCP/UDP DNS；
- UDP 全局关闭和按规则直连；
- 单臂与双网卡；
- WAN DNAT 的 TCP/UDP 服务与回包；
- 网关主机自身流量默认直连和可选接管；
- IPv6 直连警告与可选阻断；
- 并发连接、Runtime/Mihomo 崩溃、服务重启、更新和卸载；
- 只删除 Runtime 自有规则，保留用户 NAT 与防火墙。

当前实现已在 network namespace 中覆盖双网卡、单臂、新客户端、并发 TCP/UDP、QUIC 形态的 UDP/443、TCP/UDP DNS、UDP 全局关闭和类型化例外、WAN DNAT、宿主流量、IPv6 直连或阻断、Mihomo 故障、特权服务重启、更新清理和租约回收。测试还会确认用户 NAT 表保留且 Runtime 自有对象没有残留。

这些等价环境测试不能替代真实物理网关验收。在实机验收完成并记录前，Runtime 的预览和状态接口、CLI、TUI、GUI 都必须持续显示 Linux 网关为预览功能，不能作为稳定功能发布。
