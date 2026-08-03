package runtimetui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type ProxyGroupClient interface {
	ProxyGroups(context.Context, runtimeapi.ProxyGroupQuery) (runtimeapi.ProxyGroupList, error)
}

type proxyGroupListMsg struct {
	list       runtimeapi.ProxyGroupList
	sourceID   string
	generation uint64
}

type proxyGroupListErrorMsg struct {
	err        error
	sourceID   string
	generation uint64
}

func (m *Model) startProxyGroupsCmd(sourceID string) tea.Cmd {
	client, ok := m.client.(ProxyGroupClient)
	if !ok || strings.TrimSpace(sourceID) == "" {
		return nil
	}
	m.proxyGroupGeneration++
	m.proxyGroupLoading = true
	generation := m.proxyGroupGeneration
	return m.proxyGroupsCmd(client, sourceID, generation)
}

func (m Model) proxyGroupsCmd(client ProxyGroupClient, sourceID string, generation uint64) tea.Cmd {
	return func() tea.Msg {
		list, err := client.ProxyGroups(m.ctx, runtimeapi.ProxyGroupQuery{SourceID: sourceID})
		if err != nil {
			return proxyGroupListErrorMsg{err: err, sourceID: sourceID, generation: generation}
		}
		return proxyGroupListMsg{list: list, sourceID: sourceID, generation: generation}
	}
}

func (m *Model) acceptProxyGroupList(message proxyGroupListMsg) bool {
	if message.generation != m.proxyGroupGeneration || message.sourceID == "" {
		return false
	}
	m.proxyGroups = message.list
	m.proxyGroupLoading = false
	m.proxyGroupFault = nil
	m.syncProxyGroupSelection()
	return true
}

func (m *Model) acceptProxyGroupError(message proxyGroupListErrorMsg) bool {
	if message.generation != m.proxyGroupGeneration || message.sourceID == "" {
		return false
	}
	m.proxyGroupLoading = false
	m.proxyGroupFault = message.err
	return true
}

func (m *Model) syncProxyGroupSelection() {
	if len(m.proxyGroups.Groups) == 0 {
		m.proxyGroupSelected = 0
		m.proxyNodeSelected = 0
		return
	}
	if m.proxyGroupSelected < 0 || m.proxyGroupSelected >= len(m.proxyGroups.Groups) {
		m.proxyGroupSelected = 0
	}
	for index := range m.proxyGroups.Groups {
		if m.proxyGroups.Groups[index].Main {
			m.proxyGroupSelected = index
			break
		}
	}
	m.selectCurrentProxyNode()
}

func (m *Model) selectCurrentProxyNode() {
	group := m.selectedProxyGroup()
	if group == nil || len(group.Nodes) == 0 {
		m.proxyNodeSelected = 0
		return
	}
	m.proxyNodeSelected = 0
	for index := range group.Nodes {
		if group.Nodes[index].Name == group.Current {
			m.proxyNodeSelected = index
			return
		}
	}
}

func (m *Model) openProxyGroups(sourceID string) tea.Cmd {
	if _, ok := m.client.(ProxyGroupClient); !ok {
		m.err = errors.New("当前 TUI 客户端不支持代理组查看")
		m.status = m.err.Error()
		return nil
	}
	if sourceID == "" {
		m.err = errors.New("当前没有可查看的配置来源")
		m.status = m.err.Error()
		return nil
	}
	m.proxyGroupOpen = true
	m.proxyGroupSelected = 0
	m.proxyNodeSelected = 0
	m.err = nil
	m.status = "正在通过 Runtime 读取代理组和节点…"
	if m.proxyGroups.SourceID == sourceID && len(m.proxyGroups.Groups) > 0 {
		m.proxyGroupLoading = false
		m.syncProxyGroupSelection()
		m.status = "Tab 切换代理组，↑↓ 选择节点，Enter 进入统一确认"
		return nil
	}
	m.proxyGroups = runtimeapi.ProxyGroupList{SourceID: sourceID}
	return m.startProxyGroupsCmd(sourceID)
}

