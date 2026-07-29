package runtimeapp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

type executorFunc struct {
	execute func(context.Context, runtimeapi.Operation, StageReporter) (*runtimeapi.OperationResult, error)
	verify  func(context.Context) (runtimeapi.ProxyVerification, error)
}

func (e executorFunc) Execute(
	ctx context.Context,
	operation runtimeapi.Operation,
	report StageReporter,
) (*runtimeapi.OperationResult, error) {
	return e.execute(ctx, operation, report)
}

func (e executorFunc) Verify(ctx context.Context) (runtimeapi.ProxyVerification, error) {
	if e.verify != nil {
		return e.verify(ctx)
	}
	return runtimeapi.ProxyVerification{Available: true}, nil
}

func TestCoordinatorRunsPersistedOperationAfterSubmission(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	executed := make(chan string, 1)
	coordinator := &Coordinator{
		State: state,
		Executor: executorFunc{execute: func(
			_ context.Context,
			operation runtimeapi.Operation,
			report StageReporter,
		) (*runtimeapi.OperationResult, error) {
			if err := report("committing", 70, false); err != nil {
				return nil, err
			}
			executed <- operation.ID
			return &runtimeapi.OperationResult{Verified: true}, nil
		}},
		Version: "test",
	}
	serviceContext, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- coordinator.Run(serviceContext) }()

	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	operation, duplicate, err := coordinator.Execute(context.Background(), peer, "test", runtimeapi.CreateOperationRequest{
		RequestID:  "request-one",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	})
	if err != nil || duplicate {
		cancel()
		t.Fatalf("submit Runtime operation: duplicate=%v err=%v", duplicate, err)
	}
	select {
	case executedID := <-executed:
		if executedID != operation.ID {
			cancel()
			t.Fatalf("executed operation ID = %q, want %q", executedID, operation.ID)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("persisted Runtime operation did not execute")
	}
	waitForOperationState(t, state, operation.ID, runtimeapi.OperationSucceeded)
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Runtime coordinator: %v", err)
	}
}

func TestCoordinatorCancelsOnlyCancellableRunningStage(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	started := make(chan struct{})
	cancelled := make(chan struct{})
	coordinator := &Coordinator{
		State: state,
		Executor: executorFunc{execute: func(
			ctx context.Context,
			_ runtimeapi.Operation,
			report StageReporter,
		) (*runtimeapi.OperationResult, error) {
			if err := report("preparing", 10, true); err != nil {
				return nil, err
			}
			close(started)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		}},
		Version: "test",
	}
	serviceContext, cancelService := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- coordinator.Run(serviceContext) }()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	operation, _, err := coordinator.Execute(context.Background(), peer, "test", runtimeapi.CreateOperationRequest{
		RequestID:  "request-one",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	})
	if err != nil {
		cancelService()
		t.Fatalf("submit Runtime operation: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancelService()
		t.Fatal("Runtime operation did not enter cancellable stage")
	}
	snapshot, err := coordinator.Observe(context.Background(), peer)
	if err != nil {
		cancelService()
		t.Fatalf("observe Runtime before cancellation: %v", err)
	}
	cancelledOperation, _, err := coordinator.CancelOperation(context.Background(), peer, operation.ID, runtimeapi.CancelOperationRequest{
		RequestID:  "cancel-one",
		IfRevision: snapshot.Revision,
	})
	if err != nil || cancelledOperation.State != runtimeapi.OperationCancelled {
		cancelService()
		t.Fatalf("cancel Runtime operation = %#v err=%v", cancelledOperation, err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		cancelService()
		t.Fatal("running Runtime executor context was not cancelled")
	}
	waitForOperationState(t, state, operation.ID, runtimeapi.OperationCancelled)
	cancelService()
	if err := <-result; err != nil {
		t.Fatalf("stop Runtime coordinator: %v", err)
	}
}

func TestClientContextEndingDoesNotCancelPersistedOperation(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	started := make(chan struct{})
	release := make(chan struct{})
	coordinator := &Coordinator{
		State: state,
		Executor: executorFunc{execute: func(
			_ context.Context,
			_ runtimeapi.Operation,
			_ StageReporter,
		) (*runtimeapi.OperationResult, error) {
			close(started)
			<-release
			return &runtimeapi.OperationResult{Verified: true}, nil
		}},
		Version: "test",
	}
	serviceContext, cancelService := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- coordinator.Run(serviceContext) }()
	peer := runtimeapi.PeerIdentity{Platform: "test", UID: 1000}
	clientContext, cancelClient := context.WithCancel(context.Background())
	operation, _, err := coordinator.Execute(clientContext, peer, "test", runtimeapi.CreateOperationRequest{
		RequestID:  "request-one",
		IfRevision: 1,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStartProxy},
	})
	if err != nil {
		cancelService()
		t.Fatalf("submit Runtime operation: %v", err)
	}
	cancelClient()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancelService()
		t.Fatal("persisted operation did not start after client context ended")
	}
	close(release)
	waitForOperationState(t, state, operation.ID, runtimeapi.OperationSucceeded)
	cancelService()
	if err := <-result; err != nil {
		t.Fatalf("stop Runtime coordinator: %v", err)
	}
}

func waitForOperationState(t *testing.T, state *runtimestate.Store, id, expected string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		operation, err := state.GetOperation(id)
		if err == nil && operation.State == expected {
			return
		}
		if err != nil && !errors.Is(err, runtimestate.ErrOperationNotFound) {
			t.Fatalf("get Runtime operation: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	operation, _ := state.GetOperation(id)
	t.Fatalf("operation state = %q, want %q", operation.State, expected)
}
