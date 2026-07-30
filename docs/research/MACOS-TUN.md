# macOS 普通 TUN 研究（官方资料，2026-07-30）

## utun 的创建与权限边界

Apple XNU 的 `if_utun.c` 将 utun 暴露为 PF_SYSTEM 控制套接字：客户端先调用 `socket(PF_SYSTEM, SOCK_DGRAM, SYSPROTO_CONTROL)`，再用 `ioctl(fd, CTLIOCGINFO, ...)` 查询 `UTUN_CONTROL_NAME`，最后通过 `connect` 传入 `sockaddr_ctl` 建立接口；`UTUN_OPT_IFNAME` 可读出分配的 `utunN` 名称。[Apple XNU if_utun.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/if_utun.c)；[Apple XNU utun.h](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/if_utun.h)

XNU 源码中的 `utun_ctl_connect()` 在创建接口时调用 `ifnet_allocate`/`ifnet_attach`，并按系统 socket/control 权限检查连接；这不是普通用户可无条件创建系统网络接口的 API。root helper 可以负责创建并保持 utun fd，再通过受 ACL 保护的 Unix socket 传递数据或句柄；低权限 Mihomo 只有在 helper 明确授予它所需 IPC 权限、且 Mihomo 支持使用已存在设备时才可运行。Mihomo 自己启动 TUN 时仍需要获得创建接口、配置地址及路由的权限，不能假设“固定 `utun` 名称”绕过授权。[XNU if_utun.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/if_utun.c)

## 路由：精确所有权

Darwin 的路由表由 PF_ROUTE raw socket（`socket(PF_ROUTE, SOCK_RAW, AF_UNSPEC)`）交换 `RTM_ADD`、`RTM_DELETE`、`RTM_GET` 消息；消息包含 `rt_msghdr` 与按 `RTA_*` 标记排列的地址。Apple XNU 的 `route.c` 和 `route.h` 是实现与 ABI 的一手定义。[XNU route.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/route.c)；[XNU route.h](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/route.h)

预览/提交应记录每一条目标前缀、网关、接口索引、掩码、度量及创建前快照；删除时仅发送仍与本实例记录完全匹配的 `RTM_DELETE`。`/sbin/route` 只是同一 PF_ROUTE 接口的命令行客户端，不能提供额外隔离；不得按前缀盲删、清空路由表或接管已有 VPN/默认路由。路由 ioctl（例如接口地址/标志相关 `SIOCAIFADDR` 等）同样属于内核网络配置边界，应由 root helper 执行并逐项授权。[XNU route.c](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/route.c)

## DNS 快照、设置与恢复

SystemConfiguration 的 `SCDynamicStore` 读取动态网络状态（包括 `State:/Network/Global/DNS` 及按服务的 DNS 字典）；`SCPreferences` 用于持久化网络偏好设置。实现应先复制相关 key 的完整字典，提交后再次读取并只恢复仍由本实例修改且值未被其他组件改变的 key。[SCFoundation SCDynamicStore](https://developer.apple.com/documentation/systemconfiguration/scdynamicstore)；[SCPreferences](https://developer.apple.com/documentation/systemconfiguration/scpreferences)

动态 store 的通知回调可用于检测 VPN、网络服务或 DNS 被外部改变；恢复失败必须保留快照并报告人工处理，不能把当前系统值覆盖成启动时旧值。SystemConfiguration 写入通常需要受系统授权的特权进程，普通 Mihomo 不应直接改全局 DNS。[Apple SystemConfiguration framework](https://developer.apple.com/documentation/systemconfiguration)

## Unix Socket、LaunchDaemon 与用户组

LaunchDaemon 由 launchd 以 root/系统上下文管理；`ProgramArguments`、`UserName`、`GroupName` 和 `Sockets` 等键定义进程身份和监听 socket。helper 应安装在 `/Library/LaunchDaemons`，把控制 Unix socket 放入受 root 拥有的目录，权限设为仅 Runtime 用户组可读写，并在协议层验证请求来源和操作范围。[Apple launchd.plist](https://www.manpagez.com/man/5/launchd.plist/)；[Apple Daemons and Services Programming Guide](https://developer.apple.com/library/archive/documentation/MacOSX/Conceptual/BPSystemStartup/Chapters/CreatingLaunchdJobs.html)

不要把 socket 文件模式当作唯一授权：目录、socket、helper 可执行文件和配置都应由 root 拥有；组成员变更会改变控制面权限，Runtime 仍须执行 capability/请求认证。LaunchDaemon 与用户会话隔离，不能依赖 GUI 登录态或用户 Keychain。

## Mihomo TUN 与 Network Extension 限制

Mihomo 官方 TUN 文档列出 `device`、`auto-route`、`auto-detect-interface`、`strict-route`、`dns-hijack` 等配置，但没有承诺 macOS 普通用户可创建 utun、修改 PF_ROUTE 或全局 DNS；这些选项也不会替代冲突检测和回滚。[Mihomo TUN](https://github.com/MetaCubeX/Meta-Docs/blob/main/docs/config/tun.md)

macOS 的 Network Extension（`NEPacketTunnelProvider`）是 Apple 授权的 App/系统扩展边界，配置由 `NETunnelProviderManager` 管理；它与直接使用 PF_SYSTEM utun 的 root helper 是两种部署模型，不能把一个模型的权限假设套到另一个模型。[Apple Network Extension](https://developer.apple.com/documentation/networkextension)；[NEPacketTunnelProvider](https://developer.apple.com/documentation/networkextension/nepackettunnelprovider)

Mihomo 二进制需分别为 `amd64`（x86_64）和 `arm64` 构建；架构本身不改变 root、SystemConfiguration 或 Network Extension 授权边界。Universal binary 也不能让未签名/未授权的 Network Extension 加载。

## 最小可实现 preview 架构与真实测试缺口

最小实现可分为：低权限 Runtime（Unix socket 客户端、状态/预览、Mihomo 配置）与 root helper（创建/保持 utun、PF_ROUTE 精确变更、SystemConfiguration 快照/恢复）。Runtime 先请求 capability 和只读快照，展示接口、路由、DNS 变更及冲突；用户确认后 helper 按实例 ID 执行，停止时仅删除/恢复仍匹配的记录，失败即保持 direct-network fail-open。

尚缺真实 macOS 13+ 测试：Intel 与 Apple Silicon；全新 utun 创建及 helper 重启；已有 VPN/多个 utun/睡眠唤醒/网络切换；IPv4/IPv6 默认路由冲突；DNS 被 DHCP/VPN 同时修改；Unix socket 组权限和 launchd 重启；Mihomo 低权限仅使用预创建 utun 的实际读写能力；Network Extension 签名、entitlement、沙盒安装流程。Linux/Windows CI 或模拟 PF_ROUTE 不能替代这些测试。
