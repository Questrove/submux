package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/agentproto"
)

func runtimeTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestAgentEnrollmentIsShortLivedAndOneTime(t *testing.T) {
	st := runtimeTestStore(t)
	now := time.Now().UTC()
	value := AgentEnrollment{Digest: "digest", Name: "host", CreatedAt: now.Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}
	if err := st.SaveAgentEnrollment(value); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeAgentEnrollment("digest", now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeAgentEnrollment("digest", now); err == nil {
		t.Fatal("pairing code was reusable")
	}
	expired := AgentEnrollment{Digest: "expired", Name: "host", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(-time.Minute).Format(time.RFC3339)}
	if err := st.SaveAgentEnrollment(expired); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeAgentEnrollment("expired", now); err == nil {
		t.Fatal("expired pairing code was accepted")
	}
	if _, err := st.ConsumeAgentEnrollment("expired", now); err == nil {
		t.Fatal("expired pairing code was not removed")
	}
}

func TestRuntimeInstanceStartsWithObservedNotInstalledState(t *testing.T) {
	st := runtimeTestStore(t)
	instance, err := st.CreateRuntimeInstance(RuntimeInstance{Name: "host", DeviceKey: "public", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := st.GetRuntimeObservation(instance.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observation.InstanceID != instance.ID || observation.CoreStatus != RuntimeNotInstalled {
		t.Fatalf("unexpected initial observation: %#v", observation)
	}
}

func TestAgentJobTransitionsExpireAndDoNotReplay(t *testing.T) {
	st := runtimeTestStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	instance, _ := st.CreateRuntimeInstance(RuntimeInstance{Name: "host", DeviceKey: "key"})
	makeJob := func(id string, deadline time.Time) AgentJob {
		return AgentJob{Job: agentproto.Job{ID: id, ProtocolVersion: agentproto.Version, InstanceID: instance.ID, Type: agentproto.JobRestartCore, Params: json.RawMessage(`{}`), Status: agentproto.JobQueued, ActorType: agentproto.ActorAdminSession, RequestID: "request-" + id, Deadline: deadline.Format(time.RFC3339)}}
	}
	if err := st.CreateAgentJob(makeJob("run", now.Add(time.Minute))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionAgentJob("run", agentproto.JobRunning, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionAgentJob("run", agentproto.JobSucceeded, json.RawMessage(`{"ok":true}`), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.TransitionAgentJob("run", agentproto.JobRunning, nil, ""); err == nil {
		t.Fatal("terminal job was replayed")
	}
	if err := st.CreateAgentJob(makeJob("expired", now.Add(-time.Second))); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListRunnableAgentJobs(instance.ID, now)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("expired job was runnable: %#v, %v", jobs, err)
	}
	expired, _ := st.GetAgentJob("expired")
	if expired.Status != agentproto.JobExpired {
		t.Fatalf("job status = %q", expired.Status)
	}
}

func TestAuditRecordsAreIdempotent(t *testing.T) {
	st := runtimeTestStore(t)
	instance, _ := st.CreateRuntimeInstance(RuntimeInstance{Name: "host", DeviceKey: "key"})
	event := AuditEvent{ActorType: agentproto.ActorLocalCLI, RequestID: "audit-request", InstanceID: instance.ID, Action: "mihomo.restart", Result: "succeeded"}
	firstAudit, err := st.AddAuditEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	secondAudit, err := st.AddAuditEvent(event)
	if err != nil || secondAudit.ID != firstAudit.ID {
		t.Fatalf("duplicate audit was not idempotent: %#v %#v %v", firstAudit, secondAudit, err)
	}
}
