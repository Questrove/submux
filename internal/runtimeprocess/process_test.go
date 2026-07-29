package runtimeprocess

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizedEnvironmentRemovesMihomoPathExpansion(t *testing.T) {
	t.Setenv("SAFE_PATHS", "C:\\attacker")
	t.Setenv("SUBMUX_SHOULD_NOT_LEAK", "secret")
	for _, entry := range sanitizedEnvironment() {
		upper := strings.ToUpper(entry)
		if strings.HasPrefix(upper, "SAFE_PATHS=") || strings.HasPrefix(entry, "SUBMUX_SHOULD_NOT_LEAK=") {
			t.Fatalf("unsafe environment entry retained: %q", entry)
		}
	}
}

func TestMihomoEnvironmentUsesOnlyRuntimeSafePaths(t *testing.T) {
	t.Setenv("SAFE_PATHS", filepath.Join(t.TempDir(), "attacker"))
	managed := filepath.Join(t.TempDir(), "managed-resources")
	if err := os.MkdirAll(managed, 0700); err != nil {
		t.Fatalf("create managed resources: %v", err)
	}
	environment, err := mihomoEnvironment([]string{managed, managed})
	if err != nil {
		t.Fatalf("build Mihomo environment: %v", err)
	}
	var safePaths []string
	for _, entry := range environment {
		if strings.HasPrefix(strings.ToUpper(entry), "SAFE_PATHS=") {
			safePaths = append(safePaths, entry)
		}
	}
	if len(safePaths) != 1 || safePaths[0] != "SAFE_PATHS="+managed {
		t.Fatalf("SAFE_PATHS = %#v, want only %q", safePaths, managed)
	}
}

func TestMihomoEnvironmentRejectsUnsafePaths(t *testing.T) {
	tests := map[string]string{
		"relative":        "managed-resources",
		"filesystem root": filepath.VolumeName(t.TempDir()) + string(filepath.Separator),
		"path list":       filepath.Join(t.TempDir(), "one") + string(os.PathListSeparator) + "two",
	}
	for name, path := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := mihomoEnvironment([]string{path}); err == nil {
				t.Fatalf("accepted unsafe path %q", path)
			}
		})
	}
}

func TestProcessRejectsUnmanagedPaths(t *testing.T) {
	process := &Process{BinaryPath: "mihomo", ConfigPath: "config.yaml", DataDir: "data"}
	if err := process.Start(t.Context()); err == nil {
		t.Fatal("Process accepted relative Runtime-owned paths")
	}
	if running, err := process.IsRunning(t.Context()); err != nil || running {
		t.Fatalf("process running=%v err=%v", running, err)
	}
}

func TestIsRunningRefreshesExitedProcess(t *testing.T) {
	done := make(chan error, 1)
	close(done)
	process := &Process{cmd: &exec.Cmd{}, done: done}

	running, err := process.IsRunning(t.Context())
	if err != nil {
		t.Fatalf("inspect process: %v", err)
	}
	if running || process.cmd != nil || process.done != nil {
		t.Fatalf("exited process remained running: running=%v cmd=%v done=%v", running, process.cmd, process.done)
	}
}
