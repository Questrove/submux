package store

import (
	"encoding/json"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestReplaceSourceNodesPreservesIdentityAndMetadata(t *testing.T) {
	st := newTestStore(t)
	sourceID, _ := st.CreateSource(Source{Name: "airport", URL: "https://example.com/sub"})
	first := NodeRecord{
		SourceID: sourceID, Origin: SourceKindSubscription, Name: "old name", Protocol: "vless",
		Config: json.RawMessage(`{"name":"old name","type":"vless"}`), Fingerprint: "same-fingerprint",
	}
	if err := st.ReplaceSourceNodes(sourceID, []NodeRecord{first}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := st.ListNodes()
	id := nodes[0].ID
	if err := st.UpdateNodeMetadata(id, "my alias", []string{"private"}, false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeRoleOverride(id, "notice"); err != nil {
		t.Fatal(err)
	}
	second := first
	second.Name = "renamed upstream"
	second.Config = json.RawMessage(`{"name":"renamed upstream","type":"vless"}`)
	if err := st.ReplaceSourceNodes(sourceID, []NodeRecord{second, second}); err != nil {
		t.Fatal(err)
	}
	nodes, _ = st.ListNodes()
	if len(nodes) != 1 || nodes[0].ID != id || nodes[0].Name != "renamed upstream" || nodes[0].Alias != "my alias" || nodes[0].Enabled || len(nodes[0].Tags) != 1 || nodes[0].Role != "notice" || nodes[0].RoleOverride != "notice" {
		t.Fatalf("stable node metadata lost: %#v", nodes)
	}
}

func TestEmptyManagementListsAreJSONArrays(t *testing.T) {
	st := newTestStore(t)
	values := []any{}
	sources, _ := st.ListSources()
	nodes, _ := st.ListNodes()
	nodeSets, _ := st.ListNodeSets()
	templates, _ := st.ListTemplates()
	versions, _ := st.ListTemplateVersions(1)
	profiles, _ := st.ListProfiles()
	events, _ := st.ListLifecycleEvents(10)
	values = append(values, sources, nodes, nodeSets, templates, versions, profiles, events)
	for index, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != "[]" {
			t.Fatalf("empty list %d encoded as %s, err=%v", index, encoded, err)
		}
	}
}

func TestProfileTokenIndexChangesAtomically(t *testing.T) {
	st := newTestStore(t)
	profile := Profile{Name: "desktop", Token: "first", Enabled: true}
	id, err := st.SaveProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	profile.ID, profile.Token = id, "second"
	if _, err := st.SaveProfile(profile); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetProfileByToken("first"); err == nil {
		t.Fatal("old token still resolves")
	}
	if got, err := st.GetProfileByToken("second"); err != nil || got.ID != id {
		t.Fatalf("new token not indexed: %#v %v", got, err)
	}
}

func TestOpenMigratesLegacySourceAndDeletesLegacyOutputState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"settings", "sources", "source_cache", "meta"} {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		source, _ := json.Marshal(Source{ID: 1, Name: "legacy", URL: "https://example.com"})
		if err := tx.Bucket([]byte("sources")).Put(itob(1), source); err != nil {
			return err
		}
		cache, _ := json.Marshal(map[string]any{
			"SourceID": int64(1), "UserinfoJSON": `{"Upload":10,"Download":20,"Total":100,"Expire":1785456000}`,
			"LastSuccessAt": "2026-01-01T00:00:00Z", "UpdatedAt": "2026-01-01T00:00:00Z",
		})
		if err := tx.Bucket([]byte("source_cache")).Put(itob(1), cache); err != nil {
			return err
		}
		emptyCache, _ := json.Marshal(map[string]any{
			"SourceID": int64(2), "UserinfoJSON": `{}`,
			"LastSuccessAt": "2026-01-01T00:00:00Z", "UpdatedAt": "2026-01-01T00:00:00Z",
		})
		if err := tx.Bucket([]byte("source_cache")).Put(itob(2), emptyCache); err != nil {
			return err
		}
		_ = tx.Bucket([]byte("settings")).Put([]byte("output_token"), []byte("old"))
		_ = tx.Bucket([]byte("meta")).Put([]byte("override"), []byte("old override"))
		return tx.Bucket([]byte("meta")).Put([]byte("lastgood:clash"), []byte("old config"))
	}); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	source, _ := st.GetSource(1)
	if source.Kind != SourceKindSubscription || source.LifecyclePolicy != LifecycleContinuity || source.WarnBeforeDays != 7 {
		t.Fatalf("legacy source defaults not migrated: %#v", source)
	}
	if token, _ := st.GetSetting("output_token"); token != "" {
		t.Fatalf("legacy token preserved: %q", token)
	}
	cache, _ := st.GetCache(1)
	if cache.Metadata.Remaining != 70 || cache.Metadata.ExpiresAt == "" || cache.Metadata.Provenance["remaining"] != "header" {
		t.Fatalf("legacy userinfo not migrated: %+v", cache.Metadata)
	}
	emptyCache, _ := st.GetCache(2)
	if len(emptyCache.Metadata.Provenance) != 0 {
		t.Fatalf("empty legacy userinfo invented metadata: %+v", emptyCache.Metadata)
	}
	_ = st.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if got := string(meta.Get([]byte("schema_version"))); got != "3" {
			t.Fatalf("schema version = %q, want 3", got)
		}
		if meta.Get([]byte("override")) != nil || meta.Get([]byte("lastgood:clash")) != nil {
			t.Fatal("legacy global override/artifact keys were preserved")
		}
		return nil
	})
}
