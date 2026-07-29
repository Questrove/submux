package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"submux/internal/buildinfo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimeapp"
	"submux/internal/runtimeipc"
	"submux/internal/runtimestate"
)

type commandObserver struct{}

func (commandObserver) Observe(_ context.Context, _ runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	return runtimeapi.Snapshot{
		ProtocolVersion:   runtimeapi.ProtocolVersion,
		Revision:          11,
		Runtime:           runtimeapi.RuntimeStatus{Version: "v1.2.3", ServiceState: "running"},
		Mihomo:            runtimeapi.MihomoStatus{State: "not_installed", Recovery: "idle"},
		RunMode:           "unconfigured",
		LatestEventCursor: 12,
		ObservedAt:        time.Now().UTC(),
	}, nil
}

type sourceListObserver struct{}

func (sourceListObserver) Observe(_ context.Context, _ runtimeapi.PeerIdentity) (runtimeapi.Snapshot, error) {
	currentID := "src_" + strings.Repeat("a", 32)
	return runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        15,
		Runtime:         runtimeapi.RuntimeStatus{Version: buildinfo.Current().Version, ServiceState: "running"},
		Sources: runtimeapi.SourceStatus{
			Count:           2,
			CurrentSourceID: currentID,
			Items: []runtimeapi.SourceSummary{
				{
					ID:             currentID,
					Type:           runtimeapi.SourceTypeSubmuxOutput,
					Name:           "generated",
					Current:        true,
					RedactedTarget: "https://submux.example:443/…",
					Route:          runtimeapi.SourceRouteDirect,
				},
				{
					ID:                    "src_" + strings.Repeat("b", 32),
					Type:                  runtimeapi.SourceTypeLocalImport,
					Name:                  "local-copy",
					RedactedTarget:        "Runtime-managed local copy",
					HasValidatedCandidate: true,
				},
			},
		},
	}, nil
}

type commandExecutor struct {
	state *runtimestate.Store
}

