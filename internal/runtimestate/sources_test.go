package runtimestate

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestRemoteSourceSnapshotIsRedactedAndRevisionIsImmutable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	next := now.Add(6 * time.Hour)
	record, err := store.CreateRemoteSource(RemoteSourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "primary",
		URL:                    "https://user.example/config.yaml?token=very-secret-token",
		RedactedTarget:         "https://user.example:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		Username:               "operator",
		Password:               "very-secret-password",
		AuthorizedTarget:       "https://user.example:443",
		AllowPrivate:           true,
		CustomCAPEM:            "very-secret-custom-ca",
		SkipTLSVerify:          true,
		RefreshIntervalSeconds: int64((6 * time.Hour) / time.Second),
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
		LastRefreshResult:      "validated",
		LastRefreshRoute:       runtimeapi.SourceRouteDirect,
		LastRefreshAt:          timePointer(now),
		NextRefreshAt:          &next,
	}, []byte("proxies: []\n"), []byte("mixed-port: 7890\n"), "op_add", now)
	if err != nil {
		t.Fatalf("create remote source: %v", err)
	}
	if !validSourceID(record.ID) {
		t.Fatalf("source ID = %q", record.ID)
	}

	raw, candidate, err := store.ReadRemoteSourceRevision(record)
	if err != nil {
		t.Fatalf("read source revision: %v", err)
	}
	if string(raw) != "proxies: []\n" || string(candidate) != "mixed-port: 7890\n" {
		t.Fatalf("stored revision = %q / %q", raw, candidate)
	}

	snapshot, err := store.Observe("dev", now)
	if err != nil {
		t.Fatalf("observe source snapshot: %v", err)
	}
	if snapshot.Sources.Count != 1 || snapshot.Sources.CurrentSourceID != record.ID {
		t.Fatalf("source status = %#v", snapshot.Sources)
	}
	item := snapshot.Sources.Items[0]
	if item.RedactedTarget != "https://user.example:443/…" ||
		!item.HasValidatedCandidate ||
		strings.Join(item.HighRiskSettings, ",") != "allow_private,custom_ca,skip_tls_verify" {
		t.Fatalf("source summary = %#v", item)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("encode source snapshot: %v", err)
	}
	for _, secret := range []string{
		"very-secret-token",
		"very-secret-password",
		"very-secret-custom-ca",
		"config.yaml",
	} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("snapshot exposed %q: %s", secret, encoded)
		}
	}
}

func TestRemoteSourceRefreshStateKeepsLastValidatedRevisionOnFailureAndNotModified(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	next := now.Add(time.Minute)
	record, err := store.CreateRemoteSource(RemoteSourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "primary",
		URL:                    "https://example.com/config.yaml",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: int64((6 * time.Hour) / time.Second),
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
		LastRefreshResult:      "validated",
		LastRefreshAt:          timePointer(now),
		NextRefreshAt:          &next,
	}, []byte("raw-v1"), []byte("candidate-v1"), "op_add", now)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	originalRevision := record.RevisionKey

	retryAt := now.Add(2 * time.Minute)
	failed, err := store.RecordRemoteSourceFailure(record.ID, SourceRefreshFailure{
		Result:        "network_error",
		Route:         runtimeapi.SourceRouteDirect,
		FailureClass:  "temporary",
		NextRefreshAt: &retryAt,
		Manual:        true,
	}, "op_refresh_failed", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("record failed refresh: %v", err)
	}
	if failed.RevisionKey != originalRevision || failed.AttemptCount != 1 ||
		failed.LastManualRefreshAt == nil || !failed.NextRefreshAt.Equal(retryAt) {
		t.Fatalf("failed source state = %#v", failed)
	}

	normalNext := now.Add(7 * time.Hour)
	notModified, err := store.CommitRemoteSourceRefresh(record.ID, SourceRefreshSuccess{
		ETag:          `"v1"`,
		LastModified:  "Wed, 30 Jul 2026 08:00:00 GMT",
		Result:        "not_modified",
		Route:         runtimeapi.SourceRouteDirect,
		NextRefreshAt: &normalNext,
		NotModified:   true,
	}, "op_not_modified", now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("commit not-modified refresh: %v", err)
	}
	if notModified.RevisionKey != originalRevision ||
		notModified.AttemptCount != 0 ||
		notModified.FailureClass != "" ||
		notModified.ETag != `"v1"` {
		t.Fatalf("not-modified source state = %#v", notModified)
	}
}

