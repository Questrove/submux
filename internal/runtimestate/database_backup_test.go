package runtimestate

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeStateSchemaAndDatabaseBackup(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion()
	if err != nil || version != SchemaVersion {
		t.Fatalf("Runtime state schema = %d err=%v", version, err)
	}
	backupRoot := filepath.Join(t.TempDir(), "rollback")
	if err := os.Mkdir(backupRoot, 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := store.BackupDatabase(filepath.Join(backupRoot, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(backup.Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	if backup.Size != int64(len(body)) || backup.SHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("Runtime database backup = %#v size=%d", backup, len(body))
	}
	if _, err := store.BackupDatabase(backup.Path); err == nil {
		t.Fatal("Runtime database backup overwrote an existing rollback point")
	}
}
