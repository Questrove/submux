# Submux Runtime 安装、更新与发行

本文定义 Submux Runtime 的目标发行方式。目标安装器、安装包和 Release 产物尚未实现。

## 支持范围

第一版目标如下：

| 系统 | 基线 | 架构 | 说明 |
|---|---|---|---|
| Windows | Windows 10 22H2、Windows 11、Windows Server 2019 及以上 | amd64、arm64 | GUI、TUI、CLI、显式代理与 TUN |
| macOS | macOS 13 及以上 | Universal（amd64、arm64） | GUI、TUI、CLI、显式代理与 TUN |
| Linux | 主流 systemd、glibc 发行版；网关要求 Linux 5.10 及以上 | amd64、arm64 | GUI 可选；显式代理、TUN、Linux 网关 |

Linux 稳定包以 Debian/Ubuntu、Fedora/RHEL 系和其他满足依赖的 systemd 发行版为主要目标。Alpine、OpenWrt、musl 和非 systemd 系统不属于第一版承诺；后续可以提供专门适配，不能让通用 tar 包暗示完整的机器级 TUN 支持。

编译成功不等于稳定支持。某个系统或架构没有完成安装、IPC、权限、TUN、更新、回滚和卸载测试时，其产物必须标为预览，不能列入稳定支持矩阵。

## 机器级安装

Runtime 是机器服务，不依赖桌面登录。每台机器只允许一份 Runtime 安装和一个受管 Mihomo。

### Linux

- systemd system service：`submux-runtime.service`
- Runtime 账户：`submux-runtime`
- 操作员组：`submux-runtime`
- 配置根：`/etc/submux-runtime`
- 状态根：`/var/lib/submux-runtime`
- 运行根：`/run/submux-runtime`
- 管理 Socket：`/run/submux-runtime/runtime.sock`

服务账户和操作员组都使用 `submux-runtime` 这个系统名称，但状态目录保持 `0700`、文件保持 `0600`、服务使用 `umask 0077`；只有管理 Socket 以该组和 `0660` 开放。服务器安装默认只允许 root 管理。桌面安装器可以在明确确认后把当前用户加入操作员组。Mihomo 和主 Runtime 默认不持有 `CAP_NET_ADMIN`。

### Windows

- Windows Service：`SubmuxRuntime`
- 服务账户：`NT SERVICE\SubmuxRuntime`
- 操作员组：`Submux Runtime Operators`
- 状态根：`%ProgramData%\SubmuxRuntime`
- 管理 Pipe：`\\.\pipe\submux-runtime`

主 Runtime 服务不使用 LocalSystem。`submux-runtime-net` 使用单独的 LocalSystem 服务和仅向 Runtime 服务 SID 开放的内部 Pipe。

### macOS

- Runtime：LaunchDaemon
- Runtime 账户：`_submux-runtime`
- 操作员组：`submux-runtime-operators`
- 状态根：`/Library/Application Support/SubmuxRuntime`
- 管理 Socket：`/var/run/submux-runtime/runtime.sock`

特权网络进程使用独立 root LaunchDaemon。桌面 GUI 是每个获授权用户自己的登录项，不随 Runtime daemon 一同提权。

所有平台的状态文件默认只允许 Runtime 账户和系统管理员读取。操作员通过 IPC 管理，不因加入操作员组而获得状态目录的直接读取权限。

## 安装包

### Windows

- 普通在线 MSI；
- 包含固定 Mihomo 的完整离线 MSI；
- ZIP 只供开发、检查和便携 CLI 试用，不能安装机器服务或 TUN。

在没有付费代码签名资源时，MSI 会显示 Unknown Publisher，并可能触发 SmartScreen。项目必须在下载页如实说明，不使用无信任价值的自签名 Authenticode 冒充正式签名。

### macOS

- 普通在线 PKG；
- 包含固定 Mihomo 的完整离线 PKG；
- 提供从源码构建说明。

在没有 Apple Developer ID 和公证资源时，PKG 明确标记为未签名、未公证，并提供手动核验和信任步骤。不用 DMG 掩盖同样的信任问题。

### Linux

- OpenPGP 签名的 DEB 仓库与包；
- OpenPGP 签名的 RPM 仓库与包；
- 其他受支持 systemd 发行版使用 tar.zst 加安装脚本；
- 桌面 GUI 需要单独系统 WebKitGTK 依赖，缺少 GUI 依赖时 TUI 和 CLI 仍可使用。

所有平台同时发布 SHA-256、SBOM、TUF 元数据和 GitHub Artifact Attestation。

