package runtimestate

import (
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"submux/internal/runtimeapi"
)

func TestExplicitMihomoIntentIsSavedOnlyAfterSuccessfulOperation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	failed := submitRunningLifecycleOperation(t, store, peer, runtimeapi.ActionStartProxy, now)
	if err := store.CompleteOperation(
		failed.ID,
		runtimeapi.OperationFailed,
		nil,
		&runtimeapi.ProtocolError{Code: runtimeapi.ErrorServiceUnavailable, Message: "start failed"},
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("fail explicit start: %v", err)
	}
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe failed explicit start: %v", err)
	}
	if snapshot.Mihomo.DesiredState != runtimeapi.MihomoDesiredUnset {
		t.Fatalf("desired state after failed first start=%q", snapshot.Mihomo.DesiredState)
	}

	started := submitRunningLifecycleOperation(t, store, peer, runtimeapi.ActionStartProxy, now.Add(2*time.Second))
	if err := store.CompleteOperation(
		started.ID,
		runtimeapi.OperationSucceeded,
		&runtimeapi.OperationResult{Verified: true},
		nil,
		now.Add(3*time.Second),
	); err != nil {
		t.Fatalf("complete explicit start: %v", err)
	}
	snapshot, err = store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe explicit start: %v", err)
	}
	if snapshot.Mihomo.DesiredState != runtimeapi.MihomoDesiredRunning ||
		snapshot.Mihomo.State != "running" ||
		snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryIdle {
		t.Fatalf("Mihomo state after explicit start=%#v", snapshot.Mihomo)
	}

	stopped := submitRunningLifecycleOperation(t, store, peer, runtimeapi.ActionStopProxy, now.Add(4*time.Second))
	if err := store.CompleteOperation(
		stopped.ID,
		runtimeapi.OperationSucceeded,
		&runtimeapi.OperationResult{Verified: true},
		nil,
		now.Add(5*time.Second),
	); err != nil {
		t.Fatalf("complete explicit stop: %v", err)
	}
	snapshot, err = store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe explicit stop: %v", err)
	}
	if snapshot.Mihomo.DesiredState != runtimeapi.MihomoDesiredStopped ||
		snapshot.Mihomo.State != "stopped" {
		t.Fatalf("Mihomo state after explicit stop=%#v", snapshot.Mihomo)
	}
}

func TestMihomoCrashBackoffLimitAndStableReset(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		return recordExplicitMihomoStart(transaction.Bucket(metadataBucket))
	})
	if err != nil {
		t.Fatalf("seed Mihomo desired state: %v", err)
	}

	for index, delay := range MihomoRestartBackoff {
		crashedAt := now.Add(time.Duration(index) * time.Minute)
		decision, err := store.RegisterMihomoCrash(crashedAt)
		if err != nil {
			t.Fatalf("register Mihomo crash %d: %v", index+1, err)
		}
		if !decision.Restart ||
			decision.Attempt != index+1 ||
			decision.NextRestartAt == nil ||
			!decision.NextRestartAt.Equal(crashedAt.Add(delay)) {
			t.Fatalf("Mihomo crash decision %d=%#v", index+1, decision)
		}
		if err := store.MarkMihomoRestarting(*decision.NextRestartAt); err != nil {
			t.Fatalf("mark Mihomo restarting %d: %v", index+1, err)
		}
		if err := store.MarkMihomoRestartSucceeded(decision.NextRestartAt.Add(time.Second)); err != nil {
			t.Fatalf("mark Mihomo restart succeeded %d: %v", index+1, err)
		}
	}
	limited, err := store.RegisterMihomoCrash(now.Add(3 * time.Minute))
	if err != nil {
		t.Fatalf("register limited Mihomo crash: %v", err)
	}
	if limited.Restart || limited.Attempt != 3 || limited.NextRestartAt != nil {
		t.Fatalf("limited Mihomo crash decision=%#v", limited)
	}
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe limited Mihomo recovery: %v", err)
	}
	if snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryNeedsAttention ||
		snapshot.Mihomo.State != "stopped" ||
		snapshot.Mihomo.Fault == nil {
		t.Fatalf("limited Mihomo status=%#v", snapshot.Mihomo)
	}

	err = store.db.Update(func(transaction *bbolt.Tx) error {
		return recordExplicitMihomoStart(transaction.Bucket(metadataBucket))
	})
	if err != nil {
		t.Fatalf("clear Mihomo fault by manual start: %v", err)
	}
	first, err := store.RegisterMihomoCrash(now.Add(20 * time.Minute))
	if err != nil || !first.Restart || first.Attempt != 1 {
		t.Fatalf("Mihomo crash after manual reset=%#v err=%v", first, err)
	}
	if err := store.MarkMihomoRestartSucceeded(*first.NextRestartAt); err != nil {
		t.Fatalf("mark Mihomo restarted before stability: %v", err)
	}
	if err := store.MarkMihomoStable(first.NextRestartAt.Add(MihomoCrashWindow)); err != nil {
		t.Fatalf("mark Mihomo stable: %v", err)
	}
	afterStable, err := store.RegisterMihomoCrash(first.NextRestartAt.Add(MihomoCrashWindow + time.Second))
	if err != nil || !afterStable.Restart || afterStable.Attempt != 1 {
		t.Fatalf("Mihomo crash after stable reset=%#v err=%v", afterStable, err)
	}
}

