package runtimetui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type recentOperationClient struct {
	*scriptedEventClient
	operation runtimeapi.Operation
}

type failingExecuteClient struct {
	*scriptedEventClient
	executeCalls int
	err          error
}

type runtimeRejectedError struct {
	code      string
	retryable bool
}

func (err runtimeRejectedError) Error() string               { return err.code }
func (err runtimeRejectedError) RuntimeErrorCode() string    { return err.code }
func (err runtimeRejectedError) RuntimeErrorRetryable() bool { return err.retryable }

func (client *failingExecuteClient) Execute(_ context.Context, _ runtimeapi.CreateOperationRequest) (runtimeapi.Operation, error) {
	client.executeCalls++
	return runtimeapi.Operation{}, client.err
}

func (client *recentOperationClient) GetOperation(_ context.Context, id string) (runtimeapi.Operation, error) {
	client.gotten = id
	return client.operation, nil
}

func TestRuntimeTUIMutationUsesOneConfirmationRegion(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)

	updated, command := model.Update(keyPress('s'))
	model = updated.(Model)
	if command != nil || len(client.actions) != 0 {
		t.Fatalf("start submitted before confirmation: command=%v actions=%#v", command != nil, client.actions)
	}
	view := model.View().Content
	for _, text := range []string{
		"确认运行操作",
		"操作目标",
		"影响",
		"可能中断",
		"配置版本",
		"恢复方式",
		"Enter 确认",
		"Esc 取消",
	} {
		if !strings.Contains(view, text) {
			t.Fatalf("confirmation is missing %q: %q", text, view)
		}
	}

	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	if strings.Contains(model.View().Content, "确认运行操作") || len(client.actions) != 0 {
		t.Fatal("Esc did not cancel the pending operation")
	}

	updated, _ = model.Update(keyPress('s'))
	model = updated.(Model)
	updated, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command == nil || len(client.actions) != 0 {
		t.Fatalf("Enter confirmation command=%v actions=%#v", command != nil, client.actions)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.actions) != 1 || client.actions[0].Kind != runtimeapi.ActionStartProxy {
		t.Fatalf("confirmed actions=%#v", client.actions)
	}
}

func TestRuntimeTUIRejectsWritesPreparedOrConfirmedFromStaleSnapshot(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)
	model.snapshotStale = true

	updated, command := model.Update(keyPress('s'))
	model = updated.(Model)
	if command != nil || model.confirmation != nil || len(client.actions) != 0 || !strings.Contains(model.status, "Snapshot 已过期") {
		t.Fatalf("stale prepare command=%v confirmation=%v actions=%#v status=%q", command != nil, model.confirmation != nil, client.actions, model.status)
	}

	model.snapshotStale = false
	updated, _ = model.Update(keyPress('s'))
	model = updated.(Model)
	if model.confirmation == nil {
		t.Fatal("fresh Snapshot did not open confirmation")
	}
	model.snapshotStale = true
	updated, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command != nil || model.confirmation != nil || len(client.actions) != 0 || !strings.Contains(model.status, "Snapshot 已过期") {
		t.Fatalf("stale confirm command=%v confirmation=%v actions=%#v status=%q", command != nil, model.confirmation != nil, client.actions, model.status)
	}
}

func TestRuntimeTUIOperationStripSurvivesPageSwitchAndShowsProgress(t *testing.T) {
	snapshot := shellSnapshot(12, 8)
	snapshot.Operations.Queued = 2
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: snapshot}}
	model, _ := initializeShellModel(t, client)
	now := time.Now()
	model.lastOperation = runtimeapi.Operation{
		ID:          "op_running",
		Action:      runtimeapi.Action{Kind: runtimeapi.ActionRefreshSource},
		State:       runtimeapi.OperationRunning,
		Stage:       "downloading",
		Progress:    45,
		Cancellable: true,
		CreatedAt:   now.Add(-3 * time.Second),
		UpdatedAt:   now,
	}

	updated, _ := model.Update(keyPress('5'))
	model = updated.(Model)
	view := model.View().Content
	for _, text := range []string{"op_running", "downloading", "45%", "耗时", "队列 2", "可取消"} {
		if !strings.Contains(view, text) {
			t.Fatalf("operation strip is missing %q after page switch: %q", text, view)
		}
	}
}

