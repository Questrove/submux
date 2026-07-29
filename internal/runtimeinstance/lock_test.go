package runtimeinstance

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireRejectsSecondRuntimeInstance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire first Runtime lock: %v", err)
	}
	defer first.Close()

	second, err := Acquire(path)
	if !errors.Is(err, ErrAlreadyRunning) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second Runtime lock error = %v, want %v", err, ErrAlreadyRunning)
	}
}

func TestAcquireCanResumeAfterRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.lock")
	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire first Runtime lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("release first Runtime lock: %v", err)
	}
	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire Runtime lock after release: %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("release second Runtime lock: %v", err)
	}
}

func TestAcquireRejectsRelativeLockPath(t *testing.T) {
	if guard, err := Acquire("runtime.lock"); err == nil {
		_ = guard.Close()
		t.Fatal("Acquire accepted a relative lock path")
	}
}
