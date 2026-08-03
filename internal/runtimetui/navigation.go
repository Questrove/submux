package runtimetui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type pageID string

const (
	pageStatus      pageID = "status"
	pageConfig      pageID = "config"
	pageNetwork     pageID = "network"
	pageMonitor     pageID = "monitor"
	pageMaintenance pageID = "maintenance"
)

type pageDefinition struct {
	id      pageID
	number  string
	title   string
	regions []string
}

var pageDefinitions = []pageDefinition{
	{id: pageStatus, number: "1", title: "状态", regions: []string{"运行状态", "当前来源", "最近运行操作", "主要代理组"}},
	{id: pageConfig, number: "2", title: "配置", regions: []string{"配置来源", "候选配置", "本机配置层", "配置操作", "最终规则", "代理组与节点"}},
	{id: pageNetwork, number: "3", title: "网络", regions: []string{"当前网络", "网络预览", "网络操作"}},
	{id: pageMonitor, number: "4", title: "监控", regions: []string{"实时概况", "速度曲线", "活动连接", "Runtime 事件"}},
	{id: pageMaintenance, number: "5", title: "维护", regions: []string{"更新与回滚", "备份与恢复", "诊断", "运行操作"}},
}

func pageFromKey(key string) pageID {
	for _, page := range pageDefinitions {
		if page.number == key {
			return page.id
		}
	}
	return pageStatus
}

func pageForAction(kind string) pageID {
	switch kind {
	case runtimeapi.ActionApplyImportedConfig,
		runtimeapi.ActionAddRemoteSource,
		runtimeapi.ActionAddImportedSource,
		runtimeapi.ActionRefreshSource,
		runtimeapi.ActionApplySource,
		runtimeapi.ActionSwitchSource,
		runtimeapi.ActionDeleteSource,
		runtimeapi.ActionAddManagedResource,
		runtimeapi.ActionSetAdvancedOverride,
		runtimeapi.ActionSetTrafficPolicy,
		runtimeapi.ActionSelectProxyNode:
		return pageConfig
	case runtimeapi.ActionEnableTUN,
		runtimeapi.ActionDisableTUN,
		runtimeapi.ActionEnableGateway,
		runtimeapi.ActionDisableGateway:
		return pageNetwork
	case runtimeapi.ActionUpdateMihomo,
		runtimeapi.ActionRollbackMihomo,
		runtimeapi.ActionCheckProduct,
		runtimeapi.ActionUpdateProduct,
		runtimeapi.ActionRollbackProduct,
		runtimeapi.ActionRestoreBackup:
		return pageMaintenance
	default:
		return pageStatus
	}
}

func definitionFor(pageID pageID) pageDefinition {
	for _, page := range pageDefinitions {
		if page.id == pageID {
			return page
		}
	}
	return pageDefinitions[0]
}

func (m *Model) openPage(page pageID) {
	m.connectionsGeneration++
	m.connectionsPolling = false
	m.connectionFailures = 0
	m.page = definitionFor(page).id
	m.focusIndex = 0
	m.showHelp = false
	m.err = nil
	m.status = "已打开" + definitionFor(m.page).title + "页"
}

func (m *Model) moveFocus(offset int) {
	regions := definitionFor(m.page).regions
	if len(regions) == 0 {
		m.focusIndex = 0
		return
	}
	m.focusIndex = (m.focusIndex + offset + len(regions)) % len(regions)
	m.status = "焦点：" + regions[m.focusIndex]
}

func (m *Model) moveSelection(forward bool) tea.Cmd {
	if m.page == pageConfig && m.focusIndex == 0 {
		if len(m.snapshot.Sources.Items) == 0 {
			m.status = "当前区域没有可选择条目"
			return nil
		}
		m.selectAdjacentSource(forward)
		if source := m.selectedSourceSummary(); source != nil {
			m.status = "已选择来源：" + source.Name
		}
		return m.startProxyGroupsCmd(m.selectedSource())
	}
	if m.page == pageMonitor && m.focusIndex == 2 {
		if len(m.connectionPage.Items) == 0 {
			m.status = "当前筛选下没有活动连接"
			return nil
		}
		offset := -1
		if forward {
			offset = 1
		}
		m.connectionSelected = (m.connectionSelected + offset + len(m.connectionPage.Items)) % len(m.connectionPage.Items)
		m.status = "已选择活动连接：" + valueOr(m.connectionPage.Items[m.connectionSelected].Target, "未知目标")
		return nil
	}
	m.status = "当前区域没有可选择条目；使用 Tab 切换焦点"
	return nil
}

func (m *Model) activateFocus() tea.Cmd {
	switch m.page {
	case pageStatus:
		if m.focusIndex == 1 {
			m.openPage(pageConfig)
		}
		if m.focusIndex == 2 && (m.lastOperation.ID != "" || m.snapshot.Operations.CurrentOperationID != "") {
			updated, command := m.Update(commandKey("g"))
			*m = updated.(Model)
			return command
		}
		if m.focusIndex == 3 {
			return m.openProxyGroups(m.snapshot.Sources.CurrentSourceID)
		}
	case pageConfig:
		if m.focusIndex == 0 && m.selectedSource() != "" {
			updated, command := m.Update(commandKey("y"))
			*m = updated.(Model)
			return command
		}
		if m.focusIndex == 2 {
			updated, command := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
			*m = updated.(Model)
			return command
		}
		if m.focusIndex == 4 {
			return m.openAppliedRules(runtimeapi.RuleQuery{})
		}
		if m.focusIndex == 5 {
			return m.openProxyGroups(m.selectedSource())
		}
	case pageNetwork:
		if m.focusIndex == 2 {
			mode := m.snapshot.RunMode
			if !validNetworkMode(mode) {
				mode = runtimeapi.RunModeExplicit
			}
			m.networkForm = newNetworkForm(mode)
			m.err = nil
			m.status = "编辑运行方式；Ctrl+S 生成预览或进入统一确认"
			return m.networkForm.loadActiveField()
		}
	}
	m.status = "当前区域为只读；使用 Tab 切换焦点或 / 搜索操作"
	return nil
}

