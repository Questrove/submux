package runtimelog

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStoreRedactsAndBoundsLogsByAgeAndSize(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "logs"))
	if err != nil {
		t.Fatalf("open Runtime logs: %v", err)
	}
	store.Now = func() time.Time { return now }
	store.MaxAge = time.Hour
	store.MaxFileBytes = 180
	store.MaxBytes = 320
	writer, err := store.Writer("mihomo", "stderr")
	if err != nil {
		t.Fatalf("create Mihomo log writer: %v", err)
	}
	for index := 0; index < 8; index++ {
		if _, err := writer.Write([]byte("GET https://user:pass@[2001:db8::1]/config?token=one&token=two Authorization: Bearer value\n")); err != nil {
			t.Fatalf("write Mihomo log: %v", err)
		}
	}
	entries, err := os.ReadDir(store.Root)
	if err != nil {
		t.Fatalf("read Runtime logs: %v", err)
	}
	total := int64(0)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatalf("stat Runtime log: %v", err)
		}
		total += info.Size()
		body, err := os.ReadFile(filepath.Join(store.Root, entry.Name()))
		if err != nil {
			t.Fatalf("read Runtime log: %v", err)
		}
		for _, secret := range []string{"user:pass", "one", "two", "Bearer value"} {
			if strings.Contains(string(body), secret) {
				t.Fatalf("Runtime log leaked %q: %s", secret, body)
			}
		}
	}
	if total > store.MaxBytes {
		t.Fatalf("Runtime logs exceed size limit: %d", total)
	}

	old := filepath.Join(store.Root, "runtime-old.log")
	if err := os.WriteFile(old, []byte("old"), 0600); err != nil {
		t.Fatalf("write old Runtime log: %v", err)
	}
	oldTime := now.Add(-2 * time.Hour)
	if err := os.Chtimes(old, oldTime, oldTime); err != nil {
		t.Fatalf("age Runtime log: %v", err)
	}
	if err := store.GC(); err != nil {
		t.Fatalf("clean Runtime logs: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old Runtime log still exists: %v", err)
	}
}

func TestRunGCEnforcesAgeWithoutNewWrites(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime log store: %v", err)
	}
	now := time.Now().UTC()
	store.Now = func() time.Time { return now }
	store.MaxAge = time.Hour
	writer, err := store.Writer("runtime", "service")
	if err != nil {
		t.Fatalf("create Runtime log writer: %v", err)
	}
	if _, err := writer.Write([]byte("old record\n")); err != nil {
		t.Fatalf("write Runtime log: %v", err)
	}
	logPath := filepath.Join(root, "runtime.log")
	oldTime := now.Add(-2 * time.Hour)
	if err := os.Chtimes(logPath, oldTime, oldTime); err != nil {
		t.Fatalf("age Runtime log: %v", err)
	}
	now = now.Add(2 * time.Hour)

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- store.RunGC(ctx, time.Millisecond)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(logPath); os.IsNotExist(err) {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("periodic Runtime log cleanup did not remove an expired log")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Runtime log cleanup: %v", err)
	}
}
