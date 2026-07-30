package runtimediag

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

func TestDiagnosticsAreLocalPreviewedAndRedactedByDefault(t *testing.T) {
	root := t.TempDir()
	state, err := runtimestate.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if _, err := state.RecordAudit(runtimeapi.AuditRecord{
		Actor:         "windows:sid:test",
		ClientType:    "cli",
		ClientVersion: "test",
		Action:        "test",
		Stage:         "completed",
		Result:        "failed",
		At:            now,
		Error: &runtimeapi.ProtocolError{
			Code:    "failed",
			Message: `GET https://diag-user:diag-pass@[2001:db8::1]/x?token=submux-secret-alpha&token=submux-secret-beta C:\Users\DiagnosticSecret\config.yaml`,
		},
	}); err != nil {
		t.Fatalf("record Runtime audit: %v", err)
	}
	service := &Service{State: state, StateRoot: filepath.Join(root, "state"), RuntimeVersion: "test", Now: func() time.Time { return now }}
	preview, err := service.Preview(context.Background(), runtimeapi.DiagnosticsRequest{})
	if err != nil {
		t.Fatalf("preview diagnostics: %v", err)
	}
	if preview.Warning != runtimeapi.SensitiveDataWarning || len(preview.Items) < 5 {
		t.Fatalf("diagnostics preview=%#v", preview)
	}
	result, err := service.Create(context.Background(), runtimeapi.DiagnosticsRequest{})
	if err != nil {
		t.Fatalf("create diagnostics: %v", err)
	}
	path := filepath.Join(root, "state", "diagnostics", result.FileName)
	info, err := os.Stat(path)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		t.Fatalf("diagnostics permissions=%v err=%v", info, err)
	}
	archive, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open diagnostics archive: %v", err)
	}
	defer archive.Close()
	var combined strings.Builder
	for _, entry := range archive.File {
		reader, err := entry.Open()
		if err != nil {
			t.Fatalf("open diagnostics entry: %v", err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read diagnostics entry: %v", err)
		}
		_ = reader.Close()
		combined.Write(body)
	}
	for _, secret := range []string{
		"diag-user:diag-pass",
		"submux-secret-alpha",
		"submux-secret-beta",
		`C:\Users\DiagnosticSecret`,
	} {
		if strings.Contains(combined.String(), secret) {
			t.Fatalf("default diagnostics leaked %q: %s", secret, combined.String())
		}
	}
}

func TestSensitiveDiagnosticsRequireConfirmationAndAreOptIn(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	state, err := runtimestate.Open(stateRoot)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	configRoot := filepath.Join(stateRoot, "config", "current")
	if err := os.MkdirAll(configRoot, 0700); err != nil {
		t.Fatalf("create config root: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configRoot, "config.yaml"), []byte("secret: raw-secret\n"), 0600); err != nil {
		t.Fatalf("write current config: %v", err)
	}
	service := &Service{State: state, StateRoot: stateRoot, RuntimeVersion: "test"}
	request := runtimeapi.DiagnosticsRequest{IncludeRawConfig: true}
	if _, err := service.Create(context.Background(), request); !errors.Is(err, ErrSensitiveConfirmationRequired) {
		t.Fatalf("unconfirmed sensitive diagnostics error=%v", err)
	}
	request.ConfirmSensitive = true
	preview, err := service.Preview(context.Background(), request)
	if err != nil {
		t.Fatalf("preview sensitive diagnostics: %v", err)
	}
	found := false
	for _, item := range preview.Items {
		if item.Name == "sensitive/current-config.yaml" && item.Sensitive {
			found = true
		}
	}
	if !found {
		t.Fatalf("sensitive config missing from preview=%#v", preview)
	}
}
