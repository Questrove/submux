package runtimetui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func (m Model) View() tea.View {
	if m.sourceForm != nil {
		return tea.NewView(strings.Join([]string{
			m.renderShellHeader(),
			m.renderTabs(),
			"",
			m.renderSourceForm(),
			"",
			m.renderShellFooter(),
		}, "\n"))
	}
	if m.sourceDiagnosticOpen {
		return tea.NewView(strings.Join([]string{
			m.renderShellHeader(),
			m.renderTabs(),
			"",
			m.renderSourceDiagnostics(),
			"",
			m.renderShellFooter(),
		}, "\n"))
	}
	if m.networkForm != nil {
		return tea.NewView(strings.Join([]string{
			m.renderShellHeader(),
			m.renderTabs(),
			"",
			m.renderNetworkForm(),
			"",
			m.renderShellFooter(),
		}, "\n"))
	}
	if m.connectionFilter != nil {
		return tea.NewView(strings.Join([]string{
			m.renderShellHeader(),
			m.renderTabs(),
			"",
			m.renderConnectionFilter(),
			"",
			m.renderShellFooter(),
		}, "\n"))
	}
	if m.trafficPolicyEditing {
		return tea.NewView(strings.Join([]string{
			m.renderShellHeader(),
			m.renderTabs(),
			"",
			m.renderTrafficPolicyEditor(),
			"",
			m.renderShellFooter(),
		}, "\n"))
	}
	if m.editing {
		return m.legacyView()
	}

	lines := []string{
		m.renderShellHeader(),
		m.renderTabs(),
		"",
	}
	if m.paletteOpen {
		lines = append(lines, m.renderPalette())
	} else if m.showHelp {
		lines = append(lines, m.renderHelp())
	} else {
		switch m.page {
		case pageConfig:
			lines = append(lines, m.renderConfigPage())
		case pageNetwork:
			lines = append(lines, m.renderNetworkPage())
		case pageMonitor:
			lines = append(lines, m.renderMonitorPage())
		case pageMaintenance:
			lines = append(lines, m.renderMaintenancePage())
		default:
			lines = append(lines, m.renderStatusPage())
		}
	}
	if m.confirmation != nil {
		lines = append(lines, "", m.renderConfirmation())
	}
	lines = append(lines,
		"",
		m.renderShellFooter(),
	)
	return tea.NewView(strings.Join(lines, "\n"))
}

func (m Model) renderShellHeader() string {
	state := okStyle.Render("● 已同步")
	if m.snapshotStale {
		state = warnStyle.Render("● 数据已过期 · 正在重连")
	} else if m.snapshot.ProtocolVersion == 0 {
		state = warnStyle.Render("● 正在连接")
	}
	return fmt.Sprintf(
		"%s  %s\n%s  revision %d  local IPC",
		titleStyle.Render("Submux Runtime"),
		state,
		valueOr(m.snapshot.Runtime.Version, "未知版本"),
		m.snapshot.Revision,
	)
}

func (m Model) renderTabs() string {
	labels := make([]string, 0, len(pageDefinitions)+2)
	for _, page := range pageDefinitions {
		label := page.number + " " + page.title
		if m.page == page.id {
			label = "[" + label + "]"
		}
		labels = append(labels, label)
	}
	labels = append(labels, "/ 搜索", "? 帮助")
	return strings.Join(labels, "   ")
}

