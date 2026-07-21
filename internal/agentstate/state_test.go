package agentstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"submux/internal/agentproto"
)

func TestOpenRequiresAbsoluteRegularNonLinkedStatePath(t *testing.T) {
	if _, err := Open("agent.db"); err == nil {
		t.Fatal("relative Agent state path was accepted")
	}
	root := t.TempDir()
	realPath := filepath.Join(root, "real.db")
	if err := os.WriteFile(realPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(root, "linked.db")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Skipf("symbolic links are unavailable: %v", err)
	}
	if _, err := Open(linkPath); err == nil {
		t.Fatal("symbolic-link Agent state database was accepted")
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "agent.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestIdentityCannotBeSilentlyReplaced(t *testing.T) {
	st := testStore(t)
	identity := Identity{ServerURL: "https://submux.test", InstanceID: 1, PublicKey: "public", PrivateKey: "private", EnrolledAt: time.Now().Format(time.RFC3339)}
	if err := st.SaveIdentity(identity); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveIdentity(identity); err == nil {
		t.Fatal("device identity was silently replaced")
	}
}

func TestOpenRemovesAbandonedRuntimeState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketConfig).Put(keyRuntime, []byte(`{"core_status":"stopped","binding_id":7,"artifact_etag":"old","last_check_at":"old","integrations":{"docker":{}}}`)); err != nil {
			return err
		}
		jobs := tx.Bucket(bucketJobs)
		if err := jobs.Put([]byte("old"), []byte(`{"job":{"id":"old","type":"update_subscription"}}`)); err != nil {
			return err
		}
		if err := jobs.Put([]byte("keep"), []byte(`{"job":{"id":"keep","type":"restart_core"}}`)); err != nil {
			return err
		}
		audits := tx.Bucket(bucketAudits)
		if err := audits.Put([]byte("old"), []byte(`{"id":"old","action":"subscription.update"}`)); err != nil {
			return err
		}
		return audits.Put([]byte("keep"), []byte(`{"id":"keep","action":"subscription.rollback"}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketConfig).Get(keyRuntime)
		for _, removed := range []string{"binding_id", "artifact_etag", "last_check_at", "integrations"} {
			if strings.Contains(string(raw), removed) {
				t.Fatalf("abandoned runtime field %q remains: %s", removed, raw)
			}
		}
		if tx.Bucket(bucketJobs).Get([]byte("old")) != nil || tx.Bucket(bucketJobs).Get([]byte("keep")) == nil {
			t.Fatal("Agent job cleanup removed the wrong records")
		}
		if tx.Bucket(bucketAudits).Get([]byte("old")) != nil || tx.Bucket(bucketAudits).Get([]byte("keep")) == nil {
			t.Fatal("Agent audit cleanup removed the wrong records")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRenamesLocalResourceProxyState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		if err := tx.Bucket(bucketConfig).Put(keyRuntime, []byte(`{"core_status":"stopped","download_proxy_mode":"custom","download_proxy_url":"socks5://127.0.0.1:1080"}`)); err != nil {
			return err
		}
		return tx.Bucket(bucketJobs).Put([]byte("proxy-job"), []byte(`{"job":{"id":"proxy-job","type":"configure_download_proxy","params":{"download_proxy":{"mode":"custom","url":"socks5://127.0.0.1:1080"}}},"status":"succeeded","result":{"download_proxy":{"mode":"custom","url":"socks5://127.0.0.1:1080"}}}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runtimeState, err := st.Runtime()
	if err != nil || runtimeState.ResourceProxyMode != agentproto.ResourceProxyCustom || runtimeState.ResourceProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("legacy runtime proxy was not migrated: state=%+v err=%v", runtimeState, err)
	}
	jobs, err := st.UnreportedJobs()
	if err != nil || len(jobs) != 1 || jobs[0].Job.Type != agentproto.JobConfigureResourceProxy || strings.Contains(string(jobs[0].Job.Params), "download_proxy") || strings.Contains(string(jobs[0].Result), "download_proxy") {
		t.Fatalf("legacy proxy job was not migrated: jobs=%+v err=%v", jobs, err)
	}
}

func TestRuntimeMigratesLegacyRunningCoreToAutoStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(path, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketConfig).Put(keyRuntime, []byte(`{"core_status":"running","applied_revision":"verified-revision"}`))
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	runtimeState, err := st.Runtime()
	if err != nil {
		t.Fatal(err)
	}
	if !runtimeState.CoreAutoStart {
		t.Fatalf("legacy running core did not enable startup recovery: %#v", runtimeState)
	}
}

