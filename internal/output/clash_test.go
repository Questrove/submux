package output

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestClashAdapterRender(t *testing.T) {
	cfg := map[string]any{
		"proxies": []any{
			map[string]any{"name": "HK", "type": "vless"},
		},
		"rules": []any{"MATCH,PROXY"},
	}
	body, ct, err := ClashAdapter{}.Render(cfg)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if ct != "text/yaml; charset=utf-8" {
		t.Fatalf("content-type wrong: %q", ct)
	}
	var back map[string]any
	if err := yaml.Unmarshal(body, &back); err != nil {
		t.Fatalf("output not valid yaml: %v", err)
	}
	if _, ok := back["proxies"]; !ok {
		t.Fatalf("proxies missing in output")
	}
}
