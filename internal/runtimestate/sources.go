package runtimestate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
	"submux/internal/safepath"
)

var (
	sourcesBucket       = []byte("runtime_sources")
	currentSourceIDKey  = []byte("current_source_id")
	ErrSourceNotFound   = errors.New("Runtime configuration source was not found")
	ErrSourceIDConflict = errors.New("Runtime configuration source ID already exists")
	ErrCurrentSource    = errors.New("Runtime configuration source is current")
	ErrSourceChanged    = errors.New("Runtime current configuration source changed")
)

const MaxRetainedSourceRevisions = 3

type SourceRecord struct {
	ID                     string     `json:"id"`
	Type                   string     `json:"type"`
	Name                   string     `json:"name"`
	URL                    string     `json:"url"`
	RedactedTarget         string     `json:"redacted_target"`
	Route                  string     `json:"route"`
	UserAgent              string     `json:"user_agent,omitempty"`
	Username               string     `json:"username,omitempty"`
	Password               string     `json:"password,omitempty"`
	AuthorizedTarget       string     `json:"authorized_target,omitempty"`
	AllowPrivate           bool       `json:"allow_private,omitempty"`
	AllowHTTP              bool       `json:"allow_http,omitempty"`
	CustomCAPEM            string     `json:"custom_ca_pem,omitempty"`
	SkipTLSVerify          bool       `json:"skip_tls_verify,omitempty"`
	RefreshIntervalSeconds int64      `json:"refresh_interval_seconds"`
	TimeoutSeconds         int        `json:"timeout_seconds"`
	MaxResponseBytes       int64      `json:"max_response_bytes"`
	ETag                   string     `json:"etag,omitempty"`
	LastModified           string     `json:"last_modified,omitempty"`
	RawSHA256              string     `json:"raw_sha256,omitempty"`
	CandidateSHA256        string     `json:"candidate_sha256,omitempty"`
	RevisionKey            string     `json:"revision_key,omitempty"`
	LastRefreshResult      string     `json:"last_refresh_result,omitempty"`
	LastRefreshRoute       string     `json:"last_refresh_route,omitempty"`
	LastRefreshAt          *time.Time `json:"last_refresh_at,omitempty"`
	LastManualRefreshAt    *time.Time `json:"last_manual_refresh_at,omitempty"`
	NextRefreshAt          *time.Time `json:"next_refresh_at,omitempty"`
	FailureClass           string     `json:"failure_class,omitempty"`
	AttemptCount           int        `json:"attempt_count,omitempty"`
	CreatedAt              time.Time  `json:"created_at"`
	UpdatedAt              time.Time  `json:"updated_at"`
}

type RemoteSourceRecord = SourceRecord

type SourceRefreshSuccess struct {
	ETag            string
	LastModified    string
	Result          string
	Route           string
	NextRefreshAt   *time.Time
	Manual          bool
	NotModified     bool
	Raw             []byte
	Candidate       []byte
	RawSHA256       string
	CandidateSHA256 string
}

type SourceRefreshFailure struct {
	Result        string
	Route         string
	FailureClass  string
	NextRefreshAt *time.Time
	Manual        bool
}

