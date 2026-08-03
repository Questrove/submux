package runtimetui

import (
	"errors"
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

var trafficPolicyOptions = []string{
	runtimeapi.TrafficPolicyFollowSource,
	runtimeapi.TrafficPolicyRule,
	runtimeapi.TrafficPolicyGlobal,
	runtimeapi.TrafficPolicyDirect,
}

func (m Model) updateTrafficPolicyEditor(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "esc":
		m.trafficPolicyEditing = false
		m.trafficPolicyDraft = ""
		m.status = "已取消修改流量策略"
		return m, nil
	case "left", "up", "shift+tab":
		m.trafficPolicyDraft = adjacentTrafficPolicy(m.trafficPolicyDraft, -1)
		return m, nil
	case "right", "down", "tab":
		m.trafficPolicyDraft = adjacentTrafficPolicy(m.trafficPolicyDraft, 1)
		return m, nil
	case "enter":
		if m.busy {
			return m, nil
		}
		sourceID := m.snapshot.Sources.CurrentSourceID
		if sourceID == "" {
			m.err = errors.New("请先选择并校验 Runtime 配置来源")
			m.status = m.err.Error()
			return m, nil
		}
		m.busy = true
		m.err = nil
		m.status = "正在用所选流量策略生成并校验候选配置…"
		return m, m.previewTrafficPolicyCmd(sourceID, m.trafficPolicyDraft)
	default:
		return m, nil
	}
}

func adjacentTrafficPolicy(current string, offset int) string {
	index := 0
	for candidateIndex, candidate := range trafficPolicyOptions {
		if candidate == current {
			index = candidateIndex
			break
		}
	}
	index = (index + offset + len(trafficPolicyOptions)) % len(trafficPolicyOptions)
	return trafficPolicyOptions[index]
}

func (m Model) previewTrafficPolicyCmd(sourceID string, selection string) tea.Cmd {
	return func() tea.Msg {
		preview, err := m.client.PreviewCandidateRequest(m.ctx, runtimeapi.PreviewCandidateRequest{
			SourceID:      sourceID,
			TrafficPolicy: selection,
		})
		if err != nil {
			return errMsg{err: err}
		}
		return trafficPolicyPreviewMsg{preview: preview, selection: selection}
	}
}

func (m Model) renderTrafficPolicyEditor() string {
	lines := []string{
		titleStyle.Render("选择流量策略"),
		"流量策略决定进入 Mihomo 后如何处理流量，不会改变显式代理、TUN 或 Linux 网关等运行方式。",
		"",
	}
	for _, selection := range trafficPolicyOptions {
		mark := "  "
		if selection == m.trafficPolicyDraft {
			mark = "▶ "
		}
		lines = append(lines, fmt.Sprintf("%s%s · %s", mark, trafficPolicyLabel(selection), trafficPolicyExplanation(selection)))
	}
	lines = append(lines,
		"",
		"方向键或 Tab 选择 · Enter 生成候选并校验 · Esc 取消",
		mutedStyle.Render("校验通过后仍需在统一确认区域明确保存；保存不会立即应用或重启 Mihomo。"),
	)
	return strings.Join(lines, "\n")
}

func trafficPolicyLabel(selection string) string {
	switch selection {
	case runtimeapi.TrafficPolicyFollowSource:
		return "跟随来源"
	case runtimeapi.TrafficPolicyRule:
		return "规则"
	case runtimeapi.TrafficPolicyGlobal:
		return "全局"
	case runtimeapi.TrafficPolicyDirect:
		return "直连"
	case "unconfigured":
		return "未应用"
	case "unknown", "":
		return "未知"
	default:
		return selection
	}
}

func trafficPolicyExplanation(selection string) string {
	switch selection {
	case runtimeapi.TrafficPolicyFollowSource:
		return "不保存隐藏覆盖，使用来源与本机高级覆盖合成的值"
	case runtimeapi.TrafficPolicyRule:
		return "按最终规则顺序匹配流量"
	case runtimeapi.TrafficPolicyGlobal:
		return "全部流量交给 Mihomo 的 GLOBAL 策略组"
	case runtimeapi.TrafficPolicyDirect:
		return "全部流量直接连接"
	default:
		return ""
	}
}

func trafficPolicyOriginLabel(origin string) string {
	if origin == runtimeapi.TrafficPolicyOriginRuntime {
		return "本机运行设置"
	}
	return "Runtime 配置来源 / 本机高级覆盖"
}
