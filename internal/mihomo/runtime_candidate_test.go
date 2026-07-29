package mihomo

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestExplicitCandidateBuilderOwnsListenersAndControlFields(t *testing.T) {
	source := []byte(`
mixed-port: 9999
allow-lan: true
bind-address: 0.0.0.0
external-controller: 0.0.0.0:9090
external-controller-unix: /tmp/attacker.sock
secret: attacker-secret
external-ui: /tmp/ui
external-ui-url: https://example.com/ui.zip
tls:
  certificate: /tmp/server.crt
  private-key: /tmp/server.key
geox-url:
  geoip: https://example.com/geoip.dat
geo-auto-update: true
interface-name: attacker0
routing-mark: 1234
tun:
  enable: true
dns:
  enable: true
  listen: 0.0.0.0:53
proxies:
  - name: direct-node
    type: direct
rules:
  - MATCH,DIRECT
listeners:
  - name: attacker
    type: mixed
    port: 4000
    listen: 0.0.0.0
`)
	builder := ExplicitCandidateBuilder{
		Port:            17890,
		ControlEndpoint: "/run/submux-runtime/mihomo.sock",
		Platform:        "linux",
	}
	candidate, err := builder.BuildCandidate(source)
	if err != nil {
		t.Fatalf("build explicit proxy candidate: %v", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(candidate, &document); err != nil {
		t.Fatalf("decode candidate: %v", err)
	}
	root := document.Content[0]
	for _, key := range []string{"mixed-port", "port", "socks-port", "redir-port", "tproxy-port"} {
		if value := mappingValue(root, key); value == nil || value.Value != "0" {
			t.Fatalf("%s = %#v, want 0", key, value)
		}
	}
	if value := mappingValue(root, "allow-lan"); value == nil || value.Value != "false" {
		t.Fatalf("allow-lan = %#v", value)
	}
	if value := mappingValue(root, "external-controller-unix"); value == nil || value.Value != builder.ControlEndpoint {
		t.Fatalf("external-controller-unix = %#v", value)
	}
	for _, key := range []string{
		"external-controller",
		"external-ui",
		"external-ui-url",
		"tls",
		"geox-url",
		"geo-auto-update",
		"interface-name",
		"routing-mark",
	} {
		if value := mappingValue(root, key); value != nil {
			t.Fatalf("candidate retained Runtime-reserved field %q: %#v", key, value)
		}
	}
	if value := mappingValue(mappingValue(root, "tun"), "enable"); value == nil || value.Value != "false" {
		t.Fatalf("tun.enable = %#v", value)
	}
	if value := mappingValue(mappingValue(root, "dns"), "listen"); value != nil {
		t.Fatalf("candidate retained dns.listen: %#v", value)
	}
	if mappingValue(root, "proxies") == nil {
		t.Fatal("candidate removed ordinary proxy definitions")
	}
	rules := mappingValue(root, "rules")
	if rules == nil || len(rules.Content) < 3 ||
		!strings.Contains(rules.Content[0].Value, "127.0.0.0/8") ||
		!strings.Contains(rules.Content[1].Value, "::1/128") {
		t.Fatalf("Runtime health rules were not prepended: %#v", rules)
	}
	listeners, err := ProxyListeners(candidate)
	if err != nil {
		t.Fatalf("read explicit listeners: %v", err)
	}
	if len(listeners) != 2 ||
		listeners[0].Address != "127.0.0.1:17890" ||
		listeners[1].Address != "[::1]:17890" {
		t.Fatalf("explicit listeners = %#v", listeners)
	}
}

func TestExplicitCandidateBuilderRejectsUnmanagedFileProviders(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		ControlEndpoint: "/run/submux-runtime/mihomo.sock",
		Platform:        "linux",
	}
	for _, source := range []string{
		"proxy-providers:\n  unsafe:\n    type: file\n    path: /etc/passwd\n",
		"rule-providers:\n  unsafe:\n    type: http\n    path: ../escape.yaml\n    url: https://example.com/rules.yaml\n",
		"proxy-providers:\n  metadata:\n    type: http\n    url: http://169.254.169.254/latest/meta-data/\n",
	} {
		if _, err := builder.BuildCandidate([]byte(source)); err == nil {
			t.Fatalf("builder accepted unmanaged provider: %s", source)
		}
	}
}

func TestExplicitCandidateBuilderRejectsAliasesAndDuplicateFields(t *testing.T) {
	builder := ExplicitCandidateBuilder{
		ControlEndpoint: "/run/submux-runtime/mihomo.sock",
		Platform:        "linux",
	}
	for _, source := range []string{
		"base: &base\n  type: direct\nproxies:\n  - *base\n",
		"mixed-port: 7890\nmixed-port: 7891\n",
	} {
		if _, err := builder.BuildCandidate([]byte(source)); err == nil {
			t.Fatalf("builder accepted unsafe YAML: %s", source)
		}
	}
}
