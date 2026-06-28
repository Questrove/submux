package store

import (
	"encoding/binary"
	"time"

	bolt "go.etcd.io/bbolt"
)

// Source 是一个上游订阅源。
type Source struct {
	ID        int64
	Name      string
	URL       string
	UserAgent string
	Enabled   bool
	SortOrder int
	CreatedAt string // RFC3339 UTC
	UpdatedAt string
}

// Cache 是单个源的拉取缓存。
type Cache struct {
	SourceID      int64
	Raw           string
	NodesJSON     string
	UserinfoJSON  string
	LastSuccessAt string
	LastError     string
	UpdatedAt     string
}

type Store struct {
	db *bolt.DB
}

var bucketNames = []string{"settings", "sources", "source_cache", "meta"}

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
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
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
