# Submux Runtime TUI D 方案验收记录

记录日期：2026-08-03

## 验收边界

TUI、GUI 和 CLI 只通过 Runtime 本机 IPC 管理状态。TUI 不直接连接 Mihomo，也不在运行中删除 Runtime 服务。卸载流程由平台卸载器负责：先创建可恢复备份、停用网络接管并确认恢复直连、退出 TUI，再运行卸载器；默认保留 Runtime 状态，只有明确选择 purge 才清除。

## 键盘流程

| 流程 | 操作与预期 | 自动化证据 | 人工检查 |
| --- | --- | --- | --- |
| 首次配置 | 状态页按 Snapshot 显示六步；安装核心、添加来源、生成候选、预览运行方式、启动均复用现有预览和统一确认 | `TestFirstRunGuideIsDerivedFromSnapshotAndDisappearsWhenReady`、`TestFirstRunCoreStepOnlyPreviewsBeforeUnifiedConfirmation`、`TestFirstRunGuideRoutesSourceCandidateModeAndStartThroughExistingSafetyFlows` | 通过 |
| 来源切换 | 配置页用 ↑↓ 选择，`t` 进入统一确认，确认前不提交 | `TestDPlanKeyboardAcceptanceJourney`、`TestRuntimeTUISourceSwitchConfirmationExplainsTransactionOrder` | 通过 |
| 网络预览 | 网络页用 `Ctrl+P`、`Ctrl+T`、`Ctrl+L` 打开结构化表单；预览显示实际状态、预期结果、DNS、路由、冲突和恢复动作 | `TestDPlanKeyboardAcceptanceJourney`、`TestRuntimeTUIPreviewShowsActualExpectedDNSResidualsAndRecovery` | 通过 |
| 流量监控 | 监控页显示当前速度、本次运行累计、速度曲线和活动连接；`[`、`]` 切换 1/5/15 分钟范围 | `TestDPlanKeyboardAcceptanceJourney`、`TestRuntimeTUIMonitorLoadsFiveMinuteTrafficHistory`、`TestRuntimeTUIMonitorChangesRangeAndPreservesStaleData` | 通过 |
| 节点选择 | `Ctrl+N` 打开代理组和节点；切换节点及节点/组/来源延迟测试先进入统一确认 | `TestDPlanKeyboardAcceptanceJourney`、`TestProxyGroupViewerUsesUnifiedConfirmationBeforeSubmittingSelection`、`TestProxyGroupViewerPreparesFixedLatencyScopes` | 通过 |
| 日志查看 | `l` 打开默认最近 200 条脱敏日志，支持分页、跟随、暂停和筛选 | `TestDPlanKeyboardAcceptanceJourney`、`TestMaintenanceLogViewerLoadsLatestAndOlderPages`、`TestLogViewerFiltersAndKeepsStaleDataOnReadFailure` | 通过 |
| 备份恢复 | `B` 先预览完整明文备份，再输入新文件；`L` 先检查备份，确认后整体恢复 | `TestDPlanKeyboardAcceptanceJourney`、`TestModelCreatesAndRestoresBackupWithExplicitConfirmation` | 通过 |
| 卸载 | 帮助页明确 TUI 不自删除；先备份、恢复直连、`q` 退出，再执行网页列出的平台卸载命令 | `TestDPlanKeyboardAcceptanceJourney`、`TestUserGuideMatchesRuntimeTUIWorkflowAndSafetyBoundary` | 通过 |

## 故障降级

| 场景 | 预期 | 自动化证据 | 结果 |
| --- | --- | --- | --- |
| Runtime 本机 IPC 中断 | 保留最后一次 Snapshot，标记数据过期并退避重连；恢复后先读取完整 Snapshot | `TestRuntimeTUIKeepsStaleDataAndResynchronizesAfterDisconnect`、`TestDPlanDegradedPartitionsRemainNavigable` | 通过 |
| Mihomo 重启或崩溃恢复 | Snapshot 展示实际/期望状态、重试次数、下次重试和故障；页面、焦点及草稿不因状态更新丢失 | `TestModelUsesOneClientForImportPreviewApplyStartStopAndWait`、`TestDPlanDegradedPartitionsRemainNavigable` | 通过 |
| 运行操作结果不确定 | IPC 在运行操作期间断开时标记“结果不确定”，不自动重放提交；重连后由权威 Snapshot 和操作记录核对 | `TestRuntimeTUIConnectionLossMarksRunningOperationOutcomeUncertain`、`TestRuntimeTUIDoesNotReplayUncertainOperationSubmission` | 通过 |
| 分区接口失败 | 流量、活动连接或日志单独失败时保留该区域最后数据并标记过期，其他页面继续可用 | `TestRuntimeTUIMonitorChangesRangeAndPreservesStaleData`、`TestRuntimeTUIConnectionFailureKeepsLastDataAndStopsWhenHidden`、`TestLogViewerFiltersAndKeepsStaleDataOnReadFailure`、`TestDPlanDegradedPartitionsRemainNavigable` | 通过 |

