package runtimestate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"submux/internal/runtimeapi"
)

func TestOperationPersistenceIdempotencyRevisionAndCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	canceller := runtimeapi.PeerIdentity{Platform: "test", UID: 1001}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	request := runtimeapi.CreateOperationRequest{
		RequestID:  "request-one",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	}
	operation, duplicate, err := store.SubmitOperation(peer, "test", "test", request, 1, now)
	if err != nil || duplicate {
		t.Fatalf("submit operation: duplicate=%v err=%v", duplicate, err)
	}
	if operation.State != runtimeapi.OperationQueued {
		t.Fatalf("operation state = %q", operation.State)
	}
	repeated, duplicate, err := store.SubmitOperation(peer, "test", "test", request, 1, now.Add(time.Second))
	if err != nil || !duplicate || repeated.ID != operation.ID {
		t.Fatalf("repeat operation = %#v duplicate=%v err=%v", repeated, duplicate, err)
	}
	conflicting := request
	conflicting.Action.Kind = runtimeapi.ActionStopProxy
	if _, _, err := store.SubmitOperation(peer, "test", "test", conflicting, 1, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request ID conflict error = %v", err)
	}
	newRequest := runtimeapi.CreateOperationRequest{
		RequestID:  "request-two",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStopProxy},
	}
	var revisionError *RevisionConflictError
	if _, _, err := store.SubmitOperation(peer, "test", "test", newRequest, 1, now); !errors.As(err, &revisionError) || revisionError.Current != 2 {
		t.Fatalf("revision conflict = %#v / %v", revisionError, err)
	}
	newRequest.IfRevision = 2
	if _, _, err := store.SubmitOperation(peer, "test", "test", newRequest, 1, now); !errors.Is(err, ErrBusy) {
		t.Fatalf("queue capacity error = %v", err)
	}
	if _, _, err := store.CancelOperation(peer, "test", "test", operation.ID, runtimeapi.CancelOperationRequest{
		RequestID:  request.RequestID,
		IfRevision: 2,
	}, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("operation request ID reused for cancellation: %v", err)
	}

	cancelRequest := runtimeapi.CancelOperationRequest{RequestID: "cancel-one", IfRevision: 2}
	cancelled, duplicate, err := store.CancelOperation(canceller, "test", "test", operation.ID, cancelRequest, now.Add(time.Second))
	if err != nil || duplicate || cancelled.State != runtimeapi.OperationCancelled {
		t.Fatalf("cancel operation = %#v duplicate=%v err=%v", cancelled, duplicate, err)
	}
	if cancelled.CallerIdentity != peer.Key() || cancelled.CancelledBy != canceller.Key() {
		t.Fatalf("cancellation identities = caller %q canceller %q", cancelled.CallerIdentity, cancelled.CancelledBy)
	}
	repeatedCancel, duplicate, err := store.CancelOperation(canceller, "test", "test", operation.ID, cancelRequest, now.Add(2*time.Second))
	if err != nil || !duplicate || repeatedCancel.State != runtimeapi.OperationCancelled {
		t.Fatalf("repeat cancellation = %#v duplicate=%v err=%v", repeatedCancel, duplicate, err)
	}
	if _, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  cancelRequest.RequestID,
		IfRevision: 3,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStopProxy},
	}, 1, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("cancellation request ID reused for operation: %v", err)
	}
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe Runtime state: %v", err)
	}
	if snapshot.Revision != 3 || snapshot.LatestEventCursor != 2 || snapshot.Operations.Queued != 0 {
		t.Fatalf("snapshot after cancellation = %#v", snapshot)
	}
}

