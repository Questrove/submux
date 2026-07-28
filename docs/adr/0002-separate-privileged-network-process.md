# 将网络特权放入受限进程

Submux Runtime、GUI、TUI 和 CLI 保持低权限，TUN、路由、DNS、防火墙与 Linux 网关由独立的 `submux-runtime-net` 执行；它只接受 Runtime 服务账户通过内部 IPC 发出的固定类型操作，不接受 URL、任意路径、命令或防火墙片段。Linux 优先把预创建的 TUN 授予低权限 Mihomo，Windows 先验证 Wintun 设备 ACL 方案，macOS 及 Windows 确有平台限制时允许特权进程按固定路径和摘要启动官方 Mihomo。单一高权限 Runtime 实现更简单，但会让配置解析、下载器和全部管理接口进入机器级信任范围，因此不采用。
