package runtimetui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestRuntimeTUIShowsSelectedOriginAndAppliedTrafficPolicy(t *testing.T) {
	snapshot := shellSnapshot(4, 3)
	snapshot.TrafficPolicy = runtimeapi.TrafficPolicyStatus{
		Selection:   runtimeapi.TrafficPolicyRule,
		FieldOrigin: runtimeapi.TrafficPolicyOriginRuntime,
		Applied:     runtimeapi.TrafficPolicyDirect,
	}
	model, _ := initializeShellModel(t, &fakeClient{snapshot: snapshot})
	model.openPage(pageConfig)
	view := model.View().Content
	for _, expected := range []string{"流量策略", "规则", "本机运行设置", "实际应用 直连", "待应用"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("traffic policy view missing %q: %q", expected, view)
		}
	}
}

func TestRuntimeTUITrafficPolicyUsesCandidatePreviewAndUnifiedConfirmation(t *testing.T) {
	snapshot := shellSnapshot(4, 3)
	snapshot.TrafficPolicy = runtimeapi.TrafficPolicyStatus{
		Selection:   runtimeapi.TrafficPolicyRule,
		FieldOrigin: runtimeapi.TrafficPolicyOriginRuntime,
		Applied:     runtimeapi.TrafficPolicyRule,
	}
	client := &fakeClient{snapshot: snapshot}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageConfig)
	model.focusIndex = 2

	updated, command := model.Update(ctrlKey('y'))
	model = updated.(Model)
	if command != nil || !model.trafficPolicyEditing {
		t.Fatalf("traffic policy editor command=%v editing=%t", command != nil, model.trafficPolicyEditing)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyRight})
	model = updated.(Model)
	if model.trafficPolicyDraft != runtimeapi.TrafficPolicyGlobal {
		t.Fatalf("traffic policy draft=%q", model.trafficPolicyDraft)
	}
	updated, preview := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if preview == nil || !model.busy {
		t.Fatalf("traffic policy preview command=%v busy=%t", preview != nil, model.busy)
	}
	updated, command = model.Update(preview())
	model = updated.(Model)
	if command != nil || model.confirmation == nil || model.trafficPolicyEditing {
		t.Fatalf("traffic policy confirmation=%#v editing=%t", model.confirmation, model.trafficPolicyEditing)
	}
	if len(client.candidateRequests) != 1 || client.candidateRequests[0].SourceID != "source_office" || client.candidateRequests[0].TrafficPolicy != runtimeapi.TrafficPolicyGlobal {
		t.Fatalf("traffic policy preview requests=%#v", client.candidateRequests)
	}
	action := model.confirmation.Action
	if action.Kind != runtimeapi.ActionSetTrafficPolicy || action.Params.TrafficPolicy != runtimeapi.TrafficPolicyGlobal {
		t.Fatalf("traffic policy action=%#v", action)
	}
	for _, expected := range []string{"设置流量策略", "全局", "候选配置已通过校验", "不会立即切换当前配置"} {
		if !strings.Contains(model.View().Content, expected) {
			t.Fatalf("traffic policy confirmation missing %q: %q", expected, model.View().Content)
		}
	}
}

func TestRuntimeTUIEnterOpensTrafficPolicyFromLocalLayer(t *testing.T) {
	snapshot := shellSnapshot(4, 3)
	model, _ := initializeShellModel(t, &fakeClient{snapshot: snapshot})
	model.openPage(pageConfig)
	model.focusIndex = 2

	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command != nil || !model.trafficPolicyEditing {
		t.Fatalf("traffic policy editor command=%v editing=%t", command != nil, model.trafficPolicyEditing)
	}
}
