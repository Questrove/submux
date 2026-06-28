package store

import (
	"strconv"

	bolt "go.etcd.io/bbolt"
)

func (s *Store) SetSetting(key, value string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("settings")).Put([]byte(key), []byte(value))
	})
}

// GetSetting 返回 key 的值;不存在时返回 ("", nil)。
func (s *Store) GetSetting(key string) (string, error) {
	var v string
	err := s.db.View(func(tx *bolt.Tx) error {
		if b := tx.Bucket([]byte("settings")).Get([]byte(key)); b != nil {
			v = string(b) // string() 复制,事务外安全
		}
		return nil
	})
	return v, err
}

// GetSettingInt 返回 key 的整数值;不存在或解析失败时返回 def。
func (s *Store) GetSettingInt(key string, def int) (int, error) {
	v, err := s.GetSetting(key)
	if err != nil {
		return def, err
	}
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def, nil
	}
	return n, nil
}