func (e commandExecutor) Execute(
	_ context.Context,
	operation runtimeapi.Operation,
	report runtimeapp.StageReporter,
) (*runtimeapi.OperationResult, error) {
	var imported []byte
	if operation.Action.Kind == runtimeapi.ActionApplyImportedConfig ||
		operation.Action.Kind == runtimeapi.ActionAddRemoteSource ||
		operation.Action.Kind == runtimeapi.ActionAddImportedSource ||
		operation.Action.Kind == runtimeapi.ActionAddManagedResource ||
		operation.Action.Kind == runtimeapi.ActionSetAdvancedOverride {
		var err error
		imported, _, err = e.state.ConsumeImport(
			operation.Action.Params.ContentID,
			operation.CallerIdentity,
			operation.ID,
			time.Now().UTC(),
		)
		if err != nil {
			return nil, err
		}
	}
	if err := report("committing", 70, false); err != nil {
		return nil, err
	}
	result := &runtimeapi.OperationResult{
		ConfigRevision: operation.ID,
		ProxyKind:      "mixed",
		ProxyAddresses: []string{"127.0.0.1:7890", "[::1]:7890"},
		Verified:       true,
	}
	if operation.Action.Kind == runtimeapi.ActionAddRemoteSource {
		result.SourceID = "src_" + strings.Repeat("a", 32)
		result.RefreshResult = "validated"
		result.RefreshRoute = runtimeapi.SourceRouteDirect
	}
	if operation.Action.Kind == runtimeapi.ActionAddImportedSource {
		result.SourceID = "src_" + strings.Repeat("b", 32)
		result.RefreshResult = "imported"
	}
	if operation.Action.Kind == runtimeapi.ActionRefreshSource {
		result.SourceID = operation.Action.Params.SourceID
		result.RefreshResult = "not_modified"
		result.RefreshRoute = operation.Action.Params.Route
		result.NotModified = true
	}
	if operation.Action.Kind == runtimeapi.ActionApplySource {
		result.SourceID = operation.Action.Params.SourceID
	}
	if operation.Action.Kind == runtimeapi.ActionSwitchSource {
		result.SourceID = operation.Action.Params.SourceID
		result.PreviousSourceID = "src_" + strings.Repeat("a", 32)
		result.UsedCachedSource = operation.Action.Params.UseCached
	}
	if operation.Action.Kind == runtimeapi.ActionDeleteSource {
		result.SourceID = operation.Action.Params.SourceID
		result.Deleted = true
	}
	if operation.Action.Kind == runtimeapi.ActionAddManagedResource {
		record, err := e.state.CreateManagedResource(
			operation.Action.Params.ResourceName,
			operation.Action.Params.ResourceKind,
			imported,
			operation.ID,
			time.Now().UTC(),
		)
		if err != nil {
			return nil, err
		}
		result.ResourceID = record.ID
		result.ResourceKind = record.Kind
	}
	if operation.Action.Kind == runtimeapi.ActionSetAdvancedOverride {
		record, err := e.state.SetAdvancedOverride(imported, operation.ID, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		result.AdvancedOverrideSHA256 = record.SHA256
	}
	return result, nil
}

func (commandExecutor) Verify(context.Context) (runtimeapi.ProxyVerification, error) {
	return runtimeapi.ProxyVerification{
		Available: true,
		Kind:      "mixed",
		Addresses: []string{"127.0.0.1:7890", "[::1]:7890"},
		CheckedAt: time.Now().UTC(),
	}, nil
}

func (commandExecutor) PreviewCandidate(
	_ context.Context,
	_ runtimeapi.PeerIdentity,
	request runtimeapi.PreviewCandidateRequest,
) (runtimeapi.CandidatePreview, error) {
	return runtimeapi.CandidatePreview{
		ContentID:       request.ContentID,
		CandidateYAML:   "listeners: []\n",
		CandidateSHA256: strings.Repeat("0", 64),
		ProxyKind:       "mixed",
		ProxyAddresses:  []string{"127.0.0.1:7890", "[::1]:7890"},
		Validated:       true,
	}, nil
}

func TestStatusJSONUsesLocalIPC(t *testing.T) {
	endpoint := commandTestEndpoint(t)
	listener, err := runtimeipc.Listen(endpoint)
	if err != nil {
		t.Fatalf("listen on Runtime IPC: %v", err)
	}
	authorizer, err := runtimeipc.CurrentUserAuthorizer()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime authorizer: %v", err)
	}
	server, err := runtimeipc.NewServer(commandObserver{}, authorizer)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx, listener) }()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"status", "--endpoint", endpoint, "--json"}, &stdout, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("status exit code = %d; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	var snapshot runtimeapi.Snapshot
	if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
		cancel()
		t.Fatalf("decode status JSON: %v; output=%s", err, stdout.String())
	}
	if snapshot.Revision != 11 || snapshot.LatestEventCursor != 12 {
		cancel()
		t.Fatalf("status snapshot = %#v", snapshot)
	}
	if stderr.Len() != 0 {
		cancel()
		t.Fatalf("status stderr = %s", stderr.String())
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("stop Runtime IPC server: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Runtime IPC server did not stop")
	}
}

