package runtimetui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"submux/internal/runtimeapi"
)

func TestRuntimeTUIRemoteSourceUsesStructuredForm(t *testing.T) {
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)

	updated, command := model.Update(keyPress('u'))
	model = updated.(Model)
	if command == nil || model.sourceForm == nil {
		t.Fatal("remote source did not open a structured form")
	}
	view := model.View().Content
	for _, label := range []string{"来源名称", "配置地址", "刷新路线", "刷新间隔", "授权目标"} {
		if !strings.Contains(view, label) {
			t.Fatalf("remote source form missing %q: %q", label, view)
		}
	}
	if strings.Contains(view, `"url"`) || strings.Contains(view, `"name"`) {
		t.Fatalf("remote source form still exposes JSON editing: %q", view)
	}

	model.sourceForm.setValue(sourceFieldName, "secondary")
	model.sourceForm.setValue(sourceFieldURL, "https://source.example/config.yaml")
	model.sourceForm.setValue(sourceFieldRoute, runtimeapi.SourceRouteDirect)
	updated, command = model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil || model.sourceForm != nil {
		t.Fatalf("remote source form submit command=%v form=%#v", command != nil, model.sourceForm)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if model.confirmation == nil || client.uploadedType != runtimeapi.SourceDraftContentType {
		t.Fatalf("remote source did not reach unified confirmation: type=%q", client.uploadedType)
	}
	if body := string(client.uploaded); !strings.Contains(body, `"name":"secondary"`) ||
		!strings.Contains(body, `"url":"https://source.example/config.yaml"`) {
		t.Fatalf("remote source draft=%q", body)
	}
}

func TestRuntimeTUILocalSourceFormReadsFileWithoutPersistingPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.yaml")
	if err := os.WriteFile(path, []byte("proxies: []\nrules: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{snapshot: shellSnapshot(4, 3)}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(keyPress('n'))
	model = updated.(Model)
	if model.sourceForm == nil {
		t.Fatal("local source did not open a structured form")
	}
	view := model.View().Content
	if !strings.Contains(view, "来源名称") || !strings.Contains(view, "本机文件") || strings.Contains(view, `"content"`) {
		t.Fatalf("local source form=%q", view)
	}
	model.sourceForm.setValue(sourceFieldName, "local-two")
	model.sourceForm.setValue(sourceFieldPath, path)
	updated, command := model.Update(ctrlKey('s'))
	model = updated.(Model)
	if command == nil || model.sourceForm != nil {
		t.Fatalf("local source submit command=%v form=%#v", command != nil, model.sourceForm)
	}
	updated, _ = model.Update(command())
	model = updated.(Model)
	if client.uploadedType != "application/x-yaml" || string(client.uploaded) != "proxies: []\nrules: []\n" {
		t.Fatalf("local source upload type=%q body=%q", client.uploadedType, client.uploaded)
	}
	if strings.Contains(model.View().Content, path) {
		t.Fatalf("local file path persisted after upload: %q", model.View().Content)
	}
}

func TestRuntimeTUIConfigDistinguishesSelectedAndCommittedSource(t *testing.T) {
	currentID := "src_" + strings.Repeat("a", 32)
	selectedID := "src_" + strings.Repeat("b", 32)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        6,
		Sources: runtimeapi.SourceStatus{
			CurrentSourceID: currentID,
			Items: []runtimeapi.SourceSummary{
				{ID: currentID, Name: "office", Type: runtimeapi.SourceTypeRemoteHTTP, Current: true},
				{ID: selectedID, Name: "travel", Type: runtimeapi.SourceTypeLocalImport},
			},
		},
	}}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageConfig)
	model.selectedSourceID = selectedID

	view := model.View().Content
	for _, text := range []string{
		"浏览中的所选来源  travel",
		"Runtime 已提交的当前来源  office",
		"Enter 生成候选，不会切换当前来源",
	} {
		if !strings.Contains(view, text) {
			t.Fatalf("config workspace missing %q: %q", text, view)
		}
	}
}

