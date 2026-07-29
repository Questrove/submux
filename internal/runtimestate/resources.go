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
	"unicode"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
	"submux/internal/safepath"
)

const (
	MaxManagedResourceBytes  = 4 << 20
	MaxManagedResourcesBytes = 32 << 20
	MaxManagedResourceCount  = 256
)

var managedResourcesBucket = []byte("runtime_managed_resources")

var ErrManagedResourceNotFound = errors.New("Runtime managed resource was not found")

type ManagedResourceRecord struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

type ManagedResource struct {
	ManagedResourceRecord
	Path string
}

func (s *Store) CreateManagedResource(
	name string,
	kind string,
	content []byte,
	operationID string,
	now time.Time,
) (ManagedResourceRecord, error) {
	if s == nil || s.db == nil {
		return ManagedResourceRecord{}, errors.New("Runtime state is not open")
	}
	if !validResourceName(name) || !validResourceKind(kind) {
		return ManagedResourceRecord{}, errors.New("Runtime managed resource metadata is invalid")
	}
	if len(content) == 0 || len(content) > MaxManagedResourceBytes {
		return ManagedResourceRecord{}, errors.New("Runtime managed resource exceeds the per-resource limit")
	}
	digest := sha256Hex(content)
	path, err := s.managedResourceObjectPath(digest)
	if err != nil {
		return ManagedResourceRecord{}, err
	}
	id, err := randomIdentifier("res_")
	if err != nil {
		return ManagedResourceRecord{}, err
	}
	now = now.UTC()
	record := ManagedResourceRecord{
		ID:        id,
		Name:      name,
		Kind:      kind,
		Size:      int64(len(content)),
		SHA256:    digest,
		CreatedAt: now,
	}
	createdObject := false
	err = s.db.Update(func(transaction *bbolt.Tx) error {
		resources := transaction.Bucket(managedResourcesBucket)
		if resources == nil {
			return errors.New("Runtime managed resource state is unavailable")
		}
		count := 0
		var total int64
		if err := resources.ForEach(func(_, value []byte) error {
			var existing ManagedResourceRecord
			if err := json.Unmarshal(value, &existing); err != nil {
				return errors.New("Runtime managed resource record is invalid")
			}
			if err := validateManagedResourceRecord(existing); err != nil {
				return err
			}
			count++
			total += existing.Size
			return nil
		}); err != nil {
			return err
		}
		if count >= MaxManagedResourceCount {
			return errors.New("Runtime managed resource count limit is reached")
		}
		if total+record.Size > MaxManagedResourcesBytes {
			return errors.New("Runtime managed resources exceed the total size limit")
		}
		if _, statErr := os.Lstat(path); errors.Is(statErr, os.ErrNotExist) {
			createdObject = true
		} else if statErr != nil {
			return fmt.Errorf("inspect Runtime managed resource: %w", statErr)
		}
		if err := writeOrVerifyImmutableSourceFile(path, content, digest); err != nil {
			return fmt.Errorf("write Runtime managed resource: %w", err)
		}
		if err := putJSON(resources, []byte(record.ID), record); err != nil {
			return err
		}
		_, err := advanceRevisionAndEvent(transaction, "resource.added", operationID, now)
		return err
	})
	if err != nil {
		if createdObject {
			_ = os.Remove(path)
		}
		return ManagedResourceRecord{}, err
	}
	return record, nil
}

func (s *Store) ManagedResources() ([]ManagedResource, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("Runtime state is not open")
	}
	var records []ManagedResourceRecord
	if err := s.db.View(func(transaction *bbolt.Tx) error {
		resources := transaction.Bucket(managedResourcesBucket)
		if resources == nil {
			return errors.New("Runtime managed resource state is unavailable")
		}
		return resources.ForEach(func(_, value []byte) error {
			var record ManagedResourceRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return errors.New("Runtime managed resource record is invalid")
			}
			if err := validateManagedResourceRecord(record); err != nil {
				return err
			}
			records = append(records, record)
			return nil
		})
	}); err != nil {
		return nil, err
	}
	sort.Slice(records, func(left, right int) bool {
		if records[left].CreatedAt.Equal(records[right].CreatedAt) {
			return records[left].ID < records[right].ID
		}
		return records[left].CreatedAt.Before(records[right].CreatedAt)
	})
	result := make([]ManagedResource, 0, len(records))
	for _, record := range records {
		path, err := s.managedResourceObjectPath(record.SHA256)
		if err != nil {
			return nil, err
		}
		body, err := readVerifiedSourceFile(path, record.SHA256)
		if err != nil {
			return nil, fmt.Errorf("verify Runtime managed resource %s: %w", record.ID, err)
		}
		if int64(len(body)) != record.Size || len(body) > MaxManagedResourceBytes {
			return nil, fmt.Errorf("verify Runtime managed resource %s: size mismatch", record.ID)
		}
		result = append(result, ManagedResource{ManagedResourceRecord: record, Path: path})
	}
	return result, nil
}