func TestRuntimeTUIKeepsOneBackgroundWaitPerOperation(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)
	operation := runtimeapi.Operation{
		ID:    "op_running",
		State: runtimeapi.OperationRunning,
		Stage: "working",
	}

	updated, wait := model.Update(operationMsg{operation: operation})
	model = updated.(Model)
	if wait == nil {
		t.Fatal("first running update did not start background waiting")
	}
	updated, duplicate := model.Update(operationMsg{operation: operation})
	model = updated.(Model)
	if duplicate != nil {
		t.Fatal("second update started a duplicate background wait")
	}
	updated, _ = model.Update(wait())
	model = updated.(Model)
	if client.waited != "op_running" || model.waitingOperationID != "" {
		t.Fatalf("completed wait id=%q waiting=%q", client.waited, model.waitingOperationID)
	}
}

func TestRuntimeTUITerminalEventReleasesBackgroundWaitMarker(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)
	model.waitingOperationID = "op_running"

	updated, command := model.Update(operationMsg{operation: runtimeapi.Operation{
		ID:    "op_running",
		State: runtimeapi.OperationSucceeded,
		Stage: "completed",
	}})
	model = updated.(Model)
	if model.waitingOperationID != "" {
		t.Fatalf("terminal event left wait marker=%q", model.waitingOperationID)
	}
	if command == nil {
		t.Fatal("terminal event did not refresh Snapshot")
	}
}

func TestRuntimeTUICancelsOnlyWhenRuntimeDeclaresCancellable(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)
	model.lastOperation = runtimeapi.Operation{
		ID:          "op_running",
		State:       runtimeapi.OperationRunning,
		Stage:       "applying",
		Cancellable: false,
	}

	updated, command := model.Update(keyPress('c'))
	model = updated.(Model)
	if command != nil || client.cancelled != "" || !strings.Contains(model.status, "不可取消") {
		t.Fatalf("non-cancellable operation command=%v cancelled=%q status=%q", command != nil, client.cancelled, model.status)
	}

	model.lastOperation.Cancellable = true
	updated, command = model.Update(keyPress('c'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("Runtime-cancellable operation did not open cancellation confirmation")
	}
	updated, command = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed cancellation did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.cancelled != "op_running" {
		t.Fatalf("cancelled operation=%q", client.cancelled)
	}
}

func TestRuntimeTUIConnectionLossMarksRunningOperationOutcomeUncertain(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)
	model.lastOperation = runtimeapi.Operation{
		ID:    "op_running",
		State: runtimeapi.OperationRunning,
		Stage: "installing",
	}

	updated, _ := model.Update(runtimeWatchErrorMsg{err: errors.New("local IPC disconnected")})
	model = updated.(Model)
	view := model.View().Content
	if !strings.Contains(view, "结果不确定") || !strings.Contains(view, "不会自动重放") {
		t.Fatalf("connection loss did not mark operation uncertainty: %q", view)
	}
	if len(client.actions) != 0 {
		t.Fatalf("connection loss replayed actions=%#v", client.actions)
	}
}

func TestRuntimeTUIDoesNotReplayUncertainOperationSubmission(t *testing.T) {
	client := &failingExecuteClient{
		scriptedEventClient: &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}},
		err:                 errors.New("local IPC disconnected during submit"),
	}
	model, _ := initializeShellModel(t, client)
	updated, _ := model.Update(keyPress('s'))
	model = updated.(Model)
	updated, submit := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	updated, retry := model.Update(submit())
	model = updated.(Model)
	if retry != nil || client.executeCalls != 1 {
		t.Fatalf("uncertain submit retry=%v executeCalls=%d", retry != nil, client.executeCalls)
	}
	if view := model.View().Content; !strings.Contains(view, "结果不确定") || !strings.Contains(view, "不会自动重放") {
		t.Fatalf("uncertain submit view=%q", view)
	}
}

func TestRuntimeTUIKnownProtocolRejectionIsNotMarkedUncertain(t *testing.T) {
	err := runtimeRejectedError{
		code:      runtimeapi.ErrorRevisionConflict,
		retryable: true,
	}
	if operationOutcomeUncertain(err) {
		t.Fatal("known Runtime rejection was classified as an uncertain outcome")
	}
}

