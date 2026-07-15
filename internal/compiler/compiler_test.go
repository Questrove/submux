package compiler

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/node"
	"submux/internal/store"
)

func compilerTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "compiler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func addManualNodes(t *testing.T, st *store.Store, sourceName, content string) (int64, []int64) {
	t.Helper()
	sourceID, err := st.CreateSource(store.Source{Kind: store.SourceKindManual, Name: sourceName})
	if err != nil {
		t.Fatal(err)
	}
	records, err := node.Import(sourceID, store.SourceKindManual, content)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := st.CreateManualNodes(records)
	if err != nil {
		t.Fatal(err)
	}
	return sourceID, ids
}

func addTemplate(t *testing.T, st *store.Store, engine, content string, slots []store.TemplateSlot) store.TemplateVersion {
	t.Helper()
	id, err := st.SaveTemplate(store.Template{Name: "test", Engine: engine, Scenario: "test", Status: "draft"})
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.PublishTemplateVersion(id, "test", content, slots)
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestMihomoCompilerInjectsOnlyBoundSlotsAndHashesArtifact(t *testing.T) {
	st := compilerTestStore(t)
	sourceID, _ := addManualNodes(t, st, "Home", "vless://id@example.com:443?encryption=none&type=tcp#Node")
	nodeSetID, err := st.SaveNodeSet(store.NodeSet{Name: "all", SourceIDs: []int64{sourceID}, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	version := addTemplate(t, st, EngineMihomo, `proxy-groups:
  - {name: A, type: select, proxies: [DIRECT]}
  - {name: B, type: select, proxies: [DIRECT]}
rules:
  - MATCH,A
`, []store.TemplateSlot{
		{Key: "a", Target: "A", Mode: "append"},
		{Key: "b", Target: "B", Mode: "append"},
	})
	service := New(st)
	a, err := service.Preview(store.Profile{Engine: EngineMihomo, TemplateVersionID: version.ID, Bindings: []store.ProfileBinding{{Slot: "a", NodeSetID: nodeSetID}}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := service.Preview(store.Profile{Engine: EngineMihomo, TemplateVersionID: version.ID, Bindings: []store.ProfileBinding{{Slot: "b", NodeSetID: nodeSetID}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(a.Body), "[Home] Node") || a.SlotCounts["a"] != 1 {
		t.Fatalf("node or slot count missing:\n%s", a.Body)
	}
	if a.Revision == b.Revision || bytes.Equal(a.Body, b.Body) {
		t.Fatalf("slot placement must affect artifact and revision")
	}
}

func TestSingBoxCompilerMapsDocumentedVLESSFields(t *testing.T) {
	st := compilerTestStore(t)
	sourceID, _ := addManualNodes(t, st, "Airport", "vless://id@example.com:443?encryption=none&security=reality&type=grpc&serviceName=svc&sni=front.example.com&pbk=public&sid=01&fp=chrome#Reality")
	nodeSetID, _ := st.SaveNodeSet(store.NodeSet{Name: "all", SourceIDs: []int64{sourceID}, Enabled: true})
	version := addTemplate(t, st, EngineSingBox, `{
  "outbounds": [
    {"type":"selector","tag":"PROXY","outbounds":["AUTO"]},
    {"type":"urltest","tag":"AUTO","outbounds":[]}
  ],
  "route":{"final":"PROXY"}
}`, []store.TemplateSlot{{Key: "primary", Target: "AUTO", Mode: "replace", Required: true}})
	result, err := New(st).Preview(store.Profile{Engine: EngineSingBox, TemplateVersionID: version.ID, Bindings: []store.ProfileBinding{{Slot: "primary", NodeSetID: nodeSetID}}})
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(result.Body, &root); err != nil {
		t.Fatal(err)
	}
	text := string(result.Body)
	for _, required := range []string{`"type": "vless"`, `"type": "grpc"`, `"service_name": "svc"`, `"public_key": "public"`, `"fingerprint": "chrome"`} {
		if !strings.Contains(text, required) {
			t.Fatalf("missing %s in compiled config:\n%s", required, text)
		}
	}
}

func TestSingBoxCompilerRejectsCertificateFingerprintSemanticMismatch(t *testing.T) {
	st := compilerTestStore(t)
	sourceID, _ := addManualNodes(t, st, "Airport", "hysteria2://password@example.com:443?pinSHA256=certificate-hash#HY2")
	nodeSetID, _ := st.SaveNodeSet(store.NodeSet{Name: "hy2", SourceIDs: []int64{sourceID}, Enabled: true})
	version := addTemplate(t, st, EngineSingBox, `{
  "outbounds": [{"type":"urltest","tag":"AUTO","outbounds":[]}],
  "route":{"final":"AUTO"}
}`, []store.TemplateSlot{{Key: "primary", Target: "AUTO", Required: true}})
	_, err := New(st).Preview(store.Profile{Engine: EngineSingBox, TemplateVersionID: version.ID, Bindings: []store.ProfileBinding{{Slot: "primary", NodeSetID: nodeSetID}}})
	if err == nil || !strings.Contains(err.Error(), "fingerprint") {
		t.Fatalf("expected strict fingerprint rejection, got %v", err)
	}
}

func TestCompileFailurePreservesLastGoodArtifact(t *testing.T) {
	st := compilerTestStore(t)
	sourceID, _ := addManualNodes(t, st, "Home", "trojan://password@example.com:443#TR")
	nodeSet := store.NodeSet{Name: "all", SourceIDs: []int64{sourceID}, Enabled: true}
	nodeSetID, _ := st.SaveNodeSet(nodeSet)
	version := addTemplate(t, st, EngineMihomo, `proxy-groups:
  - {name: AUTO, type: select, proxies: []}
rules: ["MATCH,AUTO"]
`, []store.TemplateSlot{{Key: "primary", Target: "AUTO", Mode: "replace", Required: true}})
	profileID, err := st.SaveProfile(store.Profile{
		Name: "p", Engine: EngineMihomo, TemplateVersionID: version.ID,
		Bindings: []store.ProfileBinding{{Slot: "primary", NodeSetID: nodeSetID}}, Token: "token", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	service := New(st)
	if _, err := service.CompileAndStore(profileID); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetProfileArtifact(profileID)
	nodeSet.ID, nodeSet.Enabled = nodeSetID, false
	if _, err := st.SaveNodeSet(nodeSet); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompileAndStore(profileID); err == nil {
		t.Fatal("expected disabled node set compile failure")
	}
	after, _ := st.GetProfileArtifact(profileID)
	if !bytes.Equal(before.Body, after.Body) || after.LastError == "" || after.Revision != before.Revision {
		t.Fatalf("last-good artifact was not preserved: before=%+v after=%+v", before, after)
	}
}

func TestStrictExpiredSourceBlocksWithoutReplacingLastGood(t *testing.T) {
	st := compilerTestStore(t)
	sourceID, err := st.CreateSource(store.Source{Name: "airport", URL: "https://example.com/sub"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := node.Import(sourceID, store.SourceKindSubscription, "trojan://password@example.com:443#TR")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReplaceSourceNodes(sourceID, records); err != nil {
		t.Fatal(err)
	}
	nodeSetID, _ := st.SaveNodeSet(store.NodeSet{Name: "all", SourceIDs: []int64{sourceID}, Enabled: true})
	version := addTemplate(t, st, EngineMihomo, `proxy-groups:
  - {name: AUTO, type: select, proxies: []}
rules: ["MATCH,AUTO"]
`, []store.TemplateSlot{{Key: "primary", Target: "AUTO", Mode: "replace", Required: true}})
	profileID, _ := st.SaveProfile(store.Profile{
		Name: "p", Engine: EngineMihomo, TemplateVersionID: version.ID,
		Bindings: []store.ProfileBinding{{Slot: "primary", NodeSetID: nodeSetID}}, Token: "token", Enabled: true,
	})
	service := New(st)
	if _, err := service.CompileAndStore(profileID); err != nil {
		t.Fatal(err)
	}
	before, _ := st.GetProfileArtifact(profileID)
	source, _ := st.GetSource(sourceID)
	source.LifecyclePolicy = store.LifecycleStrict
	if err := st.UpdateSource(source); err != nil {
		t.Fatal(err)
	}
	metadata := store.SubscriptionMetadata{
		ExpiresAt:  time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		Provenance: map[string]string{"expires_at": "header"},
	}
	if err := st.CommitSourceRefreshV3(sourceID, records, "", metadata, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompileAndStore(profileID); err == nil {
		t.Fatal("strict expired source did not block")
	}
	after, _ := st.GetProfileArtifact(profileID)
	if after.BlockedReason == "" || !bytes.Equal(before.Body, after.Body) || after.Revision != before.Revision {
		t.Fatalf("strict block did not preserve auditable last-good: before=%+v after=%+v", before, after)
	}
	backupSourceID, _ := addManualNodes(t, st, "backup", "trojan://password@backup.example.com:443#Backup")
	nodeSet, _ := st.GetNodeSet(nodeSetID)
	nodeSet.SourceIDs = []int64{sourceID, backupSourceID}
	if _, err := st.SaveNodeSet(nodeSet); err != nil {
		t.Fatal(err)
	}
	failover, err := service.CompileAndStore(profileID)
	if err != nil || !bytes.Contains(failover.Body, []byte("backup.example.com")) {
		t.Fatalf("strict failover did not publish backup-only artifact: err=%v body=%s", err, failover.Body)
	}
	if bytes.Contains(failover.Body, []byte("server: example.com\n")) {
		t.Fatalf("expired source remained in strict failover artifact: %s", failover.Body)
	}
	metadata.ExpiresAt = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	if err := st.CommitSourceRefreshV3(sourceID, records, "", metadata, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CompileAndStore(profileID); err != nil {
		t.Fatalf("renewed source did not recover: %v", err)
	}
	recovered, _ := st.GetProfileArtifact(profileID)
	if recovered.BlockedReason != "" || recovered.LastError != "" {
		t.Fatalf("recovered artifact still blocked: %+v", recovered)
	}
}