func (m Model) renderStatusPage() string {
	nextRestart := "无"
	if m.snapshot.Mihomo.NextRestartAt != nil {
		nextRestart = m.snapshot.Mihomo.NextRestartAt.Local().Format(time.RFC3339)
	}
	lines := []string{
		m.focusHeading(0, "运行状态"),
		fmt.Sprintf("Mihomo 实际 / 期望  %s / %s", valueOr(m.snapshot.Mihomo.State, "未知"), valueOr(m.snapshot.Mihomo.DesiredState, "未设置")),
		fmt.Sprintf("崩溃恢复              %s", valueOr(m.snapshot.Mihomo.Recovery, "未知")),
		fmt.Sprintf("运行方式              %s", valueOr(m.snapshot.RunMode, "未配置")),
		fmt.Sprintf("网络接管              %s · %s%s", valueOr(m.snapshot.Network.State, "未知"), valueOr(m.snapshot.Network.Mode, "未配置"), networkDeviceSuffix(m.snapshot.Network.Device)),
		fmt.Sprintf("重试次数 / 下次重试   %d / %s", m.snapshot.Mihomo.CrashAttempts, nextRestart),
	}
	if m.snapshot.Mihomo.Fault != nil {
		lines = append(lines, errorStyle.Render("Mihomo 故障："+m.snapshot.Mihomo.Fault.Code+" · "+m.snapshot.Mihomo.Fault.Message))
	}
	if m.snapshot.Network.Fault != nil {
		lines = append(lines, errorStyle.Render("网络故障："+m.snapshot.Network.Fault.Code+" · "+m.snapshot.Network.Fault.Message))
	}

	lines = append(lines, "", m.focusHeading(1, "当前来源"))
	if source := m.currentSourceSummary(); source != nil {
		lines = append(lines,
			fmt.Sprintf("%s · %s · %s", source.Name, source.Type, valueOr(source.LastRefreshResult, "尚未刷新")),
			fmt.Sprintf("目标 %s · 路线 %s", valueOr(source.RedactedTarget, "本机保存"), valueOr(source.Route, "默认")),
		)
	} else {
		lines = append(lines, mutedStyle.Render("尚未选择 Runtime 配置来源"))
	}

	lines = append(lines, "", m.focusHeading(2, "最近运行操作"), m.operationLine())
	return strings.Join(lines, "\n")
}

func (m Model) renderConfigPage() string {
	lines := []string{m.focusHeading(0, "配置来源")}
	if len(m.snapshot.Sources.Items) == 0 {
		lines = append(lines, mutedStyle.Render("尚未添加 Runtime 配置来源"))
	}
	for _, source := range m.snapshot.Sources.Items {
		selection := "  "
		if source.ID == m.selectedSourceID {
			selection = "▶ "
		}
		current := ""
		if source.Current || source.ID == m.snapshot.Sources.CurrentSourceID {
			current = " [当前]"
		}
		line := fmt.Sprintf("%s%s%s · %s · %s · %s · %s", selection, source.Name, current, source.Type, source.ID, source.RedactedTarget, source.Route)
		if source.LastRefreshResult != "" {
			line += " · " + source.LastRefreshResult
		}
		if len(source.HighRiskSettings) > 0 {
			line += " · 高风险：" + strings.Join(source.HighRiskSettings, "、")
		}
		lines = append(lines, line)
	}
	selectedName := "未选择"
	if source := m.selectedSourceSummary(); source != nil {
		selectedName = source.Name + " · " + source.Type + " · " + source.ID
	}
	currentName := "未提交"
	if source := m.currentSourceSummary(); source != nil {
		currentName = source.Name + " · " + source.Type + " · " + source.ID
	}
	lines = append(lines,
		"浏览中的所选来源  "+selectedName,
		"Runtime 已提交的当前来源  "+currentName,
		mutedStyle.Render("Enter 生成候选，不会切换当前来源"),
	)

	lines = append(lines, "", m.focusHeading(1, "候选配置"))
	if m.preview.CandidateSHA256 == "" {
		lines = append(lines, mutedStyle.Render("选择来源后按 Enter 或 y 生成候选配置"))
	} else {
		lines = append(lines, fmt.Sprintf("%s · %s · %s", shortDigest(m.preview.CandidateSHA256), m.preview.ProxyKind, strings.Join(m.preview.ProxyAddresses, ", ")))
		for _, origin := range m.preview.FieldOrigins {
			lines = append(lines, fmt.Sprintf("%s · %s · %s", origin.Path, origin.Origin, origin.Status))
		}
	}

	lines = append(lines, "", m.focusHeading(2, "本机配置层"))
	policy := m.snapshot.TrafficPolicy
	policyLine := fmt.Sprintf(
		"流量策略 · 当前 %s · 字段来源 %s · 实际应用 %s",
		trafficPolicyLabel(policy.Selection),
		trafficPolicyOriginLabel(policy.FieldOrigin),
		trafficPolicyLabel(policy.Applied),
	)
	if policy.Selection != "" && policy.Selection != runtimeapi.TrafficPolicyFollowSource && policy.Applied != "" && policy.Selection != policy.Applied {
		policyLine += " · 待应用"
	}
	lines = append(lines, policyLine)
	if m.snapshot.AdvancedOverride.Present {
		lines = append(lines, fmt.Sprintf("高级覆盖 · %s · %d 字节", shortDigest(m.snapshot.AdvancedOverride.SHA256), m.snapshot.AdvancedOverride.Size))
	} else {
		lines = append(lines, "高级覆盖 · 未设置")
	}
	for _, resource := range m.snapshot.Resources.Items {
		lines = append(lines, fmt.Sprintf("%s · %s · %s · %d 字节", resource.ID, resource.Kind, resource.Name, resource.Size))
	}

	lines = append(lines,
		"",
		m.focusHeading(3, "配置操作"),
		"u 添加远程来源 · n 添加本机来源 · y 生成候选 · f/d/m 刷新 · t/k 切换 · o 高级覆盖 · e 托管资源 · Ctrl+Y 流量策略 · Ctrl+U 只读诊断",
	)
	return strings.Join(lines, "\n")
}

