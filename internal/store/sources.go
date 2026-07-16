package store

import (
	"encoding/json"
	"fmt"
	"sort"

	bolt "go.etcd.io/bbolt"
)

const DefaultManualSourceName = "自建节点"

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
		src.Kind = defStr(src.Kind, SourceKindSubscription)
		normalizeSourceLifecycle(&src)
		src.Enabled = true // M1:新建源默认启用;停用走 SetSourceEnabled
		if src.Kind == SourceKindSubscription {
			src.UserAgent = defStr(src.UserAgent, "clash-verge/v2.0.0")
		} else {
			src.URL = ""
			src.UserAgent = ""
		}
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

// EnsureDefaultManualSource returns the built-in destination used when the
// user imports nodes without choosing or creating a source. An existing manual
// source with the default name is adopted so pre-release databases do not gain
// a duplicate group.
func (s *Store) EnsureDefaultManualSource() (int64, error) {
	var id int64
	err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("sources"))
		var builtin, named *Source
		if err := b.ForEach(func(_, raw []byte) error {
			var source Source
			if err := json.Unmarshal(raw, &source); err != nil {
				return err
			}
			if source.Kind != SourceKindManual {
				return nil
			}
			if source.Builtin && builtin == nil {
				copy := source
				builtin = &copy
			}
			if source.Name == DefaultManualSourceName && named == nil {
				copy := source
				named = &copy
			}
			return nil
		}); err != nil {
			return err
		}
		if builtin != nil {
			id = builtin.ID
			if builtin.Enabled && builtin.Name == DefaultManualSourceName {
				return nil
			}
			builtin.Name, builtin.Enabled, builtin.UpdatedAt = DefaultManualSourceName, true, nowRFC3339()
			return putJSON(b, itob(id), *builtin)
		}
		if named != nil {
			id = named.ID
			named.Builtin, named.Enabled, named.UpdatedAt = true, true, nowRFC3339()
			return putJSON(b, itob(id), *named)
		}
		seq, err := b.NextSequence()
		if err != nil {
			return err
		}
		id = int64(seq)
		now := nowRFC3339()
		return putJSON(b, itob(id), Source{
			ID: id, Kind: SourceKindManual, Builtin: true, Name: DefaultManualSourceName,
			Enabled: true, CreatedAt: now, UpdatedAt: now,
		})
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
	normalizeSourceLifecycle(&src)
	return src, nil
}

func (s *Store) ListSources() ([]Source, error)        { return s.listSources(false) }
func (s *Store) ListEnabledSources() ([]Source, error) { return s.listSources(true) }

func (s *Store) listSources(enabledOnly bool) ([]Source, error) {
	out := make([]Source, 0)
	err := s.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("sources")).ForEach(func(_, v []byte) error {
			var src Source
			if e := json.Unmarshal(v, &src); e != nil {
				return e
			}
			if enabledOnly && !src.Enabled {
				return nil
			}
			normalizeSourceLifecycle(&src)
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
		current := b.Get(itob(src.ID))
		if current == nil {
			return fmt.Errorf("no source with id %d", src.ID)
		}
		var old Source
		if err := json.Unmarshal(current, &old); err != nil {
			return err
		}
		src.CreatedAt = old.CreatedAt
		src.Builtin = old.Builtin
		src.Kind = defStr(src.Kind, old.Kind)
		src.Kind = defStr(src.Kind, SourceKindSubscription)
		normalizeSourceLifecycle(&src)
		if src.Kind == SourceKindSubscription {
			src.UserAgent = defStr(src.UserAgent, "clash-verge/v2.0.0")
		} else {
			src.URL = ""
			src.UserAgent = ""
		}
		src.UpdatedAt = nowRFC3339()
		buf, e := json.Marshal(src)
		if e != nil {
			return e
		}
		return b.Put(itob(src.ID), buf)
	})
}

func normalizeSourceLifecycle(src *Source) {
	if src.Kind == "" {
		src.Kind = SourceKindSubscription
	}
	if src.Kind != SourceKindSubscription {
		src.LifecyclePolicy = ""
		src.WarnBeforeDays = 0
		src.TrustNodeNotices = false
		return
	}
	if src.LifecyclePolicy != LifecycleStrict {
		src.LifecyclePolicy = LifecycleContinuity
	}
	if src.WarnBeforeDays <= 0 {
		src.WarnBeforeDays = 7
	}
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
		if e := tx.Bucket([]byte("source_cache")).Delete(itob(id)); e != nil {
			return e
		}
		if e := tx.Bucket([]byte("meta")).Delete([]byte(fmt.Sprintf("lifecycle_state:%d", id))); e != nil {
			return e
		}
		events := tx.Bucket([]byte("lifecycle_events"))
		var eventKeys [][]byte
		if e := events.ForEach(func(k, raw []byte) error {
			var event LifecycleEvent
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			if event.SourceID == id {
				eventKeys = append(eventKeys, append([]byte(nil), k...))
			}
			return nil
		}); e != nil {
			return e
		}
		for _, key := range eventKeys {
			if e := events.Delete(key); e != nil {
				return e
			}
		}
		// 级联删除该来源的规范化节点。
		nodes := tx.Bucket([]byte("nodes"))
		var keys [][]byte
		if e := nodes.ForEach(func(k, v []byte) error {
			var node NodeRecord
			if err := json.Unmarshal(v, &node); err != nil {
				return err
			}
			if node.SourceID == id {
				keys = append(keys, append([]byte(nil), k...))
			}
			return nil
		}); e != nil {
			return e
		}
		for _, key := range keys {
			if e := nodes.Delete(key); e != nil {
				return e
			}
		}
		return nil
	})
}
