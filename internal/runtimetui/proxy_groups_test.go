package runtimetui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type proxyFakeClient struct {
	*fakeClient
	proxyGroupRequests []runtimeapi.ProxyGroupQuery
	proxyGroupLists    []runtimeapi.ProxyGroupList
	proxyGroupError    error
}

func (f *proxyFakeClient) ProxyGroups(_ context.Context, query runtimeapi.ProxyGroupQuery) (runtimeapi.ProxyGroupList, error) {
	index := len(f.proxyGroupRequests)
	f.proxyGroupRequests = append(f.proxyGroupRequests, query)
	if f.proxyGroupError != nil {
		return runtimeapi.ProxyGroupList{}, f.proxyGroupError
	}
	if len(f.proxyGroupLists) == 0 {
		return runtimeapi.ProxyGroupList{SourceID: query.SourceID, CurrentSource: true, Available: true}, nil
	}
	if index >= len(f.proxyGroupLists) {
		index = len(f.proxyGroupLists) - 1
	}
	return f.proxyGroupLists[index], nil
}

func TestStatusPageShowsMainProxyGroupCurrentNodeAndRecentDelay(t *testing.T) {
	testedAt := time.Now().UTC().Add(-time.Minute)
	model := New(context.Background(), &proxyFakeClient{fakeClient: &fakeClient{}})
	model.snapshot = proxyGroupSnapshot()
	model.proxyGroups = runtimeapi.ProxyGroupList{
		SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CurrentSource: true, Available: true,
		Groups: []runtimeapi.ProxyGroupStatus{{
			Name: "PROXY", Main: true, Selectable: true, Current: "Tokyo",
			Nodes: []runtimeapi.ProxyNodeStatus{{Name: "Tokyo", Available: true, DelayMillis: 42, DelayTestedAt: &testedAt}},
		}},
	}
	view := model.View().Content
	for _, expected := range []string{"主要代理组", "PROXY · 当前 Tokyo · 可用", "42 ms"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("view missing %q:\n%s", expected, view)
		}
	}
}

func TestConfigPageListsEveryProxyNodeWithAvailabilityAndDelay(t *testing.T) {
	testedAt := time.Date(2026, 8, 3, 10, 30, 0, 0, time.Local)
	model := New(context.Background(), &proxyFakeClient{fakeClient: &fakeClient{}})
	model.proxyGroups = runtimeapi.ProxyGroupList{
		SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CurrentSource: true, Available: true,
		Groups: []runtimeapi.ProxyGroupStatus{{
			Name: "PROXY", Type: "select", Main: true, Selectable: true, Current: "Tokyo",
			Nodes: []runtimeapi.ProxyNodeStatus{
				{Name: "Tokyo", Type: "VLESS", Available: true, DelayMillis: 42, DelayTestedAt: &testedAt},
				{Name: "Osaka", Type: "VLESS", Available: false, UnavailableReason: "provider 未加载"},
			},
		}},
	}

	view := strings.Join(model.renderConfigProxyGroupSummary(model.proxyGroups.SourceID), "\n")
	for _, expected := range []string{
		"PROXY · 可选择 · 当前 Tokyo · 2 个节点 · 主代理组",
		"Tokyo [当前] · VLESS · 可用 · 42 ms · 10:30:00",
		"Osaka · VLESS · 不可用：provider 未加载",
	} {
		if !strings.Contains(view, expected) {
			t.Fatalf("configuration proxy summary missing %q:\n%s", expected, view)
		}
	}
}

func TestProxyGroupViewerUsesUnifiedConfirmationBeforeSubmittingSelection(t *testing.T) {
	client := &proxyFakeClient{fakeClient: &fakeClient{}}
	model := New(context.Background(), client)
	model.snapshot = proxyGroupSnapshot()
	model.proxyGroups = selectableProxyGroups()
	if command := model.openProxyGroups(model.snapshot.Sources.CurrentSourceID); command != nil {
		t.Fatal("cached proxy groups unexpectedly triggered reload")
	}
	model.proxyNodeSelected = 1
	updated, command := model.updateProxyGroups(tea.KeyPressMsg{Code: tea.KeyEnter})
	if command != nil {
		t.Fatal("selection submitted before confirmation")
	}
	model = updated.(Model)
	if model.proxyGroupOpen || model.confirmation == nil || model.confirmation.Action.Kind != runtimeapi.ActionSelectProxyNode ||
		model.confirmation.Action.Params.ProxyGroup != "PROXY" || model.confirmation.Action.Params.ProxyNode != "Osaka" {
		t.Fatalf("model=%#v", model.confirmation)
	}
	command = model.confirmAction()
	if command == nil {
		t.Fatal("confirmed proxy selection did not create command")
	}
	message := command()
	if _, ok := message.(operationMsg); !ok {
		t.Fatalf("message=%T %#v", message, message)
	}
	if len(client.actions) != 1 || client.actions[0].Kind != runtimeapi.ActionSelectProxyNode || client.actions[0].Params.ProxyNode != "Osaka" {
		t.Fatalf("actions=%#v", client.actions)
	}
}

func TestConfigSourceNavigationReloadsProxyGroupsForSelectedSource(t *testing.T) {
	client := &proxyFakeClient{fakeClient: &fakeClient{}}
	model := New(context.Background(), client)
	model.page = pageConfig
	model.snapshot = proxyGroupSnapshot()
	model.snapshot.Sources.Items = append(model.snapshot.Sources.Items, runtimeapi.SourceSummary{
		ID: "src_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Name: "backup", Type: runtimeapi.SourceTypeLocalImport,
	})
	model.selectedSourceID = model.snapshot.Sources.Items[0].ID
	command := model.moveSelection(true)
	if command == nil {
		t.Fatal("source navigation did not reload proxy groups")
	}
	message := command()
	if _, ok := message.(proxyGroupListMsg); !ok {
		t.Fatalf("message=%T %#v", message, message)
	}
	if len(client.proxyGroupRequests) != 1 || client.proxyGroupRequests[0].SourceID != "src_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("requests=%#v", client.proxyGroupRequests)
	}
}

func proxyGroupSnapshot() runtimeapi.Snapshot {
	const sourceID = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	return runtimeapi.Snapshot{
		Revision: 7,
		Sources: runtimeapi.SourceStatus{
			CurrentSourceID: sourceID,
			Items:           []runtimeapi.SourceSummary{{ID: sourceID, Name: "main", Type: runtimeapi.SourceTypeLocalImport, Current: true}},
		},
	}
}

func selectableProxyGroups() runtimeapi.ProxyGroupList {
	return runtimeapi.ProxyGroupList{
		SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CurrentSource: true, Available: true,
		Groups: []runtimeapi.ProxyGroupStatus{{
			Name: "PROXY", Type: "select", Main: true, Selectable: true, Current: "Tokyo",
			Nodes: []runtimeapi.ProxyNodeStatus{{Name: "Tokyo", Available: true}, {Name: "Osaka", Available: true}},
		}},
	}
}
