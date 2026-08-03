package runtimetui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestFirstRunGuideIsDerivedFromSnapshotAndDisappearsWhenReady(t *testing.T) {
	snapshot := firstRunSnapshot()
	model, _ := initializeShellModel(t, &fakeClient{snapshot: snapshot})

	view := model.View().Content
	for _, expected := range []string{
		"首次运行引导", "2/6", "服务与权限", "官方 Mihomo 核心", "Runtime 配置来源",
		"候选校验", "运行方式预览", "应用启动", "作用", "当前值", "为什么", "失败处理",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("first-run guide missing %q:\n%s", expected, view)
		}
	}

	ready := readyFirstRunSnapshot()
	updated, _ := model.Update(snapshotMsg{snapshot: ready, generation: model.observeGeneration})
	model = updated.(Model)
	view = model.View().Content
	if strings.Contains(view, "首次运行引导") || !strings.Contains(view, "运行状态") || !strings.Contains(view, "主要代理组") {
		t.Fatalf("ready Snapshot did not switch to the normal status summary:\n%s", view)
	}

	restarted, _ := initializeShellModel(t, &fakeClient{snapshot: snapshot})
	if view := restarted.View().Content; !strings.Contains(view, "2/6") {
		t.Fatalf("restarted TUI did not resume from actual Snapshot:\n%s", view)
	}
}

