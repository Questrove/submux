package store

import (
	"encoding/json"
	"fmt"

	bolt "go.etcd.io/bbolt"
)

func (s *Store) UpsertCacheSuccess(sourceID int64, userinfoJSON string) error {
	now := nowRFC3339()
	c := Cache{
		SourceID: sourceID, UserinfoJSON: userinfoJSON,
		LastSuccessAt: now, LastError: "", UpdatedAt: now,
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		buf, e := json.Marshal(c)
		if e != nil {
			return e
		}
		return tx.Bucket([]byte("source_cache")).Put(itob(sourceID), buf)
	})
}

// UpsertCacheError 只写 last_error + updated_at，保留上次成功时间与流量元数据。
func (s *Store) UpsertCacheError(sourceID int64, errMsg string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("source_cache"))
		var c Cache
		if v := b.Get(itob(sourceID)); v != nil {
			if e := json.Unmarshal(v, &c); e != nil {
				return e
			}
		}
		c.SourceID = sourceID
		c.LastError = errMsg
		c.UpdatedAt = nowRFC3339()
		buf, e := json.Marshal(c)
		if e != nil {
			return e
		}
		return b.Put(itob(sourceID), buf)
	})
}

// SetCacheFetchResult records which route was used without replacing the
// last-good subscription metadata or node snapshot.
func (s *Store) SetCacheFetchResult(sourceID int64, successRoute, directError, proxyError string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("source_cache"))
		var c Cache
		if raw := b.Get(itob(sourceID)); raw != nil {
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
		}
		c.SourceID = sourceID
		if successRoute != "" {
			c.LastSuccessRoute = successRoute
		}
		c.LastDirectError = directError
		c.LastProxyError = proxyError
		c.UpdatedAt = nowRFC3339()
		return putJSON(b, itob(sourceID), c)
	})
}

func (s *Store) GetCache(sourceID int64) (Cache, error) {
	var c Cache
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("source_cache")).Get(itob(sourceID))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &c)
	})
	if err != nil {
		return Cache{}, err
	}
	if !found {
		return Cache{}, fmt.Errorf("no cache for source %d", sourceID)
	}
	return c, nil
}
