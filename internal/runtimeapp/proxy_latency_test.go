package runtimeapp

import (
	"testing"

	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimeprocess"
)

func TestProxyLatencyTargetsHonorNodeGroupAndSourceScopes(t *testing.T) {
	groups := []mihomo.CandidateProxyGroup{
		{Name: "PRIMARY"},
		{Name: "FALLBACK"},
	}
	live := map[string]runtimeprocess.ControlProxy{
		"PRIMARY":  {All: []string{"Tokyo", "Shared"}},
		"FALLBACK": {All: []string{"Shared", "Osaka"}},
	}

	node, err := proxyLatencyTargets(runtimeapi.ActionParams{
		LatencyScope: runtimeapi.ProxyLatencyScopeNode, ProxyGroup: "PRIMARY", ProxyNode: "Tokyo",
	}, groups, live)
	if err != nil || len(node) != 1 || node[0].Node != "Tokyo" || len(node[0].Groups) != 1 || node[0].Groups[0] != "PRIMARY" {
		t.Fatalf("node targets=%#v err=%v", node, err)
	}
	group, err := proxyLatencyTargets(runtimeapi.ActionParams{
		LatencyScope: runtimeapi.ProxyLatencyScopeGroup, ProxyGroup: "PRIMARY",
	}, groups, live)
	if err != nil || len(group) != 2 || group[1].Node != "Shared" {
		t.Fatalf("group targets=%#v err=%v", group, err)
	}
	source, err := proxyLatencyTargets(runtimeapi.ActionParams{
		LatencyScope: runtimeapi.ProxyLatencyScopeSource,
	}, groups, live)
	if err != nil || len(source) != 3 || source[1].Node != "Shared" || len(source[1].Groups) != 2 {
		t.Fatalf("source targets=%#v err=%v", source, err)
	}
	if _, err := proxyLatencyTargets(runtimeapi.ActionParams{
		LatencyScope: runtimeapi.ProxyLatencyScopeNode, ProxyGroup: "PRIMARY", ProxyNode: "Missing",
	}, groups, live); err == nil {
		t.Fatal("missing live node accepted")
	}
}
