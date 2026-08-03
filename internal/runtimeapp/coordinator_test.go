package runtimeapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"submux/internal/runtimeapi"
	"submux/internal/runtimestate"
)

type executorFunc struct {
	execute func(context.Context, runtimeapi.Operation, StageReporter) (*runtimeapi.OperationResult, error)
	verify  func(context.Context) (runtimeapi.ProxyVerification, error)
}

type scheduledExecutor struct {
	executorFunc
	action    runtimeapi.Action
	delivered bool
}

type blockingStartupRecovery struct {
	started chan struct{}
	release chan struct{}
}

type networkServiceFunc struct {
	preview func(context.Context, runtimeapi.NetworkPreviewRequest) (runtimeapi.NetworkPreview, error)
	observe func(context.Context) (runtimeapi.NetworkStatus, error)
}

type trafficServiceStub struct {
	status      runtimeapi.TrafficStatus
	history     runtimeapi.TrafficHistory
	connections runtimeapi.ConnectionPage
}

type logServiceStub struct {
	page  runtimeapi.LogPage
	query runtimeapi.LogQuery
}

func (service *logServiceStub) Query(query runtimeapi.LogQuery) (runtimeapi.LogPage, error) {
	service.query = query
	return service.page, nil
}

func (service trafficServiceStub) Status() runtimeapi.TrafficStatus {
	return service.status
}

func (service trafficServiceStub) History(runtimeapi.TrafficHistoryRequest) runtimeapi.TrafficHistory {
	return service.history
}

func (service trafficServiceStub) Connections(runtimeapi.ConnectionQuery) runtimeapi.ConnectionPage {
	return service.connections
}

type productUpdateServiceFunc struct {
	preview func(context.Context, runtimeapi.PeerIdentity, runtimeapi.ProductUpdatePreviewRequest, []byte) (runtimeapi.ProductUpdatePlan, error)
	status  func(context.Context) (runtimeapi.UpdateStatus, error)
}

func (service productUpdateServiceFunc) Preview(
	ctx context.Context,
	peer runtimeapi.PeerIdentity,
	request runtimeapi.ProductUpdatePreviewRequest,
	body []byte,
) (runtimeapi.ProductUpdatePlan, error) {
	return service.preview(ctx, peer, request, body)
}

func (service productUpdateServiceFunc) Status(ctx context.Context) (runtimeapi.UpdateStatus, error) {
	if service.status == nil {
		return runtimeapi.UpdateStatus{}, nil
	}
	return service.status(ctx)
}

func (service networkServiceFunc) Preview(
	ctx context.Context,
	request runtimeapi.NetworkPreviewRequest,
) (runtimeapi.NetworkPreview, error) {
	return service.preview(ctx, request)
}

func (service networkServiceFunc) Observe(ctx context.Context) (runtimeapi.NetworkStatus, error) {
	return service.observe(ctx)
}

func (recovery *blockingStartupRecovery) RecoverStartup(context.Context) error {
	close(recovery.started)
	<-recovery.release
	return nil
}

func (*blockingStartupRecovery) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

