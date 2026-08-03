package runtimetui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

const (
	logPageModeInitial = "initial"
	logPageModeOlder   = "older"
	logPageModeFollow  = "follow"
	maxViewerLogItems  = 5000
)

type logFilterForm struct {
	text      textinput.Model
	level     textinput.Model
	component textinput.Model
	active    int
}

func newLogFilterForm(query runtimeapi.LogQuery) *logFilterForm {
	text := textinput.New()
	text.Prompt = "文本："
	text.Placeholder = "消息包含的文字"
	text.CharLimit = 256
	text.SetWidth(64)
	text.SetValue(query.Text)
	level := textinput.New()
	level.Prompt = "级别："
	level.Placeholder = "全部 / debug / info / warn / error"
	level.CharLimit = 8
	level.SetWidth(64)
	level.SetValue(query.Level)
	component := textinput.New()
	component.Prompt = "组件："
	component.Placeholder = "全部 / runtime / mihomo / network"
	component.CharLimit = 16
	component.SetWidth(64)
	component.SetValue(query.Component)
	form := &logFilterForm{text: text, level: level, component: component}
	form.focusActive()
	return form
}

func (form *logFilterForm) focusActive() tea.Cmd {
	form.text.Blur()
	form.level.Blur()
	form.component.Blur()
	switch form.active {
	case 1:
		return form.level.Focus()
	case 2:
		return form.component.Focus()
	default:
		return form.text.Focus()
	}
}

func (form *logFilterForm) update(message tea.Msg) tea.Cmd {
	var command tea.Cmd
	switch form.active {
	case 1:
		form.level, command = form.level.Update(message)
	case 2:
		form.component, command = form.component.Update(message)
	default:
		form.text, command = form.text.Update(message)
	}
	return command
}

func (m *Model) startLogViewer(query runtimeapi.LogQuery) tea.Cmd {
	m.page = pageMaintenance
	m.focusIndex = 4
	m.logViewerOpen = true
	m.logFilter = nil
	m.logPaused = false
	m.logStale = false
	m.logFault = nil
	m.logFailures = 0
	m.logEntries = nil
	m.logHasOlder = false
	m.logSelected = 0
	m.logGeneration++
	query.Before = 0
	query.After = 0
	query.Limit = runtimeapi.LogPageDefaultSize
	m.logQuery = query
	m.status = "正在读取最近 200 条脱敏日志…"
	return m.startLogRequest(logPageModeInitial)
}

func (m *Model) startLogRequest(mode string) tea.Cmd {
	client, ok := m.client.(LogClient)
	if !ok {
		m.logLoading = false
		m.logStale = true
		m.logFault = errors.New("当前 TUI 客户端不支持日志查看")
		m.status = m.logFault.Error()
		return nil
	}
	query := m.logQuery
	query.Before = 0
	query.After = 0
	query.Limit = runtimeapi.LogPageDefaultSize
	switch mode {
	case logPageModeOlder:
		if len(m.logEntries) == 0 {
			return nil
		}
		query.Before = m.logEntries[0].Cursor
	case logPageModeFollow:
		if len(m.logEntries) > 0 {
			query.After = m.logEntries[len(m.logEntries)-1].Cursor
		}
	}
	m.logLoading = true
	generation := m.logGeneration
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		defer cancel()
		page, err := client.Logs(ctx, query)
		if err != nil {
			return logPageErrorMsg{err: err, mode: mode, generation: generation}
		}
		return logPageMsg{page: page, mode: mode, generation: generation}
	}
}

func (m Model) acceptLogPage(message logPageMsg) (tea.Model, tea.Cmd) {
	if !m.logViewerOpen || message.generation != m.logGeneration {
		return m, nil
	}
	m.logLoading = false
	m.logStale = false
	m.logFault = nil
	m.logFailures = 0
	m.logLastSuccess = message.page.ObservedAt
	if m.logLastSuccess.IsZero() {
		m.logLastSuccess = time.Now()
	}
	switch message.mode {
	case logPageModeOlder:
		m.logEntries = mergeLogEntries(message.page.Items, m.logEntries)
		m.logHasOlder = message.page.HasOlder
	case logPageModeFollow:
		if message.page.ResetRequired {
			m.logEntries = append([]runtimeapi.LogEntry(nil), message.page.Items...)
			m.logHasOlder = message.page.HasOlder
		} else {
			m.logEntries = mergeLogEntries(m.logEntries, message.page.Items)
		}
	default:
		m.logEntries = append([]runtimeapi.LogEntry(nil), message.page.Items...)
		m.logHasOlder = message.page.HasOlder
	}
	if len(m.logEntries) > maxViewerLogItems {
		m.logEntries = append([]runtimeapi.LogEntry(nil), m.logEntries[len(m.logEntries)-maxViewerLogItems:]...)
		m.logHasOlder = true
	}
	if len(m.logEntries) > 0 {
		m.logSelected = len(m.logEntries) - 1
	} else {
		m.logSelected = 0
	}
	m.status = fmt.Sprintf("日志已更新 · %d 条", len(m.logEntries))
	if m.logPaused {
		return m, nil
	}
	if message.mode == logPageModeFollow && message.page.HasNewer {
		return m, m.startLogRequest(logPageModeFollow)
	}
	return m, logTickCmd(m.logGeneration, 0)
}

