package runtimetui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

type fakeClient struct {
	snapshot           runtimeapi.Snapshot
	actions            []runtimeapi.Action
	uploaded           []byte
	uploadedType       string
	previewed          string
	gotten             string
	waited             string
	cancelled          string
	verifyCalls        int
	overrideReads      int
	revealCalls        int
	diagnosticPreviews int
	diagnosticCreates  int
	operationSerial    int
	networkRequests    []runtimeapi.NetworkPreviewRequest
	networkPreview     runtimeapi.NetworkPreview
	updatePreview      runtimeapi.MihomoUpdatePlan
	productRequests    []runtimeapi.ProductUpdatePreviewRequest
	productPreview     runtimeapi.ProductUpdatePlan
	backupPreviews     int
	backupExports      int
	backupInspections  int
	lastBackupExport   runtimeapi.BackupExportRequest
	trafficRequests    []runtimeapi.TrafficHistoryRequest
	trafficHistories   []runtimeapi.TrafficHistory
	trafficError       error
	connectionRequests []runtimeapi.ConnectionQuery
	connectionPages    []runtimeapi.ConnectionPage
	connectionError    error
	ruleRequests       []runtimeapi.RuleQuery
	ruleSets           []runtimeapi.RuleSet
	ruleError          error
	candidateRequests  []runtimeapi.PreviewCandidateRequest
}

func (f *fakeClient) Rules(_ context.Context, query runtimeapi.RuleQuery) (runtimeapi.RuleSet, error) {
	index := len(f.ruleRequests)
	f.ruleRequests = append(f.ruleRequests, query)
	if f.ruleError != nil {
		return runtimeapi.RuleSet{}, f.ruleError
	}
	if len(f.ruleSets) == 0 {
		return runtimeapi.RuleSet{View: runtimeapi.RuleViewApplied, Items: []runtimeapi.FinalRule{}, Total: 0}, nil
	}
	if index >= len(f.ruleSets) {
		index = len(f.ruleSets) - 1
	}
	return f.ruleSets[index], nil
}

func (f *fakeClient) Connections(_ context.Context, query runtimeapi.ConnectionQuery) (runtimeapi.ConnectionPage, error) {
	index := len(f.connectionRequests)
	f.connectionRequests = append(f.connectionRequests, query)
	if f.connectionError != nil {
		return runtimeapi.ConnectionPage{}, f.connectionError
	}
	if len(f.connectionPages) == 0 {
		return runtimeapi.ConnectionPage{Page: query.Page, PageSize: query.PageSize, Available: true}, nil
	}
	if index >= len(f.connectionPages) {
		index = len(f.connectionPages) - 1
	}
	return f.connectionPages[index], nil
}

func (f *fakeClient) TrafficHistory(_ context.Context, request runtimeapi.TrafficHistoryRequest) (runtimeapi.TrafficHistory, error) {
	index := len(f.trafficRequests)
	f.trafficRequests = append(f.trafficRequests, request)
	if f.trafficError != nil {
		return runtimeapi.TrafficHistory{}, f.trafficError
	}
	if len(f.trafficHistories) == 0 {
		return runtimeapi.TrafficHistory{Status: f.snapshot.Traffic}, nil
	}
	if index >= len(f.trafficHistories) {
		index = len(f.trafficHistories) - 1
	}
	return f.trafficHistories[index], nil
}

func (f *fakeClient) Observe(context.Context) (runtimeapi.Snapshot, error) {
	return f.snapshot, nil
}

func (f *fakeClient) WatchEvents(ctx context.Context, _ uint64, _ func(runtimeapi.Event) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeClient) UploadImport(_ context.Context, contentType string, body []byte) (runtimeapi.ImportContent, error) {
	f.uploaded = append([]byte(nil), body...)
	f.uploadedType = contentType
	return runtimeapi.ImportContent{ID: "content_0123456789abcdef0123456789abcdef"}, nil
}

func (f *fakeClient) GetAdvancedOverride(_ context.Context, reveal bool) (runtimeapi.AdvancedOverrideDocument, error) {
	if !reveal {
		return runtimeapi.AdvancedOverrideDocument{}, errors.New("override reveal was not confirmed")
	}
	f.overrideReads++
	return runtimeapi.AdvancedOverrideDocument{YAML: "{}\n"}, nil
}

func (f *fakeClient) PreviewCandidate(_ context.Context, contentID string) (runtimeapi.CandidatePreview, error) {
	f.previewed = contentID
	return runtimeapi.CandidatePreview{
		ContentID:       contentID,
		CandidateYAML:   "listeners: []\n",
		CandidateSHA256: strings.Repeat("a", 64),
		ProxyKind:       "mixed",
		ProxyAddresses:  []string{"127.0.0.1:7890", "[::1]:7890"},
		Validated:       true,
	}, nil
}

func (f *fakeClient) PreviewCandidateRequest(
	_ context.Context,
	request runtimeapi.PreviewCandidateRequest,
) (runtimeapi.CandidatePreview, error) {
	f.candidateRequests = append(f.candidateRequests, request)
	contentID := request.ContentID
	if contentID == "" {
		contentID = request.SourceID
	}
	preview, err := f.PreviewCandidate(context.Background(), contentID)
	preview.TrafficPolicy = request.TrafficPolicy
	return preview, err
}

func (f *fakeClient) Execute(_ context.Context, request runtimeapi.CreateOperationRequest) (runtimeapi.Operation, error) {
	f.actions = append(f.actions, request.Action)
	f.operationSerial++
	return runtimeapi.Operation{
		ID:       "op_test",
		Action:   request.Action,
		State:    runtimeapi.OperationQueued,
		Stage:    "accepted",
		Progress: 0,
	}, nil
}

func (f *fakeClient) GetOperation(_ context.Context, id string) (runtimeapi.Operation, error) {
	f.gotten = id
	return runtimeapi.Operation{ID: id, State: runtimeapi.OperationRunning, Stage: "working", Cancellable: true}, nil
}