func TestOperationStageAndCrashRecovery(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	operation, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  "request-one",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit operation: %v", err)
	}
	running, found, err := store.BeginNextOperation(now.Add(time.Second))
	if err != nil || !found || running.ID != operation.ID {
		t.Fatalf("begin operation = %#v found=%v err=%v", running, found, err)
	}
	if err := store.UpdateOperationStage(operation.ID, "starting_proxy", 60, false, now.Add(2*time.Second)); err != nil {
		t.Fatalf("update operation stage: %v", err)
	}
	if err := store.UpdateOperationStage(operation.ID, "preparing_again", 70, true, now.Add(2500*time.Millisecond)); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("re-enable cancellation after side-effect boundary: %v", err)
	}
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe Runtime state: %v", err)
	}
	if _, _, err := store.CancelOperation(peer, "test", "test", operation.ID, runtimeapi.CancelOperationRequest{
		RequestID:  "cancel-one",
		IfRevision: snapshot.Revision,
	}, now.Add(3*time.Second)); !errors.Is(err, ErrNotCancellable) {
		t.Fatalf("non-cancellable operation error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close Runtime state: %v", err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen Runtime state: %v", err)
	}
	defer reopened.Close()
	if err := reopened.RecoverOperations(now.Add(4 * time.Second)); err != nil {
		t.Fatalf("recover Runtime operations: %v", err)
	}
	recovered, err := reopened.GetOperation(operation.ID)
	if err != nil {
		t.Fatalf("get recovered operation: %v", err)
	}
	if recovered.State != runtimeapi.OperationOutcomeUnknown || recovered.Cancellable {
		t.Fatalf("recovered operation = %#v", recovered)
	}
}

func TestQueuedOperationRemainsRunnableAfterRuntimeRestart(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	store, err := Open(root)
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	queued, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  "queued-before-restart",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit queued operation: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close Runtime state: %v", err)
	}

	reopened, err := Open(root)
	if err != nil {
		t.Fatalf("reopen Runtime state: %v", err)
	}
	defer reopened.Close()
	if err := reopened.RecoverOperations(now.Add(time.Minute)); err != nil {
		t.Fatalf("recover Runtime operations: %v", err)
	}
	recovered, err := reopened.GetOperation(queued.ID)
	if err != nil || recovered.State != runtimeapi.OperationQueued {
		t.Fatalf("recovered queued operation=%#v err=%v", recovered, err)
	}
	running, found, err := reopened.BeginNextOperation(now.Add(2 * time.Minute))
	if err != nil || !found || running.ID != queued.ID || running.State != runtimeapi.OperationRunning {
		t.Fatalf("begin recovered operation=%#v found=%v err=%v", running, found, err)
	}
}

func TestBeginAndCancelRaceKeepsOnePersistedTerminalOutcome(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	canceller := runtimeapi.PeerIdentity{Platform: "test", UID: 1001}
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	operation, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  "race-operation",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit raced operation: %v", err)
	}

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	var beginErr error
	var cancelErr error
	go func() {
		defer wait.Done()
		<-start
		_, _, beginErr = store.BeginNextOperation(now.Add(time.Second))
	}()
	go func() {
		defer wait.Done()
		<-start
		request := runtimeapi.CancelOperationRequest{RequestID: "race-cancel", IfRevision: 2}
		_, _, cancelErr = store.CancelOperation(canceller, "test", "test", operation.ID, request, now.Add(time.Second))
		if !errors.Is(cancelErr, ErrRevisionConflict) {
			return
		}
		snapshot, observeErr := store.Observe("test", now.Add(time.Second))
		if observeErr != nil {
			cancelErr = observeErr
			return
		}
		request.IfRevision = snapshot.Revision
		_, _, cancelErr = store.CancelOperation(canceller, "test", "test", operation.ID, request, now.Add(2*time.Second))
	}()
	close(start)
	wait.Wait()
	if beginErr != nil || cancelErr != nil {
		t.Fatalf("begin/cancel race: begin=%v cancel=%v", beginErr, cancelErr)
	}
	cancelled, err := store.GetOperation(operation.ID)
	if err != nil ||
		cancelled.State != runtimeapi.OperationCancelled ||
		cancelled.CallerIdentity != peer.Key() ||
		cancelled.CancelledBy != canceller.Key() {
		t.Fatalf("raced operation=%#v err=%v", cancelled, err)
	}
	if err := store.UpdateOperationStage(operation.ID, "side_effect", 50, false, now.Add(3*time.Second)); !errors.Is(err, ErrOperationTerminal) {
		t.Fatalf("raced cancelled operation accepted a later stage: %v", err)
	}
}

