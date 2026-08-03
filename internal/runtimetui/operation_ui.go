package runtimetui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type runtimeErrorMetadata interface {
	RuntimeErrorCode() string
	RuntimeErrorRetryable() bool
}

type actionConfirmation struct {
	Action            runtimeapi.Action
	CancelOperationID string
	Title             string
	Target            string
	Impact            string
	Interruption      string
	ConfigVersion     string
	Recovery          string
}

func (m *Model) prepareAction(action runtimeapi.Action) {
	if m.rejectStaleMutation() {
		return
	}
	if conflict := m.actionConflict(action); conflict != "" {
		m.confirmation = nil
		m.busy = false
		m.err = errors.New(conflict)
		m.status = m.err.Error()
		return
	}
	confirmation := m.describeAction(action)
	m.confirmation = &confirmation
	m.busy = false
	m.err = nil
	m.status = "请检查运行操作预览；Enter 确认，Esc 取消"
}

func (m *Model) confirmAction() tea.Cmd {
	if m.confirmation == nil {
		return nil
	}
	if m.rejectStaleMutation() {
		return nil
	}
	confirmation := *m.confirmation
	m.confirmation = nil
	m.busy = true
	m.err = nil
	m.operationFault = nil
	m.status = "正在提交运行操作…"
	if confirmation.CancelOperationID != "" {
		return m.cancelCmd(confirmation.CancelOperationID)
	}
	return m.executeCmd(confirmation.Action)
}

func (m *Model) cancelActionConfirmation() {
	if m.confirmation == nil {
		return
	}
	m.confirmation = nil
	m.busy = false
	m.err = nil
	m.status = "已取消运行操作"
}

func (m *Model) prepareCancellation(operation runtimeapi.Operation) {
	if m.rejectStaleMutation() {
		return
	}
	m.confirmation = &actionConfirmation{
		CancelOperationID: operation.ID,
		Title:             "取消运行操作",
		Target:            operation.ID + " · " + actionTitle(operation.Action.Kind),
		Impact:            "请求 Runtime 在当前可取消阶段停止该运行操作",
		Interruption:      "已经完成的步骤不会回滚；是否可取消仍由 Runtime 最终判断",
		ConfigVersion:     fmt.Sprintf("Snapshot revision %d", m.snapshot.Revision),
		Recovery:          "取消失败时运行操作继续；重新读取运行操作确认结果",
	}
	m.busy = false
	m.err = nil
	m.status = "请检查取消操作预览；Enter 确认，Esc 取消"
}

func (m *Model) rejectStaleMutation() bool {
	if !m.snapshotStale {
		return false
	}
	m.confirmation = nil
	m.busy = false
	m.err = errors.New("Runtime Snapshot 已过期；请等待重连或按 r 刷新后再操作")
	m.status = m.err.Error()
	return true
}

