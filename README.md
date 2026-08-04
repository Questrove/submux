# submux

submux 包含两个可以独立安装的产品：

| 组件 | 用途 |
|---|---|
| `submux` 控制面 | 管理机场来源、节点、模板和规则，生成 Mihomo / sing-box 输出订阅 |
| Submux Runtime | 在本机管理一个 Mihomo，提供显式代理、TUN 和 Linux 网关 |

当前控制面版本是 [`v2.1.0`](https://github.com/Questrove/submux/releases/tag/submux-v2.1.0)，属于 Stable；当前 Runtime 版本是 [`v2.1.0`](https://github.com/Questrove/submux/releases/tag/v2.1.0)，仍属于 Preview。

## 安装

### 先选择要安装什么

本文用下面三个名称表示安装目标：

| 名称 | 安装内容 |
|---|---|
| `control` | 只安装 `submux` 控制面 |
| `runtime` | 只安装 Submux Runtime、特权网络进程和 Mihomo |
| `all` | 同时安装控制面和 Runtime |

各系统目前支持的安装方式如下：

| 系统 | 在线安装 | 离线安装 | 安装后的形式 |
|---|---|---|---|
| Linux systemd / glibc | 分别安装 `control`、`runtime`；安装全部时依次执行两组命令 | airgap 包通过同一个入口选择 `control`、`runtime` 或 `all` | 控制面和 Runtime 都是 systemd 服务 |
| Windows | 分别安装控制面 `.exe` 和 Runtime 在线 MSI | 分别转移控制面 `.exe` 和 Runtime 离线 MSI；没有统一 `all` 安装器 | 只有 Runtime 是 Windows Service，控制面需要直接运行 |
| macOS 13+ | 分别安装控制面程序和 Runtime 在线 PKG | 分别转移控制面文件和 Runtime 离线 PKG；没有统一 `all` 安装器 | 只有 Runtime 是 LaunchDaemon，控制面需要直接运行 |

快速定位：[Linux](#linux) · [Windows](#windows) · [macOS](#macos)

Linux Runtime 只支持 systemd、glibc 系统。Runtime 的 Windows MSI 尚未进行 Authenticode 签名；macOS PKG 尚未签名和公证。所有 Runtime 平台目前都属于 Preview，具体缺项见 [Runtime 支持矩阵](docs/RUNTIME-SUPPORT.md)。

### Linux

#### 在线安装控制面

下面的命令安装固定版本的控制面并创建 `submux.service`：

```sh
curl -fsSL \
  https://github.com/Questrove/submux/releases/download/submux-v2.1.0/install-submux.sh |
  bash -s -- --version submux-v2.1.0 --service
```

安装后检查：

```sh
systemctl status submux
```

#### 在线安装 Runtime：Debian / Ubuntu

把 `ARCH` 改为目标架构的 `amd64` 或 `arm64`：

```sh
VERSION=2.1.0
ARCH=amd64
BASE_URL="https://github.com/Questrove/submux/releases/download/v${VERSION}"
PACKAGE="submux-runtime_${VERSION}_${ARCH}_online.deb"
CHECKSUMS="submux-runtime_${VERSION}_${ARCH}_online.sha256"

curl -fLO "${BASE_URL}/${PACKAGE}"
curl -fLO "${BASE_URL}/${CHECKSUMS}"
grep -F "  ${PACKAGE}" "${CHECKSUMS}" | sha256sum --check
sudo dpkg -i "${PACKAGE}"
```

在线包不包含 Mihomo。安装机器服务后，用同一个 Runtime 完成 TUF 验证和 Mihomo 安装：

```sh
PLAN_ID="$(sudo submux-runtime mihomo check --json |
  sed -n 's/.*"plan_id":"\([^"]*\)".*/\1/p')"
test -n "$PLAN_ID"
sudo submux-runtime mihomo install \
  --plan "$PLAN_ID" --trust tuf --confirm --wait --json
```

检查服务和本机 IPC：

```sh
systemctl status submux-runtime submux-runtime-net
sudo submux-runtime status --json
```

在线安装全部产品时，先执行“在线安装控制面”，再执行“在线安装 Runtime”。当前没有额外的在线 `all` 安装器。

#### 在线安装 Runtime：Fedora / RHEL

```sh
VERSION=2.1.0
ARCH=amd64
BASE_URL="https://github.com/Questrove/submux/releases/download/v${VERSION}"
PACKAGE="submux-runtime_${VERSION}_${ARCH}_online.rpm"
CHECKSUMS="submux-runtime_${VERSION}_${ARCH}_online.sha256"

curl -fLO "${BASE_URL}/${PACKAGE}"
curl -fLO "${BASE_URL}/${CHECKSUMS}"
grep -F "  ${PACKAGE}" "${CHECKSUMS}" | sha256sum --check
sudo rpm -Uvh "${PACKAGE}"
```

然后执行上面的 Mihomo 安装命令。

#### 在线安装 Runtime：其他 systemd / glibc Linux

这种安装方式要求目标系统已经提供支持 zstd 的 GNU tar 和 `zstd`：

```sh
VERSION=2.1.0
ARCH=amd64
BASE_URL="https://github.com/Questrove/submux/releases/download/v${VERSION}"
PACKAGE="submux-runtime_${VERSION}_${ARCH}_online.tar.zst"
CHECKSUMS="submux-runtime_${VERSION}_${ARCH}_online.sha256"

curl -fLO "${BASE_URL}/${PACKAGE}"
curl -fLO "${BASE_URL}/${CHECKSUMS}"
grep -F "  ${PACKAGE}" "${CHECKSUMS}" | sha256sum --check

RUNTIME_PACKAGE_DIR="$(mktemp -d)"
tar --zstd -xf "${PACKAGE}" -C "${RUNTIME_PACKAGE_DIR}"
sudo "${RUNTIME_PACKAGE_DIR}/install.sh"
```

然后执行上面的 Mihomo 安装命令。

#### 离线安装控制面、Runtime 或全部

Linux 离线安装推荐使用 airgap 包。它包含控制面、完整 Runtime、固定的官方 Mihomo 和离线 TUF 验证材料。目标机器需要 systemd、glibc 和常用系统工具，但不需要访问网络，也不需要 zstd。

先在能够访问 GitHub 的机器上下载与目标架构对应的两个文件：

```sh
VERSION=2.1.0
ARCH=amd64
BASE_URL="https://github.com/Questrove/submux/releases/download/v${VERSION}"
ARCHIVE="submux-airgap_${VERSION}_linux_${ARCH}.tar.gz"

curl -fLO "${BASE_URL}/${ARCHIVE}"
curl -fLO "${BASE_URL}/${ARCHIVE}.sha256"
sha256sum --check "${ARCHIVE}.sha256"
```

把这两个文件传到目标机器，然后执行：

```sh
VERSION=2.1.0
ARCH=amd64
ARCHIVE="submux-airgap_${VERSION}_linux_${ARCH}.tar.gz"
PACKAGE_DIR="submux-airgap_${VERSION}_linux_${ARCH}"

sha256sum --check "${ARCHIVE}.sha256"
tar -xzf "${ARCHIVE}"
cd "${PACKAGE_DIR}"
./install.sh verify

sudo ./install.sh all
```

最后一条命令中的 `all` 可以替换为 `control` 或 `runtime`：

```sh
sudo ./install.sh control
sudo ./install.sh runtime
sudo ./install.sh all
```

`all` 先安装控制面，再安装 Runtime。如果 Runtime 安装失败，已经成功安装的控制面会保留；排除错误后可以单独重试 `sudo ./install.sh runtime`。

### Windows

Windows 目前只有 Runtime 可以安装为系统服务。控制面 Release 提供独立程序，需要在命令行中直接运行。

#### 在线安装控制面

在 PowerShell 中执行：

```powershell
$Version = '2.1.0'
$Arch = 'amd64' # 或 arm64
$BaseUrl = "https://github.com/Questrove/submux/releases/download/submux-v$Version"
$Program = "submux-windows-$Arch.exe"

Invoke-WebRequest "$BaseUrl/$Program" -OutFile $Program
Invoke-WebRequest "$BaseUrl/checksums.txt" -OutFile checksums.txt

$Pattern = '  ' + [regex]::Escape($Program) + '$'
$Line = (Get-Content checksums.txt | Select-String $Pattern).Line
if (-not $Line) { throw "Missing checksum for $Program" }
$Expected = ($Line -split '\s+')[0].ToLowerInvariant()
$Actual = (Get-FileHash $Program -Algorithm SHA256).Hash.ToLowerInvariant()
if ($Actual -ne $Expected) { throw 'submux checksum mismatch' }

& ".\$Program"
```

离线安装控制面时，在联网机器执行下载和校验，把 `$Program` 与 `checksums.txt` 转移到目标机器，再次校验后运行。Windows 当前不会把控制面注册为 Windows Service。

#### 在线安装 Runtime

```powershell
$Version = '2.1.0'
$Arch = 'amd64' # 或 arm64
$BaseUrl = "https://github.com/Questrove/submux/releases/download/v$Version"
$Package = "submux-runtime_${Version}_${Arch}_online.msi"

Invoke-WebRequest "$BaseUrl/$Package" -OutFile $Package
Invoke-WebRequest "$BaseUrl/$Package.sha256" -OutFile "$Package.sha256"

$Expected = ((Get-Content "$Package.sha256") -split '\s+')[0].ToLowerInvariant()
$Actual = (Get-FileHash $Package -Algorithm SHA256).Hash.ToLowerInvariant()
if ($Actual -ne $Expected) { throw 'Submux Runtime MSI checksum mismatch' }

$MsiPath = (Resolve-Path $Package).Path
$Process = Start-Process msiexec.exe -Verb RunAs -Wait -PassThru -ArgumentList @(
  '/i', "`"$MsiPath`"", 'AUTHORIZE_CURRENT_USER=1'
)
if ($Process.ExitCode -notin @(0, 3010)) {
  throw "MSI installation failed with exit code $($Process.ExitCode)"
}

$Runtime = "$env:ProgramFiles\Submux Runtime\submux-runtime.exe"
$Plan = (& $Runtime mihomo check --json | Out-String | ConvertFrom-Json)
& $Runtime mihomo install --plan $Plan.plan_id --trust tuf --confirm --wait --json
```

服务器只允许管理员管理时，从 MSI 参数中删除 `AUTHORIZE_CURRENT_USER=1`，并在提升权限的 PowerShell 中运行 Runtime 命令。

#### 离线安装 Runtime

先在能够访问 GitHub 的 Windows 机器上下载离线 MSI、它的校验文件、离线验证包和总校验清单：

```powershell
$Version = '2.1.0'
$Arch = 'amd64' # 或 arm64
$BaseUrl = "https://github.com/Questrove/submux/releases/download/v$Version"
$Package = "submux-runtime_${Version}_${Arch}_offline.msi"

Invoke-WebRequest "$BaseUrl/$Package" -OutFile $Package
Invoke-WebRequest "$BaseUrl/$Package.sha256" -OutFile "$Package.sha256"
Invoke-WebRequest "$BaseUrl/offline-verification-bundle.zip" -OutFile offline-verification-bundle.zip
Invoke-WebRequest "$BaseUrl/SHA256SUMS" -OutFile SHA256SUMS
```

把这四个文件转移到目标机器，在 PowerShell 中校验并安装：

```powershell
$Version = '2.1.0'
$Arch = 'amd64'
$Package = "submux-runtime_${Version}_${Arch}_offline.msi"

$Expected = ((Get-Content "$Package.sha256") -split '\s+')[0].ToLowerInvariant()
$Actual = (Get-FileHash $Package -Algorithm SHA256).Hash.ToLowerInvariant()
if ($Actual -ne $Expected) { throw 'Submux Runtime MSI checksum mismatch' }

$Line = Get-Content .\SHA256SUMS |
  Where-Object { $_ -match '\s+\./offline-verification-bundle\.zip$' }
if (-not $Line) { throw 'Missing offline bundle checksum' }
$Expected = ($Line -split '\s+')[0].ToLowerInvariant()
$Actual = (Get-FileHash .\offline-verification-bundle.zip -Algorithm SHA256).Hash.ToLowerInvariant()
if ($Actual -ne $Expected) { throw 'Offline verification bundle checksum mismatch' }

$MsiPath = (Resolve-Path $Package).Path
$Process = Start-Process msiexec.exe -Verb RunAs -Wait -PassThru -ArgumentList @(
  '/i', "`"$MsiPath`"", 'AUTHORIZE_CURRENT_USER=1'
)
if ($Process.ExitCode -notin @(0, 3010)) {
  throw "MSI installation failed with exit code $($Process.ExitCode)"
}

$Runtime = "$env:ProgramFiles\Submux Runtime\submux-runtime.exe"
$Plan = (& $Runtime mihomo import --json .\offline-verification-bundle.zip |
  Out-String | ConvertFrom-Json)
& $Runtime mihomo install --plan $Plan.plan_id --trust tuf --confirm --wait --json
```

Windows 离线安装全部产品时，再按“在线安装控制面”一节的方式预先下载并转移控制面程序和 `checksums.txt`。Windows 没有统一的 `all` 安装器。

### macOS

macOS 目前只有 Runtime 使用 PKG 安装为 LaunchDaemon。控制面安装器只安装 `/usr/local/bin/submux`，不会创建 LaunchDaemon。

#### 在线安装控制面

```sh
curl -fsSL \
  https://github.com/Questrove/submux/releases/download/submux-v2.1.0/install-submux.sh |
  bash -s -- --version submux-v2.1.0
```

安装后直接运行：

```sh
submux
```

#### 在线安装 Runtime

```sh
VERSION=2.1.0
BASE_URL="https://github.com/Questrove/submux/releases/download/v${VERSION}"
PACKAGE="submux-runtime_${VERSION}_universal_online_unsigned.pkg"

curl -fLO "${BASE_URL}/${PACKAGE}"
curl -fLO "${BASE_URL}/${PACKAGE}.sha256"
shasum -a 256 -c "${PACKAGE}.sha256"
sudo installer -pkg "${PACKAGE}" -target /
sudo /usr/local/sbin/submux-runtime-authorize-user --confirm "$USER"

PLAN_ID="$(submux-runtime mihomo check --json |
  sed -n 's/.*"plan_id":"\([^"]*\)".*/\1/p')"
test -n "$PLAN_ID"
submux-runtime mihomo install \
  --plan "$PLAN_ID" --trust tuf --confirm --wait --json
```

在线安装全部产品时，依次执行控制面和 Runtime 的安装命令。

#### 离线安装控制面和 Runtime

先在能够访问 GitHub 的机器上下载文件。`CONTROL_ARCH` 使用 `amd64` 或 `arm64`；Runtime PKG 是 Universal 包：

```sh
CONTROL_TAG=submux-v2.1.0
CONTROL_ARCH=arm64
CONTROL_URL="https://github.com/Questrove/submux/releases/download/${CONTROL_TAG}"

curl -fLO "${CONTROL_URL}/install-submux.sh"
curl -fLO "${CONTROL_URL}/submux-darwin-${CONTROL_ARCH}"
curl -fLO "${CONTROL_URL}/checksums.txt"

RUNTIME_VERSION=2.1.0
RUNTIME_URL="https://github.com/Questrove/submux/releases/download/v${RUNTIME_VERSION}"
RUNTIME_PACKAGE="submux-runtime_${RUNTIME_VERSION}_universal_offline_unsigned.pkg"

curl -fLO "${RUNTIME_URL}/${RUNTIME_PACKAGE}"
curl -fLO "${RUNTIME_URL}/${RUNTIME_PACKAGE}.sha256"
curl -fLO "${RUNTIME_URL}/offline-verification-bundle.zip"
curl -fLO "${RUNTIME_URL}/SHA256SUMS"
```

只安装其中一个产品时，只需要转移对应的那组文件。把文件转移到目标 Mac 后执行：

```sh
CONTROL_TAG=submux-v2.1.0
CONTROL_ARCH=arm64
CONTROL_PROGRAM="submux-darwin-${CONTROL_ARCH}"
grep "  ${CONTROL_PROGRAM}$" checksums.txt | shasum -a 256 -c -
sudo bash ./install-submux.sh \
  --version "$CONTROL_TAG" --offline-dir .

RUNTIME_VERSION=2.1.0
RUNTIME_PACKAGE="submux-runtime_${RUNTIME_VERSION}_universal_offline_unsigned.pkg"
shasum -a 256 -c "${RUNTIME_PACKAGE}.sha256"
grep '  ./offline-verification-bundle.zip$' SHA256SUMS |
  shasum -a 256 -c -
sudo installer -pkg "${RUNTIME_PACKAGE}" -target /
sudo /usr/local/sbin/submux-runtime-authorize-user --confirm "$USER"

PLAN_ID="$(submux-runtime mihomo import --json ./offline-verification-bundle.zip |
  sed -n 's/.*"plan_id":"\([^"]*\)".*/\1/p')"
test -n "$PLAN_ID"
submux-runtime mihomo install \
  --plan "$PLAN_ID" --trust tuf --confirm --wait --json
```

macOS 没有统一的 `all` 安装器。安装全部产品时执行上面两组安装命令；只安装一个产品时跳过另一组。

### 桌面用户授权

服务器安装默认只允许 root 或管理员管理 Runtime。需要让本机桌面用户使用 GUI、TUI 或 CLI 时，显式授权该用户：

Linux：

```sh
sudo submux-runtime-authorize-user --confirm "$USER"
```

Linux 用户需要重新登录以取得新的组身份。macOS 使用：

```sh
sudo /usr/local/sbin/submux-runtime-authorize-user --confirm "$USER"
```

Windows MSI 安装时加入 `AUTHORIZE_CURRENT_USER=1`。

## 安装后开始使用

### 打开控制面

Linux systemd 服务默认监听 `127.0.0.1:8080`。首次打开 <http://127.0.0.1:8080> 设置管理员密码，然后按“来源 → 节点库 → 模板 → 规则 → 输出订阅”创建输出订阅。

Windows 和 macOS 当前需要在终端中直接运行 `submux` 控制面程序。

### 给 Runtime 添加第一个配置来源

在线环境可以把控制面输出订阅添加为 Runtime 来源：

```sh
submux-runtime source add \
  --type submux_output \
  --name submux-main \
  --url 'https://sub.example.com/sub/replace-with-token' \
  --wait --json

submux-runtime source list --json
SOURCE_ID='把 source add 输出中的 source_id 填在这里'
submux-runtime source apply --wait --json "$SOURCE_ID"
submux-runtime proxy start --wait --json
submux-runtime proxy verify --json
```

完全离线时，先准备一份完整 Mihomo 配置文件，再导入本机副本：

```sh
submux-runtime source import --name local-config --wait --json ./config.yaml
submux-runtime source list --json
SOURCE_ID='把 source import 输出中的 source_id 填在这里'
submux-runtime source apply --wait --json "$SOURCE_ID"
submux-runtime proxy start --wait --json
```

添加、刷新或导入来源都不会自动启动 Mihomo。只有显式执行 `proxy start` 后才开始代理。

## 安装和更新说明

### 在线包与离线包

Runtime 安装包都在 [v2.1.0 Release](https://github.com/Questrove/submux/releases/tag/v2.1.0)：

| 系统 | 在线包 | 完整离线包 |
|---|---|---|
| Debian / Ubuntu | `submux-runtime_2.1.0_<arch>_online.deb` | `submux-runtime_2.1.0_<arch>_offline.deb` |
| Fedora / RHEL | `submux-runtime_2.1.0_<arch>_online.rpm` | `submux-runtime_2.1.0_<arch>_offline.rpm` |
| 其他 systemd / glibc Linux | `submux-runtime_2.1.0_<arch>_online.tar.zst` | `submux-runtime_2.1.0_<arch>_offline.tar.zst` |
| Windows | `submux-runtime_2.1.0_<arch>_online.msi` | `submux-runtime_2.1.0_<arch>_offline.msi` |
| macOS 13+ | `submux-runtime_2.1.0_universal_online_unsigned.pkg` | `submux-runtime_2.1.0_universal_offline_unsigned.pkg` |

在线包不包含 Mihomo。首次配置时，Runtime 通过 TUF 元数据验证并从固定的 `MetaCubeX/mihomo` 官方 Release 下载匹配的核心。

完整离线包包含固定 Mihomo、TUF 元数据、SBOM、许可证和对应源码。当前 v2.1.0 的独立离线包还需要 `offline-verification-bundle.zip` 作为首次激活 Mihomo 的导入文件；Linux airgap 包已经把它包含在归档中，并由顶层安装器自动完成导入和激活。

Linux airgap 包包含独立发布的稳定控制面和完整 Runtime，但两个产品仍然分别安装。归档中的 `AIRGAP-METADATA` 会记录各自版本。

### 系统依赖和平台限制

- Linux Runtime 支持 systemd、glibc 发行版；Alpine、OpenWrt、musl 和非 systemd 系统当前不在支持范围内。
- Linux arm64 包当前只提供 CLI 和 TUI，不包含 GUI。
- Windows Runtime 需要受支持的 Windows 版本和系统 WebView2；MSI 当前未签名。
- macOS Runtime 要求 macOS 13 或更高版本；PKG 当前未签名且未公证。
- 离线包不包含 WebView2、WebKitGTK、内核模块、systemd、glibc 或包管理器等操作系统组件。

### 状态保留、修复和降级

控制面安装器默认保留 `/var/lib/submux/submux.db`。Runtime 安装器默认保留配置和状态目录。重装、升级或卸载不会自动删除用户状态。

airgap 包同版本修复 Runtime 使用：

```sh
sudo ./install.sh runtime --repair
```

降级需要同时确认版本降级和数据库兼容：

```sh
sudo ./install.sh runtime --allow-downgrade --database-compatible
```

各平台安装器的修复、卸载和清理参数见 [Runtime 安装与发行说明](docs/RUNTIME-DISTRIBUTION.md)。

### 下载验证

安装示例都会先检查 Release 提供的 SHA-256。联网环境如果已经安装 GitHub CLI，还可以额外核对 GitHub Artifact Attestation：

```sh
gh attestation verify ./submux-runtime_2.1.0_amd64_online.deb \
  --repo Questrove/submux
```

这项检查是可选的，不用于下载，也不是安装前提。Runtime 对 Mihomo 的在线和离线安装还会执行 TUF 签名、元数据版本、有效期、平台、架构、文件大小和 SHA-256 检查。

## 产品说明

### 控制面怎样工作

```text
机场来源 ─刷新─┐
              ├─> 规范化节点库 ─选择节点─┐
手工分享链接 ─┘                           ├─> 输出订阅 ─> /sub/{token}
完整配置模板 ─> 不可变模板版本 ───────────┤
规则方案 ─────> MetaCubeX 规则目录 ───────┘
```

控制面可以读取 Mihomo YAML、明文或 Base64 分享链接，导入 VLESS、VMess、Trojan、Shadowsocks 和 Hysteria2 节点，并按节点语义指纹去重。输出订阅由固定模板版本、规则方案和选中节点生成；编译失败时保留最近可用产物。

控制面使用 bbolt 单文件存储，不运行代理核心，也不远程控制 Runtime。

### Runtime 怎样工作

Runtime 由以下程序组成：

- `submux-runtime`：低权限机器服务，同时提供 CLI 和 TUI；
- `submux-runtime-net`：只执行固定网络操作的特权网络进程；
- `submux-runtime-gui`：可选桌面客户端；
- Mihomo：由 Runtime 验证、启动、监控和回滚的官方代理核心。

一台机器或一个网络命名空间只允许一个 Runtime 和一个受管 Mihomo。GUI、TUI 和 CLI 只通过本机 Unix Socket 或 Windows Named Pipe 管理 Runtime。Runtime 不注册设备、不发送心跳，也不接受控制面发起的运行操作。

Runtime 可以保存多个来源，包括控制面输出订阅、外部 HTTP(S) 完整配置和本机导入副本，但任意时刻只有一个当前来源。本机监听、控制端点、运行方式、路由、DNS、网关和数据路径始终由 Runtime 管理。

显式代理不修改系统网络。TUN 或 Linux 网关发生故障时，Runtime 会撤销自己创建的网络设置并恢复直连。TUN 和网关必须先执行 `network preview`，检查计划后再明确启用。

## 控制面开发和配置

### 从源码运行

需要 Go 1.26.1 或 `go.mod` 指定的更新版本：

```sh
go test ./...
CGO_ENABLED=0 go build -o submux ./cmd/submux
SUBMUX_DB=submux.db ./submux
```

### 常用配置

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
- Runtime 外部管理接口只存在于本机 Socket 或 Named Pipe，不监听 TCP；
- Runtime 配置来源不能修改保留监听、系统网络、程序下载地址或任意本机文件；
- Runtime 不发送遥测、崩溃报告或后台诊断，诊断包只在本机由操作员明确生成。

## 文档

| 用途 | 文档 |
|---|---|
| Runtime 安装、更新、离线包与发行 | [docs/RUNTIME-DISTRIBUTION.md](docs/RUNTIME-DISTRIBUTION.md) |
| Runtime 当前支持状态 | [docs/RUNTIME-SUPPORT.md](docs/RUNTIME-SUPPORT.md) |
| Runtime 总体设计 | [docs/RUNTIME.md](docs/RUNTIME.md) |
| Runtime 本机 IPC | [docs/RUNTIME-IPC.md](docs/RUNTIME-IPC.md) |
| TUN、Linux 网关与权限边界 | [docs/RUNTIME-NETWORK.md](docs/RUNTIME-NETWORK.md) |
| 控制面领域模型与发布语义 | [docs/DESIGN.md](docs/DESIGN.md) |
| 支持的节点协议 | [docs/PROTOCOLS.md](docs/PROTOCOLS.md) |
| 机场生命周期 | [docs/LIFECYCLE.md](docs/LIFECYCLE.md) |
| 控制面发行 | [docs/RELEASING-SUBMUX.md](docs/RELEASING-SUBMUX.md) |
| Runtime 发行 | [docs/RELEASING-RUNTIME.md](docs/RELEASING-RUNTIME.md) |

## 许可证

submux 和 Submux Runtime 使用 [MIT License](LICENSE)。完整离线包聚合分发未经修改的官方 Mihomo；Mihomo 使用 GPLv3，Release 同时提供许可证、精确来源说明和对应源码归档。
