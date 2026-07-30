package runtimenet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenIntegrityKeyRejectsLinkedKeyFile(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(t.TempDir(), "outside.key")
	if err := os.WriteFile(target, make([]byte, 32), 0600); err != nil {
		t.Fatalf("write external key: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, integrityKeyFile)); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if _, err := openIntegrityKey(root); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("linked integrity key error=%v", err)
	}
}

func TestReadStateRejectsLinkedStateFile(t *testing.T) {
	root := t.TempDir()
	key, err := openIntegrityKey(root)
	if err != nil {
		t.Fatalf("open integrity key: %v", err)
	}
	target := filepath.Join(t.TempDir(), "outside.json")
	if err := os.WriteFile(target, []byte(`{"state":{},"mac":""}`), 0600); err != nil {
		t.Fatalf("write external state: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(root, ownershipFile)); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if _, err := readState(root, key); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("linked state error=%v", err)
	}
}

func TestReadStateRejectsNonRegularStateFile(t *testing.T) {
	root := t.TempDir()
	key, err := openIntegrityKey(root)
	if err != nil {
		t.Fatalf("open integrity key: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, ownershipFile), 0700); err != nil {
		t.Fatalf("create state directory: %v", err)
	}
	if _, err := readState(root, key); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("non-regular state error=%v", err)
	}
}
