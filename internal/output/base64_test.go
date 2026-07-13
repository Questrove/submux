package output

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestBase64VlessReality(t *testing.T) {
	cfg := map[string]any{"proxies": []any{
		map[string]any{
			"name": "HK 01", "type": "vless", "server": "1.2.3.4", "port": 443,
			"uuid": "u-1", "network": "tcp", "tls": true, "flow": "xtls-rprx-vision",
			"servername": "www.example.com", "client-fingerprint": "chrome",
			"reality-opts": map[string]any{"public-key": "PBK", "short-id": "SID"},
		},
	}}
	body, ct, err := Base64Adapter{}.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("content-type: %q", ct)
	}
	dec, err := base64.StdEncoding.DecodeString(string(body))
	if err != nil {
		t.Fatalf("not base64: %v", err)
	}
	uri := string(dec)
	for _, want := range []string{"vless://u-1@1.2.3.4:443", "encryption=none", "security=reality", "pbk=PBK", "sid=SID", "sni=www.example.com", "flow=xtls-rprx-vision", "#HK%2001"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("uri missing %q:\n%s", want, uri)
		}
	}
}

func TestBase64RejectsNonLosslessNode(t *testing.T) {
	cfg := map[string]any{"proxies": []any{
		map[string]any{"name": "x", "type": "tuic", "server": "h", "port": 1, "password": "p"},
		map[string]any{"name": "v", "type": "vless", "server": "s", "port": 2, "uuid": "u"},
	}}
	if _, _, err := (Base64Adapter{}).Render(cfg); err == nil || !strings.Contains(err.Error(), "tuic=1") {
		t.Fatalf("want strict conversion error, got %v", err)
	}
}

func TestBase64RendersVmessTrojanAndSS(t *testing.T) {
	cfg := map[string]any{"proxies": []any{
		map[string]any{"name": "VM", "type": "vmess", "server": "vm.example.com", "port": 443, "uuid": "vm-id", "network": "ws", "tls": true, "skip-cert-verify": true, "verify-peer-cert-by-name": "peer.example.com", "certificate-sha256": "pin", "ws-opts": map[string]any{"path": "/ws", "headers": map[string]any{"Host": "cdn.example.com"}}},
		map[string]any{"name": "TR", "type": "trojan", "server": "tr.example.com", "port": 443, "password": "tr-pass", "tls": true, "servername": "sni.example.com"},
		map[string]any{"name": "SS", "type": "ss", "server": "ss.example.com", "port": 8388, "cipher": "aes-128-gcm", "password": "ss-pass"},
	}}
	body, _, err := Base64Adapter{}.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(string(body))
	lines := strings.Split(string(decoded), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 links, got %d: %s", len(lines), decoded)
	}
	if !strings.HasPrefix(lines[0], "vmess://") || !strings.HasPrefix(lines[1], "trojan://") || !strings.HasPrefix(lines[2], "ss://") {
		t.Fatalf("unexpected links: %v", lines)
	}
	vmessRaw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(lines[0], "vmess://"))
	if err != nil {
		t.Fatalf("decode vmess: %v", err)
	}
	var vmess map[string]string
	if err := json.Unmarshal(vmessRaw, &vmess); err != nil {
		t.Fatalf("vmess json: %v", err)
	}
	if vmess["id"] != "vm-id" || vmess["net"] != "ws" || vmess["path"] != "/ws" || vmess["host"] != "cdn.example.com" || vmess["insecure"] != "1" || vmess["vcn"] != "peer.example.com" || vmess["pcs"] != "pin" {
		t.Fatalf("vmess fields wrong: %#v", vmess)
	}
}

func TestBase64VlessPreservesEncryption(t *testing.T) {
	cfg := map[string]any{"proxies": []any{map[string]any{
		"name": "VL", "type": "vless", "server": "vl.example.com", "port": 443,
		"uuid": "id", "encryption": "mlkem768x25519plus.native.0rtt", "tls": true,
	}}}
	body, _, err := (Base64Adapter{}).Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(string(body))
	if !strings.Contains(string(decoded), "encryption=mlkem768x25519plus.native.0rtt") {
		t.Fatalf("vless encryption lost: %s", decoded)
	}
}

func TestBase64RejectsUnsupportedTransportField(t *testing.T) {
	cfg := map[string]any{"proxies": []any{map[string]any{
		"name": "VL", "type": "vless", "server": "vl.example.com", "port": 443, "uuid": "id",
		"network": "ws", "ws-opts": map[string]any{"path": "/", "max-early-data": 2048},
	}}}
	if _, _, err := (Base64Adapter{}).Render(cfg); err == nil {
		t.Fatalf("expected strict error for unrepresentable websocket option")
	}
}

func TestBase64RejectsNoSupportedNodes(t *testing.T) {
	cfg := map[string]any{"proxies": []any{
		map[string]any{"name": "HY", "type": "hysteria2", "server": "example.com", "port": 443},
	}}
	if _, _, err := (Base64Adapter{}).Render(cfg); err == nil {
		t.Fatalf("expected error for empty supported output")
	}
}

func TestBase64Hysteria2DocumentedFields(t *testing.T) {
	cfg := map[string]any{"proxies": []any{map[string]any{
		"name": "HY 2", "type": "hysteria2", "server": "hy.example.com", "port": 443,
		"ports": "20000:30000", "password": "user:secret", "obfs": "salamander",
		"obfs-password": "obfs-secret", "sni": "cdn.example.com", "skip-cert-verify": true,
		"fingerprint": "sha256-pin", "up": "100 Mbps", "down": "200 Mbps",
	}}}
	body, _, err := (Base64Adapter{}).Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(string(body))
	uri := string(decoded)
	for _, want := range []string{"hysteria2://user%3Asecret@hy.example.com:443/", "mport=20000-30000", "obfs=salamander", "obfs-password=obfs-secret", "sni=cdn.example.com", "insecure=1", "pinSHA256=sha256-pin", "#HY%202"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("uri missing %q: %s", want, uri)
		}
	}
	if strings.Contains(uri, "up=") || strings.Contains(uri, "down=") {
		t.Fatalf("bandwidth is client policy and must not be shared: %s", uri)
	}
}

func TestBase64Shadowsocks2022UsesPlainUserinfo(t *testing.T) {
	cfg := map[string]any{"proxies": []any{map[string]any{
		"name": "SS22", "type": "ss", "server": "ss.example.com", "port": 8388,
		"cipher": "2022-blake3-aes-128-gcm", "password": "secret/value",
	}}}
	body, _, err := (Base64Adapter{}).Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(string(body))
	uri := string(decoded)
	if !strings.HasPrefix(uri, "ss://2022-blake3-aes-128-gcm:secret%2Fvalue@") {
		t.Fatalf("AEAD-2022 must use plaintext percent-encoded userinfo: %s", uri)
	}
}

func TestBase64ShadowsocksPlugin(t *testing.T) {
	cfg := map[string]any{"proxies": []any{map[string]any{
		"name": "SS", "type": "ss", "server": "ss.example.com", "port": 8388,
		"cipher": "aes-128-gcm", "password": "secret", "plugin": "obfs",
		"plugin-opts": map[string]any{"mode": "http", "host": "cdn.example.com"},
	}}}
	body, _, err := (Base64Adapter{}).Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	decoded, _ := base64.StdEncoding.DecodeString(string(body))
	if uri := string(decoded); !strings.Contains(uri, "plugin=obfs-local%3Bobfs%3Dhttp%3Bobfs-host%3Dcdn.example.com") {
		t.Fatalf("unexpected SIP002 plugin: %s", uri)
	}
}
