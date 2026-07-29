package store

import (
	"encoding/json"
	"strings"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func TestReadConsoleStateReturnsOneSortedTypedState(t *testing.T) {
	st := newTestStore(t)
	catalogRaw := json.RawMessage(`{"commit":"catalog-1","entries":[{"id":"rule-1"}]}`)

	err := st.db.Update(func(tx *bolt.Tx) error {
		settings := tx.Bucket([]byte("settings"))
		for key, value := range map[string]string{
			"base_url":                   "https://sub.example",
			"fetch_interval_sec":         "60",
			"rule_catalog_active_commit": "catalog-1",
			"admin_pw_hash":              "must-not-leave-store",
		} {
			if err := settings.Put([]byte(key), []byte(value)); err != nil {
				return err
			}
		}
		records := []struct {
			bucket string
			key    int64
			value  any
		}{
			{"sources", 2, Source{ID: 2, Name: "second", SortOrder: 10}},
			{"sources", 1, Source{ID: 1, Name: "first", SortOrder: 0}},
			{"source_cache", 1, Cache{SourceID: 1, LastSuccessAt: "2026-07-29T01:00:00Z"}},
			{"nodes", 2, NodeRecord{ID: 2, Name: "node-2"}},
			{"nodes", 1, NodeRecord{ID: 1, Name: "node-1"}},
			{"lifecycle_events", 1, LifecycleEvent{ID: 1, SourceID: 1}},
			{"templates", 1, Template{ID: 1, Name: "template"}},
			{"template_versions", 1, TemplateVersion{ID: 1, TemplateID: 1, Version: 1}},
			{"rule_profiles", 1, RuleProfile{ID: 1, Name: "rules"}},
			{"subscriptions", 1, OutputSubscription{ID: 1, Name: "output"}},
			{"subscription_artifacts", 1, SubscriptionArtifact{SubscriptionID: 1, Revision: "rev-1"}},
			{"subscription_updates", 1, SubscriptionUpdateState{SubscriptionID: 1, Status: SubscriptionUpdateReady}},
		}
		for _, record := range records {
			if err := putJSON(tx.Bucket([]byte(record.bucket)), itob(record.key), record.value); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte("rule_catalog_snapshots")).Put([]byte("catalog-1"), catalogRaw)
	})
	if err != nil {
		t.Fatal(err)
	}

	state, err := st.ReadConsoleState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sources) != 2 || state.Sources[0].ID != 1 || state.Sources[1].ID != 2 {
		t.Fatalf("sources are not stably sorted: %#v", state.Sources)
	}
	if len(state.Nodes) != 2 || state.Nodes[0].ID != 1 || state.Nodes[1].ID != 2 {
		t.Fatalf("nodes are not stably sorted: %#v", state.Nodes)
	}
	if got := state.Caches[1].LastSuccessAt; got != "2026-07-29T01:00:00Z" {
		t.Fatalf("cache last success = %q", got)
	}
	if got := state.SubscriptionArtifacts[1].Revision; got != "rev-1" {
		t.Fatalf("artifact revision = %q", got)
	}
	if got := state.SubscriptionUpdates[1].Status; got != SubscriptionUpdateReady {
		t.Fatalf("update status = %q", got)
	}
	if got := string(state.RuleCatalogRaw); got != string(catalogRaw) {
		t.Fatalf("catalog raw = %s", got)
	}
	if _, exists := state.Settings["admin_pw_hash"]; exists {
		t.Fatal("authentication setting escaped the console state boundary")
	}
}

func TestReadConsoleStateRejectsMalformedStoredJSON(t *testing.T) {
	st := newTestStore(t)
	if err := st.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("nodes")).Put(itob(1), []byte(`{"id":`))
	}); err != nil {
		t.Fatal(err)
	}

	_, err := st.ReadConsoleState()
	if err == nil || !strings.Contains(err.Error(), "decode console nodes record") {
		t.Fatalf("error = %v", err)
	}
}