func (m Model) describeAction(action runtimeapi.Action) actionConfirmation {
	description := actionConfirmation{
		Action:        action,
		Title:         actionTitle(action.Kind),
		Target:        actionTarget(action),
		Impact:        "修改 Submux Runtime 本机状态",
		Interruption:  "无计划中断",
		ConfigVersion: fmt.Sprintf("Snapshot revision %d", m.snapshot.Revision),
		Recovery:      "失败后保留当前已生效状态",
	}

	if digest := shortDigest(m.preview.CandidateSHA256); digest != "" && digest != "未生成" {
		description.ConfigVersion += " · 候选 " + digest
	}

	switch action.Kind {
	case runtimeapi.ActionTestProxyLatency:
		switch action.Params.LatencyScope {
		case runtimeapi.ProxyLatencyScopeNode:
			description.Impact = fmt.Sprintf("使用 Runtime 固定测试参数测量节点 %s 的延迟", action.Params.ProxyNode)
		case runtimeapi.ProxyLatencyScopeGroup:
			description.Impact = fmt.Sprintf("使用 Runtime 固定测试参数测量代理组 %s 的全部节点", action.Params.ProxyGroup)
		default:
			description.Impact = "使用 Runtime 固定测试参数测量当前来源的全部代理节点"
		}
		description.Interruption = "最多并行测试 4 个节点；测试期间可以取消或切换页面"
		description.Recovery = "测试失败只记录失败状态，不会改变当前代理节点选择"
	case runtimeapi.ActionSelectProxyNode:
		description.Impact = fmt.Sprintf("把代理组 %s 切换到节点 %s，并按来源保存选择", action.Params.ProxyGroup, action.Params.ProxyNode)
		description.Interruption = "已有连接通常保持不变；新连接使用新节点"
		description.Recovery = "目标不存在、Mihomo 拒绝或保存失败时保留原选择"
	case runtimeapi.ActionSetTrafficPolicy:
		description.Impact = fmt.Sprintf("把本机流量策略保存为%s；候选配置已通过校验", trafficPolicyLabel(action.Params.TrafficPolicy))
		description.Interruption = "不会立即切换当前配置或重启 Mihomo；下次应用配置后生效"
		description.Recovery = "可重新选择其他策略，或恢复为跟随来源以删除本机覆盖"
	case runtimeapi.ActionCloseConnection:
		description.Impact = "关闭所选 Mihomo 活动连接"
		description.Interruption = "现有会话立即中断，应用可能自行重新连接"
		description.Recovery = "连接无法恢复；需要时由应用重新建立连接"
	case runtimeapi.ActionCloseConnections:
		description.Impact = fmt.Sprintf("关闭当前确认范围内最多 %d 条 Mihomo 活动连接", action.Params.ConnectionCount)
		description.Interruption = "当前范围内的现有会话立即中断，应用可能自行重新连接"
		description.Recovery = "连接无法恢复；需要时由应用重新建立连接"
	case runtimeapi.ActionStartProxy:
		description.Impact = "启动 Mihomo，使用当前来源和运行方式"
		description.Recovery = "启动失败时保持停止，并保留最近可用配置"
	case runtimeapi.ActionStopProxy:
		description.Impact = "停止 Mihomo，并撤销 Runtime 管理的网络接管"
		description.Interruption = "现有代理连接会中断，流量恢复直连"
		description.Recovery = "可重新预览当前状态后再次启动"
	case runtimeapi.ActionApplyImportedConfig, runtimeapi.ActionApplySource:
		description.Impact = "校验并应用候选配置，成功后提交为最近可用配置"
		description.Interruption = "Mihomo 可能短暂重载"
		description.Recovery = "校验、应用或健康检查失败时恢复最近可用配置"
	case runtimeapi.ActionSwitchSource:
		description.Impact = "刷新所选来源 → 生成候选 → 校验 → 应用与健康检查 → 提交当前来源"
		description.Interruption = "Mihomo 可能短暂重载"
		description.Recovery = "任何一步失败都恢复原来源和原运行状态"
	case runtimeapi.ActionRefreshSource:
		description.Impact = "刷新所选 Runtime 配置来源并生成候选配置，不自动应用"
		description.Recovery = "失败时继续使用当前来源和最近可用配置"
	case runtimeapi.ActionAddRemoteSource, runtimeapi.ActionAddImportedSource:
		description.Impact = "保存新的 Runtime 配置来源，不切换当前来源"
		description.Recovery = "失败时不新增来源；已上传内容到期后自动清理"
	case runtimeapi.ActionDeleteSource:
		description.Impact = "删除所选 Runtime 配置来源及其本机保存状态"
		description.Recovery = "可重新添加来源，或从完整备份整体恢复"
	case runtimeapi.ActionAddManagedResource:
		description.Impact = "保存托管资源，供候选配置安全引用"
		description.Recovery = "失败时不改变现有托管资源"
	case runtimeapi.ActionSetAdvancedOverride:
		description.Impact = "替换本机高级覆盖；不会直接应用或启动 Mihomo"
		description.Recovery = "失败时保留当前本机高级覆盖"
	case runtimeapi.ActionEnableTUN, runtimeapi.ActionEnableGateway:
		description.Impact = "按已验证网络预览修改路由、DNS 和网络接管状态"
		description.Interruption = "切换期间网络可能短暂中断"
		description.Recovery = "失败时撤销 Runtime 创建的状态并执行故障放行"
	case runtimeapi.ActionDisableTUN, runtimeapi.ActionDisableGateway:
		description.Impact = "撤销 Runtime 管理的网络接管，切换回显式代理，并保持 Mihomo 原来的运行或停止状态"
		description.Interruption = "现有代理连接会中断"
		description.Recovery = "可重新生成网络预览后再次启用"
	case runtimeapi.ActionUpdateMihomo, runtimeapi.ActionRollbackMihomo:
		description.Impact = "替换官方 Mihomo 核心并验证当前候选配置"
		description.Interruption = "代理会短暂停止"
		description.Recovery = "启动或健康检查失败时恢复上一版核心"
	case runtimeapi.ActionUpdateProduct, runtimeapi.ActionRollbackProduct:
		description.Impact = "由平台安装器替换 Runtime 产品版本"
		description.Interruption = "先恢复直连，再停止 Runtime 相关服务"
		description.Recovery = "安装验证失败时由平台安装器恢复上一版"
	case runtimeapi.ActionRestoreBackup:
		description.Impact = "先备份当前状态，再整体替换可迁移的 Runtime 状态"
		description.Interruption = "核心保持停止，机器相关设置恢复为待确认"
		description.Recovery = "使用恢复前自动备份整体还原"
	}
	return description
}

