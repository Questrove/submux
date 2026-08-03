package runtimetui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestRuntimeTUIOpensFiltersAndSwitchesFinalRuleViews(t *testing.T) {
	applied := runtimeapi.RuleSet{
		View: runtimeapi.RuleViewApplied, ConfigurationSHA256: strings.Repeat("a", 64),
		Items: []runtimeapi.FinalRule{{Order: 7, Type: "DOMAIN", Condition: "api.example", Target: "PROXY", Origin: runtimeapi.RuleOriginSource, Content: "DOMAIN,api.example,PROXY"}},
		Total: 1,
	}
	client := &fakeClient{snapshot: shellSnapshot(4, 3), ruleSets: []runtimeapi.RuleSet{applied, applied}}
	model, _ := initializeShellModel(t, client)
	model.preview = runtimeapi.CandidatePreview{
		Validated: true,
		Rules: runtimeapi.RuleSet{
			View: runtimeapi.RuleViewCandidate, ConfigurationSHA256: strings.Repeat("b", 64),
			Items: []runtimeapi.FinalRule{{Order: 9, Type: "DOMAIN", Condition: "api.example", Target: "PROXY", Origin: runtimeapi.RuleOriginAdvancedOverride, Content: "DOMAIN,api.example,PROXY"}},
			Total: 1,
		},
	}
	model.openPage(pageConfig)
	model.focusIndex = 4

	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command == nil || !model.ruleViewerOpen || !model.busy {
		t.Fatalf("open rule viewer command=%v open=%t busy=%t", command != nil, model.ruleViewerOpen, model.busy)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	for _, expected := range []string{"最终规则只读查看器", "当前运行配置", "#7", "DOMAIN", "api.example", "PROXY", "配置来源"} {
		if !strings.Contains(model.View().Content, expected) {
			t.Fatalf("applied rule viewer missing %q: %q", expected, model.View().Content)
		}
	}

	updated, _ = model.Update(keyPress('/'))
	model = updated.(Model)
	if model.ruleFilter == nil {
		t.Fatal("rule filter did not open")
	}
	model.ruleFilter.setValue(ruleFieldContent, "api.example")
	model.ruleFilter.setValue(ruleFieldType, "domain")
	model.ruleFilter.setValue(ruleFieldTarget, "proxy")
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("applied rule filter did not request Runtime")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.ruleRequests) != 2 || client.ruleRequests[1] != (runtimeapi.RuleQuery{Content: "api.example", Type: "domain", Target: "proxy"}) {
		t.Fatalf("rule requests=%#v", client.ruleRequests)
	}

	updated, command = model.Update(keyPress('v'))
	model = updated.(Model)
	if command != nil || model.ruleSet.View != runtimeapi.RuleViewCandidate {
		t.Fatalf("candidate rule view command=%v set=%#v", command != nil, model.ruleSet)
	}
	for _, expected := range []string{"尚未应用候选配置", "#9", "高级覆盖"} {
		if !strings.Contains(model.View().Content, expected) {
			t.Fatalf("candidate rule viewer missing %q: %q", expected, model.View().Content)
		}
	}

	updated, command = model.Update(keyPress('v'))
	model = updated.(Model)
	if command == nil || model.ruleSet.View != runtimeapi.RuleViewApplied || strings.Contains(model.View().Content, "#9") ||
		!strings.Contains(model.View().Content, "正在通过 Runtime 本机 IPC 读取规则") {
		t.Fatalf("applied loading leaked candidate rules: command=%v set=%#v view=%q", command != nil, model.ruleSet, model.View().Content)
	}
}

func TestRuntimeTUIJumpsFromConnectionToMatchedRule(t *testing.T) {
	applied := runtimeapi.RuleSet{
		View:  runtimeapi.RuleViewApplied,
		Items: []runtimeapi.FinalRule{{Order: 12, Type: "DOMAIN", Condition: "api.example", Target: "PROXY", Origin: runtimeapi.RuleOriginSource, Content: "DOMAIN,api.example,PROXY"}},
		Total: 1,
	}
	client := &fakeClient{snapshot: shellSnapshot(4, 3), ruleSets: []runtimeapi.RuleSet{applied}}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	model.focusIndex = 2
	model.connectionPage = runtimeapi.ConnectionPage{Items: []runtimeapi.Connection{{ID: "connection-1", Rule: "DOMAIN", RulePayload: "api.example", OutboundChain: []string{"PROXY", "Tokyo"}}}, Total: 1, Page: 1, PageSize: 20, Available: true}
	model.connectionHasData = true

	updated, command := model.Update(ctrlKey('r'))
	model = updated.(Model)
	if command == nil || model.page != pageConfig || model.focusIndex != 4 || !model.ruleViewerOpen {
		t.Fatalf("rule jump command=%v page=%q focus=%d open=%t", command != nil, model.page, model.focusIndex, model.ruleViewerOpen)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.ruleRequests) != 1 || client.ruleRequests[0].Content != "api.example" || client.ruleRequests[0].Type != "DOMAIN" || client.ruleRequests[0].Target != "PROXY" ||
		!strings.Contains(model.View().Content, "#12") {
		t.Fatalf("rule jump requests=%#v view=%q", client.ruleRequests, model.View().Content)
	}
}
