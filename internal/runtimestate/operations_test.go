package runtimestate

import (
	"errors"
	"path/filepath"
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
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	request := runtimeapi.CreateOperationRequest{
		RequestID:  "request-one",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	}
	operation, duplicate, err := store.SubmitOperation(peer, "test", request, 1, now)
	if err != nil || duplicate {
		t.Fatalf("submit operation: duplicate=%v err=%v", duplicate, err)
	}
	if operation.State != runtimeapi.OperationQueued {
		t.Fatalf("operation state = %q", operation.State)
	}
	repeated, duplicate, err := store.SubmitOperation(peer, "test", request, 1, now.Add(time.Second))
	if err != nil || !duplicate || repeated.ID != operation.ID {
		t.Fatalf("repeat operation = %#v duplicate=%v err=%v", repeated, duplicate, err)
	}
	conflicting := request
	conflicting.Action.Kind = runtimeapi.ActionStopProxy
	if _, _, err := store.SubmitOperation(peer, "test", conflicting, 1, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request ID conflict error = %v", err)
	}
	newRequest := runtimeapi.CreateOperationRequest{
		RequestID:  "request-two",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStopProxy},
	}
	var revisionError *RevisionConflictError
	if _, _, err := store.SubmitOperation(peer, "test", newRequest, 1, now); !errors.As(err, &revisionError) || revisionError.Current != 2 {
		t.Fatalf("revision conflict = %#v / %v", revisionError, err)
	}
	newRequest.IfRevision = 2
	if _, _, err := store.SubmitOperation(peer, "test", newRequest, 1, now); !errors.Is(err, ErrBusy) {
		t.Fatalf("queue capacity error = %v", err)
	}
	if _, _, err := store.CancelOperation(peer, operation.ID, runtimeapi.CancelOperationRequest{
		RequestID:  request.RequestID,
		IfRevision: 2,
	}, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("operation request ID reused for cancellation: %v", err)
	}

	cancelRequest := runtimeapi.CancelOperationRequest{RequestID: "cancel-one", IfRevision: 2}
	cancelled, duplicate, err := store.CancelOperation(peer, operation.ID, cancelRequest, now.Add(time.Second))
	if err != nil || duplicate || cancelled.State != runtimeapi.OperationCancelled {
		t.Fatalf("cancel operation = %#v duplicate=%v err=%v", cancelled, duplicate, err)
	}
	repeatedCancel, duplicate, err := store.CancelOperation(peer, operation.ID, cancelRequest, now.Add(2*time.Second))
	if err != nil || !duplicate || repeatedCancel.State != runtimeapi.OperationCancelled {
		t.Fatalf("repeat cancellation = %#v duplicate=%v err=%v", repeatedCancel, duplicate, err)
	}
	if _, _, err := store.SubmitOperation(peer, "test", runtimeapi.CreateOperationRequest{
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
	operation, _, err := store.SubmitOperation(peer, "test", runtimeapi.CreateOperationRequest{
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
	snapshot, err := store.Observe("test", now)
	if err != nil {
		t.Fatalf("observe Runtime state: %v", err)
	}
	if _, _, err := store.CancelOperation(peer, operation.ID, runtimeapi.CancelOperationRequest{
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
