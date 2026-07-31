# 发布 Submux Runtime

`.github/workflows/release.yml` 在稳定版本 tag 上构建 Runtime 产品。工作流会先验证支持矩阵和固定 Mihomo provenance，再分别在 Linux、Windows 和 macOS runner 上生成程序、产品更新 ZIP、SBOM 和在线安装包。随后使用 TUF 的 Targets、Snapshot、Timestamp 密钥签署公开仓库，验证在线仓库与离线 bundle 的选择结果和下载内容一致，最后生成完整离线安装包、Linux airgap kit、SHA-256 和 GitHub Artifact Attestation。

当前 `docs/runtime-support.json` 中六个目标都标为预览，所以 Release 会作为 GitHub prerelease 发布。Windows MSI 明确保持未签名并显示 Unknown Publisher；macOS PKG 文件名带 `_unsigned`，发行说明同时写明未签名、未公证。只有支持矩阵中不存在预览目标时，工作流才允许生成稳定 Release。

## TUF 密钥边界

初始 `root.json` 已嵌入 Runtime。Root 私钥不得进入仓库、GitHub Actions secret 或 runner。`submux-tuf bootstrap-root` 生成的私有目录还包含 Targets、Snapshot 和 Timestamp 密钥；为 CI 准备 `TUF_RELEASE_KEYS_B64` 时，只能打包以下内容：

- `manifest.json`；
- 至少两把 Targets 私钥；
- 一把 Snapshot 私钥；
- 一把 Timestamp 私钥。

归档中出现 `root-*.pem` 时工作流立即失败；发布工具还会拒绝 manifest 未列入非 Root 角色的任何额外文件，因此重命名 Root 私钥也不能进入签名过程。CI 解包后会校验每把私钥推导出的 Key ID、Root 中的角色授权和签名阈值；公开目录也会检查不能含有 PEM 或私有 manifest。Root 轮换仍是独立的离线仪式，不能借发布工作流完成。

`submux-tuf prepare-manifest` 从各平台生成的产品 descriptor、固定 Mihomo provenance 和已验摘要的上游资产组装发布清单。`submux-tuf publish` 只接受新的绝对输出目录，生成一致性快照目标、签名元数据、在线仓库和 `offline-verification-bundle.zip`。bundle 带公开 Root 供审计和归档，但 Runtime 仍从程序内嵌的初始 Root 建立信任，不能把 bundle 自带的 Root 当作新的信任起点。发布前可用 `submux-release-check tuf-parity` 对每个平台分别复核产品和 Mihomo。最终在线仓库以一次普通 Git 提交更新到 `tuf` 分支根目录，使 Runtime 固定的 `raw.githubusercontent.com/Questrove/submux/tuf/{metadata,targets}/` 地址能够直接读取。

## 供应链材料

固定 Mihomo 版本记录在 `release/mihomo-v1.19.29.json`，内容包括 tag、提交、六个平台资产摘要、GPLv3 许可证地址和源码归档摘要。工作流重新下载并验算所有文件；离线安装包同时装入二进制、provenance、GPL 文本和对应源码归档。

`release/runtime-release-policy.json` 定义历史产品名、常见秘密前缀和体积预算。CI 逐个记录候选文件与最终下载文件的字节数；程序或压缩包没有匹配到预算会直接失败。超出目标预算的预览产物只有在文件中存在带绝对上限和原因的例外时才可继续；超过例外上限仍会失败。该例外不等于稳定版预算调整。

`release/airgap-components.json` 固定 airgap 使用的稳定控制面 Release。该控制面必须先按 `submux-vX.Y.Z` 独立发布，并包含 `install-submux.sh`、两个 Linux 二进制、`checksums.txt` 和对应的 Artifact Attestation。Runtime 工作流从已经公开的控制面 Release 下载这些文件，复核摘要和 attestation，不再把 Runtime tag 下的源码重新标成同版本控制面。更新该文件属于 Runtime 发布准备的一部分；不能指向草稿、尚未发布或缺少新安装器资产的控制面版本。

最终 Release 必须包含安装包、程序、产品更新 ZIP、按平台唯一命名的 SBOM、TUF 元数据、可移植在线仓库归档、离线验证 bundle、Mihomo provenance、GPL 文本、源码归档和 `SHA256SUMS`。Runtime Release 不单独发布裸 `submux` 控制面二进制。Linux amd64、arm64 各自包含一个 `submux-airgap_<version>_linux_<arch>.tar.gz` 及伴随的 `.sha256`，其中控制面版本由上述组件文件声明。airgap 构建器只接受完整 Runtime 离线 tar 的已展开目录，核验内部文件清单后再加入经过验证的控制面和离线 TUF bundle；生成的归档必须可重复构建，顶层只能有一个目录。工作流会拒绝重名的扁平下载资产、私钥、历史 `submux-agent` 名称和常见凭据前缀，并再次运行控制面移除和本机 Runtime 安全测试。

发布前至少在 Linux runner 执行：

```sh
bash -n packaging/airgap/*.sh
bash packaging/airgap/test.sh
```

测试会核对可重复构建、离线验证、未列入清单的文件、内容篡改以及把在线 Runtime 包误装进 airgap 的拒绝行为。正式 Release 的最终门禁还检查两个架构的归档和 `.sha256` 都已进入发布目录，并按 `release/runtime-release-policy.json` 执行体积上限。

外部发布采用草稿 Release、`tuf` 分支、公开 Release 的顺序。门禁或 attestation 失败时还没有远端草稿；上传失败只留下可重试的草稿；TUF 分支更新失败时草稿不会公开。只有 TUF 提交成功后才公开下载页。已经生成的 Actions artifact 只是候选文件，不能当作正式下载页。
