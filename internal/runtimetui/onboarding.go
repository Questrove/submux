package runtimetui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type onboardingStepID string

const (
	onboardingService   onboardingStepID = "service"
	onboardingCore      onboardingStepID = "core"
	onboardingSource    onboardingStepID = "source"
	onboardingCandidate onboardingStepID = "candidate"
	onboardingRunMode   onboardingStepID = "run_mode"
	onboardingStart     onboardingStepID = "start"
)

type onboardingStep struct {
	id       onboardingStepID
	title    string
	purpose  string
	current  string
	why      string
	recovery string
	complete bool
}

func onboardingRequired(snapshot runtimeapi.Snapshot) bool {
	if snapshot.ProtocolVersion == 0 {
		return false
	}
	_, _, pending := currentOnboardingStep(snapshot)
	return pending
}

func currentOnboardingStep(snapshot runtimeapi.Snapshot) (onboardingStep, int, bool) {
	steps := onboardingSteps(snapshot)
	for index, step := range steps {
		if !step.complete {
			return step, index, true
		}
	}
	return onboardingStep{}, len(steps), false
}

func onboardingSteps(snapshot runtimeapi.Snapshot) []onboardingStep {
	serviceReady := snapshot.Runtime.ServiceState == "running" && snapshot.Runtime.LocalIPCAuthorized && snapshot.Runtime.Fault == nil
	serviceValue := "服务 " + valueOr(snapshot.Runtime.ServiceState, "未知")
	if snapshot.Runtime.LocalIPCAuthorized {
		serviceValue += "；本机 IPC 已授权"
	} else {
		serviceValue += "；本机 IPC 未确认授权"
	}
	if snapshot.Runtime.Fault != nil {
		serviceValue += "；" + snapshot.Runtime.Fault.Code
	}

	coreReady := snapshot.Mihomo.Version != "" && snapshot.Mihomo.State != "not_installed" && snapshot.Mihomo.Fault == nil
	coreValue := valueOr(snapshot.Mihomo.Version, "未安装")
	if snapshot.Mihomo.State != "" {
		coreValue += "；状态 " + snapshot.Mihomo.State
	}

	sourceCount := snapshot.Sources.Count
	if len(snapshot.Sources.Items) > sourceCount {
		sourceCount = len(snapshot.Sources.Items)
	}
	sourceReady := len(snapshot.Sources.Items) > 0 && snapshot.Sources.Count == len(snapshot.Sources.Items)
	sourceValue := fmt.Sprintf("%d 个来源", sourceCount)
	if current := sourceByID(snapshot, snapshot.Sources.CurrentSourceID); current != nil {
		sourceValue += "；当前 " + current.Name
	} else {
		sourceValue += "；尚未提交当前来源"
	}

	candidateSource := sourceByID(snapshot, snapshot.Sources.CurrentSourceID)
	candidateReady := candidateSource != nil && candidateSource.HasValidatedCandidate
	candidateValue := "尚无已提交来源的有效候选"
	if candidateSource != nil {
		candidateValue = candidateSource.Name + "；"
		if candidateSource.HasValidatedCandidate {
			candidateValue += "候选已校验"
		} else {
			candidateValue += "候选待校验"
		}
	} else if candidate := firstValidatedSource(snapshot); candidate != nil {
		candidateValue = candidate.Name + "；候选已校验，尚未提交为当前来源"
	}

	runModeReady := validNetworkMode(snapshot.RunMode)
	runModeValue := networkModeLabel(snapshot.RunMode)
	if !runModeReady {
		runModeValue = "未配置"
	}

	startReady := snapshot.Mihomo.DesiredState == runtimeapi.MihomoDesiredRunning && snapshot.Mihomo.State == "running" && snapshot.Mihomo.Fault == nil
	startValue := fmt.Sprintf("实际 %s；期望 %s", valueOr(snapshot.Mihomo.State, "未知"), valueOr(snapshot.Mihomo.DesiredState, "未设置"))

	return []onboardingStep{
		{
			id: onboardingService, title: "服务与权限", complete: serviceReady,
			purpose:  "确认 Runtime 服务可用，并允许当前本机用户通过受限 IPC 管理",
			current:  serviceValue,
			why:      "后续读取、预览和写操作都必须由 Runtime 统一授权与审计",
			recovery: "修复或重启 submux-runtime 服务及本机用户权限后按 r 重试",
		},
		{
			id: onboardingCore, title: "官方 Mihomo 核心", complete: coreReady,
			purpose:  "通过 TUF 校验并安装 MetaCubeX/mihomo 官方稳定版",
			current:  coreValue,
			why:      "候选配置必须交给已验证的实际核心检查，Runtime 也只运行该核心",
			recovery: "在维护页重新获取官方更新计划；校验失败不会替换现有核心",
		},
		{
			id: onboardingSource, title: "Runtime 配置来源", complete: sourceReady,
			purpose:  "添加远程订阅或本机配置副本，并由 Runtime 保存来源定义",
			current:  sourceValue,
			why:      "Runtime 需要明确、可刷新且可诊断的配置来源",
			recovery: "在配置页检查地址、文件和认证信息；失败不会保存未校验来源",
		},
		{
			id: onboardingCandidate, title: "候选校验", complete: candidateReady,
			purpose:  "生成最终候选，使用已安装核心校验后再提交当前来源",
			current:  candidateValue,
			why:      "只有通过校验的配置才能进入应用和启动阶段",
			recovery: "查看来源诊断并重新生成候选；校验或应用失败会保留原配置",
		},
		{
			id: onboardingRunMode, title: "运行方式预览", complete: runModeReady,
			purpose:  "选择显式代理、普通 TUN 或 Linux 网关，并先查看实际影响",
			current:  runModeValue,
			why:      "不同方式会改变监听、路由或 DNS，必须先检查预览与冲突",
			recovery: "解决预览中的冲突后重新生成；确认前不会应用网络设置",
		},
		{
			id: onboardingStart, title: "应用启动", complete: startReady,
			purpose:  "确认启动操作，让 Runtime 应用已校验配置并启动 Mihomo",
			current:  startValue,
			why:      "启动是独立写操作，必须明确确认并保留运行结果",
			recovery: "在运行操作详情和日志中检查失败；Runtime 会恢复最近可用状态",
		},
	}
}