## 安装器状态机

安装器只检测 Submux Runtime 自己：

| 当前状态 | 请求包 | 行为 |
|---|---|---|
| 未安装 | 任意有效包 | 全新安装 |
| 已安装旧版本 | 新版本 | 保留状态并升级 |
| 已安装同版本 | 同版本 | 只有显式选择“修复”才重新安装 |
| 已安装新版本 | 旧版本 | 默认拒绝；显式降级且数据库兼容时才允许 |
| 已有第二个安装目标 | 任意 | 拒绝创建第二实例 |

安装器不寻找、停止、导入、迁移或删除其他旧运行软件。用户负责在安装 Runtime 前自行清理；安装器只在 Runtime 自身的服务、目录或安装记录冲突时停止。

修复安装校验程序、服务、权限和固定目录，但不重置状态。全新安装不会自动启用 TUN 或网关，也不会因为成功添加来源而启动 Mihomo。

## 在线与离线安装

普通在线安装包不包含 Mihomo，以控制体积。首次配置时 Runtime 先更新 TUF 元数据，再从固定 `MetaCubeX/mihomo` 官方 Release 直连下载与系统、架构匹配且已列入 TUF Targets 的核心，同时校验 TUF 摘要与 Release 提供的 SHA-256。首次下载不增加临时系统代理或手工代理入口。

无法直连时使用完整离线包。完整离线包包含：

- GUI；
- `submux-runtime`；
- `submux-runtime-net`；
- 固定的官方 Mihomo 稳定版；
- TUF 元数据、摘要、许可证和 SBOM。

离线包不捆绑 Windows WebView2、Linux WebKitGTK、系统内核模块或包管理器等 OS 级依赖。安装前检查依赖并给出离线准备清单。

第一份远程配置无法直连时，用户可以在其他机器下载后作为本机副本导入。Runtime 成功运行后，后续来源和更新才可以选择经过当前 Mihomo 下载。

## 更新原则

Runtime 永不自动安装更新。允许：

- 默认每 24 小时检查一次稳定通道，并加入抖动；
- GUI、TUI、CLI 手动检查；
- 用户明确启用后预下载稳定更新，默认关闭；
- 在线或离线手动安装；
- 安全更新显示更明显的通知。

不允许：

- 后台自动安装；
- 无确认重启 Runtime、Mihomo 或网络；
- 配置来源触发更新；
- 通过任意 URL 安装程序；
- 自动切换到预发布通道。

第一版 Runtime 更新界面只提供稳定通道。预发布构建可以在 GitHub Releases 发布，并允许操作员手动导入经过验证的预发布离线包，但不会出现在定时检查或预下载中。

## TUF 信任

Runtime 产品、普通 Mihomo 核心目标和离线更新包使用 The Update Framework，不使用自定义的单签名清单。

至少保留以下角色：

- Root：离线保存，定义受信任角色和密钥轮换；
- Targets：离线或受严格保护地签署具体程序、普通 Mihomo 核心目标、包、摘要、大小和兼容元数据；
- Snapshot：保证元数据集合一致；
- Timestamp：短期在线签名，限制冻结和旧元数据重放。

安装包嵌入初始受信任 Root 元数据。客户端保存已经接受的最高元数据版本，拒绝回滚、过期、阈值不足、摘要不符或大小不符的目标。

产品和普通核心的在线与离线更新走同一验证器。离线包必须携带完整的所需 TUF 元数据，不能因为“文件来自 U 盘”跳过签名、版本和摘要检查。

Root 私钥不进入仓库或 CI。独立开发者至少保存两份离线加密备份并记录恢复流程。CI 只能持有职责受限、有效期较短的在线元数据密钥。GitHub Attestation、OpenPGP、SHA-256 和 OS 代码签名都是补充证据，不替代 TUF 客户端验证。

## 产品更新流程

GUI、Runtime 和特权网络进程按同一产品版本安装和回滚：

1. 下载或导入包；
2. 完成 TUF、摘要、平台、架构和磁盘空间检查；
3. 展示版本、发行说明、是否需要数据库迁移以及代理会短暂停止；
4. 操作员明确确认；
5. 生成数据库与程序回滚点；
6. 先撤销 TUN、路由和 DNS，使网络恢复直连；
7. 停止 Mihomo、Runtime 和特权网络进程；
8. 由平台安装器替换整套产品；
9. 迁移数据库并启动服务；
10. 完成本机 IPC、配置和网络健康检查；
11. 按原期望运行状态恢复；
12. 失败时恢复旧程序和数据库备份，再恢复旧运行状态。