func TestInterruptedAndDuplicateJobsAreNeverReplayed(t *testing.T) {
	st := testStore(t)
	job := agentproto.Job{ID: "job", ProtocolVersion: 1, InstanceID: 1, Type: agentproto.JobRestartCore, Status: agentproto.JobQueued, ActorType: agentproto.ActorAdminSession, RequestID: "request", Deadline: time.Now().Add(time.Minute).Format(time.RFC3339), Params: json.RawMessage(`{}`)}
	started, first, err := st.BeginJob(job)
	if err != nil || !first || started.Status != agentproto.JobRunning {
		t.Fatalf("begin job: %#v, %v, %v", started, first, err)
	}
	recovered, err := st.RecoverInterruptedJobs()
	if err != nil || len(recovered) != 1 || recovered[0].Status != agentproto.JobOutcomeUnknown {
		t.Fatalf("recover: %#v, %v", recovered, err)
	}
	duplicate, execute, err := st.BeginJob(job)
	if err != nil || execute || duplicate.Status != agentproto.JobOutcomeUnknown {
		t.Fatalf("duplicate was replayable: %#v, %v, %v", duplicate, execute, err)
	}
	unreported, _ := st.UnreportedJobs()
	if len(unreported) != 1 {
		t.Fatalf("recovered result was not pending report: %#v", unreported)
	}
	if err := st.MarkJobReported(job.ID); err != nil {
		t.Fatal(err)
	}
	unreported, _ = st.UnreportedJobs()
	if len(unreported) != 0 {
		t.Fatalf("reported job remained pending: %#v", unreported)
	}
}

func TestLocalAuditPersistsUntilReportedAndEnrollmentCanBeCleared(t *testing.T) {
	st := testStore(t)
	identity := Identity{ServerURL: "https://submux.test", InstanceID: 1, PublicKey: "public", PrivateKey: "private", EnrolledAt: time.Now().Format(time.RFC3339)}
	if err := st.SaveIdentity(identity); err != nil {
		t.Fatal(err)
	}
	audit := LocalAudit{ID: "audit-1", RequestID: "request-1", Action: "mihomo.restart", Result: "succeeded"}
	if err := st.AddLocalAudit(audit); err != nil {
		t.Fatal(err)
	}
	values, err := st.UnreportedLocalAudits()
	if err != nil || len(values) != 1 || values[0].RequestID != audit.RequestID {
		t.Fatalf("unexpected local audits: %#v, %v", values, err)
	}
	if err := st.MarkLocalAuditReported(audit.ID); err != nil {
		t.Fatal(err)
	}
	values, _ = st.UnreportedLocalAudits()
	if len(values) != 0 {
		t.Fatalf("reported local audit remained pending: %#v", values)
	}
	if err := st.AddLocalAudit(LocalAudit{ID: "audit-2", RequestID: "request-2", Action: "subscription.rollback", Result: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ClearEnrollment(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Identity(); err == nil {
		t.Fatal("cleared identity remained readable")
	}
	values, _ = st.UnreportedLocalAudits()
	if len(values) != 0 {
		t.Fatalf("old-instance audits survived enrollment clearing: %#v", values)
	}
}

func TestRuntimeSubscriptionsRemainLocalAndAreClearedWithEnrollment(t *testing.T) {
	st := testStore(t)
	value := RuntimeSubscription{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Name: "测试机场", URL: "https://provider.example/token", Host: "provider.example", Config: []byte("mixed-port: 7890\n"), Revision: "revision"}
	if _, err := st.SaveRuntimeSubscription(value); err != nil {
		t.Fatal(err)
	}
	values, err := st.ListRuntimeSubscriptions()
	if err != nil || len(values) != 1 || values[0].URL != value.URL || string(values[0].Config) != string(value.Config) {
		t.Fatalf("runtime subscriptions = %#v, %v", values, err)
	}
	if err := st.ClearEnrollment(); err != nil {
		t.Fatal(err)
	}
	values, err = st.ListRuntimeSubscriptions()
	if err != nil || len(values) != 0 {
		t.Fatalf("runtime subscriptions survived enrollment clearing: %#v, %v", values, err)
	}
}