func sourceByID(snapshot runtimeapi.Snapshot, sourceID string) *runtimeapi.SourceSummary {
	if sourceID == "" {
		return nil
	}
	for index := range snapshot.Sources.Items {
		if snapshot.Sources.Items[index].ID == sourceID {
			return &snapshot.Sources.Items[index]
		}
	}
	return nil
}

func firstValidatedSource(snapshot runtimeapi.Snapshot) *runtimeapi.SourceSummary {
	for index := range snapshot.Sources.Items {
		if snapshot.Sources.Items[index].HasValidatedCandidate {
			return &snapshot.Sources.Items[index]
		}
	}
	return nil
}

func (m Model) renderOnboardingPage() string {
	steps := onboardingSteps(m.snapshot)
	_, currentIndex, _ := currentOnboardingStep(m.snapshot)
	nextRestart := "无"
	if m.snapshot.Mihomo.NextRestartAt != nil {
		nextRestart = m.snapshot.Mihomo.NextRestartAt.Local().Format(time.RFC3339)
	}
	lines := []string{
		m.focusHeading(0, "首次运行引导"),
		fmt.Sprintf("当前步骤 %d/%d · 状态完全来自 Runtime Snapshot，不保存另一套完成标记", currentIndex+1, len(steps)),
		fmt.Sprintf("Mihomo 实际 / 期望  %s / %s", valueOr(m.snapshot.Mihomo.State, "未知"), valueOr(m.snapshot.Mihomo.DesiredState, "未设置")),
		fmt.Sprintf("崩溃恢复              %s", valueOr(m.snapshot.Mihomo.Recovery, "未知")),
		fmt.Sprintf("重试次数 / 下次重试   %d / %s", m.snapshot.Mihomo.CrashAttempts, nextRestart),
	}
	if m.snapshot.Runtime.Fault != nil {
		lines = append(lines, errorStyle.Render("Runtime 故障："+m.snapshot.Runtime.Fault.Code+" · "+m.snapshot.Runtime.Fault.Message))
	}
	if m.snapshot.Mihomo.Fault != nil {
		lines = append(lines, errorStyle.Render("Mihomo 故障："+m.snapshot.Mihomo.Fault.Code+" · "+m.snapshot.Mihomo.Fault.Message))
	}
	for index, step := range steps {
		marker := "○"
		style := mutedStyle
		if step.complete {
			marker = "✓"
			style = okStyle
		} else if index == currentIndex {
			marker = "▶"
			style = warnStyle
		}
		lines = append(lines,
			"",
			style.Render(fmt.Sprintf("%s %d. %s", marker, index+1, step.title)),
			fmt.Sprintf("   作用 %s · 当前值 %s", step.purpose, step.current),
			fmt.Sprintf("   为什么 %s · 失败处理 %s", step.why, step.recovery),
		)
	}
	lines = append(lines, "", mutedStyle.Render("Enter 继续当前步骤 · 所有写操作仍需查看预览并再次确认 · r 重新读取实际状态"))
	return strings.Join(lines, "\n")
}