不能在 TUN 或网关仍接管流量时热替换机器服务。GUI 在更新前退出，更新后由用户或安装器以普通用户身份重新启动。

## Mihomo 更新

Runtime 不声明或强制 Mihomo 兼容版本范围。普通界面只安装已经进入 Submux TUF Targets 的 `MetaCubeX/mihomo` 官方稳定 Release。高级设置可以启用“直接采用上游 Release”，选择尚未进入 TUF Targets 的官方稳定版或 `Prerelease-Alpha`，并持续显示该路径只依赖 GitHub 上游账号、HTTPS 和 Release 摘要，不受 Submux TUF 目标签名保护。

“官方核心”必须同时满足：

- 仓库固定为 `MetaCubeX/mihomo`；
- tag 和资产来自官方 GitHub Release；
- 资产名称精确匹配当前系统和架构；
- 普通路径同时校验 TUF Targets 和 Release 提供的 SHA-256；高级上游路径至少校验 GitHub Release 提供的 SHA-256；
- 解压后执行 `mihomo -v` 核对版本；
- 可执行文件复制到 Runtime 固定核心目录，拒绝用户指定目标路径。

更新前使用候选核心执行当前候选配置的静态检查。切换后若核心无法启动或即时健康检查失败，自动恢复上一版。若新核心能够启动但之后表现异常，Runtime 只提供手动一键回滚，不承诺判断语义兼容性。至少保留当前版和上一版。

高级上游路径每次安装都要求再次确认，不能设为自动检查、预下载或默认路径，其 Operation 明确记录 `trust=upstream_only`。它仍然只能从固定官方仓库下载，不能接受镜像 URL 或本机同名文件。没有网络时只能导入 TUF 已签署的核心或使用完整离线包；不能用无法独立验证的 Release 页面副本绕过离线信任。

用户不能用同名本机文件替换核心，也不能把镜像仓库或下载地址保存为“官方来源”。手工导入核心必须存在于 TUF Targets 中并通过相同校验。

## 下载线路

产品和核心下载默认直连，也可以明确选择经过当前 Mihomo。两者分别保存线路设置，不继承配置来源的下载设置。

下载失败时不静默切换线路。手动操作可以选择仅本次使用另一条线路。Runtime 记录使用的线路和结果，但不记录代理凭据。

如果 Runtime 当前未运行 Mihomo，则“经过当前 Mihomo”不可选。完整离线包是无网络或无法直连环境的恢复路径。

## 体积约束

普通在线安装包不包含 Mihomo，目标压缩体积为：

| 内容 | 目标 |
|---|---:|
| GUI | 不超过 5 MiB |
| `submux-runtime` | 不超过 5 MiB |
| `submux-runtime-net` | 不超过 3 MiB |
| 普通在线安装包 | 不超过 15 MiB |
| Linux AppImage（若提供） | 约 25–30 MiB |
| 包含 Mihomo 的完整离线包 | 约 30–40 MiB |

CI 对每个目标架构记录未压缩和压缩体积，超过预算时失败或要求明确批准。不得为追求单一数字删除符号化崩溃信息、许可证、SBOM、回滚程序或安全验证。

## 许可证与第三方源码

submux 继续使用 MIT 许可证。Mihomo 作为未经修改的独立 GPLv3 程序聚合分发，不与 Runtime 链接。

每个包含 Mihomo 的发行版必须：

- 附带 Mihomo GPLv3 许可证；
- 记录精确版本、tag、提交和资产摘要；
- 在同一发行位置长期提供该精确版本的对应源码归档；
- 保证源码和二进制都可免费取得；
- 保留上游版权与无担保声明。

不能只保存一个可能失效的上游链接。若未来修改或 fork Mihomo，必须重新评估 GPLv3 对修改源码、构建脚本和安装信息的要求。

## 备份、卸载和清除

产品升级默认保留 Runtime 状态。卸载默认也保留状态目录，以便重新安装后显式恢复；操作员选择“同时清除数据”时才删除。

卸载顺序固定为：

1. 阻止新 Operation；
2. 撤销 Runtime 网络接管；
3. 停止 Mihomo、Runtime 和特权网络进程；
4. 删除服务、操作员授权和程序；
5. 根据用户选择保留或清除 Runtime 状态；
6. 检查并报告残留的路由、DNS、防火墙、TUN、服务和文件。

清除 Runtime 不负责删除其他 Mihomo、用户防火墙或非 Runtime 创建的系统设置。
