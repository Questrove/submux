package runtimestate

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestStorePersistsInstallationAndInitialSnapshot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	first, err := Open(root)
	if err != nil {
		t.Fatalf("open first Runtime state: %v", err)
	}
	installationID, err := first.InstallationID()
	if err != nil {
		t.Fatalf("read installation ID: %v", err)
	}
	if len(installationID) != 32 {
		t.Fatalf("installation ID length = %d, want 32", len(installationID))
	}
	observedAt := time.Date(2026, 7, 30, 12, 0, 0, 0, time.FixedZone("test", 8*60*60))
	snapshot, err := first.Observe("v1.2.3", observedAt)
	if err != nil {
		t.Fatalf("observe Runtime state: %v", err)
	}
	if snapshot.Revision != 1 || snapshot.LatestEventCursor != 0 {
		t.Fatalf("initial snapshot revision/cursor = %d/%d, want 1/0", snapshot.Revision, snapshot.LatestEventCursor)
	}
	if snapshot.Runtime.Version != "v1.2.3" || snapshot.Runtime.ServiceState != "running" {
		t.Fatalf("Runtime status = %#v", snapshot.Runtime)
	}
	if snapshot.Mihomo.State != "not_installed" || snapshot.RunMode != "unconfigured" {
		t.Fatalf("initial Mihomo/run mode = %#v/%q", snapshot.Mihomo, snapshot.RunMode)
	}
	if !snapshot.ObservedAt.Equal(observedAt.UTC()) {
		t.Fatalf("observed_at = %s, want %s", snapshot.ObservedAt, observedAt.UTC())
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first Runtime state: %v", err)
	}

	second, err := Open(root)
	if err != nil {
		t.Fatalf("reopen Runtime state: %v", err)
	}
	defer second.Close()
	reopenedID, err := second.InstallationID()
	if err != nil {
		t.Fatalf("read reopened installation ID: %v", err)
	}
	if reopenedID != installationID {
		t.Fatalf("installation ID changed: %q != %q", reopenedID, installationID)
	}
}

func TestSnapshotDoesNotExposeStatePathsOrSecrets(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state-with-secret-marker")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	snapshot, err := store.Observe("dev", time.Now())
	if err != nil {
		t.Fatalf("observe Runtime state: %v", err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encode snapshot: %v", err)
	}
	for _, forbidden := range []string{
		root,
		"state-with-secret-marker",
		"source_url",
		"config_body",
		"mihomo_secret",
		"privileged_process",
		"installation_id",
	} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("ordinary snapshot exposed forbidden value %q: %s", forbidden, encoded)
		}
	}
}

func TestOpenRejectsRelativeStateRoot(t *testing.T) {
	if store, err := Open("runtime-state"); err == nil {
		_ = store.Close()
		t.Fatal("Open accepted a relative Runtime state root")
	}
}

func TestOpenInitializesAuditCursorInExistingState(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close Runtime state: %v", err)
	}

	db, err := bbolt.Open(filepath.Join(root, "runtime.db"), 0600, nil)
	if err != nil {
		t.Fatalf("open raw Runtime database: %v", err)
	}
	if err := db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata.Get(eventCursorKey) == nil {
			return errors.New("existing Runtime state has no event cursor")
		}
		return metadata.Delete(auditCursorKey)
	}); err != nil {
		_ = db.Close()
		t.Fatalf("remove audit cursor: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw Runtime database: %v", err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen Runtime state: %v", err)
	}
	defer reopened.Close()
	auditCursorExists := false
	if err := reopened.db.View(func(transaction *bbolt.Tx) error {
		auditCursorExists = transaction.Bucket(metadataBucket).Get(auditCursorKey) != nil
		return nil
	}); err != nil {
		t.Fatalf("read audit cursor: %v", err)
	}
	if !auditCursorExists {
		t.Fatal("audit cursor was not initialized")
	}
}
