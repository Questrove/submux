package store

import (
	"encoding/json"
	"path/filepath"
	"strings"
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

func TestOpenV6RemovesAbandonedRuntimeState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v5.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range append(append([]string(nil), bucketNames...), "runtime_bindings", "runtime_desired_states", "deployments", "integration_states") {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("5")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("template_versions")).Put(itob(1), []byte(`{"id":1,"runtime_contract":"mihomo-agent/v1"}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("runtime_observations")).Put(itob(1), []byte(`{"instance_id":1,"last_check_at":"old","integrations":{"docker":{}}}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("runtime_instances")).Put(itob(1), []byte(`{"id":1,"capabilities":["subscription.update","diagnostics.collect","subscription.manage"]}`)); err != nil {
			return err
		}
		jobs := tx.Bucket([]byte("agent_jobs"))
		if err := jobs.Put([]byte("old-update"), []byte(`{"id":"old-update","type":"update_subscription"}`)); err != nil {
			return err
		}
		if err := jobs.Put([]byte("old-diagnostics"), []byte(`{"id":"old-diagnostics","type":"collect_diagnostics"}`)); err != nil {
			return err
		}
		if err := jobs.Put([]byte("keep"), []byte(`{"id":"keep","type":"restart_core"}`)); err != nil {
			return err
		}
		audit := tx.Bucket([]byte("audit_events"))
		if err := audit.Put(itob(1), []byte(`{"id":1,"action":"job.update_subscription.succeeded"}`)); err != nil {
			return err
		}
		return audit.Put(itob(2), []byte(`{"id":2,"action":"job.restart_core.succeeded"}`))
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
		if got := string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))); got != "8" {
			t.Fatalf("schema version = %q, want 8", got)
		}
		for _, name := range []string{"runtime_bindings", "runtime_desired_states", "deployments", "integration_states"} {
			if tx.Bucket([]byte(name)) != nil {
				t.Fatalf("abandoned bucket %q still exists", name)
			}
		}
		for _, item := range []struct {
			bucket string
			key    []byte
			fields []string
		}{
			{bucket: "template_versions", key: itob(1), fields: []string{"runtime_contract"}},
			{bucket: "runtime_observations", key: itob(1), fields: []string{"last_check_at", "integrations"}},
		} {
			var value map[string]json.RawMessage
			if err := json.Unmarshal(tx.Bucket([]byte(item.bucket)).Get(item.key), &value); err != nil {
				return err
			}
			for _, field := range item.fields {
				if _, exists := value[field]; exists {
					t.Fatalf("field %q remained in %s", field, item.bucket)
				}
			}
		}
		jobs := tx.Bucket([]byte("agent_jobs"))
		if jobs.Get([]byte("old-update")) != nil || jobs.Get([]byte("old-diagnostics")) != nil || jobs.Get([]byte("keep")) == nil {
			t.Fatal("v6 job cleanup removed the wrong records")
		}
		var instance map[string]json.RawMessage
		if err := json.Unmarshal(tx.Bucket([]byte("runtime_instances")).Get(itob(1)), &instance); err != nil {
			return err
		}
		var capabilities []string
		if err := json.Unmarshal(instance["capabilities"], &capabilities); err != nil {
			return err
		}
		if len(capabilities) != 1 || capabilities[0] != "subscription.manage" {
			t.Fatalf("obsolete capabilities remain: %#v", capabilities)
		}
		audit := tx.Bucket([]byte("audit_events"))
		if audit.Get(itob(1)) != nil || audit.Get(itob(2)) == nil {
			t.Fatal("v6 audit cleanup removed the wrong records")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenV8RenamesAgentResourceProxyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range bucketNames {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("7")); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("runtime_observations")).Put(itob(1), []byte(`{"instance_id":1,"download_proxy_mode":"custom","download_proxy_url":"socks5://127.0.0.1:1080"}`)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte("runtime_instances")).Put(itob(1), []byte(`{"id":1,"capabilities":["mihomo.core.manage","mihomo.release.proxy"]}`)); err != nil {
			return err
		}
		return tx.Bucket([]byte("agent_jobs")).Put([]byte("proxy-job"), []byte(`{"id":"proxy-job","type":"configure_download_proxy","params":{"download_proxy":{"mode":"custom","url":"http://127.0.0.1:1080"}},"result":{"download_proxy":{"mode":"custom","url":"http://127.0.0.1:1080"}}}`))
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
		for _, item := range []struct {
			bucket string
			key    []byte
		}{
			{"runtime_observations", itob(1)},
			{"runtime_instances", itob(1)},
			{"agent_jobs", []byte("proxy-job")},
		} {
			raw := string(tx.Bucket([]byte(item.bucket)).Get(item.key))
			if strings.Contains(raw, "download_proxy") || strings.Contains(raw, "mihomo.release.proxy") {
				t.Fatalf("legacy proxy state remains in %s: %s", item.bucket, raw)
			}
			if !strings.Contains(raw, "resource_proxy") && item.bucket != "runtime_instances" {
				t.Fatalf("resource proxy state is missing from %s: %s", item.bucket, raw)
			}
			if item.bucket == "runtime_instances" && !strings.Contains(raw, "agent.resource.proxy") {
				t.Fatalf("resource proxy capability is missing: %s", raw)
			}
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