## 终端兼容矩阵

| 终端 | 预期布局与能力 | 自动化检查 | 人工检查 |
| --- | --- | --- | --- |
| 120×30 及以上，TrueColor | 完整双栏，五页所有区域存在，不做高度裁剪 | `TestWideLayoutKeepsEveryPageRegionVisible` | 通过 |
| 80–119 列，ANSI 16 色 | 单栏堆叠，当前焦点展开，其他区域保留可切换标题 | `TestTerminalLayoutsKeepFocusedRegionVisible`、`TestTerminalCapabilitiesSupportNoColorANSI16AndASCII` | 通过 |
| 80×24，`NO_COLOR` | 五页均不越界；表单当前字段和确认框保持可见；不输出颜色转义 | `TestRecommendedMinimumRendersEveryPageWithin80x24`、`TestRecommendedMinimumKeepsActiveLongFormFieldVisible`、`TestRecommendedMinimumKeepsCompleteConfirmationActionable` | 通过 |
| 60–79 列，ASCII | 只显示当前焦点区域；图表、方向、状态和列表标记使用 ASCII 替代 | `TestTerminalLayoutsKeepFocusedRegionVisible`、`TestTerminalCapabilitiesSupportNoColorANSI16AndASCII` | 通过 |
| 小于 60×18 | 只保留状态摘要、尺寸提示、命令搜索、帮助和退出；隐藏操作键不会在后台执行 | `TestMinimumTerminalKeepsSummarySearchHelpAndQuit`、`TestMinimumTerminalOnlyAcceptsSearchHelpAndQuit`、`TestTinyTerminalNeverExceedsHeight` | 通过 |
| 运行中调整尺寸 | 页面、页内焦点、列表选择、筛选、查看器和未提交表单不丢失 | `TestResizePreservesNavigationSelectionsFiltersAndDrafts` | 通过 |

人工检查采用测试生成的 120×30、100×24、80×24、70×24 和 59×17 纯文本渲染，逐页核对标题、焦点、操作提示、截断标记和统一确认。检查过程中发现维护页宽屏多出一行，已压缩页面提示并重新运行矩阵；最终所有尺寸通过。

## 网页使用说明

`web/user-guide.html` 按安装、首次使用、日常操作、更新与备份、卸载和排查的顺序编写，并包含最终五页 TUI、当前快捷键、统一确认和故障处理说明。目录和正文使用同一个布局起点，不再依靠负外边距与重复的 52 像素偏移。

“返回配置编排台”只在本页由同一 submux 控制面提供，且同源 `/healthz` 返回有效构建信息时显示；目标 `/` 是该服务的配置编排页面。独立打开静态说明或只安装 Runtime 时，该入口保持隐藏。

浏览器人工检查覆盖桌面、760 像素和 390 像素视口。桌面下目录卡片与第一节正文的上边缘一致；两种窄屏下目录位于正文之前，页面没有横向溢出，长命令和宽表格只在各自容器内滚动；独立静态服务中“返回配置编排台”保持隐藏。

对应检查：`TestUserGuideIsEmbeddedAndCoversProductLifecycle`、`TestUserGuideAlignsTheContentsAndOnlyShowsARealControlPlaneLink`、`TestUserGuideMatchesRuntimeTUIWorkflowAndSafetyBoundary`。

## 执行结果

以下结果由主验收会话在 `C:\Users\Sdata\code\submux` 执行，Go 工具链为 `C:\Users\Sdata\sdk\go1.26.1\bin\go.exe`。结果记录的是本次变更完成后的实际退出状态；终端渲染和浏览器检查的范围分别见上面的兼容矩阵与网页说明。

```text
C:/Users/Sdata/sdk/go1.26.1/bin/go.exe test ./internal/runtimetui ./web -count=1    PASS
C:/Users/Sdata/sdk/go1.26.1/bin/go.exe test ./...                                   PASS
C:/Users/Sdata/sdk/go1.26.1/bin/go.exe vet ./...                                    PASS
git diff --check                                                                    PASS
```

Linux DEB、RPM 和 tar 的真实安装、授权、修复、保留状态卸载、purge 和网络残留检查由 `packaging/linux/integration-test.sh` 在带 systemd、glibc 和 root 权限的发布环境执行。Windows 和 macOS 的卸载命令与保留状态约定分别以 `packaging/windows/README.md` 和 `packaging/macos/README.md` 为准。
