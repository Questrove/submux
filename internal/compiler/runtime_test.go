package compiler

import "testing"

func TestAnalyzeMihomoRuntimeReportsRisksWithoutRejecting(t *testing.T) {
	analysis, err := AnalyzeMihomoRuntime(`
allow-lan: true
bind-address: "*"
external-controller: 0.0.0.0:9090
external-controller-unix: mihomo.sock
external-controller-pipe: '\\.\pipe\mihomo'
external-controller-tls: 0.0.0.0:9443
external-doh-server: /dns-query
external-controller-cors: {allow-origins: ['*']}
external-ui: ./ui
tun: {enable: true}
tproxy-port: 7893
proxy-groups:
  - {name: Manual, type: select, proxies: [DIRECT]}
`, RuntimeContractMihomoAgentV1)
	if err != nil {
		t.Fatal(err)
	}
	if !analysis.RequiresNetAdmin || len(analysis.Risks) < 9 {
		t.Fatalf("expected non-blocking runtime risks, got %#v", analysis)
	}
	wantRisk := map[string]bool{
		"external_controller_unix_enabled":    false,
		"external_controller_pipe_enabled":    false,
		"external_controller_tls_enabled":     false,
		"external_doh_server_enabled":         false,
		"external_controller_cors_configured": false,
		"external_ui_enabled":                 false,
	}
	for _, risk := range analysis.Risks {
		if _, ok := wantRisk[risk.Code]; ok {
			wantRisk[risk.Code] = true
		}
	}
	for code, found := range wantRisk {
		if !found {
			t.Fatalf("missing runtime risk %q: %#v", code, analysis.Risks)
		}
	}
	if len(analysis.SelectableGroups) != 1 || analysis.SelectableGroups[0] != "Manual" {
		t.Fatalf("select group not discovered: %#v", analysis.SelectableGroups)
	}
}

func TestValidateRuntimeContractIsStrict(t *testing.T) {
	if err := ValidateRuntimeContract(EngineSingBox, RuntimeContractMihomoAgentV1); err == nil {
		t.Fatal("sing-box accepted the Mihomo Agent contract")
	}
	if err := ValidateRuntimeContract(EngineMihomo, "mihomo-agent/v2"); err == nil {
		t.Fatal("unknown runtime contract was accepted")
	}
}