type paletteCommand struct {
	label    string
	keywords string
	key      string
	page     pageID
}

func paletteCommands() []paletteCommand {
	return []paletteCommand{
		{label: "打开状态页", keywords: "首页 概览 运行", page: pageStatus},
		{label: "打开配置页", keywords: "来源 候选 覆盖 资源", page: pageConfig},
		{label: "打开网络页", keywords: "TUN 网关 接管 路由 DNS", page: pageNetwork},
		{label: "打开监控页", keywords: "流量 连接 事件", page: pageMonitor},
		{label: "打开维护页", keywords: "更新 回滚 备份 恢复 诊断", page: pageMaintenance},
		{label: "刷新 Runtime 状态", keywords: "重新读取", key: "r"},
		{label: "启动 Mihomo", keywords: "代理 运行", key: "s"},
		{label: "停止 Mihomo 并恢复直连", keywords: "代理 停止", key: "x"},
		{label: "验证显式代理", keywords: "测试 可用性", key: "v"},
		{label: "选择显式代理运行方式", keywords: "网络 运行方式 直连", key: "ctrl+p"},
		{label: "配置普通 TUN", keywords: "网络 运行方式 路由 DNS", key: "ctrl+t"},
		{label: "配置 Linux 网关", keywords: "网络 运行方式 LAN", key: "ctrl+l"},
		{label: "添加远程配置来源", keywords: "订阅 URL", key: "u"},
		{label: "导入本机配置来源", keywords: "YAML 文件", key: "n"},
		{label: "预览所选来源", keywords: "候选 字段来源", key: "y"},
		{label: "查看或选择代理节点", keywords: "代理组 节点 延迟", key: "ctrl+n"},
		{label: "刷新所选来源", keywords: "下载", key: "f"},
		{label: "切换到所选来源", keywords: "应用", key: "t"},
		{label: "编辑本机高级覆盖", keywords: "YAML 敏感", key: "o"},
		{label: "预览或生成诊断包", keywords: "日志 故障", key: "ctrl+g"},
		{label: "检查 Mihomo 更新", keywords: "核心 升级", key: "U"},
		{label: "检查 Runtime 产品更新", keywords: "升级", key: "P"},
		{label: "创建脱敏备份清单", keywords: "导出", key: "b"},
	}
}

func (m Model) openPalette() (tea.Model, tea.Cmd) {
	m.paletteOpen = true
	m.paletteIndex = 0
	m.palette.SetValue("")
	m.showHelp = false
	return m, m.palette.Focus()
}

func (m Model) updatePalette(message tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc", "ctrl+k":
			m.paletteOpen = false
			m.palette.Blur()
			m.status = "已关闭命令搜索"
			return m, nil
		case "up":
			matches := m.filteredCommands()
			if len(matches) > 0 {
				m.paletteIndex = (m.paletteIndex + len(matches) - 1) % len(matches)
			}
			return m, nil
		case "down":
			matches := m.filteredCommands()
			if len(matches) > 0 {
				m.paletteIndex = (m.paletteIndex + 1) % len(matches)
			}
			return m, nil
		case "enter":
			matches := m.filteredCommands()
			if len(matches) == 0 {
				return m, nil
			}
			if m.paletteIndex >= len(matches) {
				m.paletteIndex = 0
			}
			command := matches[m.paletteIndex]
			m.paletteOpen = false
			m.palette.Blur()
			if command.page != "" {
				m.openPage(command.page)
				if command.page == pageMonitor {
					return m, m.trafficHistoryCmd(true)
				}
				if command.page == pageStatus {
					return m, m.startProxyGroupsCmd(m.snapshot.Sources.CurrentSourceID)
				}
				if command.page == pageConfig {
					return m, m.startProxyGroupsCmd(m.selectedSource())
				}
				return m, nil
			}
			return m.Update(commandKey(command.key))
		}
	}
	previous := m.palette.Value()
	var command tea.Cmd
	m.palette, command = m.palette.Update(message)
	if m.palette.Value() != previous {
		m.paletteIndex = 0
	}
	return m, command
}

func (m Model) filteredCommands() []paletteCommand {
	query := strings.ToLower(strings.TrimSpace(m.palette.Value()))
	all := paletteCommands()
	if query == "" {
		return all
	}
	words := strings.Fields(query)
	filtered := make([]paletteCommand, 0, len(all))
	for _, command := range all {
		haystack := strings.ToLower(command.label + " " + command.keywords + " " + command.key)
		matched := true
		for _, word := range words {
			if !strings.Contains(haystack, word) {
				matched = false
				break
			}
		}
		if matched {
			filtered = append(filtered, command)
		}
	}
	return filtered
}

func commandKey(binding string) tea.KeyPressMsg {
	if strings.HasPrefix(binding, "ctrl+") && len([]rune(binding)) == 6 {
		character := []rune(binding)[5]
		return tea.KeyPressMsg{Code: character, Mod: tea.ModCtrl}
	}
	runes := []rune(binding)
	if len(runes) == 1 {
		return tea.KeyPressMsg{Code: runes[0], Text: binding}
	}
	return tea.KeyPressMsg{}
}
