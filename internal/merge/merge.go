package merge

import (
	"fmt"

	"submux/internal/parse"
)

// Config 是 clash 主模型(整棵配置树)。
type Config = map[string]any

// SourceNodes 是一个源的节点集合(带源名,用于加前缀)。
type SourceNodes struct {
	SourceName string
	Nodes      []parse.Node
}

// Merge 把多源节点合并成一个 clash 主模型:
//   - 节点名加来源前缀 "[源名] 原名"
//   - 按 server+port+uuid 去重(保留首次出现)
//   - 组装基础 proxy-group(select,含全部节点 + DIRECT)与基础 rules
func Merge(sources []SourceNodes) Config {
	proxies := []any{}
	names := []string{}
	seen := map[string]bool{}

	for _, sn := range sources {
		for _, node := range sn.Nodes {
			key := dedupKey(node)
			if seen[key] {
				continue
			}
			seen[key] = true

			nn := cloneNode(node)
			prefixed := fmt.Sprintf("[%s] %s", sn.SourceName, nn.Name())
			nn["name"] = prefixed
			proxies = append(proxies, map[string]any(nn))
			names = append(names, prefixed)
		}
	}

	groupProxies := make([]any, 0, len(names)+1)
	for _, n := range names {
		groupProxies = append(groupProxies, n)
	}
	groupProxies = append(groupProxies, "DIRECT")

	return Config{
		"proxies": proxies,
		"proxy-groups": []any{
			map[string]any{
				"name":    "PROXY",
				"type":    "select",
				"proxies": groupProxies,
			},
		},
		"rules": []any{
			"MATCH,PROXY",
		},
	}
}

func dedupKey(n parse.Node) string {
	return fmt.Sprintf("%v|%v|%v", n["server"], n["port"], n["uuid"])
}

func cloneNode(n parse.Node) parse.Node {
	m := make(parse.Node, len(n))
	for k, v := range n {
		m[k] = v
	}
	return m
}
