package runtimetui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

const (
	ruleFieldContent = "content"
	ruleFieldType    = "type"
	ruleFieldTarget  = "target"
)

type ruleFilterForm struct {
	*structuredForm
}

func newRuleFilterForm(query runtimeapi.RuleQuery) *ruleFilterForm {
	return &ruleFilterForm{structuredForm: newStructuredForm([]structuredField{
		{key: ruleFieldContent, label: "规则内容", value: query.Content, placeholder: "条件或完整规则片段"},
		{key: ruleFieldType, label: "规则类型", value: query.Type, placeholder: "例如 DOMAIN、IP-CIDR"},
		{key: ruleFieldTarget, label: "规则目标", value: query.Target, placeholder: "策略组、节点或 DIRECT"},
	})}
}

func (m *Model) openAppliedRules(query runtimeapi.RuleQuery) tea.Cmd {
	m.openPage(pageConfig)
	m.focusIndex = 4
	m.ruleViewerOpen = true
	m.ruleView = runtimeapi.RuleViewApplied
	m.ruleSet = runtimeapi.RuleSet{View: runtimeapi.RuleViewApplied, Items: []runtimeapi.FinalRule{}}
	m.ruleQuery = query
	m.ruleSelected = 0
	m.ruleGeneration++
	m.busy = true
	m.err = nil
	m.status = "正在读取当前运行配置的最终规则…"
	return m.rulesCmd()
}

func (m Model) rulesCmd() tea.Cmd {
	client, ok := m.client.(RuleClient)
	query := m.ruleQuery
	generation := m.ruleGeneration
	if !ok {
		return func() tea.Msg {
			return ruleSetErrorMsg{err: errors.New("Runtime 不支持最终规则接口"), query: query, generation: generation}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		rules, err := client.Rules(ctx, query)
		if err != nil {
			return ruleSetErrorMsg{err: err, query: query, generation: generation}
		}
		return ruleSetMsg{rules: rules, query: query, generation: generation}
	}
}

func (m Model) updateRuleViewer(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	if m.busy && key.String() != "esc" {
		return m, nil
	}
	switch key.String() {
	case "esc":
		m.ruleViewerOpen = false
		m.ruleFilter = nil
		m.ruleGeneration++
		m.busy = false
		m.err = nil
		m.status = "已关闭最终规则只读查看器"
		return m, nil
	case "up", "down":
		if len(m.ruleSet.Items) == 0 {
			return m, nil
		}
		offset := -1
		if key.String() == "down" {
			offset = 1
		}
		m.ruleSelected = (m.ruleSelected + offset + len(m.ruleSet.Items)) % len(m.ruleSet.Items)
		return m, nil
	case "/":
		m.ruleFilter = newRuleFilterForm(m.ruleQuery)
		m.status = "填写最终规则筛选；Ctrl+S 应用，Esc 取消"
		return m, m.ruleFilter.loadActiveField()
	case "c":
		m.ruleQuery = runtimeapi.RuleQuery{}
		m.ruleSelected = 0
		if m.ruleView == runtimeapi.RuleViewCandidate {
			m.ruleSet = filterRuleSet(m.preview.Rules, m.ruleQuery)
			m.status = "已清除候选规则筛选"
			return m, nil
		}
		m.ruleGeneration++
		m.busy = true
		m.ruleSet = runtimeapi.RuleSet{View: runtimeapi.RuleViewApplied, Items: []runtimeapi.FinalRule{}}
		m.status = "正在读取全部当前运行规则…"
		return m, m.rulesCmd()
	case "v", "tab":
		if m.ruleView == runtimeapi.RuleViewApplied {
			if !m.preview.Validated || m.preview.Rules.View != runtimeapi.RuleViewCandidate {
				m.err = errors.New("当前没有已校验且尚未应用的候选规则")
				m.status = m.err.Error()
				return m, nil
			}
			if m.ruleSet.ConfigurationSHA256 != "" &&
				strings.EqualFold(m.ruleSet.ConfigurationSHA256, m.preview.Rules.ConfigurationSHA256) {
				m.preview = runtimeapi.CandidatePreview{}
				m.previewSourceID = ""
				m.err = errors.New("最近候选已经是当前运行配置")
				m.status = m.err.Error()
				return m, nil
			}
			m.ruleGeneration++
			m.ruleView = runtimeapi.RuleViewCandidate
			m.ruleSet = filterRuleSet(m.preview.Rules, m.ruleQuery)
			m.ruleSelected = 0
			m.err = nil
			m.status = "正在查看尚未应用候选配置的最终规则"
			return m, nil
		}
		m.ruleView = runtimeapi.RuleViewApplied
		m.ruleSet = runtimeapi.RuleSet{View: runtimeapi.RuleViewApplied, Items: []runtimeapi.FinalRule{}}
		m.ruleSelected = 0
		m.ruleGeneration++
		m.busy = true
		m.err = nil
		m.status = "正在读取当前运行配置的最终规则…"
		return m, m.rulesCmd()
	}
	return m, nil
}

func (m Model) updateRuleFilter(message tea.Msg) (tea.Model, tea.Cmd) {
	form := m.ruleFilter
	if form == nil {
		return m, nil
	}
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			form.close()
			m.ruleFilter = nil
			m.status = "已取消最终规则筛选"
			return m, nil
		case "tab", "down", "enter":
			return m, form.move(1)
		case "shift+tab", "up":
			return m, form.move(-1)
		case "ctrl+s":
			form.commitActiveField()
			query := runtimeapi.RuleQuery{
				Content: strings.TrimSpace(form.value(ruleFieldContent)),
				Type:    strings.TrimSpace(form.value(ruleFieldType)),
				Target:  strings.TrimSpace(form.value(ruleFieldTarget)),
			}
			for _, value := range []string{query.Content, query.Type, query.Target} {
				if utf8.RuneCountInString(value) > runtimeapi.RuleFilterMaxLength {
					m.err = errors.New("最终规则筛选条件不能超过 256 个字符")
					m.status = m.err.Error()
					return m, nil
				}
			}
			form.close()
			m.ruleFilter = nil
			m.ruleQuery = query
			m.ruleSelected = 0
			m.err = nil
			if m.ruleView == runtimeapi.RuleViewCandidate {
				m.ruleSet = filterRuleSet(m.preview.Rules, query)
				m.status = "已应用候选规则筛选"
				return m, nil
			}
			m.ruleGeneration++
			m.busy = true
			m.ruleSet = runtimeapi.RuleSet{View: runtimeapi.RuleViewApplied, Items: []runtimeapi.FinalRule{}}
			m.status = "正在应用当前运行规则筛选…"
			return m, m.rulesCmd()
		}
	}
	var command tea.Cmd
	form.input, command = form.input.Update(message)
	return m, command
}

