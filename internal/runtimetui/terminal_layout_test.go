package runtimetui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"

	"submux/internal/runtimeapi"
)

func TestTerminalLayoutThresholds(t *testing.T) {
	tests := []struct {
		width  int
		height int
		want   terminalLayout
	}{
		{120, 30, terminalLayoutColumns},
		{119, 30, terminalLayoutStack},
		{120, 29, terminalLayoutStack},
		{80, 24, terminalLayoutStack},
		{79, 24, terminalLayoutFocus},
		{60, 18, terminalLayoutFocus},
		{59, 24, terminalLayoutMinimum},
		{80, 17, terminalLayoutMinimum},
		{0, 0, terminalLayoutUnknown},
	}
	for _, test := range tests {
		if got := resolveTerminalLayout(test.width, test.height); got != test.want {
			t.Errorf("layout %dx%d = %s, want %s", test.width, test.height, got, test.want)
		}
	}
}

func TestTerminalLayoutsKeepFocusedRegionVisible(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})

	model = resizeModel(t, model, 120, 30)
	wide := ansi.Strip(model.View().Content)
	if !lineContainsBoth(wide, "运行状态", "当前来源") {
		t.Fatalf("120x30 status page is not a two-column layout:\n%s", wide)
	}
	for _, expected := range []string{"运行状态", "当前来源", "最近运行操作", "主要代理组"} {
		if !strings.Contains(wide, expected) {
			t.Fatalf("120x30 status page lost region %q:\n%s", expected, wide)
		}
	}
	assertTerminalBounds(t, model.View().Content, 120, 30)

	model = resizeModel(t, model, 100, 24)
	stacked := ansi.Strip(model.View().Content)
	if !strings.Contains(stacked, "运行状态") || !strings.Contains(stacked, "当前来源") || strings.Contains(stacked, "办公室出口") {
		t.Fatalf("100x24 stack did not collapse non-focused regions:\n%s", stacked)
	}
	updated, _ := model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	model = updated.(Model)
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "办公室出口") {
		t.Fatalf("stacked layout did not reveal the newly focused region:\n%s", view)
	}

	model = resizeModel(t, model, 70, 24)
	focused := ansi.Strip(model.View().Content)
	if !strings.Contains(focused, "当前来源") || !strings.Contains(focused, "办公室出口") || strings.Contains(focused, "运行状态") {
		t.Fatalf("70x24 layout did not isolate the current focus region:\n%s", focused)
	}
}

func TestWideLayoutKeepsEveryPageRegionVisible(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model = resizeModel(t, model, 120, 30)
	for _, definition := range pageDefinitions {
		model.page = definition.id
		view := ansi.Strip(model.View().Content)
		for _, region := range definition.regions {
			if !strings.Contains(view, region) {
				t.Fatalf("120x30 page %s lost region %q:\n%s", definition.id, region, view)
			}
		}
		if strings.Contains(view, "当前区域还有更多内容") || strings.Contains(view, "其余内容可用") {
			t.Fatalf("120x30 page %s unexpectedly truncated a region:\n%s", definition.id, view)
		}
		assertTerminalBounds(t, model.View().Content, 120, 30)
	}
}

func TestRecommendedMinimumRendersEveryPageWithin80x24(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model = resizeModel(t, model, 80, 24)
	for _, page := range []rune{'1', '2', '3', '4', '5'} {
		updated, _ := model.Update(keyPress(page))
		model = updated.(Model)
		content := model.View().Content
		assertTerminalBounds(t, content, 80, 24)
		if plain := ansi.Strip(content); !strings.Contains(plain, "焦点") || !strings.Contains(plain, "["+string(page)+" ") {
			t.Fatalf("80x24 page %c lost navigation or focus:\n%s", page, plain)
		}
	}
}

