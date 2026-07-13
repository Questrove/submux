package parse

import "testing"

const sampleClash = `
proxies:
  - name: "HK-01"
    type: vless
    server: 1.2.3.4
    port: 443
    uuid: abc
  - name: "US-01"
    type: vless
    server: 5.6.7.8
    port: 443
    uuid: def
proxy-groups:
  - name: PROXY
    type: select
rules:
  - MATCH,PROXY
`

func TestParseClash(t *testing.T) {
	nodes, err := ParseClash(sampleClash)
	if err != nil {
		t.Fatalf("ParseClash: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("want 2 nodes, got %d", len(nodes))
	}
	if nodes[0].Name() != "HK-01" {
		t.Fatalf("name wrong: %q", nodes[0].Name())
	}
	if nodes[0]["server"] != "1.2.3.4" {
		t.Fatalf("field not preserved: %v", nodes[0]["server"])
	}
}

func TestParseClashNoProxies(t *testing.T) {
	nodes, err := ParseClash("dns: {}\n")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if nodes != nil {
		t.Fatalf("want nil, got %v", nodes)
	}
}

func TestParseClashInvalid(t *testing.T) {
	_, err := ParseClash("\tfoo: : : bad")
	if err == nil {
		t.Fatalf("expected error for invalid yaml")
	}
}

func TestParseClashRejectsMalformedProxyInsteadOfPartialResult(t *testing.T) {
	raw := "proxies:\n  - {name: valid, type: vless, server: example.com, port: 443, uuid: id}\n  - not-an-object\n"
	if nodes, err := ParseClash(raw); err == nil || len(nodes) != 0 {
		t.Fatalf("want strict proxy validation, got nodes=%#v err=%v", nodes, err)
	}
}
