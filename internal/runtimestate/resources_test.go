package runtimestate

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestManagedResourcesAreContentAddressedAndVisibleInSnapshot(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	body := []byte("proxies:\n  - name: local\n    type: socks5\n    server: 127.0.0.1\n    port: 1080\n")
	first, err := store.CreateManagedResource(
		"provider.main",
		runtimeapi.ResourceKindProxyProvider,
		body,
		"op_first",
		now,
	)
	if err != nil {
		t.Fatalf("create first managed resource: %v", err)
	}
	second, err := store.CreateManagedResource(
		"provider.backup",
		runtimeapi.ResourceKindProxyProvider,
		body,
		"op_second",
		now.Add(time.Second),
	)
	if err != nil {
		t.Fatalf("create second managed resource: %v", err)
	}
	if first.ID == second.ID || first.SHA256 != second.SHA256 {
		t.Fatalf("content addressing = %#v / %#v", first, second)
	}

	resources, err := store.ManagedResources()
	if err != nil {
		t.Fatalf("read managed resources: %v", err)
	}
	if len(resources) != 2 || resources[0].Path != resources[1].Path {
		t.Fatalf("managed resources = %#v", resources)
	}
	snapshot, err := store.Observe("dev", now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("observe Runtime state: %v", err)
	}
	if snapshot.Resources.Count != 2 || snapshot.Resources.TotalBytes != int64(2*len(body)) {
		t.Fatalf("resource status = %#v", snapshot.Resources)
	}
}

func TestManagedResourceRootIsFixedInsideState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	resourceRoot, err := store.ManagedResourceRoot()
	if err != nil {
		t.Fatalf("resolve managed resource root: %v", err)
	}
	want := filepath.Join(root, "managed-resources")
	if resourceRoot != want || !filepath.IsAbs(resourceRoot) {
		t.Fatalf("managed resource root = %q, want absolute %q", resourceRoot, want)
	}
}

func TestManagedResourceRejectsUnsafeMetadataAndEnforcesSizeLimitsBeforeWriting(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	for _, name := range []string{"../provider", ".hidden", "provider/name", "提供者"} {
		if _, err := store.CreateManagedResource(
			name,
			runtimeapi.ResourceKindProxyProvider,
			[]byte("proxies: []\n"),
			"op_unsafe",
			now,
		); err == nil {
			t.Fatalf("unsafe resource name %q was accepted", name)
		}
	}
	if _, err := store.CreateManagedResource(
		"provider",
		"arbitrary-file",
		[]byte("value"),
		"op_type",
		now,
	); err == nil {
		t.Fatal("unknown resource kind was accepted")
	}
	if _, err := store.CreateManagedResource(
		"provider",
		runtimeapi.ResourceKindProxyProvider,
		make([]byte, MaxManagedResourceBytes+1),
		"op_large",
		now,
	); err == nil {
		t.Fatal("oversized managed resource was accepted")
	}

	full := bytes.Repeat([]byte("x"), MaxManagedResourceBytes)
	for index := 0; index < MaxManagedResourcesBytes/MaxManagedResourceBytes; index++ {
		if _, err := store.CreateManagedResource(
			"provider-"+string(rune('a'+index)),
			runtimeapi.ResourceKindProxyProvider,
			full,
			"op_fill",
			now.Add(time.Duration(index)*time.Second),
		); err != nil {
			t.Fatalf("fill managed resource total at %d: %v", index, err)
		}
	}
	rejected := []byte("over-total-limit")
	if _, err := store.CreateManagedResource(
		"provider-overflow",
		runtimeapi.ResourceKindProxyProvider,
		rejected,
		"op_overflow",
		now.Add(time.Hour),
	); err == nil || !strings.Contains(err.Error(), "total size limit") {
		t.Fatalf("total size limit error = %v", err)
	}
	path, err := store.managedResourceObjectPath(sha256Hex(rejected))
	if err != nil {
		t.Fatalf("resolve rejected resource path: %v", err)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("rejected resource left an object at %s: %v", path, err)
	}
}

func TestManagedResourceDetectsContentTampering(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	record, err := store.CreateManagedResource(
		"provider",
		runtimeapi.ResourceKindProxyProvider,
		[]byte("proxies: []\n"),
		"op_resource",
		time.Now(),
	)
	if err != nil {
		t.Fatalf("create managed resource: %v", err)
	}
	path, err := store.managedResourceObjectPath(record.SHA256)
	if err != nil {
		t.Fatalf("resolve managed resource path: %v", err)
	}
	if err := os.WriteFile(path, []byte("tampered"), 0600); err != nil {
		t.Fatalf("tamper managed resource: %v", err)
	}
	if _, err := store.ManagedResources(); err == nil {
		t.Fatal("tampered managed resource was accepted")
	}
}