func (s *Store) CreateSource(
	record SourceRecord,
	raw []byte,
	candidate []byte,
	operationID string,
	now time.Time,
) (SourceRecord, error) {
	if s == nil || s.db == nil {
		return SourceRecord{}, errors.New("Runtime state is not open")
	}
	if record.ID == "" {
		var err error
		record.ID, err = randomIdentifier("src_")
		if err != nil {
			return SourceRecord{}, err
		}
	}
	if err := validateSourceRecord(record, true); err != nil {
		return SourceRecord{}, err
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	rawDigest, candidateDigest, revisionKey, revisionCreated, err := s.writeSourceRevision(record.ID, raw, candidate)
	if err != nil {
		return SourceRecord{}, err
	}
	revisionCommitted := false
	defer func() {
		if revisionCreated && !revisionCommitted {
			_ = s.removeSourceRevision(record.ID, revisionKey)
		}
	}()
	now = now.UTC()
	record.RawSHA256 = rawDigest
	record.CandidateSHA256 = candidateDigest
	record.RevisionKey = revisionKey
	record.CreatedAt = now
	record.UpdatedAt = now
	err = s.db.Update(func(transaction *bbolt.Tx) error {
		sources := transaction.Bucket(sourcesBucket)
		metadata := transaction.Bucket(metadataBucket)
		if sources == nil || metadata == nil {
			return errors.New("Runtime source state is unavailable")
		}
		if sources.Get([]byte(record.ID)) != nil {
			return ErrSourceIDConflict
		}
		if err := putJSON(sources, []byte(record.ID), record); err != nil {
			return err
		}
		if metadata.Get(currentSourceIDKey) == nil {
			if err := metadata.Put(currentSourceIDKey, []byte(record.ID)); err != nil {
				return err
			}
		}
		_, err := advanceRevisionAndEvent(transaction, "source.added", operationID, now)
		return err
	})
	if err != nil {
		return SourceRecord{}, err
	}
	revisionCommitted = true
	s.maintainSourceRevisionHistory(record.ID, record.RevisionKey, now)
	return record, nil
}

func (s *Store) CreateRemoteSource(
	record RemoteSourceRecord,
	raw []byte,
	candidate []byte,
	operationID string,
	now time.Time,
) (RemoteSourceRecord, error) {
	return s.CreateSource(record, raw, candidate, operationID, now)
}

func (s *Store) GetSource(id string) (SourceRecord, error) {
	if s == nil || s.db == nil {
		return SourceRecord{}, errors.New("Runtime state is not open")
	}
	if !validSourceID(id) {
		return SourceRecord{}, ErrSourceNotFound
	}
	var record SourceRecord
	err := s.db.View(func(transaction *bbolt.Tx) error {
		var err error
		record, err = readRemoteSource(transaction.Bucket(sourcesBucket), id)
		return err
	})
	return record, err
}

func (s *Store) GetRemoteSource(id string) (RemoteSourceRecord, error) {
	record, err := s.GetSource(id)
	if err != nil {
		return RemoteSourceRecord{}, err
	}
	if !remoteSourceType(record.Type) {
		return RemoteSourceRecord{}, ErrSourceNotFound
	}
	return record, nil
}

func (s *Store) CurrentSource() (SourceRecord, error) {
	if s == nil || s.db == nil {
		return SourceRecord{}, errors.New("Runtime state is not open")
	}
	var record SourceRecord
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		id := string(metadata.Get(currentSourceIDKey))
		if id == "" {
			return ErrSourceNotFound
		}
		var err error
		record, err = readRemoteSource(transaction.Bucket(sourcesBucket), id)
		return err
	})
	return record, err
}

func (s *Store) CurrentRemoteSource() (RemoteSourceRecord, error) {
	record, err := s.CurrentSource()
	if err != nil {
		return RemoteSourceRecord{}, err
	}
	if !remoteSourceType(record.Type) {
		return RemoteSourceRecord{}, ErrSourceNotFound
	}
	return record, nil
}

func (s *Store) SwitchCurrentSource(
	expectedCurrentID string,
	targetSourceID string,
	operationID string,
	now time.Time,
) (SourceRecord, error) {
	if s == nil || s.db == nil {
		return SourceRecord{}, errors.New("Runtime state is not open")
	}
	if (expectedCurrentID != "" && !validSourceID(expectedCurrentID)) ||
		!validSourceID(targetSourceID) {
		return SourceRecord{}, ErrSourceNotFound
	}
	now = now.UTC()
	var target SourceRecord
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		sources := transaction.Bucket(sourcesBucket)
		if metadata == nil || sources == nil {
			return errors.New("Runtime source state is unavailable")
		}
		currentID := string(metadata.Get(currentSourceIDKey))
		if currentID != expectedCurrentID {
			return ErrSourceChanged
		}
		var err error
		target, err = readRemoteSource(sources, targetSourceID)
		if err != nil {
			return err
		}
		if currentID == targetSourceID {
			return nil
		}
		if err := metadata.Put(currentSourceIDKey, []byte(targetSourceID)); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "source.switched", operationID, now)
		return err
	})
	return target, err
}