func TestMinimumTerminalKeepsSummarySearchHelpAndQuit(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model = resizeModel(t, model, 59, 17)
	view := ansi.Strip(model.View().Content)
	for _, expected := range []string{"Submux Runtime", "Mihomo", "59x17", "80x24", "/ 搜索", "? 帮助", "q 退出"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("minimum view missing %q:\n%s", expected, view)
		}
	}
	assertTerminalBounds(t, model.View().Content, 59, 17)

	updated, _ := model.Update(keyPress('/'))
	model = updated.(Model)
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "命令搜索") {
		t.Fatalf("minimum terminal cannot open command search:\n%s", view)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	updated, _ = model.Update(keyPress('?'))
	model = updated.(Model)
	if view := ansi.Strip(model.View().Content); !strings.Contains(view, "键盘帮助") {
		t.Fatalf("minimum terminal cannot open help:\n%s", view)
	}
}

func TestTinyTerminalNeverExceedsHeight(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	for height := 1; height <= 4; height++ {
		model = resizeModel(t, model, 40, height)
		assertTerminalBounds(t, model.View().Content, 40, height)
	}
}

func TestMinimumTerminalOnlyAcceptsSearchHelpAndQuit(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model.page = pageConfig
	model.focusIndex = 2
	model.sourceForm = newRemoteSourceForm()
	model.confirmation = &actionConfirmation{Title: "待确认操作"}
	model = resizeModel(t, model, 59, 17)

	for _, key := range []tea.KeyPressMsg{keyPress('3'), {Code: tea.KeyTab}, {Code: tea.KeyEnter}} {
		updated, command := model.Update(key)
		model = updated.(Model)
		if command != nil || model.page != pageConfig || model.focusIndex != 2 || model.confirmation == nil || model.sourceForm == nil {
			t.Fatalf("minimum terminal accepted hidden operation key %q", key.String())
		}
	}

	updated, command := model.Update(keyPress('/'))
	model = updated.(Model)
	if command == nil || !model.paletteOpen {
		t.Fatal("minimum terminal did not open command search")
	}
	updated, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command != nil || !model.paletteOpen || model.page != pageConfig {
		t.Fatal("minimum terminal executed a searched command while its result was hidden")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	updated, command = model.Update(keyPress('q'))
	if command == nil {
		t.Fatal("minimum terminal did not keep quit available")
	}
}

func TestTerminalCapabilitiesSupportNoColorANSI16AndASCII(t *testing.T) {
	noColor := terminalCapabilitiesFromEnvironment([]string{"TERM=xterm-256color", "NO_COLOR=anything"})
	if !noColor.NoColor || noColor.Profile != colorprofile.ASCII || noColor.ASCII {
		t.Fatalf("NO_COLOR capabilities=%#v", noColor)
	}
	ansi16 := terminalCapabilitiesFromEnvironment([]string{"TERM=xterm"})
	if ansi16.NoColor || ansi16.ASCII || ansi16.Profile != colorprofile.ANSI {
		t.Fatalf("ANSI16 capabilities=%#v", ansi16)
	}
	ascii := terminalCapabilitiesFromEnvironment([]string{"TERM=dumb"})
	if !ascii.NoColor || !ascii.ASCII || ascii.Profile != colorprofile.ASCII {
		t.Fatalf("dumb terminal capabilities=%#v", ascii)
	}

	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model.terminal = ascii
	model = resizeModel(t, model, 80, 24)
	view := model.View().Content
	if strings.Contains(view, "\x1b[") {
		t.Fatalf("ASCII/no-color view contains ANSI escapes: %q", view)
	}
	for _, unsupported := range []string{"●", "○", "✓", "▶", "│", "↑", "↓", "←", "→", "▁", "█", "•"} {
		if strings.Contains(view, unsupported) {
			t.Fatalf("ASCII view contains unsupported glyph %q:\n%s", unsupported, view)
		}
	}
}

func TestResizePreservesNavigationSelectionsFiltersAndDrafts(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model.page = pageConfig
	model.focusIndex = 2
	model.selectedSourceID = model.snapshot.Sources.CurrentSourceID
	model.sourceForm = newRemoteSourceForm()
	model.sourceForm.index = len(model.sourceForm.fields) - 1
	model.sourceForm.loadActiveField()
	model.sourceForm.input.SetValue("draft-value")
	model.networkForm = newNetworkForm(runtimeapi.RunModeGateway)
	model.connectionFilter = newConnectionFilterForm(runtimeapi.ConnectionQuery{Target: "example.com"})
	model.ruleFilter = newRuleFilterForm(runtimeapi.RuleQuery{Content: "DOMAIN"})
	model.logFilter = newLogFilterForm(runtimeapi.LogQuery{Text: "dial", Level: runtimeapi.LogLevelError})
	model.proxyGroupOpen = true
	model.proxyGroupSelected = 2
	model.proxyNodeSelected = 7
	model.connectionSelected = 5
	model.ruleSelected = 9
	model.logSelected = 11
	model.editor.SetValue("unsubmitted yaml")
	model.palette.SetValue("维护")

	model = resizeModel(t, model, 70, 20)
	model = resizeModel(t, model, 130, 35)
	if model.page != pageConfig || model.focusIndex != 2 || model.selectedSourceID != model.snapshot.Sources.CurrentSourceID ||
		model.sourceForm == nil || model.sourceForm.index != len(model.sourceForm.fields)-1 || model.sourceForm.input.Value() != "draft-value" ||
		model.networkForm == nil || model.networkForm.mode != runtimeapi.RunModeGateway ||
		model.connectionFilter == nil || model.connectionFilter.value(connectionFieldTarget) != "example.com" ||
		model.ruleFilter == nil || model.ruleFilter.value(ruleFieldContent) != "DOMAIN" ||
		model.logFilter == nil || model.logFilter.text.Value() != "dial" || model.logFilter.level.Value() != runtimeapi.LogLevelError ||
		!model.proxyGroupOpen || model.proxyGroupSelected != 2 || model.proxyNodeSelected != 7 ||
		model.connectionSelected != 5 || model.ruleSelected != 9 || model.logSelected != 11 ||
		model.editor.Value() != "unsubmitted yaml" || model.palette.Value() != "维护" {
		t.Fatalf("resize changed interaction state: %#v", model)
	}
}

func TestRecommendedMinimumKeepsActiveLongFormFieldVisible(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model.sourceForm = newRemoteSourceForm()
	model.sourceForm.index = len(model.sourceForm.fields) - 1
	model.sourceForm.loadActiveField()
	model = resizeModel(t, model, 80, 24)
	view := ansi.Strip(model.View().Content)
	if !strings.Contains(view, "跳过 TLS 校验") || !strings.Contains(view, "上方还有") {
		t.Fatalf("80x24 source form lost the active field or scroll context:\n%s", view)
	}
	assertTerminalBounds(t, model.View().Content, 80, 24)
}

func TestRecommendedMinimumKeepsCompleteConfirmationActionable(t *testing.T) {
	model, _ := initializeShellModel(t, &fakeClient{snapshot: shellSnapshot(7, 4)})
	model.page = pageMaintenance
	model.confirmation = &actionConfirmation{
		Title:         "恢复备份",
		Target:        "Runtime 本机状态",
		Impact:        "覆盖便携状态",
		Interruption:  "可能重启 Mihomo",
		ConfigVersion: "revision-7",
		Recovery:      "使用自动备份恢复",
	}
	model = resizeModel(t, model, 80, 24)
	view := ansi.Strip(model.View().Content)
	for _, expected := range []string{"确认运行操作", "操作目标", "影响", "可能中断", "配置版本", "恢复方式", "Enter 确认", "Esc 取消"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("80x24 confirmation lost %q:\n%s", expected, view)
		}
	}
	assertTerminalBounds(t, model.View().Content, 80, 24)
}

func resizeModel(t *testing.T, model Model, width, height int) Model {
	t.Helper()
	updated, _ := model.Update(tea.WindowSizeMsg{Width: width, Height: height})
	return updated.(Model)
}

func lineContainsBoth(content, first, second string) bool {
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, first) && strings.Contains(line, second) {
			return true
		}
	}
	return false
}

func assertTerminalBounds(t *testing.T, content string, width, height int) {
	t.Helper()
	lines := strings.Split(content, "\n")
	if len(lines) > height {
		t.Fatalf("rendered %d lines, terminal height %d:\n%s", len(lines), height, ansi.Strip(content))
	}
	for index, line := range lines {
		if got := ansi.StringWidth(line); got > width {
			t.Fatalf("line %d width=%d, terminal width=%d: %q", index+1, got, width, ansi.Strip(line))
		}
	}
}