func (m Model) renderNetworkPage() string {
	networkMode := valueOr(m.snapshot.Network.Mode, "未配置")
	if m.snapshot.Network.PreviewOnly {
		networkMode += "（预览）"
	}
	lines := []string{
		m.focusHeading(0, "当前网络"),
		fmt.Sprintf("状态 %s · 方式 %s%s · 特权服务 %t", valueOr(m.snapshot.Network.State, "未知"), networkMode, networkDeviceSuffix(m.snapshot.Network.Device), m.snapshot.Network.Available),
	}
	for _, conflict := range m.snapshot.Network.Conflicts {
		lines = append(lines, errorStyle.Render(fmt.Sprintf("网络冲突：%s · %s · %s", conflict.Kind, valueOr(conflict.Owner, "未知所有者"), conflict.Detail)))
	}
	for _, residual := range m.snapshot.Network.Residuals {
		lines = append(lines, errorStyle.Render(fmt.Sprintf("网络残留：%s · %s · %s", residual.Kind, residual.Name, residual.State)))
	}
	if settings := m.snapshot.Network.GatewaySettings; settings != nil {
		lines = append(lines, fmt.Sprintf("网关设置 · IPv6 %s · DNS %s · TCP %t · UDP %t · 主机 %t", settings.IPv6Policy, settings.DNSPolicy, settings.CaptureTCP, settings.CaptureUDP, settings.ProxyHostTraffic))
	}

	lines = append(lines, "", m.focusHeading(1, "网络预览"))
	if m.networkPreview.PlanID == "" {
		lines = append(lines,
			fmt.Sprintf("当前实际  %s · %s%s", valueOr(m.snapshot.Network.Mode, "未知"), valueOr(m.snapshot.Network.State, "未知"), networkDeviceSuffix(m.snapshot.Network.Device)),
			mutedStyle.Render("尚未生成网络预览；选择普通 TUN 或 Linux 网关后先读取真实网络状态"),
		)
	} else {
		previewMode := m.networkPreview.Mode
		if m.networkPreview.PreviewOnly {
			previewMode += "（预览）"
		}
		lines = append(lines,
			fmt.Sprintf("当前实际  %s · %s%s", valueOr(m.snapshot.Network.Mode, "未知"), valueOr(m.snapshot.Network.State, "未知"), networkDeviceSuffix(m.snapshot.Network.Device)),
			fmt.Sprintf("预期结果  %s · %s%s", previewMode, runtimeapi.NetworkStateActive, networkDeviceSuffix(m.networkPreview.Device)),
			fmt.Sprintf("DNS 变化  %s", previewDNSPolicy(m.networkPreview)),
			fmt.Sprintf("预览计划  %s · 观测 %s · 到期 %s", m.networkPreview.PlanID, formatPreviewTime(m.networkPreview.ObservedAt), formatPreviewTime(m.networkPreview.ExpiresAt)),
		)
		for _, route := range m.networkPreview.Routes {
			disposition := "绕过"
			if !route.Bypass {
				disposition = "接管"
			}
			lines = append(lines, fmt.Sprintf("%s · %s · %s · %s · %s", route.ID, route.CIDR, route.Interface, route.Role, disposition))
		}
		if settings := m.networkPreview.GatewaySettings; settings != nil {
			if len(settings.DNSDirectCIDRs) > 0 {
				lines = append(lines, "DNS 直连网段 · "+strings.Join(settings.DNSDirectCIDRs, "、"))
			}
			for index, exception := range settings.UDPExceptions {
				lines = append(lines, fmt.Sprintf("UDP 直连例外 %d · %s", index+1, formatGatewayException(exception)))
			}
			for index, exception := range settings.HostExceptions {
				lines = append(lines, fmt.Sprintf("本机直连例外 %d · %s", index+1, formatGatewayException(exception)))
			}
		}
		for _, conflict := range m.networkPreview.Conflicts {
			lines = append(lines, errorStyle.Render("冲突预览："+conflict.Detail))
		}
		for _, warning := range m.networkPreview.Warnings {
			lines = append(lines, warnStyle.Render("警告："+warning))
		}
		for _, residual := range m.snapshot.Network.Residuals {
			lines = append(lines, errorStyle.Render(fmt.Sprintf("现有残留：%s · %s · %s", residual.Kind, residual.Name, residual.State)))
		}
		lines = append(lines, "恢复动作  失败时撤销 Runtime 创建的网络对象并恢复直连")
		if m.networkPreview.PreviewOnly {
			lines = append(lines, warnStyle.Render("当前平台实现仍处于预览阶段，确认前请核对真实路由与冲突"))
		}
	}

	lines = append(lines,
		"",
		m.focusHeading(2, "网络操作"),
		"Ctrl+P 显式代理 · Ctrl+T 普通 TUN · Ctrl+L Linux 网关 · Ctrl+E 应用已确认预览 · Ctrl+X 停用并恢复直连",
	)
	return strings.Join(lines, "\n")
}