func (m Model) updateProxyGroups(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "esc", "ctrl+n":
		m.proxyGroupOpen = false
		m.status = "已关闭代理组选择"
		return m, nil
	case "r":
		m.status = "正在刷新代理组和节点…"
		return m, m.startProxyGroupsCmd(m.proxyGroups.SourceID)
	case "tab", "right", "shift+tab", "left":
		if len(m.proxyGroups.Groups) == 0 {
			return m, nil
		}
		offset := 1
		if key.String() == "shift+tab" || key.String() == "left" {
			offset = -1
		}
		m.proxyGroupSelected = (m.proxyGroupSelected + offset + len(m.proxyGroups.Groups)) % len(m.proxyGroups.Groups)
		m.selectCurrentProxyNode()
		m.status = "已选择代理组：" + m.proxyGroups.Groups[m.proxyGroupSelected].Name
		return m, nil
	case "up", "down":
		group := m.selectedProxyGroup()
		if group == nil || len(group.Nodes) == 0 {
			return m, nil
		}
		offset := -1
		if key.String() == "down" {
			offset = 1
		}
		m.proxyNodeSelected = (m.proxyNodeSelected + offset + len(group.Nodes)) % len(group.Nodes)
		m.status = "已选择节点：" + group.Nodes[m.proxyNodeSelected].Name
		return m, nil
	case "enter":
		group := m.selectedProxyGroup()
		node := m.selectedProxyNode()
		if group == nil || node == nil {
			m.status = "当前没有可选择的代理节点"
			return m, nil
		}
		if !m.proxyGroups.CurrentSource {
			m.err = errors.New("请先把该配置来源切换为当前来源")
			m.status = m.err.Error()
			return m, nil
		}
		if !group.Selectable {
			m.err = errors.New("该代理组由 Mihomo 自动选择，不能手动切换")
			m.status = m.err.Error()
			return m, nil
		}
		if !node.Available {
			m.err = errors.New(valueOr(node.UnavailableReason, "该节点当前不可用"))
			m.status = m.err.Error()
			return m, nil
		}
		m.proxyGroupOpen = false
		m.prepareAction(runtimeapi.Action{
			Kind: runtimeapi.ActionSelectProxyNode,
			Params: runtimeapi.ActionParams{
				SourceID:   m.proxyGroups.SourceID,
				ProxyGroup: group.Name,
				ProxyNode:  node.Name,
			},
		})
		return m, nil
	}
	return m, nil
}

