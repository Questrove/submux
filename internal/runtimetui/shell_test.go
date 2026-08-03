package runtimetui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type scriptedEventClient struct {
	*fakeClient
	observeCalls int
	observeErr   error
	watchAfter   []uint64
	event        runtimeapi.Event
	watchErr     error
}

func (client *scriptedEventClient) Observe(ctx context.Context) (runtimeapi.Snapshot, error) {
	client.observeCalls++
	if client.observeErr != nil {
		return runtimeapi.Snapshot{}, client.observeErr
	}
	return client.fakeClient.Observe(ctx)
}

func (client *scriptedEventClient) WatchEvents(
	_ context.Context,
	after uint64,
	handle func(runtimeapi.Event) error,
) error {
	client.watchAfter = append(client.watchAfter, after)
	if client.watchErr != nil {
		return client.watchErr
	}
	return handle(client.event)
}

func TestRuntimeTUIShellUsesFivePagesAndPageLocalFocus(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)

	view := model.View().Content
	for _, label := range []string{"[1 状态]", "2 配置", "3 网络", "4 监控", "5 维护", "/ 搜索", "? 帮助"} {
		if !strings.Contains(view, label) {
			t.Fatalf("TUI shell is missing %q: %q", label, view)
		}
	}

	updated, _ := model.Update(keyPress('2'))
	model = updated.(Model)
	beforeTab := model.View().Content
	if !strings.Contains(beforeTab, "[2 配置]") || !strings.Contains(beforeTab, "焦点 1/") {
		t.Fatalf("configuration page did not become active: %q", beforeTab)
	}

	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	model = updated.(Model)
	afterTab := model.View().Content
	if !strings.Contains(afterTab, "[2 配置]") || !strings.Contains(afterTab, "焦点 2/") {
		t.Fatalf("Tab did not move focus inside the configuration page: %q", afterTab)
	}

	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	model = updated.(Model)
	afterBackTab := model.View().Content
	if !strings.Contains(afterBackTab, "[2 配置]") || !strings.Contains(afterBackTab, "焦点 1/") {
		t.Fatalf("Shift+Tab did not move focus backward inside the page: %q", afterBackTab)
	}
}

func TestRuntimeTUICommandSearchOpensAPage(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(keyPress('/'))
	model = updated.(Model)
	for _, character := range []rune("维护") {
		updated, _ = model.Update(keyPress(character))
		model = updated.(Model)
	}
	if view := model.View().Content; !strings.Contains(view, "打开维护页") {
		t.Fatalf("command search did not find the maintenance page: %q", view)
	}

	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if view := model.View().Content; !strings.Contains(view, "[5 维护]") {
		t.Fatalf("command search did not open the maintenance page: %q", view)
	}
}

func TestRuntimeTUIDirectionalSelectionEnterAndEscapeStayPageLocal(t *testing.T) {
	snapshot := shellSnapshot(7, 4)
	snapshot.Sources.Count = 2
	snapshot.Sources.Items = append(snapshot.Sources.Items, runtimeapi.SourceSummary{
		ID:             "source_backup",
		Name:           "备用出口",
		Type:           runtimeapi.SourceTypeLocalImport,
		RedactedTarget: "本机配置副本",
	})
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: snapshot}}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(keyPress('2'))
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
	model = updated.(Model)
	if model.selectedSource() != "source_backup" {
		t.Fatalf("Down selected source %q", model.selectedSource())
	}

	updated, preview := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if preview == nil {
		t.Fatal("Enter did not activate the selected configuration source")
	}
	updated, _ = model.Update(preview())
	model = updated.(Model)
	if view := model.View().Content; !strings.Contains(view, "[2 配置]") || !strings.Contains(view, "候选配置") {
		t.Fatalf("Enter left the active page or lost its result: %q", view)
	}

	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	if view := model.View().Content; !strings.Contains(view, "[1 状态]") {
		t.Fatalf("Esc did not return to the status page: %q", view)
	}
}

func TestRuntimeTUIRegionFailureKeepsSnapshotAndOtherPagesAvailable(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(errMsg{err: errors.New("network preview failed")})
	model = updated.(Model)
	updated, _ = model.Update(keyPress('5'))
	model = updated.(Model)
	view := model.View().Content
	if !strings.Contains(view, "[5 维护]") || !strings.Contains(view, "revision 7") {
		t.Fatalf("one region failure blocked another page or discarded the Snapshot: %q", view)
	}
}

func TestRuntimeTUIReadOnlyRegionsGiveConsistentKeyboardFeedback(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)

	for _, page := range []rune{'3', '4'} {
		updated, _ := model.Update(keyPress(page))
		model = updated.(Model)
		updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyDown})
		model = updated.(Model)
		if !strings.Contains(model.status, "没有可选择条目") {
			t.Fatalf("page %c Down did not explain the read-only region: %q", page, model.status)
		}
		updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
		model = updated.(Model)
		if command != nil || !strings.Contains(model.status, "只读") {
			t.Fatalf("page %c Enter command=%v status=%q", page, command != nil, model.status)
		}
	}
}