func (s *Store) DeleteSource(
	sourceID string,
	allowCurrent bool,
	operationID string,
	now time.Time,
) (SourceRecord, error) {
	if s == nil || s.db == nil {
		return SourceRecord{}, errors.New("Runtime state is not open")
	}
	if !validSourceID(sourceID) {
		return SourceRecord{}, ErrSourceNotFound
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	now = now.UTC()
	var deleted SourceRecord
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		sources := transaction.Bucket(sourcesBucket)
		if metadata == nil || sources == nil {
			return errors.New("Runtime source state is unavailable")
		}
		currentID := string(metadata.Get(currentSourceIDKey))
		if currentID == sourceID && !allowCurrent {
			return ErrCurrentSource
		}
		var err error
		deleted, err = readRemoteSource(sources, sourceID)
		if err != nil {
			return err
		}
		if err := sources.Delete([]byte(sourceID)); err != nil {
			return err
		}
		if currentID == sourceID {
			if err := metadata.Delete(currentSourceIDKey); err != nil {
				return err
			}
		}
		_, err = advanceRevisionAndEvent(transaction, "source.deleted", operationID, now)
		return err
	})
	return deleted, err
}

func (s *Store) CommitRemoteSourceRefresh(
	id string,
	update SourceRefreshSuccess,
	operationID string,
	now time.Time,
) (RemoteSourceRecord, error) {
	if s == nil || s.db == nil {
		return RemoteSourceRecord{}, errors.New("Runtime state is not open")
	}
	if !validSourceID(id) {
		return RemoteSourceRecord{}, ErrSourceNotFound
	}
	s.sourceMu.Lock()
	defer s.sourceMu.Unlock()
	var revisionKey string
	var revisionCreated bool
	revisionCommitted := false
	defer func() {
		if revisionCreated && !revisionCommitted {
			_ = s.removeSourceRevision(id, revisionKey)
		}
	}()
	if !update.NotModified {
		rawDigest, candidateDigest, storedRevision, created, err := s.writeSourceRevision(id, update.Raw, update.Candidate)
		if err != nil {
			return RemoteSourceRecord{}, err
		}
		revisionCreated = created
		revisionKey = storedRevision
		if update.RawSHA256 != "" && !strings.EqualFold(update.RawSHA256, rawDigest) {
			return RemoteSourceRecord{}, errors.New("Runtime source raw digest changed before commit")
		}
		if update.CandidateSHA256 != "" && !strings.EqualFold(update.CandidateSHA256, candidateDigest) {
			return RemoteSourceRecord{}, errors.New("Runtime source candidate digest changed before commit")
		}
		update.RawSHA256 = rawDigest
		update.CandidateSHA256 = candidateDigest
	}
	now = now.UTC()
	var record RemoteSourceRecord
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		sources := transaction.Bucket(sourcesBucket)
		var err error
		record, err = readRemoteSource(sources, id)
		if err != nil {
			return err
		}
		if !remoteSourceType(record.Type) {
			return ErrSourceNotFound
		}
		record.ETag = update.ETag
		record.LastModified = update.LastModified
		record.LastRefreshResult = update.Result
		record.LastRefreshRoute = update.Route
		record.LastRefreshAt = timePointer(now)
		if update.Manual {
			record.LastManualRefreshAt = timePointer(now)
		}
		record.NextRefreshAt = normalizedTimePointer(update.NextRefreshAt)
		record.FailureClass = ""
		record.AttemptCount = 0
		record.UpdatedAt = now
		if !update.NotModified {
			record.RawSHA256 = update.RawSHA256
			record.CandidateSHA256 = update.CandidateSHA256
			record.RevisionKey = revisionKey
		}
		if err := putJSON(sources, []byte(id), record); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "source.refreshed", operationID, now)
		return err
	})
	if err != nil {
		return RemoteSourceRecord{}, err
	}
	if !update.NotModified {
		revisionCommitted = true
		s.maintainSourceRevisionHistory(record.ID, record.RevisionKey, now)
	}
	return record, nil
}