func (m Model) acceptLogPageError(message logPageErrorMsg) (tea.Model, tea.Cmd) {
	if !m.logViewerOpen || message.generation != m.logGeneration {
		return m, nil
	}
	m.logLoading = false
	m.logStale = true
	m.logFault = message.err
	m.logFailures++
	m.status = "日志读取失败，保留上次成功数据"
	if m.logPaused {
		return m, nil
	}
	return m, logTickCmd(m.logGeneration, m.logFailures)
}

func logTickCmd(generation uint64, failures int) tea.Cmd {
	delay := time.Second
	if failures > 0 {
		delay = time.Duration(failures+1) * time.Second
		if delay > 5*time.Second {
			delay = 5 * time.Second
		}
	}
	return tea.Tick(delay, func(time.Time) tea.Msg {
		return logTickMsg{generation: generation}
	})
}

func (m Model) updateLogViewer(message tea.Msg) (tea.Model, tea.Cmd) {
	if m.logFilter != nil {
		return m.updateLogFilter(message)
	}
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "esc":
		m.logViewerOpen = false
		m.logGeneration++
		m.logLoading = false
		m.status = "已关闭日志查看器"
		return m, nil
	case "space", " ":
		m.logPaused = !m.logPaused
		m.logGeneration++
		m.logLoading = false
		if m.logPaused {
			m.status = "日志跟随已暂停；Runtime 仍继续记录日志"
			return m, nil
		}
		m.status = "正在从暂停位置追赶日志…"
		return m, m.startLogRequest(logPageModeFollow)
	case "m", "pgup":
		if m.logLoading || !m.logHasOlder {
			m.status = "当前没有更早日志可加载"
			return m, nil
		}
		m.status = "正在加载更早日志…"
		return m, m.startLogRequest(logPageModeOlder)
	case "f":
		m.logFilter = newLogFilterForm(m.logQuery)
		m.status = "编辑日志筛选；Tab 切换字段，Ctrl+S 应用，Esc 取消"
		return m, m.logFilter.focusActive()
	case "r":
		m.logGeneration++
		m.logEntries = nil
		m.logHasOlder = false
		m.logSelected = 0
		m.logStale = false
		m.logFault = nil
		m.status = "正在重新读取最近 200 条日志…"
		return m, m.startLogRequest(logPageModeInitial)
	case "up":
		if m.logSelected > 0 {
			m.logSelected--
		}
		return m, nil
	case "down":
		if m.logSelected+1 < len(m.logEntries) {
			m.logSelected++
		}
		return m, nil
	case "home":
		m.logSelected = 0
		return m, nil
	case "end":
		if len(m.logEntries) > 0 {
			m.logSelected = len(m.logEntries) - 1
		}
		return m, nil
	}
	return m, nil
}

func (m Model) updateLogFilter(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyPressMsg)
	if ok {
		switch key.String() {
		case "esc":
			m.logFilter = nil
			m.status = "已取消日志筛选修改"
			return m, nil
		case "tab":
			m.logFilter.active = (m.logFilter.active + 1) % 3
			return m, m.logFilter.focusActive()
		case "shift+tab":
			m.logFilter.active = (m.logFilter.active + 2) % 3
			return m, m.logFilter.focusActive()
		case "ctrl+s":
			text := strings.TrimSpace(m.logFilter.text.Value())
			level := strings.ToLower(strings.TrimSpace(m.logFilter.level.Value()))
			component := strings.ToLower(strings.TrimSpace(m.logFilter.component.Value()))
			if level == "all" || level == "全部" {
				level = ""
			}
			if component == "all" || component == "全部" {
				component = ""
			}
			if !validTUILogLevel(level) || !validTUILogComponent(component) {
				m.status = "日志级别或组件无效，请使用表单提示中的值"
				return m, nil
			}
			m.logQuery.Text = text
			m.logQuery.Level = level
			m.logQuery.Component = component
			m.logQuery.Before = 0
			m.logQuery.After = 0
			m.logQuery.Limit = runtimeapi.LogPageDefaultSize
			m.logFilter = nil
			m.logGeneration++
			m.logEntries = nil
			m.logHasOlder = false
			m.logSelected = 0
			m.logStale = false
			m.logFault = nil
			m.status = "正在应用日志筛选…"
			return m, m.startLogRequest(logPageModeInitial)
		}
	}
	return m, m.logFilter.update(message)
}

