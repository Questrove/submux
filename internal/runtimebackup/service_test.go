package runtimebackup

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

func TestBackupExportPreviewAndRestore(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	sourceState := openBackupTestState(t, "source")
	source, err := sourceState.CreateSource(runtimestate.SourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "portable",
		URL:                    "https://example.com/config?token=backup-url-secret",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		Username:               "backup-user-secret",
		Password:               "backup-password-secret",
		AuthorizedTarget:       "https://example.com:443",
		RefreshIntervalSeconds: 900,
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
	}, []byte("source-body-secret"), []byte("candidate-body-secret"), "op_source", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sourceState.SetAdvancedOverride([]byte("override-body-secret"), "op_override", now); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceState.CreateManagedResource(
		"secret.pem",
		runtimeapi.ResourceKindPrivateKey,
		[]byte("resource-body-secret"),
		"op_resource",
		now,
	); err != nil {
		t.Fatal(err)
	}
	configRoot := filepath.Join(t.TempDir(), "config")
	writeConfigurationSet(t, filepath.Join(configRoot, "current"), "current-config-secret")
	writeConfigurationSet(t, filepath.Join(configRoot, "previous-good"), "previous-good-config-secret")
	writeConfigurationSet(t, filepath.Join(configRoot, "history", "rev-older"), "history-config-secret")
	if err := os.MkdirAll(filepath.Join(filepath.Dir(configRoot), "logs"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(configRoot), "logs", "runtime.ndjson"), []byte("log-only-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		State:      sourceState,
		Root:       filepath.Join(t.TempDir(), "automatic-backups"),
		ConfigRoot: configRoot,
		Now:        func() time.Time { return now },
	}
	preview, err := service.Preview(true)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Restorable || !preview.IncludeSecrets || len(preview.Excluded) == 0 ||
		!strings.Contains(preview.Warning, "未加密") {
		t.Fatalf("full backup preview = %#v", preview)
	}
	if _, err := service.Export(runtimeapi.BackupExportRequest{IncludeSecrets: true}); err == nil {
		t.Fatal("plaintext backup export without confirmation succeeded")
	}
	full, err := service.Export(runtimeapi.BackupExportRequest{
		IncludeSecrets:   true,
		ConfirmPlaintext: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !full.Restorable || full.Size != int64(len(full.Body)) || len(full.SHA256) != 64 {
		t.Fatalf("full backup = %#v", full)
	}
	assertArchiveExcludes(t, full.Body, "logs", "operations", "events", "audit", "core")
	fullState := archiveStateBody(t, full.Body)
	for _, secret := range []string{
		"backup-url-secret",
		"backup-user-secret",
		"backup-password-secret",
	} {
		if !bytes.Contains(fullState, []byte(secret)) {
			t.Fatalf("full restorable backup omitted selected secret %q", secret)
		}
	}
	parsed, err := parseArchive(full.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(parsed.State.Sources[0].Raw) != "source-body-secret" ||
		string(parsed.State.Sources[0].Candidate) != "candidate-body-secret" ||
		string(parsed.State.AdvancedOverride.Body) != "override-body-secret" ||
		string(parsed.State.ManagedResources[0].Body) != "resource-body-secret" {
		t.Fatal("full restorable backup omitted selected opaque state")
	}
	if bytes.Contains(fullState, []byte("log-only-secret")) {
		t.Fatal("backup included a Runtime log")
	}
	restorePreview, err := service.Inspect(full.Body, "content_backup")
	if err != nil {
		t.Fatal(err)
	}
	if !restorePreview.Restorable || restorePreview.SourceCount != 1 ||
		restorePreview.ManagedResourceCount != 1 || !restorePreview.HasAdvancedOverride ||
		restorePreview.RecentConfigurationCount != 3 || len(restorePreview.PendingSettings) == 0 {
		t.Fatalf("restore preview = %#v", restorePreview)
	}

	targetState := openBackupTestState(t, "target")
	original, err := targetState.CreateSource(runtimestate.SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "original",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("original"), []byte("candidate-original"), "op_original", now)
	if err != nil {
		t.Fatal(err)
	}
	targetConfigRoot := filepath.Join(t.TempDir(), "target-config")
	writeConfigurationSet(t, filepath.Join(targetConfigRoot, "current"), "target-original-config-secret")
	targetService := &Service{
		State:      targetState,
		Root:       filepath.Join(t.TempDir(), "target-backups"),
		ConfigRoot: targetConfigRoot,
		Now:        func() time.Time { return now.Add(time.Minute) },
	}
	result, err := targetService.Restore(t.Context(), full.Body, "op_restore", func(_ context.Context, state runtimestate.PortableState) error {
		current, err := targetState.CurrentSource()
		if err != nil || current.ID != source.ID || state.CurrentSourceID != source.ID {
			return errors.New("replacement was not visible to post-restore validation")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.MachineSettingsPending || result.AutomaticBackupFile == "" || result.BackupSHA256 != full.SHA256 {
		t.Fatalf("restore result = %#v", result)
	}
	if _, err := os.Stat(filepath.Join(targetService.Root, result.AutomaticBackupFile)); err != nil {
		t.Fatalf("automatic pre-restore backup was not saved: %v", err)
	}
	if _, err := targetState.GetSource(original.ID); !errors.Is(err, runtimestate.ErrSourceNotFound) {
		t.Fatalf("whole restore retained original source: %v", err)
	}
	currentConfig, err := os.ReadFile(filepath.Join(targetConfigRoot, "current", "config.yaml"))
	if err != nil || !bytes.Contains(currentConfig, []byte("current-config-secret")) ||
		bytes.Contains(currentConfig, []byte("target-original-config-secret")) {
		t.Fatalf("whole restore current configuration = %q err=%v", currentConfig, err)
	}
	restoredPrevious, err := os.ReadFile(filepath.Join(
		targetConfigRoot,
		"history",
		"restored-previous-good-"+full.SHA256[:12],
		"config.yaml",
	))
	if err != nil || !bytes.Contains(restoredPrevious, []byte("previous-good-config-secret")) {
		t.Fatalf("restored previous-good configuration = %q err=%v", restoredPrevious, err)
	}
	historyConfig, err := os.ReadFile(filepath.Join(targetConfigRoot, "history", "rev-older", "config.yaml"))
	if err != nil || !bytes.Contains(historyConfig, []byte("history-config-secret")) {
		t.Fatalf("restored configuration history = %q err=%v", historyConfig, err)
	}
	snapshot, err := targetState.Observe("test", now)
	if err != nil || !snapshot.Backups.MachineSettingsPending ||
		snapshot.RunMode != runtimeapi.RunModeUnconfigured ||
		snapshot.Mihomo.DesiredState != runtimeapi.MihomoDesiredStopped {
		t.Fatalf("restored snapshot = %#v err=%v", snapshot, err)
	}
}

func TestRedactedBackupDoesNotContainUnselectedSecretsAndCannotRestore(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	state := openBackupTestState(t, "redacted")
	if _, err := state.CreateSource(runtimestate.SourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "secret",
		URL:                    "https://example.com/config?token=unselected-url-secret",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		Password:               "unselected-password-secret",
		AuthorizedTarget:       "https://example.com:443",
		RefreshIntervalSeconds: 900,
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
	}, []byte("unselected-source-secret"), []byte("unselected-candidate-secret"), "op_source", now); err != nil {
		t.Fatal(err)
	}
	service := &Service{
		State: state,
		Root:  filepath.Join(t.TempDir(), "backups"),
		Now:   func() time.Time { return now },
	}
	redacted, err := service.Export(runtimeapi.BackupExportRequest{ConfirmPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}
	if redacted.Restorable || redacted.IncludeSecrets {
		t.Fatalf("redacted backup = %#v", redacted)
	}
	stateBody := archiveStateBody(t, redacted.Body)
	for _, secret := range []string{
		"unselected-url-secret",
		"unselected-password-secret",
		"unselected-source-secret",
		"unselected-candidate-secret",
	} {
		if bytes.Contains(stateBody, []byte(secret)) {
			t.Fatalf("redacted backup exposed %q", secret)
		}
	}
	parsed, err := parseArchive(redacted.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.State.Sources[0].Raw) != 0 || len(parsed.State.Sources[0].Candidate) != 0 {
		t.Fatal("redacted backup retained opaque source content")
	}
	if _, err := service.Restore(t.Context(), redacted.Body, "op_restore", nil); err == nil {
		t.Fatal("redacted backup was accepted for restore")
	}
}

func TestFailedPostRestoreValidationKeepsOriginalPortableState(t *testing.T) {
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	sourceState := openBackupTestState(t, "replacement")
	if _, err := sourceState.CreateSource(runtimestate.SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "replacement",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("replacement"), []byte("candidate-replacement"), "op_replacement", now); err != nil {
		t.Fatal(err)
	}
	sourceConfigRoot := filepath.Join(t.TempDir(), "source-config")
	writeConfigurationSet(t, filepath.Join(sourceConfigRoot, "current"), "replacement-config")
	sourceService := &Service{
		State:      sourceState,
		Root:       filepath.Join(t.TempDir(), "source-backups"),
		ConfigRoot: sourceConfigRoot,
		Now:        func() time.Time { return now },
	}
	archive, err := sourceService.Export(runtimeapi.BackupExportRequest{IncludeSecrets: true, ConfirmPlaintext: true})
	if err != nil {
		t.Fatal(err)
	}

	targetState := openBackupTestState(t, "original")
	original, err := targetState.CreateSource(runtimestate.SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "original",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("original"), []byte("candidate-original"), "op_original", now)
	if err != nil {
		t.Fatal(err)
	}
	targetConfigRoot := filepath.Join(t.TempDir(), "target-config")
	writeConfigurationSet(t, filepath.Join(targetConfigRoot, "current"), "original-config")
	targetService := &Service{
		State:      targetState,
		Root:       filepath.Join(t.TempDir(), "target-backups"),
		ConfigRoot: targetConfigRoot,
		Now:        func() time.Time { return now },
	}
	_, err = targetService.Restore(t.Context(), archive.Body, "op_restore", func(context.Context, runtimestate.PortableState) error {
		return errors.New("injected restored configuration validation failure")
	})
	if err == nil || !strings.Contains(err.Error(), "original portable state was restored") {
		t.Fatalf("failed restore error = %v", err)
	}
	current, err := targetState.CurrentSource()
	if err != nil || current.ID != original.ID {
		t.Fatalf("failed restore changed current source = %#v err=%v", current, err)
	}
	raw, _, err := targetState.ReadSourceRevision(current)
	if err != nil || string(raw) != "original" {
		t.Fatalf("failed restore changed source bytes = %q err=%v", raw, err)
	}
	config, err := os.ReadFile(filepath.Join(targetConfigRoot, "current", "config.yaml"))
	if err != nil || !bytes.Contains(config, []byte("original-config")) ||
		bytes.Contains(config, []byte("replacement-config")) {
		t.Fatalf("failed restore changed configuration = %q err=%v", config, err)
	}
}

func TestBackupParserRejectsTraversalAndUnlistedContentWithoutEchoingNames(t *testing.T) {
	for _, name := range []string{"../escape", "logs/runtime.ndjson", "logs/access-token-super-secret.ndjson"} {
		t.Run(strings.ReplaceAll(name, "/", "_"), func(t *testing.T) {
			var output bytes.Buffer
			writer := zip.NewWriter(&output)
			entry, err := writer.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := entry.Write([]byte("secret")); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := parseArchive(output.Bytes()); err == nil {
				t.Fatalf("unsafe backup entry %q was accepted", name)
			} else if strings.Contains(err.Error(), "access-token-super-secret") {
				t.Fatalf("backup parser error exposed an unselected secret: %v", err)
			}
		})
	}
}

func TestRecentConfigurationManifestRequiresCompleteConfigurationSets(t *testing.T) {
	err := validateRecentConfigurationManifest([]archiveEntry{{
		Name:   "recent-configurations/current/config.yaml",
		Size:   10,
		SHA256: strings.Repeat("a", 64),
	}})
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete recent configuration manifest error = %v", err)
	}
}

func TestRecoverConfigurationReplacementAfterInterruptedSwap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	writeConfigurationSet(t, filepath.Join(root, "current"), "original-config")
	recent := []filePayload{
		{Name: "recent-configurations/current/source.yaml", Body: []byte("proxies: []\n")},
		{Name: "recent-configurations/current/config.yaml", Body: []byte("mixed-port: 7890\n# restored-config\n")},
		{Name: "recent-configurations/current/metadata.json", Body: []byte(`{"revision":"restored"}`)},
	}
	replacement, err := stageConfigurationReplacement(root, recent, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, "current", "config.yaml"))
	if err != nil || !bytes.Contains(config, []byte("restored-config")) {
		t.Fatalf("staged configuration = %q err=%v", config, err)
	}
	if err := RecoverConfigurationReplacement(root); err != nil {
		t.Fatal(err)
	}
	config, err = os.ReadFile(filepath.Join(root, "current", "config.yaml"))
	if err != nil || !bytes.Contains(config, []byte("original-config")) {
		t.Fatalf("recovered original configuration = %q err=%v", config, err)
	}
	replacement.noop = true
}

func TestRecoverConfigurationReplacementKeepsCommittedSwap(t *testing.T) {
	root := filepath.Join(t.TempDir(), "config")
	writeConfigurationSet(t, filepath.Join(root, "current"), "original-config")
	recent := []filePayload{
		{Name: "recent-configurations/current/source.yaml", Body: []byte("proxies: []\n")},
		{Name: "recent-configurations/current/config.yaml", Body: []byte("mixed-port: 7890\n# committed-config\n")},
		{Name: "recent-configurations/current/metadata.json", Body: []byte(`{"revision":"committed"}`)},
	}
	replacement, err := stageConfigurationReplacement(root, recent, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.markCommitted(); err != nil {
		t.Fatal(err)
	}
	if err := RecoverConfigurationReplacement(root); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join(root, "current", "config.yaml"))
	if err != nil || !bytes.Contains(config, []byte("committed-config")) {
		t.Fatalf("committed configuration = %q err=%v", config, err)
	}
	replacement.noop = true
}

func openBackupTestState(t *testing.T, name string) *runtimestate.Store {
	t.Helper()
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = state.Close() })
	return state
}

func writeConfigurationSet(t *testing.T, root, secret string) {
	t.Helper()
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"source.yaml":   "proxies: []\n",
		"config.yaml":   "mixed-port: 7890\n# " + secret + "\n",
		"metadata.json": `{"revision":"rev","source_hash":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","candidate_hash":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","applied_at":"2026-07-30T12:00:00Z"}`,
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func archiveStateBody(t *testing.T, body []byte) []byte {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range reader.File {
		if entry.Name != "state.json" {
			continue
		}
		source, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		result, err := io.ReadAll(source)
		source.Close()
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	t.Fatal("state.json is unavailable")
	return nil
}

func assertArchiveExcludes(t *testing.T, body []byte, forbidden ...string) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range reader.File {
		for _, prefix := range forbidden {
			if strings.Contains(entry.Name, prefix) {
				t.Fatalf("backup unexpectedly includes %q as %q", prefix, entry.Name)
			}
		}
	}
}