func (s *Store) RecordRemoteSourceFailure(
	id string,
	update SourceRefreshFailure,
	operationID string,
	now time.Time,
) (RemoteSourceRecord, error) {
	if s == nil || s.db == nil {
		return RemoteSourceRecord{}, errors.New("Runtime state is not open")
	}
	if !validSourceID(id) {
		return RemoteSourceRecord{}, ErrSourceNotFound
	}
	now = now.UTC()
	var record RemoteSourceRecord
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		sources := transaction.Bucket(sourcesBucket)
		var err error
		record, err = readRemoteSource(sources, id)
		if err != nil {
			return err
		}
		if !remoteSourceType(record.Type) {
			return ErrSourceNotFound
		}
		record.LastRefreshResult = update.Result
		record.LastRefreshRoute = update.Route
		record.LastRefreshAt = timePointer(now)
		if update.Manual {
			record.LastManualRefreshAt = timePointer(now)
		}
		record.NextRefreshAt = normalizedTimePointer(update.NextRefreshAt)
		record.FailureClass = update.FailureClass
		record.AttemptCount++
		record.UpdatedAt = now
		if err := putJSON(sources, []byte(id), record); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "source.refresh_failed", operationID, now)
		return err
	})
	return record, err
}

func (s *Store) DueCurrentRemoteSource(now time.Time) (RemoteSourceRecord, bool, error) {
	if s == nil || s.db == nil {
		return RemoteSourceRecord{}, false, errors.New("Runtime state is not open")
	}
	now = now.UTC()
	var record RemoteSourceRecord
	found := false
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		id := string(metadata.Get(currentSourceIDKey))
		if id == "" {
			return nil
		}
		var err error
		record, err = readRemoteSource(transaction.Bucket(sourcesBucket), id)
		if err != nil {
			return err
		}
		found = remoteSourceType(record.Type) &&
			record.RefreshIntervalSeconds > 0 &&
			record.NextRefreshAt != nil &&
			!now.Before(record.NextRefreshAt.UTC())
		return nil
	})
	return record, found, err
}

func (s *Store) HasActiveSourceRefresh(sourceID string) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("Runtime state is not open")
	}
	active := false
	err := s.db.View(func(transaction *bbolt.Tx) error {
		operations := transaction.Bucket(operationsBucket)
		if operations == nil {
			return errors.New("Runtime operation state is unavailable")
		}
		return operations.ForEach(func(_, value []byte) error {
			var operation runtimeapi.Operation
			if err := json.Unmarshal(value, &operation); err != nil {
				return errors.New("Runtime operation record is invalid")
			}
			if operation.Action.Kind == runtimeapi.ActionRefreshSource &&
				operation.Action.Params.SourceID == sourceID &&
				!isTerminalOperationState(operation.State) {
				active = true
			}
			return nil
		})
	})
	return active, err
}

func (s *Store) ReadSourceRevision(record SourceRecord) ([]byte, []byte, error) {
	if s == nil || s.root == "" {
		return nil, nil, errors.New("Runtime state is not open")
	}
	if err := validateSourceRecord(record, false); err != nil {
		return nil, nil, err
	}
	directory, err := s.sourceRevisionDirectory(record.ID, record.RevisionKey, false)
	if err != nil {
		return nil, nil, err
	}
	raw, err := readVerifiedSourceFile(filepath.Join(directory, "source.yaml"), record.RawSHA256)
	if err != nil {
		return nil, nil, err
	}
	candidate, err := readVerifiedSourceFile(filepath.Join(directory, "candidate.yaml"), record.CandidateSHA256)
	if err != nil {
		return nil, nil, err
	}
	return raw, candidate, nil
}

