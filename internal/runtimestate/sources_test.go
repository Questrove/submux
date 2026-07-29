package runtimestate

import (
	"encoding/json"
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
