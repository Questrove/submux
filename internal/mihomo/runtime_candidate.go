package mihomo

import (
	"errors"
	"fmt"
	"net"
	"runtime"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultExplicitProxyPort = 7890

type ExplicitCandidateBuilder struct {
	Port            int
	ControlEndpoint string
	Platform        string
}

func (b ExplicitCandidateBuilder) BuildCandidate(source []byte) ([]byte, error) {
	port := b.Port
	if port == 0 {
		port = DefaultExplicitProxyPort
	}
	if port < 1 || port > 65535 {
		return nil, errors.New("explicit proxy port must be between 1 and 65535")
	}
	platform := b.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	if b.ControlEndpoint == "" {
		return nil, errors.New("Mihomo local control endpoint is required")
	}
	if platform != "windows" && platform != "linux" && platform != "darwin" {
		return nil, fmt.Errorf("unsupported Mihomo Runtime platform %q", platform)
	}

	var document yaml.Node
	if err := yaml.Unmarshal(source, &document); err != nil {
		return nil, fmt.Errorf("parse imported Mihomo configuration: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("imported Mihomo configuration must be a YAML mapping")
	}
	if err := validateRuntimeYAML(document.Content[0], 0, new(int)); err != nil {
		return nil, err
	}
	root := document.Content[0]
	if err := validateManagedProviders(root); err != nil {
		return nil, err
	}

	for _, key := range []string{
		"mixed-port",
		"port",
		"socks-port",
		"redir-port",
		"tproxy-port",
		"allow-lan",
		"bind-address",
		"authentication",
		"skip-auth-prefixes",
		"lan-allowed-ips",
		"lan-disallowed-ips",
		"listeners",
		"external-controller",
		"external-controller-unix",
		"external-controller-pipe",
		"external-controller-tls",
		"external-controller-cors",
		"external-doh-server",
		"secret",
		"external-ui",
		"external-ui-name",
		"external-ui-url",
		"tls",
		"geox-url",
		"geo-auto-update",
		"geo-update-interval",
		"interface-name",
		"routing-mark",
		"tun",
	} {
		removeMappingKey(root, key)
	}
	if dns := mappingValue(root, "dns"); dns != nil && dns.Kind == yaml.MappingNode {
		removeMappingKey(dns, "listen")
	}
	if err := prependRuntimeHealthRules(root); err != nil {
		return nil, err
	}

	setMappingScalar(root, "mixed-port", "0", "!!int")
	setMappingScalar(root, "port", "0", "!!int")
	setMappingScalar(root, "socks-port", "0", "!!int")
	setMappingScalar(root, "redir-port", "0", "!!int")
	setMappingScalar(root, "tproxy-port", "0", "!!int")
	setMappingScalar(root, "allow-lan", "false", "!!bool")
	setMappingScalar(root, "bind-address", "127.0.0.1", "!!str")
	setMappingScalar(root, "ipv6", "true", "!!bool")
	setMappingScalar(root, "secret", "", "!!str")
	setMappingNode(root, "authentication", &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"})
	setMappingNode(root, "tun", mappingNode(map[string]*yaml.Node{
		"enable": scalarNode("false", "!!bool"),
	}))
	if platform == "windows" {
		setMappingScalar(root, "external-controller-pipe", b.ControlEndpoint, "!!str")
	} else {
		setMappingScalar(root, "external-controller-unix", b.ControlEndpoint, "!!str")
	}
	setMappingNode(root, "listeners", explicitListeners(port))

	candidate, err := yaml.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("encode Runtime-owned Mihomo configuration: %w", err)
	}
	return candidate, nil
}

func prependRuntimeHealthRules(root *yaml.Node) error {
	rules := mappingValue(root, "rules")
	if rules == nil {
		rules = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMappingNode(root, "rules", rules)
	}
	if rules.Kind != yaml.SequenceNode {
		return errors.New("Mihomo rules must be a sequence")
	}
	owned := []*yaml.Node{
		scalarNode("IP-CIDR,127.0.0.0/8,DIRECT,no-resolve", "!!str"),
		scalarNode("IP-CIDR6,::1/128,DIRECT,no-resolve", "!!str"),
	}
	rules.Content = append(owned, rules.Content...)
	return nil
}

func explicitListeners(port int) *yaml.Node {
	return &yaml.Node{
		Kind: yaml.SequenceNode,
		Tag:  "!!seq",
		Content: []*yaml.Node{
			mappingNode(map[string]*yaml.Node{
				"name":   scalarNode("submux-loopback-ipv4", "!!str"),
				"type":   scalarNode("mixed", "!!str"),
				"port":   scalarNode(strconv.Itoa(port), "!!int"),
				"listen": scalarNode("127.0.0.1", "!!str"),
				"udp":    scalarNode("false", "!!bool"),
				"users":  {Kind: yaml.SequenceNode, Tag: "!!seq"},
			}),
			mappingNode(map[string]*yaml.Node{
				"name":   scalarNode("submux-loopback-ipv6", "!!str"),
				"type":   scalarNode("mixed", "!!str"),
				"port":   scalarNode(strconv.Itoa(port), "!!int"),
				"listen": scalarNode("::1", "!!str"),
				"udp":    scalarNode("false", "!!bool"),
				"users":  {Kind: yaml.SequenceNode, Tag: "!!seq"},
			}),
		},
	}
}

func validateRuntimeYAML(node *yaml.Node, depth int, count *int) error {
	(*count)++
	if *count > 100000 {
		return errors.New("imported Mihomo configuration is too complex")
	}
	if depth > 64 {
		return errors.New("imported Mihomo configuration is nested too deeply")
	}
	if node.Kind == yaml.AliasNode {
		return errors.New("imported Mihomo configuration must not contain YAML aliases")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{})
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Value == "" {
				return errors.New("imported Mihomo configuration contains an invalid mapping key")
			}
			if _, exists := seen[key.Value]; exists {
				return fmt.Errorf("imported Mihomo configuration contains duplicate field %q", key.Value)
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := validateRuntimeYAML(child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

func validateManagedProviders(root *yaml.Node) error {
	for _, section := range []string{"proxy-providers", "rule-providers"} {
		providers := mappingValue(root, section)
		if providers == nil {
			continue
		}
		if providers.Kind != yaml.MappingNode {
			return fmt.Errorf("%s must be a mapping", section)
		}
		if len(providers.Content) != 0 {
			return fmt.Errorf("%s require Runtime-managed resources and are not available yet", section)
		}
	}
	return nil
}

func mappingValue(mapping *yaml.Node, key string) *yaml.Node {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return mapping.Content[index+1]
		}
	}
	return nil
}

func removeMappingKey(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value != key {
			continue
		}
		mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
		return
	}
}

func setMappingScalar(mapping *yaml.Node, key, value, tag string) {
	setMappingNode(mapping, key, scalarNode(value, tag))
}

func setMappingNode(mapping *yaml.Node, key string, value *yaml.Node) {
	removeMappingKey(mapping, key)
	mapping.Content = append(mapping.Content, scalarNode(key, "!!str"), value)
}

func scalarNode(value, tag string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

func mappingNode(values map[string]*yaml.Node) *yaml.Node {
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	order := []string{"name", "type", "port", "listen", "udp", "users", "enable"}
	for _, key := range order {
		value, ok := values[key]
		if !ok {
			continue
		}
		node.Content = append(node.Content, scalarNode(key, "!!str"), value)
	}
	return node
}

type ProxyListener struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
	Port    int    `json:"port"`
}

func ProxyListeners(config []byte) ([]ProxyListener, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(config, &document); err != nil {
		return nil, fmt.Errorf("parse Mihomo proxy listener: %w", err)
	}
	if len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("Mihomo config must be a YAML mapping")
	}
	root := document.Content[0]
	var listeners []ProxyListener
	if custom := mappingValue(root, "listeners"); custom != nil {
		if custom.Kind != yaml.SequenceNode {
			return nil, errors.New("Mihomo listeners must be a sequence")
		}
		for _, entry := range custom.Content {
			if entry.Kind != yaml.MappingNode {
				return nil, errors.New("Mihomo listener must be a mapping")
			}
			kindNode := mappingValue(entry, "type")
			portNode := mappingValue(entry, "port")
			listenNode := mappingValue(entry, "listen")
			if kindNode == nil || portNode == nil || listenNode == nil {
				continue
			}
			kind := strings.ToLower(kindNode.Value)
			if kind != "mixed" && kind != "http" && kind != "socks" && kind != "socks5" {
				continue
			}
			if kind == "socks" {
				kind = "socks5"
			}
			port, err := strconv.Atoi(portNode.Value)
			if err != nil || port < 1 || port > 65535 {
				return nil, errors.New("Mihomo listener port must be between 1 and 65535")
			}
			host := strings.Trim(listenNode.Value, "[]")
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return nil, errors.New("Runtime-owned Mihomo listener must use a loopback IP")
			}
			listeners = append(listeners, ProxyListener{
				Address: net.JoinHostPort(host, strconv.Itoa(port)),
				Kind:    kind,
				Port:    port,
			})
		}
	}
	if len(listeners) > 0 {
		return listeners, nil
	}
	port, kind, err := topLevelProxyEndpoint(root)
	if err != nil || port == 0 {
		return nil, err
	}
	return []ProxyListener{{
		Address: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Kind:    kind,
		Port:    port,
	}}, nil
}

func topLevelProxyEndpoint(root *yaml.Node) (int, string, error) {
	candidates := []struct {
		key  string
		kind string
	}{
		{key: "mixed-port", kind: "mixed"},
		{key: "port", kind: "http"},
		{key: "socks-port", kind: "socks5"},
	}
	for _, candidate := range candidates {
		value := mappingValue(root, candidate.key)
		if value == nil {
			continue
		}
		if value.Kind != yaml.ScalarNode {
			return 0, "", fmt.Errorf("%s must be an integer port", candidate.key)
		}
		port, err := strconv.Atoi(value.Value)
		if err != nil || port < 0 || port > 65535 {
			return 0, "", fmt.Errorf("%s must be between 0 and 65535", candidate.key)
		}
		if port > 0 {
			return port, candidate.kind, nil
		}
	}
	return 0, "", nil
}