func (s *Store) ReadRemoteSourceRevision(record RemoteSourceRecord) ([]byte, []byte, error) {
	return s.ReadSourceRevision(record)
}

func sourceSummary(transaction *bbolt.Tx) (runtimeapi.SourceStatus, error) {
	sources := transaction.Bucket(sourcesBucket)
	metadata := transaction.Bucket(metadataBucket)
	if sources == nil || metadata == nil {
		return runtimeapi.SourceStatus{}, errors.New("Runtime source state is unavailable")
	}
	currentID := string(metadata.Get(currentSourceIDKey))
	status := runtimeapi.SourceStatus{CurrentSourceID: currentID}
	if err := sources.ForEach(func(_, value []byte) error {
		var record RemoteSourceRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return errors.New("Runtime source record is invalid")
		}
		if err := validateSourceRecord(record, false); err != nil {
			return err
		}
		summary := runtimeapi.SourceSummary{
			ID:                     record.ID,
			Type:                   record.Type,
			Name:                   record.Name,
			Current:                record.ID == currentID,
			RedactedTarget:         record.RedactedTarget,
			Route:                  record.Route,
			RefreshIntervalSeconds: record.RefreshIntervalSeconds,
			LastRefreshResult:      record.LastRefreshResult,
			LastRefreshAt:          normalizedTimePointer(record.LastRefreshAt),
			NextRefreshAt:          normalizedTimePointer(record.NextRefreshAt),
			LastRefreshRoute:       record.LastRefreshRoute,
			FailureClass:           record.FailureClass,
			HighRiskSettings:       sourceRiskSettings(record),
			HasValidatedCandidate:  record.CandidateSHA256 != "",
		}
		status.Items = append(status.Items, summary)
		if record.ID == currentID {
			status.LastRefreshResult = record.LastRefreshResult
		}
		return nil
	}); err != nil {
		return runtimeapi.SourceStatus{}, err
	}
	sort.Slice(status.Items, func(left, right int) bool {
		return status.Items[left].ID < status.Items[right].ID
	})
	status.Count = len(status.Items)
	return status, nil
}

func readRemoteSource(bucket *bbolt.Bucket, id string) (RemoteSourceRecord, error) {
	if bucket == nil {
		return RemoteSourceRecord{}, errors.New("Runtime source state is unavailable")
	}
	value := bucket.Get([]byte(id))
	if value == nil {
		return RemoteSourceRecord{}, ErrSourceNotFound
	}
	var record RemoteSourceRecord
	if err := json.Unmarshal(value, &record); err != nil {
		return RemoteSourceRecord{}, errors.New("Runtime source record is invalid")
	}
	if record.ID != id {
		return RemoteSourceRecord{}, errors.New("Runtime source record is invalid")
	}
	if err := validateSourceRecord(record, false); err != nil {
		return RemoteSourceRecord{}, err
	}
	return record, nil
}

