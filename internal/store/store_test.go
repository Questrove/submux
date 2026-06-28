package store

import (
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOpenCreatesBuckets(t *testing.T) {
	s := newTestStore(t)
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, name := range []string{"settings", "sources", "source_cache", "meta"} {
			if tx.Bucket([]byte(name)) == nil {
				t.Fatalf("bucket %q missing", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("view: %v", err)
	}
}
