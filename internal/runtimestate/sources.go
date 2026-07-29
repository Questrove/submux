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
)

type RemoteSourceRecord struct {
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

func (s *Store) CreateRemoteSource(
	record RemoteSourceRecord,
	raw []byte,
	candidate []byte,
	operationID string,
	now time.Time,
) (RemoteSourceRecord, error) {
	if s == nil || s.db == nil {
		return RemoteSourceRecord{}, errors.New("Runtime state is not open")
	}
	if record.ID == "" {
		var err error
		record.ID, err = randomIdentifier("src_")
		if err != nil {
			return RemoteSourceRecord{}, err
		}
	}
	if err := validateRemoteSourceRecord(record, true); err != nil {
		return RemoteSourceRecord{}, err
	}
	rawDigest, candidateDigest, revisionKey, err := s.writeSourceRevision(record.ID, raw, candidate)
	if err != nil {
		return RemoteSourceRecord{}, err
	}
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
		return RemoteSourceRecord{}, err
	}
	return record, nil
}

func (s *Store) GetRemoteSource(id string) (RemoteSourceRecord, error) {
	if s == nil || s.db == nil {
		return RemoteSourceRecord{}, errors.New("Runtime state is not open")
	}
	if !validSourceID(id) {
		return RemoteSourceRecord{}, ErrSourceNotFound
	}
	var record RemoteSourceRecord
	err := s.db.View(func(transaction *bbolt.Tx) error {
		var err error
		record, err = readRemoteSource(transaction.Bucket(sourcesBucket), id)
		return err
	})
	return record, err
}

func (s *Store) CurrentRemoteSource() (RemoteSourceRecord, error) {
	if s == nil || s.db == nil {
		return RemoteSourceRecord{}, errors.New("Runtime state is not open")
	}
	var record RemoteSourceRecord
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
	var revisionKey string
	if !update.NotModified {
		rawDigest, candidateDigest, storedRevision, err := s.writeSourceRevision(id, update.Raw, update.Candidate)
		if err != nil {
			return RemoteSourceRecord{}, err
		}
		if update.RawSHA256 != "" && !strings.EqualFold(update.RawSHA256, rawDigest) {
			return RemoteSourceRecord{}, errors.New("Runtime source raw digest changed before commit")
		}
		if update.CandidateSHA256 != "" && !strings.EqualFold(update.CandidateSHA256, candidateDigest) {
			return RemoteSourceRecord{}, errors.New("Runtime source candidate digest changed before commit")
		}
		update.RawSHA256 = rawDigest
		update.CandidateSHA256 = candidateDigest
		revisionKey = storedRevision
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
	return record, err
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
		found = record.RefreshIntervalSeconds > 0 &&
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

func (s *Store) ReadRemoteSourceRevision(record RemoteSourceRecord) ([]byte, []byte, error) {
	if s == nil || s.root == "" {
		return nil, nil, errors.New("Runtime state is not open")
	}
	if err := validateRemoteSourceRecord(record, false); err != nil {
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
		if err := validateRemoteSourceRecord(record, false); err != nil {
			return err
		}
		summary := runtimeapi.SourceSummary{
			ID:                     record.ID,
			Type:                   record.Type,
			Name:                   record.Name,
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
	if err := validateRemoteSourceRecord(record, false); err != nil {
		return RemoteSourceRecord{}, err
	}
	return record, nil
}

func validateRemoteSourceRecord(record RemoteSourceRecord, allowMissingRevision bool) error {
	if !validSourceID(record.ID) ||
		record.Type != runtimeapi.SourceTypeRemoteHTTP ||
		record.Name == "" ||
		record.URL == "" ||
		record.RedactedTarget == "" ||
		(record.Route != runtimeapi.SourceRouteDirect && record.Route != runtimeapi.SourceRouteMihomo) {
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

func (s *Store) writeSourceRevision(sourceID string, raw, candidate []byte) (string, string, string, error) {
	if len(raw) == 0 || len(candidate) == 0 {
		return "", "", "", errors.New("Runtime source revision content is empty")
	}
	rawHash := sha256.Sum256(raw)
	candidateHash := sha256.Sum256(candidate)
	rawDigest := hex.EncodeToString(rawHash[:])
	candidateDigest := hex.EncodeToString(candidateHash[:])
	revisionKey := rawDigest + "-" + candidateDigest
	directory, err := s.sourceRevisionDirectory(sourceID, revisionKey, true)
	if err != nil {
		return "", "", "", err
	}
	if err := writeOrVerifyImmutableSourceFile(filepath.Join(directory, "source.yaml"), raw, rawDigest); err != nil {
		return "", "", "", err
	}
	if err := writeOrVerifyImmutableSourceFile(filepath.Join(directory, "candidate.yaml"), candidate, candidateDigest); err != nil {
		return "", "", "", err
	}
	return rawDigest, candidateDigest, revisionKey, nil
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
