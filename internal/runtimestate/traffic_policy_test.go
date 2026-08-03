package runtimestate

import (
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestTrafficPolicyDefaultsToSourceAndClearsPersistentOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TrafficPolicy.Selection != runtimeapi.TrafficPolicyFollowSource || snapshot.TrafficPolicy.FieldOrigin != runtimeapi.TrafficPolicyOriginSource {
		t.Fatalf("default traffic policy=%#v", snapshot.TrafficPolicy)
	}
	if err := store.SetTrafficPolicy(runtimeapi.TrafficPolicyRule, "op-rule", now); err != nil {
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
	snapshot, err = store.Observe("test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TrafficPolicy.Selection != runtimeapi.TrafficPolicyRule || snapshot.TrafficPolicy.FieldOrigin != runtimeapi.TrafficPolicyOriginRuntime {
		t.Fatalf("persisted traffic policy=%#v", snapshot.TrafficPolicy)
	}
	if err := store.SetTrafficPolicy(runtimeapi.TrafficPolicyFollowSource, "op-follow", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Observe("test", now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TrafficPolicy.Selection != runtimeapi.TrafficPolicyFollowSource || snapshot.TrafficPolicy.FieldOrigin != runtimeapi.TrafficPolicyOriginSource {
		t.Fatalf("cleared traffic policy=%#v", snapshot.TrafficPolicy)
	}
}

func TestPortableStatePreservesTrafficPolicy(t *testing.T) {
	source, err := Open(filepath.Join(t.TempDir(), "source"))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if err := source.SetTrafficPolicy(runtimeapi.TrafficPolicyGlobal, "op-global", now); err != nil {
		t.Fatal(err)
	}
	portable, err := source.ExportPortableState(true, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if portable.TrafficPolicy != runtimeapi.TrafficPolicyGlobal {
		t.Fatalf("portable traffic policy=%q", portable.TrafficPolicy)
	}

	target, err := Open(filepath.Join(t.TempDir(), "target"))
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if err := target.ReplacePortableState(portable, "op-restore", now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	snapshot, err := target.Observe("test", now.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.TrafficPolicy.Selection != runtimeapi.TrafficPolicyGlobal || snapshot.TrafficPolicy.FieldOrigin != runtimeapi.TrafficPolicyOriginRuntime {
		t.Fatalf("restored traffic policy=%#v", snapshot.TrafficPolicy)
	}
}
