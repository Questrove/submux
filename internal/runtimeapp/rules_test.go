package runtimeapp

import (
	"os"
	"path/filepath"
	"testing"

	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
)

func TestMihomoExecutorReadsAndFiltersAppliedFinalRules(t *testing.T) {
	root := t.TempDir()
	current := filepath.Join(root, "current")
	if err := os.MkdirAll(current, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "source.yaml"), []byte("rules:\n  - DOMAIN,source.example,DIRECT\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(current, "config.yaml"), []byte("rules:\n  - IP-CIDR,127.0.0.0/8,DIRECT,no-resolve\n  - IP-CIDR6,::1/128,DIRECT,no-resolve\n  - DOMAIN,source.example,DIRECT\n  - DOMAIN-SUFFIX,proxy.example,PROXY\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executor := &MihomoExecutor{ConfigRoot: root}
	rules, err := executor.AppliedRules(t.Context(), runtimeapi.RuleQuery{Content: "example", Type: "domain", Target: "proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if rules.View != runtimeapi.RuleViewApplied || rules.Total != 1 || len(rules.Items) != 1 ||
		rules.Items[0].Order != 4 || rules.Items[0].Type != "DOMAIN-SUFFIX" ||
		rules.Items[0].Origin != runtimeapi.RuleOriginAdvancedOverride {
		t.Fatalf("applied rules=%#v", rules)
	}
}

func TestMihomoExecutorRejectsOversizedAppliedRuleFilter(t *testing.T) {
	executor := &MihomoExecutor{ConfigRoot: t.TempDir()}
	if _, err := executor.AppliedRules(t.Context(), runtimeapi.RuleQuery{Content: string(make([]byte, runtimeapi.RuleFilterMaxLength+1))}); err == nil {
		t.Fatal("oversized rule filter accepted")
	}
}

func TestCandidateRuleOriginUsesLayerReplacedByRuntimeRules(t *testing.T) {
	for _, test := range []struct {
		name     string
		origin   mihomo.CandidateFieldOrigin
		expected string
	}{
		{name: "source", origin: mihomo.CandidateFieldOrigin{Path: "rules", Origin: mihomo.CandidateOriginRuntime, ReplacedOrigin: mihomo.CandidateOriginSource}, expected: runtimeapi.RuleOriginSource},
		{name: "advanced override", origin: mihomo.CandidateFieldOrigin{Path: "rules", Origin: mihomo.CandidateOriginRuntime, ReplacedOrigin: mihomo.CandidateOriginOverride}, expected: runtimeapi.RuleOriginAdvancedOverride},
	} {
		t.Run(test.name, func(t *testing.T) {
			if actual := candidateRuleOrigin([]mihomo.CandidateFieldOrigin{test.origin}); actual != test.expected {
				t.Fatalf("candidate rule origin=%q", actual)
			}
		})
	}
}
