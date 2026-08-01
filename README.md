# submux

submux 生成并交付可用的 Mihomo / sing-box 配置；Submux Runtime 独立管理本机的 Mihomo 与 Runtime 配置来源。两者可以配合使用，也可以分别部署。

| 组件 | 用途 | 管理边界 |
|---|---|---|
| `submux` | 汇集机场来源和手工节点，按模板、规则方案和节点选择生成输出订阅 | 提供 Web 控制台和 HTTP 输出订阅，不运行代理核心 |
| Submux Runtime | 管理一台机器上的一个 Mihomo，提供显式代理、TUN 和 Linux 网关 | GUI、TUI、CLI 只通过本机 Unix Socket 或 Windows Named Pipe 管理，不接受 submux 远程控制 |

## 发布状态

| 产品 | 当前版本 | 状态 |
|---|---|---|
| submux 控制面 | [`v2.0.2`](https://github.com/Questrove/submux/releases/tag/submux-v2.0.2) | Stable |
| Submux Runtime | [`v2.0.1`](https://github.com/Questrove/submux/releases/tag/v2.0.1) | Preview |

Runtime 的功能、三平台安装包和发行门禁已经实现，但 Windows、macOS 和 Linux 尚未完成全部原生系统验收，因此目前都属于 Preview。Windows MSI 尚未进行 Authenticode 签名，会显示 Unknown Publisher；macOS PKG 尚未使用 Developer ID Installer 签名，也未公证。具体证据和缺项见 [Runtime 支持矩阵](docs/RUNTIME-SUPPORT.md)。

控制面和 Runtime 独立发版。控制面使用 `submux-vX.Y.Z` Release 标签，程序报告的版本仍为 `vX.Y.Z`；Runtime 使用 `vX.Y.Z` 标签。这样控制面的稳定安装通道不会受到 Runtime 预览状态影响。

## submux 控制面

### 工作方式

```text
机场来源 ─刷新─┐
              ├─> 规范化节点库 ─选择节点─┐
手工分享链接 ─┘                           ├─> 输出订阅 ─> /sub/{token}
完整配置模板 ─> 不可变模板版本 ───────────┤
规则方案 ─────> MetaCubeX 规则目录 ───────┘
```

1. 添加机场来源，或直接导入手工分享链接。
2. 在统一节点库中整理分类、标签和启用状态。
3. 选择内置模板，或者发布自己的 Mihomo / sing-box 模板版本。
4. 在规则方案中配置直连、主代理、流媒体代理和拦截规则。
5. 创建输出订阅，选择模板、规则方案和节点，保存后获得独立链接。

主要能力包括：

- 读取 Mihomo YAML、明文分享链接和 Base64 分享链接列表；
- 导入 VLESS、VMess、Trojan、Shadowsocks、Hysteria2 节点；
- 按节点语义指纹去重，在机场改名或连接参数变化后保留本机整理结果；
- 识别流量和到期信息，支持 continuity 与 strict 两种机场生命周期策略；
- 根据不可变模板版本和固定规则目录生成 Mihomo YAML 或 sing-box JSON；
- 编译失败时保留最近可用产物，不以不完整结果覆盖已经发布的输出订阅；
- 使用 bbolt 单文件存储，控制面为无 CGO 的 Go 单二进制。

### 安装稳定版

安装器支持 Linux 和 macOS，会固定准确版本、下载对应架构的 Release 二进制并校验 `checksums.txt`：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh |
  bash -s -- --version submux-v2.0.2
```

Linux 可以同时安装并启动 systemd 服务：

```sh
curl -fsSL https://raw.githubusercontent.com/Questrove/submux/main/scripts/install.sh |
  bash -s -- --version submux-v2.0.2 --service
```

不指定 `--version` 时，安装器使用 GitHub 的最新稳定 Release。它还支持 `--upgrade`、`--rollback` 和 `--uninstall`。Windows 用户可以从 [v2.0.2 Release](https://github.com/Questrove/submux/releases/tag/submux-v2.0.2) 下载 `submux-windows-amd64.exe` 和 `checksums.txt`，完成 SHA-256 校验后直接运行。

### 手动或离线安装控制面

从 v2.0.2 开始，控制面 Release 同时提供 `install-submux.sh`。在能够访问 GitHub 的机器上用浏览器、`curl` 或 `wget` 下载安装脚本、目标系统的单二进制和 `checksums.txt`，校验后把这几个文件放在同一个目录并转移到目标机器。下面以当前稳定版和 Linux amd64 为例：

```sh
RELEASE_TAG=submux-v2.0.2
BASE_URL="https://github.com/Questrove/submux/releases/download/${RELEASE_TAG}"

curl -fLO "${BASE_URL}/install-submux.sh"
curl -fLO "${BASE_URL}/submux-linux-amd64"
curl -fLO "${BASE_URL}/checksums.txt"
grep '  submux-linux-amd64$' checksums.txt | sha256sum --check
```

转移后在目标机器再次执行 SHA-256 校验。Linux 可以用一个安装入口创建专用账户、安装或更新二进制、写入 systemd 服务并完成健康检查；整个过程不访问网络：

```sh
RELEASE_TAG=submux-v2.0.2
grep '  submux-linux-amd64$' checksums.txt | sha256sum --check
sudo bash ./install-submux.sh \
  --version "$RELEASE_TAG" --offline-dir . --service
```

安装器默认保留 `/var/lib/submux/submux.db`。重装、修复或升级不会清空管理员、来源、模板和输出订阅；需要全新数据库时必须在服务停止后单独处理。macOS 把校验命令换成 `shasum -a 256 -c -`。Windows 使用 `Get-FileHash -Algorithm SHA256` 核对 `checksums.txt` 中的 `submux-windows-amd64.exe` 摘要，然后直接运行二进制。

### 从源码运行

需要 Go 1.26.1 或 `go.mod` 指定的更新版本：

```sh
go test ./...
CGO_ENABLED=0 go build -o submux ./cmd/submux
SUBMUX_DB=submux.db ./submux
```

默认监听 `127.0.0.1:8080`。首次打开 <http://127.0.0.1:8080> 设置管理员密码，然后按“来源 → 节点库 → 模板 → 规则 → 输出订阅”创建输出订阅。

## Submux Runtime

Runtime 可以保存多个 Runtime 配置来源，包括 submux 输出订阅、外部 HTTP(S) 完整配置和本机导入副本，但任意时刻只有一个当前来源。来源原文之上可以应用本机高级覆盖，监听、控制端点、运行方式、路由、DNS、网关和数据路径等本机运行设置始终由 Runtime 管理。

Runtime 由以下程序组成：

- `submux-runtime`：低权限机器服务，同时提供 CLI 和 TUI；
- `submux-runtime-net`：只执行固定网络操作的特权网络进程；
- `submux-runtime-gui`：可选的 Tauri 桌面客户端；
- Mihomo：由 Runtime 校验、启动、监控和回滚的官方代理核心。

一台机器或一个网络命名空间只允许一个 Runtime 和一个受管 Mihomo。显式代理不修改系统网络；TUN 或 Linux 网关发生故障时，Runtime 会撤销自己创建的网络设置并恢复直连。

### 选择在线包或离线包

所有安装包都在 [Submux Runtime v2.0.1 Release](https://github.com/Questrove/submux/releases/tag/v2.0.1) 中。

| 系统 | 在线包 | 完整离线包 |
|---|---|---|
| Debian / Ubuntu | `submux-runtime_2.0.1_<arch>_online.deb` | `submux-runtime_2.0.1_<arch>_offline.deb` |
| Fedora / RHEL | `submux-runtime_2.0.1_<arch>_online.rpm` | `submux-runtime_2.0.1_<arch>_offline.rpm` |
| 其他 systemd / glibc Linux | `submux-runtime_2.0.1_<arch>_online.tar.zst` | `submux-runtime_2.0.1_<arch>_offline.tar.zst` |
| Windows | `submux-runtime_2.0.1_<arch>_online.msi` | `submux-runtime_2.0.1_<arch>_offline.msi` |
| macOS 13+ | `submux-runtime_2.0.1_universal_online_unsigned.pkg` | `submux-runtime_2.0.1_universal_offline_unsigned.pkg` |

Linux 和 Windows 的 `<arch>` 为 `amd64` 或 `arm64`；macOS PKG 同时包含 Intel 和 Apple Silicon 程序。

在线包不包含 Mihomo。首次配置时，Runtime 通过 TUF 元数据核验并从固定的 `MetaCubeX/mihomo` 官方 Release 下载匹配的核心。完整离线包包含 Runtime、特权网络进程、固定的官方 Mihomo、TUF 元数据、SHA-256 证据、SBOM、许可证和对应的 Mihomo 源码归档，并在目标提供时包含 GUI。Linux arm64 包当前只提供 CLI 和 TUI，不包含 GUI。

离线包不包含 Windows WebView2、Linux WebKitGTK、系统内核模块、systemd、glibc 或包管理器等操作系统组件。这些依赖需要提前在目标系统中准备。

Release 中的产品 ZIP、裸二进制、`.wixpdb`、SBOM 和独立 TUF 元数据主要用于开发、更新验证和审计。普通机器安装应选择上表中的 DEB、RPM、tar.zst、MSI 或 PKG。

联网环境还可以使用 GitHub CLI 核对安装包的构建来源。把示例文件名换成实际下载的包；这项检查用于补充 SHA-256 与 Runtime 内部的 TUF 验证：

```sh
gh attestation verify ./submux-runtime_2.0.1_amd64_online.deb \
  --repo Questrove/submux
```

### Linux 安装

以下示例安装 amd64 在线 DEB。离线安装时把 `KIND` 改为 `offline`；arm64 机器把 `ARCH` 改为 `arm64`。

```sh
VERSION=2.0.1
ARCH=amd64
KIND=online
BASE_URL="https://github.com/Questrove/submux/releases/download/v${VERSION}"
PACKAGE="submux-runtime_${VERSION}_${ARCH}_${KIND}.deb"
CHECKSUMS="submux-runtime_${VERSION}_${ARCH}_${KIND}.sha256"

curl -fLO "${BASE_URL}/${PACKAGE}"
curl -fLO "${BASE_URL}/${CHECKSUMS}"
sha256sum --ignore-missing --check "${CHECKSUMS}"
sudo env SUBMUX_RUNTIME_AUTHORIZE_USER="$USER" dpkg -i "${PACKAGE}"
```

服务器只允许 root 管理时，安装命令不要设置 `SUBMUX_RUNTIME_AUTHORIZE_USER`：

```sh
sudo dpkg -i "${PACKAGE}"
```

RPM 使用相同的下载和校验方式：

```sh
PACKAGE="submux-runtime_${VERSION}_${ARCH}_${KIND}.rpm"
curl -fLO "${BASE_URL}/${PACKAGE}"
sha256sum --ignore-missing --check "${CHECKSUMS}"
sudo env SUBMUX_RUNTIME_AUTHORIZE_USER="$USER" rpm -Uvh "${PACKAGE}"
```

tar.zst 包适用于其他满足 systemd 和 glibc 要求的发行版：

```sh
PACKAGE="submux-runtime_${VERSION}_${ARCH}_${KIND}.tar.zst"
curl -fLO "${BASE_URL}/${PACKAGE}"
sha256sum --ignore-missing --check "${CHECKSUMS}"
RUNTIME_PACKAGE_DIR="$(mktemp -d)"
tar --zstd -xf "${PACKAGE}" -C "${RUNTIME_PACKAGE_DIR}"
sudo "${RUNTIME_PACKAGE_DIR}/install.sh" --authorize-desktop "$USER"
```

安装后检查服务和本机 IPC：

```sh
systemctl status submux-runtime submux-runtime-net
submux-runtime status --json
```

如果安装时没有授权桌面用户，之后可以由 root 明确授权；Linux 用户需要重新登录以取得新的组身份：

```sh
sudo submux-runtime-authorize-user --confirm "$USER"
```

### Windows 安装

在 PowerShell 中选择架构和包类型：

```powershell
$Version = '2.0.1'
$Arch = 'amd64'       # 或 arm64
$Kind = 'online'      # 无法联网时使用 offline
$BaseUrl = "https://github.com/Questrove/submux/releases/download/v$Version"
$Package = "submux-runtime_${Version}_${Arch}_${Kind}.msi"

Invoke-WebRequest "$BaseUrl/$Package" -OutFile $Package
Invoke-WebRequest "$BaseUrl/$Package.sha256" -OutFile "$Package.sha256"

$Expected = ((Get-Content "$Package.sha256") -split '\s+')[0].ToLowerInvariant()
$Actual = (Get-FileHash $Package -Algorithm SHA256).Hash.ToLowerInvariant()
if ($Actual -ne $Expected) { throw 'Submux Runtime MSI checksum mismatch' }
```

下面的命令安装机器服务，并授权当前 Windows 用户通过 Named Pipe 管理 Runtime：

```powershell
$MsiPath = (Resolve-Path $Package).Path
$Process = Start-Process msiexec.exe -Verb RunAs -Wait -PassThru -ArgumentList @(
  '/i', "`"$MsiPath`"", 'AUTHORIZE_CURRENT_USER=1'
)
if ($Process.ExitCode -notin @(0, 3010)) {
  throw "MSI installation failed with exit code $($Process.ExitCode)"
}
```

服务器安装只允许管理员管理时，删除 `AUTHORIZE_CURRENT_USER=1`。安装后可以检查：

```powershell
Get-Service SubmuxRuntime, SubmuxRuntimeNet
& "$env:ProgramFiles\Submux Runtime\submux-runtime.exe" status --json
```

Windows MSI 当前未签名。只有在从本项目 Release 下载、SHA-256 校验通过并接受 Preview 风险后才应继续安装。

### macOS 安装

macOS 使用同一个 Universal PKG 支持 Intel 和 Apple Silicon：

```sh
VERSION=2.0.1
KIND=online
BASE_URL="https://github.com/Questrove/submux/releases/download/v${VERSION}"
PACKAGE="submux-runtime_${VERSION}_universal_${KIND}_unsigned.pkg"

curl -fLO "${BASE_URL}/${PACKAGE}"
curl -fLO "${BASE_URL}/${PACKAGE}.sha256"
shasum -a 256 -c "${PACKAGE}.sha256"
sudo installer -pkg "${PACKAGE}" -target /
sudo /usr/local/sbin/submux-runtime-authorize-user --confirm "$USER"
```

安装后检查本机 IPC，或打开 GUI：

```sh
submux-runtime status --json
open -a 'Submux Runtime'
```

PKG 当前未签名且未公证，macOS 可能阻止安装。不要全局关闭 Gatekeeper；只有在下载来源和 SHA-256 都确认无误并接受 Preview 风险后才应安装。

### 完全离线安装

为了让首次核心安装与在线路径使用同一套 TUF 验证，v2.0.1 的完全离线流程还需要 Release 中的 `offline-verification-bundle.zip`。安装包负责安装机器服务并携带离线内容，Runtime 首次激活 Mihomo 时把该 ZIP 作为签名验证输入。

在能够访问 GitHub 的机器上完成以下准备：

1. 下载目标系统和架构对应的 `_offline` 安装包；
2. 下载该安装包对应的 `.sha256` 文件；
3. 下载 `offline-verification-bundle.zip` 和 `SHA256SUMS`；
4. 校验下载内容，然后把安装包、校验文件、离线验证包和准备好的 Mihomo 配置文件一起转移到目标机器；
5. 按上面的 Linux、Windows 或 macOS 命令安装，只是不再执行下载步骤。

Linux 可以这样校验离线验证包：

```sh
grep '  ./offline-verification-bundle.zip$' SHA256SUMS | sha256sum --check
```

macOS 使用：

```sh
grep '  ./offline-verification-bundle.zip$' SHA256SUMS | shasum -a 256 -c -
```

Windows PowerShell 使用：

```powershell
$Line = Get-Content .\SHA256SUMS |
  Where-Object { $_ -match '\s+\./offline-verification-bundle\.zip$' }
$Expected = ($Line -split '\s+')[0].ToLowerInvariant()
$Actual = (Get-FileHash .\offline-verification-bundle.zip -Algorithm SHA256).Hash.ToLowerInvariant()
if ($Actual -ne $Expected) { throw 'Offline verification bundle checksum mismatch' }
```

安装完成后，通过同一套 TUF 验证器导入离线验证包。第一条命令会返回一次性 `plan_id`：

```sh
submux-runtime mihomo import --json ./offline-verification-bundle.zip
submux-runtime mihomo install \
  --plan <plan_id> --trust tuf --confirm --wait --json
```

Windows PowerShell 中使用 `& "$env:ProgramFiles\Submux Runtime\submux-runtime.exe"` 代替上述命令开头的 `submux-runtime`。

没有可用网络时，Runtime 配置来源也应使用本机导入副本：

```sh
submux-runtime source import --name local-config --wait --json ./config.yaml
submux-runtime source list --json
submux-runtime source apply --wait --json <source_id>
submux-runtime proxy start --wait --json
```

离线文件来自 U 盘或局域网并不会绕过验证。Runtime 仍会检查 TUF 签名、元数据版本和有效期、目标平台、架构、文件大小与 SHA-256。

### Linux airgap kit

从 v2.0.1 开始，Runtime Release 提供按架构生成的 `submux-airgap_<version>_linux_<arch>.tar.gz`。它是便于整机离线部署的标准归档，包含归档元数据声明的控制面版本、完整 Runtime 机器包、特权网络进程、固定的官方 Mihomo 核心、离线 TUF 验证包、SBOM、许可证、源码材料和统一安装入口。它不替代 DEB、RPM 或独立控制面 Release；只是把 Linux 无网机器需要转移的材料放在一处。

在联网机器校验归档及其 `.sha256` 后，只需把归档上传到目标机。目标机不需要 zstd：

```sh
tar -xzf submux-airgap_<version>_linux_amd64.tar.gz
cd submux-airgap_<version>_linux_amd64
sudo ./install.sh all
```

`all` 依次安装控制面和 Runtime；只安装其中一个时改为 `control` 或 `runtime`。安装命令会重复验证归档内全部文件、创建并检查服务，并通过 Runtime 的 TUF 验证器激活包内官方 Mihomo。它不会删除 `/var/lib/submux` 或 Runtime 状态，不会创建 Runtime 配置来源，也不会自动启动代理。服务器默认只允许 root 管理；桌面机器可以显式授权一个本机操作员：

```sh
sudo ./install.sh runtime --authorize-desktop "$USER"
```

同版本 Runtime 重新安装需要 `--repair`。降级必须同时给出 `--allow-downgrade --database-compatible`。可先以普通用户运行 `./install.sh verify`，但实际安装也会执行相同校验。

### 在线首次配置

桌面用户可以直接打开 GUI；终端用户运行 `submux-runtime tui`。无桌面的服务器可以按以下顺序配置。

在线包需要先检查并安装 TUF 已签署的 Mihomo。`mihomo check` 会返回一次性 `plan_id`：

```sh
submux-runtime mihomo check --json
submux-runtime mihomo install \
  --plan <plan_id> --trust tuf --confirm --wait --json
```

添加 submux 输出订阅作为第一个 Runtime 配置来源：

```sh
submux-runtime source add \
  --type submux_output \
  --name submux-main \
  --url 'https://sub.example.com/sub/replace-with-token' \
  --wait --json

submux-runtime source list --json
submux-runtime source apply --wait --json <source_id>
submux-runtime proxy start --wait --json
submux-runtime proxy verify --json
```

第一个来源会成为当前来源。添加、刷新和导入来源都不会自动启动 Mihomo；首次应用仍保持停止，只有显式执行 `proxy start` 后才开始代理。多个来源可以通过 `source switch <source_id>` 明确切换，失败时保留原当前来源和最近可用配置。

显式代理是服务器和终端流程的默认运行方式。TUN 与 Linux 网关必须先执行 `network preview`，检查计划后再用返回的 `plan_id` 明确启用。完整参数见 [Runtime 网络设计](docs/RUNTIME-NETWORK.md) 和命令自身的帮助输出。

## 控制面配置

| 项 | 默认值 | 说明 |
|---|---:|---|
| `SUBMUX_DB` | `submux.db` | bbolt 数据文件路径 |
| `listen_addr` | `127.0.0.1:8080` | 控制面监听地址，在数据库设置中修改后重启生效 |
| `base_url` | 空 | 生成输出订阅外部链接时使用 |
| `fetch_interval_sec` | `10800` | 机场来源刷新间隔，范围 60–604800 秒 |
| 平台资源代理 | 直连 | 只供规则目录刷新和明确启用回退的机场来源使用 |
| 共享 `fake-ip-filter` | blacklist、空列表 | 保存后更新已经启用的 Mihomo 输出订阅 |

输出订阅 token 相当于访问凭据。对外提供输出订阅时必须使用 HTTPS：

```nginx
server {
    listen 443 ssl;
    server_name sub.example.com;
    ssl_certificate     /path/fullchain.pem;
    ssl_certificate_key /path/privkey.pem;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_http_version 1.1;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

然后把控制台 `base_url` 设置为 `https://sub.example.com`。Runtime 不通过这个反向代理接受管理，也不需要 WebSocket、设备注册或远程任务端点。

## 安全边界

- 控制面管理密码使用 bcrypt，登录会话使用 HMAC 签名的 `HttpOnly` / `SameSite` Cookie；
- 管理 API 需要会话，公开端点只能通过随机输出订阅 token 读取已发布产物；
- 模板、规则方案、输出订阅和节点转换采用严格校验，失败不会覆盖最近可用产物；
- Runtime 不注册设备、不发送心跳，也不接受 submux 发起的运行操作；
- Runtime 外部管理接口只存在于本机 Socket 或 Named Pipe，不监听 TCP；
- Runtime 配置来源不能修改保留监听、系统网络、程序下载地址或任意本机文件；
- Runtime 不发送遥测、崩溃报告或后台诊断，诊断包只在本机由操作员明确生成。

## 文档

| 主题 | 文档 |
|---|---|
| 领域术语和边界 | [CONTEXT.md](CONTEXT.md) |
| 控制面领域模型与发布语义 | [docs/DESIGN.md](docs/DESIGN.md) |
| 支持的节点协议 | [docs/PROTOCOLS.md](docs/PROTOCOLS.md) |
| 机场生命周期 | [docs/LIFECYCLE.md](docs/LIFECYCLE.md) |
| 控制面发行 | [docs/RELEASING-SUBMUX.md](docs/RELEASING-SUBMUX.md) |
| Runtime 总体设计 | [docs/RUNTIME.md](docs/RUNTIME.md) |
| Runtime 本机 IPC | [docs/RUNTIME-IPC.md](docs/RUNTIME-IPC.md) |
| TUN、Linux 网关与权限边界 | [docs/RUNTIME-NETWORK.md](docs/RUNTIME-NETWORK.md) |
| 安装、更新、离线包与发行 | [docs/RUNTIME-DISTRIBUTION.md](docs/RUNTIME-DISTRIBUTION.md) |
| Runtime 支持矩阵 | [docs/RUNTIME-SUPPORT.md](docs/RUNTIME-SUPPORT.md) |

## 许可证

submux 和 Submux Runtime 使用 [MIT License](LICENSE)。完整离线包聚合分发未经修改的官方 Mihomo；Mihomo 使用 GPLv3，Release 同时提供许可证、精确来源说明和对应源码归档。