func validateSourceRecord(record SourceRecord, allowMissingRevision bool) error {
	if !validSourceID(record.ID) ||
		record.Name == "" ||
		len(record.Name) > 128 ||
		containsControl(record.Name) ||
		record.RedactedTarget == "" ||
		containsControl(record.RedactedTarget) {
		return errors.New("Runtime source record is invalid")
	}
	switch record.Type {
	case runtimeapi.SourceTypeRemoteHTTP, runtimeapi.SourceTypeSubmuxOutput:
		if record.URL == "" ||
			(record.Route != runtimeapi.SourceRouteDirect && record.Route != runtimeapi.SourceRouteMihomo) {
			return errors.New("Runtime source record is invalid")
		}
	case runtimeapi.SourceTypeLocalImport:
		if record.URL != "" ||
			record.Route != "" ||
			record.UserAgent != "" ||
			record.Username != "" ||
			record.Password != "" ||
			record.AuthorizedTarget != "" ||
			record.AllowPrivate ||
			record.AllowHTTP ||
			record.CustomCAPEM != "" ||
			record.SkipTLSVerify ||
			record.RefreshIntervalSeconds != 0 ||
			record.TimeoutSeconds != 0 ||
			record.MaxResponseBytes != 0 ||
			record.ETag != "" ||
			record.LastModified != "" ||
			record.LastRefreshRoute != "" ||
			record.LastManualRefreshAt != nil ||
			record.NextRefreshAt != nil ||
			record.FailureClass != "" ||
			record.AttemptCount != 0 {
			return errors.New("Runtime local source record is invalid")
		}
	default:
		return errors.New("Runtime source record is invalid")
	}
	if allowMissingRevision {
		return nil
	}
	if len(record.RawSHA256) != 64 ||
		len(record.CandidateSHA256) != 64 ||
		record.RevisionKey != record.RawSHA256+"-"+record.CandidateSHA256 {
		return errors.New("Runtime source revision record is invalid")
	}
	return nil
}

func validateRemoteSourceRecord(record RemoteSourceRecord, allowMissingRevision bool) error {
	if !remoteSourceType(record.Type) {
		return errors.New("Runtime remote source record is invalid")
	}
	return validateSourceRecord(record, allowMissingRevision)
}

func remoteSourceType(sourceType string) bool {
	return sourceType == runtimeapi.SourceTypeRemoteHTTP ||
		sourceType == runtimeapi.SourceTypeSubmuxOutput
}

func validSourceID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "src_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func sourceRiskSettings(record RemoteSourceRecord) []string {
	var risks []string
	if record.AllowPrivate {
		risks = append(risks, "allow_private")
	}
	if record.AllowHTTP {
		risks = append(risks, "allow_http")
	}
	if record.CustomCAPEM != "" {
		risks = append(risks, "custom_ca")
	}
	if record.SkipTLSVerify {
		risks = append(risks, "skip_tls_verify")
	}
	return risks
}

func (s *Store) writeSourceRevision(sourceID string, raw, candidate []byte) (string, string, string, bool, error) {
	if len(raw) == 0 || len(candidate) == 0 {
		return "", "", "", false, errors.New("Runtime source revision content is empty")
	}
	rawHash := sha256.Sum256(raw)
	candidateHash := sha256.Sum256(candidate)
	rawDigest := hex.EncodeToString(rawHash[:])
	candidateDigest := hex.EncodeToString(candidateHash[:])
	revisionKey := rawDigest + "-" + candidateDigest
	if !validSourceID(sourceID) {
		return "", "", "", false, errors.New("Runtime source ID is invalid")
	}
	root, err := s.safeSourceRoot()
	if err != nil {
		return "", "", "", false, err
	}
	revisionPath := filepath.Join(root, sourceID, "revisions", revisionKey)
	_, statErr := os.Lstat(revisionPath)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return "", "", "", false, statErr
	}
	directory, err := s.sourceRevisionDirectory(sourceID, revisionKey, true)
	if err != nil {
		return "", "", "", false, err
	}
	if err := writeOrVerifyImmutableSourceFile(filepath.Join(directory, "source.yaml"), raw, rawDigest); err != nil {
		if created {
			_ = s.removeSourceRevision(sourceID, revisionKey)
		}
		return "", "", "", false, err
	}
	if err := writeOrVerifyImmutableSourceFile(filepath.Join(directory, "candidate.yaml"), candidate, candidateDigest); err != nil {
		if created {
			_ = s.removeSourceRevision(sourceID, revisionKey)
		}
		return "", "", "", false, err
	}
	return rawDigest, candidateDigest, revisionKey, created, nil
}