func TestRuntimeTUIRawSourceDataOnlyAppearsInReadOnlyDiagnostics(t *testing.T) {
	sourceID := "src_" + strings.Repeat("c", 32)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Revision:        7,
		Sources: runtimeapi.SourceStatus{
			CurrentSourceID: sourceID,
			Items: []runtimeapi.SourceSummary{{
				ID: sourceID, Name: "primary", Type: runtimeapi.SourceTypeRemoteHTTP, Current: true,
			}},
		},
	}}
	model, _ := initializeShellModel(t, client)
	model.openPage(pageConfig)
	model.preview = runtimeapi.CandidatePreview{CandidateYAML: "secret: candidate\n", CandidateSHA256: strings.Repeat("d", 64)}
	model.previewSourceID = sourceID

	updated, _ := model.Update(ctrlKey('u'))
	model = updated.(Model)
	updated, command := model.Update(ctrlKey('u'))
	model = updated.(Model)
	updated, _ = model.Update(command())
	model = updated.(Model)
	if !model.sourceDiagnosticOpen {
		t.Fatal("revealed source did not open the read-only diagnostic view")
	}
	view := model.View().Content
	for _, text := range []string{"只读诊断", "token=secret", "secret: candidate", "不可编辑"} {
		if !strings.Contains(view, text) {
			t.Fatalf("read-only diagnostic missing %q: %q", text, view)
		}
	}

	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	if model.sourceDiagnosticOpen || strings.Contains(model.View().Content, "token=secret") ||
		strings.Contains(model.View().Content, "secret: candidate") {
		t.Fatalf("raw source data escaped diagnostic view: %q", model.View().Content)
	}
}

func TestRuntimeTUILocalSourceOpensDiagnosticsWithoutRemoteReveal(t *testing.T) {
	sourceID := "src_" + strings.Repeat("e", 32)
	client := &fakeClient{snapshot: runtimeapi.Snapshot{
		ProtocolVersion: runtimeapi.ProtocolVersion,
		Sources: runtimeapi.SourceStatus{
			CurrentSourceID: sourceID,
			Items: []runtimeapi.SourceSummary{{
				ID: sourceID, Name: "local", Type: runtimeapi.SourceTypeLocalImport, Current: true,
			}},
		},
	}}
	model, _ := initializeShellModel(t, client)

	updated, _ := model.Update(ctrlKey('u'))
	model = updated.(Model)
	updated, command := model.Update(ctrlKey('u'))
	model = updated.(Model)
	if command != nil || !model.sourceDiagnosticOpen || client.revealCalls != 0 {
		t.Fatalf("local diagnostics command=%v open=%v revealCalls=%d", command != nil, model.sourceDiagnosticOpen, client.revealCalls)
	}
	updated, _ = model.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	model = updated.(Model)
	updated, _ = model.Update(keyPress('U'))
	model = updated.(Model)
	if model.sourceDiagnosticOpen {
		t.Fatal("Mihomo update shortcut opened source diagnostics")
	}
}

func TestRuntimeTUISourceSwitchConfirmationExplainsTransactionOrder(t *testing.T) {
	model := Model{snapshot: runtimeapi.Snapshot{Revision: 8}}
	confirmation := model.describeAction(runtimeapi.Action{Kind: runtimeapi.ActionSwitchSource})
	for _, text := range []string{"刷新", "生成候选", "校验", "应用与健康检查", "提交当前来源"} {
		if !strings.Contains(confirmation.Impact, text) {
			t.Fatalf("source switch impact missing %q: %q", text, confirmation.Impact)
		}
	}
	if !strings.Contains(confirmation.Recovery, "恢复原来源和原运行状态") {
		t.Fatalf("source switch recovery=%q", confirmation.Recovery)
	}
}