func (m Model) renderMonitorPage() string {
	observed := "尚未同步"
	if !m.snapshot.ObservedAt.IsZero() {
		observed = m.snapshot.ObservedAt.Local().Format(time.RFC3339)
	}
	lines := []string{
		m.focusHeading(0, "实时概况"),
		"Mihomo 状态      " + valueOr(m.snapshot.Mihomo.State, "未知"),
		"网络状态         " + valueOr(m.snapshot.Network.State, "未知"),
		fmt.Sprintf("当前速度         ↑ %s/s    ↓ %s/s", formatTrafficBytes(m.snapshot.Traffic.UploadSpeed), formatTrafficBytes(m.snapshot.Traffic.DownloadSpeed)),
		fmt.Sprintf("本次运行累计     ↑ %s      ↓ %s", formatTrafficBytes(m.snapshot.Traffic.UploadTotal), formatTrafficBytes(m.snapshot.Traffic.DownloadTotal)),
		fmt.Sprintf("活动连接         %d", m.snapshot.Traffic.ActiveConnections),
		"",
		m.focusHeading(1, "速度曲线"),
		fmt.Sprintf("范围 %s · [ / ] 切换 1、5、15 分钟 · 每秒增量刷新", trafficRangeLabel(m.trafficRange)),
		"上传 " + m.renderTrafficSparkline(true),
		"下载 " + m.renderTrafficSparkline(false),
		mutedStyle.Render("│ 表示 Mihomo 停止、重启或计数器重置造成的中断"),
	}
	if !m.snapshot.Traffic.Available {
		lines = append(lines, warnStyle.Render("Mihomo 流量接口当前不可用；保留最后一次确认的本次运行累计"))
	}
	if m.trafficStale {
		message := "流量数据已过期，正在通过独立 IPC 重试"
		if m.trafficFault != nil {
			message += "：" + publicErrorMessage(m.trafficFault)
		}
		lines = append(lines, warnStyle.Render(message))
	}

	loadedQuery := m.connectionQuery
	if m.connectionHasData {
		loadedQuery = m.connectionLoadedQuery
	}
	page := m.connectionPage.Page
	if page <= 0 {
		page = loadedQuery.Page
	}
	if page <= 0 {
		page = 1
	}
	pageSize := m.connectionPage.PageSize
	if pageSize <= 0 {
		pageSize = loadedQuery.PageSize
	}
	if pageSize <= 0 {
		pageSize = runtimeapi.ConnectionPageDefaultSize
	}
	lastPage := 1
	if m.connectionPage.Total > 0 {
		lastPage = (m.connectionPage.Total + pageSize - 1) / pageSize
	}
	lines = append(lines,
		"",
		m.focusHeading(2, "活动连接"),
		fmt.Sprintf("%s · 第 %d/%d 页 · 共 %d 条 · F 筛选 · ,/. 翻页 · D 关闭所选 · X 关闭当前范围", connectionFilterSummary(loadedQuery), page, lastPage, m.connectionPage.Total),
	)
	if len(m.connectionPage.Items) == 0 {
		lines = append(lines, mutedStyle.Render("当前筛选下没有活动连接"))
	}
	for index, connection := range m.connectionPage.Items {
		prefix := "  "
		if index == m.connectionSelected {
			prefix = "▶ "
		}
		lines = append(lines, fmt.Sprintf(
			"%s%s → %s · %s · %s · ↑ %s ↓ %s",
			prefix,
			valueOr(connection.Source, "未知来源"),
			valueOr(connection.Target, "未知目标"),
			valueOr(connection.Protocol, "未知协议"),
			valueOr(connection.Process, "未知进程"),
			formatTrafficBytes(connection.Upload),
			formatTrafficBytes(connection.Download),
		))
	}
	if len(m.connectionPage.Items) > 0 {
		selected := m.connectionSelected
		if selected < 0 || selected >= len(m.connectionPage.Items) {
			selected = 0
		}
		connection := m.connectionPage.Items[selected]
		lines = append(lines,
			fmt.Sprintf("详情 ID %s · 开始 %s · 持续 %s · 入站 %s", valueOr(connection.ID, "未知"), formatConnectionTime(connection.StartedAt), formatConnectionDuration(connection.DurationSeconds), valueOr(connection.Inbound, "未知")),
			fmt.Sprintf("规则 %s%s · 出站 %s", valueOr(connection.Rule, "未匹配"), connectionRulePayloadSuffix(connection.RulePayload), valueOr(strings.Join(connection.OutboundChain, " → "), "未知")),
			mutedStyle.Render("↑/↓ 选择连接；详情由 Runtime 本机 IPC 提供，TUI 不直接连接 Mihomo"),
		)
	}
	if m.connectionStale {
		message := "活动连接数据已过期；保留最近一次结果并继续重试"
		if m.connectionFault != nil {
			message += "：" + publicErrorMessage(m.connectionFault)
		}
		lines = append(lines, warnStyle.Render(message))
	}

	lines = append(lines,
		"",
		m.focusHeading(3, "Runtime 事件"),
		fmt.Sprintf("最新游标 %d · Snapshot revision %d · 观测时间 %s", m.snapshot.LatestEventCursor, m.snapshot.Revision, observed),
	)
	if m.snapshotFault != nil {
		lines = append(lines, warnStyle.Render("本机 IPC："+publicErrorMessage(m.snapshotFault)))
	}
	return strings.Join(lines, "\n")
}