func (f *fakeClient) WaitOperation(_ context.Context, id string, _ time.Duration) (runtimeapi.Operation, error) {
	f.waited = id
	return runtimeapi.Operation{
		ID:       id,
		State:    runtimeapi.OperationSucceeded,
		Stage:    "completed",
		Progress: 100,
	}, nil
}

func (f *fakeClient) CancelOperation(
	_ context.Context,
	id string,
	_ runtimeapi.CancelOperationRequest,
) (runtimeapi.Operation, error) {
	f.cancelled = id
	return runtimeapi.Operation{ID: id, State: runtimeapi.OperationCancelled, Stage: "cancelled"}, nil
}

func (f *fakeClient) VerifyProxy(context.Context) (runtimeapi.ProxyVerification, error) {
	f.verifyCalls++
	return runtimeapi.ProxyVerification{
		Available: true,
		Kind:      "mixed",
		Addresses: []string{"127.0.0.1:7890", "[::1]:7890"},
	}, nil
}

func (f *fakeClient) RevealSourceURL(
	context.Context,
	string,
	bool,
) (runtimeapi.RevealSourceURLResponse, error) {
	f.revealCalls++
	return runtimeapi.RevealSourceURLResponse{URL: "https://example.com/config?token=secret"}, nil
}

func (f *fakeClient) PreviewDiagnostics(
	context.Context,
	runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsPreview, error) {
	f.diagnosticPreviews++
	return runtimeapi.DiagnosticsPreview{
		Warning: runtimeapi.SensitiveDataWarning,
		Items:   []runtimeapi.DiagnosticItem{{Name: "snapshot.json", Included: true, Size: 10}},
	}, nil
}

func (f *fakeClient) CreateDiagnostics(
	context.Context,
	runtimeapi.DiagnosticsRequest,
) (runtimeapi.DiagnosticsResult, error) {
	f.diagnosticCreates++
	return runtimeapi.DiagnosticsResult{FileName: "diagnostics.zip", Size: 10}, nil
}

func (f *fakeClient) PreviewNetwork(
	_ context.Context,
	request runtimeapi.NetworkPreviewRequest,
) (runtimeapi.NetworkPreview, error) {
	f.networkRequests = append(f.networkRequests, request)
	preview := f.networkPreview
	if preview.PlanID == "" {
		preview = runtimeapi.NetworkPreview{
			PlanID:    "plan_0123456789abcdef0123456789abcdef",
			Mode:      runtimeapi.RunModeTUN,
			Device:    "smxtun0",
			Settings:  runtimeapi.TUNSettings{IPv6Policy: request.IPv6Policy, DNSPolicy: request.DNSPolicy},
			ExpiresAt: time.Now().Add(5 * time.Minute),
		}
	}
	return preview, nil
}

func (f *fakeClient) PreviewMihomoUpdate(
	_ context.Context,
	_ runtimeapi.MihomoUpdatePreviewRequest,
) (runtimeapi.MihomoUpdatePlan, error) {
	preview := f.updatePreview
	if preview.PlanID == "" {
		preview = runtimeapi.MihomoUpdatePlan{
			PlanID:         "plan_0123456789abcdef0123456789abcdef",
			Source:         runtimeapi.MihomoUpdateSourceOnlineTUF,
			Trust:          runtimeapi.MihomoUpdateTrustTUF,
			Version:        "v1.2.3",
			CurrentVersion: "v1.2.2",
			Platform:       "linux",
			Arch:           "amd64",
			Repository:     "MetaCubeX/mihomo",
			ExpiresAt:      time.Now().Add(5 * time.Minute),
		}
	}
	return preview, nil
}

func (f *fakeClient) PreviewProductUpdate(
	_ context.Context,
	request runtimeapi.ProductUpdatePreviewRequest,
) (runtimeapi.ProductUpdatePlan, error) {
	f.productRequests = append(f.productRequests, request)
	preview := f.productPreview
	if preview.PlanID == "" {
		preview = runtimeapi.ProductUpdatePlan{
			PlanID:         "product_plan_0123456789abcdef0123456789abcdef",
			Source:         request.Source,
			Trust:          runtimeapi.ProductUpdateTrustTUF,
			Channel:        runtimeapi.ProductUpdateChannelStable,
			Version:        "v2.0.0",
			CurrentVersion: "v1.0.0",
			Installable:    true,
			Platform:       "linux",
			Arch:           "amd64",
			ExpiresAt:      time.Now().Add(5 * time.Minute),
		}
	}
	return preview, nil
}

func (f *fakeClient) PreviewBackup(
	_ context.Context,
	request runtimeapi.BackupPreviewRequest,
) (runtimeapi.BackupPreview, error) {
	f.backupPreviews++
	return runtimeapi.BackupPreview{
		FormatVersion:  1,
		Restorable:     request.IncludeSecrets,
		IncludeSecrets: request.IncludeSecrets,
		Items: []runtimeapi.BackupItem{{
			Name:      "configuration_sources",
			Included:  true,
			Sensitive: request.IncludeSecrets,
			Count:     1,
			Size:      20,
		}},
		Warning: "完整明文备份警告",
	}, nil
}

func (f *fakeClient) ExportBackup(
	_ context.Context,
	request runtimeapi.BackupExportRequest,
) (runtimeapi.BackupArchive, error) {
	f.backupExports++
	f.lastBackupExport = request
	if !request.ConfirmPlaintext {
		return runtimeapi.BackupArchive{}, errors.New("backup export was not confirmed")
	}
	return runtimeapi.BackupArchive{
		FileName:       "submux-runtime-backup.zip",
		Size:           int64(len("backup archive")),
		SHA256:         strings.Repeat("b", 64),
		Restorable:     request.IncludeSecrets,
		IncludeSecrets: request.IncludeSecrets,
		Body:           []byte("backup archive"),
	}, nil
}

