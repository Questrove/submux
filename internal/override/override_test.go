package override

import "testing"

func TestApplyEmptyReturnsInput(t *testing.T) {
	cfg := map[string]any{"a": 1}
	out, err := Apply(cfg, "")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out["a"] != 1 {
		t.Fatalf("empty override changed cfg: %v", out)
	}
}

func TestApplyScalarOverrideAndDeepMerge(t *testing.T) {
	cfg := map[string]any{
		"mode": "rule",
		"dns": map[string]any{
			"enable":        true,
			"enhanced-mode": "fake-ip",
		},
	}
	ov := `
mode: global
dns:
  fallback: [1.1.1.1]
`
	out, err := Apply(cfg, ov)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out["mode"] != "global" {
		t.Fatalf("scalar not overridden: %v", out["mode"])
	}
	dns := out["dns"].(map[string]any)
	if dns["enable"] != true || dns["enhanced-mode"] != "fake-ip" {
		t.Fatalf("deep merge lost existing dns keys: %v", dns)
	}
	if _, ok := dns["fallback"]; !ok {
		t.Fatalf("deep merge did not add fallback: %v", dns)
	}
}

func TestApplyInvalidYAML(t *testing.T) {
	if _, err := Apply(map[string]any{}, "\tbad: : :"); err == nil {
		t.Fatalf("expected error")
	}
}

func TestApplyPrependAppendTopLevel(t *testing.T) {
	cfg := map[string]any{"rules": []any{"MATCH,PROXY"}}
	ov := `
prepend-rules:
  - DOMAIN-SUFFIX,a.com,DIRECT
append-rules:
  - DOMAIN-SUFFIX,z.com,REJECT
`
	out, err := Apply(cfg, ov)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rules := out["rules"].([]any)
	if len(rules) != 3 {
		t.Fatalf("want 3 rules, got %v", rules)
	}
	if rules[0] != "DOMAIN-SUFFIX,a.com,DIRECT" || rules[2] != "DOMAIN-SUFFIX,z.com,REJECT" {
		t.Fatalf("prepend/append order wrong: %v", rules)
	}
	if _, ok := out["prepend-rules"]; ok {
		t.Fatalf("directive key leaked into output")
	}
}

func TestApplyAppendNested(t *testing.T) {
	cfg := map[string]any{
		"dns": map[string]any{"fake-ip-filter": []any{"*.lan"}},
		"tun": map[string]any{},
	}
	ov := `
dns:
  append-fake-ip-filter:
    - "+.example.com"
tun:
  append-route-exclude-address:
    - 192.168.1.0/24
`
	out, err := Apply(cfg, ov)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	dns := out["dns"].(map[string]any)
	fif := dns["fake-ip-filter"].([]any)
	if len(fif) != 2 || fif[1] != "+.example.com" {
		t.Fatalf("nested append wrong: %v", fif)
	}
	tun := out["tun"].(map[string]any)
	rea := tun["route-exclude-address"].([]any)
	if len(rea) != 1 || rea[0] != "192.168.1.0/24" {
		t.Fatalf("nested append into empty wrong: %v", rea)
	}
}
