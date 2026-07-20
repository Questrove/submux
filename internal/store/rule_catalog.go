package store

import (
	"fmt"

	bolt "go.etcd.io/bbolt"
)

func (s *Store) PutRuleCatalogSnapshot(commit string, raw []byte) error {
	if commit == "" || len(raw) == 0 {
		return fmt.Errorf("rule catalog commit and snapshot are required")
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("rule_catalog_snapshots")).Put([]byte(commit), append([]byte(nil), raw...))
	})
}

func (s *Store) GetRuleCatalogSnapshot(commit string) ([]byte, error) {
	var result []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte("rule_catalog_snapshots")).Get([]byte(commit))
		if raw == nil {
			return fmt.Errorf("no rule catalog snapshot for commit %q", commit)
		}
		result = append([]byte(nil), raw...)
		return nil
	})
	return result, err
}
