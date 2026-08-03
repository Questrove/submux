package runtimeapp

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
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
	groups       map[string]runtimeprocess.ControlProxy
	selected     [][2]string
	delayByNode  map[string]int
	delayErrors  map[string]error
	delayCalls   []string
	delayStarted chan string
	blockDelay   bool
	activeDelay  int
	maxActive    int
	err          error
	mu           sync.Mutex
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

func (control *proxyControlStub) ProxyDelay(ctx context.Context, node string) (int, error) {
	control.mu.Lock()
	control.delayCalls = append(control.delayCalls, node)
	control.activeDelay++
	if control.activeDelay > control.maxActive {
		control.maxActive = control.activeDelay
	}
	control.mu.Unlock()
	defer func() {
		control.mu.Lock()
		control.activeDelay--
		control.mu.Unlock()
	}()
	if control.delayStarted != nil {
		control.delayStarted <- node
	}
	if control.blockDelay {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	default:
	}
	if err := control.delayErrors[node]; err != nil {
		return 0, err
	}
	return control.delayByNode[node], nil
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
	if err := store.SetProxyDelayResult(
		"src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "PROXY", "Provider Tokyo", 56, "", "op-delay", executor.now(),
	); err != nil {
		t.Fatal(err)
	}
	groups, err := executor.ProxyGroups(t.Context(), runtimeapi.ProxyGroupQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups.Groups) != 1 || len(groups.Groups[0].Nodes) != 3 || groups.Groups[0].Nodes[2].Name != "Provider Tokyo" ||
		groups.Groups[0].Nodes[2].DelayMillis != 56 {
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

func TestProxyLatencyGroupScopePersistsResultsWithoutChangingSelection(t *testing.T) {
	executor, store, control := newProxyGroupTestExecutor(t, true)
	alive := true
	control.groups = map[string]runtimeprocess.ControlProxy{
		"PROXY": {Name: "PROXY", Type: "Selector", Now: "Tokyo", All: []string{"Tokyo", "Osaka"}},
		"Tokyo": {Name: "Tokyo", Type: "Shadowsocks", Alive: &alive},
		"Osaka": {Name: "Osaka", Type: "Shadowsocks", Alive: &alive},
	}
	control.delayByNode = map[string]int{"Tokyo": 42}
	control.delayErrors = map[string]error{"Osaka": context.DeadlineExceeded}
	if err := store.SetProxySelection("src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "PROXY", "Tokyo", "op-old", executor.now()); err != nil {
		t.Fatal(err)
	}
	var cancellableProgress bool
	result, err := executor.testProxyLatency(t.Context(), runtimeapi.Operation{
		ID: "op-latency",
		Action: runtimeapi.Action{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{
			SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", LatencyScope: runtimeapi.ProxyLatencyScopeGroup, ProxyGroup: "PROXY",
		}},
	}, func(_ string, progress int, cancellable bool) error {
		cancellableProgress = cancellableProgress || (progress > 0 && progress < 100 && cancellable)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.LatencyTested != 2 || result.LatencySucceeded != 1 || result.LatencyFailed != 1 || !cancellableProgress {
		t.Fatalf("result=%#v cancellable=%v", result, cancellableProgress)
	}
	delays, err := store.ProxyDelayResults(result.SourceID)
	if err != nil || len(delays) != 2 || delays[0].DelayMillis != 0 || delays[0].Failure != "测试超时" || delays[1].DelayMillis != 42 {
		t.Fatalf("delays=%#v err=%v", delays, err)
	}
	selections, err := store.ProxySelections(result.SourceID)
	if err != nil || len(selections) != 1 || selections[0].Node != "Tokyo" || len(control.selected) != 0 {
		t.Fatalf("selections=%#v selected=%#v err=%v", selections, control.selected, err)
	}
	if control.maxActive > proxyLatencyConcurrency {
		t.Fatalf("max concurrency=%d", control.maxActive)
	}
	groups, err := executor.ProxyGroups(t.Context(), runtimeapi.ProxyGroupQuery{})
	if err != nil || len(groups.Groups) != 1 || len(groups.Groups[0].Nodes) != 2 ||
		groups.Groups[0].Nodes[0].DelayMillis != 42 || groups.Groups[0].Nodes[0].DelayFailure != "" ||
		groups.Groups[0].Nodes[1].DelayFailure != "测试超时" || groups.Groups[0].Nodes[1].DelayTestedAt == nil {
		t.Fatalf("groups after latency test=%#v err=%v", groups, err)
	}
}

func TestProxyLatencyStopsWhenOperationContextIsCancelled(t *testing.T) {
	executor, _, control := newProxyGroupTestExecutor(t, true)
	alive := true
	control.groups = map[string]runtimeprocess.ControlProxy{
		"PROXY": {Name: "PROXY", Type: "Selector", Now: "Tokyo", All: []string{"Tokyo"}},
		"Tokyo": {Name: "Tokyo", Type: "Shadowsocks", Alive: &alive},
	}
	control.delayStarted = make(chan string, 1)
	control.blockDelay = true
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := executor.testProxyLatency(ctx, runtimeapi.Operation{
			ID: "op-cancelled",
			Action: runtimeapi.Action{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{
				SourceID: "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", LatencyScope: runtimeapi.ProxyLatencyScopeNode,
				ProxyGroup: "PROXY", ProxyNode: "Tokyo",
			}},
		}, func(string, int, bool) error { return nil })
		done <- err
	}()
	select {
	case <-control.delayStarted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("latency probe did not start")
	}
	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("latency probe did not stop after cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestProxyLatencyActionValidationAcceptsOnlyItsScopeParameters(t *testing.T) {
	const sourceID = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	valid := []runtimeapi.Action{
		{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{
			SourceID: sourceID, LatencyScope: runtimeapi.ProxyLatencyScopeNode, ProxyGroup: "PROXY", ProxyNode: "Tokyo",
		}},
		{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{
			SourceID: sourceID, LatencyScope: runtimeapi.ProxyLatencyScopeGroup, ProxyGroup: "PROXY",
		}},
		{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{
			SourceID: sourceID, LatencyScope: runtimeapi.ProxyLatencyScopeSource,
		}},
	}
	for _, action := range valid {
		if err := validateAction(action); err != nil {
			t.Fatalf("valid action %#v: %v", action, err)
		}
	}
	for _, action := range []runtimeapi.Action{
		{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{SourceID: sourceID, LatencyScope: runtimeapi.ProxyLatencyScopeNode, ProxyGroup: "PROXY"}},
		{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{SourceID: sourceID, LatencyScope: runtimeapi.ProxyLatencyScopeGroup, ProxyGroup: "PROXY", ProxyNode: "Tokyo"}},
		{Kind: runtimeapi.ActionTestProxyLatency, Params: runtimeapi.ActionParams{SourceID: sourceID, LatencyScope: runtimeapi.ProxyLatencyScopeSource, ProxyGroup: "PROXY"}},
		{Kind: runtimeapi.ActionSelectProxyNode, Params: runtimeapi.ActionParams{SourceID: sourceID, ProxyGroup: "PROXY", ProxyNode: "Tokyo", LatencyScope: runtimeapi.ProxyLatencyScopeNode}},
	} {
		if err := validateAction(action); err == nil {
			t.Fatalf("invalid action accepted: %#v", action)
		}
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