func (m Model) renderLogViewer() string {
	if m.logFilter != nil {
		return strings.Join([]string{
			titleStyle.Render("日志筛选"),
			m.logFilter.text.View(),
			m.logFilter.level.View(),
			m.logFilter.component.View(),
			"",
			"Tab/Shift+Tab 切换字段 · Ctrl+S 应用 · Esc 取消",
		}, "\n")
	}
	state := okStyle.Render("跟随中")
	if m.logPaused {
		state = warnStyle.Render("已暂停")
	}
	if m.logLoading {
		state += " · 正在读取"
	}
	lines := []string{
		titleStyle.Render("脱敏日志"),
		fmt.Sprintf("%s · %d 条 · %s", state, len(m.logEntries), renderLogFilters(m.logQuery)),
	}
	if !m.logQuery.Since.IsZero() || !m.logQuery.Until.IsZero() {
		lines = append(lines, fmt.Sprintf("时间范围 %s — %s", formatOperationTime(m.logQuery.Since), formatOperationTime(m.logQuery.Until)))
	}
	if m.logStale {
		last := "尚无成功记录"
		if !m.logLastSuccess.IsZero() {
			last = "上次成功 " + formatOperationTime(m.logLastSuccess)
		}
		message := "日志读取失败"
		if m.logFault != nil {
			message += "：" + publicErrorMessage(m.logFault)
		}
		lines = append(lines, errorStyle.Render(message+" · "+last+" · 当前数据可能过期"))
	} else if !m.logLastSuccess.IsZero() {
		lines = append(lines, mutedStyle.Render("上次成功 "+formatOperationTime(m.logLastSuccess)))
	}
	if len(m.logEntries) == 0 {
		lines = append(lines, "", mutedStyle.Render("当前筛选没有日志"))
	} else {
		rows := m.height - 12
		if rows < 6 {
			rows = 6
		}
		if rows > 30 {
			rows = 30
		}
		start := m.logSelected - rows + 1
		if start < 0 {
			start = 0
		}
		end := start + rows
		if end > len(m.logEntries) {
			end = len(m.logEntries)
		}
		lines = append(lines, "")
		for index := start; index < end; index++ {
			entry := m.logEntries[index]
			prefix := "  "
			if index == m.logSelected {
				prefix = "▶ "
			}
			message := strings.ReplaceAll(entry.Message, "\n", " ")
			width := m.width - 44
			if width < 24 {
				width = 24
			}
			if len([]rune(message)) > width {
				message = string([]rune(message)[:width-1]) + "…"
			}
			lines = append(lines, fmt.Sprintf("%s%s  %-5s  %s/%s  %s", prefix, entry.At.Local().Format("15:04:05.000"), strings.ToUpper(entry.Level), entry.Component, entry.Stream, message))
		}
	}
	older := "没有更早日志"
	if m.logHasOlder {
		older = "m/PageUp 加载更早"
	}
	lines = append(lines, "", older+" · ↑↓ 选择 · f 筛选 · Space 暂停/继续 · r 刷新 · Esc 返回")
	return strings.Join(lines, "\n")
}

func mergeLogEntries(left, right []runtimeapi.LogEntry) []runtimeapi.LogEntry {
	byCursor := make(map[uint64]runtimeapi.LogEntry, len(left)+len(right))
	for _, entry := range left {
		if entry.Cursor != 0 {
			byCursor[entry.Cursor] = entry
		}
	}
	for _, entry := range right {
		if entry.Cursor != 0 {
			byCursor[entry.Cursor] = entry
		}
	}
	result := make([]runtimeapi.LogEntry, 0, len(byCursor))
	for _, entry := range byCursor {
		result = append(result, entry)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Cursor < result[right].Cursor })
	return result
}

func renderLogFilters(query runtimeapi.LogQuery) string {
	parts := make([]string, 0, 3)
	if query.Component != "" {
		parts = append(parts, "组件 "+query.Component)
	}
	if query.Level != "" {
		parts = append(parts, "级别 "+query.Level)
	}
	if query.Text != "" {
		parts = append(parts, "文本 "+query.Text)
	}
	if len(parts) == 0 {
		return "全部组件和级别"
	}
	return strings.Join(parts, " · ")
}

func validTUILogLevel(value string) bool {
	return value == "" || value == runtimeapi.LogLevelDebug || value == runtimeapi.LogLevelInfo || value == runtimeapi.LogLevelWarn || value == runtimeapi.LogLevelError
}

func validTUILogComponent(value string) bool {
	return value == "" || value == runtimeapi.LogComponentRuntime || value == runtimeapi.LogComponentMihomo || value == runtimeapi.LogComponentNetwork
}
