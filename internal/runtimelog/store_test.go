package runtimelog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
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

func TestQueryReturnsLatestPageFiltersAndCursorCatchUp(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 3, 1, 0, 0, 0, time.UTC)
	store.Now = func() time.Time {
		now = now.Add(time.Millisecond)
		return now
	}
	runtimeWriter, _ := store.Writer(runtimeapi.LogComponentRuntime, "service")
	mihomoWriter, _ := store.Writer(runtimeapi.LogComponentMihomo, "stderr")
	networkWriter, _ := store.Writer(runtimeapi.LogComponentNetwork, "privileged-ipc")
	for index := 0; index < 205; index++ {
		_, _ = runtimeWriter.Write([]byte("INFO runtime healthy\n"))
	}
	_, _ = mihomoWriter.Write([]byte("ERROR dial failed\n"))
	_, _ = networkWriter.Write([]byte("WARN privileged reconnect\n"))

	page, err := store.Query(runtimeapi.LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != runtimeapi.LogPageDefaultSize || !page.HasOlder || page.EarliestCursor == 0 || page.LatestCursor == 0 {
		t.Fatalf("default page=%#v", page)
	}
	for index := 1; index < len(page.Items); index++ {
		if page.Items[index-1].Cursor >= page.Items[index].Cursor {
			t.Fatalf("log page is not ascending: %#v", page.Items[index-1:index+1])
		}
	}

	filtered, err := store.Query(runtimeapi.LogQuery{
		Component: runtimeapi.LogComponentNetwork,
		Level:     runtimeapi.LogLevelWarn,
		Text:      "reconnect",
	})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].Component != runtimeapi.LogComponentNetwork {
		t.Fatalf("filtered page=%#v err=%v", filtered, err)
	}

	older, err := store.Query(runtimeapi.LogQuery{Before: page.EarliestCursor, Limit: 20})
	if err != nil || len(older.Items) != 7 || older.HasOlder {
		t.Fatalf("older page=%#v err=%v", older, err)
	}
	_, _ = runtimeWriter.Write([]byte("DEBUG bounded catch-up\n"))
	catchUp, err := store.Query(runtimeapi.LogQuery{After: page.LatestCursor, Limit: 20})
	if err != nil || len(catchUp.Items) != 1 || catchUp.Items[0].Level != runtimeapi.LogLevelDebug {
		t.Fatalf("catch-up page=%#v err=%v", catchUp, err)
	}
}

func TestQueryNeverReturnsFullURLsPathsOrConfigurationBodies(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "logs"))
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := store.Writer(runtimeapi.LogComponentMihomo, "stderr")
	_, _ = writer.Write([]byte("GET https://user:pass@example.com/private/config.yaml?token=secret C:\\private\\config.yaml\n"))
	_, _ = writer.Write([]byte("proxies: [{name: secret-node, password: secret-pass}]\n"))
	page, err := store.Query(runtimeapi.LogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"example.com/private/config.yaml", "user:pass", "secret", `C:\\private\\config.yaml`, "secret-node"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("log query exposed %q: %s", secret, encoded)
		}
	}
}

func TestCorruptLogOnlyBreaksQueriesAndDoesNotBlockRuntimeLogging(t *testing.T) {
	root := filepath.Join(t.TempDir(), "logs")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runtime.log"), []byte("not-json\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatalf("corrupt retained log blocked Runtime startup: %v", err)
	}
	if _, err := store.Query(runtimeapi.LogQuery{}); err == nil {
		t.Fatal("corrupt retained log did not fail the isolated log query")
	}
	writer, err := store.Writer(runtimeapi.LogComponentRuntime, "service")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("INFO Runtime continues\n")); err != nil {
		t.Fatalf("corrupt retained log blocked new Runtime logging: %v", err)
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
