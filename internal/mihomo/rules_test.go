package mihomo

import "testing"

func TestFinalRulesPreserveOrderSyntaxAndLayerOrigin(t *testing.T) {
	candidate := []byte(`
rules:
  - IP-CIDR,127.0.0.0/8,DIRECT,no-resolve
  - IP-CIDR6,::1/128,DIRECT,no-resolve
  - DOMAIN-SUFFIX,example.com,PROXY
  - AND,((DOMAIN,api.example.com),(NETWORK,TCP)),DIRECT
  - MATCH,FALLBACK
`)
	source := []byte(`
rules:
  - DOMAIN-SUFFIX,source.example,PROXY
`)
	rules, err := FinalRules(candidate, source, CandidateOriginOverride)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 5 {
		t.Fatalf("rules=%#v", rules)
	}
	assertRule := func(index int, ruleType, condition, target, origin string) {
		t.Helper()
		rule := rules[index]
		if rule.Order != index+1 || rule.Type != ruleType || rule.Condition != condition || rule.Target != target || rule.Origin != origin {
			t.Fatalf("rule[%d]=%#v", index, rule)
		}
	}
	assertRule(0, "IP-CIDR", "127.0.0.0/8", "DIRECT", CandidateOriginRuntime)
	assertRule(1, "IP-CIDR6", "::1/128", "DIRECT", CandidateOriginRuntime)
	assertRule(2, "DOMAIN-SUFFIX", "example.com", "PROXY", CandidateOriginOverride)
	assertRule(3, "AND", "((DOMAIN,api.example.com),(NETWORK,TCP))", "DIRECT", CandidateOriginOverride)
	assertRule(4, "MATCH", "全部流量", "FALLBACK", CandidateOriginOverride)
}

func TestFinalRulesInferAppliedSourceOrigin(t *testing.T) {
	source := []byte("rules:\n  - DOMAIN,source.example,DIRECT\n")
	candidate := []byte("rules:\n  - IP-CIDR,127.0.0.0/8,DIRECT,no-resolve\n  - IP-CIDR6,::1/128,DIRECT,no-resolve\n  - DOMAIN,source.example,DIRECT\n")
	rules, err := FinalRules(candidate, source, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 || rules[2].Origin != CandidateOriginSource {
		t.Fatalf("rules=%#v", rules)
	}
}

func TestFinalRulesRejectMalformedRule(t *testing.T) {
	for _, candidate := range [][]byte{
		[]byte("rules: [DOMAIN-only]\n"),
		[]byte("rules:\n  - type: DOMAIN\n"),
	} {
		if _, err := FinalRules(candidate, nil, ""); err == nil {
			t.Fatalf("malformed rule accepted: %s", candidate)
		}
	}
}