func TestPreparedConfigurationRemainsStoppedUntilExplicitStart(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	now := time.Now().UTC()
	body := []byte("proxies: []\n")
	digest := sha256.Sum256(body)
	content, err := store.UploadImport(
		peer,
		"text/yaml",
		int64(len(body)),
		hex.EncodeToString(digest[:]),
		body,
		now,
	)
	if err != nil {
		t.Fatalf("upload Runtime import: %v", err)
	}
	operation, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  "prepare-config",
		IfRevision: 1,
		Action: runtimeapi.Action{
			Kind:   runtimeapi.ActionApplyImportedConfig,
			Params: runtimeapi.ActionParams{ContentID: content.ID},
		},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit prepare operation: %v", err)
	}
	if _, found, err := store.BeginNextOperation(now); err != nil || !found {
		t.Fatalf("begin prepare operation: found=%v err=%v", found, err)
	}
	if err := store.CompleteOperation(
		operation.ID,
		runtimeapi.OperationSucceeded,
		&runtimeapi.OperationResult{ConfigRevision: operation.ID, Verified: false},
		nil,
		now,
	); err != nil {
		t.Fatalf("complete prepare operation: %v", err)
	}
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe prepared Runtime: %v", err)
	}
	if snapshot.Mihomo.State != "stopped" || snapshot.RunMode != "explicit" {
		t.Fatalf("prepared Runtime snapshot = %#v", snapshot)
	}

	sourceOperation, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  "prepare-source",
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionApplySource,
			Params: runtimeapi.ActionParams{
				SourceID: "src_" + strings.Repeat("a", 32),
			},
		},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit source prepare operation: %v", err)
	}
	if _, found, err := store.BeginNextOperation(now); err != nil || !found {
		t.Fatalf("begin source prepare operation: found=%v err=%v", found, err)
	}
	if err := store.CompleteOperation(
		sourceOperation.ID,
		runtimeapi.OperationSucceeded,
		&runtimeapi.OperationResult{SourceID: sourceOperation.Action.Params.SourceID, Verified: false},
		nil,
		now,
	); err != nil {
		t.Fatalf("complete source prepare operation: %v", err)
	}
	snapshot, err = store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe source-prepared Runtime: %v", err)
	}
	if snapshot.Mihomo.State != "stopped" || snapshot.RunMode != "explicit" {
		t.Fatalf("source-prepared Runtime snapshot = %#v", snapshot)
	}
}

func TestCompleteTUNOperationsRecordsRunModeTransitions(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer store.Close()
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	enable, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  "enable-tun",
		IfRevision: 1,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionEnableTUN,
			Params: runtimeapi.ActionParams{
				PlanID: "plan_0123456789abcdef0123456789abcdef",
			},
		},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit enable TUN operation: %v", err)
	}
	if _, found, err := store.BeginNextOperation(now); err != nil || !found {
		t.Fatalf("begin enable TUN operation: found=%v err=%v", found, err)
	}
	if err := store.CompleteOperation(
		enable.ID,
		runtimeapi.OperationSucceeded,
		&runtimeapi.OperationResult{Verified: true, RunMode: runtimeapi.RunModeTUN},
		nil,
		now,
	); err != nil {
		t.Fatalf("complete enable TUN operation: %v", err)
	}
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe enabled TUN Runtime: %v", err)
	}
	if snapshot.RunMode != runtimeapi.RunModeTUN || snapshot.Mihomo.State != "running" {
		t.Fatalf("enabled TUN snapshot=%#v", snapshot)
	}

	disable, _, err := store.SubmitOperation(peer, "test", "test", runtimeapi.CreateOperationRequest{
		RequestID:  "disable-tun",
		IfRevision: snapshot.Revision,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionDisableTUN},
	}, 4, now)
	if err != nil {
		t.Fatalf("submit disable TUN operation: %v", err)
	}
	if _, found, err := store.BeginNextOperation(now); err != nil || !found {
		t.Fatalf("begin disable TUN operation: found=%v err=%v", found, err)
	}
	if err := store.CompleteOperation(
		disable.ID,
		runtimeapi.OperationSucceeded,
		&runtimeapi.OperationResult{Verified: true, RunMode: runtimeapi.RunModeExplicit},
		nil,
		now,
	); err != nil {
		t.Fatalf("complete disable TUN operation: %v", err)
	}
	snapshot, err = store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe disabled TUN Runtime: %v", err)
	}
	if snapshot.RunMode != runtimeapi.RunModeExplicit || snapshot.Mihomo.State != "running" {
		t.Fatalf("disabled TUN snapshot=%#v", snapshot)
	}
}
