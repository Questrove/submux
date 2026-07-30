package runtimestate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/owneracl"
	"submux/internal/runtimeapi"
	"submux/internal/safepath"
)

const (
	DefaultImportTTL = 30 * time.Minute
	MaxImportBytes   = 8 << 20
)

var (
	ErrImportNotFound = errors.New("Runtime imported content was not found")
	ErrImportExpired  = errors.New("Runtime imported content has expired")
	ErrImportConsumed = errors.New("Runtime imported content was already consumed")
	ErrImportOwner    = errors.New("Runtime imported content belongs to another caller")
)

type importRecord struct {
	Content    runtimeapi.ImportContent `json:"content"`
	Caller     string                   `json:"caller"`
	Consumed   bool                     `json:"consumed"`
	ReservedBy string                   `json:"reserved_by,omitempty"`
}

func (s *Store) UploadImport(
	peer runtimeapi.PeerIdentity,
	contentType string,
	expectedSize int64,
	expectedSHA256 string,
	body []byte,
	now time.Time,
) (runtimeapi.ImportContent, error) {
	if s == nil || s.db == nil || s.root == "" {
		return runtimeapi.ImportContent{}, errors.New("Runtime state is not open")
	}
	if peer.Key() == "" {
		return runtimeapi.ImportContent{}, errors.New("Runtime import caller identity is unavailable")
	}
	if !allowedImportContentType(contentType) {
		return runtimeapi.ImportContent{}, errors.New("Runtime import content type is not supported")
	}
	if expectedSize < 0 || expectedSize != int64(len(body)) {
		return runtimeapi.ImportContent{}, errors.New("Runtime import size does not match its metadata")
	}
	maxBytes := maxImportBytes(contentType)
	if len(body) == 0 || len(body) > maxBytes {
		return runtimeapi.ImportContent{}, errors.New("Runtime import size is outside the allowed range")
	}
	digest := sha256.Sum256(body)
	actualDigest := hex.EncodeToString(digest[:])
	if len(expectedSHA256) != 64 || !strings.EqualFold(expectedSHA256, actualDigest) {
		return runtimeapi.ImportContent{}, errors.New("Runtime import SHA-256 does not match its metadata")
	}
	contentID, err := randomIdentifier("content_")
	if err != nil {
		return runtimeapi.ImportContent{}, err
	}
	path, err := s.importPath(contentID)
	if err != nil {
		return runtimeapi.ImportContent{}, err
	}
	if err := writeImmutableImport(path, body); err != nil {
		return runtimeapi.ImportContent{}, err
	}
	now = now.UTC()
	content := runtimeapi.ImportContent{
		ID:          contentID,
		ContentType: contentType,
		Size:        int64(len(body)),
		SHA256:      actualDigest,
		ExpiresAt:   now.Add(DefaultImportTTL),
	}
	record := importRecord{
		Content: content,
		Caller:  peer.Key(),
	}
	if err := s.db.Update(func(transaction *bbolt.Tx) error {
		imports := transaction.Bucket(importsBucket)
		if imports == nil {
			return errors.New("Runtime import state is unavailable")
		}
		return putJSON(imports, []byte(contentID), record)
	}); err != nil {
		_ = os.Remove(path)
		return runtimeapi.ImportContent{}, err
	}
	return content, nil
}

func (s *Store) ConsumeImport(contentID, caller, operationID string, now time.Time) ([]byte, runtimeapi.ImportContent, error) {
	if s == nil || s.db == nil {
		return nil, runtimeapi.ImportContent{}, errors.New("Runtime state is not open")
	}
	var claimed importRecord
	if err := s.db.View(func(transaction *bbolt.Tx) error {
		current, err := readImport(transaction.Bucket(importsBucket), contentID)
		if err != nil {
			return err
		}
		if err := validateImportForCaller(current, caller, operationID, now); err != nil {
			return err
		}
		claimed = current
		return nil
	}); err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	path, err := s.importPath(contentID)
	if err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	body, err := readVerifiedImport(path, claimed.Content)
	if err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	if err := s.db.Update(func(transaction *bbolt.Tx) error {
		imports := transaction.Bucket(importsBucket)
		current, err := readImport(imports, contentID)
		if err != nil {
			return err
		}
		if err := validateImportForCaller(current, caller, operationID, now); err != nil {
			return err
		}
		if current.Content != claimed.Content || current.Caller != claimed.Caller {
			return errors.New("Runtime import record changed while its content was being verified")
		}
		current.Consumed = true
		return putJSON(imports, []byte(contentID), current)
	}); err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	return body, claimed.Content, nil
}