func (m Model) renderProxyGroupViewer() string {
	lines := []string{
		titleStyle.Render("代理组与节点"),
		fmt.Sprintf("来源 %s · %s", valueOr(m.proxyGroups.SourceID, "未知"), proxyGroupSourceState(m.proxyGroups)),
		"Tab/←→ 切换代理组 · ↑↓ 选择节点 · Enter 确认选择 · r 刷新 · Esc 关闭",
		"",
	}
	if m.proxyGroupLoading {
		lines = append(lines, warnStyle.Render("正在从 Runtime 本机 IPC 读取代理组…"))
	}
	if m.proxyGroupFault != nil {
		lines = append(lines, errorStyle.Render("代理组读取失败："+publicErrorMessage(m.proxyGroupFault)))
	}
	if m.proxyGroups.Message != "" {
		lines = append(lines, warnStyle.Render(m.proxyGroups.Message))
	}
	if len(m.proxyGroups.Groups) == 0 && !m.proxyGroupLoading {
		lines = append(lines, mutedStyle.Render("该来源没有可显示的代理组"))
	}
	for index, group := range m.proxyGroups.Groups {
		prefix := "  "
		if index == m.proxyGroupSelected {
			prefix = "▶ "
		}
		main := ""
		if group.Main {
			main = " · 主代理组"
		}
		mode := "自动"
		if group.Selectable {
			mode = "可手动选择"
		}
		lines = append(lines, fmt.Sprintf("%s%s · %s · 当前 %s · %s%s", prefix, group.Name, group.Type, valueOr(group.Current, "未选择"), mode, main))
	}
	if group := m.selectedProxyGroup(); group != nil {
		lines = append(lines, "", labelStyle.Render("节点 · "+group.Name))
		for index, node := range group.Nodes {
			prefix := "  "
			if index == m.proxyNodeSelected {
				prefix = "▶ "
			}
			current := ""
			if node.Name == group.Current {
				current = " [当前]"
			}
			line := fmt.Sprintf("%s%s%s · %s", prefix, node.Name, current, valueOr(node.Type, "未知类型"))
			if node.DelayTestedAt != nil {
				line += fmt.Sprintf(" · %d ms · %s", node.DelayMillis, node.DelayTestedAt.Local().Format("15:04:05"))
				if node.DelayStale {
					line += " · 已过期"
				}
			}
			if !node.Available {
				line += " · 不可用：" + valueOr(node.UnavailableReason, "未知原因")
			}
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) selectedProxyGroup() *runtimeapi.ProxyGroupStatus {
	if m.proxyGroupSelected < 0 || m.proxyGroupSelected >= len(m.proxyGroups.Groups) {
		return nil
	}
	return &m.proxyGroups.Groups[m.proxyGroupSelected]
}

func (m Model) selectedProxyNode() *runtimeapi.ProxyNodeStatus {
	group := m.selectedProxyGroup()
	if group == nil || m.proxyNodeSelected < 0 || m.proxyNodeSelected >= len(group.Nodes) {
		return nil
	}
	return &group.Nodes[m.proxyNodeSelected]
}

func (m Model) mainProxyGroup() *runtimeapi.ProxyGroupStatus {
	for index := range m.proxyGroups.Groups {
		if m.proxyGroups.Groups[index].Main {
			return &m.proxyGroups.Groups[index]
		}
	}
	if len(m.proxyGroups.Groups) > 0 {
		return &m.proxyGroups.Groups[0]
	}
	return nil
}

func proxyGroupSourceState(list runtimeapi.ProxyGroupList) string {
	if !list.CurrentSource {
		return "非当前来源，只读"
	}
	if list.Available {
		return "Mihomo 状态已同步"
	}
	return "使用 Runtime 保存状态"
}

func proxyNodeDelay(node *runtimeapi.ProxyNodeStatus) string {
	if node == nil || node.DelayTestedAt == nil {
		return "尚无延迟结果"
	}
	state := fmt.Sprintf("%d ms · %s", node.DelayMillis, node.DelayTestedAt.Local().Format("15:04:05"))
	if node.DelayStale || time.Since(*node.DelayTestedAt) > 5*time.Minute {
		state += " · 已过期"
	}
	return state
}

func (m Model) renderMainProxyGroupSummary(sourceID string) []string {
	if sourceID == "" {
		return []string{mutedStyle.Render("尚未选择 Runtime 配置来源")}
	}
	if m.proxyGroups.SourceID != sourceID {
		return []string{warnStyle.Render("正在通过 Runtime 读取当前代理组…")}
	}
	if m.proxyGroupFault != nil {
		return []string{warnStyle.Render("代理组读取失败：" + publicErrorMessage(m.proxyGroupFault))}
	}
	group := m.mainProxyGroup()
	if group == nil {
		return []string{mutedStyle.Render("当前来源没有代理组")}
	}
	var currentNode *runtimeapi.ProxyNodeStatus
	for index := range group.Nodes {
		if group.Nodes[index].Name == group.Current {
			currentNode = &group.Nodes[index]
			break
		}
	}
	availability := "可用"
	if currentNode != nil && !currentNode.Available {
		availability = "不可用：" + valueOr(currentNode.UnavailableReason, "未知原因")
	} else if !m.proxyGroups.Available && m.proxyGroups.Message != "" {
		availability = m.proxyGroups.Message
	}
	return []string{
		fmt.Sprintf("%s · 当前 %s · %s", group.Name, valueOr(group.Current, "未选择"), availability),
		"最近延迟  " + proxyNodeDelay(currentNode),
		mutedStyle.Render("Enter 或 Ctrl+N 快速切换节点；提交仍需统一确认"),
	}
}

func (m Model) renderConfigProxyGroupSummary(sourceID string) []string {
	if sourceID == "" {
		return []string{mutedStyle.Render("选择一个配置来源后查看代理组")}
	}
	if m.proxyGroups.SourceID != sourceID {
		return []string{warnStyle.Render("正在通过 Runtime 读取所选来源的代理组…")}
	}
	if m.proxyGroupFault != nil {
		return []string{warnStyle.Render("代理组读取失败：" + publicErrorMessage(m.proxyGroupFault))}
	}
	if len(m.proxyGroups.Groups) == 0 {
		return []string{mutedStyle.Render("所选来源没有代理组")}
	}
	lines := make([]string, 0, len(m.proxyGroups.Groups)*2+2)
	for _, group := range m.proxyGroups.Groups {
		mode := "自动"
		if group.Selectable {
			mode = "可选择"
		}
		main := ""
		if group.Main {
			main = " · 主代理组"
		}
		lines = append(lines, fmt.Sprintf("%s · %s · 当前 %s · %d 个节点%s", group.Name, mode, valueOr(group.Current, "未选择"), len(group.Nodes), main))
		for _, node := range group.Nodes {
			current := ""
			if node.Name == group.Current {
				current = " [当前]"
			}
			availability := "可用"
			if !node.Available {
				availability = "不可用：" + valueOr(node.UnavailableReason, "未知原因")
			}
			line := fmt.Sprintf("  · %s%s · %s · %s", node.Name, current, valueOr(node.Type, "未知类型"), availability)
			if node.DelayTestedAt != nil {
				line += fmt.Sprintf(" · %d ms · %s", node.DelayMillis, node.DelayTestedAt.Local().Format("15:04:05"))
				if node.DelayStale {
					line += " · 已过期"
				}
			}
			lines = append(lines, line)
		}
	}
	if m.proxyGroups.Message != "" {
		lines = append(lines, warnStyle.Render(m.proxyGroups.Message))
	}
	lines = append(lines, mutedStyle.Render("Enter 或 Ctrl+N 打开节点选择器；提交仍需统一确认"))
	return lines
}
