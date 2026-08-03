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

const (
	connectionFieldTarget  = "target"
	connectionFieldProcess = "process"
	connectionFieldRule    = "rule"
	connectionFieldNode    = "node"
)

type connectionFilterForm struct {
	*structuredForm
}

func newConnectionFilterForm(query runtimeapi.ConnectionQuery) *connectionFilterForm {
	return &connectionFilterForm{structuredForm: newStructuredForm([]structuredField{
		{key: connectionFieldTarget, label: "目标地址", value: query.Target, placeholder: "域名、IP 或端口"},
		{key: connectionFieldProcess, label: "进程名称", value: query.Process, placeholder: "进程名称"},
		{key: connectionFieldRule, label: "匹配规则", value: query.Rule, placeholder: "规则类型或规则内容"},
		{key: connectionFieldNode, label: "出站节点", value: query.Node, placeholder: "出站链中的节点名称"},
	})}
}

func (m Model) connectionsCmd() tea.Cmd {
	client, ok := m.client.(ConnectionClient)
	query := m.connectionQuery
	generation := m.connectionsGeneration
	if !ok {
		return func() tea.Msg {
			return connectionPageErrorMsg{err: errors.New("Runtime 不支持活动连接接口"), query: query, generation: generation}
		}
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		page, err := client.Connections(ctx, query)
		if err != nil {
			return connectionPageErrorMsg{err: err, query: query, generation: generation}
		}
		return connectionPageMsg{page: page, query: query, generation: generation}
	}
}

func connectionTickCmd(generation uint64, failures int) tea.Cmd {
	return tea.Tick(connectionRetryDelay(failures), func(time.Time) tea.Msg { return connectionTickMsg{generation: generation} })
}

func connectionRetryDelay(failures int) time.Duration {
	delay := time.Second
	for attempt := 1; attempt < failures && delay < 5*time.Second; attempt++ {
		delay *= 2
	}
	if delay > 5*time.Second {
		delay = 5 * time.Second
	}
	return delay
}

func (m *Model) changeConnectionPage(forward bool) bool {
	page := m.connectionQuery.Page
	if page <= 0 {
		page = 1
	}
	pageSize := m.connectionQuery.PageSize
	if pageSize <= 0 {
		pageSize = runtimeapi.ConnectionPageDefaultSize
	}
	lastPage := 1
	if m.connectionPage.Total > 0 {
		lastPage = (m.connectionPage.Total + pageSize - 1) / pageSize
	}
	if forward {
		if page >= lastPage {
			m.status = "已经是活动连接最后一页"
			return false
		}
		page++
	} else {
		if page <= 1 {
			m.status = "已经是活动连接第一页"
			return false
		}
		page--
	}
	m.connectionQuery.Page = page
	m.connectionsGeneration++
	m.connectionsPolling = true
	m.connectionFailures = 0
	m.connectionSelected = 0
	m.connectionStale = false
	m.connectionFault = nil
	m.status = fmt.Sprintf("正在读取活动连接第 %d 页…", page)
	return true
}

func (m Model) updateConnectionFilter(message tea.Msg) (tea.Model, tea.Cmd) {
	form := m.connectionFilter
	if form == nil {
		return m, nil
	}
	if key, ok := message.(tea.KeyPressMsg); ok {
		switch key.String() {
		case "esc":
			form.close()
			m.connectionFilter = nil
			m.err = nil
			m.status = "已取消活动连接筛选"
			return m, nil
		case "tab", "down", "enter":
			return m, form.move(1)
		case "shift+tab", "up":
			return m, form.move(-1)
		case "ctrl+s":
			form.commitActiveField()
			m.connectionQuery.Target = strings.TrimSpace(form.value(connectionFieldTarget))
			m.connectionQuery.Process = strings.TrimSpace(form.value(connectionFieldProcess))
			m.connectionQuery.Rule = strings.TrimSpace(form.value(connectionFieldRule))
			m.connectionQuery.Node = strings.TrimSpace(form.value(connectionFieldNode))
			m.connectionQuery.Page = 1
			if m.connectionQuery.PageSize <= 0 {
				m.connectionQuery.PageSize = runtimeapi.ConnectionPageDefaultSize
			}
			form.close()
			m.connectionFilter = nil
			m.connectionsGeneration++
			m.connectionsPolling = true
			m.connectionFailures = 0
			m.connectionSelected = 0
			m.connectionStale = false
			m.connectionFault = nil
			m.err = nil
			m.status = "正在应用活动连接筛选…"
			return m, m.connectionsCmd()
		}
	}
	var command tea.Cmd
	form.input, command = form.input.Update(message)
	return m, command
}

func (m Model) renderConnectionFilter() string {
	form := m.connectionFilter
	if form == nil {
		return ""
	}
	lines := []string{
		titleStyle.Render("活动连接筛选"),
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

func connectionFilterSummary(query runtimeapi.ConnectionQuery) string {
	filters := make([]string, 0, 4)
	for _, filter := range []struct {
		label string
		value string
	}{
		{label: "目标", value: query.Target},
		{label: "进程", value: query.Process},
		{label: "规则", value: query.Rule},
		{label: "节点", value: query.Node},
	} {
		if filter.value != "" {
			filters = append(filters, filter.label+"="+filter.value)
		}
	}
	if len(filters) == 0 {
		return "无筛选"
	}
	return strings.Join(filters, " · ")
}
