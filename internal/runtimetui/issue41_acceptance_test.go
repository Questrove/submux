package runtimetui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestDPlanKeyboardAcceptanceJourney(t *testing.T) {
	snapshot := shellSnapshot(41, 12)
	snapshot.Sources.Count = 2
	snapshot.Sources.Items = append(snapshot.Sources.Items, runtimeapi.SourceSummary{
		ID:                    "source_backup",
		Name:                  "备用出口",
		Type:                  runtimeapi.SourceTypeLocalImport,
		HasValidatedCandidate: true,
	})
	snapshot.Traffic = runtimeapi.TrafficStatus{
		Available:         true,
		UploadSpeed:       1024,
		DownloadSpeed:     4096,
		UploadTotal:       8 * 1024 * 1024,
		DownloadTotal:     32 * 1024 * 1024,
		ActiveConnections: 2,
	}
	baseClient := &fakeClient{
		snapshot: snapshot,
		trafficHistories: []runtimeapi.TrafficHistory{{
			Status: snapshot.Traffic,
			Samples: []runtimeapi.TrafficSample{{
				ObservedAt: time.Now(), UploadSpeed: 1024, DownloadSpeed: 4096,
			}},
		}},
		logPages: []runtimeapi.LogPage{{
			Items: []runtimeapi.LogEntry{{At: time.Now(), Level: runtimeapi.LogLevelInfo, Component: "runtime", Message: "ready"}},
		}},
	}
	client := &proxyFakeClient{fakeClient: baseClient}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(keyPress('2'))
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(Model)
	updated, command := model.Update(keyPress('t'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionSwitchSource {
		t.Fatalf("source switch did not reach unified confirmation: %#v", model.confirmation)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)

	updated, _ = model.Update(keyPress('3'))
	model = updated.(Model)
	updated, command = model.Update(ctrlKey('t'))
	model = updated.(Model)
	if model.networkForm == nil || model.networkForm.mode != runtimeapi.RunModeTUN {
		t.Fatal("Ctrl+T did not open the TUN preview form")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)

	updated, command = model.Update(keyPress('4'))
	model = updated.(Model)
	if command == nil || model.page != pageMonitor {
		t.Fatal("monitor page did not start the bounded traffic history request")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	monitor := model.View().Content
	for _, expected := range []string{"本次运行累计", "速度曲线", "活动连接"} {
		if !strings.Contains(monitor, expected) {
			t.Fatalf("monitor acceptance lost %q:\n%s", expected, monitor)
		}
	}

	updated, command = model.Update(ctrlKey('n'))
	model = updated.(Model)
	if command == nil || !model.proxyGroupOpen {
		t.Fatal("Ctrl+N did not open the proxy group and node viewer")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)

	updated, command = model.Update(keyPress('l'))
	model = updated.(Model)
	if command == nil || !model.logViewerOpen {
		t.Fatal("l did not open the redacted log viewer")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if !strings.Contains(model.View().Content, "ready") {
		t.Fatal("log viewer did not render the Runtime log page")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)

	updated, _ = model.Update(keyPress('5'))
	model = updated.(Model)
	updated, command = model.Update(keyPress('B'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("B did not preview a restorable full backup")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	updated, command = model.Update(keyPress('B'))
	model = updated.(Model)
	if command == nil || !model.editing || model.editorMode != editorModeBackupExport {
		t.Fatal("second B did not open the confirmed backup output editor")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	updated, command = model.Update(keyPress('L'))
	model = updated.(Model)
	if command == nil || !model.editing || model.editorMode != editorModeBackupRestore {
		t.Fatal("L did not open backup restore inspection")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)

	updated, _ = model.Update(keyPress('?'))
	model = updated.(Model)
	help := model.View().Content
	if !strings.Contains(help, "TUI 不会删除自身服务") {
		t.Fatalf("TUI help does not explain the installer-owned uninstall boundary:\n%s", help)
	}
	updated, _ = model.Update(keyPress('?'))
	model = updated.(Model)
	updated, command = model.Update(keyPress('q'))
	if command == nil {
		t.Fatal("q did not leave the TUI before platform uninstall")
	}
}

func TestDPlanDegradedPartitionsRemainNavigable(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(52, 20)}}
	model, _ := initializeShellModel(t, client)
	model.page = pageMonitor
	model.focusIndex = 2
	model.connectionHasData = true
	model.connectionPage = runtimeapi.ConnectionPage{Available: true, Total: 1}
	model.lastOperation = runtimeapi.Operation{ID: "op-running", State: runtimeapi.OperationRunning}

	updated, _ := model.Update(trafficHistoryErrorMsg{err: errors.New("traffic partition unavailable"), rangeAt: model.trafficRange})
	model = updated.(Model)
	updated, _ = model.Update(connectionPageErrorMsg{err: errors.New("connections partition unavailable"), query: model.connectionQuery, generation: model.connectionsGeneration})
	model = updated.(Model)
	if !model.trafficStale || !model.connectionStale || model.snapshot.Revision != 52 {
		t.Fatalf("partition failures corrupted Snapshot state: traffic=%v connections=%v revision=%d", model.trafficStale, model.connectionStale, model.snapshot.Revision)
	}

	updated, reconnect := model.Update(snapshotErrorMsg{err: errors.New("local IPC disconnected"), generation: model.observeGeneration})
	model = updated.(Model)
	if reconnect == nil || !model.snapshotStale || !model.operationUncertain || model.page != pageMonitor || model.focusIndex != 2 {
		t.Fatalf("disconnect degradation stale=%v uncertain=%v page=%s focus=%d", model.snapshotStale, model.operationUncertain, model.page, model.focusIndex)
	}

	restarted := shellSnapshot(53, 21)
	restarted.Mihomo.State = "restarting"
	restarted.Mihomo.CrashAttempts = 1
	updated, _ = model.Update(snapshotMsg{snapshot: restarted, generation: model.observeGeneration})
	model = updated.(Model)
	if model.snapshotStale || model.snapshot.Revision != 53 || model.snapshot.Mihomo.State != "restarting" || model.page != pageMonitor || model.focusIndex != 2 {
		t.Fatalf("authoritative resync lost restart/navigation state: %#v", model.snapshot)
	}

	updated, _ = model.Update(keyPress('5'))
	model = updated.(Model)
	if view := model.View().Content; !strings.Contains(view, "[5 维护]") || !strings.Contains(view, "revision 53") {
		t.Fatalf("partition failures blocked unrelated maintenance page:\n%s", view)
	}
}