func formatConnectionTime(value time.Time) string {
	if value.IsZero() {
		return "未知"
	}
	return value.Local().Format("15:04:05")
}

func formatConnectionDuration(seconds int64) string {
	if seconds < 0 {
		seconds = 0
	}
	return (time.Duration(seconds) * time.Second).String()
}

func connectionRulePayloadSuffix(payload string) string {
	if strings.TrimSpace(payload) == "" {
		return ""
	}
	return "(" + payload + ")"
}

func (m Model) renderMaintenancePage() string {
	lines := []string{
		m.focusHeading(0, "更新与回滚"),
		fmt.Sprintf("Runtime %s · 上一版 %s", valueOr(m.snapshot.Updates.RuntimeCurrentVersion, "未知"), valueOr(m.snapshot.Updates.RuntimePreviousVersion, "无")),
		fmt.Sprintf("Mihomo %s · 上一版 %s", valueOr(m.snapshot.Updates.MihomoCurrentVersion, "未安装"), valueOr(m.snapshot.Updates.MihomoPreviousVersion, "无")),
	}
	if m.mihomoUpdate.PlanID != "" {
		lines = append(lines, fmt.Sprintf("Mihomo 更新计划 · %s · %s", m.mihomoUpdate.Version, m.mihomoUpdate.PlanID))
	}
	if m.productUpdate.PlanID != "" {
		lines = append(lines, fmt.Sprintf("Runtime 更新计划 · %s · %s", m.productUpdate.Version, m.productUpdate.PlanID))
	}

	lines = append(lines, "", m.focusHeading(1, "备份与恢复"))
	if len(m.backupPreview.Items) == 0 && m.backupRestore.ContentID == "" && m.backupFile == "" {
		lines = append(lines, mutedStyle.Render("尚未预览备份或恢复"))
	}
	for _, item := range m.backupPreview.Items {
		if item.Included {
			lines = append(lines, fmt.Sprintf("%s · %d 项 · %d 字节", item.Name, item.Count, item.Size))
		}
	}
	if m.backupFile != "" {
		lines = append(lines, okStyle.Render("备份已保存："+m.backupFile))
	}
	if m.backupRestore.ContentID != "" {
		lines = append(lines, fmt.Sprintf("恢复预览 · %d 个来源 · %d 个托管资源", m.backupRestore.SourceCount, m.backupRestore.ManagedResourceCount))
	}

	lines = append(lines, "", m.focusHeading(2, "诊断"))
	if len(m.diagnostics.Items) == 0 {
		lines = append(lines, mutedStyle.Render("Ctrl+G 预览或生成脱敏诊断包"))
	}
	for _, item := range m.diagnostics.Items {
		lines = append(lines, fmt.Sprintf("%s · %d 字节", item.Name, item.Size))
	}
	if m.diagnosticsFile.FileName != "" {
		lines = append(lines, okStyle.Render("诊断包已保存："+m.diagnosticsFile.FileName))
	}

	lines = append(lines, "", m.focusHeading(3, "运行操作"), m.operationLine())
	return strings.Join(lines, "\n")
}

