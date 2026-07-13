package store

import (
	"encoding/binary"
	"encoding/json"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Source 是一个上游订阅源。
type Source struct {
	ID          int64    `json:"id"`
	Kind        string   `json:"kind"`
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	URL         string   `json:"url,omitempty"`
	UserAgent   string   `json:"user_agent,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Enabled     bool     `json:"enabled"`
	SortOrder   int      `json:"sort_order"`
	CreatedAt   string   `json:"created_at"` // RFC3339 UTC
	UpdatedAt   string   `json:"updated_at"`
}

const (
	SourceKindSubscription = "subscription"
	SourceKindManual       = "manual"
)

// Cache 是单个源的拉取缓存。
type Cache struct {
	SourceID      int64  `json:"source_id"`
	UserinfoJSON  string `json:"userinfo_json,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

type Store struct {
	db *bolt.DB
}

var bucketNames = []string{
	"settings", "sources", "source_cache", "meta",
	"nodes", "node_sets", "templates", "template_versions", "profiles", "profile_artifacts", "token_index",
}

func Open(path string) (*Store, error) {
	db, err := bolt.Open(path, 0600, &bolt.Options{Timeout: 5 * time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			if _, e := tx.CreateBucketIfNotExists([]byte(name)); e != nil {
				return e
			}
		}
		meta := tx.Bucket([]byte("meta"))
		if string(meta.Get([]byte("schema_version"))) != "2" {
			if e := migrateV2(tx); e != nil {
				return e
			}
			if e := meta.Put([]byte("schema_version"), []byte("2")); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrateV2(tx *bolt.Tx) error {
	sources := tx.Bucket([]byte("sources"))
	type sourceUpdate struct {
		key   []byte
		value Source
	}
	var sourceUpdates []sourceUpdate
	if err := sources.ForEach(func(k, raw []byte) error {
		var source Source
		if err := json.Unmarshal(raw, &source); err != nil {
			return err
		}
		if source.Kind != "" {
			return nil
		}
		source.Kind = SourceKindSubscription
		sourceUpdates = append(sourceUpdates, sourceUpdate{key: append([]byte(nil), k...), value: source})
		return nil
	}); err != nil {
		return err
	}
	for _, update := range sourceUpdates {
		if err := putJSON(sources, update.key, update.value); err != nil {
			return err
		}
	}
	// v2 intentionally stores normalized nodes only, never the upstream raw
	// subscription or a second serialized node snapshot.
	caches := tx.Bucket([]byte("source_cache"))
	type cacheUpdate struct {
		key   []byte
		value Cache
	}
	var cacheUpdates []cacheUpdate
	if err := caches.ForEach(func(k, raw []byte) error {
		var legacy struct {
			SourceID      int64  `json:"SourceID"`
			UserinfoJSON  string `json:"UserinfoJSON"`
			LastSuccessAt string `json:"LastSuccessAt"`
			LastError     string `json:"LastError"`
			UpdatedAt     string `json:"UpdatedAt"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return err
		}
		cacheUpdates = append(cacheUpdates, cacheUpdate{key: append([]byte(nil), k...), value: Cache{
			SourceID: legacy.SourceID, UserinfoJSON: legacy.UserinfoJSON,
			LastSuccessAt: legacy.LastSuccessAt, LastError: legacy.LastError, UpdatedAt: legacy.UpdatedAt,
		}})
		return nil
	}); err != nil {
		return err
	}
	for _, update := range cacheUpdates {
		if err := putJSON(caches, update.key, update.value); err != nil {
			return err
		}
	}
	settings := tx.Bucket([]byte("settings"))
	for _, key := range []string{"output_token", "default_format", "output_update_interval_hours"} {
		if err := settings.Delete([]byte(key)); err != nil {
			return err
		}
	}
	meta := tx.Bucket([]byte("meta"))
	var obsolete [][]byte
	if err := meta.ForEach(func(k, _ []byte) error {
		key := string(k)
		if key == "override" || len(key) >= 9 && key[:9] == "lastgood:" {
			obsolete = append(obsolete, append([]byte(nil), k...))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range obsolete {
		if err := meta.Delete(key); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error { return s.db.Close() }

func itob(v int64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(v))
	return b
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

func defStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func putJSON(b *bolt.Bucket, key []byte, value any) error {
	buf, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return b.Put(key, buf)
}