func TestRuntimeTUIDiscardsOutOfOrderSnapshotResponses(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(1, 4)}}
	model, _ := initializeShellModel(t, client)

	updated, olderObserve := model.Update(keyPress('r'))
	model = updated.(Model)
	updated, newerObserve := model.Update(runtimeEventMsg{event: runtimeapi.Event{Cursor: 5, Type: "runtime.changed"}})
	model = updated.(Model)
	if olderObserve == nil || newerObserve == nil {
		t.Fatal("concurrent refresh setup did not return both Snapshot reads")
	}

	client.snapshot = shellSnapshot(3, 6)
	updated, _ = model.Update(newerObserve())
	model = updated.(Model)
	client.snapshot = shellSnapshot(2, 5)
	updated, _ = model.Update(olderObserve())
	model = updated.(Model)
	if model.snapshot.Revision != 3 {
		t.Fatalf("older Snapshot overwrote revision %d", model.snapshot.Revision)
	}
}

func TestRuntimeTUIRetriesFailedRefreshWhileEventWatchIsIdle(t *testing.T) {
	client := &scriptedEventClient{fakeClient: &fakeClient{snapshot: shellSnapshot(7, 4)}}
	model, _ := initializeShellModel(t, client)
	client.observeErr = errors.New("temporary Snapshot failure")

	updated, observe := model.Update(keyPress('r'))
	model = updated.(Model)
	updated, retry := model.Update(observe())
	model = updated.(Model)
	if retry == nil {
		t.Fatal("failed refresh did not schedule a reconnect while the event watch was idle")
	}
	if !model.snapshotStale || model.snapshot.Revision != 7 {
		t.Fatalf("failed refresh stale=%v revision=%d", model.snapshotStale, model.snapshot.Revision)
	}
}

func TestRuntimeTUIRefreshesSnapshotAfterRuntimeEvent(t *testing.T) {
	client := &scriptedEventClient{
		fakeClient: &fakeClient{snapshot: shellSnapshot(1, 4)},
		event: runtimeapi.Event{
			Cursor:           5,
			Type:             "operation.succeeded",
			SnapshotRevision: 2,
		},
	}
	model, watch := initializeShellModel(t, client)
	if watch == nil {
		t.Fatal("initial Snapshot did not start Runtime event watching")
	}

	updated, refresh := model.Update(watch())
	model = updated.(Model)
	if refresh == nil {
		t.Fatal("Runtime event did not request an authoritative Snapshot")
	}
	client.snapshot = shellSnapshot(2, 5)
	updated, nextWatch := model.Update(refresh())
	model = updated.(Model)
	if nextWatch == nil {
		t.Fatal("refreshed Snapshot did not resume Runtime event watching")
	}
	if client.observeCalls != 2 || len(client.watchAfter) != 1 || client.watchAfter[0] != 4 {
		t.Fatalf("event refresh calls observe=%d watchAfter=%v", client.observeCalls, client.watchAfter)
	}
	if view := model.View().Content; !strings.Contains(view, "revision 2") || strings.Contains(view, "数据已过期") {
		t.Fatalf("event-refreshed Snapshot is not authoritative: %q", view)
	}
}

func TestRuntimeTUIKeepsStaleDataAndResynchronizesAfterDisconnect(t *testing.T) {
	client := &scriptedEventClient{
		fakeClient: &fakeClient{snapshot: shellSnapshot(8, 9)},
		watchErr:   errors.New("local IPC disconnected"),
	}
	model, watch := initializeShellModel(t, client)
	if watch == nil {
		t.Fatal("initial Snapshot did not start Runtime event watching")
	}

	updated, retry := model.Update(watch())
	model = updated.(Model)
	staleView := model.View().Content
	if !strings.Contains(staleView, "数据已过期") || !strings.Contains(staleView, "办公室出口") {
		t.Fatalf("disconnect did not retain and mark the last Snapshot: %q", staleView)
	}
	if retry == nil {
		t.Fatal("disconnect did not schedule a reconnect")
	}

	client.watchErr = nil
	client.event = runtimeapi.Event{Cursor: 11, Type: "runtime.changed", SnapshotRevision: 9}
	client.snapshot = shellSnapshot(9, 10)
	updated, observe := model.Update(retry())
	model = updated.(Model)
	if observe == nil {
		t.Fatal("reconnect did not request a full authoritative Snapshot")
	}
	updated, resumedWatch := model.Update(observe())
	model = updated.(Model)
	if resumedWatch == nil {
		t.Fatal("full resynchronization did not resume Runtime event watching")
	}
	if view := model.View().Content; strings.Contains(view, "数据已过期") || !strings.Contains(view, "revision 9") {
		t.Fatalf("full resynchronization did not clear stale state: %q", view)
	}
}

func initializeShellModel(t *testing.T, client Client) (Model, tea.Cmd) {
	t.Helper()
	model := New(t.Context(), client)
	updated, command := model.Update(model.Init()())
	return updated.(Model), command
}

func shellSnapshot(revision, cursor uint64) runtimeapi.Snapshot {
	return runtimeapi.Snapshot{
		ProtocolVersion:   runtimeapi.ProtocolVersion,
		Revision:          revision,
		LatestEventCursor: cursor,
		Runtime:           runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
		Mihomo: runtimeapi.MihomoStatus{
			DesiredState: runtimeapi.MihomoDesiredRunning,
			State:        "running",
			Recovery:     runtimeapi.MihomoRecoveryIdle,
		},
		RunMode: runtimeapi.RunModeExplicit,
		Sources: runtimeapi.SourceStatus{
			CurrentSourceID: "source_office",
			Items: []runtimeapi.SourceSummary{{
				ID:      "source_office",
				Name:    "办公室出口",
				Type:    runtimeapi.SourceTypeSubmuxOutput,
				Current: true,
			}},
		},
		Network: runtimeapi.NetworkStatus{Available: true, Mode: runtimeapi.RunModeExplicit, State: runtimeapi.NetworkStateInactive},
	}
}