func (m Model) renderPalette() string {
	lines := []string{
		titleStyle.Render("命令搜索"),
		m.palette.View(),
		"",
	}
	matches := m.filteredCommands()
	if len(matches) == 0 {
		lines = append(lines, mutedStyle.Render("没有匹配的页面或操作"))
	}
	for index, command := range matches {
		prefix := "  "
		if index == m.paletteIndex {
			prefix = "▶ "
		}
		key := command.key
		if command.page != "" {
			key = definitionFor(command.page).number
		}
		lines = append(lines, fmt.Sprintf("%s%s  %s", prefix, command.label, key))
	}
	lines = append(lines, "", "↑↓ 选择 · Enter 打开 · Esc 关闭")
	return strings.Join(lines, "\n")
}

func (m Model) renderHelp() string {
	return strings.Join([]string{
		titleStyle.Render("键盘帮助"),
		"1–5 切换页面 · Tab/Shift+Tab 移动页内焦点",
		"↑↓ 选择当前区域条目 · Enter 打开或执行当前区域的安全默认操作",
		"配置页的本机配置层：Enter 或 Ctrl+Y 选择流量策略并生成候选确认",
		"/ 或 Ctrl+K 搜索页面和操作 · Esc 返回 · ? 关闭帮助 · q 退出",
		"现有字母快捷键继续可用，页面底部会显示当前焦点。",
	}, "\n")
}