func (m *Model) activateOnboarding() tea.Cmd {
	if m.snapshotStale {
		m.err = nil
		m.status = "当前 Snapshot 已过期；请等待重连或按 r 刷新后再继续"
		return nil
	}
	step, _, pending := currentOnboardingStep(m.snapshot)
	if !pending {
		return nil
	}
	switch step.id {
	case onboardingService:
		m.err = nil
		m.status = "Runtime 服务或当前用户权限尚未就绪；修复后按 r 重新读取状态"
		return nil
	case onboardingCore:
		return m.beginMihomoUpdate()
	case onboardingSource:
		m.openPage(pageConfig)
		m.status = "选择配置来源：u 添加远程来源，n 添加本机配置副本；保存前都会校验"
		return nil
	case onboardingCandidate:
		sourceID := m.snapshot.Sources.CurrentSourceID
		actionKind := runtimeapi.ActionApplySource
		if sourceByID(m.snapshot, sourceID) == nil {
			sourceID = ""
			actionKind = runtimeapi.ActionSwitchSource
			if candidate := firstValidatedSource(m.snapshot); candidate != nil {
				sourceID = candidate.ID
			} else if len(m.snapshot.Sources.Items) > 0 {
				sourceID = m.snapshot.Sources.Items[0].ID
			}
		}
		if sourceID == "" {
			m.openPage(pageConfig)
			m.status = "先添加配置来源，再生成并校验候选"
			return nil
		}
		m.selectedSourceID = sourceID
		m.prepareAction(runtimeapi.Action{Kind: actionKind, Params: runtimeapi.ActionParams{SourceID: sourceID}})
		return nil
	case onboardingRunMode:
		m.openPage(pageNetwork)
		mode := m.snapshot.Network.Mode
		if !validNetworkMode(mode) {
			mode = runtimeapi.RunModeExplicit
		}
		m.networkForm = newNetworkForm(mode)
		m.err = nil
		m.status = "选择运行方式；Ctrl+S 生成真实预览或进入统一确认"
		return m.networkForm.loadActiveField()
	case onboardingStart:
		m.prepareAction(runtimeapi.Action{Kind: runtimeapi.ActionStartProxy})
		return nil
	default:
		return nil
	}
}

func (m *Model) beginMihomoUpdate() tea.Cmd {
	if m.mihomoUpdate.PlanID == "" ||
		(!m.mihomoUpdate.ExpiresAt.IsZero() && !time.Now().Before(m.mihomoUpdate.ExpiresAt)) {
		client, ok := m.client.(MihomoUpdateClient)
		if !ok {
			m.err = fmt.Errorf("当前 TUI 客户端不支持 Mihomo 更新")
			m.status = m.err.Error()
			return nil
		}
		m.busy = true
		m.mihomoUpdate = runtimeapi.MihomoUpdatePlan{}
		m.status = "正在通过 TUF 检查官方 Mihomo 稳定更新…"
		return m.previewMihomoUpdateCmd(client)
	}
	m.sensitiveConfirm = ""
	m.prepareAction(runtimeapi.Action{
		Kind: runtimeapi.ActionUpdateMihomo,
		Params: runtimeapi.ActionParams{
			PlanID:  m.mihomoUpdate.PlanID,
			Trust:   m.mihomoUpdate.Trust,
			Confirm: true,
		},
	})
	return nil
}
