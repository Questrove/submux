package runtimetui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestRuntimeTUIShowsPagedConnectionDetailsThroughIPC(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 10, 0, time.UTC)
	client := &fakeClient{
		snapshot: shellSnapshot(4, 3),
		connectionPages: []runtimeapi.ConnectionPage{{
			Items: []runtimeapi.Connection{{
				ID: "connection-1", Source: "127.0.0.1:52000", Target: "api.example.com:443",
				Protocol: "tcp", Inbound: "Mixed", Process: "browser", Rule: "DomainSuffix",
				RulePayload: "example.com", OutboundChain: []string{"Proxy", "Tokyo"},
				StartedAt: now.Add(-10 * time.Second), DurationSeconds: 10, Upload: 1024, Download: 2048,
			}},
			Total: 1, Page: 1, PageSize: runtimeapi.ConnectionPageDefaultSize, Available: true, ObservedAt: now,
		}},
	}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)

	updated, tick := model.Update(model.connectionsCmd()())
	model = updated.(Model)
	if tick == nil || len(client.connectionRequests) != 1 {
		t.Fatalf("connection refresh tick=%v requests=%#v", tick != nil, client.connectionRequests)
	}
	request := client.connectionRequests[0]
	if request.Page != 1 || request.PageSize != runtimeapi.ConnectionPageDefaultSize {
		t.Fatalf("default connection query=%#v", request)
	}
	view := model.View().Content
	for _, expected := range []string{
		"活动连接", "127.0.0.1:52000", "api.example.com:443", "browser", "connection-1",
		"DomainSuffix(example.com)", "Proxy → Tokyo", "TUI 不直接连接 Mihomo",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("connection view missing %q: %q", expected, view)
		}
	}
}

func TestRuntimeTUIConnectionFilterAndPaginationAreBounded(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	model.connectionPage = runtimeapi.ConnectionPage{Total: 25, Page: 1, PageSize: 20, Available: true}
	model.connectionHasData = true
	model.connectionLoadedQuery = model.connectionQuery
	model.focusIndex = 2
	initialGeneration := model.connectionsGeneration

	updated, pageCommand := model.Update(keyPress('.'))
	model = updated.(Model)
	if pageCommand == nil || model.connectionQuery.Page != 2 || model.connectionsGeneration != initialGeneration+1 || model.connectionFailures != 0 {
		t.Fatalf("next page command=%v query=%#v", pageCommand != nil, model.connectionQuery)
	}
	updated, staleCommand := model.Update(connectionTickMsg{generation: initialGeneration})
	model = updated.(Model)
	if staleCommand != nil || !model.connectionsPolling {
		t.Fatalf("old page tick restarted polling: command=%v polling=%t", staleCommand != nil, model.connectionsPolling)
	}
	if updated, command := model.Update(keyPress('.')); command != nil || updated.(Model).connectionQuery.Page != 2 {
		t.Fatalf("last page advanced: command=%v query=%#v", command != nil, updated.(Model).connectionQuery)
	}

	updated, command := model.Update(keyPress('f'))
	model = updated.(Model)
	if command == nil || model.connectionFilter == nil {
		t.Fatal("filter shortcut did not open the connection filter form")
	}
	model.connectionFilter.setValue(connectionFieldTarget, "api.example")
	model.connectionFilter.setValue(connectionFieldProcess, "browser")
	model.connectionFilter.setValue(connectionFieldRule, "Domain")
	model.connectionFilter.setValue(connectionFieldNode, "Tokyo")
	pageGeneration := model.connectionsGeneration
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil || model.connectionFilter != nil || model.connectionQuery.Page != 1 ||
		model.connectionQuery.Target != "api.example" || model.connectionQuery.Process != "browser" ||
		model.connectionQuery.Rule != "Domain" || model.connectionQuery.Node != "Tokyo" ||
		model.connectionsGeneration != pageGeneration+1 || model.connectionFailures != 0 {
		t.Fatalf("applied connection filter command=%v query=%#v form=%#v", command != nil, model.connectionQuery, model.connectionFilter)
	}
	_ = command()
	if len(client.connectionRequests) == 0 || client.connectionRequests[len(client.connectionRequests)-1] != model.connectionQuery {
		t.Fatalf("connection filter request=%#v want=%#v", client.connectionRequests, model.connectionQuery)
	}
}

