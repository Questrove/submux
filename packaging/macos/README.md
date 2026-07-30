# macOS LaunchDaemon 模板

这两个 plist 是 PKG 构建输入，不是可直接复制后加载的完整安装器。打包时必须先创建系统账户 `_submux-runtime`、同名服务组和独立的 `submux-runtime-operators` 操作员组，再把 root helper 模板中的两个占位符替换为服务账户的十进制 UID 和 GID。未替换占位符时不得安装。

安装器负责创建并核对以下固定对象：

| 路径 | 所有者 | mode | 用途 |
|---|---|---:|---|
| `/Library/PrivilegedHelperTools/submux-runtime` | `root:wheel` | `0755` | 低权限 LaunchDaemon 的不可写程序 |
| `/Library/PrivilegedHelperTools/submux-runtime-net` | `root:wheel` | `0755` | root 网络 LaunchDaemon |
| `/Library/Application Support/SubmuxRuntime` | `_submux-runtime:_submux-runtime` | `0700` | Runtime 状态与待校验对象 |
| `/Library/Application Support/SubmuxRuntimePrivileged` | `root:wheel` | `0700` | 特权状态与摘要锁定对象 |
| `/var/run/submux-runtime` | `_submux-runtime:submux-runtime-operators` | `0750` | 管理 Socket 目录 |
| `/var/run/submux-runtime-privileged` | `root:_submux-runtime` | `0750` | 内部网络与 Mihomo 控制 Socket 目录 |

管理目录只有 `_submux-runtime` 所有者可写，`submux-runtime-operators` 只能进入目录并连接 `runtime.sock`，不能在 Runtime 停止时放入替代 Socket。普通 `admin` 组不会获得该权限。root helper 使用独立的 root 所有目录，并把内部 `runtime-net.sock` 收紧为 `root:_submux-runtime` 和 `0660`。

两个 plist 最终安装到 `/Library/LaunchDaemons`，必须由 `root:wheel` 拥有且 mode 为 `0644`。PKG 应先启动 root helper，再启动低权限 Runtime。更新、卸载或回滚时，先让 Runtime 撤销网络接管并停止 Mihomo，再停止两个 LaunchDaemon；不能在路由仍由 Runtime 持有时直接删除程序或状态目录。
