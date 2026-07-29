package runtimestate

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
	"submux/internal/safepath"
)

const MaxAdvancedOverrideBytes = 1 << 20

var advancedOverrideKey = []byte("advanced_override")

type AdvancedOverrideRecord struct {
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) SetAdvancedOverride(
	body []byte,
	operationID string,
	now time.Time,
) (AdvancedOverrideRecord, error) {
	if s == nil || s.db == nil {
		return AdvancedOverrideRecord{}, errors.New("Runtime state is not open")
	}
	if len(body) == 0 || len(body) > MaxAdvancedOverrideBytes {
		return AdvancedOverrideRecord{}, errors.New("Runtime advanced override exceeds the allowed size")
	}
	digest := sha256Hex(body)
	path, err := s.advancedOverridePath(digest)
	if err != nil {
		return AdvancedOverrideRecord{}, err
	}
	if err := writeOrVerifyImmutableSourceFile(path, body, digest); err != nil {
		return AdvancedOverrideRecord{}, fmt.Errorf("write Runtime advanced override: %w", err)
	}
	now = now.UTC()
	record := AdvancedOverrideRecord{
		Size:      int64(len(body)),
		SHA256:    digest,
		UpdatedAt: now,
	}
	err = s.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		encoded, err := json.Marshal(record)
		if err != nil {
			return err
		}
		if err := metadata.Put(advancedOverrideKey, encoded); err != nil {
			return err
		}
		_, err = advanceRevisionAndEvent(transaction, "override.updated", operationID, now)
		return err
	})
	if err != nil {
		return AdvancedOverrideRecord{}, err
	}
	return record, nil
}

func (s *Store) AdvancedOverride() ([]byte, AdvancedOverrideRecord, error) {
	if s == nil || s.db == nil {
		return nil, AdvancedOverrideRecord{}, errors.New("Runtime state is not open")
	}
	var record AdvancedOverrideRecord
	err := s.db.View(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		if metadata == nil {
			return errors.New("Runtime metadata is unavailable")
		}
		value := metadata.Get(advancedOverrideKey)
		if value == nil {
			return nil
		}
		if err := json.Unmarshal(value, &record); err != nil {
			return errors.New("Runtime advanced override record is invalid")
		}
		return validateAdvancedOverrideRecord(record)
	})
	if err != nil {
		return nil, AdvancedOverrideRecord{}, err
	}
	if record.SHA256 == "" {
		return []byte("{}\n"), AdvancedOverrideRecord{}, nil
	}
	path, err := s.advancedOverridePath(record.SHA256)
	if err != nil {
		return nil, AdvancedOverrideRecord{}, err
	}
	body, err := readVerifiedSourceFile(path, record.SHA256)
	if err != nil {
		return nil, AdvancedOverrideRecord{}, fmt.Errorf("read Runtime advanced override: %w", err)
	}
	if int64(len(body)) != record.Size {
		return nil, AdvancedOverrideRecord{}, errors.New("Runtime advanced override size is invalid")
	}
	return body, record, nil
}

func advancedOverrideSummary(transaction *bbolt.Tx) (runtimeapi.OverrideStatus, error) {
	metadata := transaction.Bucket(metadataBucket)
	if metadata == nil {
		return runtimeapi.OverrideStatus{}, errors.New("Runtime metadata is unavailable")
	}
	value := metadata.Get(advancedOverrideKey)
	if value == nil {
		return runtimeapi.OverrideStatus{}, nil
	}
	var record AdvancedOverrideRecord
	if err := json.Unmarshal(value, &record); err != nil {
		return runtimeapi.OverrideStatus{}, errors.New("Runtime advanced override record is invalid")
	}
	if err := validateAdvancedOverrideRecord(record); err != nil {
		return runtimeapi.OverrideStatus{}, err
	}
	updatedAt := record.UpdatedAt
	return runtimeapi.OverrideStatus{
		Present:   true,
		Size:      record.Size,
		SHA256:    record.SHA256,
		UpdatedAt: &updatedAt,
	}, nil
}

func (s *Store) advancedOverridePath(digest string) (string, error) {
	if len(digest) != 64 {
		return "", errors.New("Runtime advanced override digest is invalid")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errors.New("Runtime advanced override digest is invalid")
	}
	root := filepath.Join(s.root, "advanced-overrides")
	if filepath.Dir(root) != s.root {
		return "", errors.New("Runtime advanced override root escaped the state root")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	linked, err := safepath.ContainsLink(root)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime advanced override root: %w", err)
	}
	if linked {
		return "", errors.New("Runtime advanced override root must not contain symbolic or reparse links")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(root, strings.ToLower(digest)+".yaml")
	if filepath.Dir(path) != root {
		return "", errors.New("Runtime advanced override path escaped its root")
	}
	return path, nil
}

func validateAdvancedOverrideRecord(record AdvancedOverrideRecord) error {
	if record.Size < 1 ||
		record.Size > MaxAdvancedOverrideBytes ||
		len(record.SHA256) != 64 ||
		record.UpdatedAt.IsZero() {
		return errors.New("Runtime advanced override record is invalid")
	}
	if _, err := hex.DecodeString(record.SHA256); err != nil {
		return errors.New("Runtime advanced override record is invalid")
	}
	return nil
}
