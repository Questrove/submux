//go:build darwin

package safepath

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestContainsLinkAllowsDarwinVarSystemAlias(t *testing.T) {
	root := t.TempDir()
	if root != "/var" && !strings.HasPrefix(root, "/var/") {
		t.Skipf("temporary directory does not use the macOS /var system alias: %s", root)
	}

	linked, err := ContainsLink(root)
	if err != nil {
		t.Fatal(err)
	}
	if linked {
		t.Fatalf("macOS /var system alias was treated as a user-controlled link: %s", root)
	}

	linked, err = ContainsLinkInExistingPath(filepath.Join(root, "not-created", "managed"))
	if err != nil {
		t.Fatal(err)
	}
	if linked {
		t.Fatal("macOS /var system alias prevented safe child creation")
	}
}