func TestSourceListJSONUsesSnapshotSourceTypesAndCurrentMarker(t *testing.T) {
	endpoint := commandTestEndpoint(t)
	listener, err := runtimeipc.Listen(endpoint)
	if err != nil {
		t.Fatalf("listen on Runtime IPC: %v", err)
	}
	authorizer, err := runtimeipc.CurrentUserAuthorizer()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime authorizer: %v", err)
	}
	server, err := runtimeipc.NewServer(sourceListObserver{}, authorizer)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime IPC server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.Serve(ctx, listener) }()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := runSourceList([]string{"--endpoint", endpoint, "--json"}, &stdout, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("source list exit=%d stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	var status runtimeapi.SourceStatus
	if err := json.Unmarshal(stdout.Bytes(), &status); err != nil {
		cancel()
		t.Fatalf("decode source list: %v; output=%s", err, stdout.String())
	}
	if status.Count != 2 ||
		status.Items[0].Type != runtimeapi.SourceTypeSubmuxOutput ||
		!status.Items[0].Current ||
		status.Items[1].Type != runtimeapi.SourceTypeLocalImport {
		cancel()
		t.Fatalf("source list status = %#v", status)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("serve Runtime IPC: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Runtime IPC server did not stop")
	}
}

func TestStatusJSONReturnsStableUnavailableEnvelope(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"status", "--endpoint", "not-a-local-endpoint", "--json"}, &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("status exit code = %d, want 1; stdout=%s stderr=%s", exitCode, stdout.String(), stderr.String())
	}
	var envelope runtimeapi.ErrorEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode status error JSON: %v; output=%s", err, stdout.String())
	}
	if envelope.Error.Code != runtimeapi.ErrorServiceUnavailable || !envelope.Error.Retryable {
		t.Fatalf("status error = %#v", envelope.Error)
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON status stderr = %s", stderr.String())
	}
}

func TestVersionAndUsage(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if exitCode := run([]string{"--version-json"}, &stdout, &stderr); exitCode != 0 {
		t.Fatalf("version exit code = %d", exitCode)
	}
	if !json.Valid(stdout.Bytes()) {
		t.Fatalf("version output is not JSON: %s", stdout.String())
	}
	stdout.Reset()
	if exitCode := run([]string{"unknown"}, &stdout, &stderr); exitCode != 2 {
		t.Fatalf("unknown command exit code = %d, want 2", exitCode)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("usage stderr = %s", stderr.String())
	}
}

