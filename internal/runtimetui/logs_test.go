package runtimetui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestMaintenanceLogViewerLoadsLatestAndOlderPages(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	client := &fakeClient{
		snapshot: shellSnapshot(8, 5),
		logPages: []runtimeapi.LogPage{
			{
				Items: []runtimeapi.LogEntry{
					{Cursor: 8, At: now.Add(-time.Second), Component: runtimeapi.LogComponentRuntime, Stream: "service", Level: runtimeapi.LogLevelInfo, Message: "Runtime ready"},
					{Cursor: 9, At: now, Component: runtimeapi.LogComponentMihomo, Stream: "stderr", Level: runtimeapi.LogLevelError, Message: "dial failed"},
				},
				EarliestCursor: 8, LatestCursor: 9, HasOlder: true, ObservedAt: now,
			},
			{
				Items:          []runtimeapi.LogEntry{{Cursor: 7, At: now.Add(-time.Minute), Component: runtimeapi.LogComponentNetwork, Stream: "privileged-ipc", Level: runtimeapi.LogLevelWarn, Message: "network reconnected"}},
				EarliestCursor: 7, LatestCursor: 7, ObservedAt: now,
			},
		},
	}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMaintenance)
	model.focusIndex = 4
	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command == nil {
		t.Fatal("log viewer did not request the latest page")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.logRequests) != 1 || client.logRequests[0].Limit != runtimeapi.LogPageDefaultSize || client.logRequests[0].Before != 0 || client.logRequests[0].After != 0 {
		t.Fatalf("initial log query=%#v", client.logRequests)
	}
	view := model.View().Content
	for _, expected := range []string{"脱敏日志", "Runtime ready", "dial failed", "跟随中", "加载更早"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("log viewer missing %q:\n%s", expected, view)
		}
	}

	updated, command = model.Update(commandKey("m"))
	model = updated.(Model)
	if command == nil {
		t.Fatal("load older did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.logRequests) != 2 || client.logRequests[1].Before != 8 || len(model.logEntries) != 3 || model.logEntries[0].Cursor != 7 {
		t.Fatalf("older query=%#v entries=%#v", client.logRequests, model.logEntries)
	}
}

func TestLogViewerPauseResumeAndBoundedCursorCatchUp(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	client := &fakeClient{snapshot: shellSnapshot(8, 5), logPages: []runtimeapi.LogPage{
		{Items: []runtimeapi.LogEntry{{Cursor: 10, At: now, Component: runtimeapi.LogComponentRuntime, Level: runtimeapi.LogLevelInfo, Message: "before pause"}}, EarliestCursor: 10, LatestCursor: 10, ObservedAt: now},
		{Items: []runtimeapi.LogEntry{{Cursor: 11, At: now.Add(time.Second), Component: runtimeapi.LogComponentRuntime, Level: runtimeapi.LogLevelInfo, Message: "after resume"}}, EarliestCursor: 11, LatestCursor: 11, ObservedAt: now.Add(time.Second)},
	}}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMaintenance)
	model.focusIndex = 4
	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)

	updated, command = model.Update(keyPress(' '))
	model = updated.(Model)
	if command != nil || !model.logPaused {
		t.Fatalf("pause state=%v command=%v", model.logPaused, command != nil)
	}
	updated, command = model.Update(logTickMsg{generation: model.logGeneration})
	model = updated.(Model)
	if command != nil || len(client.logRequests) != 1 {
		t.Fatal("paused viewer continued polling")
	}
	updated, command = model.Update(keyPress(' '))
	model = updated.(Model)
	if command == nil || model.logPaused {
		t.Fatal("resume did not start catch-up")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.logRequests) != 2 || client.logRequests[1].After != 10 || client.logRequests[1].Limit != runtimeapi.LogPageDefaultSize ||
		len(model.logEntries) != 2 || model.logEntries[1].Message != "after resume" {
		t.Fatalf("resume queries=%#v entries=%#v", client.logRequests, model.logEntries)
	}
}

func TestLogViewerFiltersAndKeepsStaleDataOnReadFailure(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	client := &fakeClient{snapshot: shellSnapshot(8, 5), logPages: []runtimeapi.LogPage{{
		Items:          []runtimeapi.LogEntry{{Cursor: 20, At: now, Component: runtimeapi.LogComponentRuntime, Level: runtimeapi.LogLevelInfo, Message: "retained log"}},
		EarliestCursor: 20, LatestCursor: 20, ObservedAt: now,
	}}}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMaintenance)
	model.focusIndex = 4
	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)

	updated, _ = model.Update(commandKey("f"))
	model = updated.(Model)
	if model.logFilter == nil {
		t.Fatal("log filter form did not open")
	}
	model.logFilter.text.SetValue("retry")
	model.logFilter.level.SetValue(runtimeapi.LogLevelWarn)
	model.logFilter.component.SetValue(runtimeapi.LogComponentNetwork)
	client.logPages = []runtimeapi.LogPage{{Items: []runtimeapi.LogEntry{}, ObservedAt: now.Add(time.Second)}}
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("log filter did not start a new query")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	last := client.logRequests[len(client.logRequests)-1]
	if last.Text != "retry" || last.Level != runtimeapi.LogLevelWarn || last.Component != runtimeapi.LogComponentNetwork {
		t.Fatalf("filtered query=%#v", last)
	}

	model.logEntries = []runtimeapi.LogEntry{{Cursor: 20, At: now, Message: "retained log"}}
	model.logLastSuccess = now
	client.logError = errors.New("log store unavailable")
	updated, command = model.Update(logTickMsg{generation: model.logGeneration})
	model = updated.(Model)
	if command == nil {
		t.Fatal("active viewer did not poll")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	view := model.View().Content
	if !model.logStale || len(model.logEntries) != 1 || !strings.Contains(view, "retained log") || !strings.Contains(view, "数据可能过期") || !strings.Contains(view, "上次成功") {
		t.Fatalf("stale log view=%q state=%#v", view, model)
	}
}

func TestOperationDetailJumpsToRelatedLogTimeRange(t *testing.T) {
	created := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	updatedAt := created.Add(2 * time.Minute)
	client := &fakeClient{snapshot: shellSnapshot(8, 5), logPages: []runtimeapi.LogPage{{ObservedAt: updatedAt}}}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMaintenance)
	model.lastOperation = runtimeapi.Operation{ID: "op_logs", CreatedAt: created, UpdatedAt: updatedAt}
	model.operationDetailOpen = true
	updated, command := model.Update(commandKey("l"))
	model = updated.(Model)
	if command == nil || !model.logViewerOpen || model.operationDetailOpen {
		t.Fatal("operation detail did not open related logs")
	}
	updated, _ = model.Update(command())
	_ = updated.(Model)
	if len(client.logRequests) != 1 || !client.logRequests[0].Since.Equal(created.Add(-30*time.Second)) || !client.logRequests[0].Until.Equal(updatedAt.Add(30*time.Second)) {
		t.Fatalf("operation log query=%#v", client.logRequests)
	}
}
