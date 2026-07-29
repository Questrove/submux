package store

import (
	"encoding/json"
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

func TestOpenV9RemovesRemoteRuntimeState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	legacyBuckets := []string{
		"runtime_instances", "runtime_observations", "agent_jobs", "audit_events", "agent_enrollments",
		"runtime_bindings", "runtime_desired_states", "deployments", "integration_states",
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range append(append([]string(nil), bucketNames...), legacyBuckets...) {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("8")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("template_versions")).Put(itob(1), []byte(`{"id":1,"content":"kept","runtime_contract":"mihomo-agent/v1"}`)); err != nil {
			return err
		}
		for _, name := range legacyBuckets {
			if err := tx.Bucket([]byte(name)).Put([]byte("legacy"), []byte(`{"id":"legacy"}`)); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.db.View(func(tx *bolt.Tx) error {
		if got := string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))); got != "10" {
			t.Fatalf("schema version = %q, want 10", got)
		}
		for _, name := range legacyBuckets {
			if tx.Bucket([]byte(name)) != nil {
				t.Fatalf("obsolete remote-runtime bucket %q still exists", name)
			}
		}
		var version map[string]json.RawMessage
		if err := json.Unmarshal(tx.Bucket([]byte("template_versions")).Get(itob(1)), &version); err != nil {
			return err
		}
		if _, exists := version["runtime_contract"]; exists {
			t.Fatal("obsolete runtime contract remains")
		}
		if string(version["content"]) != `"kept"` {
			t.Fatalf("template content changed: %s", version["content"])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCreatesBuckets(t *testing.T) {
	s := newTestStore(t)
	err := s.db.View(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
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