func TestDueCurrentRemoteSourceAndActiveRefreshDetection(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	dueAt := now.Add(-time.Second)
	record, err := store.CreateRemoteSource(RemoteSourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "primary",
		URL:                    "https://example.com/config.yaml",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: 900,
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
		LastRefreshResult:      "validated",
		NextRefreshAt:          &dueAt,
	}, []byte("raw"), []byte("candidate"), "op_add", now)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	due, found, err := store.DueCurrentRemoteSource(now)
	if err != nil || !found || due.ID != record.ID {
		t.Fatalf("due source = %#v, found=%v, err=%v", due, found, err)
	}

	snapshot, err := store.Observe("dev", now)
	if err != nil {
		t.Fatalf("observe before refresh submit: %v", err)
	}
	_, _, err = store.SubmitOperation(peer, "dev", runtimeapi.CreateOperationRequest{
		RequestID:  "refresh-request",
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{
				SourceID: record.ID,
			},
		},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit source refresh: %v", err)
	}
	active, err := store.HasActiveSourceRefresh(record.ID)
	if err != nil || !active {
		t.Fatalf("active refresh = %v, err=%v", active, err)
	}
}

func TestSourceTypesCoexistAndCurrentSourceSwitchIsCompareAndSwap(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	remote, err := store.CreateSource(SourceRecord{
		Type:                   runtimeapi.SourceTypeSubmuxOutput,
		Name:                   "submux output",
		URL:                    "https://submux.example/output.yaml",
		RedactedTarget:         "https://submux.example:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: 900,
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
	}, []byte("remote"), []byte("candidate-remote"), "op_remote", now)
	if err != nil {
		t.Fatalf("create submux output source: %v", err)
	}
	local, err := store.CreateSource(SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "offline copy",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("local"), []byte("candidate-local"), "op_local", now.Add(time.Second))
	if err != nil {
		t.Fatalf("create local source: %v", err)
	}
	current, err := store.CurrentSource()
	if err != nil || current.ID != remote.ID {
		t.Fatalf("initial current source = %#v err=%v", current, err)
	}

	switched, err := store.SwitchCurrentSource(remote.ID, local.ID, "op_switch", now.Add(2*time.Second))
	if err != nil || switched.ID != local.ID {
		t.Fatalf("switch current source = %#v err=%v", switched, err)
	}
	if _, err := store.SwitchCurrentSource(remote.ID, remote.ID, "op_stale", now.Add(3*time.Second)); !errors.Is(err, ErrSourceChanged) {
		t.Fatalf("stale source switch error = %v", err)
	}
	snapshot, err := store.Observe("dev", now.Add(4*time.Second))
	if err != nil {
		t.Fatalf("observe switched sources: %v", err)
	}
	if snapshot.Sources.Count != 2 || snapshot.Sources.CurrentSourceID != local.ID {
		t.Fatalf("source status = %#v", snapshot.Sources)
	}
	for _, item := range snapshot.Sources.Items {
		if item.Current != (item.ID == local.ID) {
			t.Fatalf("source current marker = %#v", item)
		}
	}
}

func TestDeleteSourceProtectsCurrentAndSourceDataDirectoriesAreIsolated(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 30, 9, 0, 0, 0, time.UTC)
	first, err := store.CreateSource(SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "first",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("first"), []byte("candidate-first"), "op_first", now)
	if err != nil {
		t.Fatalf("create first source: %v", err)
	}
	second, err := store.CreateSource(SourceRecord{
		Type:              runtimeapi.SourceTypeLocalImport,
		Name:              "second",
		RedactedTarget:    "Runtime-managed local copy",
		LastRefreshResult: "imported",
	}, []byte("second"), []byte("candidate-second"), "op_second", now.Add(time.Second))
	if err != nil {
		t.Fatalf("create second source: %v", err)
	}
	firstData, err := store.SourceRuntimeDataDir(first.ID)
	if err != nil {
		t.Fatalf("create first source data directory: %v", err)
	}
	secondData, err := store.SourceRuntimeDataDir(second.ID)
	if err != nil {
		t.Fatalf("create second source data directory: %v", err)
	}
	if firstData == secondData || filepath.Dir(firstData) != filepath.Dir(secondData) {
		t.Fatalf("source data directories = %q / %q", firstData, secondData)
	}
	defaultData, err := store.RuntimeDataDir()
	if err != nil {
		t.Fatalf("create default Runtime data directory: %v", err)
	}
	if filepath.Dir(filepath.Dir(firstData)) != defaultData ||
		strings.Contains(defaultData, first.ID) ||
		strings.Contains(defaultData, second.ID) {
		t.Fatalf("default/source data directories = %q / %q / %q", defaultData, firstData, secondData)
	}
	if _, err := store.DeleteSource(first.ID, false, "op_protected", now.Add(2*time.Second)); !errors.Is(err, ErrCurrentSource) {
		t.Fatalf("delete protected current source error = %v", err)
	}
	if _, err := store.DeleteSource(second.ID, false, "op_delete_second", now.Add(3*time.Second)); err != nil {
		t.Fatalf("delete non-current source: %v", err)
	}
	if _, err := store.DeleteSource(first.ID, true, "op_delete_current", now.Add(4*time.Second)); err != nil {
		t.Fatalf("delete confirmed current source: %v", err)
	}
	if _, err := store.CurrentSource(); !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("current source after deletion error = %v", err)
	}
}
