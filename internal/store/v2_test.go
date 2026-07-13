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
	second := first
	second.Name = "renamed upstream"
	second.Config = json.RawMessage(`{"name":"renamed upstream","type":"vless"}`)
	if err := st.ReplaceSourceNodes(sourceID, []NodeRecord{second, second}); err != nil {
		t.Fatal(err)
	}
	nodes, _ = st.ListNodes()
	if len(nodes) != 1 || nodes[0].ID != id || nodes[0].Name != "renamed upstream" || nodes[0].Alias != "my alias" || nodes[0].Enabled || len(nodes[0].Tags) != 1 {
		t.Fatalf("stable node metadata lost: %#v", nodes)
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
	if source.Kind != SourceKindSubscription {
		t.Fatalf("legacy source kind not migrated: %#v", source)
	}
	if token, _ := st.GetSetting("output_token"); token != "" {
		t.Fatalf("legacy token preserved: %q", token)
	}
	_ = st.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket([]byte("meta"))
		if meta.Get([]byte("override")) != nil || meta.Get([]byte("lastgood:clash")) != nil {
			t.Fatal("legacy global override/artifact keys were preserved")
		}
		return nil
	})
}