func (m Model) renderShellFooter() string {
	definition := definitionFor(m.page)
	focus := m.focusIndex
	if focus < 0 || focus >= len(definition.regions) {
		focus = 0
	}
	region := "无"
	if len(definition.regions) > 0 {
		region = definition.regions[focus]
	}
	status := renderStatus(m.status, m.err, m.busy)
	return strings.Join([]string{
		status,
		m.renderOperationStrip(),
		warnStyle.Render(runtimeapi.SensitiveDataWarning),
		fmt.Sprintf("焦点 %d/%d · %s  |  1–5 页面 · Tab 焦点 · / 搜索 · ? 帮助 · r 刷新 · q 退出", focus+1, len(definition.regions), region),
		"所有客户端只通过 Runtime 本机 IPC 管理",
	}, "\n")
}

func (m Model) renderConfirmation() string {
	confirmation := m.confirmation
	if confirmation == nil {
		return ""
	}
	return strings.Join([]string{
		warnStyle.Render("确认运行操作 · " + confirmation.Title),
		"操作目标    " + confirmation.Target,
		"影响        " + confirmation.Impact,
		"可能中断    " + confirmation.Interruption,
		"配置版本    " + confirmation.ConfigVersion,
		"恢复方式    " + confirmation.Recovery,
		"Enter 确认 · Esc 取消；确认前不会修改 Runtime 状态",
	}, "\n")
}

func (m Model) renderOperationStrip() string {
	operation := m.lastOperation
	if currentID := m.snapshot.Operations.CurrentOperationID; currentID != "" && currentID != operation.ID {
		return fmt.Sprintf("运行操作 · %s · 正在读取详情 · 队列 %d", currentID, m.snapshot.Operations.Queued)
	}
	if operation.ID == "" {
		if m.operationUncertain {
			return errorStyle.Render("运行操作 · 结果不确定 · 不会自动重放；重新连接后请核对运行操作")
		}
		if m.snapshot.Operations.CurrentOperationID != "" {
			return fmt.Sprintf("运行操作 · %s · 正在读取详情 · 队列 %d", m.snapshot.Operations.CurrentOperationID, m.snapshot.Operations.Queued)
		}
		if m.snapshot.Operations.RecentOperationID != "" {
			return fmt.Sprintf("最近运行操作 · %s · 正在读取结果 · 队列 %d", m.snapshot.Operations.RecentOperationID, m.snapshot.Operations.Queued)
		}
		return fmt.Sprintf("运行操作 · 无 · 队列 %d", m.snapshot.Operations.Queued)
	}
	state := fmt.Sprintf(
		"运行操作 · %s · %s · %s · %d%% · 耗时 %s · 队列 %d",
		operation.ID,
		valueOr(operation.State, "未知"),
		valueOr(operation.Stage, "未知阶段"),
		operation.Progress,
		operationElapsed(operation),
		m.snapshot.Operations.Queued,
	)
	if operation.Cancellable {
		state += " · 可取消"
	} else if !operationTerminal(operation.State) {
		state += " · 不可取消"
	}
	if m.operationUncertain {
		state += " · 结果不确定 · 不会自动重放"
	}
	if operation.Error != nil {
		state += " · " + operation.Error.Code + ": " + operation.Error.Message
	}
	if operation.Result != nil && (operation.Action.Kind == runtimeapi.ActionCloseConnection || operation.Action.Kind == runtimeapi.ActionCloseConnections) {
		if operation.Result.ClosedConnections > 0 {
			state += fmt.Sprintf(" · 已关闭 %d 条", operation.Result.ClosedConnections)
		}
		if operation.Result.AlreadyClosedConnections > 0 {
			state += fmt.Sprintf(" · 已消失 %d 条", operation.Result.AlreadyClosedConnections)
		}
	}
	return state
}

