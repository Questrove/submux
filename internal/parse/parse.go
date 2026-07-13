package parse

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Node 是一个 clash proxy(保留原始全部字段,保证 reality 等字段不丢)。
type Node map[string]any

// Name 返回节点 name 字段。
func (n Node) Name() string {
	if s, ok := n["name"].(string); ok {
		return s
	}
	return ""
}

// ParseClash 解析上游 clash yaml,返回其 proxies 列表。
// 没有 proxies 字段时返回 nil, nil。
func ParseClash(raw string) ([]Node, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, err
	}
	proxiesRaw, ok := doc["proxies"]
	if !ok || proxiesRaw == nil {
		return nil, nil
	}
	list, ok := proxiesRaw.([]any)
	if !ok {
		return nil, fmt.Errorf("proxies is not a list")
	}
	var nodes []Node
	for i, item := range list {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("proxy %d is not an object", i)
		}
		name, nameOK := m["name"].(string)
		kind, typeOK := m["type"].(string)
		if !nameOK || name == "" || !typeOK || kind == "" {
			return nil, fmt.Errorf("proxy %d is missing name or type", i)
		}
		nodes = append(nodes, Node(m))
	}
	return nodes, nil
}
