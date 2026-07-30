package runtimestate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.etcd.io/bbolt"

	"submux/internal/owneracl"
	"submux/internal/safepath"
)

type DatabaseBackup struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (s *Store) BackupDatabase(name string) (DatabaseBackup, error) {
	if s == nil || s.db == nil {
		return DatabaseBackup{}, errors.New("Runtime state is not open")
	}
	if name == "" || !filepath.IsAbs(name) {
		return DatabaseBackup{}, errors.New("Runtime database backup path must be fixed and absolute")
	}
	name = filepath.Clean(name)
	parent := filepath.Dir(name)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return DatabaseBackup{}, errors.New("Runtime database backup parent is invalid")
	}
	linked, err := safepath.ContainsLink(parent)
	if err != nil || linked {
		return DatabaseBackup{}, errors.New("Runtime database backup path must not contain symbolic or reparse links")
	}
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return DatabaseBackup{}, fmt.Errorf("create Runtime database backup: %w", err)
	}
	success := false
	defer func() {
		_ = file.Close()
		if !success {
			_ = os.Remove(name)
		}
	}()
	if err := owneracl.RestrictFile(name); err != nil {
		return DatabaseBackup{}, err
	}
	if err := file.Chmod(0600); err != nil {
		return DatabaseBackup{}, err
	}
	var size int64
	if err := s.db.View(func(transaction *bbolt.Tx) error {
		var err error
		size, err = transaction.WriteTo(file)
		return err
	}); err != nil {
		return DatabaseBackup{}, fmt.Errorf("copy Runtime database rollback point: %w", err)
	}
	if size <= 0 {
		return DatabaseBackup{}, errors.New("Runtime database rollback point is empty")
	}
	if err := file.Sync(); err != nil {
		return DatabaseBackup{}, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		return DatabaseBackup{}, err
	}
	hash := sha256.New()
	copied, err := io.Copy(hash, file)
	if err != nil {
		return DatabaseBackup{}, err
	}
	if copied != size {
		return DatabaseBackup{}, errors.New("Runtime database rollback point changed while hashing")
	}
	if err := file.Close(); err != nil {
		return DatabaseBackup{}, err
	}
	if err := syncDatabaseBackupParent(parent); err != nil {
		return DatabaseBackup{}, fmt.Errorf("sync Runtime database backup directory: %w", err)
	}
	success = true
	return DatabaseBackup{
		Path:   name,
		Size:   size,
		SHA256: hex.EncodeToString(hash.Sum(nil)),
	}, nil
}

func (s *Store) DatabasePath() (string, error) {
	if s == nil || s.db == nil || s.root == "" {
		return "", errors.New("Runtime state is not open")
	}
	return filepath.Join(s.root, "runtime.db"), nil
}
