package compiler

import (
	"strings"
	"testing"

	"submux/internal/rulecatalog"
	"submux/internal/store"
)

func TestApplyMihomoRuleProfileEmitsOnlySelectedProvidersInOrder(t *testing.T) {
	root := map[string]any{
		"dns": map[string]any{
			"direct-nameserver": []any{"https://dns.alidns.com/dns-query"},
			"nameserver-policy": map[string]any{"+.lan": "system", "rule-set:old": []any{"old"}},
		},
	}
	profile := store.RuleProfile{
		Name: "test", FallbackAction: rulecatalog.ActionProxy,
		CustomRules: []store.CustomRule{{Type: "DOMAIN-SUFFIX", Value: "example.cn", Action: rulecatalog.ActionDirect}},
		Rules: []store.RuleSelection{
			{Key: "geosite/youtube", Action: rulecatalog.ActionMedia},
			{Key: "geosite/cn", Action: rulecatalog.ActionDirect},
			{Key: "geoip/cn", Action: rulecatalog.ActionDirect},
		},
	}
	if err := applyMihomoRuleProfile(root, profile); err != nil {
		t.Fatal(err)
	}
	rules := root["rules"].([]any)
	if len(rules) != 5 || rules[0] != "DOMAIN-SUFFIX,example.cn,DIRECT" || !strings.Contains(rules[1].(string), ",MEDIA") || !strings.Contains(rules[2].(string), ",DIRECT") || !strings.HasSuffix(rules[3].(string), ",DIRECT,no-resolve") || rules[4] != "MATCH,PROXY" {
		t.Fatalf("compiled rule order is wrong: %#v", rules)
	}
	providers := root["rule-providers"].(map[string]any)
	if len(providers) != 3 {
		t.Fatalf("unexpected providers: %#v", providers)
	}
	for _, raw := range providers {
		provider := raw.(map[string]any)
		if provider["proxy"] != "PROXY" || provider["format"] != "mrs" || !strings.Contains(provider["url"].(string), rulecatalog.Commit()) {
			t.Fatalf("provider does not use the pinned snapshot through PROXY: %#v", provider)
		}
	}
	policy := root["dns"].(map[string]any)["nameserver-policy"].(map[string]any)
	if policy["+.lan"] != "system" || policy["rule-set:old"] != nil {
		t.Fatalf("template DNS policies were not preserved or stale rule policy remains: %#v", policy)
	}
	youtubeName := ruleProviderName(mustCatalogEntry(t, "geosite/youtube"))
	youtubeDNS := policy["rule-set:"+youtubeName].([]any)
	if len(youtubeDNS) == 0 || !strings.Contains(youtubeDNS[0].(string), "#MEDIA") {
		t.Fatalf("media DNS does not follow MEDIA: %#v", youtubeDNS)
	}
}

