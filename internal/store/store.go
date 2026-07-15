package store

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Source 是一个上游订阅源。
type Source struct {
	ID               int64    `json:"id"`
	Kind             string   `json:"kind"`
	Name             string   `json:"name"`
	Description      string   `json:"description,omitempty"`
	URL              string   `json:"url,omitempty"`
	UserAgent        string   `json:"user_agent,omitempty"`
	Tags             []string `json:"tags,omitempty"`
	Enabled          bool     `json:"enabled"`
	SortOrder        int      `json:"sort_order"`
	LifecyclePolicy  string   `json:"lifecycle_policy,omitempty"`
	WarnBeforeDays   int      `json:"warn_before_days,omitempty"`
	TrustNodeNotices bool     `json:"trust_node_notices,omitempty"`
	CreatedAt        string   `json:"created_at"` // RFC3339 UTC
	UpdatedAt        string   `json:"updated_at"`
}

const (
	SourceKindSubscription = "subscription"
	SourceKindManual       = "manual"
	LifecycleContinuity    = "continuity"
	LifecycleStrict        = "strict"
)

// SubscriptionMetadata is the last successfully observed entitlement data.
// Provenance contains one entry per known field (header, node_name or manual).
type SubscriptionMetadata struct {
	Upload     int64             `json:"upload,omitempty"`
	Download   int64             `json:"download,omitempty"`
	Total      int64             `json:"total,omitempty"`
	Remaining  int64             `json:"remaining,omitempty"`
	ExpiresAt  string            `json:"expires_at,omitempty"`
	ResetAt    string            `json:"reset_at,omitempty"`
	ObservedAt string            `json:"observed_at,omitempty"`
	Provenance map[string]string `json:"provenance,omitempty"`
	Conflicts  []string          `json:"conflicts,omitempty"`
	Stale      bool              `json:"stale,omitempty"`
}

type NodeNotice struct {
	Type       string `json:"type"`
	RawText    string `json:"raw_text"`
	Value      int64  `json:"value,omitempty"`
	TotalValue int64  `json:"total_value,omitempty"`
	TextValue  string `json:"text_value,omitempty"`
	Unit       string `json:"unit,omitempty"`
	Confidence string `json:"confidence"`
}

// Cache 是单个源的拉取缓存。
type Cache struct {
	SourceID      int64                `json:"source_id"`
	UserinfoJSON  string               `json:"userinfo_json,omitempty"`
	Metadata      SubscriptionMetadata `json:"metadata,omitempty"`
	LastSuccessAt string               `json:"last_success_at,omitempty"`
	LastError     string               `json:"last_error,omitempty"`
	UpdatedAt     string               `json:"updated_at"`
}

type Store struct {
	db *bolt.DB
}

var bucketNames = []string{
	"settings", "sources", "source_cache", "meta",
	"nodes", "node_sets", "templates", "template_versions", "profiles", "profile_artifacts", "token_index", "lifecycle_events",
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
		version := string(meta.Get([]byte("schema_version")))
		if version == "" || version == "1" {
			if e := migrateV2(tx); e != nil {
				return e
			}
			version = "2"
		}
		if version == "2" {
			if e := migrateV3(tx); e != nil {
				return e
			}
			version = "3"
		}
		if version != "3" {
			return fmt.Errorf("unsupported database schema version %q", version)
		}
		if string(meta.Get([]byte("schema_version"))) != version {
			if e := meta.Put([]byte("schema_version"), []byte(version)); e != nil {
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

func migrateV3(tx *bolt.Tx) error {
	b := tx.Bucket([]byte("sources"))
	type update struct {
		key   []byte
		value Source
	}
	var updates []update
	if err := b.ForEach(func(k, raw []byte) error {
		var source Source
		if err := json.Unmarshal(raw, &source); err != nil {
			return err
		}
		before := source
		normalizeSourceLifecycle(&source)
		if source.LifecyclePolicy != before.LifecyclePolicy || source.WarnBeforeDays != before.WarnBeforeDays {
			updates = append(updates, update{append([]byte(nil), k...), source})
		}
		return nil
	}); err != nil {
		return err
	}
	for _, item := range updates {
		if err := putJSON(b, item.key, item.value); err != nil {
			return err
		}
	}
	caches := tx.Bucket([]byte("source_cache"))
	type cacheUpdate struct {
		key   []byte
		value Cache
	}
	var cacheUpdates []cacheUpdate
	if err := caches.ForEach(func(k, raw []byte) error {
		var cache Cache
		if err := json.Unmarshal(raw, &cache); err != nil {
			return err
		}
		if len(cache.Metadata.Provenance) > 0 || cache.UserinfoJSON == "" {
			return nil
		}
		var legacyFields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cache.UserinfoJSON), &legacyFields); err != nil {
			return nil
		}
		values := make(map[string]int64)
		for key, rawValue := range legacyFields {
			normalized := strings.ToLower(key)
			if normalized != "upload" && normalized != "download" && normalized != "total" && normalized != "expire" {
				continue
			}
			var value int64
			if err := json.Unmarshal(rawValue, &value); err == nil && value >= 0 {
				values[normalized] = value
			}
		}
		if len(values) == 0 {
			return nil
		}
		cache.Metadata = SubscriptionMetadata{ObservedAt: cache.LastSuccessAt, Provenance: map[string]string{}}
		if value, ok := values["upload"]; ok {
			cache.Metadata.Upload = value
			cache.Metadata.Provenance["upload"] = "header"
		}
		if value, ok := values["download"]; ok {
			cache.Metadata.Download = value
			cache.Metadata.Provenance["download"] = "header"
		}
		if value, ok := values["total"]; ok {
			cache.Metadata.Total = value
			cache.Metadata.Provenance["total"] = "header"
		}
		if _, uploadOK := values["upload"]; uploadOK {
			if _, downloadOK := values["download"]; downloadOK {
				if total, totalOK := values["total"]; totalOK {
					cache.Metadata.Remaining = total - values["upload"] - values["download"]
					cache.Metadata.Provenance["remaining"] = "header"
				}
			}
		}
		if expire := values["expire"]; expire > 0 {
			cache.Metadata.ExpiresAt = time.Unix(expire, 0).UTC().Format(time.RFC3339)
			cache.Metadata.Provenance["expires_at"] = "header"
		}
		cacheUpdates = append(cacheUpdates, cacheUpdate{append([]byte(nil), k...), cache})
		return nil
	}); err != nil {
		return err
	}
	for _, item := range cacheUpdates {
		if err := putJSON(caches, item.key, item.value); err != nil {
			return err
		}
	}
	return nil
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
