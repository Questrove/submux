package mihomo

import (
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

type CandidateProxyNode struct {
	Name              string
	Type              string
	Available         bool
	UnavailableReason string
}

type CandidateProxyGroup struct {
	Name       string
	Type       string
	Main       bool
	Selectable bool
	Current    string
	Nodes      []CandidateProxyNode
	Providers  []string
}

func CandidateProxyGroups(configuration []byte) ([]CandidateProxyGroup, error) {
	root, err := parseConfigurationLayer(configuration, "candidate configuration")
	if err != nil {
		return nil, err
	}
	available := map[string]string{
		"DIRECT": "direct",
		"REJECT": "reject",
	}
	proxies := mappingValue(root, "proxies")
	if proxies != nil {
		if proxies.Kind != yaml.SequenceNode {
			return nil, errors.New("Mihomo proxies must be a sequence")
		}
		for _, proxy := range proxies.Content {
			name, proxyType, parseErr := namedProxyMapping(proxy)
			if parseErr != nil {
				return nil, parseErr
			}
			available[name] = proxyType
		}
	}
	groupsNode := mappingValue(root, "proxy-groups")
	if groupsNode == nil {
		return []CandidateProxyGroup{}, nil
	}
	if groupsNode.Kind != yaml.SequenceNode {
		return nil, errors.New("Mihomo proxy-groups must be a sequence")
	}
	for _, group := range groupsNode.Content {
		name, groupType, parseErr := namedProxyMapping(group)
		if parseErr != nil {
			return nil, parseErr
		}
		available[name] = groupType
	}
	groups := make([]CandidateProxyGroup, 0, len(groupsNode.Content))
	for _, node := range groupsNode.Content {
		name, groupType, parseErr := namedProxyMapping(node)
		if parseErr != nil {
			return nil, parseErr
		}
		group := CandidateProxyGroup{Name: name, Type: groupType, Selectable: strings.EqualFold(groupType, "select")}
		providers := mappingValue(node, "use")
		if providers != nil {
			if providers.Kind != yaml.SequenceNode {
				return nil, errors.New("Mihomo proxy group providers must be a sequence")
			}
			for _, provider := range providers.Content {
				if provider.Kind != yaml.ScalarNode || strings.TrimSpace(provider.Value) == "" {
					return nil, errors.New("Mihomo proxy group provider must be a non-empty scalar")
				}
				group.Providers = append(group.Providers, strings.TrimSpace(provider.Value))
			}
		}
		members := mappingValue(node, "proxies")
		if members != nil {
			if members.Kind != yaml.SequenceNode {
				return nil, errors.New("Mihomo proxy group members must be a sequence")
			}
			for _, member := range members.Content {
				if member.Kind != yaml.ScalarNode || strings.TrimSpace(member.Value) == "" {
					return nil, errors.New("Mihomo proxy group member must be a non-empty scalar")
				}
				memberName := strings.TrimSpace(member.Value)
				memberType, found := available[memberName]
				proxy := CandidateProxyNode{Name: memberName, Type: memberType, Available: found}
				if !found {
					proxy.UnavailableReason = "候选配置中不存在该节点或代理组"
				}
				group.Nodes = append(group.Nodes, proxy)
			}
		}
		if len(group.Nodes) > 0 {
			group.Current = group.Nodes[0].Name
		}
		groups = append(groups, group)
	}
	mainIndex := -1
	for index := range groups {
		if groups[index].Name == "PROXY" {
			mainIndex = index
			break
		}
		if mainIndex < 0 && groups[index].Selectable {
			mainIndex = index
		}
	}
	if mainIndex >= 0 {
		groups[mainIndex].Main = true
	}
	return groups, nil
}

func namedProxyMapping(node *yaml.Node) (string, string, error) {
	if node == nil || node.Kind != yaml.MappingNode {
		return "", "", errors.New("Mihomo proxy or group must be a mapping")
	}
	nameNode := mappingValue(node, "name")
	typeNode := mappingValue(node, "type")
	if nameNode == nil || nameNode.Kind != yaml.ScalarNode || strings.TrimSpace(nameNode.Value) == "" ||
		typeNode == nil || typeNode.Kind != yaml.ScalarNode || strings.TrimSpace(typeNode.Value) == "" {
		return "", "", errors.New("Mihomo proxy or group name and type are required")
	}
	return strings.TrimSpace(nameNode.Value), strings.ToLower(strings.TrimSpace(typeNode.Value)), nil
}