func (s *Store) sourceRevisionDirectory(sourceID, revisionKey string, create bool) (string, error) {
	if !validSourceID(sourceID) {
		return "", errors.New("Runtime source ID is invalid")
	}
	parts := strings.Split(revisionKey, "-")
	if len(parts) != 2 || len(parts[0]) != 64 || len(parts[1]) != 64 {
		return "", errors.New("Runtime source revision key is invalid")
	}
	if _, err := hex.DecodeString(parts[0]); err != nil {
		return "", errors.New("Runtime source revision key is invalid")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return "", errors.New("Runtime source revision key is invalid")
	}
	root, err := s.safeSourceRoot()
	if err != nil {
		return "", err
	}
	sourceRoot := filepath.Join(root, sourceID)
	revisionsRoot := filepath.Join(sourceRoot, "revisions")
	directory := filepath.Join(revisionsRoot, revisionKey)
	if filepath.Dir(sourceRoot) != root ||
		filepath.Dir(revisionsRoot) != sourceRoot ||
		filepath.Dir(directory) != revisionsRoot {
		return "", errors.New("Runtime source revision path escaped the state root")
	}
	if create {
		for _, path := range []string{sourceRoot, revisionsRoot, directory} {
			if err := ensureRealDirectory(path); err != nil {
				return "", err
			}
		}
	}
	linked, err := safepath.ContainsLink(directory)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime source revision: %w", err)
	}
	if linked {
		return "", errors.New("Runtime source revision must not contain symbolic or reparse links")
	}
	return directory, nil
}

func (s *Store) pruneSourceRevisions(sourceID, currentRevisionKey string, limit int) error {
	if limit < 1 {
		return errors.New("Runtime source revision retention must be positive")
	}
	if !validSourceID(sourceID) {
		return errors.New("Runtime source ID is invalid")
	}
	if _, err := s.sourceRevisionDirectory(sourceID, currentRevisionKey, false); err != nil {
		return err
	}
	root, err := s.safeSourceRoot()
	if err != nil {
		return err
	}
	revisionsRoot := filepath.Join(root, sourceID, "revisions")
	if filepath.Dir(filepath.Dir(revisionsRoot)) != root {
		return errors.New("Runtime source revision path escaped the state root")
	}
	entries, err := os.ReadDir(revisionsRoot)
	if err != nil {
		return err
	}
	type revisionEntry struct {
		name    string
		path    string
		modTime time.Time
		current bool
	}
	revisions := make([]revisionEntry, 0, len(entries))
	for _, entry := range entries {
		directory, err := s.sourceRevisionDirectory(sourceID, entry.Name(), false)
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !entry.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Runtime source revision history contains an unmanaged entry")
		}
		revisions = append(revisions, revisionEntry{
			name:    entry.Name(),
			path:    directory,
			modTime: info.ModTime(),
			current: entry.Name() == currentRevisionKey,
		})
	}
	sort.Slice(revisions, func(left, right int) bool {
		if revisions[left].current != revisions[right].current {
			return revisions[left].current
		}
		if revisions[left].modTime.Equal(revisions[right].modTime) {
			return revisions[left].name > revisions[right].name
		}
		return revisions[left].modTime.After(revisions[right].modTime)
	})
	if len(revisions) <= limit {
		return nil
	}
	for _, revision := range revisions[limit:] {
		if err := removeSourceRevision(revisionsRoot, revision.path); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) markSourceRevisionApplied(sourceID, revisionKey string, appliedAt time.Time) error {
	directory, err := s.sourceRevisionDirectory(sourceID, revisionKey, false)
	if err != nil {
		return err
	}
	appliedAt = appliedAt.UTC()
	return os.Chtimes(directory, appliedAt, appliedAt)
}

func (s *Store) maintainSourceRevisionHistory(sourceID, revisionKey string, appliedAt time.Time) {
	// Once the bbolt transaction commits, the source record is authoritative.
	// History maintenance must not turn that successful state transition into a
	// reported failure. A later successful source write retries the pruning.
	if err := s.markSourceRevisionApplied(sourceID, revisionKey, appliedAt); err != nil {
		return
	}
	_ = s.pruneSourceRevisions(sourceID, revisionKey, MaxRetainedSourceRevisions)
}

