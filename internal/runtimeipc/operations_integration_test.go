package runtimeipc

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimeapp"
	"submux/internal/runtimestate"
)

type IPCExecutor struct{}

func (IPCExecutor) Execute(
	_ context.Context,
	operation runtimeapi.Operation,
	report runtimeapp.StageReporter,
) (*runtimeapi.OperationResult, error) {
	if err := report("committing", 70, false); err != nil {
		return nil, err
	}
	return &runtimeapi.OperationResult{
		ConfigRevision: operation.ID,
		ProxyKind:      "mixed",
		ProxyAddresses: []string{"127.0.0.1:7890", "[::1]:7890"},
		Verified:       true,
	}, nil
}

func (IPCExecutor) Verify(context.Context) (runtimeapi.ProxyVerification, error) {
	return runtimeapi.ProxyVerification{
		Available: true,
		Kind:      "mixed",
		Addresses: []string{"127.0.0.1:7890", "[::1]:7890"},
		CheckedAt: time.Now().UTC(),
	}, nil
}

func (IPCExecutor) PreviewCandidate(
	_ context.Context,
	_ runtimeapi.PeerIdentity,
	request runtimeapi.PreviewCandidateRequest,
) (runtimeapi.CandidatePreview, error) {
	return runtimeapi.CandidatePreview{
		ContentID:          request.ContentID,
		CandidateYAML:      "listeners: []\n",
		CandidateSHA256:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		ProxyKind:          "mixed",
		ProxyAddresses:     []string{"127.0.0.1:7890", "[::1]:7890"},
		RuntimeOwnedFields: []string{"listeners"},
		Validated:          true,
	}, nil
}

func TestImportOperationWaitAndVerifyOverLocalIPC(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	coordinator := &runtimeapp.Coordinator{
		State:    state,
		Executor: IPCExecutor{},
		Version:  "test",
	}
	endpoint := testEndpoint(t)
	listener, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("listen on Runtime IPC: %v", err)
	}
	authorizer, err := CurrentUserAuthorizer()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime authorizer: %v", err)
	}
	server, err := NewServer(coordinator, authorizer)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerResult := make(chan error, 1)
	serverResult := make(chan error, 1)
	go func() { workerResult <- coordinator.Run(ctx) }()
	go func() { serverResult <- server.Serve(ctx, listener) }()

	client, err := NewClient(endpoint, "test")
	if err != nil {
		cancel()
		t.Fatalf("create Runtime IPC client: %v", err)
	}
	defer client.CloseIdleConnections()
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		cancel()
		t.Fatalf("observe Runtime: %v", err)
	}
	body := []byte("proxies: []\nrules: []\n")
	content, err := client.UploadImport(context.Background(), "application/x-yaml", body)
	if err != nil {
		cancel()
		t.Fatalf("upload Runtime import: %v", err)
	}
	preview, err := client.PreviewCandidate(context.Background(), content.ID)
	if err != nil {
		cancel()
		t.Fatalf("preview Runtime candidate: %v", err)
	}
	if !preview.Validated || preview.ContentID != content.ID {
		cancel()
		t.Fatalf("candidate preview = %#v", preview)
	}
	request := runtimeapi.CreateOperationRequest{
		RequestID:  "operation-request-one",
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind:   runtimeapi.ActionApplyImportedConfig,
			Params: runtimeapi.ActionParams{ContentID: content.ID},
		},
	}
	operation, err := client.Execute(context.Background(), request)
	if err != nil {
		cancel()
		t.Fatalf("create Runtime operation: %v", err)
	}
	finished, err := client.WaitOperation(context.Background(), operation.ID, 10*time.Millisecond)
	if err != nil {
		cancel()
		t.Fatalf("wait for Runtime operation: %v", err)
	}
	if finished.State != runtimeapi.OperationSucceeded || finished.Result == nil || !finished.Result.Verified {
		cancel()
		t.Fatalf("finished operation = %#v", finished)
	}
	repeated, err := client.Execute(context.Background(), request)
	if err != nil || repeated.ID != operation.ID {
		cancel()
		t.Fatalf("repeat operation = %#v err=%v", repeated, err)
	}
	_, err = client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		RequestID:  "stale-revision",
		IfRevision: snapshot.Revision,
		Action:     runtimeapi.Action{Kind: runtimeapi.ActionStopProxy},
	})
	var clientError *ClientError
	if !errors.As(err, &clientError) ||
		clientError.Code != runtimeapi.ErrorRevisionConflict ||
		clientError.CurrentRevision <= snapshot.Revision {
		cancel()
		t.Fatalf("stale revision error = %#v / %v", clientError, err)
	}
	verification, err := client.VerifyProxy(context.Background())
	if err != nil || !verification.Available {
		cancel()
		t.Fatalf("verify Runtime proxy = %#v err=%v", verification, err)
	}
	updated, err := client.Observe(context.Background())
	if err != nil {
		cancel()
		t.Fatalf("observe updated Runtime: %v", err)
	}
	if updated.Mihomo.State != "running" || updated.RunMode != "explicit" {
		cancel()
		t.Fatalf("updated Runtime snapshot = %#v", updated)
	}

	cancel()
	if err := <-serverResult; err != nil {
		t.Fatalf("stop Runtime IPC server: %v", err)
	}
	if err := <-workerResult; err != nil {
		t.Fatalf("stop Runtime coordinator: %v", err)
	}
}
