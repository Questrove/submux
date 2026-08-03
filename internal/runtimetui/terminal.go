package runtimetui

import (
	"os"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

type terminalCapabilities struct {
	Profile colorprofile.Profile
	NoColor bool
	ASCII   bool
}

var asciiGlyphs = strings.NewReplacer(
	"●", "*",
	"○", "o",
	"✓", "+",
	"▶", ">",
	"│", "|",
	"↑", "^",
	"↓", "v",
	"←", "<",
	"→", ">",
	"▁", ".",
	"▂", "_",
	"▃", ":",
	"▄", "-",
	"▅", "=",
	"▆", "+",
	"▇", "*",
	"█", "#",
	"•", "*",
)

func defaultTerminalCapabilities() terminalCapabilities {
	return terminalCapabilitiesFromEnvironment(os.Environ())
}

func terminalCapabilitiesFromEnvironment(environment []string) terminalCapabilities {
	values := make(map[string]string, len(environment))
	for _, item := range environment {
		key, value, found := strings.Cut(item, "=")
		if !found {
			continue
		}
		values[strings.ToUpper(strings.TrimSpace(key))] = value
	}
	_, noColorPresent := values["NO_COLOR"]
	noColor := noColorPresent && values["NO_COLOR"] != ""
	termDumb := strings.EqualFold(strings.TrimSpace(values["TERM"]), "dumb")
	ascii := termDumb || environmentBool(values["SUBMUX_ASCII"])
	profile := colorprofile.Env(environment)
	if noColor || termDumb {
		profile = colorprofile.ASCII
	}
	return terminalCapabilities{Profile: profile, NoColor: noColor || termDumb, ASCII: ascii}
}

func environmentBool(value string) bool {
	enabled, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && enabled
}

func (m Model) terminalView(content string) tea.View {
	return m.renderTerminal(content, true)
}

func (m Model) pageTerminalView(content string) tea.View {
	fitHeight := resolveTerminalLayout(m.width, m.height) != terminalLayoutColumns ||
		m.paletteOpen || m.showHelp || m.confirmation != nil
	return m.renderTerminal(content, fitHeight)
}

func (m Model) renderTerminal(content string, fitHeight bool) tea.View {
	if m.terminal.ASCII {
		content = asciiGlyphs.Replace(content)
	}
	if m.terminal.NoColor {
		content = ansi.Strip(content)
	}
	content = m.fitTerminalWidth(content)
	if fitHeight {
		content = m.fitTerminalHeight(content)
	}
	return tea.NewView(content)
}

func (m Model) updateMinimumTerminal(message tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := message.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	if m.paletteOpen {
		switch key.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "enter":
			// Search remains usable, but commands stay dormant until the
			// terminal is large enough to show their result safely.
			return m, nil
		default:
			return m.updatePalette(message)
		}
	}
	switch key.String() {
	case "/", "ctrl+k":
		return m.openPalette()
	case "?":
		m.showHelp = !m.showHelp
		return m, nil
	case "esc":
		m.showHelp = false
		return m, nil
	case "q", "ctrl+c":
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m Model) fitTerminalWidth(content string) string {
	if m.width <= 0 {
		return content
	}
	tail := "…"
	if m.terminal.ASCII {
		tail = "..."
	}
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		if ansi.StringWidth(line) > m.width {
			lines[index] = ansi.Truncate(line, m.width, tail)
		}
	}
	return strings.Join(lines, "\n")
}

func (m Model) fitTerminalHeight(content string) string {
	if m.height <= 0 {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) <= m.height {
		return content
	}
	if m.height == 1 {
		return lines[0]
	}
	if m.height == 2 {
		return strings.Join([]string{lines[0], lines[len(lines)-1]}, "\n")
	}
	tailRows := 3
	if m.confirmation != nil {
		// Keep the complete confirmation and compact footer actionable when the
		// underlying page is taller than the terminal.
		tailRows = 11
	}
	if m.height < 8 {
		tailRows = 1
	}
	if tailRows > m.height-2 {
		tailRows = m.height - 2
	}
	headRows := m.height - tailRows - 1
	marker := mutedStyle.Render("… 其余内容可用 Tab 或方向键聚焦后查看")
	if m.terminal.ASCII {
		marker = "... 其余内容可用 Tab 或方向键聚焦后查看"
	}
	fitted := append([]string(nil), lines[:headRows]...)
	fitted = append(fitted, marker)
	fitted = append(fitted, lines[len(lines)-tailRows:]...)
	return strings.Join(fitted, "\n")
}

func (m *Model) resizeInteractiveControls(width int) {
	inputWidth := width - 30
	if inputWidth < 24 {
		inputWidth = 24
	}
	if inputWidth > 72 {
		inputWidth = 72
	}
	forms := []*structuredForm{}
	if m.sourceForm != nil {
		forms = append(forms, m.sourceForm.structuredForm)
	}
	if m.networkForm != nil {
		forms = append(forms, m.networkForm.structuredForm)
	}
	if m.connectionFilter != nil {
		forms = append(forms, m.connectionFilter.structuredForm)
	}
	if m.ruleFilter != nil {
		forms = append(forms, m.ruleFilter.structuredForm)
	}
	for _, form := range forms {
		if form != nil {
			form.input.SetWidth(inputWidth)
		}
	}
	if m.logFilter != nil {
		m.logFilter.setWidth(inputWidth)
	}
	paletteWidth := width - 16
	if paletteWidth < 24 {
		paletteWidth = 24
	}
	if paletteWidth > 64 {
		paletteWidth = 64
	}
	m.palette.SetWidth(paletteWidth)
}

func (form *logFilterForm) setWidth(width int) {
	if form == nil {
		return
	}
	form.text.SetWidth(width)
	form.level.SetWidth(width)
	form.component.SetWidth(width)
}