func (m Model) focusHeading(index int, title string) string {
	prefix := "  "
	if m.focusIndex == index {
		prefix = "▶ "
	}
	return labelStyle.Render(prefix + title)
}

func (m Model) currentSourceSummary() *runtimeapi.SourceSummary {
	for index := range m.snapshot.Sources.Items {
		source := &m.snapshot.Sources.Items[index]
		if source.Current || source.ID == m.snapshot.Sources.CurrentSourceID {
			return source
		}
	}
	return nil
}

func (m Model) operationLine() string {
	operation := m.lastOperation
	if currentID := m.snapshot.Operations.CurrentOperationID; currentID != "" && currentID != operation.ID {
		return fmt.Sprintf("%s · 正在读取详情 · 队列 %d", currentID, m.snapshot.Operations.Queued)
	}
	if operation.ID == "" {
		if m.snapshot.Operations.CurrentOperationID == "" {
			return mutedStyle.Render("没有正在执行或排队的运行操作")
		}
		return fmt.Sprintf("%s · 队列 %d", m.snapshot.Operations.CurrentOperationID, m.snapshot.Operations.Queued)
	}
	return fmt.Sprintf("%s · %s · %s · %d%%", operation.ID, operation.State, operation.Stage, operation.Progress)
}

func previewDNSPolicy(preview runtimeapi.NetworkPreview) string {
	if preview.GatewaySettings != nil {
		return valueOr(preview.GatewaySettings.DNSPolicy, "未改变")
	}
	return valueOr(preview.Settings.DNSPolicy, "未改变")
}

func formatGatewayException(exception runtimeapi.GatewayTrafficException) string {
	parts := make([]string, 0, 4)
	if exception.SourceCIDR != "" {
		parts = append(parts, "来源 "+exception.SourceCIDR)
	}
	if exception.DestinationCIDR != "" {
		parts = append(parts, "目标 "+exception.DestinationCIDR)
	}
	if len(exception.DestinationPorts) > 0 {
		ports := make([]string, 0, len(exception.DestinationPorts))
		for _, portRange := range exception.DestinationPorts {
			if portRange.End == 0 || portRange.End == portRange.Start {
				ports = append(ports, strconv.FormatUint(uint64(portRange.Start), 10))
				continue
			}
			ports = append(ports, fmt.Sprintf("%d-%d", portRange.Start, portRange.End))
		}
		parts = append(parts, "端口 "+strings.Join(ports, ","))
	}
	if exception.UID != nil {
		parts = append(parts, fmt.Sprintf("UID %d", *exception.UID))
	}
	if len(parts) == 0 {
		return "未指定条件"
	}
	return strings.Join(parts, " · ")
}

func formatPreviewTime(value time.Time) string {
	if value.IsZero() {
		return "未知"
	}
	return value.Local().Format("15:04:05")
}
