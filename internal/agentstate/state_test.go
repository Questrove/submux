package agentstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

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
