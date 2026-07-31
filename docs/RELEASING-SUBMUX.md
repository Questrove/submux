# submux 控制面发行

submux 控制面和 Submux Runtime 独立发版。控制面使用 `submux-vX.Y.Z` Git 标签和 GitHub Release 标签，二进制内部版本为去掉 `submux-` 前缀后的 `vX.Y.Z`。Runtime 继续使用 `vX.Y.Z` 标签，并按照 Runtime 支持矩阵决定是否标记为预发布。

## 发布前检查

发布必须来自已经推送且 CI 全部通过的 `main`。先更新 README 中的控制面版本，再执行：

```sh
go vet ./...
go test ./... -count=1
bash -n scripts/*.sh
bash scripts/test-installers.sh
```

控制面发行工作流还会运行 Gitleaks、重新执行 Go 检查、构建六个平台资产、生成 SBOM、`checksums.txt` 和 `install-submux.sh`，并为全部下载文件生成 GitHub Artifact Attestation。

## 创建版本

把 `X.Y.Z` 换成准备发布的新版本：

```sh
git tag -a submux-vX.Y.Z -m "submux vX.Y.Z"
git push origin submux-vX.Y.Z
```

`.github/workflows/release-submux.yml` 只接受 `submux-vX.Y.Z`。工作流生成以下文件：

- `submux-linux-amd64`、`submux-linux-arm64`；
- `submux-darwin-amd64`、`submux-darwin-arm64`；
- `submux-windows-amd64.exe`、`submux-windows-arm64.exe`；
- `install-submux.sh`、`SBOM.spdx.json` 和 `checksums.txt`。

所有检查和构建通过后，工作流先创建草稿、上传并核对资产，最后才公开 Release 并将其标记为 Latest。若检查或构建失败，不会创建 Release；上传阶段失败可能留下可重试的草稿。已经公开的 Release 不允许工作流覆盖资产。

## 验证

Release 完成后至少核对：

```sh
gh release view submux-vX.Y.Z
gh release download submux-vX.Y.Z \
  --pattern 'install-submux.sh' \
  --pattern 'checksums.txt' \
  --pattern 'submux-linux-amd64'
sha256sum --check --ignore-missing checksums.txt
gh attestation verify ./submux-linux-amd64 --repo Questrove/submux
```

最后确认 `https://github.com/Questrove/submux/releases/latest` 指向新的控制面 Release，并分别验证在线下载和无网安装。无网测试把安装脚本、目标二进制和 `checksums.txt` 放在同一目录，再执行：

```sh
sudo bash ./install-submux.sh \
  --version submux-vX.Y.Z --offline-dir . --service
```

安装器必须在没有网络访问的情况下完成校验、版本匹配、systemd 服务安装和健康检查，并保留已有 `/var/lib/submux/submux.db`。
