# Runtime 支持矩阵

`docs/runtime-support.json` 是发行门禁读取的机器可读支持矩阵。当前所有平台都属于预览：

| 平台 | 架构 | 当前证据 | 仍缺少的关键证据 |
|---|---|---|---|
| Ubuntu 24.04、systemd/glibc Linux | amd64 | Go 测试、真实 network namespace、WSL2 systemd 下 DEB/RPM/tar 安装与卸载 | 完整 IPv6/UDP、真实重启、非虚拟化发行版和产品更新回滚 |
| systemd/glibc Linux | arm64 | 交叉编译 | 原生执行与完整平台验收 |
| Windows 10/11/Server | amd64 | 原生单测、PE 和 MSI 构建校验 | 服务安装、IPC/TUN、崩溃/重启、更新/回滚/卸载和可信签名 |
| Windows 11/Server | arm64 | 交叉编译、MSI 结构校验 | 原生 arm64 完整平台验收和可信签名 |
| macOS 13+ Intel | amd64 | 交叉编译、Universal PKG 结构 | Intel 原生完整平台验收、Developer ID 签名与公证 |
| macOS 13+ Apple Silicon | arm64 | 原生单测、Universal PKG 结构 | 服务安装、IPC/TUN、崩溃/重启、更新/回滚/卸载、签名与公证 |

矩阵校验器不会根据编译成功推断稳定支持。一个目标只有在 `native` 证据同时覆盖安装与首次授权、IPC、显式代理、TUN、IPv4/IPv6、TCP/UDP、DNS、路由冲突、故障放行、三个进程崩溃、系统重启、升级、数据库迁移、程序回滚、卸载残留、安全拒绝和 GUI/TUI/CLI 能力一致性后，才允许标为 `stable`。Linux 还必须提供网关 TCP/UDP 的原生证据。

证据来源写入具体工作流或测试文件。工作流尚未在目标系统通过，或者只能交叉编译、静态检查、虚拟化执行时，状态继续保持 `preview`。Windows 的 Unknown Publisher、macOS 的未签名和未公证状态也属于预览边界，不能在下载页省略。