func TestRuntimeTUIConnectionFailureKeepsLastDataAndStopsWhenHidden(t *testing.T) {
	client := &fakeClient{
		snapshot: shellSnapshot(4, 3),
		connectionPages: []runtimeapi.ConnectionPage{{
			Items: []runtimeapi.Connection{{ID: "connection-1", Target: "old.example:443"}},
			Total: 1, Page: 1, PageSize: 20, Available: true,
		}},
	}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	updated, _ := model.Update(model.connectionsCmd()())
	model = updated.(Model)

	client.connectionError = errors.New("local IPC unavailable")
	updated, retry := model.Update(model.connectionsCmd()())
	model = updated.(Model)
	if retry == nil || !model.connectionStale || len(model.connectionPage.Items) != 1 {
		t.Fatalf("stale connection state retry=%v stale=%t page=%#v", retry != nil, model.connectionStale, model.connectionPage)
	}
	if model.connectionFailures != 1 || connectionRetryDelay(1) != time.Second ||
		connectionRetryDelay(2) != 2*time.Second || connectionRetryDelay(20) != 5*time.Second {
		t.Fatalf("connection retry failures=%d delays=%s/%s/%s", model.connectionFailures, connectionRetryDelay(1), connectionRetryDelay(2), connectionRetryDelay(20))
	}
	view := model.View().Content
	if !strings.Contains(view, "old.example:443") || !strings.Contains(view, "活动连接数据已过期") {
		t.Fatalf("stale connection view=%q", view)
	}

	generation := model.connectionsGeneration
	model.openPage(pageStatus)
	updated, command := model.Update(connectionTickMsg{generation: generation})
	model = updated.(Model)
	if command != nil || model.connectionsPolling {
		t.Fatalf("hidden connection viewer continued polling: command=%v polling=%t", command != nil, model.connectionsPolling)
	}
}

func TestRuntimeTUIConnectionSelectionUsesMonitorRegion(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	model.focusIndex = 2
	model.connectionPage = runtimeapi.ConnectionPage{
		Items: []runtimeapi.Connection{{ID: "1", Target: "one.example"}, {ID: "2", Target: "two.example"}},
		Total: 2, Page: 1, PageSize: 20, Available: true,
	}

	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(Model)
	if command != nil || model.connectionSelected != 1 || !strings.Contains(model.View().Content, "详情 ID 2") {
		t.Fatalf("connection selection=%d command=%v view=%q", model.connectionSelected, command != nil, model.View().Content)
	}
}

