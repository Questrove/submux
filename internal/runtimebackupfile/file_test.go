package runtimebackupfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteNewAndReadPreserveOwnerOnlyBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime-backup.zip")
	body := []byte("bounded backup archive")
	written, err := WriteNew(path, body, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if written != path {
		t.Fatalf("written path = %q, want %q", written, path)
	}
	read, err := Read(path, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(read) != string(body) {
		t.Fatalf("read body = %q", read)
	}
	if _, err := WriteNew(path, []byte("replacement"), 1024); err == nil {
		t.Fatal("backup output unexpectedly overwrote an existing file")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("backup mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestReadAndWriteRejectLinkedPaths(t *testing.T) {
	root := t.TempDir()
	actual := filepath.Join(root, "actual")
	if err := os.Mkdir(actual, 0700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked")
	if err := os.Symlink(actual, linked); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if _, err := WriteNew(filepath.Join(linked, "backup.zip"), []byte("backup"), 1024); err == nil {
		t.Fatal("backup write accepted a linked parent")
	}
	target := filepath.Join(actual, "target.zip")
	if err := os.WriteFile(target, []byte("backup"), 0600); err != nil {
		t.Fatal(err)
	}
	fileLink := filepath.Join(root, "backup.zip")
	if err := os.Symlink(target, fileLink); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(fileLink, 1024); err == nil {
		t.Fatal("backup read accepted a symbolic link")
	}
}
