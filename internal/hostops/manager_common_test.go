package hostops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReportsExactVersion(t *testing.T) {
	for _, test := range []struct {
		output  string
		version string
		want    bool
	}{
		{"Mihomo Meta v1.19.14 linux amd64", "v1.19.14", true},
		{"Mihomo Meta v1.19.140 linux amd64", "v1.19.14", false},
		{"Mihomo Meta release-v1.19.14-custom", "v1.19.14", false},
		{"", "v1.19.14", false},
	} {
		if got := reportsExactVersion(test.output, test.version); got != test.want {
			t.Fatalf("reportsExactVersion(%q, %q) = %v, want %v", test.output, test.version, got, test.want)
		}
	}
}

func TestEnsureManagedDirectoryRejectsSymlinkPath(t *testing.T) {
	root := t.TempDir()
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if err := ensureManagedDirectory(filepath.Join(link, "managed"), 0700); err == nil {
		t.Fatal("managed directory accepted a symbolic-link ancestor")
	}
}

func TestRecoverCoreSwapCompletesInterruptedRollback(t *testing.T) {
	root := t.TempDir()
	writeTestCoreDirectory(t, filepath.Join(root, "previous"), "v1.0.0")
	writeTestCoreDirectory(t, filepath.Join(root, "failed"), "v1.1.0")
	if err := recoverCoreSwap(root); err != nil {
		t.Fatal(err)
	}
	current, err := readCoreMetadata(filepath.Join(root, "current", "metadata.json"))
	if err != nil || current.Version != "v1.0.0" {
		t.Fatalf("recovered current=%#v err=%v", current, err)
	}
	previous, err := readCoreMetadata(filepath.Join(root, "previous", "metadata.json"))
	if err != nil || previous.Version != "v1.1.0" {
		t.Fatalf("recovered previous=%#v err=%v", previous, err)
	}
}

func TestCoreOperationMarkerIsStrictAndAtomic(t *testing.T) {
	root := t.TempDir()
	want := coreOperation{Kind: "install", TargetVersion: "v1.2.3", WasRunning: true}
	if err := beginCoreOperation(root, want); err != nil {
		t.Fatal(err)
	}
	got, exists, err := readCoreOperation(root)
	if err != nil || !exists || got != want {
		t.Fatalf("marker=%#v exists=%v err=%v", got, exists, err)
	}
	if err := finishCoreOperation(root); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := readCoreOperation(root); err != nil || exists {
		t.Fatalf("finished marker exists=%v err=%v", exists, err)
	}
	if err := os.WriteFile(filepath.Join(root, "core-operation.json"), []byte(`{"kind":"install","target_version":"v1.2.3","unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readCoreOperation(root); err == nil {
		t.Fatal("operation marker accepted an unknown field")
	}
}

func writeTestCoreDirectory(t *testing.T, path, version string) {
	t.Helper()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(coreMetadata{Version: version, Digest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"})
	if err := os.WriteFile(filepath.Join(path, "metadata.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