func actionTitle(kind string) string {
	titles := map[string]string{
		runtimeapi.ActionSetTrafficPolicy:    "设置流量策略",
		runtimeapi.ActionTestProxyLatency:    "测试代理延迟",
		runtimeapi.ActionSelectProxyNode:     "选择代理节点",
		runtimeapi.ActionCloseConnection:     "关闭活动连接",
		runtimeapi.ActionCloseConnections:    "关闭当前范围内全部连接",
		runtimeapi.ActionApplyImportedConfig: "应用导入的候选配置",
		runtimeapi.ActionStartProxy:          "启动 Mihomo",
		runtimeapi.ActionStopProxy:           "停止 Mihomo 并恢复直连",
		runtimeapi.ActionAddRemoteSource:     "添加远程配置来源",
		runtimeapi.ActionAddImportedSource:   "添加本机配置来源",
		runtimeapi.ActionRefreshSource:       "刷新配置来源",
		runtimeapi.ActionApplySource:         "应用当前来源",
		runtimeapi.ActionSwitchSource:        "切换配置来源",
		runtimeapi.ActionDeleteSource:        "删除配置来源",
		runtimeapi.ActionAddManagedResource:  "添加托管资源",
		runtimeapi.ActionSetAdvancedOverride: "保存本机高级覆盖",
		runtimeapi.ActionEnableTUN:           "启用 TUN",
		runtimeapi.ActionDisableTUN:          "停用 TUN",
		runtimeapi.ActionEnableGateway:       "启用 Linux 网关",
		runtimeapi.ActionDisableGateway:      "停用 Linux 网关",
		runtimeapi.ActionUpdateMihomo:        "更新 Mihomo",
		runtimeapi.ActionRollbackMihomo:      "回滚 Mihomo",
		runtimeapi.ActionUpdateProduct:       "更新 Runtime",
		runtimeapi.ActionRollbackProduct:     "回滚 Runtime",
		runtimeapi.ActionRestoreBackup:       "整体恢复 Runtime 状态",
	}
	if title := titles[kind]; title != "" {
		return title
	}
	return valueOr(kind, "未知运行操作")
}

func actionTarget(action runtimeapi.Action) string {
	params := action.Params
	switch {
	case params.ProxyGroup != "":
		return fmt.Sprintf("%s → %s · 来源 %s", params.ProxyGroup, params.ProxyNode, params.SourceID)
	case params.TrafficPolicy != "":
		return trafficPolicyLabel(params.TrafficPolicy)
	case params.ConnectionID != "":
		return valueOr(params.ConnectionTarget, "未知目标") + " · " + params.ConnectionID
	case params.ConnectionScope != nil:
		return fmt.Sprintf("%s · 已确认 %d 条", connectionFilterSummary(*params.ConnectionScope), params.ConnectionCount)
	case params.SourceName != "":
		return params.SourceName
	case params.SourceID != "":
		return params.SourceID
	case params.ResourceName != "":
		return params.ResourceName + " · " + params.ResourceKind
	case params.PlanID != "":
		return params.PlanID
	case params.ContentID != "":
		return params.ContentID
	case strings.Contains(action.Kind, "mihomo"):
		return "官方 Mihomo 核心"
	case strings.Contains(action.Kind, "product"):
		return "Submux Runtime 产品"
	case strings.Contains(action.Kind, "network"):
		return "本机网络接管"
	default:
		return "本机 Submux Runtime"
	}
}

func (m Model) actionConflict(action runtimeapi.Action) string {
	current := m.lastOperation
	if currentID := m.snapshot.Operations.CurrentOperationID; currentID != "" &&
		(current.ID != currentID || operationTerminal(current.State)) {
		return fmt.Sprintf("正在读取当前运行操作 %s 的详情；暂不提交写操作", currentID)
	}
	if current.ID == "" || operationTerminal(current.State) {
		return ""
	}
	currentGroup := actionConflictGroup(current.Action.Kind)
	requestedGroup := actionConflictGroup(action.Kind)
	if currentGroup == "" || requestedGroup == "" || currentGroup != requestedGroup {
		return ""
	}
	return fmt.Sprintf("与当前运行操作冲突：%s 正在 %s", actionTitle(current.Action.Kind), valueOr(current.Stage, current.State))
}

