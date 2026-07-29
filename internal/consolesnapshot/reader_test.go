package consolesnapshot

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/fakeip"
	"submux/internal/resourceproxy"
	"submux/internal/rulecatalog"
	"submux/internal/store"
)

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestReaderReadBuildsCompleteConsoleSnapshot(t *testing.T) {
	st := openTestStore(t)
	for key, value := range map[string]string{
		"base_url":                   "https://sub.example/",
		"fetch_interval_sec":         "600",
		resourceproxy.SettingMode:    resourceproxy.ModeHTTP,
		resourceproxy.SettingURL:     "http://127.0.0.1:7890",
		fakeip.SettingKey:            `{"mode":"whitelist","entries":["example.com"]}`,
		"rule_catalog_active_commit": rulecatalog.Catalog().Commit,
		"rule_catalog_refresh_state": `{"status":"ready","updated_at":"2026-07-29T00:00:00Z"}`,
	} {
		if err := st.SetSetting(key, value); err != nil {
			t.Fatal(err)
		}
	}

	sourceID, err := st.CreateSource(store.Source{
		Kind: store.SourceKindSubscription, Name: "source", URL: "https://upstream.example/sub",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CommitSourceRefresh(sourceID, []store.NodeRecord{{
		SourceID: sourceID, Name: "node", Protocol: "ss",
		Config: json.RawMessage(`{"server":"node.example","port":443}`), Fingerprint: "node-1",
	}}, `{"total":1024}`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordLifecycleState(sourceID, "healthy"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordLifecycleState(sourceID, "warning"); err != nil {
		t.Fatal(err)
	}
	nodes, err := st.ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes = %#v, err = %v", nodes, err)
	}

	templateID, err := st.SaveTemplate(store.Template{
		Name: "desktop", Engine: "mihomo", Scenario: "desktop", Status: "draft",
	})
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.PublishTemplateVersion(templateID, "1", "proxies: []", []store.TemplateSlot{{
		Key: "main", Target: "proxies", Required: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	profileID, err := st.SaveRuleProfile(store.RuleProfile{
		Name: "rules", CatalogCommit: rulecatalog.Catalog().Commit, FallbackAction: "DIRECT",
	})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := st.OutputInputsGeneration()
	if err != nil {
		t.Fatal(err)
	}
	subscriptionID, err := st.SaveOutputSubscriptionWithArtifact(store.OutputSubscription{
		Name: "output", TemplateVersionID: version.ID, RuleProfileID: profileID,
		Engine: "mihomo", Token: "console-token", Enabled: true,
		Bindings: []store.SubscriptionBinding{{Slot: "main", NodeIDs: []int64{nodes[0].ID}}},
	}, store.SubscriptionArtifact{
		Body: []byte("secret generated body"), ContentType: "text/yaml", Revision: "rev-1",
	}, []string{"warning"}, store.OutputPublicationGuard{InputsGeneration: generation})
	if err != nil {
		t.Fatal(err)
	}

	fixedNow := time.Date(2026, 7, 29, 12, 30, 0, 0, time.FixedZone("test", 8*60*60))
	reader := New(st)
	reader.now = func() time.Time { return fixedNow }
	snapshot, err := reader.Read()
	if err != nil {
		t.Fatal(err)
	}

	if snapshot.GeneratedAt != "2026-07-29T04:30:00Z" {
		t.Fatalf("generated_at = %q", snapshot.GeneratedAt)
	}
	if snapshot.Settings.BaseURL != "https://sub.example/" || snapshot.Settings.FetchIntervalSec != 600 {
		t.Fatalf("settings = %#v", snapshot.Settings)
	}
	if snapshot.Settings.PlatformResourceProxy.Mode != resourceproxy.ModeHTTP {
		t.Fatalf("proxy settings = %#v", snapshot.Settings.PlatformResourceProxy)
	}
	if snapshot.Settings.SharedFakeIPFilter.Mode != fakeip.ModeWhitelist {
		t.Fatalf("fake-ip settings = %#v", snapshot.Settings.SharedFakeIPFilter)
	}
	if len(snapshot.Sources) != 1 || snapshot.Sources[0].NodeCount != 1 || snapshot.Sources[0].Lifecycle == nil {
		t.Fatalf("sources = %#v", snapshot.Sources)
	}
	if len(snapshot.LifecycleEvents) != 1 || snapshot.LifecycleEvents[0].ToState != "warning" {
		t.Fatalf("lifecycle events = %#v", snapshot.LifecycleEvents)
	}
	if len(snapshot.Templates) != 1 || len(snapshot.Templates[0].Versions) != 1 {
		t.Fatalf("templates = %#v", snapshot.Templates)
	}
	if snapshot.RuleCatalog.ActiveCommit != rulecatalog.Catalog().Commit || len(snapshot.RuleCatalog.Entries) == 0 {
		t.Fatalf("rule catalog = %#v", snapshot.RuleCatalog)
	}
	if len(snapshot.OutputSubscriptions) != 1 {
		t.Fatalf("subscriptions = %#v", snapshot.OutputSubscriptions)
	}
	output := snapshot.OutputSubscriptions[0]
	if output.ID != subscriptionID || output.URL != "https://sub.example/sub/console-token" || output.Scenario != "desktop" {
		t.Fatalf("output subscription = %#v", output)
	}
	if output.Artifact == nil || output.Artifact.Revision != "rev-1" || output.Update == nil {
		t.Fatalf("artifact/update = %#v / %#v", output.Artifact, output.Update)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) == "" || containsBytes(encoded, []byte("secret generated body")) {
		t.Fatalf("snapshot leaked artifact body: %s", encoded)
	}
}

func TestReaderReadKeepsDanglingSubscriptionVisible(t *testing.T) {
	st := openTestStore(t)
	id, err := st.SaveOutputSubscription(store.OutputSubscription{
		Name: "dangling", TemplateVersionID: 91, RuleProfileID: 92,
		Token: "dangling-token", Enabled: false,
		Bindings: []store.SubscriptionBinding{{Slot: "main", NodeIDs: []int64{93, 93}}},
	})
	if err != nil {
		t.Fatal(err)
	}

	snapshot, err := New(st).Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.OutputSubscriptions) != 1 || snapshot.OutputSubscriptions[0].ID != id {
		t.Fatalf("subscriptions = %#v", snapshot.OutputSubscriptions)
	}
	problems := snapshot.OutputSubscriptions[0].Problems
	if len(problems) != 3 ||
		problems[0].Code != "missing_template_version" ||
		problems[1].Code != "missing_rule_profile" ||
		problems[2].Code != "missing_node" {
		t.Fatalf("problems = %#v", problems)
	}
	if snapshot.OutputSubscriptions[0].Artifact != nil || snapshot.OutputSubscriptions[0].Update != nil {
		t.Fatalf("optional build state should be absent: %#v", snapshot.OutputSubscriptions[0])
	}
}

func TestActiveRuleCatalogRejectsMalformedPersistedSnapshot(t *testing.T) {
	_, err := activeRuleCatalog(store.ConsoleState{
		Settings:       map[string]string{"rule_catalog_active_commit": "custom"},
		RuleCatalogRaw: json.RawMessage(`{"commit":`),
	})
	if err == nil {
		t.Fatal("malformed catalog snapshot was accepted")
	}
}

func TestReaderReadRejectsMalformedJSONSettings(t *testing.T) {
	for _, test := range []struct {
		name  string
		key   string
		value string
	}{
		{"fake ip filter", fakeip.SettingKey, `{"mode":`},
		{"catalog refresh state", "rule_catalog_refresh_state", `{"status":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			st := openTestStore(t)
			if err := st.SetSetting(test.key, test.value); err != nil {
				t.Fatal(err)
			}
			if _, err := New(st).Read(); err == nil {
				t.Fatalf("malformed %s setting was accepted", test.key)
			}
		})
	}
}

func containsBytes(haystack, needle []byte) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if string(haystack[i:i+len(needle)]) == string(needle) {
			return true
		}
	}
	return false
}
