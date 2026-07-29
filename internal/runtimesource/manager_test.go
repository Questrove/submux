package runtimesource

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

type fakeSourceFetcher struct {
	results []FetchResult
	errors  []error
	configs []SourceConfig
}

func (f *fakeSourceFetcher) Fetch(
	_ context.Context,
	config SourceConfig,
	_ ConditionalRequest,
) (FetchResult, error) {
	f.configs = append(f.configs, config)
	index := len(f.configs) - 1
	if index < len(f.errors) && f.errors[index] != nil {
		return FetchResult{}, f.errors[index]
	}
	if index >= len(f.results) {
		return FetchResult{}, errors.New("unexpected fetch")
	}
	return f.results[index], nil
}

type fakeCandidateValidator struct {
	err error
}

func (v fakeCandidateValidator) ValidateSourceCandidate(
	_ context.Context,
	body []byte,
) (ValidatedCandidate, error) {
	if v.err != nil {
		return ValidatedCandidate{}, v.err
	}
	candidate := append([]byte("runtime-owned: true\n"), body...)
	digest := sha256.Sum256(candidate)
	return ValidatedCandidate{
		YAML:   candidate,
		SHA256: hex.EncodeToString(digest[:]),
	}, nil
}

func TestManagerAddsOnlyDownloadedAndValidatedRemoteSource(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	content := uploadSourceDraft(t, state, peer, runtimeapi.RemoteSourceDraft{
		Name:     "primary",
		URL:      "https://example.com/config.yaml?token=secret",
		Route:    runtimeapi.SourceRouteDirect,
		Username: "operator",
		Password: "password",
	}, now)
	raw := []byte("proxies: []\n")
	rawDigest := sha256.Sum256(raw)
	fetcher := &fakeSourceFetcher{results: []FetchResult{{
		Body:         raw,
		SHA256:       hex.EncodeToString(rawDigest[:]),
		ETag:         `"v1"`,
		LastModified: "Wed, 30 Jul 2025 08:00:00 GMT",
		Route:        runtimeapi.SourceRouteDirect,
	}}}
	manager := &Manager{
		State:     state,
		Fetcher:   fetcher,
		Validator: fakeCandidateValidator{},
		Now:       func() time.Time { return now },
		Random:    func() float64 { return 0.5 },
	}
	result, err := manager.Execute(context.Background(), runtimeapi.Operation{
		ID:             "op_add",
		CallerIdentity: peer.Key(),
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionAddRemoteSource,
			Params: runtimeapi.ActionParams{
				ContentID: content.ID,
			},
		},
	}, successfulReporter)
	if err != nil {
		t.Fatalf("add remote source: %v", err)
	}
	if result.SourceID == "" || result.CandidateSHA256 == "" ||
		result.RefreshResult != "validated" ||
		result.NextRefreshAt == nil ||
		!result.NextRefreshAt.Equal(now.Add(DefaultRefreshInterval)) {
		t.Fatalf("add result = %#v", result)
	}
	snapshot, err := state.Observe("dev", now)
	if err != nil {
		t.Fatalf("observe sources: %v", err)
	}
	if snapshot.Sources.Count != 1 ||
		snapshot.Sources.Items[0].RedactedTarget != "https://example.com:443/…" {
		t.Fatalf("source snapshot = %#v", snapshot.Sources)
	}
}

func TestManagerDoesNotPersistSourceWhenCandidateValidationFails(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	content := uploadSourceDraft(t, state, peer, runtimeapi.RemoteSourceDraft{
		Name: "invalid",
		URL:  "https://example.com/config.yaml",
	}, now)
	raw := []byte("invalid: true\n")
	rawDigest := sha256.Sum256(raw)
	manager := &Manager{
		State: state,
		Fetcher: &fakeSourceFetcher{results: []FetchResult{{
			Body:   raw,
			SHA256: hex.EncodeToString(rawDigest[:]),
		}}},
		Validator: fakeCandidateValidator{err: errors.New("Mihomo rejected configuration")},
		Now:       func() time.Time { return now },
	}
	_, err = manager.Execute(context.Background(), runtimeapi.Operation{
		ID:             "op_add",
		CallerIdentity: peer.Key(),
		Action: runtimeapi.Action{
			Kind:   runtimeapi.ActionAddRemoteSource,
			Params: runtimeapi.ActionParams{ContentID: content.ID},
		},
	}, successfulReporter)
	var managerError *ManagerError
	if !errors.As(err, &managerError) || managerError.Code != runtimeapi.ErrorInvalidRequest {
		t.Fatalf("validation error = %#v / %v", managerError, err)
	}
	snapshot, observeErr := state.Observe("dev", now)
	if observeErr != nil {
		t.Fatalf("observe sources: %v", observeErr)
	}
	if snapshot.Sources.Count != 0 {
		t.Fatalf("failed candidate persisted source: %#v", snapshot.Sources)
	}
}