func removeSourceRevision(revisionsRoot, path string) error {
	if filepath.Dir(path) != revisionsRoot {
		return errors.New("refusing to remove a path outside Runtime source revision history")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return fmt.Errorf("inspect Runtime source revision before removal: %w", err)
	}
	if linked {
		return errors.New("refusing to remove a linked Runtime source revision")
	}
	return os.RemoveAll(path)
}

func (s *Store) removeSourceRevision(sourceID, revisionKey string) error {
	directory, err := s.sourceRevisionDirectory(sourceID, revisionKey, false)
	if err != nil {
		return err
	}
	root, err := s.safeSourceRoot()
	if err != nil {
		return err
	}
	revisionsRoot := filepath.Join(root, sourceID, "revisions")
	return removeSourceRevision(revisionsRoot, directory)
}

func (s *Store) safeSourceRoot() (string, error) {
	root := filepath.Join(s.root, "sources")
	if filepath.Dir(root) != s.root {
		return "", errors.New("Runtime source root escaped the state root")
	}
	if err := ensureRealDirectory(root); err != nil {
		return "", err
	}
	linked, err := safepath.ContainsLink(root)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime source root: %w", err)
	}
	if linked {
		return "", errors.New("Runtime source root must not contain symbolic or reparse links")
	}
	return root, nil
}

func (s *Store) RuntimeDataDir() (string, error) {
	if s == nil || s.root == "" {
		return "", errors.New("Runtime state is not open")
	}
	dataRoot := filepath.Join(s.root, "mihomo-data")
	if filepath.Dir(dataRoot) != s.root {
		return "", errors.New("Runtime data path escaped the state root")
	}
	if err := ensureRealDirectory(dataRoot); err != nil {
		return "", err
	}
	linked, err := safepath.ContainsLink(dataRoot)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime data directory: %w", err)
	}
	if linked {
		return "", errors.New("Runtime data directory must not contain symbolic or reparse links")
	}
	return dataRoot, nil
}

func (s *Store) SourceRuntimeDataDir(sourceID string) (string, error) {
	if !validSourceID(sourceID) {
		return "", errors.New("Runtime source ID is invalid")
	}
	dataRoot, err := s.RuntimeDataDir()
	if err != nil {
		return "", err
	}
	sourcesRoot := filepath.Join(dataRoot, "sources")
	sourceRoot := filepath.Join(sourcesRoot, sourceID)
	if filepath.Dir(sourcesRoot) != dataRoot ||
		filepath.Dir(sourceRoot) != sourcesRoot {
		return "", errors.New("Runtime source data path escaped the state root")
	}
	for _, path := range []string{sourcesRoot, sourceRoot} {
		if err := ensureRealDirectory(path); err != nil {
			return "", err
		}
	}
	linked, err := safepath.ContainsLink(sourceRoot)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime source data directory: %w", err)
	}
	if linked {
		return "", errors.New("Runtime source data directory must not contain symbolic or reparse links")
	}
	return sourceRoot, nil
}

func ensureRealDirectory(path string) error {
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("Runtime source directory is not a real directory")
	}
	return os.Chmod(path, 0700)
}

func writeOrVerifyImmutableSourceFile(path string, body []byte, expectedDigest string) error {
	err := writeImmutableImport(path, body)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	_, err = readVerifiedSourceFile(path, expectedDigest)
	return err
}

func readVerifiedSourceFile(path, expectedDigest string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Runtime source revision is not a regular file")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), expectedDigest) {
		return nil, errors.New("Runtime source revision digest mismatch")
	}
	return body, nil
}

func timePointer(value time.Time) *time.Time {
	value = value.UTC()
	return &value
}

func normalizedTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	return timePointer(*value)
}