func TestBuiltinMediaSlotAddsIndependentNodesWithProxyFallback(t *testing.T) {
	st := compilerTestStore(t)
	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBuiltinRuleProfiles(); err != nil {
		t.Fatal(err)
	}
	templates, _ := st.ListTemplates()
	version, err := st.GetTemplateVersion(templates[0].CurrentVersionID)
	if err != nil {
		t.Fatal(err)
	}
	_, nodeIDs := addManualNodes(t, st, "Media", strings.Join([]string{
		"vless://00000000-0000-0000-0000-000000000001@main.example.com:443?encryption=none&type=tcp#Main",
		"vless://00000000-0000-0000-0000-000000000002@media.example.com:443?encryption=none&type=tcp#Media",
	}, "\n"))
	profile, _ := st.GetRuleProfileByKey("default")
	result, err := service.Preview(store.OutputSubscription{
		Engine: EngineMihomo, TemplateVersionID: version.ID, RuleProfileID: profile.ID,
		Bindings: []store.SubscriptionBinding{
			{Slot: "primary", NodeIDs: []int64{nodeIDs[0]}},
			{Slot: "media", NodeIDs: []int64{nodeIDs[1]}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	root, err := parseYAMLMap(string(result.Body))
	if err != nil {
		t.Fatal(err)
	}
	groups, _ := objectList(root["proxy-groups"], "proxy-groups")
	for _, group := range groups {
		if stringValue(group["name"]) != "MEDIA" {
			continue
		}
		members, err := optionalStringList(group["proxies"], "proxies")
		if err != nil || len(members) != 2 || members[0] != "PROXY" || !strings.Contains(members[1], "Media") {
			t.Fatalf("MEDIA group is wrong: %v %#v", err, members)
		}
		return
	}
	t.Fatal("MEDIA group is missing")
}

func TestInferTemplateSlotsUsesStandardGroups(t *testing.T) {
	mihomoSlots, err := InferTemplateSlots(EngineMihomo, `proxy-groups:
  - name: PROXY
    type: select
    proxies: []
  - name: MEDIA
    type: select
    proxies: [PROXY]
`)
	if err != nil || len(mihomoSlots) != 2 || mihomoSlots[0].Key != "primary" || mihomoSlots[0].Target != "PROXY" || !mihomoSlots[0].Required || mihomoSlots[1].Key != "media" || mihomoSlots[1].Target != "MEDIA" || mihomoSlots[1].Required {
		t.Fatalf("Mihomo standard groups were not inferred: slots=%+v err=%v", mihomoSlots, err)
	}
	if _, err := InferTemplateSlots(EngineMihomo, "proxy-groups:\n  - name: OTHER\n    type: select\n    proxies: []\n"); err == nil {
		t.Fatal("Mihomo template without PROXY was accepted")
	}
	singBoxSlots, err := InferTemplateSlots(EngineSingBox, `{"outbounds":[{"type":"selector","tag":"AUTO","outbounds":[]},{"type":"selector","tag":"MEDIA","outbounds":["AUTO"]}]}`)
	if err != nil || len(singBoxSlots) != 2 || singBoxSlots[0].Target != "AUTO" || singBoxSlots[1].Target != "MEDIA" {
		t.Fatalf("sing-box standard groups were not inferred: slots=%+v err=%v", singBoxSlots, err)
	}
}

func TestExistingRuleProfileStaysPinnedAfterCatalogRefresh(t *testing.T) {
	st := compilerTestStore(t)
	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBuiltinRuleProfiles(); err != nil {
		t.Fatal(err)
	}
	profile, err := st.GetRuleProfileByKey("default")
	if err != nil {
		t.Fatal(err)
	}
	if profile.CatalogCommit != rulecatalog.Commit() {
		t.Fatalf("default profile is not pinned to its creation catalog: %+v", profile)
	}
	refreshed := rulecatalog.Catalog()
	refreshed.Commit = strings.Repeat("c", 40)
	refreshed.Origin = "github"
	if err := rulecatalog.SaveActiveCatalog(st, refreshed); err != nil {
		t.Fatal(err)
	}

	templates, _ := st.ListTemplates()
	version, err := st.GetTemplateVersion(templates[0].CurrentVersionID)
	if err != nil {
		t.Fatal(err)
	}
	_, nodeIDs := addManualNodes(t, st, "Pinned", "vless://00000000-0000-0000-0000-000000000001@node.example.com:443?encryption=none&type=tcp#Pinned")
	result, err := service.Preview(store.OutputSubscription{
		Engine: EngineMihomo, TemplateVersionID: version.ID, RuleProfileID: profile.ID,
		Bindings: []store.SubscriptionBinding{{Slot: "primary", NodeIDs: nodeIDs}},
	})
	if err != nil {
		t.Fatal(err)
	}
	compiled := string(result.Body)
	if !strings.Contains(compiled, rulecatalog.Commit()) || strings.Contains(compiled, refreshed.Commit) {
		t.Fatalf("existing profile silently changed catalog version:\n%s", compiled)
	}
}

func mustCatalogEntry(t *testing.T, key string) rulecatalog.Entry {
	t.Helper()
	entry, ok := rulecatalog.Lookup(key)
	if !ok {
		t.Fatalf("missing catalog entry %q", key)
	}
	return entry
}