func TestManagerRefreshUsesSelectedRouteAndPersistsBackoffWithoutReplacingRevision(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	next := now.Add(DefaultRefreshInterval)
	record, err := state.CreateRemoteSource(runtimestate.RemoteSourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "primary",
		URL:                    "https://example.com:443/config.yaml",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: int64(DefaultRefreshInterval / time.Second),
		TimeoutSeconds:         int(DefaultFetchTimeout / time.Second),
		MaxResponseBytes:       DefaultResponseBytes,
		LastRefreshResult:      "validated",
		LastRefreshRoute:       runtimeapi.SourceRouteDirect,
		LastRefreshAt:          timePointer(now),
		NextRefreshAt:          &next,
	}, []byte("raw-v1"), []byte("candidate-v1"), "op_add", now)
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	originalRevision := record.RevisionKey
	fetcher := &fakeSourceFetcher{
		results: []FetchResult{{}},
		errors: []error{&FetchError{
			Class:      FailureTemporary,
			Result:     "server_unavailable",
			Message:    "Remote source server is temporarily unavailable",
			Retryable:  true,
			RetryAfter: 2 * time.Minute,
		}},
	}
	refreshAt := now.Add(time.Minute)
	manager := &Manager{
		State:     state,
		Fetcher:   fetcher,
		Validator: fakeCandidateValidator{},
		Now:       func() time.Time { return refreshAt },
		Random:    func() float64 { return 0.5 },
	}
	_, err = manager.Execute(context.Background(), runtimeapi.Operation{
		ID:             "op_refresh",
		CallerIdentity: "linux:uid:1000",
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{
				SourceID: record.ID,
				Route:    runtimeapi.SourceRouteMihomo,
			},
		},
	}, successfulReporter)
	var managerError *ManagerError
	if !errors.As(err, &managerError) || !managerError.Retryable {
		t.Fatalf("refresh error = %#v / %v", managerError, err)
	}
	if len(fetcher.configs) != 1 || fetcher.configs[0].Route != runtimeapi.SourceRouteMihomo {
		t.Fatalf("fetch routes = %#v", fetcher.configs)
	}
	updated, err := state.GetRemoteSource(record.ID)
	if err != nil {
		t.Fatalf("read failed source state: %v", err)
	}
	if updated.RevisionKey != originalRevision ||
		updated.FailureClass != FailureTemporary ||
		updated.LastRefreshRoute != runtimeapi.SourceRouteMihomo ||
		updated.NextRefreshAt == nil ||
		!updated.NextRefreshAt.Equal(refreshAt.Add(2*time.Minute)) {
		t.Fatalf("failed refresh state = %#v", updated)
	}
}

func TestManagerNotModifiedRefreshKeepsValidatedCandidateAndDueActionIsUnique(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	due := now.Add(-time.Second)
	record, err := state.CreateRemoteSource(runtimestate.RemoteSourceRecord{
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "primary",
		URL:                    "https://example.com:443/config.yaml",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: int64(DefaultRefreshInterval / time.Second),
		TimeoutSeconds:         int(DefaultFetchTimeout / time.Second),
		MaxResponseBytes:       DefaultResponseBytes,
		ETag:                   `"v1"`,
		LastRefreshResult:      "validated",
		NextRefreshAt:          &due,
	}, []byte("raw-v1"), []byte("candidate-v1"), "op_add", now.Add(-DefaultRefreshInterval))
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	manager := &Manager{
		State: state,
		Fetcher: &fakeSourceFetcher{results: []FetchResult{{
			NotModified: true,
			Route:       runtimeapi.SourceRouteDirect,
		}}},
		Validator: fakeCandidateValidator{},
		Now:       func() time.Time { return now },
		Random:    func() float64 { return 0.5 },
	}
	actions, err := manager.DueActions(now)
	if err != nil || len(actions) != 1 || actions[0].Params.SourceID != record.ID {
		t.Fatalf("due actions = %#v, err=%v", actions, err)
	}
	result, err := manager.Execute(context.Background(), runtimeapi.Operation{
		ID:             "op_scheduled",
		CallerIdentity: "runtime:uid:0",
		Action:         actions[0],
	}, successfulReporter)
	if err != nil {
		t.Fatalf("scheduled not-modified refresh: %v", err)
	}
	if !result.NotModified || result.CandidateSHA256 != record.CandidateSHA256 {
		t.Fatalf("not-modified result = %#v", result)
	}
}

func TestRefreshSchedulingUsesJitterBackoffAndReasonableRetryAfter(t *testing.T) {
	now := time.Date(2026, 7, 30, 8, 0, 0, 0, time.UTC)
	regularLow := nextRegularRefresh(now, 6*time.Hour, 0)
	regularHigh := nextRegularRefresh(now, 6*time.Hour, 1)
	if !regularLow.Equal(now.Add(5*time.Hour+24*time.Minute)) ||
		!regularHigh.Equal(now.Add(6*time.Hour+36*time.Minute)) {
		t.Fatalf("regular jitter = %s / %s", regularLow, regularHigh)
	}
	first := nextFailureRefresh(now, 0, 6*time.Hour, 0, 0)
	second := nextFailureRefresh(now, 1, 6*time.Hour, 0, 0)
	retryAfter := nextFailureRefresh(now, 3, 6*time.Hour, 20*time.Minute, 1)
	if !first.Equal(now.Add(time.Minute)) ||
		!second.Equal(now.Add(5*time.Minute)) ||
		!retryAfter.Equal(now.Add(20*time.Minute)) {
		t.Fatalf("failure schedule = %s / %s / %s", first, second, retryAfter)
	}
}

func uploadSourceDraft(
	t *testing.T,
	state *runtimestate.Store,
	peer runtimeapi.PeerIdentity,
	draft runtimeapi.RemoteSourceDraft,
	now time.Time,
) runtimeapi.ImportContent {
	t.Helper()
	body, err := json.Marshal(draft)
	if err != nil {
		t.Fatalf("encode source draft: %v", err)
	}
	digest := sha256.Sum256(body)
	content, err := state.UploadImport(
		peer,
		runtimeapi.SourceDraftContentType,
		int64(len(body)),
		hex.EncodeToString(digest[:]),
		body,
		now,
	)
	if err != nil {
		t.Fatalf("upload source draft: %v", err)
	}
	return content
}

func successfulReporter(string, int, bool) error {
	return nil
}