func TestRuntimeTUIBlocksOnlyConflictingWrites(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)
	model.lastOperation = runtimeapi.Operation{
		ID:     "op_source",
		Action: runtimeapi.Action{Kind: runtimeapi.ActionRefreshSource},
		State:  runtimeapi.OperationRunning,
		Stage:  "downloading",
	}

	updated, command := model.Update(keyPress('f'))
	model = updated.(Model)
	if command != nil || !strings.Contains(model.status, "冲突") || strings.Contains(model.View().Content, "确认运行操作") {
		t.Fatalf("conflicting source write command=%v status=%q", command != nil, model.status)
	}

	updated, command = model.Update(keyPress('s'))
	model = updated.(Model)
	if command != nil || !strings.Contains(model.View().Content, "确认运行操作") {
		t.Fatalf("non-conflicting proxy write was disabled: command=%v status=%q", command != nil, model.status)
	}
	updated, _ = model.Update(keyPress('4'))
	model = updated.(Model)
	if !strings.Contains(model.View().Content, "[4 监控]") {
		t.Fatal("running operation blocked page browsing")
	}
}

func TestRuntimeTUIBlocksWritesUntilSnapshotOperationIsHydrated(t *testing.T) {
	snapshot := shellSnapshot(7, 4)
	snapshot.Operations.CurrentOperationID = "op_pending"
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: snapshot}}
	model, _ := initializeShellModel(t, client)

	updated, command := model.Update(keyPress('s'))
	model = updated.(Model)
	if command != nil || model.confirmation != nil {
		t.Fatalf("write was accepted before current operation details loaded: command=%v", command != nil)
	}
	if !strings.Contains(model.status, "正在读取当前运行操作 op_pending 的详情") {
		t.Fatalf("pending operation status=%q", model.status)
	}

	updated, _ = model.Update(keyPress('4'))
	model = updated.(Model)
	if !strings.Contains(model.View().Content, "[4 监控]") {
		t.Fatal("pending operation details blocked page browsing")
	}
}

func TestRuntimeTUIRecoversCurrentOperationFromSnapshot(t *testing.T) {
	snapshot := shellSnapshot(7, 4)
	snapshot.Operations.CurrentOperationID = "op_existing"
	client := &scriptedEventClient{
		fakeClient: &fakeClient{snapshot: snapshot},
		watchErr:   errors.New("stop test watch"),
	}
	model := New(t.Context(), client)
	updated, command := model.Update(model.Init()())
	model = updated.(Model)
	if command == nil {
		t.Fatal("Snapshot with a current operation returned no follow-up commands")
	}
	batch, ok := command().(tea.BatchMsg)
	if !ok {
		t.Fatalf("Snapshot follow-up message=%T, want tea.BatchMsg", command())
	}
	for _, child := range batch {
		message := child()
		if operation, ok := message.(operationMsg); ok {
			updated, _ = model.Update(operation)
			model = updated.(Model)
		}
	}
	if client.gotten != "op_existing" || model.lastOperation.ID != "op_existing" {
		t.Fatalf("recovered operation gotten=%q model=%#v", client.gotten, model.lastOperation)
	}
}

func TestRuntimeTUIRecoversRecentResultAfterClientRestart(t *testing.T) {
	snapshot := shellSnapshot(9, 6)
	snapshot.Operations.RecentOperationID = "op_recent"
	now := time.Now()
	client := &recentOperationClient{
		scriptedEventClient: &scriptedEventClient{
			fakeClient: &fakeClient{snapshot: snapshot},
			watchErr:   errors.New("stop test watch"),
		},
		operation: runtimeapi.Operation{
			ID:        "op_recent",
			Action:    runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
			State:     runtimeapi.OperationSucceeded,
			Stage:     "completed",
			Progress:  100,
			CreatedAt: now.Add(-2 * time.Second),
			UpdatedAt: now,
		},
	}
	model := New(t.Context(), client)
	updated, command := model.Update(model.Init()())
	model = updated.(Model)
	batch, ok := command().(tea.BatchMsg)
	if !ok {
		t.Fatalf("recent result follow-up message=%T", command())
	}
	for _, child := range batch {
		if message, ok := child().(operationMsg); ok {
			updated, _ = model.Update(message)
			model = updated.(Model)
		}
	}
	if client.gotten != "op_recent" || !strings.Contains(model.View().Content, "completed") {
		t.Fatalf("recent result gotten=%q view=%q", client.gotten, model.View().Content)
	}
}
