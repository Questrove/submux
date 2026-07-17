package safepath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestContainsLink(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	linked, err := ContainsLink(linkedDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Fatal("symbolic-link path was not detected")
	}
}

func TestContainsLinkInExistingPathDoesNotCreateThroughLink(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	linked, err := ContainsLinkInExistingPath(filepath.Join(linkedDirectory, "not-created", "managed"))
	if err != nil {
		t.Fatal(err)
	}
	if !linked {
		t.Fatal("symbolic-link ancestor was not detected before directory creation")
	}
	if _, err := os.Stat(filepath.Join(realDirectory, "not-created")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight check created a directory through the link: %v", err)
	}
}
