package runtimestate

import (
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestProxySelectionsAreIsolatedBySourceAndSurviveReopen(t *testing.T) {
	const sourceA = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const sourceB = "src_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	createProxySelectionSource(t, store, sourceA, "source-a", now)
	createProxySelectionSource(t, store, sourceB, "source-b", now)
	if err := store.SetProxySelection(sourceA, "PROXY", "Tokyo", "op-a", now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProxySelection(sourceB, "PROXY", "Osaka", "op-b", now); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	first, err := store.ProxySelections(sourceA)
	if err != nil || len(first) != 1 || first[0].Node != "Tokyo" {
		t.Fatalf("source_a selections=%#v err=%v", first, err)
	}
	second, err := store.ProxySelections(sourceB)
	if err != nil || len(second) != 1 || second[0].Node != "Osaka" {
		t.Fatalf("source_b selections=%#v err=%v", second, err)
	}
}

func TestProxySelectionRejectsUnsafeNamesWithoutChangingExistingValue(t *testing.T) {
	const sourceID = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	createProxySelectionSource(t, store, sourceID, "source-a", now)
	if err := store.SetProxySelection(sourceID, "PROXY", "Tokyo", "op-a", now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProxySelection(sourceID, "PROXY", "bad\nnode", "op-b", now); err == nil {
		t.Fatal("unsafe node accepted")
	}
	selections, err := store.ProxySelections(sourceID)
	if err != nil || len(selections) != 1 || selections[0].Node != "Tokyo" {
		t.Fatalf("selection changed=%#v err=%v", selections, err)
	}
}

func createProxySelectionSource(t *testing.T, store *Store, id, name string, now time.Time) {
	t.Helper()
	_, err := store.CreateSource(SourceRecord{
		ID:                id,
		Name:              name,
		Type:              runtimeapi.SourceTypeLocalImport,
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("source"), []byte("proxies: []\nproxy-groups: []\n"), "op-create", now)
	if err != nil {
		t.Fatal(err)
	}
}
