package runtimestate

import (
	"path/filepath"
	"testing"
	"time"
)

func TestProxyDelayResultsAreIsolatedBySourceAndSurviveReopen(t *testing.T) {
	const sourceA = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const sourceB = "src_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 9, 30, 0, 0, time.UTC)
	createProxySelectionSource(t, store, sourceA, "source-a", now)
	createProxySelectionSource(t, store, sourceB, "source-b", now)
	if err := store.SetProxyDelayResult(sourceA, "PROXY", "Tokyo", 42, "", "op-a", now); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProxyDelayResult(sourceB, "PROXY", "Tokyo", 0, "测试超时", "op-b", now.Add(time.Minute)); err != nil {
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

	first, err := store.ProxyDelayResults(sourceA)
	if err != nil || len(first) != 1 || first[0].DelayMillis != 42 || first[0].Failure != "" || !first[0].TestedAt.Equal(now) {
		t.Fatalf("source_a delays=%#v err=%v", first, err)
	}
	second, err := store.ProxyDelayResults(sourceB)
	if err != nil || len(second) != 1 || second[0].Failure != "测试超时" || !second[0].TestedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("source_b delays=%#v err=%v", second, err)
	}
}

func TestDeletingSourceRemovesProxyDelayResults(t *testing.T) {
	const sourceID = "src_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	createProxySelectionSource(t, store, sourceID, "source-a", now)
	if err := store.SetProxyDelayResult(sourceID, "PROXY", "Tokyo", 42, "", "op-a", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeleteSource(sourceID, true, "op-delete", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if results, err := store.ProxyDelayResults(sourceID); err == nil || len(results) != 0 {
		t.Fatalf("deleted source delays=%#v err=%v", results, err)
	}
}
