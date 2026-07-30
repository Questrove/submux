# Runtime 更新的 TUF 信任与校验（官方资料，2026-07-30）

## 初始信任根与 Root 轮换

TUF 客户端必须通过带外方式取得最初信任的 Root 元数据。客户端更新 Root 时，应按当前可信版本的下一个连续版本逐份下载；每一份新 Root 同时通过旧 Root 和新 Root 各自规定的签名阈值校验，版本号必须恰好递增，并在成功后立即保存。轮换结束时还要检查最后一份 Root 是否过期。[TUF 规范 1.0.35](https://theupdateframework.github.io/specification/latest/)

这意味着 Submux 二进制里只能嵌入已经完成签名仪式的公开 `root.json`。生成一份临时 Root、把私钥放进仓库，或者在首次联网时自行信任远端 Root，都会破坏初始信任的边界。Root 私钥的生成、保管和轮换需要作为发布流程单独完成，客户端代码不应持有这些私钥。

## 元数据与目标文件校验

标准客户端工作流固定一次刷新所使用的参考时间，依次校验 timestamp、snapshot 和 targets 元数据的签名阈值、过期时间、版本回退以及元数据长度和摘要；下载目标文件时还要校验 Targets 中声明的长度和摘要。[TUF 规范](https://github.com/theupdateframework/specification/blob/master/tuf-spec.md)

本仓库使用 `github.com/theupdateframework/go-tuf/v2` 的 updater 实现这一流程。在线更新把元数据地址固定到 Submux 官方 TUF 仓库，并把候选目标进一步限制为 MetaCubeX/mihomo 官方 Release 的平台资产；离线包通过自定义 Fetcher 向同一个 `Updater.Refresh` 和目标下载流程提供字节，不使用 `UnsafeLocalMode`，因此离线导入不会绕开阈值、过期、回退、长度或摘要检查。[go-tuf v2](https://github.com/theupdateframework/go-tuf)

Targets 的自定义字段还必须声明：

- `kind` 为 `mihomo`；
- `repository` 为 `MetaCubeX/mihomo`；
- 目标路径与版本、操作系统、架构和官方资产名一致；
- TUF 声明的 SHA-256 与上游 Release checksum 一致。

`upstream_only` 只验证固定的 MetaCubeX/mihomo Release、资产名、下载主机和上游 checksum。它不获得 TUF 的回退、撤销和阈值签名保护，所以每次都必须显示警告并要求操作者单独确认；不能用于自动更新、预加载，也不能作为离线包的替代验证路径。

## 对实现和发布流程的约束

1. 客户端必须嵌入真实签名且公开可审计的初始 Root；没有初始 Root 时，TUF 更新入口应明确报告不可用，不能自动降级到 `upstream_only`。
2. 在线和离线来源共用同一个 go-tuf verifier；离线 ZIP 只负责提供受限的元数据和目标字节。
3. 通过 TUF 后仍须由候选 Mihomo 对当前候选配置做静态检查。替换后若进程无法启动或即时健康检查失败，应恢复 previous 版本并重新启动。
4. Runtime 只保留 current 和 previous 两个已验证版本；安装与回滚都由带调用者、信任级别和有效期绑定的计划触发，并要求明确确认。
5. 发布侧需要在仓库外保管 Root、Targets、Snapshot 和 Timestamp 私钥，生成并签署元数据后只发布公开元数据、目标文件及审计材料。私钥保管方式必须由项目所有者确认，不能由开发代码自行决定。