func TestFirstRunCoreStepOnlyPreviewsBeforeUnifiedConfirmation(t *testing.T) {
	client := &fakeClient{snapshot: firstRunSnapshot()}
	model, _ := initializeShellModel(t, client)

	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command == nil || len(client.actions) != 0 {
		t.Fatalf("core step command=%v actions=%#v", command != nil, client.actions)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.mihomoUpdate.PlanID == "" || model.page != pageMaintenance || model.confirmation != nil || len(client.actions) != 0 {
		t.Fatalf("core preview plan=%#v page=%s confirmation=%v actions=%#v", model.mihomoUpdate, model.page, model.confirmation != nil, client.actions)
	}

	updated, _ = model.Update(keyPress('U'))
	model = updated.(Model)
	if model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionUpdateMihomo || len(client.actions) != 0 {
		t.Fatalf("core confirmation=%#v actions=%#v", model.confirmation, client.actions)
	}
}

func TestFirstRunGuideRoutesSourceCandidateModeAndStartThroughExistingSafetyFlows(t *testing.T) {
	t.Run("source", func(t *testing.T) {
		snapshot := firstRunSnapshot()
		snapshot.Mihomo = runtimeapi.MihomoStatus{Version: "1.19.29", State: "stopped", DesiredState: runtimeapi.MihomoDesiredStopped}
		client := &fakeClient{snapshot: snapshot}
		model, _ := initializeShellModel(t, client)

		updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		model = updated.(Model)
		if command != nil || model.page != pageConfig || model.confirmation != nil || len(client.actions) != 0 {
			t.Fatalf("source step page=%s command=%v confirmation=%v actions=%#v", model.page, command != nil, model.confirmation != nil, client.actions)
		}
	})

	t.Run("candidate", func(t *testing.T) {
		snapshot := firstRunSnapshot()
		snapshot.Mihomo = runtimeapi.MihomoStatus{Version: "1.19.29", State: "stopped", DesiredState: runtimeapi.MihomoDesiredStopped}
		snapshot.Sources = runtimeapi.SourceStatus{Count: 1, Items: []runtimeapi.SourceSummary{{
			ID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "主配置", Type: runtimeapi.SourceTypeLocalImport, HasValidatedCandidate: true,
		}}}
		client := &fakeClient{snapshot: snapshot}
		model, _ := initializeShellModel(t, client)

		updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		model = updated.(Model)
		if command != nil || model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionSwitchSource || len(client.actions) != 0 {
			t.Fatalf("candidate confirmation=%#v command=%v actions=%#v", model.confirmation, command != nil, client.actions)
		}
	})

	t.Run("run mode", func(t *testing.T) {
		snapshot := firstRunSnapshot()
		snapshot.Mihomo = runtimeapi.MihomoStatus{Version: "1.19.29", State: "stopped", DesiredState: runtimeapi.MihomoDesiredStopped}
		snapshot.Sources = readySourceStatus()
		client := &fakeClient{snapshot: snapshot}
		model, _ := initializeShellModel(t, client)

		updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		model = updated.(Model)
		if command != nil || model.page != pageNetwork || model.networkForm == nil || model.confirmation != nil || len(client.actions) != 0 {
			t.Fatalf("run-mode page=%s form=%v command=%v confirmation=%v actions=%#v", model.page, model.networkForm != nil, command != nil, model.confirmation != nil, client.actions)
		}
	})

	t.Run("start", func(t *testing.T) {
		snapshot := firstRunSnapshot()
		snapshot.Mihomo = runtimeapi.MihomoStatus{Version: "1.19.29", State: "stopped", DesiredState: runtimeapi.MihomoDesiredStopped}
		snapshot.Sources = readySourceStatus()
		snapshot.RunMode = runtimeapi.RunModeExplicit
		client := &fakeClient{snapshot: snapshot}
		model, _ := initializeShellModel(t, client)

		updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		model = updated.(Model)
		if command != nil || model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionStartProxy || len(client.actions) != 0 {
			t.Fatalf("start confirmation=%#v command=%v actions=%#v", model.confirmation, command != nil, client.actions)
		}
	})
}

func TestFirstRunGuideDoesNotAdvanceOnFaultsOrInconsistentSourceSummary(t *testing.T) {
	snapshot := firstRunSnapshot()
	snapshot.Runtime.LocalIPCAuthorized = false
	step, index, pending := currentOnboardingStep(snapshot)
	if !pending || index != 0 || step.id != onboardingService {
		t.Fatalf("unauthorized Snapshot step=%#v index=%d pending=%v", step, index, pending)
	}

	snapshot.Runtime.LocalIPCAuthorized = true
	snapshot.Mihomo = runtimeapi.MihomoStatus{
		Version: "1.19.29", State: "stopped", DesiredState: runtimeapi.MihomoDesiredStopped,
		Fault: &runtimeapi.Fault{Code: "mihomo_core_unavailable", Message: "installed core failed verification"},
	}
	step, index, pending = currentOnboardingStep(snapshot)
	if !pending || index != 1 || step.id != onboardingCore {
		t.Fatalf("faulted core step=%#v index=%d pending=%v", step, index, pending)
	}

	snapshot.Mihomo.Fault = nil
	snapshot.Sources.Count = 1
	step, index, pending = currentOnboardingStep(snapshot)
	if !pending || index != 2 || step.id != onboardingSource {
		t.Fatalf("count-only source step=%#v index=%d pending=%v", step, index, pending)
	}
	snapshot.Sources = runtimeapi.SourceStatus{Items: []runtimeapi.SourceSummary{{
		ID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "摘要缺少计数", Type: runtimeapi.SourceTypeLocalImport,
	}}}
	step, index, pending = currentOnboardingStep(snapshot)
	if !pending || index != 2 || step.id != onboardingSource {
		t.Fatalf("items-only source step=%#v index=%d pending=%v", step, index, pending)
	}

	snapshot.Sources = readySourceStatus()
	snapshot.RunMode = runtimeapi.RunModeExplicit
	snapshot.Mihomo.State = "running"
	snapshot.Mihomo.DesiredState = runtimeapi.MihomoDesiredRunning
	snapshot.Mihomo.Fault = &runtimeapi.Fault{Code: "mihomo_unhealthy", Message: "health check failed"}
	step, index, pending = currentOnboardingStep(snapshot)
	if !pending || index != 1 || step.id != onboardingCore {
		t.Fatalf("running fault step=%#v index=%d pending=%v", step, index, pending)
	}
}

func TestFirstRunCandidateFallsBackFromUnknownCurrentSourceToValidatedSwitch(t *testing.T) {
	snapshot := firstRunSnapshot()
	snapshot.Mihomo = runtimeapi.MihomoStatus{Version: "1.19.29", State: "stopped", DesiredState: runtimeapi.MihomoDesiredStopped}
	snapshot.Sources = readySourceStatus()
	snapshot.Sources.CurrentSourceID = "src_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	client := &fakeClient{snapshot: snapshot}
	model, _ := initializeShellModel(t, client)

	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command != nil || model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionSwitchSource ||
		model.confirmation.Action.Params.SourceID != snapshot.Sources.Items[0].ID {
		t.Fatalf("candidate fallback confirmation=%#v command=%v", model.confirmation, command != nil)
	}
}

func firstRunSnapshot() runtimeapi.Snapshot {
	return runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        4,
		Runtime:         runtimeapi.RuntimeStatus{Version: "2.0.2", ServiceState: "running", LocalIPCAuthorized: true},
		Mihomo:          runtimeapi.MihomoStatus{State: "not_installed", DesiredState: runtimeapi.MihomoDesiredUnset, Recovery: runtimeapi.MihomoRecoveryIdle},
		RunMode:         "unconfigured",
		Network:         runtimeapi.NetworkStatus{Available: true, Mode: runtimeapi.RunModeExplicit, State: runtimeapi.NetworkStateInactive},
	}
}

func readyFirstRunSnapshot() runtimeapi.Snapshot {
	snapshot := firstRunSnapshot()
	snapshot.Mihomo = runtimeapi.MihomoStatus{Version: "1.19.29", State: "running", DesiredState: runtimeapi.MihomoDesiredRunning, Recovery: runtimeapi.MihomoRecoveryIdle}
	snapshot.Sources = readySourceStatus()
	snapshot.RunMode = runtimeapi.RunModeExplicit
	return snapshot
}

func readySourceStatus() runtimeapi.SourceStatus {
	const sourceID = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return runtimeapi.SourceStatus{
		Count: 1, CurrentSourceID: sourceID,
		Items: []runtimeapi.SourceSummary{{
			ID: sourceID, Name: "主配置", Type: runtimeapi.SourceTypeLocalImport, Current: true, HasValidatedCandidate: true,
		}},
	}
}
