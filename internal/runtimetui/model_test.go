package runtimetui

import (
	"context"
	"errors"
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
}

func (f *fakeClient) Observe(context.Context) (runtimeapi.Snapshot, error) {
	return f.snapshot, nil
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
	contentID := request.ContentID
	if contentID == "" {
		contentID = request.SourceID
	}
	return f.PreviewCandidate(context.Background(), contentID)
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
	return runtimeapi.Operation{ID: id, State: runtimeapi.OperationRunning, Stage: "working"}, nil
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
	if command != nil {
		t.Fatal("snapshot update unexpectedly returned a command")
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
	updated, _ = model.Update(command())
	model = updated.(Model)
	if len(client.actions) != 1 || client.actions[0].Kind != runtimeapi.ActionApplyImportedConfig {
		t.Fatalf("apply actions = %#v", client.actions)
	}

	updated, command = model.Update(keyPress('w'))
	model = updated.(Model)
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
	updated, command = model.Update(command())
	model = updated.(Model)
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
		updated, command = model.Update(keyPress(test.key))
		model = updated.(Model)
		updated, _ = model.Update(command())
		model = updated.(Model)
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
	model.editor.SetValue(`{
	  "name": "secondary",
	  "url": "https://source.example/config.yaml",
	  "route": "direct",
	  "refresh_interval_seconds": 21600
	}`)
	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("source add did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.editing ||
		client.uploadedType != runtimeapi.SourceDraftContentType ||
		len(client.actions) != 1 ||
		client.actions[0].Kind != runtimeapi.ActionAddRemoteSource {
		t.Fatalf("source add state: editing=%v type=%q actions=%#v",
			model.editing, client.uploadedType, client.actions)
	}

	for _, test := range []struct {
		key   rune
		route string
	}{
		{key: 'f', route: ""},
		{key: 'd', route: runtimeapi.SourceRouteDirect},
		{key: 'm', route: runtimeapi.SourceRouteMihomo},
	} {
		updated, command = model.Update(keyPress(test.key))
		model = updated.(Model)
		if command == nil {
			t.Fatalf("source refresh key %q did not return a command", test.key)
		}
		updated, _ = model.Update(command())
		model = updated.(Model)
		action := client.actions[len(client.actions)-1]
		if action.Kind != runtimeapi.ActionRefreshSource ||
			action.Params.SourceID != sourceID ||
			action.Params.Route != test.route {
			t.Fatalf("source refresh action for %q = %#v", test.key, action)
		}
	}
	updated, command = model.Update(keyPress('p'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("source apply did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	applied := client.actions[len(client.actions)-1]
	if applied.Kind != runtimeapi.ActionApplySource || applied.Params.SourceID != sourceID {
		t.Fatalf("source apply action = %#v", applied)
	}
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
		{key: 'z', kind: runtimeapi.ActionDeleteSource},
	} {
		updated, command := model.Update(keyPress(test.key))
		model = updated.(Model)
		if command == nil {
			t.Fatalf("source action key %q did not return a command", test.key)
		}
		updated, _ = model.Update(command())
		model = updated.(Model)
		action := client.actions[len(client.actions)-1]
		if action.Kind != test.kind ||
			action.Params.SourceID != localSourceID ||
			action.Params.UseCached != test.useCached ||
			action.Params.Confirm != test.confirm {
			t.Fatalf("source action for %q = %#v", test.key, action)
		}
	}

	updated, command := model.Update(ctrlKey('d'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("confirmed source delete did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	confirmedDelete := client.actions[len(client.actions)-1]
	if confirmedDelete.Kind != runtimeapi.ActionDeleteSource ||
		confirmedDelete.Params.SourceID != localSourceID ||
		!confirmedDelete.Params.Confirm {
		t.Fatalf("confirmed source delete = %#v", confirmedDelete)
	}

	updated, _ = model.Update(keyPress('n'))
	model = updated.(Model)
	if !model.editing || model.editorMode != editorModeImportedSource {
		t.Fatalf("imported source editor = editing %v mode %q", model.editing, model.editorMode)
	}
	model.editor.SetValue(`{"name":"local-two","content":"proxies: []\nrules: []\n"}`)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil {
		t.Fatal("imported source add did not return a command")
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	imported := client.actions[len(client.actions)-1]
	if client.uploadedType != "application/x-yaml" ||
		imported.Kind != runtimeapi.ActionAddImportedSource ||
		imported.Params.SourceName != "local-two" ||
		imported.Params.ContentID == "" {
		t.Fatalf("imported source operation = type %q action %#v", client.uploadedType, imported)
	}

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
	updated, _ = model.Update(command())
	model = updated.(Model)
	overrideAction := client.actions[len(client.actions)-1]
	if client.uploadedType != "application/x-yaml" ||
		overrideAction.Kind != runtimeapi.ActionSetAdvancedOverride {
		t.Fatalf("override operation = type %q action %#v", client.uploadedType, overrideAction)
	}

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
