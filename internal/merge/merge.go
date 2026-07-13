package merge

import (
	"encoding/json"
	"fmt"
	"strings"

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
//   - 按协议连接身份去重(保留首次出现)
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
	// 名称只是展示信息；其余字段均可能改变连接语义。宁可少去重，也不能把
	// 同一端点上不同传输、端口集、凭据或安全参数的节点错误折叠。
	identity := make(map[string]any, len(n)-1)
	for key, value := range n {
		if key != "name" {
			identity[key] = value
		}
	}
	if kind, ok := identity["type"].(string); ok {
		identity["type"] = strings.ToLower(kind)
	}
	b, err := json.Marshal(identity)
	if err != nil {
		return fmt.Sprintf("%#v", identity)
	}
	return string(b)
}

func cloneNode(n parse.Node) parse.Node {
	m := make(parse.Node, len(n))
	for k, v := range n {
		m[k] = v
	}
	return m
}