func actionConflictGroup(kind string) string {
	switch kind {
	case runtimeapi.ActionSetTrafficPolicy, runtimeapi.ActionSelectProxyNode:
		return "configuration"
	case runtimeapi.ActionTestProxyLatency:
		return "proxy-latency"
	case runtimeapi.ActionCloseConnection, runtimeapi.ActionCloseConnections:
		return "connection-control"
	case runtimeapi.ActionAddRemoteSource,
		runtimeapi.ActionAddImportedSource,
		runtimeapi.ActionRefreshSource,
		runtimeapi.ActionDeleteSource,
		runtimeapi.ActionAddManagedResource,
		runtimeapi.ActionSetAdvancedOverride:
		return "configuration-data"
	case runtimeapi.ActionApplyImportedConfig,
		runtimeapi.ActionApplySource,
		runtimeapi.ActionSwitchSource,
		runtimeapi.ActionStartProxy,
		runtimeapi.ActionStopProxy,
		runtimeapi.ActionEnableTUN,
		runtimeapi.ActionDisableTUN,
		runtimeapi.ActionEnableGateway,
		runtimeapi.ActionDisableGateway,
		runtimeapi.ActionUpdateMihomo,
		runtimeapi.ActionRollbackMihomo,
		runtimeapi.ActionUpdateProduct,
		runtimeapi.ActionRollbackProduct,
		runtimeapi.ActionRestoreBackup:
		return "runtime-state"
	default:
		return ""
	}
}

func (m Model) runningOperation() bool {
	return m.lastOperation.ID != "" && !operationTerminal(m.lastOperation.State)
}

func (m Model) operationIDForControl() string {
	if m.snapshot.Operations.CurrentOperationID != "" {
		return m.snapshot.Operations.CurrentOperationID
	}
	return m.lastOperation.ID
}

func (m *Model) snapshotFollowUpCmd() tea.Cmd {
	commands := make([]tea.Cmd, 0, 3)
	if !m.watchingEvents {
		m.watchingEvents = true
		commands = append(commands, m.watchEventsCmd(m.snapshot.LatestEventCursor))
	}
	operationID := m.snapshot.Operations.CurrentOperationID
	if operationID == "" {
		operationID = m.snapshot.Operations.RecentOperationID
	}
	if operationID != "" &&
		(operationID != m.lastOperation.ID || m.operationUncertain) {
		commands = append(commands, m.getCmd(operationID))
	}
	proxySourceID := m.snapshot.Sources.CurrentSourceID
	if m.page == pageConfig {
		proxySourceID = m.selectedSource()
	}
	if command := m.startProxyGroupsCmd(proxySourceID); command != nil {
		commands = append(commands, command)
	}
	return tea.Batch(commands...)
}

func (m *Model) waitForOperationCmd(operationID string) tea.Cmd {
	if operationID == "" || m.waitingOperationID == operationID {
		return nil
	}
	m.waitingOperationID = operationID
	return m.waitCmd(operationID)
}

func operationElapsed(operation runtimeapi.Operation) string {
	if operation.CreatedAt.IsZero() {
		return "未知"
	}
	end := operation.UpdatedAt
	if !operationTerminal(operation.State) || end.IsZero() {
		end = time.Now()
	}
	duration := end.Sub(operation.CreatedAt)
	if duration < 0 {
		duration = 0
	}
	return duration.Round(time.Second).String()
}

func operationOutcomeUncertain(err error) bool {
	if err == nil {
		return false
	}
	var metadata runtimeErrorMetadata
	if !errors.As(err, &metadata) {
		return true
	}
	switch metadata.RuntimeErrorCode() {
	case runtimeapi.ErrorInvalidRequest,
		runtimeapi.ErrorProtocolUnsupported,
		runtimeapi.ErrorUnauthorized,
		runtimeapi.ErrorRequestTooLarge,
		runtimeapi.ErrorRevisionConflict,
		runtimeapi.ErrorRequestConflict,
		runtimeapi.ErrorBusy,
		runtimeapi.ErrorNotFound,
		runtimeapi.ErrorNotCancellable,
		runtimeapi.ErrorContentExpired,
		runtimeapi.ErrorContentConsumed,
		runtimeapi.ErrorSourceAuthentication:
		return false
	default:
		return metadata.RuntimeErrorRetryable()
	}
}
