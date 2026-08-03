package runtimetui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

type terminalLayout string

const (
	terminalLayoutUnknown terminalLayout = "unknown"
	terminalLayoutColumns terminalLayout = "columns"
	terminalLayoutStack   terminalLayout = "stack"
	terminalLayoutFocus   terminalLayout = "focus"
	terminalLayoutMinimum terminalLayout = "minimum"
)

func resolveTerminalLayout(width, height int) terminalLayout {
	if width <= 0 || height <= 0 {
		return terminalLayoutUnknown
	}
	if width < 60 || height < 18 {
		return terminalLayoutMinimum
	}
	if width < 80 {
		return terminalLayoutFocus
	}
	if width >= 120 && height >= 30 {
		return terminalLayoutColumns
	}
	return terminalLayoutStack
}

func (m Model) layoutPageBody(body string) string {
	layout := resolveTerminalLayout(m.width, m.height)
	if layout == terminalLayoutUnknown || layout == terminalLayoutMinimum || onboardingRequired(m.snapshot) && m.page == pageStatus {
		return body
	}
	regions := m.splitPageRegions(body)
	if len(regions) <= 1 {
		return body
	}
	focus := m.focusIndex
	if focus < 0 || focus >= len(regions) {
		focus = 0
	}
	switch layout {
	case terminalLayoutColumns:
		columnWidth := (m.width - 3) / 2
		rows := make([]string, 0, (len(regions)+1)/2)
		for index := 0; index < len(regions); index += 2 {
			left := lipgloss.NewStyle().Width(columnWidth).Render(regions[index])
			right := ""
			if index+1 < len(regions) {
				right = lipgloss.NewStyle().Width(columnWidth).Render(regions[index+1])
			}
			rows = append(rows, lipgloss.JoinHorizontal(lipgloss.Top, left, "   ", right))
		}
		return strings.Join(rows, "\n\n")
	case terminalLayoutStack:
		lines := make([]string, 0, len(regions)+8)
		activeLimit := m.height - 12 - (len(regions) - 1)
		if activeLimit < 4 {
			activeLimit = 4
		}
		for index, region := range regions {
			if index == focus {
				lines = append(lines, limitRegionLines(region, activeLimit))
				continue
			}
			heading := strings.SplitN(region, "\n", 2)[0]
			lines = append(lines, heading+mutedStyle.Render("  [Tab 查看]"))
		}
		return strings.Join(lines, "\n")
	case terminalLayoutFocus:
		return regions[focus]
	default:
		return body
	}
}

func (m Model) splitPageRegions(body string) []string {
	definition := definitionFor(m.page)
	if len(definition.regions) == 0 {
		return []string{body}
	}
	starts := make([]int, 0, len(definition.regions))
	searchFrom := 0
	for index, title := range definition.regions {
		marker := m.focusHeading(index, title)
		position := strings.Index(body[searchFrom:], marker)
		if position < 0 {
			return []string{body}
		}
		position += searchFrom
		starts = append(starts, position)
		searchFrom = position + len(marker)
	}
	regions := make([]string, 0, len(starts))
	for index, start := range starts {
		end := len(body)
		if index+1 < len(starts) {
			end = starts[index+1]
		}
		regions = append(regions, strings.TrimSpace(body[start:end]))
	}
	return regions
}

func limitRegionLines(region string, maximum int) string {
	lines := strings.Split(region, "\n")
	if maximum <= 0 || len(lines) <= maximum {
		return region
	}
	return strings.Join(append(lines[:maximum-1], mutedStyle.Render("… 当前区域还有更多内容")), "\n")
}

func (m Model) renderMinimumTerminal() string {
	state := valueOr(m.snapshot.Runtime.ServiceState, "正在连接")
	mihomo := fmt.Sprintf("Mihomo %s / %s", valueOr(m.snapshot.Mihomo.State, "未知"), valueOr(m.snapshot.Mihomo.DesiredState, "未设置"))
	lines := []string{
		titleStyle.Render("Submux Runtime"),
		fmt.Sprintf("Runtime %s · revision %d", state, m.snapshot.Revision),
		mihomo,
		fmt.Sprintf("终端尺寸 %dx%d；至少需要 60x18，建议 80x24", m.width, m.height),
		"",
	}
	if m.paletteOpen {
		lines = append(lines, m.renderPalette())
	} else if m.showHelp {
		lines = append(lines, m.renderHelp())
	} else {
		lines = append(lines, mutedStyle.Render("窗口过小，已暂时隐藏页面内容；调整大小后会恢复原页面和输入。"))
	}
	lines = append(lines, "", "/ 搜索 · ? 帮助 · q 退出")
	return strings.Join(lines, "\n")
}

func (m Model) visibleFormRange(total, selected int) (int, int) {
	if total <= 0 || m.height <= 0 || m.height >= 30 {
		return 0, total
	}
	maximum := m.height - 15
	if maximum < 4 {
		maximum = 4
	}
	if maximum >= total {
		return 0, total
	}
	start := selected - maximum/2
	if start < 0 {
		start = 0
	}
	if start+maximum > total {
		start = total - maximum
	}
	return start, start + maximum
}
