package merge

import (
	"testing"

	"submux/internal/parse"
)

func TestMergePrefixAndGroups(t *testing.T) {
	sources := []SourceNodes{
		{SourceName: "AirA", Nodes: []parse.Node{
			{"name": "HK", "server": "1.1.1.1", "port": 443, "uuid": "a"},
		}},
		{SourceName: "AirB", Nodes: []parse.Node{
			{"name": "JP", "server": "2.2.2.2", "port": 443, "uuid": "b"},
		}},
	}
	cfg := Merge(sources)

	proxies := cfg["proxies"].([]any)
	if len(proxies) != 2 {
		t.Fatalf("want 2 proxies, got %d", len(proxies))
	}
	first := proxies[0].(map[string]any)
	if first["name"] != "[AirA] HK" {
		t.Fatalf("prefix wrong: %v", first["name"])
	}
	// 原节点不应被改名(cloneNode 保护)
	if sources[0].Nodes[0]["name"] != "HK" {
		t.Fatalf("original node mutated: %v", sources[0].Nodes[0]["name"])
	}

	groups := cfg["proxy-groups"].([]any)
	g0 := groups[0].(map[string]any)
	gp := g0["proxies"].([]any)
	if len(gp) != 3 || gp[len(gp)-1] != "DIRECT" {
		t.Fatalf("group proxies wrong: %v", gp)
	}
}

func TestMergeDedup(t *testing.T) {
	sources := []SourceNodes{
		{SourceName: "AirA", Nodes: []parse.Node{
			{"name": "HK", "server": "1.1.1.1", "port": 443, "uuid": "a"},
		}},
		{SourceName: "AirB", Nodes: []parse.Node{
			{"name": "HK-dup", "server": "1.1.1.1", "port": 443, "uuid": "a"},
		}},
	}
	cfg := Merge(sources)
	proxies := cfg["proxies"].([]any)
	if len(proxies) != 1 {
		t.Fatalf("dedup failed, got %d proxies", len(proxies))
	}
}

func TestMergeDoesNotDedupDifferentCredentials(t *testing.T) {
	sources := []SourceNodes{{SourceName: "AirA", Nodes: []parse.Node{
		{"name": "SS-1", "type": "ss", "server": "1.1.1.1", "port": 443, "password": "a"},
		{"name": "SS-2", "type": "ss", "server": "1.1.1.1", "port": 443, "password": "b"},
	}}}
	if got := len(Merge(sources)["proxies"].([]any)); got != 2 {
		t.Fatalf("different credentials must not be deduplicated, got %d", got)
	}
}

func TestMergeDoesNotDedupDifferentHysteria2PortSets(t *testing.T) {
	sources := []SourceNodes{{SourceName: "AirA", Nodes: []parse.Node{
		{"name": "HY-1", "type": "hysteria2", "server": "hy.example.com", "port": 443, "password": "p", "ports": "20000:30000"},
		{"name": "HY-2", "type": "hysteria2", "server": "hy.example.com", "port": 443, "password": "p", "ports": "30001:40000"},
	}}}
	if got := len(Merge(sources)["proxies"].([]any)); got != 2 {
		t.Fatalf("different port sets must not be deduplicated, got %d", got)
	}
}