func TestMihomoStartupRecoveryFailureDoesNotScheduleLoop(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		return recordExplicitMihomoStart(transaction.Bucket(metadataBucket))
	})
	if err != nil {
		t.Fatalf("seed Mihomo desired state: %v", err)
	}
	restore, err := store.PrepareMihomoStartup(now)
	if err != nil || !restore {
		t.Fatalf("prepare Mihomo startup recovery: restore=%v err=%v", restore, err)
	}
	if err := store.MarkMihomoRecoveryFailure(
		"startup_recovery_failed",
		"last-good configuration did not start",
		false,
		now.Add(time.Second),
	); err != nil {
		t.Fatalf("mark Mihomo startup recovery failed: %v", err)
	}
	decision, err := store.RegisterMihomoCrash(now.Add(2 * time.Second))
	if err != nil {
		t.Fatalf("inspect recovery after startup failure: %v", err)
	}
	if decision.Restart {
		t.Fatalf("startup failure scheduled another recovery=%#v", decision)
	}
}

func TestMihomoFaultClearsOnlyForChangedConfigOrCore(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	if err := store.MarkMihomoRecoveryFailure("failed", "failed", false, now); err != nil {
		t.Fatalf("mark Mihomo recovery failure: %v", err)
	}
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		return clearMihomoRecoveryForNewConfig(metadata, "same", "same")
	})
	if err != nil {
		t.Fatalf("apply identical Mihomo config digest: %v", err)
	}
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe identical config recovery: %v", err)
	}
	if snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryNeedsAttention {
		t.Fatalf("identical config cleared recovery=%#v", snapshot.Mihomo)
	}
	err = store.db.Update(func(transaction *bbolt.Tx) error {
		metadata := transaction.Bucket(metadataBucket)
		return clearMihomoRecoveryForNewConfig(metadata, "same", "changed")
	})
	if err != nil {
		t.Fatalf("apply changed Mihomo config digest: %v", err)
	}
	snapshot, err = store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe changed config recovery: %v", err)
	}
	if snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryIdle || snapshot.Mihomo.Fault != nil {
		t.Fatalf("changed config did not clear recovery=%#v", snapshot.Mihomo)
	}

	changed, err := store.ObserveMihomoCoreVersion("v1", now)
	if err != nil || changed {
		t.Fatalf("observe initial Mihomo core: changed=%v err=%v", changed, err)
	}
	if err := store.MarkMihomoRecoveryFailure("failed", "failed", false, now); err != nil {
		t.Fatalf("mark second Mihomo recovery failure: %v", err)
	}
	changed, err = store.ObserveMihomoCoreVersion("v1", now)
	if err != nil || changed {
		t.Fatalf("apply identical Mihomo core: %v", err)
	}
	snapshot, _ = store.Observe("test", now)
	if snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryNeedsAttention {
		t.Fatalf("identical core cleared recovery=%#v", snapshot.Mihomo)
	}
	changed, err = store.ObserveMihomoCoreVersion("v2", now)
	if err != nil || !changed {
		t.Fatalf("apply changed Mihomo core: changed=%v err=%v", changed, err)
	}
	snapshot, _ = store.Observe("test", now)
	if snapshot.Mihomo.Version != "v2" ||
		snapshot.Mihomo.Recovery != runtimeapi.MihomoRecoveryIdle ||
		snapshot.Mihomo.Fault != nil {
		t.Fatalf("changed core did not clear recovery=%#v", snapshot.Mihomo)
	}
}

func submitRunningLifecycleOperation(
	t *testing.T,
	store *Store,
	peer runtimeapi.PeerIdentity,
	action string,
	now time.Time,
) runtimeapi.Operation {
	t.Helper()
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe Runtime before lifecycle operation: %v", err)
	}
	operation, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  action + "-" + now.Format(time.RFC3339Nano),
		IfRevision: snapshot.Revision,
		Action:     runtimeapi.Action{Kind: action},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit lifecycle operation %q: %v", action, err)
	}
	running, found, err := store.BeginNextOperation(now)
	if err != nil || !found || running.ID != operation.ID {
		t.Fatalf("begin lifecycle operation %q: running=%#v found=%v err=%v", action, running, found, err)
	}
	return running
}
