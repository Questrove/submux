package store

import (
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

func (s *Store) CreateSource(src Source) (int64, error) {
	var id int64
	now := nowRFC3339()
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("sources"))
		seq, e := b.NextSequence()
		if e != nil {
			return e
		}
		id = int64(seq)
		src.ID = id
		src.Enabled = true // M1:新建源默认启用;停用走 SetSourceEnabled
		src.UserAgent = defStr(src.UserAgent, "clash-verge/v2.0.0")
		src.CreatedAt = now
		src.UpdatedAt = now
		buf, e := json.Marshal(src)
		if e != nil {
			return e
		}
		return b.Put(itob(id), buf)
	})
	return id, err
}

func (s *Store) GetSource(id int64) (Source, error) {
	var src Source
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte("sources")).Get(itob(id))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &src)
	})
	if err != nil {
		return Source{}, err
	}
	if !found {
		return Source{}, fmt.Errorf("no source with id %d", id)
	}
	return src, nil
}

func (s *Store) ListSources() ([]Source, error)        { return s.listSources(false) }
func (s *Store) ListEnabledSources() ([]Source, error) { return s.listSources(true) }

func (s *Store) listSources(enabledOnly bool) ([]Source, error) {
	var out []Source
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("sources")).ForEach(func(_, v []byte) error {
			var src Source
			if e := json.Unmarshal(v, &src); e != nil {
				return e
			}
			if enabledOnly && !src.Enabled {
				return nil
			}
			out = append(out, src)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SortOrder != out[j].SortOrder {
			return out[i].SortOrder < out[j].SortOrder
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Store) UpdateSource(src Source) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("sources"))
		if b.Get(itob(src.ID)) == nil {
			return fmt.Errorf("no source with id %d", src.ID)
		}
		src.UserAgent = defStr(src.UserAgent, "clash-verge/v2.0.0")
		src.UpdatedAt = nowRFC3339()
		buf, e := json.Marshal(src)
		if e != nil {
			return e
		}
		return b.Put(itob(src.ID), buf)
	})
}

func (s *Store) SetSourceEnabled(id int64, enabled bool) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("sources"))
		v := b.Get(itob(id))
		if v == nil {
			return fmt.Errorf("no source with id %d", id)
		}
		var src Source
		if e := json.Unmarshal(v, &src); e != nil {
			return e
		}
		src.Enabled = enabled
		src.UpdatedAt = nowRFC3339()
		buf, e := json.Marshal(src)
		if e != nil {
			return e
		}
		return b.Put(itob(id), buf)
	})
}

func (s *Store) DeleteSource(id int64) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("sources"))
		if b.Get(itob(id)) == nil {
			return fmt.Errorf("no source with id %d", id)
		}
		if e := b.Delete(itob(id)); e != nil {
			return e
		}
		// 级联删除该源缓存
		return tx.Bucket([]byte("source_cache")).Delete(itob(id))
	})
}
