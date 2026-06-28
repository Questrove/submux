package store

import bolt "go.etcd.io/bbolt"

func (s *Store) GetOverride() (string, error) {
	var v string
	err := s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte("meta")).Get([]byte("override")); b != nil {
			v = string(b)
		}
		return nil
	})
	return v, err
}

func (s *Store) SetOverride(content string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("override"), []byte(content))
	})
}

func (s *Store) GetLastGood(format string) ([]byte, error) {
	var out []byte
	err := s.db.View(func(tx *bolt.Tx) error {
		if v := tx.Bucket([]byte("meta")).Get([]byte("lastgood:" + format)); v != nil {
			out = append([]byte(nil), v...) // bbolt 值仅事务内有效,需复制
		}
		return nil
	})
	return out, err
}

func (s *Store) SetLastGood(format string, body []byte) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("meta")).Put([]byte("lastgood:"+format), body)
	})
}