func TestRuntimeTUICloseSelectedConnectionUsesUnifiedOperationConfirmation(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	model.focusIndex = 2
	model.connectionPage = runtimeapi.ConnectionPage{
		Items: []runtimeapi.Connection{{ID: "connection-1", Target: "api.example.com:443"}},
		Total: 1, Page: 1, PageSize: 20, Available: true, ScopeToken: strings.Repeat("a", 64),
	}
	model.connectionHasData = true
	model.connectionLoadedQuery = model.connectionQuery

	updated, command := model.Update(keyPress('D'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil || len(client.actions) != 0 {
		t.Fatalf("close selected skipped confirmation: command=%v confirmation=%#v actions=%#v", command != nil, model.confirmation, client.actions)
	}
	action := model.confirmation.Action
	if action.Kind != runtimeapi.ActionCloseConnection || action.Params.ConnectionID != "connection-1" || action.Params.ConnectionTarget != "api.example.com:443" {
		t.Fatalf("close selected action=%#v", action)
	}
	view := model.View().Content
	for _, expected := range []string{"确认运行操作 · 关闭活动连接", "api.example.com:443", "现有会话立即中断", "连接无法恢复"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("close selected confirmation missing %q: %q", expected, view)
		}
	}
	updated, submit := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if submit == nil || len(client.actions) != 0 {
		t.Fatal("close selected was submitted before confirmation command ran")
	}
	_, _ = model.Update(submit())
	if len(client.actions) != 1 || client.actions[0].Kind != runtimeapi.ActionCloseConnection {
		t.Fatalf("submitted close actions=%#v", client.actions)
	}
}

func TestRuntimeTUICloseCurrentConnectionScopeShowsCountFiltersAndImpact(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	model.focusIndex = 2
	model.connectionLoadedQuery = runtimeapi.ConnectionQuery{Node: "Tokyo", Process: "browser", Page: 1, PageSize: 20}
	model.connectionQuery = model.connectionLoadedQuery
	model.connectionPage = runtimeapi.ConnectionPage{Total: 2, Page: 1, PageSize: 20, Available: true, ScopeToken: strings.Repeat("b", 64)}
	model.connectionHasData = true

	updated, command := model.Update(keyPress('X'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil || len(client.actions) != 0 {
		t.Fatalf("close scope skipped confirmation: command=%v confirmation=%#v actions=%#v", command != nil, model.confirmation, client.actions)
	}
	action := model.confirmation.Action
	if action.Kind != runtimeapi.ActionCloseConnections || !action.Params.Confirm || action.Params.ConnectionCount != 2 ||
		action.Params.ConnectionScope == nil || action.Params.ConnectionScope.Node != "Tokyo" ||
		action.Params.ConnectionScope.Process != "browser" || action.Params.ConnectionScope.Page != 0 || action.Params.ConnectionScope.PageSize != 0 ||
		action.Params.ConnectionScopeToken != strings.Repeat("b", 64) {
		t.Fatalf("close scope action=%#v", action)
	}
	view := model.View().Content
	for _, expected := range []string{
		"关闭当前范围内全部连接", "已确认 2 条", "进程=browser", "节点=Tokyo",
		"关闭当前确认范围内最多 2 条", "现有会话立即中断", "连接无法恢复",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("close scope confirmation missing %q: %q", expected, view)
		}
	}
}

func TestRuntimeTUIRefusesScopeCloseFromStaleConnectionData(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageMonitor)
	model.focusIndex = 2
	model.connectionPage = runtimeapi.ConnectionPage{Total: 2, Page: 1, PageSize: 20, Available: false}
	model.connectionHasData = true
	model.connectionStale = true

	updated, command := model.Update(keyPress('X'))
	model = updated.(Model)
	if command != nil || model.confirmation != nil || len(client.actions) != 0 {
		t.Fatalf("stale scope close command=%v confirmation=%#v actions=%#v", command != nil, model.confirmation, client.actions)
	}
	if !strings.Contains(model.status, "已过期") {
		t.Fatalf("stale scope close status=%q", model.status)
	}
}

func TestRuntimeTUIShowsConnectionCloseResultInGlobalOperationStrip(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)
	model.lastOperation = runtimeapi.Operation{
		ID: "op-close", Action: runtimeapi.Action{Kind: runtimeapi.ActionCloseConnections},
		State: runtimeapi.OperationSucceeded, Stage: "succeeded", Progress: 100,
		CreatedAt: time.Now().Add(-time.Second), UpdatedAt: time.Now(),
		Result: &runtimeapi.OperationResult{MatchedConnections: 3, ClosedConnections: 2, AlreadyClosedConnections: 1},
	}
	view := model.View().Content
	for _, expected := range []string{"op-close", "succeeded", "已关闭 2 条", "已消失 1 条"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("operation strip missing %q: %q", expected, view)
		}
	}
}