func (s *Store) ManagedResourceRoot() (string, error) {
	if s == nil || s.db == nil {
		return "", errors.New("Runtime state is not open")
	}
	return s.managedResourceRoot()
}

func managedResourceSummary(transaction *bbolt.Tx) (runtimeapi.ResourceStatus, error) {
	resources := transaction.Bucket(managedResourcesBucket)
	if resources == nil {
		return runtimeapi.ResourceStatus{}, errors.New("Runtime managed resource state is unavailable")
	}
	var result runtimeapi.ResourceStatus
	if err := resources.ForEach(func(_, value []byte) error {
		var record ManagedResourceRecord
		if err := json.Unmarshal(value, &record); err != nil {
			return errors.New("Runtime managed resource record is invalid")
		}
		if err := validateManagedResourceRecord(record); err != nil {
			return err
		}
		result.Items = append(result.Items, runtimeapi.ManagedResourceSummary{
			ID:        record.ID,
			Name:      record.Name,
			Kind:      record.Kind,
			Size:      record.Size,
			SHA256:    record.SHA256,
			CreatedAt: record.CreatedAt,
		})
		result.TotalBytes += record.Size
		return nil
	}); err != nil {
		return runtimeapi.ResourceStatus{}, err
	}
	sort.Slice(result.Items, func(left, right int) bool {
		if result.Items[left].CreatedAt.Equal(result.Items[right].CreatedAt) {
			return result.Items[left].ID < result.Items[right].ID
		}
		return result.Items[left].CreatedAt.Before(result.Items[right].CreatedAt)
	})
	result.Count = len(result.Items)
	return result, nil
}

func (s *Store) managedResourceObjectPath(digest string) (string, error) {
	if len(digest) != 64 {
		return "", errors.New("Runtime managed resource digest is invalid")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return "", errors.New("Runtime managed resource digest is invalid")
	}
	root, err := s.managedResourceRoot()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, strings.ToLower(digest)+".resource")
	if filepath.Dir(path) != root {
		return "", errors.New("Runtime managed resource path escaped its root")
	}
	return path, nil
}

func (s *Store) managedResourceRoot() (string, error) {
	root := filepath.Join(s.root, "managed-resources")
	if filepath.Dir(root) != s.root {
		return "", errors.New("Runtime managed resource root escaped the state root")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	linked, err := safepath.ContainsLink(root)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime managed resource root: %w", err)
	}
	if linked {
		return "", errors.New("Runtime managed resource root must not contain symbolic or reparse links")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	return root, nil
}

func validateManagedResourceRecord(record ManagedResourceRecord) error {
	if !validResourceID(record.ID) ||
		!validResourceName(record.Name) ||
		!validResourceKind(record.Kind) ||
		record.Size < 1 ||
		record.Size > MaxManagedResourceBytes ||
		len(record.SHA256) != 64 ||
		record.CreatedAt.IsZero() {
		return errors.New("Runtime managed resource record is invalid")
	}
	if _, err := hex.DecodeString(record.SHA256); err != nil {
		return errors.New("Runtime managed resource record is invalid")
	}
	return nil
}

func validResourceID(id string) bool {
	suffix, ok := strings.CutPrefix(id, "res_")
	if !ok || len(suffix) != 32 {
		return false
	}
	_, err := hex.DecodeString(suffix)
	return err == nil
}

func validResourceKind(kind string) bool {
	switch kind {
	case runtimeapi.ResourceKindProxyProvider,
		runtimeapi.ResourceKindRuleProvider,
		runtimeapi.ResourceKindCertificate,
		runtimeapi.ResourceKindPrivateKey:
		return true
	default:
		return false
	}
}

func validResourceName(name string) bool {
	if name == "" || len(name) > 128 || strings.HasPrefix(name, ".") || containsControl(name) {
		return false
	}
	for _, character := range []byte(name) {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			strings.ContainsRune("-_.:", rune(character)) {
			continue
		}
		return false
	}
	return true
}

func sha256Hex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
