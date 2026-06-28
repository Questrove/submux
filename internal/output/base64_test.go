package output

import (
	"encoding/base64"
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
	for _, want := range []string{"vless://u-1@1.2.3.4:443", "security=reality", "pbk=PBK", "sid=SID", "sni=www.example.com", "flow=xtls-rprx-vision", "#HK%2001"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("uri missing %q:\n%s", want, uri)
		}
	}
}

func TestBase64SkipsUnknownType(t *testing.T) {
	cfg := map[string]any{"proxies": []any{
		map[string]any{"name": "x", "type": "hysteria2", "server": "h", "port": 1},
		map[string]any{"name": "v", "type": "vless", "server": "s", "port": 2, "uuid": "u"},
	}}
	body, _, _ := Base64Adapter{}.Render(cfg)
	dec, _ := base64.StdEncoding.DecodeString(string(body))
	lines := strings.Split(strings.TrimSpace(string(dec)), "\n")
	if len(lines) != 1 || !strings.HasPrefix(lines[0], "vless://") {
		t.Fatalf("should skip unknown type, got: %v", lines)
	}
}
