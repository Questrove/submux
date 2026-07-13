package parse

import (
	"encoding/base64"
	"fmt"
	"testing"
)

func TestParseBase64ShareSubscription(t *testing.T) {
	vmessJSON := `{"v":"2","ps":"VM","add":"vm.example.com","port":"443","id":"vm-id","aid":"0","scy":"auto","net":"ws","host":"cdn.example.com","path":"/ws","tls":"tls","sni":"sni.example.com"}`
	vmess := "vmess://" + base64.StdEncoding.EncodeToString([]byte(vmessJSON))
	ssCredential := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret"))
	lines := fmt.Sprintf("vless://vl-id@vl.example.com:443?security=reality&type=grpc&serviceName=svc&sni=sni.example.com&pbk=key&sid=sid#VL\n"+
		"trojan://trojan-pass@tr.example.com:443?security=tls&sni=tr-sni.example.com#TR\n"+
		"ss://%s@ss.example.com:8388#SS\n%s\n", ssCredential, vmess)
	raw := base64.StdEncoding.EncodeToString([]byte(lines))

	nodes, err := ParseSubscription(raw)
	if err != nil {
		t.Fatalf("ParseSubscription: %v", err)
	}
	if len(nodes) != 4 {
		t.Fatalf("want 4 nodes, got %d: %#v", len(nodes), nodes)
	}
	if nodes[0]["type"] != "vless" || nodes[0]["uuid"] != "vl-id" || nodes[0]["network"] != "grpc" {
		t.Fatalf("vless wrong: %#v", nodes[0])
	}
	if nodes[1]["type"] != "trojan" || nodes[1]["password"] != "trojan-pass" {
		t.Fatalf("trojan wrong: %#v", nodes[1])
	}
	if nodes[2]["type"] != "ss" || nodes[2]["cipher"] != "aes-128-gcm" || nodes[2]["password"] != "secret" {
		t.Fatalf("ss wrong: %#v", nodes[2])
	}
	if nodes[3]["type"] != "vmess" || nodes[3]["uuid"] != "vm-id" || nodes[3]["network"] != "ws" {
		t.Fatalf("vmess wrong: %#v", nodes[3])
	}
}

func TestParseSubscriptionSupportsHysteria2(t *testing.T) {
	raw := "hysteria2://user%3Asecret@example.com:443/?mport=20000-30000&obfs=salamander&obfs-password=op&sni=cdn.example.com&insecure=1&pinSHA256=pin#HY\n" +
		"trojan://pass@example.com:443#TR\n"
	nodes, err := ParseSubscription(raw)
	if err != nil || len(nodes) != 2 || nodes[0]["type"] != "hysteria2" {
		t.Fatalf("unexpected result: nodes=%#v err=%v", nodes, err)
	}
	hy := nodes[0]
	if hy["password"] != "user:secret" || hy["ports"] != "20000:30000" || hy["obfs"] != "salamander" || hy["sni"] != "cdn.example.com" || hy["fingerprint"] != "pin" || hy["skip-cert-verify"] != true {
		t.Fatalf("hysteria2 fields wrong: %#v", hy)
	}
}

func TestParseHysteria2OfficialMultiPortAuthority(t *testing.T) {
	nodes, err := ParseSubscription("hy2://secret@hy.example.com:80,443,2000-3000/?sni=cdn.example.com#HY")
	if err != nil || len(nodes) != 1 {
		t.Fatalf("parse: nodes=%#v err=%v", nodes, err)
	}
	if nodes[0]["port"] != 80 || nodes[0]["ports"] != "80,443,2000:3000" {
		t.Fatalf("official multi-port fields wrong: %#v", nodes[0])
	}
}

func TestParseSubscriptionRejectsUnknownProtocolInsteadOfPartialResult(t *testing.T) {
	raw := "tuic://credential@example.com:443#TUIC\n" +
		"trojan://pass@example.com:443#TR\n"
	if nodes, err := ParseSubscription(raw); err == nil || len(nodes) != 0 {
		t.Fatalf("want fail-closed result, got nodes=%#v err=%v", nodes, err)
	}
}

func TestParseShadowsocks2022AndPlugin(t *testing.T) {
	raw := "ss://2022-blake3-aes-128-gcm:secret%2Fvalue@ss.example.com:8388/?plugin=obfs-local%3Bobfs%3Dhttp%3Bobfs-host%3Dcdn.example.com#SS"
	nodes, err := ParseSubscription(raw)
	if err != nil || len(nodes) != 1 {
		t.Fatalf("parse: nodes=%#v err=%v", nodes, err)
	}
	n := nodes[0]
	if n["cipher"] != "2022-blake3-aes-128-gcm" || n["password"] != "secret/value" || n["plugin"] != "obfs" {
		t.Fatalf("ss fields wrong: %#v", n)
	}
	opts, _ := n["plugin-opts"].(map[string]any)
	if opts["mode"] != "http" || opts["host"] != "cdn.example.com" {
		t.Fatalf("ss plugin options wrong: %#v", opts)
	}
}

func TestParseSubscriptionRejectsInvalidOrEmpty(t *testing.T) {
	for _, raw := range []string{
		"", "not a subscription", "trojan://missing-port@example.com",
		"trojan://pass@example.com:443?security=none",
		"vless://id@example.com:443?type=ws&unknown=value",
	} {
		if nodes, err := ParseSubscription(raw); err == nil || len(nodes) != 0 {
			t.Fatalf("raw %q: want error, got nodes=%#v err=%v", raw, nodes, err)
		}
	}
}
