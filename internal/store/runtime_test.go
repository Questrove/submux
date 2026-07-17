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

func TestRuntimeDesiredStateUsesOptimisticGeneration(t *testing.T) {
	st := runtimeTestStore(t)
	instance, err := st.CreateRuntimeInstance(RuntimeInstance{Name: "host", DeviceKey: "public", OS: "linux", Arch: "amd64"})
	if err != nil {
		t.Fatal(err)
	}
	desired, err := st.GetRuntimeDesiredState(instance.ID)
	if err != nil || desired.Generation != 1 || desired.RuntimeState != RuntimeStopped {
		t.Fatalf("unexpected initial desired state: %#v, %v", desired, err)
	}
	desired.CoreInstalled, desired.CoreVersion, desired.RuntimeState = true, "v1.19.10", RuntimeRunning
	updated, err := st.UpdateRuntimeDesiredState(desired, 1)
	if err != nil || updated.Generation != 2 {
		t.Fatalf("desired update failed: %#v, %v", updated, err)
	}
	if _, err := st.UpdateRuntimeDesiredState(desired, 1); err == nil {
		t.Fatal("stale desired generation was accepted")
	}
}

func TestRuntimeBindingIsOnePerInstance(t *testing.T) {
	st := runtimeTestStore(t)
	instance, _ := st.CreateRuntimeInstance(RuntimeInstance{Name: "host", DeviceKey: "key"})
	first, err := st.SaveRuntimeBinding(RuntimeBinding{InstanceID: instance.ID, OutputSubscriptionID: 1, RuntimeContract: "mihomo-agent/v1", CheckIntervalSec: 300})
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.SaveRuntimeBinding(RuntimeBinding{InstanceID: instance.ID, OutputSubscriptionID: 2, RuntimeContract: "mihomo-agent/v1", CheckIntervalSec: 600})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.OutputSubscriptionID != 2 {
		t.Fatalf("binding was duplicated instead of replaced: first=%#v second=%#v", first, second)
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

func TestDeploymentIntegrationAndAuditRecordsRemainDistinct(t *testing.T) {
	st := runtimeTestStore(t)
	instance, _ := st.CreateRuntimeInstance(RuntimeInstance{Name: "host", DeviceKey: "key"})
	first, err := st.AddDeployment(Deployment{InstanceID: instance.ID, RequestID: "request-1", ActorType: agentproto.ActorScheduler, RemoteRevision: "rev-1", Status: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddDeployment(Deployment{InstanceID: instance.ID, RequestID: "request-2", ActorType: agentproto.ActorAdminSession, RemoteRevision: "rev-2", Status: "failed"}); err != nil {
		t.Fatal(err)
	}
	deployments, err := st.ListDeployments(instance.ID, 1)
	if err != nil || len(deployments) != 1 || deployments[0].RemoteRevision != "rev-2" || deployments[0].ID == first.ID {
		t.Fatalf("unexpected deployments: %#v, %v", deployments, err)
	}
	integrationState, err := st.UpsertIntegrationState(IntegrationState{InstanceID: instance.ID, Type: "docker_daemon", Scope: "system", DesiredState: "active", ObservedState: "applying"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := st.UpsertIntegrationState(IntegrationState{InstanceID: instance.ID, Type: "docker_daemon", Scope: "system", DesiredState: "active", ObservedState: "active", Validation: "verified"})
	if err != nil || updated.ID != integrationState.ID {
		t.Fatalf("integration upsert failed: %#v, %v", updated, err)
	}
	states, err := st.ListIntegrationStates(instance.ID)
	if err != nil || len(states) != 1 || states[0].ObservedState != "active" || states[0].Validation != "verified" {
		t.Fatalf("unexpected integration states: %#v, %v", states, err)
	}
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