func TestServeLockFileCannotBeOverridden(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode := run([]string{"serve", "--lock-file", filepath.Join(t.TempDir(), "other.lock")}, &stdout, &stderr)
	if exitCode != 2 {
		t.Fatalf("serve exit code = %d, want 2; stderr=%s", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("serve accepted lock override: %s", stderr.String())
	}
}

func TestImportProxyStartWaitQueryAndVerifyCLI(t *testing.T) {
	state, err := runtimestate.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatalf("open Runtime state: %v", err)
	}
	defer state.Close()
	coordinator := &runtimeapp.Coordinator{
		State:    state,
		Executor: commandExecutor{state: state},
		Version:  buildinfo.Current().Version,
	}
	endpoint := commandTestEndpoint(t)
	listener, err := runtimeipc.Listen(endpoint)
	if err != nil {
		t.Fatalf("listen on Runtime IPC: %v", err)
	}
	authorizer, err := runtimeipc.CurrentUserAuthorizer()
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime authorizer: %v", err)
	}
	server, err := runtimeipc.NewServer(coordinator, authorizer)
	if err != nil {
		_ = listener.Close()
		t.Fatalf("create Runtime server: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerResult := make(chan error, 1)
	serverResult := make(chan error, 1)
	go func() { workerResult <- coordinator.Run(ctx) }()
	go func() { serverResult <- server.Serve(ctx, listener) }()

	var importOut bytes.Buffer
	var stderr bytes.Buffer
	source := "proxies: []\nrules: []\n"
	exitCode := runImport(
		[]string{"--endpoint", endpoint, "--json", "-"},
		strings.NewReader(source),
		&importOut,
		&stderr,
	)
	if exitCode != 0 {
		cancel()
		t.Fatalf("import exit=%d stdout=%s stderr=%s", exitCode, importOut.String(), stderr.String())
	}
	var content runtimeapi.ImportContent
	if err := json.Unmarshal(importOut.Bytes(), &content); err != nil {
		cancel()
		t.Fatalf("decode CLI import: %v", err)
	}

	var previewOut bytes.Buffer
	stderr.Reset()
	exitCode = runProxy([]string{
		"preview",
		"--endpoint", endpoint,
		"--content-id", content.ID,
		"--json",
	}, &previewOut, &stderr)
	if exitCode != 0 || !json.Valid(previewOut.Bytes()) {
		cancel()
		t.Fatalf("proxy preview exit=%d stdout=%s stderr=%s", exitCode, previewOut.String(), stderr.String())
	}

	var applyOut bytes.Buffer
	stderr.Reset()
	exitCode = runProxy([]string{
		"apply",
		"--endpoint", endpoint,
		"--content-id", content.ID,
		"--wait",
		"--json",
	}, &applyOut, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("proxy apply exit=%d stdout=%s stderr=%s", exitCode, applyOut.String(), stderr.String())
	}
	var operationEnvelope runtimeapi.OperationResponse
	if err := json.Unmarshal(applyOut.Bytes(), &operationEnvelope); err != nil {
		cancel()
		t.Fatalf("decode CLI operation: %v", err)
	}
	if operationEnvelope.Operation.State != runtimeapi.OperationSucceeded {
		cancel()
		t.Fatalf("CLI operation = %#v", operationEnvelope.Operation)
	}

	var startOut bytes.Buffer
	stderr.Reset()
	exitCode = runProxy([]string{
		"start",
		"--endpoint", endpoint,
		"--wait",
		"--json",
	}, &startOut, &stderr)
	if exitCode != 0 || !json.Valid(startOut.Bytes()) {
		cancel()
		t.Fatalf("proxy start exit=%d stdout=%s stderr=%s", exitCode, startOut.String(), stderr.String())
	}

	var getOut bytes.Buffer
	stderr.Reset()
	exitCode = runOperation([]string{
		"get",
		"--endpoint", endpoint,
		"--json",
		operationEnvelope.Operation.ID,
	}, &getOut, &stderr)
	if exitCode != 0 || !json.Valid(getOut.Bytes()) {
		cancel()
		t.Fatalf("operation get exit=%d stdout=%s stderr=%s", exitCode, getOut.String(), stderr.String())
	}

	var verifyOut bytes.Buffer
	stderr.Reset()
	exitCode = runProxy([]string{"verify", "--endpoint", endpoint, "--json"}, &verifyOut, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("proxy verify exit=%d stdout=%s stderr=%s", exitCode, verifyOut.String(), stderr.String())
	}
	var verification runtimeapi.ProxyVerification
	if err := json.Unmarshal(verifyOut.Bytes(), &verification); err != nil || !verification.Available {
		cancel()
		t.Fatalf("CLI verification = %#v err=%v", verification, err)
	}

	var sourceAddOut bytes.Buffer
	stderr.Reset()
	exitCode = runSourceAdd([]string{
		"--endpoint", endpoint,
		"--type", runtimeapi.SourceTypeSubmuxOutput,
		"--name", "local-source",
		"--url", "http://127.0.0.1:8080/config.yaml?token=secret",
		"--json",
	}, strings.NewReader(""), &sourceAddOut, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("source add exit=%d stdout=%s stderr=%s", exitCode, sourceAddOut.String(), stderr.String())
	}
	var sourceAddOperation runtimeapi.OperationResponse
	if err := json.Unmarshal(sourceAddOut.Bytes(), &sourceAddOperation); err != nil ||
		sourceAddOperation.Operation.Result == nil ||
		sourceAddOperation.Operation.Result.SourceID == "" {
		cancel()
		t.Fatalf("source add operation = %#v err=%v", sourceAddOperation, err)
	}

	var sourceImportOut bytes.Buffer
	stderr.Reset()
	exitCode = runSourceImport([]string{
		"--endpoint", endpoint,
		"--name", "offline-copy",
		"--json",
		"-",
	}, strings.NewReader(source), &sourceImportOut, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("source import exit=%d stdout=%s stderr=%s", exitCode, sourceImportOut.String(), stderr.String())
	}
	var sourceImportOperation runtimeapi.OperationResponse
	if err := json.Unmarshal(sourceImportOut.Bytes(), &sourceImportOperation); err != nil ||
		sourceImportOperation.Operation.Result == nil ||
		sourceImportOperation.Operation.Result.SourceID == "" {
		cancel()
		t.Fatalf("source import operation = %#v err=%v", sourceImportOperation, err)
	}

	var sourceSwitchOut bytes.Buffer
	stderr.Reset()
	exitCode = runSourceSwitch([]string{
		"--endpoint", endpoint,
		"--use-cache",
		"--json",
		sourceImportOperation.Operation.Result.SourceID,
	}, &sourceSwitchOut, &stderr)
	var sourceSwitchOperation runtimeapi.OperationResponse
	if exitCode != 0 ||
		json.Unmarshal(sourceSwitchOut.Bytes(), &sourceSwitchOperation) != nil ||
		sourceSwitchOperation.Operation.Result == nil ||
		!sourceSwitchOperation.Operation.Result.UsedCachedSource {
		cancel()
		t.Fatalf("source switch exit=%d operation=%#v stdout=%s stderr=%s",
			exitCode, sourceSwitchOperation, sourceSwitchOut.String(), stderr.String())
	}

	var sourceDeleteOut bytes.Buffer
	stderr.Reset()
	exitCode = runSourceDelete([]string{
		"--endpoint", endpoint,
		"--json",
		sourceAddOperation.Operation.Result.SourceID,
	}, &sourceDeleteOut, &stderr)
	var sourceDeleteOperation runtimeapi.OperationResponse
	if exitCode != 0 ||
		json.Unmarshal(sourceDeleteOut.Bytes(), &sourceDeleteOperation) != nil ||
		sourceDeleteOperation.Operation.Result == nil ||
		!sourceDeleteOperation.Operation.Result.Deleted {
		cancel()
		t.Fatalf("source delete exit=%d operation=%#v stdout=%s stderr=%s",
			exitCode, sourceDeleteOperation, sourceDeleteOut.String(), stderr.String())
	}

	var sourceRefreshOut bytes.Buffer
	stderr.Reset()
	exitCode = runSourceRefresh([]string{
		"--endpoint", endpoint,
		"--route", runtimeapi.SourceRouteMihomo,
		"--json",
		sourceAddOperation.Operation.Result.SourceID,
	}, &sourceRefreshOut, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("source refresh exit=%d stdout=%s stderr=%s", exitCode, sourceRefreshOut.String(), stderr.String())
	}
	var sourceRefreshOperation runtimeapi.OperationResponse
	if err := json.Unmarshal(sourceRefreshOut.Bytes(), &sourceRefreshOperation); err != nil ||
		sourceRefreshOperation.Operation.Result == nil ||
		sourceRefreshOperation.Operation.Result.RefreshRoute != runtimeapi.SourceRouteMihomo {
		cancel()
		t.Fatalf("source refresh operation = %#v err=%v", sourceRefreshOperation, err)
	}

	var sourceApplyOut bytes.Buffer
	stderr.Reset()
	exitCode = runSourceApply([]string{
		"--endpoint", endpoint,
		"--json",
		sourceAddOperation.Operation.Result.SourceID,
	}, &sourceApplyOut, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("source apply exit=%d stdout=%s stderr=%s", exitCode, sourceApplyOut.String(), stderr.String())
	}
	var sourceApplyOperation runtimeapi.OperationResponse
	if err := json.Unmarshal(sourceApplyOut.Bytes(), &sourceApplyOperation); err != nil ||
		sourceApplyOperation.Operation.Result == nil ||
		sourceApplyOperation.Operation.Result.SourceID != sourceAddOperation.Operation.Result.SourceID {
		cancel()
		t.Fatalf("source apply operation = %#v err=%v", sourceApplyOperation, err)
	}

	var sourcePreviewOut bytes.Buffer
	stderr.Reset()
	exitCode = runProxy([]string{
		"preview",
		"--endpoint", endpoint,
		"--source-id", sourceAddOperation.Operation.Result.SourceID,
		"--override-content-id", content.ID,
		"--json",
	}, &sourcePreviewOut, &stderr)
	if exitCode != 0 || !json.Valid(sourcePreviewOut.Bytes()) {
		cancel()
		t.Fatalf("source preview exit=%d stdout=%s stderr=%s", exitCode, sourcePreviewOut.String(), stderr.String())
	}

	var resourceAddOut bytes.Buffer
	stderr.Reset()
	exitCode = runResourceAdd([]string{
		"--endpoint", endpoint,
		"--name", "provider.main",
		"--kind", runtimeapi.ResourceKindProxyProvider,
		"--json",
		"-",
	}, strings.NewReader("proxies:\n  - name: local\n"), &resourceAddOut, &stderr)
	if exitCode != 0 {
		cancel()
		t.Fatalf("resource add exit=%d stdout=%s stderr=%s", exitCode, resourceAddOut.String(), stderr.String())
	}
	var resourceOperation runtimeapi.OperationResponse
	if err := json.Unmarshal(resourceAddOut.Bytes(), &resourceOperation); err != nil ||
		resourceOperation.Operation.Result == nil ||
		resourceOperation.Operation.Result.ResourceID == "" {
		cancel()
		t.Fatalf("resource add operation = %#v err=%v", resourceOperation, err)
	}
	var resourceListOut bytes.Buffer
	stderr.Reset()
	exitCode = runResourceList(
		[]string{"--endpoint", endpoint, "--json"},
		&resourceListOut,
		&stderr,
	)
	var resourceStatus runtimeapi.ResourceStatus
	if exitCode != 0 || json.Unmarshal(resourceListOut.Bytes(), &resourceStatus) != nil ||
		resourceStatus.Count != 1 {
		cancel()
		t.Fatalf("resource list exit=%d status=%#v stdout=%s stderr=%s",
			exitCode, resourceStatus, resourceListOut.String(), stderr.String())
	}

	var overrideSetOut bytes.Buffer
	stderr.Reset()
	overrideYAML := "rules:\n  - MATCH,DIRECT\n"
	exitCode = runOverrideSet(
		[]string{"--endpoint", endpoint, "--json", "-"},
		strings.NewReader(overrideYAML),
		&overrideSetOut,
		&stderr,
	)
	if exitCode != 0 {
		cancel()
		t.Fatalf("override set exit=%d stdout=%s stderr=%s", exitCode, overrideSetOut.String(), stderr.String())
	}
	var overrideGetOut bytes.Buffer
	stderr.Reset()
	exitCode = runOverrideGet([]string{"--endpoint", endpoint}, &overrideGetOut, &stderr)
	if exitCode != 0 || overrideGetOut.String() != overrideYAML {
		cancel()
		t.Fatalf("override get exit=%d stdout=%q stderr=%s", exitCode, overrideGetOut.String(), stderr.String())
	}

	cancel()
	if err := <-serverResult; err != nil {
		t.Fatalf("stop Runtime server: %v", err)
	}
	if err := <-workerResult; err != nil {
		t.Fatalf("stop Runtime coordinator: %v", err)
	}
}

func TestOpenedContentFileMustMatchInspectedFile(t *testing.T) {
	root := t.TempDir()
	firstPath := filepath.Join(root, "first.yaml")
	secondPath := filepath.Join(root, "second.yaml")
	if err := os.WriteFile(firstPath, []byte("rules: []\n"), 0600); err != nil {
		t.Fatalf("write first content file: %v", err)
	}
	if err := os.WriteFile(secondPath, []byte("proxies: []\n"), 0600); err != nil {
		t.Fatalf("write second content file: %v", err)
	}
	first, err := os.Lstat(firstPath)
	if err != nil {
		t.Fatalf("inspect first content file: %v", err)
	}
	second, err := os.Stat(secondPath)
	if err != nil {
		t.Fatalf("inspect second content file: %v", err)
	}
	if err := validateOpenedSmallRegularFile(first, second, 1024); err == nil {
		t.Fatal("accepted a content file that changed while opening")
	}
}

func commandTestEndpoint(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return fmt.Sprintf(`\\.\pipe\submux-runtime-test-%d`, time.Now().UnixNano())
	}
	return filepath.Join(t.TempDir(), "runtime.sock")
}