func filterRuleSet(source runtimeapi.RuleSet, query runtimeapi.RuleQuery) runtimeapi.RuleSet {
	content := strings.ToLower(strings.TrimSpace(query.Content))
	ruleType := strings.ToLower(strings.TrimSpace(query.Type))
	target := strings.ToLower(strings.TrimSpace(query.Target))
	result := source
	result.Items = make([]runtimeapi.FinalRule, 0, len(source.Items))
	for _, rule := range source.Items {
		if content != "" && !strings.Contains(strings.ToLower(rule.Content), content) {
			continue
		}
		if ruleType != "" && !strings.Contains(strings.ToLower(rule.Type), ruleType) {
			continue
		}
		if target != "" && !strings.Contains(strings.ToLower(rule.Target), target) {
			continue
		}
		result.Items = append(result.Items, rule)
	}
	result.Total = len(result.Items)
	return result
}

func (m Model) renderFinalRuleViewer() string {
	view := "当前运行配置"
	if m.ruleView == runtimeapi.RuleViewCandidate {
		view = "尚未应用候选配置"
	}
	lines := []string{
		titleStyle.Render("最终规则只读查看器"),
		fmt.Sprintf("数据：%s · 配置 %s · %s", view, shortDigest(m.ruleSet.ConfigurationSHA256), ruleQuerySummary(m.ruleQuery)),
		mutedStyle.Render("规则顺序只读；修改请使用 Runtime 配置来源、本机高级覆盖或已有控制面流程。"),
		"",
	}
	if m.busy && len(m.ruleSet.Items) == 0 {
		lines = append(lines, mutedStyle.Render("正在通过 Runtime 本机 IPC 读取规则…"))
	} else if len(m.ruleSet.Items) == 0 {
		lines = append(lines, mutedStyle.Render("没有符合当前筛选条件的最终规则。"))
	} else {
		start, end := ruleWindow(m.ruleSelected, len(m.ruleSet.Items), 12)
		for index := start; index < end; index++ {
			rule := m.ruleSet.Items[index]
			prefix := "  "
			if index == m.ruleSelected {
				prefix = "▶ "
			}
			lines = append(lines, fmt.Sprintf("%s#%-4d %-14s %-28s → %-16s · %s", prefix, rule.Order, rule.Type, truncateRuleText(rule.Condition, 28), truncateRuleText(rule.Target, 16), ruleOriginLabel(rule.Origin)))
		}
		selected := m.ruleSelected
		if selected < 0 || selected >= len(m.ruleSet.Items) {
			selected = 0
		}
		rule := m.ruleSet.Items[selected]
		lines = append(lines, "", "所选规则", fmt.Sprintf("顺序 #%d · 类型 %s · 条件 %s · 目标 %s · 来源 %s", rule.Order, rule.Type, rule.Condition, rule.Target, ruleOriginLabel(rule.Origin)), "内容 "+rule.Content)
	}
	lines = append(lines, "", "↑↓ 选择 · / 筛选 · v 切换当前/候选 · c 清除筛选 · Esc 关闭", renderStatus(m.status, m.err, m.busy))
	return strings.Join(lines, "\n")
}