func (s *Store) PeekImport(contentID, caller string, now time.Time) ([]byte, runtimeapi.ImportContent, error) {
	if s == nil || s.db == nil {
		return nil, runtimeapi.ImportContent{}, errors.New("Runtime state is not open")
	}
	var record importRecord
	if err := s.db.View(func(transaction *bbolt.Tx) error {
		var err error
		record, err = readImport(transaction.Bucket(importsBucket), contentID)
		return err
	}); err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	if err := validateImportForCaller(record, caller, "", now); err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	path, err := s.importPath(contentID)
	if err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	body, err := readVerifiedImport(path, record.Content)
	if err != nil {
		return nil, runtimeapi.ImportContent{}, err
	}
	return body, record.Content, nil
}

func (s *Store) RemoveImport(contentID string) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	path, err := s.importPath(contentID)
	if err != nil {
		return err
	}
	err = s.db.Update(func(transaction *bbolt.Tx) error {
		imports := transaction.Bucket(importsBucket)
		_, err := readImport(imports, contentID)
		if errors.Is(err, ErrImportNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		return imports.Delete([]byte(contentID))
	})
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove Runtime imported content: %w", err)
	}
	return nil
}

func (s *Store) GCExpiredImports(now time.Time) error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	now = now.UTC()
	var expired []string
	err := s.db.Update(func(transaction *bbolt.Tx) error {
		imports := transaction.Bucket(importsBucket)
		operations := transaction.Bucket(operationsBucket)
		if imports == nil || operations == nil {
			return errors.New("Runtime import state is unavailable")
		}
		var keys [][]byte
		if err := imports.ForEach(func(key, value []byte) error {
			var record importRecord
			if err := json.Unmarshal(value, &record); err != nil {
				return errors.New("Runtime import record is invalid")
			}
			remove := record.ReservedBy == "" && !now.Before(record.Content.ExpiresAt)
			if record.ReservedBy != "" {
				operation, err := readOperation(operations, record.ReservedBy)
				remove = errors.Is(err, ErrOperationNotFound) ||
					(err == nil && isTerminalOperationState(operation.State))
				if err != nil && !errors.Is(err, ErrOperationNotFound) {
					return err
				}
			}
			if remove {
				keys = append(keys, append([]byte(nil), key...))
				expired = append(expired, string(key))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range keys {
			if err := imports.Delete(key); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, contentID := range expired {
		path, err := s.importPath(contentID)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove expired Runtime import: %w", err)
		}
	}
	return nil
}

func (s *Store) safeImportRoot() (string, error) {
	root := filepath.Join(s.root, "imports")
	if filepath.Dir(root) != s.root {
		return "", errors.New("Runtime import root escaped the state root")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	linked, err := safepath.ContainsLink(root)
	if err != nil {
		return "", fmt.Errorf("inspect Runtime import root: %w", err)
	}
	if linked {
		return "", errors.New("Runtime import root must not contain symbolic or reparse links")
	}
	if err := os.Chmod(root, 0700); err != nil {
		return "", err
	}
	if err := owneracl.RestrictDirectory(root); err != nil {
		return "", fmt.Errorf("restrict Runtime import root permissions: %w", err)
	}
	return root, nil
}

func (s *Store) importPath(contentID string) (string, error) {
	fileName, err := importFileName(contentID)
	if err != nil {
		return "", err
	}
	root, err := s.safeImportRoot()
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, fileName)
	if filepath.Dir(path) != root {
		return "", errors.New("Runtime import path escaped the import root")
	}
	return path, nil
}

func readImport(bucket *bbolt.Bucket, contentID string) (importRecord, error) {
	if bucket == nil {
		return importRecord{}, errors.New("Runtime import state is unavailable")
	}
	value := bucket.Get([]byte(contentID))
	if value == nil {
		return importRecord{}, ErrImportNotFound
	}
	var record importRecord
	if err := json.Unmarshal(value, &record); err != nil {
		return importRecord{}, errors.New("Runtime import record is invalid")
	}
	if record.Content.ID != contentID || record.Caller == "" {
		return importRecord{}, errors.New("Runtime import record is invalid")
	}
	return record, nil
}

func validateImportForCaller(record importRecord, caller, operationID string, now time.Time) error {
	if record.Caller != caller {
		return ErrImportOwner
	}
	if record.Consumed {
		return ErrImportConsumed
	}
	if record.ReservedBy != "" && record.ReservedBy != operationID {
		return ErrImportConsumed
	}
	if !now.UTC().Before(record.Content.ExpiresAt) && record.ReservedBy == "" {
		return ErrImportExpired
	}
	return nil
}

func allowedImportContentType(contentType string) bool {
	switch strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0])) {
	case "application/yaml",
		"application/x-yaml",
		"text/yaml",
		runtimeapi.SourceDraftContentType,
		runtimeapi.ManagedResourceContentType,
		runtimeapi.ProductUpdateBundleContentType,
		runtimeapi.RuntimeBackupContentType:
		return true
	default:
		return false
	}
}

func maxImportBytes(contentType string) int {
	mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0])
	if strings.EqualFold(mediaType, runtimeapi.RuntimeBackupContentType) {
		return runtimeapi.RuntimeBackupMaxBytes
	}
	if strings.EqualFold(mediaType, runtimeapi.ProductUpdateBundleContentType) {
		return runtimeapi.RuntimeProductUpdateMaxBytes
	}
	return MaxImportBytes
}