func (e *scheduledExecutor) DueActions(time.Time) ([]runtimeapi.Action, error) {
	if e.delivered {
		return nil, nil
	}
	e.delivered = true
	return []runtimeapi.Action{e.action}, nil
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
	operation, duplicate, err := coordinator.Execute(context.Background(), peer, "test", "test", runtimeapi.CreateOperationRequest{
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

func TestCoordinatorUsesObservedNetworkStateAndTypedPreview(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	called := 0
	logs := &logServiceStub{page: runtimeapi.LogPage{Items: []runtimeapi.LogEntry{{Cursor: 9, Message: "ready"}}, LatestCursor: 9}}
	coordinator := &Coordinator{
		State: state,
		Network: networkServiceFunc{
			preview: func(
				_ context.Context,
				request runtimeapi.NetworkPreviewRequest,
			) (runtimeapi.NetworkPreview, error) {
				called++
				if request.IPv6Policy != runtimeapi.TUNIPv6Direct ||
					request.DNSPolicy != runtimeapi.TUNDNSOff {
					t.Fatalf("network preview request=%#v", request)
				}
				return runtimeapi.NetworkPreview{PlanID: "plan_0123456789abcdef0123456789abcdef"}, nil
			},
			observe: func(context.Context) (runtimeapi.NetworkStatus, error) {
				return runtimeapi.NetworkStatus{
					Available:   true,
					Mode:        runtimeapi.RunModeTUN,
					State:       runtimeapi.NetworkStateActive,
					OwnershipID: "net_0123456789abcdef",
				}, nil
			},
		},
		Traffic: trafficServiceStub{
			status: runtimeapi.TrafficStatus{Available: true, UploadTotal: 123, ActiveConnections: 2},
			history: runtimeapi.TrafficHistory{
				Samples:      []runtimeapi.TrafficSample{{Cursor: 7, UploadTotal: 123}},
				LatestCursor: 7,
			},
			connections: runtimeapi.ConnectionPage{
				Items: []runtimeapi.Connection{{ID: "connection-1", Target: "example.com:443"}},
				Total: 1, Page: 1, PageSize: 20,
			},
		},
		LogStore: logs,
		Version:  "test",
	}
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	preview, err := coordinator.PreviewNetwork(t.Context(), peer, runtimeapi.NetworkPreviewRequest{
		Mode:       runtimeapi.RunModeTUN,
		IPv6Policy: runtimeapi.TUNIPv6Direct,
		DNSPolicy:  runtimeapi.TUNDNSOff,
	})
	if err != nil || called != 1 || preview.PlanID == "" {
		t.Fatalf("network preview=%#v calls=%d err=%v", preview, called, err)
	}
	snapshot, err := coordinator.Observe(t.Context(), peer)
	if err != nil {
		t.Fatalf("observe Runtime network: %v", err)
	}
	if snapshot.RunMode != runtimeapi.RunModeTUN ||
		snapshot.Network.State != runtimeapi.NetworkStateActive ||
		snapshot.Network.OwnershipID == "" || snapshot.Traffic.UploadTotal != 123 ||
		snapshot.Traffic.ActiveConnections != 2 {
		t.Fatalf("observed Runtime network snapshot=%#v", snapshot)
	}
	history, err := coordinator.TrafficHistory(t.Context(), peer, runtimeapi.TrafficHistoryRequest{After: 6})
	if err != nil || history.LatestCursor != 7 || len(history.Samples) != 1 {
		t.Fatalf("traffic history=%#v err=%v", history, err)
	}
	connections, err := coordinator.Connections(t.Context(), peer, runtimeapi.ConnectionQuery{Target: "example"})
	if err != nil || connections.Total != 1 || connections.Items[0].ID != "connection-1" {
		t.Fatalf("connections=%#v err=%v", connections, err)
	}
	logPage, err := coordinator.Logs(t.Context(), peer, runtimeapi.LogQuery{After: 8, Limit: 20})
	if err != nil || logPage.LatestCursor != 9 || logs.query.After != 8 || logs.query.Limit != 20 {
		t.Fatalf("logs=%#v query=%#v err=%v", logPage, logs.query, err)
	}
}

func TestCoordinatorAcceptsOnlyBoundedConnectionCloseActions(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	coordinator := &Coordinator{
		State: state,
		Executor: executorFunc{execute: func(context.Context, runtimeapi.Operation, StageReporter) (*runtimeapi.OperationResult, error) {
			return &runtimeapi.OperationResult{}, nil
		}},
	}
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	requests := []runtimeapi.CreateOperationRequest{
		{
			RequestID: "close-one",
			Action: runtimeapi.Action{Kind: runtimeapi.ActionCloseConnection, Params: runtimeapi.ActionParams{
				ConnectionID: "connection-1", ConnectionTarget: "api.example.com:443",
			}},
		},
		{
			RequestID: "close-scope",
			Action: runtimeapi.Action{Kind: runtimeapi.ActionCloseConnections, Params: runtimeapi.ActionParams{
				ConnectionScope: &runtimeapi.ConnectionQuery{Node: "Tokyo"}, ConnectionScopeToken: strings.Repeat("a", 64), ConnectionCount: 2, Confirm: true,
			}},
		},
	}
	for _, request := range requests {
		snapshot, err := coordinator.Observe(t.Context(), peer)
		if err != nil {
			t.Fatal(err)
		}
		request.IfRevision = snapshot.Revision
		if _, _, err := coordinator.Execute(t.Context(), peer, "tui", "test", request); err != nil {
			t.Fatalf("valid connection action %q: %v", request.Action.Kind, err)
		}
	}
	invalid := runtimeapi.CreateOperationRequest{
		RequestID: "close-invalid",
		Action: runtimeapi.Action{Kind: runtimeapi.ActionCloseConnections, Params: runtimeapi.ActionParams{
			ConnectionScope: &runtimeapi.ConnectionQuery{Page: 1}, ConnectionCount: 0,
		}},
	}
	if _, _, err := coordinator.Execute(t.Context(), peer, "tui", "test", invalid); err == nil {
		t.Fatal("Runtime accepted unconfirmed, unbounded connection scope")
	}
}

func TestCoordinatorReportsUnavailableNetworkWithoutClaimingCleanup(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	coordinator := &Coordinator{
		State: state,
		Network: networkServiceFunc{
			preview: func(context.Context, runtimeapi.NetworkPreviewRequest) (runtimeapi.NetworkPreview, error) {
				return runtimeapi.NetworkPreview{}, nil
			},
			observe: func(context.Context) (runtimeapi.NetworkStatus, error) {
				return runtimeapi.NetworkStatus{
					Mode:  runtimeapi.RunModeTUN,
					State: runtimeapi.NetworkStateUnknown,
					Residuals: []runtimeapi.NetworkObject{{
						Kind: "policy_rule",
						ID:   "rule_test",
						Name: "12000",
					}},
				}, errors.New("helper unavailable")
			},
		},
		Version: "test",
	}
	snapshot, err := coordinator.Observe(
		t.Context(),
		runtimeapi.PeerIdentity{Platform: "linux", UID: 1000},
	)
	if err != nil {
		t.Fatalf("observe unavailable Runtime network: %v", err)
	}
	if snapshot.Network.State != runtimeapi.NetworkStateUnavailable ||
		snapshot.Network.Available ||
		snapshot.Network.Fault == nil ||
		len(snapshot.Network.Residuals) != 1 {
		t.Fatalf("unavailable Runtime network snapshot=%#v", snapshot.Network)
	}
}

func TestCoordinatorBindsOfflineProductPreviewToUploadedContentAndCaller(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	peer := runtimeapi.PeerIdentity{Platform: "linux", UID: 1000}
	body := []byte("bounded offline TUF product bundle")
	digest := sha256.Sum256(body)
	called := false
	coordinator := &Coordinator{
		State: state,
		ProductUpdates: productUpdateServiceFunc{
			preview: func(
				_ context.Context,
				gotPeer runtimeapi.PeerIdentity,
				request runtimeapi.ProductUpdatePreviewRequest,
				gotBody []byte,
			) (runtimeapi.ProductUpdatePlan, error) {
				called = true
				if gotPeer.Key() != peer.Key() ||
					request.Source != runtimeapi.ProductUpdateSourceOfflineTUF ||
					!bytes.Equal(gotBody, body) {
					t.Fatalf("offline product preview peer=%#v request=%#v body=%q", gotPeer, request, gotBody)
				}
				return runtimeapi.ProductUpdatePlan{
					PlanID:  "product_plan_" + strings.Repeat("a", 32),
					Version: "v2.0.0",
				}, nil
			},
		},
	}
	content, err := coordinator.UploadImport(
		t.Context(),
		peer,
		runtimeapi.ProductUpdateBundleContentType,
		int64(len(body)),
		hex.EncodeToString(digest[:]),
		body,
	)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := coordinator.PreviewProductUpdate(
		t.Context(),
		peer,
		"cli",
		"test",
		"request-product-preview",
		runtimeapi.ProductUpdatePreviewRequest{
			Source:    runtimeapi.ProductUpdateSourceOfflineTUF,
			ContentID: content.ID,
		},
	)
	if err != nil || !called || preview.PlanID == "" {
		t.Fatalf("offline product preview=%#v called=%v err=%v", preview, called, err)
	}
	if _, _, err := state.PeekImport(content.ID, peer.Key(), time.Now().UTC()); err == nil {
		t.Fatal("offline product bundle remained after the verified plan copied it")
	}
	other := runtimeapi.PeerIdentity{Platform: "linux", UID: 1001}
	if _, err := coordinator.PreviewProductUpdate(
		t.Context(),
		other,
		"cli",
		"test",
		"request-product-preview-other",
		runtimeapi.ProductUpdatePreviewRequest{
			Source:    runtimeapi.ProductUpdateSourceOfflineTUF,
			ContentID: content.ID,
		},
	); err == nil {
		t.Fatal("offline product bundle was accepted for a different caller")
	}
}

func TestProductActionsRequireFixedPlanTrustAndConfirmation(t *testing.T) {
	valid := []runtimeapi.Action{
		{Kind: runtimeapi.ActionCheckProduct},
		{
			Kind: runtimeapi.ActionUpdateProduct,
			Params: runtimeapi.ActionParams{
				PlanID:  "product_plan_" + strings.Repeat("a", 32),
				Trust:   runtimeapi.ProductUpdateTrustTUF,
				Confirm: true,
			},
		},
		{
			Kind:   runtimeapi.ActionRollbackProduct,
			Params: runtimeapi.ActionParams{Confirm: true},
		},
	}
	for _, action := range valid {
		if err := validateAction(action); err != nil {
			t.Fatalf("valid product action %#v: %v", action, err)
		}
	}
	invalid := valid[1]
	invalid.Params.Trust = "arbitrary"
	if err := validateAction(invalid); err == nil {
		t.Fatal("product update accepted arbitrary trust")
	}
	invalid = valid[1]
	invalid.Params.PlanID = "plan_" + strings.Repeat("a", 32)
	if err := validateAction(invalid); err == nil {
		t.Fatal("product update accepted a non-product plan")
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
	operation, _, err := coordinator.Execute(context.Background(), peer, "test", "test", runtimeapi.CreateOperationRequest{
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
	cancelledOperation, _, err := coordinator.CancelOperation(context.Background(), peer, "test", "test", operation.ID, runtimeapi.CancelOperationRequest{
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
	operation, _, err := coordinator.Execute(clientContext, peer, "test", "test", runtimeapi.CreateOperationRequest{
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

func TestCoordinatorRevealsSourceOnlyAfterConfirmationAndAuditsNoSecret(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	source, err := state.CreateRemoteSource(runtimestate.RemoteSourceRecord{
		ID:                     "src_0123456789abcdef0123456789abcdef",
		Type:                   runtimeapi.SourceTypeRemoteHTTP,
		Name:                   "primary",
		URL:                    "https://user:pass@example.com/config?token=one&token=two",
		RedactedTarget:         "https://example.com:443/…",
		Route:                  runtimeapi.SourceRouteDirect,
		RefreshIntervalSeconds: 900,
		TimeoutSeconds:         30,
		MaxResponseBytes:       8 << 20,
	}, []byte("proxies: []\n"), []byte("mixed-port: 7890\n"), "op-source", now)
	if err != nil {
		t.Fatalf("create Runtime source: %v", err)
	}
	coordinator := &Coordinator{State: state, Version: "test", Now: func() time.Time { return now }}
	peer := runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-21-test"}
	if _, err := coordinator.RevealSourceURL(context.Background(), peer, "gui", "test", "request-denied", runtimeapi.RevealSourceURLRequest{
		SourceID: source.ID,
	}); err == nil {
		t.Fatal("source URL was revealed without confirmation")
	}
	response, err := coordinator.RevealSourceURL(context.Background(), peer, "gui", "test", "request-reveal", runtimeapi.RevealSourceURLRequest{
		SourceID: source.ID,
		Confirm:  true,
	})
	if err != nil || response.URL != source.URL {
		t.Fatalf("reveal Runtime source URL response=%#v err=%v", response, err)
	}
	audit, err := state.RecentAudit(10)
	if err != nil || len(audit) != 1 {
		t.Fatalf("read source reveal audit=%#v err=%v", audit, err)
	}
	record := audit[0]
	if record.Actor != peer.Key() || record.ClientType != "gui" || record.ClientVersion != "test" ||
		record.RequestID != "request-reveal" || record.Action != "source.reveal_url" ||
		record.ObjectID != source.ID || record.Result != "succeeded" {
		t.Fatalf("source reveal audit=%#v", record)
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatalf("encode source reveal audit: %v", err)
	}
	for _, secret := range []string{"user:pass", "token=one", "token=two"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("source reveal audit leaked %q: %s", secret, encoded)
		}
	}
}

func TestCoordinatorPersistsAndRunsDueSourceRefreshAsRuntimeCaller(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	sourceID := "src_0123456789abcdef0123456789abcdef"
	executed := make(chan runtimeapi.Operation, 1)
	executor := &scheduledExecutor{
		action: runtimeapi.Action{
			Kind: runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{
				SourceID: sourceID,
			},
		},
	}
	executor.execute = func(
		_ context.Context,
		operation runtimeapi.Operation,
		report StageReporter,
	) (*runtimeapi.OperationResult, error) {
		if err := report("refreshing_source", 50, true); err != nil {
			return nil, err
		}
		executed <- operation
		return &runtimeapi.OperationResult{
			SourceID:      sourceID,
			RefreshResult: "not_modified",
		}, nil
	}
	coordinator := &Coordinator{
		State:    state,
		Executor: executor,
		Version:  "test",
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- coordinator.Run(ctx) }()

	var operation runtimeapi.Operation
	select {
	case operation = <-executed:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("due source refresh was not scheduled")
	}
	if operation.Action.Kind != runtimeapi.ActionRefreshSource ||
		operation.CallerIdentity != "runtime:uid:0" ||
		!strings.HasPrefix(operation.RequestID, "scheduled-") {
		cancel()
		t.Fatalf("scheduled operation = %#v", operation)
	}
	waitForOperationState(t, state, operation.ID, runtimeapi.OperationSucceeded)
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("stop Runtime coordinator: %v", err)
	}
}

func TestCoordinatorCompletesStartupRecoveryBeforeDueRefresh(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	executed := make(chan struct{}, 1)
	executor := &scheduledExecutor{
		action: runtimeapi.Action{
			Kind: runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{
				SourceID: "src_0123456789abcdef0123456789abcdef",
			},
		},
	}
	executor.execute = func(
		context.Context,
		runtimeapi.Operation,
		StageReporter,
	) (*runtimeapi.OperationResult, error) {
		executed <- struct{}{}
		return &runtimeapi.OperationResult{RefreshResult: "not_modified"}, nil
	}
	recovery := &blockingStartupRecovery{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	coordinator := &Coordinator{
		State:    state,
		Executor: executor,
		Recovery: recovery,
		Version:  "test",
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- coordinator.Run(ctx) }()
	select {
	case <-recovery.started:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Mihomo startup recovery did not start")
	}
	select {
	case <-executed:
		cancel()
		t.Fatal("due source refresh ran before Mihomo startup recovery completed")
	default:
	}
	close(recovery.release)
	select {
	case <-executed:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("due source refresh did not run after Mihomo startup recovery")
	}
	cancel()
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

func TestAdvancedOverrideReadRequiresConfirmationAndWritesContentFreeAudit(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	override := []byte("proxies:\n  - password: super-secret\n")
	if _, err := state.SetAdvancedOverride(override, "op-override", now); err != nil {
		t.Fatalf("set Runtime advanced override: %v", err)
	}
	coordinator := &Coordinator{
		State: state,
		Now:   func() time.Time { return now.Add(time.Minute) },
	}
	peer := runtimeapi.PeerIdentity{Platform: "windows", SID: "S-1-5-21-test"}

	if _, err := coordinator.GetAdvancedOverride(
		t.Context(),
		peer,
		"cli",
		"test",
		"request-unconfirmed",
		false,
	); err == nil {
		t.Fatal("advanced override read did not require confirmation")
	}
	document, err := coordinator.GetAdvancedOverride(
		t.Context(),
		peer,
		"cli",
		"test",
		"request-confirmed",
		true,
	)
	if err != nil || document.YAML != string(override) {
		t.Fatalf("read advanced override document=%#v err=%v", document, err)
	}
	audit, err := state.RecentAudit(10)
	if err != nil {
		t.Fatalf("read Runtime audit: %v", err)
	}
	if len(audit) != 1 || audit[0].Action != "override.reveal" ||
		audit[0].Actor != peer.Key() || audit[0].ObjectID != "advanced-override" {
		t.Fatalf("advanced override audit=%#v", audit)
	}
	encoded, err := json.Marshal(audit[0])
	if err != nil {
		t.Fatalf("encode Runtime audit: %v", err)
	}
	if strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), "proxies:") {
		t.Fatalf("advanced override audit leaked content: %s", encoded)
	}
}
