package store

import (
	"encoding/json"
	"fmt"
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
	if err := st.UpdateNodeMetadata(id, []string{"private"}, false); err != nil {
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
	if len(nodes) != 1 || nodes[0].ID != id || nodes[0].Name != "renamed upstream" || nodes[0].Enabled || len(nodes[0].Tags) != 1 || nodes[0].Role != "notice" || nodes[0].RoleOverride != "notice" {
		t.Fatalf("stable node metadata lost: %#v", nodes)
	}
}

func TestReplaceSourceNodesTreatsUniqueSameNameAsConfigurationChange(t *testing.T) {
	st := newTestStore(t)
	sourceID, _ := st.CreateSource(Source{Name: "airport", URL: "https://example.com/sub"})
	first := NodeRecord{
		SourceID: sourceID, Origin: SourceKindSubscription, Name: "Hong Kong 01", Protocol: "vless",
		Config: json.RawMessage(`{"name":"Hong Kong 01","type":"vless","server":"192.0.2.1","port":443}`), Fingerprint: "old-fingerprint",
	}
	if err := st.ReplaceSourceNodes(sourceID, []NodeRecord{first}); err != nil {
		t.Fatal(err)
	}
	nodes, _ := st.ListNodes()
	id := nodes[0].ID
	if err := st.UpdateNodeMetadata(id, []string{"primary"}, false); err != nil {
		t.Fatal(err)
	}
	subscriptionID, err := st.SaveOutputSubscription(OutputSubscription{
		Name: "desktop", Token: "stable-binding-token", Bindings: []SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{id}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.Config = json.RawMessage(`{"name":"Hong Kong 01","type":"vless","server":"198.51.100.2","port":8443}`)
	second.Fingerprint = "new-fingerprint"
	if err := st.ReplaceSourceNodes(sourceID, []NodeRecord{second}); err != nil {
		t.Fatal(err)
	}
	nodes, _ = st.ListNodes()
	if len(nodes) != 1 || nodes[0].ID != id || nodes[0].Fingerprint != "new-fingerprint" || nodes[0].Enabled || nodes[0].ConfigRevision != 2 || nodes[0].LastChange != "configuration_changed" || len(nodes[0].Tags) != 1 {
		t.Fatalf("same-name configuration update lost identity or metadata: %#v", nodes)
	}
	stored, err := st.GetOutputSubscription(subscriptionID)
	if err != nil || len(stored.Bindings) != 1 || len(stored.Bindings[0].NodeIDs) != 1 || stored.Bindings[0].NodeIDs[0] != id {
		t.Fatalf("subscription binding did not follow the stable node id: %#v, %v", stored, err)
	}
}

func TestReplaceSourceNodesDoesNotGuessBetweenDuplicateNames(t *testing.T) {
	st := newTestStore(t)
	sourceID, _ := st.CreateSource(Source{Name: "airport", URL: "https://example.com/sub"})
	first := []NodeRecord{
		{SourceID: sourceID, Name: "duplicate", Protocol: "vless", Config: json.RawMessage(`{"name":"duplicate","server":"192.0.2.1"}`), Fingerprint: "old-a"},
		{SourceID: sourceID, Name: "duplicate", Protocol: "vless", Config: json.RawMessage(`{"name":"duplicate","server":"192.0.2.2"}`), Fingerprint: "old-b"},
	}
	if err := st.ReplaceSourceNodes(sourceID, first); err != nil {
		t.Fatal(err)
	}
	old, _ := st.ListNodes()
	oldIDs := map[int64]bool{old[0].ID: true, old[1].ID: true}
	updated := []NodeRecord{
		{SourceID: sourceID, Name: "duplicate", Protocol: "vless", Config: json.RawMessage(`{"name":"duplicate","server":"198.51.100.1"}`), Fingerprint: "new-a"},
		{SourceID: sourceID, Name: "duplicate", Protocol: "vless", Config: json.RawMessage(`{"name":"duplicate","server":"198.51.100.2"}`), Fingerprint: "new-b"},
	}
	if err := st.ReplaceSourceNodes(sourceID, updated); err != nil {
		t.Fatal(err)
	}
	nodes, _ := st.ListNodes()
	if len(nodes) != 2 || oldIDs[nodes[0].ID] || oldIDs[nodes[1].ID] {
		t.Fatalf("ambiguous duplicate names were matched heuristically: %#v", nodes)
	}
}

func TestEmptyManagementListsAreJSONArrays(t *testing.T) {
	st := newTestStore(t)
	values := []any{}
	sources, _ := st.ListSources()
	nodes, _ := st.ListNodes()
	templates, _ := st.ListTemplates()
	versions, _ := st.ListTemplateVersions(1)
	subscriptions, _ := st.ListOutputSubscriptions()
	events, _ := st.ListLifecycleEvents(10)
	values = append(values, sources, nodes, templates, versions, subscriptions, events)
	for index, value := range values {
		encoded, err := json.Marshal(value)
		if err != nil || string(encoded) != "[]" {
			t.Fatalf("empty list %d encoded as %s, err=%v", index, encoded, err)
		}
	}
}

func TestOutputSubscriptionTokenIndexChangesAtomically(t *testing.T) {
	st := newTestStore(t)
	subscription := OutputSubscription{Name: "desktop", Token: "first", Enabled: true}
	id, err := st.SaveOutputSubscription(subscription)
	if err != nil {
		t.Fatal(err)
	}
	subscription.ID, subscription.Token = id, "second"
	if _, err := st.SaveOutputSubscription(subscription); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetOutputSubscriptionByToken("first"); err == nil {
		t.Fatal("old token still resolves")
	}
	if got, err := st.GetOutputSubscriptionByToken("second"); err != nil || got.ID != id {
		t.Fatalf("new token not indexed: %#v %v", got, err)
	}
}

func TestOutputSubscriptionPreservesSelectedNodeOrder(t *testing.T) {
	st := newTestStore(t)
	value := OutputSubscription{
		Name: "ordered", Token: "ordered-token", Enabled: true,
		Bindings: []SubscriptionBinding{{Slot: "primary", NodeIDs: []int64{30, 10, 20, 10}}},
	}
	id, err := st.SaveOutputSubscription(value)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.GetOutputSubscription(id)
	if err != nil || len(stored.Bindings) != 1 {
		t.Fatalf("load ordered subscription: value=%+v err=%v", stored, err)
	}
	want := []int64{30, 10, 20}
	if fmt.Sprint(stored.Bindings[0].NodeIDs) != fmt.Sprint(want) {
		t.Fatalf("selected order changed: got %v want %v", stored.Bindings[0].NodeIDs, want)
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
		if got := string(meta.Get([]byte("schema_version"))); got != "8" {
			t.Fatalf("schema version = %q, want 8", got)
		}
		if meta.Get([]byte("override")) != nil || meta.Get([]byte("lastgood:clash")) != nil {
			t.Fatal("legacy global override/artifact keys were preserved")
		}
		return nil
	})
}

func TestOpenMigratesV3OutputStateToV4(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v3.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{
			"settings", "sources", "source_cache", "meta",
			"nodes", "node_sets", "templates", "template_versions",
			"profiles", "profile_artifacts", "token_index", "lifecycle_events",
		} {
			if _, err := tx.CreateBucket([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("3")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("sources")).Put(itob(1), []byte(`{"id":1,"name":"kept-source"}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("nodes")).Put(itob(2), []byte(`{"id":2,"name":"kept-node"}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("templates")).Put(itob(3), []byte(`{"id":3,"name":"kept-template"}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("lifecycle_events")).Put(itob(4), []byte(`{"id":4,"source_id":1}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("node_sets")).Put(itob(5), []byte(`{"id":5}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("profiles")).Put(itob(6), []byte(`{"id":6,"token":"legacy-token"}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("profile_artifacts")).Put(itob(6), []byte(`{"profile_id":6}`)); err != nil {
			return err
		}
		return tx.Bucket([]byte("token_index")).Put([]byte("legacy-token"), itob(6))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.db.View(func(tx *bolt.Tx) error {
		if got := string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))); got != "8" {
			t.Fatalf("schema version = %q, want 8", got)
		}
		for _, name := range []string{"node_sets", "profiles", "profile_artifacts"} {
			if tx.Bucket([]byte(name)) != nil {
				t.Fatalf("legacy output bucket %q still exists", name)
			}
		}
		if key, _ := tx.Bucket([]byte("token_index")).Cursor().First(); key != nil {
			t.Fatalf("legacy token index was not cleared: %q", key)
		}
		for _, item := range []struct {
			bucket string
			key    int64
		}{
			{bucket: "sources", key: 1},
			{bucket: "nodes", key: 2},
			{bucket: "templates", key: 3},
			{bucket: "lifecycle_events", key: 4},
		} {
			if tx.Bucket([]byte(item.bucket)).Get(itob(item.key)) == nil {
				t.Fatalf("preserved bucket %q lost key %d", item.bucket, item.key)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
