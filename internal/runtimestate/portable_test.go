package runtimestate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

func TestPortableStateRoundTripReplacesOnlyPortableState(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	sourceStore := openPortableTestStore(t, "source")
	source, err := sourceStore.CreateSource(SourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "portable",
		URL:                    "https://backup.example/config.yaml?token=portable-secret",
		RedactedTarget:         "https://backup.example:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		Username:               "portable-user",
		Password:               "portable-password",
		AuthorizedTarget:       "https://backup.example:443",
		RefreshIntervalSeconds: 900,
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
	}, []byte("proxies:\n- password: source-secret\n"), []byte("mixed-port: 7890\n"), "op_source", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStore.CreateManagedResource(
		"portable-key.pem",
		runtimeapi.ResourceKindPrivateKey,
		[]byte("managed-resource-secret"),
		"op_resource",
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceStore.SetAdvancedOverride(
		[]byte("authentication: override-secret\n"),
		"op_override",
		now,
	); err != nil {
		t.Fatal(err)
	}
	state, err := sourceStore.ExportPortableState(true, now)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Complete || state.CurrentSourceID != source.ID {
		t.Fatalf("portable state = %#v", state)
	}

	targetStore := openPortableTestStore(t, "target")
	original, err := targetStore.CreateSource(SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "original",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("original"), []byte("candidate-original"), "op_original", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := targetStore.db.Update(func(tx *bbolt.Tx) error {
		metadata := tx.Bucket(metadataBucket)
		if err := metadata.Put(runModeKey, []byte(runtimeapi.RunModeGateway)); err != nil {
			return err
		}
		return metadata.Put(mihomoDesiredStateKey, []byte(runtimeapi.MihomoDesiredRunning))
	}); err != nil {
		t.Fatal(err)
	}
	if err := targetStore.ReplacePortableState(state, "op_restore", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := targetStore.GetSource(original.ID); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("original source remained after whole restore: %v", err)
	}
	restored, err := targetStore.CurrentSource()
	if err != nil || restored.ID != source.ID || restored.Password != "portable-password" {
		t.Fatalf("restored source = %#v err=%v", restored, err)
	}
	raw, candidate, err := targetStore.ReadSourceRevision(restored)
	if err != nil || !strings.Contains(string(raw), "source-secret") || string(candidate) != "mixed-port: 7890\n" {
		t.Fatalf("restored source bytes raw=%q candidate=%q err=%v", raw, candidate, err)
	}
	override, _, err := targetStore.AdvancedOverride()
	if err != nil || !strings.Contains(string(override), "override-secret") {
		t.Fatalf("restored override = %q err=%v", override, err)
	}
	resources, err := targetStore.ManagedResources()
	if err != nil || len(resources) != 1 {
		t.Fatalf("restored resources = %#v err=%v", resources, err)
	}
	resourceBody, err := os.ReadFile(resources[0].Path)
	if err != nil || string(resourceBody) != "managed-resource-secret" {
		t.Fatalf("restored resource body = %q err=%v", resourceBody, err)
	}
	snapshot, err := targetStore.Observe("test", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.RunMode != runtimeapi.RunModeUnconfigured ||
		snapshot.Mihomo.DesiredState != runtimeapi.MihomoDesiredStopped ||
		!snapshot.Backups.MachineSettingsPending {
		t.Fatalf("restored machine state = %#v", snapshot)
	}
}

func TestPortableStateRedactedExportOmitsUnselectedSecrets(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	store := openPortableTestStore(t, "redacted")
	if _, err := store.CreateSource(SourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "secret",
		URL:                    "https://example.com/config?token=url-secret",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		Username:               "username-secret",
		Password:               "password-secret",
		AuthorizedTarget:       "https://example.com:443",
		CustomCAPEM:            "custom-ca-secret",
		RefreshIntervalSeconds: 900,
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
	}, []byte("source-body-secret"), []byte("candidate-body-secret"), "op_source", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateManagedResource(
		"secret.pem",
		runtimeapi.ResourceKindPrivateKey,
		[]byte("resource-body-secret"),
		"op_resource",
		now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAdvancedOverride([]byte("override-body-secret"), "op_override", now); err != nil {
		t.Fatal(err)
	}
	redacted, err := store.ExportPortableState(false, now)
	if err != nil {
		t.Fatal(err)
	}
	if redacted.Complete {
		t.Fatal("redacted portable state was marked complete")
	}
	body, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"url-secret",
		"username-secret",
		"password-secret",
		"custom-ca-secret",
		"source-body-secret",
		"candidate-body-secret",
		"resource-body-secret",
		"override-body-secret",
	} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("redacted portable state exposed %q: %s", secret, body)
		}
	}
	if err := store.ReplacePortableState(redacted, "op_restore", now); err == nil {
		t.Fatal("redacted portable state was accepted for restore")
	}
}

func TestInvalidPortableReplacementKeepsOriginalState(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	store := openPortableTestStore(t, "invalid")
	original, err := store.CreateSource(SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "original",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("original"), []byte("candidate-original"), "op_original", now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.ExportPortableState(true, now)
	if err != nil {
		t.Fatal(err)
	}
	state.Sources[0].Raw = []byte("tampered")
	if err := store.ReplacePortableState(state, "op_restore", now.Add(time.Minute)); err == nil {
		t.Fatal("tampered portable state was accepted")
	}
	current, err := store.CurrentSource()
	if err != nil || current.ID != original.ID {
		t.Fatalf("invalid restore changed current source = %#v err=%v", current, err)
	}
	raw, _, err := store.ReadSourceRevision(current)
	if err != nil || string(raw) != "original" {
		t.Fatalf("invalid restore changed source bytes = %q err=%v", raw, err)
	}
}

func openPortableTestStore(t *testing.T, name string) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