func (m Model) renderRuleFilter() string {
	form := m.ruleFilter
	if form == nil {
		return ""
	}
	lines := []string{
		titleStyle.Render("最终规则筛选"),
		mutedStyle.Render("Tab / Shift+Tab 切换字段 · Ctrl+S 应用 · Esc 取消；留空表示不过滤"),
		"",
	}
	for index, field := range form.fields {
		prefix := "  "
		if index == form.index {
			prefix = "▶ "
		}
		value := field.value
		if index == form.index {
			value = form.input.View()
		} else if value == "" {
			value = mutedStyle.Render("不限")
		}
		lines = append(lines, fmt.Sprintf("%s%-12s %s", prefix, field.label, value))
	}
	lines = append(lines, "", renderStatus(m.status, m.err, m.busy))
	return strings.Join(lines, "\n")
}

func ruleWindow(selected, total, limit int) (int, int) {
	if total <= limit {
		return 0, total
	}
	start := selected - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > total {
		start = total - limit
	}
	return start, start + limit
}

func ruleQuerySummary(query runtimeapi.RuleQuery) string {
	parts := make([]string, 0, 3)
	for _, item := range []struct{ label, value string }{
		{label: "内容", value: query.Content},
		{label: "类型", value: query.Type},
		{label: "目标", value: query.Target},
	} {
		if item.value != "" {
			parts = append(parts, item.label+"="+item.value)
		}
	}
	if len(parts) == 0 {
		return "未筛选"
	}
	return strings.Join(parts, " · ")
}

func ruleOriginLabel(origin string) string {
	switch origin {
	case runtimeapi.RuleOriginSource:
		return "配置来源"
	case runtimeapi.RuleOriginAdvancedOverride:
		return "高级覆盖"
	case runtimeapi.RuleOriginRuntime:
		return "Runtime"
	case runtimeapi.RuleOriginCurrentConfiguration:
		return "当前运行配置"
	default:
		return valueOr(origin, "未知")
	}
}

func candidateRuleSummary(preview runtimeapi.CandidatePreview) string {
	if !preview.Validated || preview.Rules.View != runtimeapi.RuleViewCandidate {
		return "未生成"
	}
	return fmt.Sprintf("%d 条 · %s", preview.Rules.Total, shortDigest(preview.Rules.ConfigurationSHA256))
}

func truncateRuleText(value string, limit int) string {
	characters := []rune(value)
	if len(characters) <= limit {
		return value
	}
	if limit <= 1 {
		return string(characters[:limit])
	}
	return string(characters[:limit-1]) + "…"
}
