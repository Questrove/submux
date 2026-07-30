# Windows TUN 研究（官方资料，2026-07-30）

## Wintun 适配器与低权限进程

Wintun API 明确区分创建和打开：`WintunCreateAdapter(Name, TunnelType, RequestedGUID)` 创建适配器，`WintunOpenAdapter(Name)` “opens an existing Wintun adapter”，名称最长 `MAX_ADAPTER_NAME-1`；打开得到的句柄须用 `WintunCloseAdapter` 释放。[Wintun API](https://git.zx2c4.com/wintun/about/)

“按 device 名复用”因此只在适配器已存在且名称匹配时成立；名称不是授权机制。官方源码的 `AdapterOpenDeviceObject` 最终对设备接口调用 `CreateFileW(..., GENERIC_READ | GENERIC_WRITE, FILE_SHARE_READ | FILE_SHARE_WRITE | FILE_SHARE_DELETE, ..., OPEN_EXISTING, ...)`，所以低权限 Mihomo 必须能打开该设备对象并取得读写句柄；仅由 LocalSystem 预创建并不会自动授予普通用户权限。[adapter.c](https://git.zx2c4.com/wintun/tree/api/adapter.c)

Wintun 文档没有承诺“预创建后任意用户可打开”，也没有公开一个可由应用传入的 adapter ACL 参数。实际权限取决于 Windows 设备对象/驱动安全描述符；若要让低权限服务使用，必须在安装/特权阶段以最小 DACL 授予该服务 SID（或明确的低权限账户）访问设备对象，并验证 `WintunOpenAdapter`、启动 session 和收发包均成功。驱动安装、创建/删除适配器及修改系统网络配置仍应留在特权边界内。

## 路由、DNS 与清理 API

IPv4/IPv6 路由应使用 IP Helper 的同一组 `MIB_IPFORWARD_ROW2` API：`InitializeIpForwardEntry`、`GetIpForwardTable2`、`CreateIpForwardEntry2`、`SetIpForwardEntry2`、`DeleteIpForwardEntry2`；`GetBestRoute2` 可用于预览选择的路由。[Microsoft IP Helper 路由表](https://learn.microsoft.com/en-us/windows-hardware/drivers/network/ip-helper) `CreateIpForwardEntry2` 要求管理员权限，并拒绝同一接口上完全重复的 DestinationPrefix/NextHop。[CreateIpForwardEntry2](https://learn.microsoft.com/en-us/windows/win32/api/netioapi/nf-netioapi-createipforwardentry2)

精确清理必须保存创建前后的 `InterfaceLuid/Index`、前缀、下一跳、Metric、协议/策略等行，并只调用 `DeleteIpForwardEntry2` 删除本次创建且仍匹配的行；不要用“清空路由表”或按前缀盲删。接口和地址状态可通过 `GetIfTable2`、`GetIpInterfaceTable`、`GetUnicastIpAddressTable` 查询，相关枚举见同一 IP Helper 文档。

DNS 的官方接口是 `GetInterfaceDnsSettings` / `SetInterfaceDnsSettings`（`netioapi.h`，按接口 LUID 设置 DNS 服务器、域和 DoH 等设置）；旧式地址枚举/修改可用 `GetAdaptersAddresses` 及 IP Helper 接口。实现必须先快照原设置，停止时仅恢复仍由本实例修改的接口设置，并保留失败后的 preview/人工确认路径。[GetInterfaceDnsSettings](https://learn.microsoft.com/en-us/windows/win32/api/netioapi/nf-netioapi-getinterfacednssettings)、[SetInterfaceDnsSettings](https://learn.microsoft.com/en-us/windows/win32/api/netioapi/nf-netioapi-setinterfacednssettings)

全隧道（0/1 默认路由或等价 IPv6 路由）会与控制面、DNS、已有 VPN/代理及回环访问发生递归或断联。Windows API 能创建路由，但不会替应用解决回环豁免、控制面 bootstrap 或并发路由所有权；这些必须在 preview 中展示并在提交前验证。

## Named Pipe、服务 SID 与 DACL 边界

创建本地控制管道时使用 `CreateNamedPipe` 的 `PIPE_REJECT_REMOTE_CLIENTS`，该选项使远端客户端被拒绝；Microsoft 的 HLK 说明要求通过 `GetNamedPipeInfo` 检查该选项。[Named Pipe Reject Remote Clients](https://learn.microsoft.com/en-us/windows-hardware/test/hlk/testref/e3bcbd3f-3e9c-484a-a587-3c081cb28f7a) 这只限制远端，不替代 DACL，也不提供认证。

服务 SID 应启用 `SERVICE_SID_TYPE_UNRESTRICTED` 或更严格的 `SERVICE_SID_TYPE_RESTRICTED`，然后在管道、配置目录、Wintun 设备等对象的 DACL 中只授予 `NT SERVICE\\<ServiceName>` 所需权限。`SERVICE_SID_TYPE_RESTRICTED` 会把服务 SID 加入 restricted SID 列表；同一进程承载的多个服务必须全部使用 restricted 类型。[SERVICE_SID_INFO](https://learn.microsoft.com/en-us/windows/win32/api/winsvc/ns-winsvc-service_sid_info) 服务对象自身的启动/停止权限另由 SCM 服务对象 DACL 控制，使用 `QueryServiceObjectSecurity`/`SetServiceObjectSecurity` 管理，不能把它与管道 DACL 混为一谈。[Modifying the DACL for a Service](https://learn.microsoft.com/en-us/windows/win32/services/modifying-the-dacl-for-a-service)

## Mihomo external-controller-pipe 与 Windows TUN 限制

Mihomo 文档支持 `external-controller-pipe: \\\\.\\pipe\\mihomo`，但明确警告：Windows Named Pipe API 不校验 secret，启用后必须自行做好安全措施；因此只能把它放在本机、受 DACL 保护的 Runtime IPC 后面，不能当作远程管理接口。[Mihomo general config](https://github.com/MetaCubeX/Meta-Docs/blob/main/docs/config/general.en.md)

Mihomo 的 TUN 配置（官方文档）包含 `device`、`stack`、`dns-hijack`、`auto-route`、`auto-detect-interface`、`strict-route` 等选项；文档没有承诺“低权限进程自动获得 Wintun/路由/DNS 权限”。Windows 上实际能否创建或打开 TUN、写入路由和 DNS，取决于驱动设备 DACL、进程权限和系统已有路由；`auto-route`/`strict-route` 也不能替代冲突检测与回滚。[Mihomo TUN](https://github.com/MetaCubeX/Meta-Docs/blob/main/docs/config/tun.md)

## 对本仓库的最小安全建议

1. LocalSystem/安装器一次性创建并持久化固定名称和 GUID 的 Wintun adapter；低权限 Mihomo 只调用 `WintunOpenAdapter`，特权阶段为其服务 SID 配置最小设备 DACL，失败即保持 direct-network fail-open。
2. Runtime 在提交前读取并保存接口、路由、DNS 快照，生成 preview；提交时仅执行明确的 `CreateIpForwardEntry2`/`SetInterfaceDnsSettings` 变更，停止时按快照和实例标记精确删除/恢复。任何默认路由、IPv6 全隧道、DNS 劫持或检测到其他 VPN/代理时必须继续保留 preview，不能静默应用。
3. `external-controller-pipe` 仅本机 Named Pipe：`PIPE_REJECT_REMOTE_CLIENTS` + 显式 DACL（Runtime 服务 SID），并在协议层仍要求认证/授权；不要依赖 Mihomo 的 secret（官方明确说明 pipe 不校验 secret）。
4. 先实现状态读取、预览、回滚和 capability 报告，再开放写入；驱动安装、适配器创建、系统路由/DNS 写入保持单独的特权网络进程边界。