func (s *Store) removeOrphanImportFiles() error {
	if s == nil || s.db == nil {
		return errors.New("Runtime state is not open")
	}
	root, err := s.safeImportRoot()
	if err != nil {
		return err
	}
	live := make(map[string]struct{})
	if err := s.db.View(func(transaction *bbolt.Tx) error {
		imports := transaction.Bucket(importsBucket)
		if imports == nil {
			return errors.New("Runtime import state is unavailable")
		}
		return imports.ForEach(func(key, _ []byte) error {
			contentID := string(key)
			if _, err := importFileName(contentID); err != nil {
				return errors.New("Runtime import record has an invalid content ID")
			}
			live[contentID] = struct{}{}
			return nil
		})
	}); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		contentID, ok := strings.CutSuffix(entry.Name(), ".content")
		if !ok {
			continue
		}
		if _, err := importFileName(contentID); err != nil {
			continue
		}
		if _, exists := live[contentID]; exists {
			continue
		}
		target := filepath.Join(root, entry.Name())
		info, err := os.Lstat(target)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("Runtime import root contains an unmanaged orphan entry")
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove orphaned Runtime imported content: %w", err)
		}
	}
	return nil
}

func writeImmutableImport(path string, body []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := owneracl.RestrictFile(path); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("restrict Runtime imported content permissions: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return fmt.Errorf("restrict Runtime imported content mode: %w", err)
	}
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func importFileName(contentID string) (string, error) {
	suffix, ok := strings.CutPrefix(contentID, "content_")
	if !ok || len(suffix) != 32 {
		return "", errors.New("Runtime import content ID is invalid")
	}
	if _, err := hex.DecodeString(suffix); err != nil {
		return "", errors.New("Runtime import content ID is invalid")
	}
	return contentID + ".content", nil
}

func readVerifiedImport(path string, content runtimeapi.ImportContent) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Runtime imported content: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("Runtime imported content must be a regular non-linked file")
	}
	linked, err := safepath.ContainsLink(path)
	if err != nil {
		return nil, fmt.Errorf("inspect Runtime imported content path: %w", err)
	}
	if linked {
		return nil, errors.New("Runtime imported content path must not contain symbolic or reparse links")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read Runtime imported content: %w", err)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect opened Runtime imported content: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		return nil, errors.New("Runtime imported content changed while it was being opened")
	}
	maxBytes := maxImportBytes(content.ContentType)
	body, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)+1))
	if err != nil {
		return nil, fmt.Errorf("read Runtime imported content: %w", err)
	}
	digest := sha256.Sum256(body)
	if int64(len(body)) != content.Size ||
		!strings.EqualFold(hex.EncodeToString(digest[:]), content.SHA256) {
		return nil, errors.New("Runtime imported content integrity check failed")
	}
	return body, nil
}
