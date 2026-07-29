package runtimestate

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAdvancedOverrideDefaultsToEmptyMappingAndPersistsImmutableContent(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	body, record, err := store.AdvancedOverride()
	if err != nil {
		t.Fatalf("read default advanced override: %v", err)
	}
	if string(body) != "{}\n" || record.SHA256 != "" {
		t.Fatalf("default advanced override = %q / %#v", body, record)
	}

	now := time.Date(2026, 7, 30, 11, 0, 0, 0, time.UTC)
	expected := []byte("proxy-groups:\n  - name: local\n    type: select\n    proxies: [DIRECT]\n")
	record, err = store.SetAdvancedOverride(expected, "op_override", now)
	if err != nil {
		t.Fatalf("set advanced override: %v", err)
	}
	actual, loaded, err := store.AdvancedOverride()
	if err != nil {
		t.Fatalf("read advanced override: %v", err)
	}
	if string(actual) != string(expected) || loaded != record {
		t.Fatalf("advanced override = %q / %#v, want %#v", actual, loaded, record)
	}
	snapshot, err := store.Observe("dev", now.Add(time.Second))
	if err != nil {
		t.Fatalf("observe Runtime state: %v", err)
	}
	if !snapshot.AdvancedOverride.Present ||
		snapshot.AdvancedOverride.SHA256 != record.SHA256 ||
		snapshot.AdvancedOverride.Size != int64(len(expected)) {
		t.Fatalf("advanced override status = %#v", snapshot.AdvancedOverride)
	}
}

func TestAdvancedOverrideEnforcesLimitAndDetectsTampering(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	if _, err := store.SetAdvancedOverride(nil, "op_empty", time.Now()); err == nil {
		t.Fatal("empty advanced override was accepted")
	}
	if _, err := store.SetAdvancedOverride(
		make([]byte, MaxAdvancedOverrideBytes+1),
		"op_large",
		time.Now(),
	); err == nil {
		t.Fatal("oversized advanced override was accepted")
	}
	record, err := store.SetAdvancedOverride([]byte("{}\n"), "op_valid", time.Now())
	if err != nil {
		t.Fatalf("set advanced override: %v", err)
	}
	path, err := store.advancedOverridePath(record.SHA256)
	if err != nil {
		t.Fatalf("resolve advanced override path: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatalf("tamper advanced override: %v", err)
	}
	if _, _, err := store.AdvancedOverride(); err == nil {
		t.Fatal("tampered advanced override was accepted")
	}
}
