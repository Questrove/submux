package compiler

import (
	"strings"
	"testing"

	"submux/internal/store"
)

func TestEnsureBuiltinTemplatesSeedsDesktopTUNAndLinuxServer(t *testing.T) {
	st := compilerTestStore(t)
	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBuiltinRuleProfiles(); err != nil {
		t.Fatal(err)
	}
	templates, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 2 || templates[0].Name != "Mihomo 桌面 TUN" || templates[1].Name != "Mihomo Linux 服务器" {
		t.Fatalf("want desktop TUN and Linux server builtins, got %#v", templates)
	}
	var version store.TemplateVersion
	for _, template := range templates {
		if template.Name == "Mihomo 桌面 TUN" {
			version, err = st.GetTemplateVersion(template.CurrentVersionID)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if version.ID == 0 {
		t.Fatal("recommended Mihomo desktop TUN template was not installed")
	}
	for _, required := range []string{
		"log-level: warning",
		"find-process-mode: strict",
		"stack: mixed",
		"strict-route: true",
		"route-exclude-address:\n    - 192.0.2.0/24\n    - 198.51.100.0/24",
		"sniff:\n    HTTP:",
		"cache-algorithm: arc",
		"direct-nameserver:",
		"https://1.1.1.1/dns-query#PROXY",
		"- name: MEDIA",
		"rules: []",
	} {
		if !strings.Contains(version.Content, required) {
			t.Fatalf("recommended desktop TUN template missing %q:\n%s", required, version.Content)
		}
	}
	if strings.Count(version.Content, "ipv6: false") != 2 {
		t.Fatalf("IPv6 must be disabled at both general and DNS levels:\n%s", version.Content)
	}
	if len(version.Slots) != 2 || version.Slots[0].Target != "PROXY" || version.Slots[1].Target != "MEDIA" || version.Slots[1].Required {
		t.Fatalf("desktop template must expose primary and optional media slots: %#v", version.Slots)
	}
	if strings.Contains(version.Content, "name: AUTO") || strings.Contains(version.Content, "type: url-test") {
		t.Fatalf("recommended desktop TUN template still contains automatic node selection:\n%s", version.Content)
	}
	if !strings.Contains(version.Content, "- name: PROXY\n    type: select\n    proxies: []") {
		t.Fatalf("recommended desktop TUN template is missing the manual PROXY group:\n%s", version.Content)
	}
	for _, forbidden := range []string{
		"auto-redirect: true",
		"fallback:",
		"listen: 0.0.0.0",
		"fake-ip-range6",
		"global-client-fingerprint",
		"browserleaks.com",
		"DST-PORT,25",
		"DOMAIN-SUFFIX,gov,DIRECT",
		"rule-providers:",
		"DOMAIN-SUFFIX,",
	} {
		if strings.Contains(version.Content, forbidden) {
			t.Fatalf("recommended desktop TUN template contains forbidden legacy or service-specific setting %q:\n%s", forbidden, version.Content)
		}
	}

	_, nodeIDs := addManualNodes(t, st, "Home", "vless://id@example.com:443?encryption=none&type=tcp#Node")
	profile, err := st.GetRuleProfileByKey("default")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Preview(store.OutputSubscription{
		Engine: EngineMihomo, TemplateVersionID: version.ID, RuleProfileID: profile.ID,
		Bindings: []store.SubscriptionBinding{{Slot: "primary", NodeIDs: nodeIDs}},
	})
	if err != nil {
		t.Fatalf("compile recommended desktop TUN template: %v", err)
	}
	if !strings.Contains(string(result.Body), "server: example.com") {
		t.Fatalf("compiled desktop TUN template is missing selected node:\n%s", result.Body)
	}
	for _, required := range []string{"format: mrs", "proxy: PROXY", "MATCH,PROXY"} {
		if !strings.Contains(string(result.Body), required) {
			t.Fatalf("compiled desktop TUN configuration is missing rule profile output %q:\n%s", required, result.Body)
		}
	}
	if strings.Contains(string(result.Body), "DOMAIN-SUFFIX,") {
		t.Fatalf("compiled desktop TUN configuration contains user-specific defaults:\n%s", result.Body)
	}
}

func TestEnsureBuiltinTemplatesUpgradesV3TUNTemplateWithWireGuardRoutes(t *testing.T) {
	st := compilerTestStore(t)
	item := mihomoDesktopTUNTemplate()
	routeBlock := "  route-exclude-address:\n    - 192.0.2.0/24\n    - 198.51.100.0/24\n"
	legacyContent := strings.Replace(item.content, routeBlock, "", 1)
	templateID, err := st.SaveTemplate(store.Template{
		Name: item.name, Engine: item.engine, Scenario: item.scenario, Description: item.description, Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyVersion, err := st.PublishTemplateVersion(templateID, item.engineVersion, legacyContent, item.slots)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("builtin_templates_v3", "1"); err != nil {
		t.Fatal(err)
	}
	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	template, err := st.GetTemplate(templateID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := st.GetTemplateVersion(template.CurrentVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Version != 1 || current.ID != legacyVersion.ID || !strings.Contains(current.Content, routeBlock) {
		t.Fatalf("v3 TUN template was not upgraded with WireGuard exclusions: %#v", current)
	}
	if current.Checksum == legacyVersion.Checksum {
		t.Fatal("in-place development migration did not update the version checksum")
	}
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	again, _ := st.GetTemplate(templateID)
	if again.CurrentVersionID != current.ID {
		t.Fatalf("v5 catalog upgrade is not idempotent: first=%d second=%d", current.ID, again.CurrentVersionID)
	}
}

func TestEnsureBuiltinTemplatesV5OverwritesCurrentVersionInDevelopment(t *testing.T) {
	st := compilerTestStore(t)
	item := mihomoDesktopTUNTemplate()
	templateID, err := st.SaveTemplate(store.Template{
		Name: item.name, Engine: item.engine, Scenario: item.scenario, Description: item.description, Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyContent := strings.Replace(item.content, "    - 198.51.100.0/24", "    - 172.28.8.1/32", 1)
	if _, err := st.PublishTemplateVersion(templateID, item.engineVersion, legacyContent, item.slots); err != nil {
		t.Fatal(err)
	}
	currentBefore, err := st.PublishTemplateVersion(templateID, "custom", legacyContent+"\n# user version\n", item.slots)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("builtin_templates_v4", "1"); err != nil {
		t.Fatal(err)
	}
	if err := New(st).EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	template, _ := st.GetTemplate(templateID)
	currentAfter, err := st.GetTemplateVersion(template.CurrentVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if template.CurrentVersionID != currentBefore.ID || currentAfter.Version != currentBefore.Version || currentAfter.Content != item.content {
		t.Fatalf("current development version was not overwritten in place: before=%#v after=%#v", currentBefore, currentAfter)
	}
}

func TestEnsureBuiltinTemplatesV6SimplifiesAllMihomoGroupsInPlace(t *testing.T) {
	st := compilerTestStore(t)
	legacyContent := `proxy-groups:
  - name: AUTO
    type: url-test
    proxies: []
  - name: PROXY
    type: select
    proxies: [AUTO, DIRECT]
rules:
  - MATCH,PROXY
`
	legacySlots := []store.TemplateSlot{{Key: "primary", Target: "AUTO", Mode: "replace", Required: true}}
	expected := make(map[string]builtinTemplate)
	versionIDs := make(map[string]int64)
	for _, item := range builtinTemplates() {
		if item.engine != EngineMihomo {
			continue
		}
		expected[item.name] = item
		templateID, err := st.SaveTemplate(store.Template{
			Name: item.name, Engine: item.engine, Scenario: item.scenario, Description: item.description, Status: "draft",
		})
		if err != nil {
			t.Fatal(err)
		}
		version, err := st.PublishTemplateVersion(templateID, item.engineVersion, legacyContent, legacySlots)
		if err != nil {
			t.Fatal(err)
		}
		versionIDs[item.name] = version.ID
	}
	if err := st.SetSetting("builtin_templates_v5", "1"); err != nil {
		t.Fatal(err)
	}
	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	templates, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range templates {
		item, ok := expected[template.Name]
		if !ok {
			continue
		}
		if template.CurrentVersionID != versionIDs[template.Name] {
			t.Fatalf("%s migration created a new immutable version: got %d want %d", template.Name, template.CurrentVersionID, versionIDs[template.Name])
		}
		current, err := st.GetTemplateVersion(template.CurrentVersionID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Content != item.content {
			t.Fatalf("%s was not migrated to the current single-group template", template.Name)
		}
		if len(current.Slots) != 2 || current.Slots[0].Target != "PROXY" || current.Slots[1].Target != "MEDIA" {
			t.Fatalf("%s still targets the legacy AUTO group: %#v", template.Name, current.Slots)
		}
	}
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureBuiltinTemplatesV7DisablesAutomaticNodeSwitching(t *testing.T) {
	st := compilerTestStore(t)
	v6Content := `proxy-groups:
  - name: PROXY
    type: url-test
    proxies: []
    url: https://www.gstatic.com/generate_204
    interval: 300
    tolerance: 50
rules:
  - MATCH,PROXY
`
	slots := []store.TemplateSlot{{Key: "primary", Target: "PROXY", Mode: "replace", Required: true}}
	expected := make(map[string]builtinTemplate)
	for _, item := range builtinTemplates() {
		if item.engine != EngineMihomo {
			continue
		}
		expected[item.name] = item
		templateID, err := st.SaveTemplate(store.Template{
			Name: item.name, Engine: item.engine, Scenario: item.scenario, Description: item.description, Status: "draft",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PublishTemplateVersion(templateID, item.engineVersion, v6Content, slots); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SetSetting("builtin_templates_v6", "1"); err != nil {
		t.Fatal(err)
	}
	if err := New(st).EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	templates, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range templates {
		item, ok := expected[template.Name]
		if !ok {
			continue
		}
		current, err := st.GetTemplateVersion(template.CurrentVersionID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Content != item.content || strings.Contains(current.Content, "type: url-test") {
			t.Fatalf("%s still enables automatic node switching:\n%s", template.Name, current.Content)
		}
		if !strings.Contains(current.Content, "- name: PROXY\n    type: select\n    proxies: []") {
			t.Fatalf("%s is missing the manual PROXY selector:\n%s", template.Name, current.Content)
		}
	}
}

func TestEnsureBuiltinTemplatesUpgradesV2CatalogOnce(t *testing.T) {
	st := compilerTestStore(t)
	if err := st.SetSetting("builtin_templates_v2", "1"); err != nil {
		t.Fatal(err)
	}
	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBuiltinRuleProfiles(); err != nil {
		t.Fatal(err)
	}
	first, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Name != "Mihomo 桌面 TUN" || first[1].Name != "Mihomo Linux 服务器" {
		t.Fatalf("v2 catalog upgrade should install the current catalog: %#v", first)
	}
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	second, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 2 || second[0].CurrentVersionID != first[0].CurrentVersionID || second[1].CurrentVersionID != first[1].CurrentVersionID {
		t.Fatalf("builtin catalog upgrade was not idempotent: first=%#v second=%#v", first, second)
	}
}

func TestLinuxServerTemplateIsRootlessAgentSafe(t *testing.T) {
	st := compilerTestStore(t)
	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	if err := service.EnsureBuiltinRuleProfiles(); err != nil {
		t.Fatal(err)
	}
	templates, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	var version store.TemplateVersion
	for _, template := range templates {
		if template.Name == "Mihomo Linux 服务器" {
			version, err = st.GetTemplateVersion(template.CurrentVersionID)
			if err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if version.ID == 0 {
		t.Fatal("Linux server template was not installed")
	}
	for _, required := range []string{
		"mixed-port: 7890", "allow-lan: false", "bind-address: 127.0.0.1",
		"find-process-mode: off", "enhanced-mode: redir-host", "cache-algorithm: arc",
		"- name: MEDIA", "rules: []",
	} {
		if !strings.Contains(version.Content, required) {
			t.Fatalf("Linux server template is missing %q:\n%s", required, version.Content)
		}
	}
	for _, forbidden := range []string{
		"tun:", "redir-port:", "tproxy-port:", "external-controller:", "secret:",
		"external-ui:", "listen: 0.0.0.0", "enhanced-mode: fake-ip", "store-fake-ip: true",
	} {
		if strings.Contains(version.Content, forbidden) {
			t.Fatalf("Linux server template contains unsafe or unnecessary setting %q:\n%s", forbidden, version.Content)
		}
	}
	_, nodeIDs := addManualNodes(t, st, "Server", "trojan://password@example.com:443#Node")
	profile, err := st.GetRuleProfileByKey("default")
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Preview(store.OutputSubscription{
		Engine: EngineMihomo, TemplateVersionID: version.ID, RuleProfileID: profile.ID,
		Bindings: []store.SubscriptionBinding{{Slot: "primary", NodeIDs: nodeIDs}},
	})
	if err != nil {
		t.Fatalf("compile Linux server template: %v", err)
	}
	if !strings.Contains(string(result.Body), "server: example.com") {
		t.Fatalf("compiled Linux server template is missing selected node:\n%s", result.Body)
	}
	if !strings.Contains(string(result.Body), "RULE-SET") || strings.Contains(string(result.Body), "DOMAIN-SUFFIX,") {
		t.Fatalf("compiled Linux server template is missing the default rule profile:\n%s", result.Body)
	}
}

func TestEnsureBuiltinTemplatesRemovesUnreferencedLegacyBuiltinsAndPreservesCustomTemplates(t *testing.T) {
	st := compilerTestStore(t)
	for _, item := range legacyBuiltinTemplates() {
		templateID, err := st.SaveTemplate(store.Template{
			Name: item.name, Engine: item.engine, Scenario: item.scenario,
			Description: item.description, Status: "draft",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.PublishTemplateVersion(templateID, item.engineVersion, item.content, item.slots); err != nil {
			t.Fatal(err)
		}
	}
	customID, err := st.SaveTemplate(store.Template{
		Name: "我的 Mihomo 模板", Engine: EngineMihomo, Scenario: "custom", Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PublishTemplateVersion(customID, "custom", "rules:\n  - MATCH,DIRECT\n", nil); err != nil {
		t.Fatal(err)
	}

	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	templates, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 3 {
		t.Fatalf("want two builtins plus custom template, got %#v", templates)
	}
	foundDesktop, foundServer, foundCustom := false, false, false
	for _, template := range templates {
		switch template.ID {
		case customID:
			foundCustom = template.Name == "我的 Mihomo 模板"
		default:
			foundDesktop = foundDesktop || template.Name == "Mihomo 桌面 TUN"
			foundServer = foundServer || template.Name == "Mihomo Linux 服务器"
		}
		if retiredBuiltinTemplateKeys[templateKey(template.Engine, template.Name)] {
			t.Fatalf("unreferenced legacy builtin was not removed: %#v", template)
		}
	}
	if !foundDesktop || !foundServer || !foundCustom {
		t.Fatalf("catalog reconciliation lost expected templates: %#v", templates)
	}
}

func TestEnsureBuiltinTemplatesRetiresReferencedLegacyBuiltin(t *testing.T) {
	st := compilerTestStore(t)
	item := legacyBuiltinTemplates()[0]
	templateID, err := st.SaveTemplate(store.Template{
		Name: item.name, Engine: item.engine, Scenario: item.scenario,
		Description: item.description, Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.PublishTemplateVersion(templateID, item.engineVersion, item.content, item.slots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveOutputSubscription(store.OutputSubscription{
		Name: "legacy", Engine: item.engine, TemplateVersionID: version.ID,
		Token: "legacy-token", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	if err := New(st).EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	legacy, err := st.GetTemplate(templateID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Status != "retired" || legacy.CurrentVersionID != version.ID {
		t.Fatalf("referenced legacy builtin was not safely retired: %#v", legacy)
	}
	if _, err := st.GetTemplateVersion(version.ID); err != nil {
		t.Fatalf("referenced legacy template version was deleted: %v", err)
	}
	seeded, err := st.GetSetting(builtinTemplatesCatalogVersion)
	if err != nil || seeded != "1" {
		t.Fatalf("catalog migration was not recorded: value=%q err=%v", seeded, err)
	}
}

func TestEnsureBuiltinTemplatesV11RenamesEntriesAndExtractsRules(t *testing.T) {
	st := compilerTestStore(t)
	desktop := mihomoDesktopTUNTemplate()
	legacyDesktopID, err := st.SaveTemplate(store.Template{
		Name: "Mihomo 桌面 TUN（推荐）", Engine: desktop.engine, Scenario: desktop.scenario,
		Description: desktop.description, Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	legacyContent := strings.Replace(desktop.content, "rules: []", "rules:\n  - DOMAIN-SUFFIX,example.com,DIRECT\n  - MATCH,PROXY", 1)
	legacyVersion, err := st.PublishTemplateVersion(legacyDesktopID, desktop.engineVersion, legacyContent, desktop.slots)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("builtin_templates_v10", "1"); err != nil {
		t.Fatal(err)
	}

	service := New(st)
	if err := service.EnsureBuiltinTemplates(); err != nil {
		t.Fatal(err)
	}
	renamed, err := st.GetTemplate(legacyDesktopID)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Name != "Mihomo 桌面 TUN" || renamed.CurrentVersionID != legacyVersion.ID || renamed.Status != "published" {
		t.Fatalf("desktop catalog entry was not renamed in place: %#v", renamed)
	}
	current, err := st.GetTemplateVersion(renamed.CurrentVersionID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(current.Content, "DOMAIN-SUFFIX,example.com,DIRECT") || !strings.Contains(current.Content, "rules: []") || !strings.Contains(current.Content, "name: MEDIA") {
		t.Fatalf("renamed desktop version did not receive the v11 rule extraction: %s", current.Content)
	}
	templates, err := st.ListTemplates()
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 2 || templates[0].Name != "Mihomo 桌面 TUN" || templates[1].Name != "Mihomo Linux 服务器" {
		t.Fatalf("unexpected v11 catalog: %#v", templates)
	}
	seeded, err := st.GetSetting(builtinTemplatesCatalogVersion)
	if err != nil || seeded != "1" {
		t.Fatalf("v11 catalog migration was not recorded: value=%q err=%v", seeded, err)
	}
}