func (f *fakeClient) PreviewBackupRestore(
	_ context.Context,
	request runtimeapi.BackupRestorePreviewRequest,
) (runtimeapi.BackupRestorePreview, error) {
	f.backupInspections++
	return runtimeapi.BackupRestorePreview{
		ContentID:                request.ContentID,
		Restorable:               true,
		SourceCount:              1,
		ManagedResourceCount:     2,
		RecentConfigurationCount: 1,
		PendingSettings:          []string{"listeners", "tun", "gateway"},
		Warning:                  "整体替换警告",
	}, nil
}

func TestModelCreatesAndRestoresBackupWithExplicitConfirmation(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        17,
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)

	updated, command := model.Update(keyPress('B'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("backup preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.backupPreviews != 1 || model.sensitiveConfirm != "backup-export-full" {
		t.Fatalf("backup preview state = calls %d confirmation %q", client.backupPreviews, model.sensitiveConfirm)
	}

	updated, command = model.Update(keyPress('B'))
	model = updated.(Model)
	if command == nil || !model.editing || model.editorMode != editorModeBackupExport {
		t.Fatal("confirmed backup export did not open the output editor")
	}
	output := filepath.Join(t.TempDir(), "runtime-backup.zip")
	model.editor.SetValue(output)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("backup export did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	body, err := os.ReadFile(output)
	if err != nil || string(body) != "backup archive" || client.backupExports != 1 {
		t.Fatalf("backup export result body=%q calls=%d err=%v", body, client.backupExports, err)
	}
	if !client.lastBackupExport.IncludeSecrets || !client.lastBackupExport.ConfirmPlaintext {
		t.Fatalf("full backup export request = %#v", client.lastBackupExport)
	}

	updated, command = model.Update(keyPress('L'))
	model = updated.(Model)
	if command == nil || !model.editing || model.editorMode != editorModeBackupRestore {
		t.Fatal("backup restore did not open the input editor")
	}
	model.editor.SetValue(output)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("backup restore preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.backupInspections != 1 ||
		client.uploadedType != runtimeapi.RuntimeBackupContentType ||
		model.backupRestore.ContentID == "" ||
		model.sensitiveConfirm != "backup-restore:"+model.backupRestore.ContentID {
		t.Fatalf("restore preview was not retained: %#v", model.backupRestore)
	}

	updated, command = model.Update(keyPress('L'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("backup restore did not open the unified confirmation")
	}
	model, command = confirmAndSubmit(t, model)
	action := client.actions[len(client.actions)-1]
	if action.Kind != runtimeapi.ActionRestoreBackup ||
		!action.Params.Confirm ||
		action.Params.ContentID == "" {
		t.Fatalf("unexpected backup restore action: %#v", action)
	}
}

func TestModelCanCreateRedactedBackupInventory(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        17,
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)

	updated, command := model.Update(keyPress('b'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("redacted backup preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.sensitiveConfirm != "backup-export-redacted" || model.backupPreview.IncludeSecrets {
		t.Fatalf("redacted preview state = %#v / %q", model.backupPreview, model.sensitiveConfirm)
	}

	updated, command = model.Update(keyPress('b'))
	model = updated.(Model)
	if command == nil || !model.editing {
		t.Fatal("redacted backup confirmation did not open the output editor")
	}
	output := filepath.Join(t.TempDir(), "runtime-backup-redacted.zip")
	model.editor.SetValue(output)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("redacted backup export did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.lastBackupExport.IncludeSecrets || !client.lastBackupExport.ConfirmPlaintext {
		t.Fatalf("redacted backup export request = %#v", client.lastBackupExport)
	}
}

func TestModelMihomoUpdateAndRollbackRequireTwoStepConfirmation(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        9,
		Updates: runtimeapi.UpdateStatus{
			MihomoCurrentVersion:  "v1.2.3",
			MihomoPreviousVersion: "v1.2.2",
		},
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)

	updated, command := model.Update(keyPress('U'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("Mihomo update check did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.mihomoUpdate.PlanID == "" {
		t.Fatal("Mihomo update plan was not retained")
	}

	updated, command = model.Update(keyPress('U'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("Mihomo update did not open unified confirmation")
	}
	model, command = confirmAndSubmit(t, model)
	updateAction := client.actions[len(client.actions)-1]
	if updateAction.Kind != runtimeapi.ActionUpdateMihomo ||
		!updateAction.Params.Confirm ||
		updateAction.Params.Trust != runtimeapi.MihomoUpdateTrustTUF {
		t.Fatalf("unexpected Mihomo update action: %#v", updateAction)
	}

	model.busy = false
	model.lastOperation.State = runtimeapi.OperationSucceeded
	updated, command = model.Update(keyPress('R'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("Mihomo rollback did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	rollbackAction := client.actions[len(client.actions)-1]
	if rollbackAction.Kind != runtimeapi.ActionRollbackMihomo || !rollbackAction.Params.Confirm {
		t.Fatalf("unexpected Mihomo rollback action: %#v", rollbackAction)
	}
}

func TestModelProductUpdateOnlineOfflineAndRollbackUseSameConfirmedActions(t *testing.T) {
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        9,
		Updates: runtimeapi.UpdateStatus{
			RuntimeCurrentVersion:  "v1.0.0",
			RuntimePreviousVersion: "v0.9.0",
		},
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)

	updated, command := model.Update(keyPress('P'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("Runtime product update check did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.productRequests) != 1 ||
		client.productRequests[0].Source != runtimeapi.ProductUpdateSourceOnlineTUF {
		t.Fatalf("online product requests = %#v", client.productRequests)
	}
	updated, command = model.Update(keyPress('P'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("Runtime product update did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	updateAction := client.actions[len(client.actions)-1]
	if updateAction.Kind != runtimeapi.ActionUpdateProduct ||
		updateAction.Params.Trust != runtimeapi.ProductUpdateTrustTUF ||
		!updateAction.Params.Confirm {
		t.Fatalf("unexpected Runtime product update action: %#v", updateAction)
	}

	model.busy = false
	model.lastOperation.State = runtimeapi.OperationSucceeded
	updated, command = model.Update(keyPress('O'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("Runtime product rollback did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	rollbackAction := client.actions[len(client.actions)-1]
	if rollbackAction.Kind != runtimeapi.ActionRollbackProduct || !rollbackAction.Params.Confirm {
		t.Fatalf("unexpected Runtime product rollback action: %#v", rollbackAction)
	}

	model.busy = false
	bundle := filepath.Join(t.TempDir(), "product-update.zip")
	if err := os.WriteFile(bundle, []byte("offline product TUF bundle"), 0600); err != nil {
		t.Fatal(err)
	}
	updated, _ = model.Update(keyPress('I'))
	model = updated.(Model)
	model.editor.SetValue(bundle)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("offline Runtime product preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	lastRequest := client.productRequests[len(client.productRequests)-1]
	if client.uploadedType != runtimeapi.ProductUpdateBundleContentType ||
		lastRequest.Source != runtimeapi.ProductUpdateSourceOfflineTUF ||
		lastRequest.ContentID == "" {
		t.Fatalf("offline product upload type=%s request=%#v", client.uploadedType, lastRequest)
	}
}

func TestModelUsesOneClientForImportPreviewApplyStartStopAndWait(t *testing.T) {
	nextRestart := time.Date(2026, 7, 30, 12, 34, 56, 0, time.Local)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        7,
		Runtime:         runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
		Mihomo: runtimeapi.MihomoStatus{
			DesiredState:  runtimeapi.MihomoDesiredRunning,
			State:         "stopped",
			Recovery:      runtimeapi.MihomoRecoveryNeedsAttention,
			CrashAttempts: 3,
			NextRestartAt: &nextRestart,
			Fault:         &runtimeapi.Fault{Code: "mihomo_restart_limit", Message: "restart limit reached"},
		},
		RunMode: "explicit",
	}}
	model := New(t.Context(), client)
	updated, command := model.Update(model.Init()())
	model = updated.(Model)
	if command == nil {
		t.Fatal("snapshot update did not start Runtime event watching")
	}

	updated, _ = model.Update(keyPress('i'))
	model = updated.(Model)
	model.editor.SetValue("proxies: []\nrules: []\n")
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("import did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if string(client.uploaded) != "proxies: []\nrules: []\n" || client.previewed == "" || model.preview.ContentID == "" {
		t.Fatalf("import/preview did not use client: uploaded=%q previewed=%q model=%#v", client.uploaded, client.previewed, model.preview)
	}

	updated, command = model.Update(keyPress('a'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("candidate apply did not open unified confirmation")
	}
	model, command = confirmAndSubmit(t, model)
	if len(client.actions) != 1 || client.actions[0].Kind != runtimeapi.ActionApplyImportedConfig {
		t.Fatalf("apply actions = %#v", client.actions)
	}

	if command == nil {
		t.Fatal("queued operation did not start background waiting")
	}
	updated, command = model.Update(command())
	model = updated.(Model)
	if client.waited != "op_test" || model.lastOperation.State != runtimeapi.OperationSucceeded {
		t.Fatalf("waited=%q operation=%#v", client.waited, model.lastOperation)
	}
	if command == nil {
		t.Fatal("terminal operation did not request a fresh snapshot")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)

	updated, command = model.Update(keyPress('g'))
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.gotten != "op_test" || model.lastOperation.State != runtimeapi.OperationRunning {
		t.Fatalf("gotten=%q operation=%#v", client.gotten, model.lastOperation)
	}

	updated, command = model.Update(keyPress('c'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("cancellable operation did not open cancellation confirmation")
	}
	model, command = confirmAndSubmit(t, model)
	if client.cancelled != "op_test" || model.lastOperation.State != runtimeapi.OperationCancelled {
		t.Fatalf("cancelled=%q operation=%#v", client.cancelled, model.lastOperation)
	}
	if command == nil {
		t.Fatal("cancelled operation did not request a fresh snapshot")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)

	for _, test := range []struct {
		key    rune
		action string
	}{
		{key: 's', action: runtimeapi.ActionStartProxy},
		{key: 'x', action: runtimeapi.ActionStopProxy},
	} {
		model.lastOperation.State = runtimeapi.OperationSucceeded
		updated, command = model.Update(keyPress(test.key))
		model = updated.(Model)
		if command != nil || model.confirmation == nil {
			t.Fatalf("key %q did not open unified confirmation", test.key)
		}
		model, _ = confirmAndSubmit(t, model)
		if client.actions[len(client.actions)-1].Kind != test.action {
			t.Fatalf("key %q action = %#v", test.key, client.actions[len(client.actions)-1])
		}
	}

	updated, command = model.Update(keyPress('v'))
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.verifyCalls != 1 || !model.verification.Available {
		t.Fatalf("verification calls=%d value=%#v", client.verifyCalls, model.verification)
	}
	updated, _ = model.Update(keyPress('1'))
	model = updated.(Model)
	if !strings.Contains(model.View().Content, "Submux Runtime") ||
		!strings.Contains(model.View().Content, "Mihomo 实际") ||
		!strings.Contains(model.View().Content, runtimeapi.MihomoDesiredRunning) ||
		!strings.Contains(model.View().Content, runtimeapi.MihomoRecoveryNeedsAttention) ||
		!strings.Contains(model.View().Content, "重试次数") ||
		!strings.Contains(model.View().Content, nextRestart.Format(time.RFC3339)) ||
		!strings.Contains(model.View().Content, "mihomo_restart_limit") {
		t.Fatalf("view=%q", model.View().Content)
	}
}

func TestQuitDoesNotSubmitStopOperation(t *testing.T) {
	client := &fakeClient{}
	model := New(t.Context(), client)
	model.busy = false

	_, command := model.Update(keyPress('q'))

	if command == nil {
		t.Fatal("quit did not return a command")
	}
	if len(client.actions) != 0 {
		t.Fatalf("quitting TUI submitted actions: %#v", client.actions)
	}
}

func TestModelPreviewsDisplaysAndControlsOrdinaryTUN(t *testing.T) {
	client := &fakeClient{
		snapshot: runtimeapi.Snapshot{
			ProtocolVersion: runtimeapi.ProtocolVersion,
			Runtime:         runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
			Network: runtimeapi.NetworkStatus{
				Available: true,
				Mode:      runtimeapi.RunModeExplicit,
				State:     runtimeapi.NetworkStateConflict,
				Conflicts: []runtimeapi.NetworkConflict{{
					Kind:   "full_tunnel",
					Owner:  "wg0",
					Detail: "另一个默认路由正在生效",
				}},
				Residuals: []runtimeapi.NetworkObject{{
					Kind:  "policy_rule",
					ID:    "rule_test",
					Name:  "12000",
					State: runtimeapi.NetworkStateUnknown,
				}},
			},
		},
		networkPreview: runtimeapi.NetworkPreview{
			PlanID: "plan_0123456789abcdef0123456789abcdef",
			Mode:   runtimeapi.RunModeTUN,
			Device: "smxtun0",
			Settings: runtimeapi.TUNSettings{
				IPv6Policy: runtimeapi.TUNIPv6Direct,
				DNSPolicy:  runtimeapi.TUNDNSOff,
			},
			Conflicts: []runtimeapi.NetworkConflict{{
				Kind:   "full_tunnel",
				Owner:  "wg0",
				Detail: "冲突预览",
			}},
			Warnings:  []string{"IPv6 将直连"},
			ExpiresAt: time.Now().Add(5 * time.Minute),
		},
	}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)
	updated, _ = model.Update(ctrlKey('t'))
	model = updated.(Model)
	if model.networkForm == nil {
		t.Fatal("ordinary TUN structured form did not open")
	}
	model.networkForm.setValue(networkFieldIPv6Policy, runtimeapi.TUNIPv6Direct)
	model.networkForm.setValue(networkFieldDNSPolicy, runtimeapi.TUNDNSOff)
	model.networkForm.setValue(networkFieldCaptureRouteIDs, "route_lan")
	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("ordinary TUN preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.networkRequests) != 1 ||
		client.networkRequests[0].Mode != runtimeapi.RunModeTUN ||
		client.networkRequests[0].IPv6Policy != runtimeapi.TUNIPv6Direct ||
		client.networkRequests[0].DNSPolicy != runtimeapi.TUNDNSOff ||
		len(client.networkRequests[0].CaptureRouteIDs) != 1 {
		t.Fatalf("ordinary TUN preview requests=%#v", client.networkRequests)
	}
	view := model.View().Content
	for _, expected := range []string{"网络冲突", "网络残留", "冲突预览", "IPv6 将直连"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("ordinary TUN view missing %q: %q", expected, view)
		}
	}
	updated, command = model.Update(ctrlKey('e'))
	model = updated.(Model)
	if command != nil || len(client.actions) != 0 {
		t.Fatalf("conflicting ordinary TUN preview was enabled: command=%v actions=%#v", command, client.actions)
	}

	model.networkPreview.Conflicts = nil
	updated, command = model.Update(ctrlKey('e'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("ordinary TUN enable did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	if len(client.actions) != 1 ||
		client.actions[0].Kind != runtimeapi.ActionEnableTUN ||
		client.actions[0].Params.PlanID != model.networkPreview.PlanID {
		t.Fatalf("ordinary TUN enable actions=%#v", client.actions)
	}
	model.busy = false
	model.lastOperation.State = runtimeapi.OperationSucceeded
	model.snapshot.Network.Mode = runtimeapi.RunModeTUN
	updated, command = model.Update(ctrlKey('x'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("ordinary TUN disable did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	if len(client.actions) != 2 || client.actions[1].Kind != runtimeapi.ActionDisableTUN {
		t.Fatalf("ordinary TUN disable actions=%#v", client.actions)
	}
}

func TestModelPreviewsDisplaysAndControlsLinuxGateway(t *testing.T) {
	client := &fakeClient{
		snapshot: runtimeapi.Snapshot{
			ProtocolVersion: runtimeapi.ProtocolVersion,
			Runtime:         runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
			Network: runtimeapi.NetworkStatus{
				Available: true,
				Mode:      runtimeapi.RunModeExplicit,
				State:     runtimeapi.NetworkStateInactive,
			},
		},
		networkPreview: runtimeapi.NetworkPreview{
			PlanID: "plan_0123456789abcdef0123456789abcdef",
			Mode:   runtimeapi.RunModeGateway,
			Device: "smxgw0",
			GatewaySettings: &runtimeapi.GatewaySettings{
				IPv6Policy:       runtimeapi.TUNIPv6Block,
				DNSPolicy:        runtimeapi.TUNDNSHijack,
				CaptureTCP:       true,
				CaptureUDP:       false,
				ProxyHostTraffic: true,
				ExcludedRouteIDs: []string{"route_lan"},
			},
			Routes: []runtimeapi.NetworkRoute{{
				ID:        "route_lan",
				Family:    "ipv4",
				CIDR:      "10.0.0.0/24",
				Interface: "lan0",
				Role:      runtimeapi.NetworkRouteRoleGatewayLAN,
				Bypass:    true,
			}},
			Warnings:    []string{"IPv6 转发将阻断"},
			PreviewOnly: true,
			ExpiresAt:   time.Now().Add(5 * time.Minute),
		},
	}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)
	updated, _ = model.Update(ctrlKey('l'))
	model = updated.(Model)
	if model.networkForm == nil {
		t.Fatal("Linux gateway structured form did not open")
	}
	model.networkForm.setValue(networkFieldIPv6Policy, runtimeapi.TUNIPv6Block)
	model.networkForm.setValue(networkFieldDNSPolicy, runtimeapi.TUNDNSHijack)
	model.networkForm.setValue(networkFieldCaptureTCP, "true")
	model.networkForm.setValue(networkFieldCaptureUDP, "false")
	model.networkForm.setValue(networkFieldProxyHostTraffic, "true")
	model.networkForm.setValue(networkFieldExcludedRouteIDs, "route_lan")
	model.networkForm.setValue(networkFieldUDPDirectCIDRs, "203.0.113.0/24")
	model.networkForm.setValue(networkFieldUDPDirectPorts, "443")
	model.networkForm.setValue(networkFieldDNSDirectCIDRs, "10.0.0.53/32")
	model.networkForm.setValue(networkFieldHostExceptionUIDs, "2001")
	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("Linux gateway preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.networkRequests) != 1 ||
		client.networkRequests[0].Mode != runtimeapi.RunModeGateway ||
		client.networkRequests[0].CaptureUDP == nil ||
		*client.networkRequests[0].CaptureUDP ||
		!client.networkRequests[0].ProxyHostTraffic ||
		len(client.networkRequests[0].ExcludedRouteIDs) != 1 ||
		len(client.networkRequests[0].UDPExceptions) != 1 ||
		len(client.networkRequests[0].DNSDirectCIDRs) != 1 ||
		len(client.networkRequests[0].HostExceptions) != 1 {
		t.Fatalf("Linux gateway preview requests=%#v", client.networkRequests)
	}
	view := model.View().Content
	for _, expected := range []string{"gateway（预览）", "smxgw0", "IPv6 转发将阻断", "route_lan"} {
		if !strings.Contains(view, expected) {
			t.Fatalf("Linux gateway view missing %q: %q", expected, view)
		}
	}
	updated, command = model.Update(ctrlKey('e'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("Linux gateway preview did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	if len(client.actions) != 1 ||
		client.actions[0].Kind != runtimeapi.ActionEnableGateway ||
		client.actions[0].Params.PlanID != model.networkPreview.PlanID {
		t.Fatalf("Linux gateway enable actions=%#v", client.actions)
	}
	model.busy = false
	model.lastOperation.State = runtimeapi.OperationSucceeded
	model.snapshot.Network.Mode = runtimeapi.RunModeGateway
	updated, command = model.Update(ctrlKey('x'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("Linux gateway disable did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	if len(client.actions) != 2 || client.actions[1].Kind != runtimeapi.ActionDisableGateway {
		t.Fatalf("Linux gateway disable actions=%#v", client.actions)
	}
}

func TestModelAddsRefreshesAndDisplaysRemoteSourceThroughRuntimeClient(t *testing.T) {
	sourceID := "src_" + strings.Repeat("a", 32)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        9,
		Runtime:         runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
		Sources: runtimeapi.SourceStatus{
			Count:           1,
			CurrentSourceID: sourceID,
			Items: []runtimeapi.SourceSummary{{
				ID:                sourceID,
				Type:              runtimeapi.SourceTypeRemoteHTTP,
				Name:              "primary",
				Current:           true,
				RedactedTarget:    "https://example.com:443/…",
				Route:             runtimeapi.SourceRouteDirect,
				LastRefreshResult: "validated",
				HighRiskSettings:  []string{"skip_tls_verify"},
			}},
		},
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)

	updated, _ = model.Update(keyPress('u'))
	model = updated.(Model)
	model.sourceForm.setValue(sourceFieldName, "secondary")
	model.sourceForm.setValue(sourceFieldURL, "https://source.example/config.yaml")
	model.sourceForm.setValue(sourceFieldRoute, runtimeapi.SourceRouteDirect)
	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("source add did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.editing ||
		client.uploadedType != runtimeapi.SourceDraftContentType ||
		model.confirmation == nil || len(client.actions) != 0 {
		t.Fatalf("source add state: editing=%v type=%q actions=%#v",
			model.editing, client.uploadedType, client.actions)
	}
	model, _ = confirmAndSubmit(t, model)
	if len(client.actions) != 1 || client.actions[0].Kind != runtimeapi.ActionAddRemoteSource {
		t.Fatalf("source add actions=%#v", client.actions)
	}

	for _, test := range []struct {
		key   rune
		route string
	}{
		{key: 'f', route: ""},
		{key: 'd', route: runtimeapi.SourceRouteDirect},
		{key: 'm', route: runtimeapi.SourceRouteMihomo},
	} {
		model.lastOperation.State = runtimeapi.OperationSucceeded
		updated, command = model.Update(keyPress(test.key))
		model = updated.(Model)
		if command != nil || model.confirmation == nil {
			t.Fatalf("source refresh key %q did not open unified confirmation", test.key)
		}
		model, _ = confirmAndSubmit(t, model)
		action := client.actions[len(client.actions)-1]
		if action.Kind != runtimeapi.ActionRefreshSource ||
			action.Params.SourceID != sourceID ||
			action.Params.Route != test.route {
			t.Fatalf("source refresh action for %q = %#v", test.key, action)
		}
	}
	updated, command = model.Update(keyPress('p'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("source apply did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	applied := client.actions[len(client.actions)-1]
	if applied.Kind != runtimeapi.ActionApplySource || applied.Params.SourceID != sourceID {
		t.Fatalf("source apply action = %#v", applied)
	}
	updated, _ = model.Update(keyPress('2'))
	model = updated.(Model)
	view := model.View().Content
	if !strings.Contains(view, "https://example.com:443/…") ||
		!strings.Contains(view, "skip_tls_verify") {
		t.Fatalf("source view = %q", view)
	}
}

func TestModelManagesMultipleSourceTypesAndSwitchesSelectedSource(t *testing.T) {
	currentSourceID := "src_" + strings.Repeat("c", 32)
	localSourceID := "src_" + strings.Repeat("d", 32)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        14,
		Runtime:         runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
		Sources: runtimeapi.SourceStatus{
			Count:           2,
			CurrentSourceID: currentSourceID,
			Items: []runtimeapi.SourceSummary{
				{
					ID:             currentSourceID,
					Type:           runtimeapi.SourceTypeSubmuxOutput,
					Name:           "generated",
					Current:        true,
					RedactedTarget: "https://submux.example:443/…",
					Route:          runtimeapi.SourceRouteDirect,
				},
				{
					ID:                    localSourceID,
					Type:                  runtimeapi.SourceTypeLocalImport,
					Name:                  "local-copy",
					RedactedTarget:        "本机配置副本",
					HasValidatedCandidate: true,
				},
			},
		},
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)
	if model.selectedSource() != currentSourceID {
		t.Fatalf("initial selected source = %q", model.selectedSource())
	}

	updated, _ = model.Update(keyPress(']'))
	model = updated.(Model)
	if model.selectedSource() != localSourceID {
		t.Fatalf("selected source after ] = %q", model.selectedSource())
	}

	for _, test := range []struct {
		key       rune
		kind      string
		useCached bool
		confirm   bool
	}{
		{key: 'f', kind: runtimeapi.ActionRefreshSource},
		{key: 't', kind: runtimeapi.ActionSwitchSource},
		{key: 'k', kind: runtimeapi.ActionSwitchSource, useCached: true},
		{key: 'z', kind: runtimeapi.ActionDeleteSource, confirm: true},
	} {
		model.lastOperation.State = runtimeapi.OperationSucceeded
		updated, command := model.Update(keyPress(test.key))
		model = updated.(Model)
		if command != nil || model.confirmation == nil {
			t.Fatalf("source action key %q did not open unified confirmation", test.key)
		}
		model, _ = confirmAndSubmit(t, model)
		action := client.actions[len(client.actions)-1]
		if action.Kind != test.kind ||
			action.Params.SourceID != localSourceID ||
			action.Params.UseCached != test.useCached ||
			action.Params.Confirm != test.confirm {
			t.Fatalf("source action for %q = %#v", test.key, action)
		}
	}

	model.lastOperation.State = runtimeapi.OperationSucceeded
	updated, command := model.Update(ctrlKey('d'))
	model = updated.(Model)
	if command != nil || model.confirmation == nil {
		t.Fatal("source delete did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	confirmedDelete := client.actions[len(client.actions)-1]
	if confirmedDelete.Kind != runtimeapi.ActionDeleteSource ||
		confirmedDelete.Params.SourceID != localSourceID ||
		!confirmedDelete.Params.Confirm {
		t.Fatalf("confirmed source delete = %#v", confirmedDelete)
	}

	updated, _ = model.Update(keyPress('n'))
	model = updated.(Model)
	if model.sourceForm == nil || model.sourceForm.kind != sourceFormLocal {
		t.Fatalf("imported source form = %#v", model.sourceForm)
	}
	localSource := filepath.Join(t.TempDir(), "local-two.yaml")
	if err := os.WriteFile(localSource, []byte("proxies: []\nrules: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	model.sourceForm.setValue(sourceFieldName, "local-two")
	model.sourceForm.setValue(sourceFieldPath, localSource)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("imported source add did not return a command")
	}
	model.lastOperation.State = runtimeapi.OperationSucceeded
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.confirmation == nil {
		t.Fatal("imported source upload did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	imported := client.actions[len(client.actions)-1]
	if client.uploadedType != "application/x-yaml" ||
		imported.Kind != runtimeapi.ActionAddImportedSource ||
		imported.Params.SourceName != "local-two" ||
		imported.Params.ContentID == "" {
		t.Fatalf("imported source operation = type %q action %#v", client.uploadedType, imported)
	}

	updated, _ = model.Update(keyPress('2'))
	model = updated.(Model)
	view := model.View().Content
	if !strings.Contains(view, "generated [当前]") ||
		!strings.Contains(view, runtimeapi.SourceTypeSubmuxOutput) ||
		!strings.Contains(view, runtimeapi.SourceTypeLocalImport) {
		t.Fatalf("multiple source view = %q", view)
	}
}

func TestSensitiveTUIActionsRequireTwoStepsAndUseSharedWarning(t *testing.T) {
	sourceID := "src_" + strings.Repeat("e", 32)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        4,
		Runtime:         runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
		Sources: runtimeapi.SourceStatus{
			Count:           2,
			CurrentSourceID: sourceID,
			Items: []runtimeapi.SourceSummary{{
				ID:             sourceID,
				Type:           runtimeapi.SourceTypeRemoteHTTP,
				Name:           "primary",
				Current:        true,
				RedactedTarget: "https://example.com:443/…",
			}, {
				ID:             "src_" + strings.Repeat("f", 32),
				Type:           runtimeapi.SourceTypeRemoteHTTP,
				Name:           "secondary",
				RedactedTarget: "https://backup.example.com:443/…",
			}},
		},
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)

	updated, command := model.Update(ctrlKey('u'))
	model = updated.(Model)
	if command != nil || client.revealCalls != 0 || !strings.Contains(model.status, runtimeapi.SensitiveDataWarning) {
		t.Fatalf("first source reveal confirmation: command=%v calls=%d status=%q", command != nil, client.revealCalls, model.status)
	}
	updated, command = model.Update(ctrlKey('u'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed source reveal did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.revealCalls != 1 || !strings.Contains(model.revealedURL, "token=secret") {
		t.Fatalf("confirmed source reveal calls=%d URL=%q", client.revealCalls, model.revealedURL)
	}
	if !model.sourceDiagnosticOpen || !strings.Contains(model.View().Content, "只读诊断") {
		t.Fatal("confirmed source reveal did not open read-only diagnostics")
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyPressMsg{Code: ']'})
	model = updated.(Model)
	if model.revealedURL != "" || strings.HasPrefix(model.sensitiveConfirm, "reveal:") {
		t.Fatalf("source selection retained revealed URL=%q confirmation=%q", model.revealedURL, model.sensitiveConfirm)
	}

	updated, command = model.Update(ctrlKey('g'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("diagnostics preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.diagnosticPreviews != 1 || client.diagnosticCreates != 0 {
		t.Fatalf("diagnostics preview/create calls=%d/%d", client.diagnosticPreviews, client.diagnosticCreates)
	}
	updated, command = model.Update(ctrlKey('g'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed diagnostics creation did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.diagnosticCreates != 1 || model.diagnosticsFile.FileName != "diagnostics.zip" {
		t.Fatalf("diagnostics create calls=%d result=%#v", client.diagnosticCreates, model.diagnosticsFile)
	}
	if !strings.Contains(model.View().Content, runtimeapi.SensitiveDataWarning) {
		t.Fatalf("shared sensitive warning missing from TUI: %q", model.View().Content)
	}
}

func TestOperationStatusReportsSourceSwitchAndCacheUse(t *testing.T) {
	status := operationStatus(runtimeapi.Operation{
		ID:    "op_switch",
		State: runtimeapi.OperationSucceeded,
		Stage: "completed",
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionSwitchSource,
		},
		Result: &runtimeapi.OperationResult{
			PreviousSourceID: "source-a",
			SourceID:         "source-b",
			UsedCachedSource: true,
		},
	})
	if !strings.Contains(status, "source-a → source-b") ||
		!strings.Contains(status, "使用已验证缓存") {
		t.Fatalf("switch operation status = %q", status)
	}
}

func TestModelManagesResourcesOverridesAndLayeredPreviewThroughRuntimeClient(t *testing.T) {
	sourceID := "src_" + strings.Repeat("b", 32)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        12,
		Runtime:         runtimeapi.RuntimeStatus{Version: "dev", ServiceState: "running"},
		Sources: runtimeapi.SourceStatus{
			CurrentSourceID: sourceID,
		},
		Resources: runtimeapi.ResourceStatus{
			Count:      1,
			TotalBytes: 14,
			Items: []runtimeapi.ManagedResourceSummary{{
				ID:     "res_" + strings.Repeat("c", 32),
				Name:   "provider.main",
				Kind:   runtimeapi.ResourceKindProxyProvider,
				Size:   14,
				SHA256: strings.Repeat("d", 64),
			}},
		},
		AdvancedOverride: runtimeapi.OverrideStatus{
			Present: true,
			Size:    3,
			SHA256:  strings.Repeat("e", 64),
		},
	}}
	model := New(t.Context(), client)
	updated, _ := model.Update(model.Init()())
	model = updated.(Model)

	updated, command := model.Update(keyPress('y'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("source preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.previewed != sourceID || model.preview.CandidateSHA256 == "" {
		t.Fatalf("source preview = %q / %#v", client.previewed, model.preview)
	}

	updated, _ = model.Update(keyPress('e'))
	model = updated.(Model)
	model.editor.SetValue(`{"name":"provider.main","kind":"proxy-provider-yaml","content":"proxies: []\n"}`)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("resource add did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.confirmation == nil {
		t.Fatal("resource upload did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	resourceAction := client.actions[len(client.actions)-1]
	if client.uploadedType != runtimeapi.ManagedResourceContentType ||
		resourceAction.Kind != runtimeapi.ActionAddManagedResource ||
		resourceAction.Params.ResourceName != "provider.main" ||
		resourceAction.Params.ResourceKind != runtimeapi.ResourceKindProxyProvider {
		t.Fatalf("resource operation = type %q action %#v", client.uploadedType, resourceAction)
	}

	updated, command = model.Update(keyPress('o'))
	model = updated.(Model)
	if command != nil || !strings.Contains(model.status, runtimeapi.SensitiveDataWarning) || client.overrideReads != 0 {
		t.Fatalf("first override confirmation: command=%v reads=%d status=%q", command != nil, client.overrideReads, model.status)
	}
	updated, command = model.Update(keyPress('o'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("override read did not return a command")
	}
	updated, command = model.Update(command())
	model = updated.(Model)
	if client.overrideReads != 1 || !model.editing || model.editorMode != editorModeOverride {
		t.Fatalf("override editor state = editing %v mode %q", model.editing, model.editorMode)
	}
	model.editor.SetValue("rules:\n  - MATCH,DIRECT\n")
	updated, command = model.Update(ctrlKey('p'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("prospective override preview did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if !model.editing || model.preview.CandidateSHA256 == "" || client.previewed != sourceID {
		t.Fatalf("prospective override preview = editing %v preview %#v source %q",
			model.editing, model.preview, client.previewed)
	}
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("override set did not return a command")
	}
	model.lastOperation.State = runtimeapi.OperationSucceeded
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.confirmation == nil {
		t.Fatal("override upload did not open unified confirmation")
	}
	model, _ = confirmAndSubmit(t, model)
	overrideAction := client.actions[len(client.actions)-1]
	if client.uploadedType != "application/x-yaml" ||
		overrideAction.Kind != runtimeapi.ActionSetAdvancedOverride {
		t.Fatalf("override operation = type %q action %#v", client.uploadedType, overrideAction)
	}

	updated, _ = model.Update(keyPress('2'))
	model = updated.(Model)
	view := model.View().Content
	if !strings.Contains(view, "provider.main") || !strings.Contains(view, "高级覆盖") {
		t.Fatalf("layer view = %q", view)
	}
}

func keyPress(character rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: character, Text: string(character)}
}

func ctrlKey(character rune) tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: character, Mod: tea.ModCtrl}
}

func confirmAndSubmit(t *testing.T, model Model) (Model, tea.Cmd) {
	t.Helper()
	if model.confirmation == nil {
		t.Fatal("expected a pending Runtime operation confirmation")
	}
	updated, command := model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed Runtime operation did not return a submit command")
	}
	updated, followUp := model.Update(command())
	return updated.(Model), followUp
}
