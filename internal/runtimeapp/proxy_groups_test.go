package runtimeapp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimestate"
)

type proxyProcessDelegate struct{ running bool }

func (delegate *proxyProcessDelegate) IsRunning(context.Context) (bool, error) {
	return delegate.running, nil
}
func (delegate *proxyProcessDelegate) Start(context.Context, runtimeprocess.Specification) error {
	delegate.running = true
	return nil
}
func (delegate *proxyProcessDelegate) Stop(context.Context) error {
	delegate.running = false
	return nil
}
func (delegate *proxyProcessDelegate) ReloadOrRestart(context.Context, runtimeprocess.Specification) error {
	return nil
}
func (delegate *proxyProcessDelegate) ExitEvents() <-chan runtimeprocess.ExitEvent { return nil }

type proxyControlStub struct {
	groups   map[string]runtimeprocess.ControlProxy
	selected [][2]string
	err      error
}

func (control *proxyControlStub) ProxyGroups(context.Context) (map[string]runtimeprocess.ControlProxy, error) {
	return control.groups, control.err
}

func (control *proxyControlStub) SelectProxy(_ context.Context, group, node string) error {
	if control.err != nil {
		return control.err
	}
	control.selected = append(control.selected, [2]string{group, node})
	return nil
}

func TestProxyGroupsOverlayLiveSelectionAndPersistValidChange(t *testing.T) {
	executor, store, control := newProxyGroupTestExecutor(t, true)
	alive := true
	testedAt := time.Now().UTC().Add(-time.Minute)
	control.groups = map[string]runtimeprocess.ControlProxy{
		"PROXY": {Name: "PROXY", Type: "Selector", Now: "Osaka", All: []string{"Tokyo", "Osaka"}},
		"Tokyo": {Name: "Tokyo", Type: "Shadowsocks", Alive: &alive, History: []runtimeprocess.ControlProxyHistory{{Time: testedAt, Delay: 42}}},
		"Osaka": {Name: "Osaka", Type: "Shadowsocks", Alive: &alive},
	}
	groups, err := executor.ProxyGroups(t.Context(), runtimeapi.ProxyGroupQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !groups.CurrentSource || !groups.Available || len(groups.Groups) != 1 || groups.Groups[0].Current != "Osaka" ||
		groups.Groups[0].Nodes[0].DelayMillis != 42 || groups.Groups[0].Nodes[0].DelayTestedAt == nil {
		t.Fatalf("groups=%#v", groups)
	}
	result, err := executor.selectProxyNode(t.Context(), runtimeapi.Operation{
		ID: "op-select",
		Action: runtimeapi.Action{Kind: runtimeapi.ActionSelectProxyNode, Params: runtimeapi.ActionParams{
			SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ProxyGroup: "PROXY", ProxyNode: "Tokyo",
		}},
	}, func(string, int, bool) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if result.ProxyNode != "Tokyo" || len(control.selected) != 1 || control.selected[0] != [2]string{"PROXY", "Tokyo"} {
		t.Fatalf("result=%#v selected=%#v", result, control.selected)
	}
	selections, err := store.ProxySelections(result.SourceID)
	if err != nil || len(selections) != 1 || selections[0].Node != "Tokyo" {
		t.Fatalf("selections=%#v err=%v", selections, err)
	}
}

func TestProxySelectionRejectsNodeMissingFromCandidateAndPreservesOldChoice(t *testing.T) {
	executor, store, control := newProxyGroupTestExecutor(t, false)
	if err := store.SetProxySelection("src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "PROXY", "Osaka", "op-old", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	_, err := executor.selectProxyNode(t.Context(), runtimeapi.Operation{
		ID: "op-invalid",
		Action: runtimeapi.Action{Kind: runtimeapi.ActionSelectProxyNode, Params: runtimeapi.ActionParams{
			SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ProxyGroup: "PROXY", ProxyNode: "Missing",
		}},
	}, func(string, int, bool) error { return nil })
	if err == nil {
		t.Fatal("missing candidate node accepted")
	}
	selections, readErr := store.ProxySelections("src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if readErr != nil || len(selections) != 1 || selections[0].Node != "Osaka" || len(control.selected) != 0 {
		t.Fatalf("selections=%#v selected=%#v err=%v", selections, control.selected, readErr)
	}
}

func TestReplayProxySelectionsRestoresSourceChoice(t *testing.T) {
	executor, store, control := newProxyGroupTestExecutor(t, true)
	if err := store.SetProxySelection("src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "PROXY", "Osaka", "op-old", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := executor.replayProxySelections(t.Context(), "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if len(control.selected) != 1 || control.selected[0] != [2]string{"PROXY", "Osaka"} {
		t.Fatalf("selected=%#v", control.selected)
	}
}

func TestProviderBackedNodeIsValidatedAgainstLiveGroupMembership(t *testing.T) {
	executor, store, control := newProxyGroupTestExecutor(t, true)
	alive := true
	control.groups = map[string]runtimeprocess.ControlProxy{
		"PROXY":          {Name: "PROXY", Type: "Selector", Now: "Tokyo", All: []string{"Tokyo", "Provider Tokyo"}},
		"Tokyo":          {Name: "Tokyo", Type: "Shadowsocks", Alive: &alive},
		"Provider Tokyo": {Name: "Provider Tokyo", Type: "Vless", Alive: &alive},
	}
	groups, err := executor.ProxyGroups(t.Context(), runtimeapi.ProxyGroupQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 1 || len(groups.Groups[0].Nodes) != 3 || groups.Groups[0].Nodes[2].Name != "Provider Tokyo" {
		t.Fatalf("groups=%#v", groups)
	}
	result, err := executor.selectProxyNode(t.Context(), runtimeapi.Operation{
		ID: "op-provider",
		Action: runtimeapi.Action{Kind: runtimeapi.ActionSelectProxyNode, Params: runtimeapi.ActionParams{
			SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ProxyGroup: "PROXY", ProxyNode: "Provider Tokyo",
		}},
	}, func(string, int, bool) error { return nil })
	if err != nil || result.ProxyNode != "Provider Tokyo" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	selections, readErr := store.ProxySelections(result.SourceID)
	if readErr != nil || len(selections) != 1 || selections[0].Node != "Provider Tokyo" {
		t.Fatalf("selections=%#v err=%v", selections, readErr)
	}
}

func TestProxySelectionActionValidationRejectsInjectedParameters(t *testing.T) {
	valid := runtimeapi.Action{Kind: runtimeapi.ActionSelectProxyNode, Params: runtimeapi.ActionParams{
		SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ProxyGroup: "PROXY", ProxyNode: "Tokyo",
	}}
	if err := validateAction(valid); err != nil {
		t.Fatalf("valid action: %v", err)
	}
	invalid := valid
	invalid.Params.ProxyNode = "bad\nnode"
	if err := validateAction(invalid); err == nil {
		t.Fatal("control character accepted")
	}
	invalid = valid
	invalid.Params.Confirm = true
	if err := validateAction(invalid); err == nil {
		t.Fatal("unrelated parameter accepted")
	}
}

func newProxyGroupTestExecutor(t *testing.T, running bool) (*MihomoExecutor, *runtimestate.Store, *proxyControlStub) {
	t.Helper()
	store, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	now := time.Now().UTC()
	_, err = store.CreateSource(runtimestate.SourceRecord{
		ID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Type: runtimeapi.SourceTypeLocalImport,
		Name: "source", RedactedTarget: "Runtime-managed local copy", LastRefreshResult: "imported",
	}, []byte("source"), []byte("proxies:\n  - {name: Tokyo, type: ss}\n  - {name: Osaka, type: ss}\nproxy-groups:\n  - name: PROXY\n    type: select\n    proxies: [Tokyo, Osaka]\n    use: [provider-a]\nrules: [MATCH,PROXY]\n"), "op-create", now)
	if err != nil {
		t.Fatal(err)
	}
	control := &proxyControlStub{}
	executor := &MihomoExecutor{
		State: store, Process: &runtimeprocess.Process{Delegate: &proxyProcessDelegate{running: running}},
		ProxyControl: control, Now: func() time.Time { return now },
	}
	return executor, store, control
}
