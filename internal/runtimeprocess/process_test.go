package runtimeprocess

import (
	"os/exec"
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
